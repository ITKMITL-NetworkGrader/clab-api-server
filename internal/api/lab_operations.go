package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/srl-labs/clab-api-server/internal/clab"
	"github.com/srl-labs/clab-api-server/internal/models"
)

type labOperationRegistry struct {
	mu     sync.Mutex
	active map[string]string
}

var labOperations = &labOperationRegistry{
	active: make(map[string]string),
}

func (r *labOperationRegistry) begin(labName, operation string) (func(), string, bool) {
	key := strings.TrimSpace(labName)
	if key == "" {
		return func() {}, "", true
	}

	op := strings.TrimSpace(operation)
	if op == "" {
		op = "unknown"
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if active, ok := r.active[key]; ok {
		return nil, active, false
	}

	r.active[key] = op
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.active[key] == op {
			delete(r.active, key)
		}
	}, "", true
}

func beginLabOperationOrConflict(c *gin.Context, labName, operation string) (func(), bool) {
	release, active, ok := labOperations.begin(labName, operation)
	if ok {
		return release, true
	}

	c.Header("Retry-After", "2")
	c.JSON(http.StatusConflict, models.ErrorResponse{
		Error: fmt.Sprintf("Lab '%s' is busy with %s operation.", labName, active),
	})
	return nil, false
}

// beginLabOperationWaiting is beginLabOperationOrConflict that first waits up to wait for a
// running operation on the lab to finish. It gives up when the caller has gone.
func beginLabOperationWaiting(c *gin.Context, labName, operation string, wait time.Duration) (func(), bool) {
	deadline := time.Now().Add(wait)
	for {
		release, _, ok := labOperations.begin(labName, operation)
		if ok {
			if c.Request.Context().Err() != nil {
				release()
				return nil, false
			}
			return release, true
		}
		if c.Request.Context().Err() != nil {
			return nil, false
		}
		if time.Now().After(deadline) {
			return beginLabOperationOrConflict(c, labName, operation)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func ensureLabDestroyed(ctx context.Context, svc *clab.Service, labName string) error {
	containers, err := svc.ListContainers(ctx, clab.ListOptions{LabName: labName})
	if err != nil {
		if isContainersNotFoundError(err) {
			return nil
		}
		return fmt.Errorf("failed to verify lab '%s' destroy state: %w", labName, err)
	}

	names := make([]string, 0, len(containers))
	for _, container := range containers {
		info := clab.ContainerToClabContainerInfo(container)
		if info.LabName != labName {
			continue
		}
		if info.Name != "" {
			names = append(names, info.Name)
		} else if info.ContainerID != "" {
			names = append(names, info.ContainerID)
		}
	}

	if len(names) > 0 {
		return fmt.Errorf(
			"destroy verification failed for lab '%s': %d container(s) still exist: %s",
			labName,
			len(names),
			strings.Join(names, ", "),
		)
	}

	return nil
}

func isContainersNotFoundError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no containers found") ||
		strings.Contains(msg, "no containerlab labs found") ||
		strings.Contains(msg, "not found")
}
