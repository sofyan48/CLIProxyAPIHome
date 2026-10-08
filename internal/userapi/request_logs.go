package userapi

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

type userRequestLogItem struct {
	Timestamp string `json:"timestamp"`
	EventType string `json:"event_type"`
	Status    string `json:"status"`
	Model     string `json:"model"`

	Tokens    int64  `json:"tokens"`
	LatencyMS int64  `json:"latency_ms"`
	RequestID string `json:"request_id"`
}

func (h *Handler) ListCurrentUserRequestLogs(c *gin.Context) {
	ctx, cancel := requestContext(c)
	defer cancel()
	user, ok := h.authenticatedUser(c, ctx, authFields{})
	if !ok {
		return
	}
	from, ok := userRequestLogTimeQuery(c, "from")
	if !ok {
		return
	}
	to, ok := userRequestLogTimeQuery(c, "to")
	if !ok {
		return
	}
	if from != nil && to != nil && from.After(*to) {
		respondError(c, http.StatusBadRequest, "invalid_time_range", fmt.Errorf("from must not be after to"))
		return
	}
	limit, offset, ok := userBillingPaginationFromRequest(c)
	if !ok {
		return
	}
	// Only these parameters are read. Identity overrides and broad searches must
	// never affect the selected rows, count, or aggregates.
	result, errLogs := h.repo.ListUserRequestLogs(ctx, user.ID, cluster.UserRequestLogQuery{
		From: from, To: to, RequestID: c.Query("request_id"), Limit: limit, Offset: offset,
	})
	if errLogs != nil {
		respondError(c, http.StatusInternalServerError, "request_logs_load_failed", errLogs)
		return
	}
	items := make([]userRequestLogItem, 0, len(result.Items))
	for _, record := range result.Items {
		items = append(items, userRequestLogItem{
			Timestamp: record.Timestamp.UTC().Format(time.RFC3339Nano),
			EventType: record.EventType, Status: record.Status, Model: record.Model,
			Tokens: record.Tokens, LatencyMS: record.LatencyMS, RequestID: record.RequestID,
		})
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": result.Total, "limit": result.Limit, "offset": result.Offset, "summary": result.Summary})
}

func userRequestLogTimeQuery(c *gin.Context, key string) (*time.Time, bool) {
	raw, present := c.GetQuery(key)
	if !present {
		// GetQuery treats an explicit empty value as absent; reject it too.
		if _, exists := c.Request.URL.Query()[key]; !exists {
			return nil, true
		}
	}
	parsed, errParse := time.Parse(time.RFC3339Nano, raw)
	if errParse != nil {
		respondError(c, http.StatusBadRequest, "invalid_"+key, fmt.Errorf("%s must be RFC3339", key))
		return nil, false
	}
	parsed = parsed.UTC()
	return &parsed, true
}
