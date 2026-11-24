package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/log"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/srl-labs/clab-api-server/internal/clab"
	"github.com/srl-labs/clab-api-server/internal/config"
	"github.com/srl-labs/clab-api-server/internal/models"
	termsvc "github.com/srl-labs/clab-api-server/internal/terminal"
)

var terminalManager *termsvc.Manager

var resumeKeyExpr = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)

var terminalUpgrader = websocket.Upgrader{
	CheckOrigin: func(_ *http.Request) bool { return true },
}

type terminalStreamClientMessage struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

func InitTerminalManager() {
	terminalManager = termsvc.NewManager(
		termsvc.DefaultCleanupTick,
		config.AppConfig.TerminalSessionTTL,
		termsvc.DefaultIdleTimeout,
		config.AppConfig.TerminalMaxSessionsPerUser,
	)
}

// closeResumableTerminals ends parked shells of nodes about to stop or be recreated (NTG-505).
func closeResumableTerminals(labName string, nodes ...string) {
	if terminalManager != nil {
		terminalManager.CloseResumable(labName, nodes...)
	}
}

func ShutdownTerminalManager() {
	if terminalManager != nil {
		terminalManager.Shutdown()
	}
}

// RequestTerminalSessionHandler creates a constrained interactive terminal session for an owned node.
// @Summary Create browser terminal session
// @Description Creates a constrained interactive terminal session for an owned lab node using one of the supported protocols: `ssh`, `shell`, `telnet`, `console` (Cisco IOL out-of-band console), or `console-control` (the grader's console write lock).
// @Description
// @Description **Protocol behavior**
// @Description - `ssh`: launches the server-side `ssh` client in a PTY directly to the node management IP and streams that terminal over WebSocket.
// @Description - `shell`: launches a PTY-backed `docker|podman exec -it <container> <server-selected-command>` session. Processes it starts in the container are hung up (SIGHUP) when the session ends.
// @Description - `ssh` with `resumeKey` on a `linux` node: the session is resumable. When the client drops without a deliberate close it stays alive for 10 minutes and the same key gets the same session back (`reattached: true`); the next stream replays the last 64 KiB of output.
// @Description - `telnet`: launches a PTY-backed `docker|podman exec -it <container> telnet 127.0.0.1 <port>` session. Processes it starts in the container are hung up (SIGHUP) when the session ends.
// @Description
// @Description **Security notes**
// @Description - The backend chooses the launch command for every protocol. Clients choose only the protocol and terminal size.
// @Description - Access is limited to owned nodes unless the caller is a configured superuser.
// @Description
// @Description **Relationship to `/api/v1/labs/{labName}/nodes/{nodeName}/ssh`**
// @Description - This endpoint creates the interactive browser terminal session.
// @Description - The separate `/ssh` endpoint returns temporary SSH access details for an external SSH client and is not required for browser terminals.
// @Tags Terminal Sessions
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param labName path string true "Lab name" example="my-lab"
// @Param nodeName path string true "Full container name of the node (e.g., clab-my-lab-srl1)" example="clab-my-lab-srl1"
// @Param terminal_request body models.TerminalSessionRequest true "Terminal session creation parameters"
// @Success 200 {object} models.TerminalSessionInfo "Created terminal session"
// @Failure 400 {object} models.ErrorResponse "Invalid request parameters"
// @Failure 401 {object} models.ErrorResponse "Unauthorized"
// @Failure 404 {object} models.ErrorResponse "Lab or node not found"
// @Failure 429 {object} models.ErrorResponse "Too many active terminal sessions"
// @Failure 500 {object} models.ErrorResponse "Internal server error"
// @Router /api/v1/labs/{labName}/nodes/{nodeName}/terminal-sessions [post]
func RequestTerminalSessionHandler(c *gin.Context) {
	username := c.GetString("username")
	labName := c.Param("labName")
	requestedNodeName := strings.TrimSpace(c.Param("nodeName"))

	if !isValidLabName(labName) {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid lab name format."})
		return
	}
	if !isResolvableContainerReference(requestedNodeName) {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid container name format."})
		return
	}

	var req models.TerminalSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		if err != io.EOF {
			c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid request format: " + err.Error()})
			return
		}
	}

	if req.ResumeKey != "" && !resumeKeyExpr.MatchString(req.ResumeKey) {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid resumeKey format."})
		return
	}

	if _, err := verifyLabOwnership(c, username, labName); err != nil {
		return
	}

	svc := GetClabService()
	if svc == nil {
		err := fmt.Errorf("containerlab service not initialized")
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := resolveLabNodeContainer(ctx, svc, labName, requestedNodeName)
	if err != nil {
		if hasDifferentDefaultContainerlabPrefix(labName, requestedNodeName) {
			c.JSON(http.StatusBadRequest, models.ErrorResponse{
				Error: fmt.Sprintf("Container '%s' does not belong to lab '%s'", requestedNodeName, labName),
			})
			return
		}
		c.JSON(http.StatusNotFound, models.ErrorResponse{Error: err.Error()})
		return
	}
	containerInfo := clab.ContainerToClabContainerInfo(*container)

	if containerInfo.LabName != labName {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{
			Error: fmt.Sprintf("Container '%s' does not belong to lab '%s'", requestedNodeName, labName),
		})
		return
	}
	if !isSuperuser(username) && containerInfo.Owner != username {
		err := fmt.Errorf("container '%s' not found or not owned by user", requestedNodeName)
		c.JSON(http.StatusNotFound, models.ErrorResponse{Error: err.Error()})
		return
	}

	shortNodeName := resolveTopologyNodeName(labName, container)
	sessionInfo, err := terminalManager.CreateSession(termsvc.CreateSessionOptions{
		Username:      username,
		LabName:       labName,
		NodeName:      shortNodeName,
		ContainerID:   strings.TrimSpace(containerInfo.ContainerID),
		ContainerIP:   strings.TrimSpace(containerInfo.IPv4Address),
		ContainerKind: strings.TrimSpace(containerInfo.Kind),
		Runtime:       config.AppConfig.ClabRuntime,
		Protocol:      req.Protocol,
		Cols:          req.Cols,
		Rows:          req.Rows,
		SSHUsername:   req.SSHUsername,
		TelnetPort:    req.TelnetPort,
		ResumeKey:     req.ResumeKey,
	})
	if err != nil {
		writeTerminalManagerError(c, username, "create terminal session", err)
		return
	}

	log.Infof(
		"terminal session granted for user=%s lab=%s node=%s protocol=%s session=%s",
		username,
		labName,
		shortNodeName,
		sessionInfo.Protocol,
		sessionInfo.SessionID,
	)

	c.JSON(http.StatusOK, sessionInfo)
}

// GetTerminalSessionHandler returns metadata for a terminal session.
// @Summary Get terminal session
// @Description Returns metadata and lifecycle state for a terminal session.
// @Tags Terminal Sessions
// @Security BearerAuth
// @Produce json
// @Param sessionId path string true "Terminal session ID"
// @Success 200 {object} models.TerminalSessionInfo "Terminal session metadata"
// @Failure 401 {object} models.ErrorResponse "Unauthorized"
// @Failure 404 {object} models.ErrorResponse "Session not found"
// @Failure 500 {object} models.ErrorResponse "Internal server error"
// @Router /api/v1/terminal-sessions/{sessionId} [get]
func GetTerminalSessionHandler(c *gin.Context) {
	username := c.GetString("username")
	sessionID := strings.TrimSpace(c.Param("sessionId"))
	info, err := terminalManager.GetSessionInfo(sessionID, username, isSuperuser(username))
	if err != nil {
		writeTerminalManagerError(c, username, "get terminal session", err)
		return
	}
	c.JSON(http.StatusOK, info)
}

// TerminateTerminalSessionHandler closes a terminal session.
// @Summary Terminate terminal session
// @Description Terminates a terminal session owned by the caller (or any session for a superuser).
// @Tags Terminal Sessions
// @Security BearerAuth
// @Produce json
// @Param sessionId path string true "Terminal session ID"
// @Success 200 {object} models.GenericSuccessResponse "Terminal session terminated"
// @Failure 401 {object} models.ErrorResponse "Unauthorized"
// @Failure 404 {object} models.ErrorResponse "Session not found"
// @Failure 500 {object} models.ErrorResponse "Internal server error"
// @Router /api/v1/terminal-sessions/{sessionId} [delete]
func TerminateTerminalSessionHandler(c *gin.Context) {
	username := c.GetString("username")
	sessionID := strings.TrimSpace(c.Param("sessionId"))
	if err := terminalManager.TerminateSession(sessionID, username, isSuperuser(username)); err != nil {
		writeTerminalManagerError(c, username, "terminate terminal session", err)
		return
	}
	c.JSON(http.StatusOK, models.GenericSuccessResponse{Message: "Terminal session terminated"})
}

// StreamTerminalSessionHandler upgrades the request to a WebSocket stream for terminal I/O.
// @Summary Stream terminal session
// @Description Upgrades the request to a WebSocket connection for terminal input, output, resize, and close events.
// @Description
// @Description **Protocol notes**
// @Description - Connect with a WebSocket client to this endpoint after creating a terminal session.
// @Description - Client messages are JSON objects with `type` set to `input`, `resize`, or `close`.
// @Description - Server messages are JSON objects with `type` set to `ready`, `output`, or `exit`.
// @Tags Terminal Sessions
// @Security BearerAuth
// @Produce json
// @Param sessionId path string true "Terminal session ID"
// @Success 101 {string} string "Switching Protocols to WebSocket"
// @Failure 401 {object} models.ErrorResponse "Unauthorized"
// @Failure 404 {object} models.ErrorResponse "Session not found"
// @Failure 409 {object} models.ErrorResponse "Session already attached"
// @Failure 410 {object} models.ErrorResponse "Session already exited"
// @Failure 500 {object} models.ErrorResponse "Internal server error"
// @Router /api/v1/terminal-sessions/{sessionId}/stream [get]
func StreamTerminalSessionHandler(c *gin.Context) {
	username := c.GetString("username")
	sessionID := strings.TrimSpace(c.Param("sessionId"))
	session, stream, err := terminalManager.BeginStream(sessionID, username, isSuperuser(username))
	if err != nil {
		writeTerminalManagerError(c, username, "stream terminal session", err)
		return
	}

	conn, err := terminalUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		session.EndStream(stream)
		log.Warnf("terminal websocket upgrade failed for user '%s' session '%s': %v", username, sessionID, err)
		return
	}
	defer conn.Close()
	defer session.EndStream(stream)

	ready := session.Snapshot()
	if err := conn.WriteJSON(gin.H{
		"type":      "ready",
		"sessionId": ready.SessionID,
		"protocol":  ready.Protocol,
		"createdAt": ready.CreatedAt,
		"expiresAt": ready.ExpiresAt,
	}); err != nil {
		session.Drop(stream, "client disconnected before terminal became ready")
		return
	}
	if len(stream.Replay) > 0 {
		if err := conn.WriteJSON(gin.H{
			"type":     "output",
			"data":     base64.StdEncoding.EncodeToString(stream.Replay),
			"encoding": "base64",
		}); err != nil {
			session.Drop(stream, "client disconnected during replay")
			return
		}
	}
	if stream.Reattached {
		session.Redraw()
	}

	readErrCh := make(chan error, 1)
	go func() {
		// Any way out of this loop but a deliberate close detaches (parks) a resumable session.
		defer session.Drop(stream, "client disconnected")
		for {
			_, payload, readErr := conn.ReadMessage()
			if readErr != nil {
				readErrCh <- readErr
				return
			}

			var message terminalStreamClientMessage
			if err := json.Unmarshal(payload, &message); err != nil {
				readErrCh <- err
				return
			}

			switch message.Type {
			case "input":
				if err := session.WriteInput(stream, message.Data); err != nil {
					readErrCh <- err
					return
				}
			case "resize":
				if err := session.Resize(stream, message.Cols, message.Rows); err != nil {
					readErrCh <- err
					return
				}
			case "close":
				session.Close(stream, "client requested close")
				readErrCh <- nil
				return
			default:
				readErrCh <- fmt.Errorf("unsupported terminal stream message %q", message.Type)
				return
			}
		}
	}()

	buffer := make([]byte, 4096)
	for {
		n, readErr := stream.Read(buffer)
		if n > 0 {
			if err := conn.WriteJSON(gin.H{
				"type":     "output",
				"data":     base64.StdEncoding.EncodeToString(buffer[:n]),
				"encoding": "base64",
			}); err != nil {
				session.Drop(stream, "client disconnected during terminal output")
				return
			}
		}

		select {
		case clientErr := <-readErrCh:
			if clientErr != nil && !websocket.IsCloseError(clientErr, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Debugf("terminal websocket client loop ended for session '%s': %v", sessionID, clientErr)
			}
			session.Drop(stream, "client disconnected")
			return
		default:
		}

		if errors.Is(readErr, termsvc.ErrTakenOver) {
			_ = conn.WriteJSON(gin.H{
				"type":     "output",
				"data":     base64.StdEncoding.EncodeToString([]byte("\r\n[console opened in another tab]\r\n")),
				"encoding": "base64",
			})
			return
		}
		if errors.Is(readErr, termsvc.ErrDetached) {
			return
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				log.Debugf("terminal PTY read ended for session '%s': %v", sessionID, readErr)
			}
			break
		}
	}

	_ = session.WaitForExit(2 * time.Second)
	snapshot := session.Snapshot()
	_ = conn.WriteJSON(gin.H{
		"type":     "exit",
		"exitCode": snapshot.ExitCode,
		"error":    snapshot.Error,
		"state":    snapshot.State,
	})
}

func writeTerminalManagerError(c *gin.Context, username, action string, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, termsvc.ErrSessionNotFound):
		status = http.StatusNotFound
	case errors.Is(err, termsvc.ErrSessionForbidden):
		status = http.StatusNotFound
	case errors.Is(err, termsvc.ErrSessionAttached):
		status = http.StatusConflict
	case errors.Is(err, termsvc.ErrSessionExited):
		status = http.StatusGone
	case errors.Is(err, termsvc.ErrTooManySessions):
		status = http.StatusTooManyRequests
	case strings.Contains(err.Error(), "unsupported terminal protocol"):
		status = http.StatusBadRequest
	}

	log.Warnf("terminal action failed: user=%s action=%s error=%v", username, action, err)
	c.JSON(status, models.ErrorResponse{Error: err.Error()})
}
