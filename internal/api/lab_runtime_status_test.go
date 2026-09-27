package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestLabRuntimeStatusRejectsInvalidLabName(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "labName", Value: "../other-lab"}}
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/v1/labs/../other-lab/runtime-status", nil)

	LabRuntimeStatusHandler(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestManagementNetworkCleanupRejectsInvalidLabName(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "labName", Value: "../other-lab"}}
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/api/v1/labs/../other-lab/management-network", nil)

	ManagementNetworkCleanupHandler(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestManagementNetworkCleanupRequiresSuperuser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("username", "someone")
	ctx.Params = gin.Params{{Key: "labName", Value: "pg-ntg114"}}
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/api/v1/labs/pg-ntg114/management-network", nil)

	ManagementNetworkCleanupHandler(ctx)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
}
