package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/srl-labs/clab-api-server/internal/clab"
)

func TestNormalizeLifecycleNodeNames(t *testing.T) {
	got, err := normalizeLifecycleNodeNames([]string{" R3 ", "R1"})
	if err != nil {
		t.Fatalf("normalizeLifecycleNodeNames() error = %v", err)
	}
	want := []string{"R3", "R1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeLifecycleNodeNames() = %v, want %v", got, want)
	}
}

func TestNormalizeLifecycleNodeNamesRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name  string
		nodes []string
	}{
		{name: "empty", nodes: nil},
		{name: "blank", nodes: []string{"R1", " "}},
		{name: "duplicate after trimming", nodes: []string{"R1", " R1 "}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := normalizeLifecycleNodeNames(tt.nodes); err == nil {
				t.Fatal("normalizeLifecycleNodeNames() error = nil, want validation error")
			}
		})
	}
}

func TestValidBulkLifecycleAction(t *testing.T) {
	for _, action := range []clab.NodeLifecycleAction{
		clab.NodeLifecycleActionStart,
		clab.NodeLifecycleActionStop,
	} {
		if !validBulkLifecycleAction(action) {
			t.Fatalf("validBulkLifecycleAction(%q) = false, want true", action)
		}
	}

	for _, action := range []clab.NodeLifecycleAction{
		clab.NodeLifecycleActionRestart,
		clab.NodeLifecycleActionPause,
		clab.NodeLifecycleActionUnpause,
		clab.NodeLifecycleAction(""),
	} {
		if validBulkLifecycleAction(action) {
			t.Fatalf("validBulkLifecycleAction(%q) = true, want false", action)
		}
	}
}

func TestBulkNodeLifecycleRejectsBeforeDispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Params = gin.Params{{Key: "labName", Value: "test-lab"}}
	context.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/v1/labs/test-lab/nodes/lifecycle",
		strings.NewReader(`{"action":"restart","nodeNames":["R1"]}`),
	)
	context.Request.Header.Set("Content-Type", "application/json")

	BulkNodeLifecycleHandler(context)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	var response struct {
		Dispatched bool `json:"dispatched"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Dispatched {
		t.Fatal("dispatched = true, want false")
	}
}
