package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/quota"
)

type fakeResetConsumer struct {
	id, key string
	err     error
}

func (f *fakeResetConsumer) ConsumeCodexResetCredit(_ context.Context, id, key string) (string, error) {
	f.id, f.key = id, key
	return "reset", f.err
}

func TestConsumeCodexResetCreditManagementContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	consumer := &fakeResetConsumer{}
	handler := &Handler{resetCreditConsumer: consumer}
	engine := gin.New()
	engine.POST("/quota/credentials/:credential_id/reset-credits/consume", handler.ConsumeCodexResetCredit)
	path := "/quota/credentials/auth-1/reset-credits/consume"
	for _, input := range []string{`{}`, `{"idempotency_key":" "}`, `{"idempotency_key":12}`} {
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, strings.NewReader(input)))
		if response.Code != http.StatusBadRequest || consumer.key != "" {
			t.Fatalf("body %s status=%d key=%q", input, response.Code, consumer.key)
		}
	}
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"idempotency_key":" stable "}`)))
	var payload map[string]any
	if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil || response.Code != http.StatusOK || payload["outcome"] != "reset" || consumer.id != "auth-1" || consumer.key != "stable" {
		t.Fatalf("response=%d %s id=%q key=%q err=%v", response.Code, response.Body.String(), consumer.id, consumer.key, errDecode)
	}
	consumer.err = &quota.ResetCreditError{Status: http.StatusConflict, Code: "nothing_to_reset"}
	response = httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"idempotency_key":"new"}`)))
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "NOTHING_TO_RESET") {
		t.Fatalf("error response=%d %s", response.Code, response.Body.String())
	}
}
