package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/quota"
)

type CodexResetCreditConsumer interface {
	ConsumeCodexResetCredit(context.Context, string, string) (string, error)
}

func (h *Handler) SetCodexResetCreditConsumer(consumer CodexResetCreditConsumer) {
	if h != nil {
		h.resetCreditConsumer = consumer
	}
}

func (h *Handler) ConsumeCodexResetCredit(c *gin.Context) {
	if h.resetCreditConsumer == nil {
		respondQuotaHTTPError(c, http.StatusNotFound, "RESET_CREDIT_UNSUPPORTED", "reset credit consumption is unavailable", false)
		return
	}
	var body struct {
		IdempotencyKey string `json:"idempotency_key"`
	}
	if c.Request == nil || c.Request.Body == nil || json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 4096)).Decode(&body) != nil || strings.TrimSpace(body.IdempotencyKey) == "" || len(body.IdempotencyKey) > 256 {
		respondQuotaHTTPError(c, http.StatusBadRequest, "INVALID_BODY", "a nonempty idempotency_key (max 256 characters) is required", false)
		return
	}
	credentialID := strings.TrimSpace(c.Param("credential_id"))
	if credentialID == "" {
		respondQuotaHTTPError(c, http.StatusNotFound, "QUOTA_CREDENTIAL_NOT_FOUND", "quota credential not found", false)
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	outcome, errConsume := h.resetCreditConsumer.ConsumeCodexResetCredit(ctx, credentialID, strings.TrimSpace(body.IdempotencyKey))
	if errConsume != nil {
		var known *quota.ResetCreditError
		if errors.As(errConsume, &known) {
			respondQuotaHTTPError(c, known.Status, strings.ToUpper(known.Code), known.Code, false)
		} else {
			respondQuotaHTTPError(c, http.StatusInternalServerError, "RESET_CREDIT_FAILED", "reset credit request failed", false)
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"outcome": outcome})
}
