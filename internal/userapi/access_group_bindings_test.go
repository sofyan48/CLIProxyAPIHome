package userapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestAPIKeyWriteRejectsDeletedAccessGroup(t *testing.T) {
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	respondAPIKeyWriteError(ctx, "api_key_update_failed", fmt.Errorf("update bindings: %w", cluster.ErrDeletedAccessGroup))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_access_group") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
