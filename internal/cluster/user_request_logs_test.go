package cluster

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestListUserRequestLogsRequiresHistoricalAttribution(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	owner := uint(11)
	other := uint(22)
	keyOwner := owner
	key := APIKeyRecord{APIKey: "legacy-current-key", UserID: &keyOwner}
	if errCreate := db.Create(&key).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	stamp := time.Date(2026, 6, 10, 1, 0, 0, 0, time.UTC)
	// Include missing, null, zero, foreign, and genuine snapshot ownership. The
	// payload and current key binding must not rescue unattributed legacy rows.
	for index, attribution := range []*uint{nil, nil, new(uint), &other, &owner} {
		usage := UsageRecord{
			Timestamp: stamp, APIKey: key.APIKey, RequestID: fmt.Sprintf("request-%d", index),
			Model: fmt.Sprintf("model-%d", index), LatencyMS: int64(index * 10),
			EventType: "completion", Failed: index == 4, FailBody: "sensitive-marker",
			PayloadJSON: JSONB(`{"user_id":11,"user":"owner","secret":"sensitive-marker"}`),
			TotalTokens: 100, TokenAccountingVersion: UsageTokenAccountingSchemaVersion, AccountingTotalTokens: 123,
		}
		if errCreate := db.Create(&usage).Error; errCreate != nil {
			t.Fatal(errCreate)
		}
		if index == 0 {
			continue
		}
		charge := BillingChargeRecord{
			ID: fmt.Sprintf("charge-%d", index), UsageID: usage.ID, PayloadHash: fmt.Sprintf("hash-%d", index),
			UserID: attribution, PriceSnapshot: JSONB(`{}`), CreatedAt: stamp,
		}
		if errCreate := db.Create(&charge).Error; errCreate != nil {
			t.Fatal(errCreate)
		}
	}
	if _, errZero := repo.ListUserRequestLogs(ctx, 0, UserRequestLogQuery{}); errZero == nil {
		t.Fatal("zero user must fail closed")
	}
	check := func() {
		t.Helper()
		result, errList := repo.ListUserRequestLogs(ctx, owner, UserRequestLogQuery{})
		if errList != nil {
			t.Fatal(errList)
		}
		latency := 40.0
		wantSummary := UserRequestLogAggregates{
			ByModel:          []UserRequestLogModelCount{{Model: "model-4", Requests: 1}},
			AverageLatencyMS: &latency, Status: UserRequestLogStatusCounts{Failed: 1},
		}
		if !reflect.DeepEqual(result.Summary, wantSummary) {
			t.Fatalf("summary = %#v want %#v", result.Summary, wantSummary)
		}
		if result.Total != 1 || len(result.Items) != 1 || result.Items[0].RequestID != "request-4" || result.Items[0].Status != "failed" || result.Items[0].Tokens != 123 {
			t.Fatalf("result = %#v", result)
		}
	}
	check()
	if errReassign := db.Model(&key).Update("user_id", other).Error; errReassign != nil {
		t.Fatal(errReassign)
	}
	check()
	if errDelete := db.Unscoped().Delete(&key).Error; errDelete != nil {
		t.Fatal(errDelete)
	}
	check()
	page, errPage := repo.ListUserRequestLogs(ctx, owner, UserRequestLogQuery{Limit: 500, Offset: 1})
	if errPage != nil || page.Total != 1 || page.Limit != 200 || len(page.Items) != 0 || page.Summary.Status.Failed != 1 || len(page.Summary.ByModel) != 1 {
		t.Fatalf("page = %#v, %v", page, errPage)
	}
	result, errExact := repo.ListUserRequestLogs(ctx, owner, UserRequestLogQuery{RequestID: "equest-4"})
	if errExact != nil || result.Total != 0 || len(result.Items) != 0 || !reflect.DeepEqual(result.Summary, UserRequestLogAggregates{ByModel: []UserRequestLogModelCount{}}) {
		t.Fatalf("eight-character suffix matched: %#v, %v", result, errExact)
	}
}

func TestListUserRequestLogsAggregates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	owner, other := uint(11), uint(22)
	stamp := time.Date(2026, 6, 10, 1, 0, 0, 0, time.UTC)
	for index, row := range []struct {
		user    uint
		model   string
		latency int64
		failed  bool
	}{
		{owner, "z-model", 10, false},
		{owner, "z-model", 21, true},
		{owner, "a-model", 0, false},
		{owner, "b-model", -100, true},
		{other, "private-model", 1000, true},
	} {
		usage := UsageRecord{Timestamp: stamp.Add(time.Duration(index) * time.Minute),
			RequestID: fmt.Sprintf("aggregate-%d", index), Model: row.model, LatencyMS: row.latency, Failed: row.failed, PayloadJSON: JSONB(`{}`)}
		if errCreate := db.Create(&usage).Error; errCreate != nil {
			t.Fatal(errCreate)
		}
		charge := BillingChargeRecord{ID: fmt.Sprintf("aggregate-charge-%d", index), UsageID: usage.ID,
			PayloadHash: fmt.Sprintf("aggregate-hash-%d", index), UserID: &row.user, PriceSnapshot: JSONB(`{}`), CreatedAt: stamp}
		if errCreate := db.Create(&charge).Error; errCreate != nil {
			t.Fatal(errCreate)
		}
	}
	end := stamp.Add(2 * time.Minute)
	later := stamp.Add(4 * time.Minute)
	average, otherAverage, firstAverage := 15.5, 1000.0, 10.0
	full := UserRequestLogAggregates{
		ByModel:          []UserRequestLogModelCount{{Model: "z-model", Requests: 2}, {Model: "a-model", Requests: 1}, {Model: "b-model", Requests: 1}},
		AverageLatencyMS: &average, Status: UserRequestLogStatusCounts{Success: 2, Failed: 2},
	}
	for _, test := range []struct {
		name  string
		user  uint
		query UserRequestLogQuery
		want  UserRequestLogAggregates
	}{
		{"all", owner, UserRequestLogQuery{}, full},
		{"page", owner, UserRequestLogQuery{Limit: 1, Offset: 1}, full},
		{"past last page", owner, UserRequestLogQuery{Limit: 1, Offset: 100}, full},
		{"other user", other, UserRequestLogQuery{}, UserRequestLogAggregates{
			ByModel: []UserRequestLogModelCount{{Model: "private-model", Requests: 1}}, AverageLatencyMS: &otherAverage, Status: UserRequestLogStatusCounts{Failed: 1}}},
		{"date range", owner, UserRequestLogQuery{From: &stamp, To: &end}, UserRequestLogAggregates{
			ByModel: []UserRequestLogModelCount{{Model: "z-model", Requests: 2}}, AverageLatencyMS: &average, Status: UserRequestLogStatusCounts{Success: 1, Failed: 1}}},
		{"unmeasured", owner, UserRequestLogQuery{From: &end, To: &later}, UserRequestLogAggregates{
			ByModel: []UserRequestLogModelCount{{Model: "a-model", Requests: 1}, {Model: "b-model", Requests: 1}}, Status: UserRequestLogStatusCounts{Success: 1, Failed: 1}}},
		{"exact request", owner, UserRequestLogQuery{From: &stamp, To: &end, RequestID: "aggregate-0"}, UserRequestLogAggregates{
			ByModel: []UserRequestLogModelCount{{Model: "z-model", Requests: 1}}, AverageLatencyMS: &firstAverage, Status: UserRequestLogStatusCounts{Success: 1}}},
		{"request outside range", owner, UserRequestLogQuery{From: &end, RequestID: "aggregate-0"}, UserRequestLogAggregates{ByModel: []UserRequestLogModelCount{}}},
		{"foreign request", owner, UserRequestLogQuery{RequestID: "aggregate-4"}, UserRequestLogAggregates{ByModel: []UserRequestLogModelCount{}}},
		{"equal boundaries", owner, UserRequestLogQuery{From: &stamp, To: &stamp}, UserRequestLogAggregates{ByModel: []UserRequestLogModelCount{}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, errList := repo.ListUserRequestLogs(ctx, test.user, test.query)
			if errList != nil {
				t.Fatal(errList)
			}
			if !reflect.DeepEqual(result.Summary, test.want) || result.Total != test.want.Status.Success+test.want.Status.Failed {
				t.Fatalf("result=%#v want summary=%#v", result, test.want)
			}
		})
	}
}
