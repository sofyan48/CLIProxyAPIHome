package cluster

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

func usageObservabilityIsPostgres(db *gorm.DB) bool {
	return db != nil && db.Dialector != nil && db.Dialector.Name() == "postgres"
}

// percentile_disc preserves the existing nearest-rank semantics, including ties
// and zero latency. Negative latencies remain excluded from the distribution.
func usageObservabilityPostgresPercentile(column string, percentile int) string {
	return fmt.Sprintf("percentile_disc(%d / 100.0) WITHIN GROUP (ORDER BY %s) FILTER (WHERE %s >= 0)", percentile, column, column)
}

// usageObservabilityOverviewGroupsPostgres computes percentiles in the same scan
// as each ranking, avoiding an additional window sort and join per section.
func usageObservabilityOverviewGroupsPostgres(db *gorm.DB, query UsageObservabilityRecordQuery) (UsageObservabilityTopGroups, []UsageObservabilityAggregateItem, error) {
	top := UsageObservabilityTopGroups{
		Users: []UsageObservabilityAggregateItem{}, ClientKeys: []UsageObservabilityAggregateItem{},
		Credentials: []UsageObservabilityAggregateItem{}, Providers: []UsageObservabilityAggregateItem{},
		Models: []UsageObservabilityAggregateItem{}, Endpoints: []UsageObservabilityAggregateItem{}, Errors: []UsageObservabilityAggregateItem{},
	}
	efficiency := []UsageObservabilityAggregateItem{}
	for _, group := range []struct {
		name  string
		items *[]UsageObservabilityAggregateItem
	}{
		{"user", &top.Users}, {"client_key", &top.ClientKeys},
		{"provider", &top.Providers}, {"endpoint", &top.Endpoints}, {"status_code", &top.Errors},
	} {
		metric := "request_count"
		groupQuery := query
		if group.name == "status_code" {
			metric, groupQuery.Status = "failed_count", "failed"
		}
		var rows []usageObservabilityAggregateRow
		if errScan := db.Table("(?) AS scoped", usageObservabilityOverviewGroupPostgres(db, groupQuery, group.name)).
			Order(usageObservabilityPreAggregatedSQLOrder(metric, "desc")).Limit(10).Scan(&rows).Error; errScan != nil {
			return UsageObservabilityTopGroups{}, nil, errScan
		}
		for index := range rows {
			*group.items = append(*group.items, usageObservabilityAggregateItemFromRow(&rows[index], group.name))
		}
	}
	// Preserve credential pre-aggregation: resolving auth identities before
	// grouping would join auth three times for every request instead of per index.
	var errCredentials error
	top.Credentials, errCredentials = usageObservabilityAggregateItemsSQL(db, query, "credential", "request_count", "desc", 10)
	if errCredentials != nil {
		return UsageObservabilityTopGroups{}, nil, errCredentials
	}
	// Share the small model aggregate between the two rankings. Materializing
	// only grouped rows avoids spooling the entire usage window to temporary disk.
	var modelRows []struct {
		Efficiency bool                           `gorm:"column:efficiency"`
		Row        usageObservabilityAggregateRow `gorm:"embedded"`
	}
	models := db.Table("model_aggregate AS scoped").Select("FALSE AS efficiency, scoped.*").
		Order(usageObservabilityPreAggregatedSQLOrder("request_count", "desc")).Limit(10)
	efficientModels := db.Table("model_aggregate AS scoped").Select("TRUE AS efficiency, scoped.*").
		Order(usageObservabilityPreAggregatedSQLOrder("total_tokens", "desc")).Limit(10)
	// The final ordering is explicit because UNION ALL does not guarantee that
	// the two inputs' order is preserved.
	statement := `WITH model_aggregate AS MATERIALIZED (?)
		SELECT * FROM ((?) UNION ALL (?)) AS ranked
		ORDER BY efficiency, CASE WHEN efficiency THEN accounting_total_tokens ELSE request_count END DESC,
		last_used_at DESC, aggregate_label ASC`
	if errScan := db.Raw(statement, usageObservabilityOverviewGroupPostgres(db, query, "model"), models, efficientModels).Scan(&modelRows).Error; errScan != nil {
		return UsageObservabilityTopGroups{}, nil, errScan
	}
	for _, row := range modelRows {
		item := usageObservabilityAggregateItemFromRow(&row.Row, "model")
		if row.Efficiency {
			efficiency = append(efficiency, item)
		} else {
			top.Models = append(top.Models, item)
		}
	}
	return top, efficiency, nil
}

func usageObservabilityOverviewGroupPostgres(db *gorm.DB, query UsageObservabilityRecordQuery, group string) *gorm.DB {
	base := usageObservabilityAggregateBaseQuery(db, query, group)
	selectSQL := strings.Replace(usageObservabilityAggregateSQLSelect(false), "NULL AS p95_latency_ms", usageObservabilityPostgresPercentile("scoped.latency_ms", 95)+" AS p95_latency_ms", 1)
	return db.Table("(?) AS scoped", base).Select(selectSQL).Group("scoped.aggregate_id")
}
