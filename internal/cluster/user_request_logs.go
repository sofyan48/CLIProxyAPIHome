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

type UserRequestLogModelCount struct {
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
}

type UserRequestLogStatusCounts struct {
	Success int64 `json:"success"`
	Failed  int64 `json:"failed"`
}

type UserRequestLogAggregates struct {
	ByModel          []UserRequestLogModelCount `json:"by_model"`
	AverageLatencyMS *float64                   `json:"average_latency_ms"`
	Status           UserRequestLogStatusCounts `json:"status"`
}

type UserRequestLogResult struct {
	Items   []UserRequestLogSummary
	Total   int64
	Limit   int
	Offset  int
	Summary UserRequestLogAggregates
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
	result := UserRequestLogResult{
		Items: make([]UserRequestLogSummary, 0), Limit: query.Limit, Offset: query.Offset,
		Summary: UserRequestLogAggregates{ByModel: make([]UserRequestLogModelCount, 0)},
	}
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
	// Aggregate the same ownership/filter scope before adding any pagination.
	var totals struct {
		Total            int64
		AverageLatencyMS *float64
		Success          int64
		Failed           int64
	}
	if errAggregate := scope.Session(&gorm.Session{}).Select(`COUNT(*) AS total,
		AVG(CASE WHEN "usage"."latency_ms" > 0 THEN "usage"."latency_ms" END) AS average_latency_ms,
		COALESCE(SUM(CASE WHEN "usage"."failed" THEN 0 ELSE 1 END), 0) AS success,
		COALESCE(SUM(CASE WHEN "usage"."failed" THEN 1 ELSE 0 END), 0) AS failed`).
		Scan(&totals).Error; errAggregate != nil {
		return UserRequestLogResult{}, errAggregate
	}
	result.Total = totals.Total
	result.Summary.AverageLatencyMS = totals.AverageLatencyMS
	result.Summary.Status = UserRequestLogStatusCounts{Success: totals.Success, Failed: totals.Failed}
	if errModels := scope.Session(&gorm.Session{}).
		Select(`"usage"."model", COUNT(*) AS requests`).Group(`"usage"."model"`).
		Order(`requests DESC, "usage"."model" ASC`).Scan(&result.Summary.ByModel).Error; errModels != nil {
		return UserRequestLogResult{}, errModels
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
