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
