package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/log"
	"github.com/gin-gonic/gin"

	"github.com/srl-labs/clab-api-server/internal/models"
)

type labTopologyDocPaths struct {
	deployed      bool
	runningDoc    string
	localDoc      string
	ownerUsername string
}

// lookupLabInfo resolves a lab's newest container info; a variable so tests can stub it.
// NTG-224: it is not cached. A 1.5 s cache sent the write after an apply to the file the
// newest container pointed at before the apply.
var lookupLabInfo = getLabInfo

func resolveLabTopologyDocPaths(c *gin.Context, docType string) (*labTopologyDocPaths, error) {
	username := c.GetString("username")
	labName := c.Param("labName")

	if !isValidLabName(labName) {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid characters in lab name."})
		return nil, fmt.Errorf("invalid lab name")
	}

	if docType != "yaml" && docType != "annotations" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid topology document type."})
		return nil, fmt.Errorf("invalid topology document type")
	}

	localDoc, _, _, _, err := resolveDefaultTopologyDocPath(username, labName, docType)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: fmt.Sprintf("Failed to resolve lab document path: %s", err.Error())})
		return nil, err
	}

	paths := &labTopologyDocPaths{
		deployed:      false,
		localDoc:      localDoc,
		ownerUsername: username,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	labInfo, exists, lookupErr := lookupLabInfo(ctx, username, labName)
	if lookupErr != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: fmt.Sprintf("Failed to check lab '%s' status: %s", labName, lookupErr.Error())})
		return nil, lookupErr
	}

	if !exists || labInfo == nil {
		return paths, nil
	}

	if !isSuperuser(username) && labInfo.Owner != username {
		c.JSON(http.StatusNotFound, models.ErrorResponse{Error: fmt.Sprintf("lab '%s' not found or not owned by user", labName)})
		return nil, fmt.Errorf("lab '%s' not found or not owned by user", labName)
	}

	paths.deployed = true
	if strings.TrimSpace(labInfo.Owner) != "" {
		paths.ownerUsername = labInfo.Owner
	}

	if strings.TrimSpace(labInfo.AbsLabPath) != "" {
		paths.runningDoc = filepath.Clean(labInfo.AbsLabPath)
		if docType == "annotations" {
			paths.runningDoc += ".annotations.json"
		}
	}

	ownerLocalDoc, _, _, _, ownerPathErr := resolveDefaultTopologyDocPath(paths.ownerUsername, labName, docType)
	if ownerPathErr != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: fmt.Sprintf("Failed to resolve owner lab document path: %s", ownerPathErr.Error())})
		return nil, ownerPathErr
	}
	paths.localDoc = ownerLocalDoc

	return paths, nil
}

func readLabTopologyDoc(c *gin.Context, docType string) {
	paths, err := resolveLabTopologyDocPaths(c, docType)
	if err != nil {
		return
	}

	if paths.deployed && strings.TrimSpace(paths.runningDoc) != "" {
		content, readErr := os.ReadFile(paths.runningDoc)
		if readErr == nil {
			c.Data(http.StatusOK, "text/plain; charset=utf-8", content)
			return
		}
		if !os.IsNotExist(readErr) {
			c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: readErr.Error()})
			return
		}
	}

	content, readErr := os.ReadFile(paths.localDoc)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			c.JSON(http.StatusNotFound, models.ErrorResponse{Error: "File not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: readErr.Error()})
		return
	}

	c.Data(http.StatusOK, "text/plain; charset=utf-8", content)
}

func writeLabTopologyDocFile(absPath, ownerUsername, labName string, body []byte) error {
	targetDir := filepath.Dir(absPath)
	if mkdirErr := os.MkdirAll(targetDir, 0750); mkdirErr != nil {
		return fmt.Errorf("failed to ensure lab directory: %w", mkdirErr)
	}

	// NTG-224: write a temp file and rename it over the target, so a reader never sees a
	// half-written document.
	tmp, createErr := os.CreateTemp(targetDir, "."+filepath.Base(absPath)+".tmp-*")
	if createErr != nil {
		return fmt.Errorf("failed to write file: %w", createErr)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // nothing left to remove after a successful rename
	if _, writeErr := tmp.Write(body); writeErr != nil {
		tmp.Close()
		return fmt.Errorf("failed to write file: %w", writeErr)
	}
	if closeErr := tmp.Close(); closeErr != nil {
		return fmt.Errorf("failed to write file: %w", closeErr)
	}
	if chmodErr := os.Chmod(tmpPath, 0640); chmodErr != nil {
		return fmt.Errorf("failed to write file: %w", chmodErr)
	}

	if _, uid, gid, uidErr := getLabDirectoryInfo(ownerUsername, labName); uidErr == nil {
		_ = os.Chown(targetDir, uid, gid)
		_ = os.Chown(tmpPath, uid, gid)
	}

	if renameErr := os.Rename(tmpPath, absPath); renameErr != nil {
		return fmt.Errorf("failed to write file: %w", renameErr)
	}
	return nil
}

// topologyDocLockWait bounds how long a topology document write waits for a lab operation.
// It stays under Elysia's 15 s request timeout.
var topologyDocLockWait = 10 * time.Second

func writeLabTopologyDoc(c *gin.Context, docType string) {
	body, readErr := io.ReadAll(c.Request.Body)
	if readErr != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Failed to read request body"})
		return
	}

	// NTG-224: wait for a running lab operation, so this write cannot land between an apply
	// and the apply's own update of the running document.
	releaseLabOperation, ok := beginLabOperationWaiting(c, c.Param("labName"), "topology-doc", topologyDocLockWait)
	if !ok {
		return
	}
	defer releaseLabOperation()

	paths, err := resolveLabTopologyDocPaths(c, docType)
	if err != nil {
		return
	}

	targetPath := paths.localDoc
	if paths.deployed && strings.TrimSpace(paths.runningDoc) != "" {
		if _, statErr := os.Stat(paths.runningDoc); statErr == nil {
			targetPath = paths.runningDoc
		} else if !os.IsNotExist(statErr) {
			c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: statErr.Error()})
			return
		}
	}

	if writeErr := writeLabTopologyDocFile(targetPath, paths.ownerUsername, c.Param("labName"), body); writeErr != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: writeErr.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// @Summary Get lab topology YAML
// @Description Returns the topology YAML for the specified lab. For deployed labs, the running topology source path is preferred and local files are used as fallback.
// @Tags Labs
// @Security BearerAuth
// @Produce plain
// @Param labName path string true "Lab name"
// @Success 200 {string} string "Topology YAML content"
// @Failure 400 {object} models.ErrorResponse "Invalid lab name"
// @Failure 401 {object} models.ErrorResponse "Unauthorized"
// @Failure 404 {object} models.ErrorResponse "File not found"
// @Failure 500 {object} models.ErrorResponse "Internal server error"
// @Router /api/v1/labs/{labName}/topology/yaml [get]
// GetRunningLabYamlHandler returns the source YAML used by a lab.
func GetRunningLabYamlHandler(c *gin.Context) {
	readLabTopologyDoc(c, "yaml")
}

// @Summary Update lab topology YAML
// @Description Updates the topology YAML for the specified lab. For deployed labs, writes to the running topology source when present and otherwise writes to local files.
// @Tags Labs
// @Security BearerAuth
// @Accept plain
// @Produce json
// @Param labName path string true "Lab name"
// @Param content body string true "Topology YAML content"
// @Success 200 {object} models.SimpleSuccessResponse "Write success"
// @Failure 400 {object} models.ErrorResponse "Invalid input"
// @Failure 401 {object} models.ErrorResponse "Unauthorized"
// @Failure 404 {object} models.ErrorResponse "Lab not found"
// @Failure 500 {object} models.ErrorResponse "Internal server error"
// @Router /api/v1/labs/{labName}/topology/yaml [put]
// PutRunningLabYamlHandler updates the source YAML used by a lab.
func PutRunningLabYamlHandler(c *gin.Context) {
	writeLabTopologyDoc(c, "yaml")
}

// @Summary Get lab annotations
// @Description Returns annotations for the specified lab. For deployed labs, the running annotations path is preferred and local files are used as fallback.
// @Tags Labs
// @Security BearerAuth
// @Produce plain
// @Param labName path string true "Lab name"
// @Success 200 {string} string "Annotations content"
// @Failure 400 {object} models.ErrorResponse "Invalid lab name"
// @Failure 401 {object} models.ErrorResponse "Unauthorized"
// @Failure 404 {object} models.ErrorResponse "File not found"
// @Failure 500 {object} models.ErrorResponse "Internal server error"
// @Router /api/v1/labs/{labName}/topology/annotations [get]
// GetRunningLabAnnotationsHandler returns the annotations JSON associated with a lab.
func GetRunningLabAnnotationsHandler(c *gin.Context) {
	readLabTopologyDoc(c, "annotations")
}

// @Summary Update lab annotations
// @Description Updates annotations for the specified lab. For deployed labs, writes to the running annotations path when present and otherwise writes to local files.
// @Tags Labs
// @Security BearerAuth
// @Accept plain
// @Produce json
// @Param labName path string true "Lab name"
// @Param content body string true "Annotations content"
// @Success 200 {object} models.SimpleSuccessResponse "Write success"
// @Failure 400 {object} models.ErrorResponse "Invalid input"
// @Failure 401 {object} models.ErrorResponse "Unauthorized"
// @Failure 404 {object} models.ErrorResponse "Lab not found"
// @Failure 500 {object} models.ErrorResponse "Internal server error"
// @Router /api/v1/labs/{labName}/topology/annotations [put]
// PutRunningLabAnnotationsHandler updates or creates the annotations JSON associated with a lab.
func PutRunningLabAnnotationsHandler(c *gin.Context) {
	writeLabTopologyDoc(c, "annotations")
}

// syncRunningTopologyDoc makes the lab's running topology document hold what an apply just
// applied (NTG-224). The running document is the topo-file label of the newest container, so
// an apply that removed the newest containers leaves it on an older file nobody rewrote, and
// the document goes on listing deleted nodes. applied is the file as read before the apply.
func syncRunningTopologyDoc(ctx context.Context, labName, appliedPath string, applied []byte) error {
	info, exists, err := lookupLabInfo(ctx, "", labName)
	if err != nil {
		return err
	}
	if !exists || info == nil {
		return nil
	}
	running := strings.TrimSpace(info.AbsLabPath)
	if running == "" || strings.HasPrefix(running, "http://") || strings.HasPrefix(running, "https://") {
		return nil
	}
	running = filepath.Clean(running)
	appliedPath = filepath.Clean(appliedPath)
	if running == appliedPath {
		return nil
	}
	// Relative paths inside the YAML (startup-config) only mean the same in the same directory.
	// The applied path was resolved inside the owner's lab dir, so this also keeps the write there.
	if filepath.Dir(running) != filepath.Dir(appliedPath) {
		log.Warnf("Lab '%s': running topology %s is not beside applied %s; not updated", labName, running, appliedPath)
		return nil
	}
	if stat, statErr := os.Lstat(running); statErr != nil || !stat.Mode().IsRegular() {
		log.Warnf("Lab '%s': running topology %s is not a regular file; not updated", labName, running)
		return nil
	}
	return writeLabTopologyDocFile(running, info.Owner, labName, applied)
}
