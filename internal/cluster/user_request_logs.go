package cluster

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// UserRequestLogQuery deliberately has no administrative or free-text filters.
type UserRequestLogQuery struct {
	From      *time.Time
	To        *time.Time
	RequestID string
	Limit     int
	Offset    int
}

type UserRequestLogSummary struct {
	Timestamp time.Time
	EventType string
	Status    string
	Model     string
	Tokens    int64
	LatencyMS int64
	RequestID string
}

type UserRequestLogResult struct {
	Items  []UserRequestLogSummary
	Total  int64
	Limit  int
	Offset int
}

// ListUserRequestLogs requires the immutable billing ownership snapshot. The
// observability UserID filter's current-key fallback is unsafe after reassignment.
// Failed/non-billable and legacy rows without a snapshot are excluded, never
// backfilled from a current key binding or a payload-supplied user identity.
func (r *Repository) ListUserRequestLogs(ctx context.Context, userID uint, query UserRequestLogQuery) (UserRequestLogResult, error) {
	if userID == 0 {
		return UserRequestLogResult{}, fmt.Errorf("user id is required")
	}
	db, errDB := r.database()
	if errDB != nil {
		return UserRequestLogResult{}, errDB
	}
	query.Limit, query.Offset = normalizeUsageObservabilityPagination(query.Limit, query.Offset, 50, 200)
	result := UserRequestLogResult{Items: make([]UserRequestLogSummary, 0), Limit: query.Limit, Offset: query.Offset}
	scope := db.WithContext(contextOrBackground(ctx)).Table("usage").
		Where(`EXISTS (SELECT 1 FROM "billing_charge" WHERE "billing_charge"."usage_id" = "usage"."id" AND "billing_charge"."user_id" = ?)`, userID)
	if query.From != nil {
		scope = scope.Where(`"usage"."timestamp" >= ?`, query.From.UTC())
	}
	if query.To != nil {
		scope = scope.Where(`"usage"."timestamp" < ?`, query.To.UTC())
	}
	if query.RequestID != "" {
		scope = scope.Where(`"usage"."request_id" = ?`, query.RequestID)
	}
	if errCount := scope.Session(&gorm.Session{}).Count(&result.Total).Error; errCount != nil {
		return UserRequestLogResult{}, errCount
	}
	// Do not load payloads, errors, keys, routing, or billing data even internally.
	selectSQL := fmt.Sprintf(`"usage"."timestamp", "usage"."event_type",
		CASE WHEN "usage"."failed" THEN 'failed' ELSE 'success' END AS status,
		"usage"."model", %s AS tokens, "usage"."latency_ms", "usage"."request_id"`, usageObservabilitySQLAccountingTotalTokens(`"usage"`))
	if errFind := scope.Session(&gorm.Session{}).Select(selectSQL).
		Order(`"usage"."timestamp" DESC, "usage"."id" DESC`).
		Limit(query.Limit).Offset(query.Offset).Scan(&result.Items).Error; errFind != nil {
		return UserRequestLogResult{}, errFind
	}
	for index := range result.Items {
		// Unknown event labels can contain arbitrary producer metadata; expose only
		// known categories rather than copying that metadata into the summary.
		event := normalizeUsageObservabilityEventType(result.Items[index].EventType)
		switch event {
		case "embedding", "response", "message", "stream", "completion":
			result.Items[index].EventType = event
		default:
			result.Items[index].EventType = "unknown"
		}
	}
	return result, nil
}
