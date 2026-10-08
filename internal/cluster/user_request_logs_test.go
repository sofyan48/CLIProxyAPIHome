package cluster

import (
	"context"
	"fmt"
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
	if errPage != nil || page.Total != 1 || page.Limit != 200 || len(page.Items) != 0 {
		t.Fatalf("page = %#v, %v", page, errPage)
	}
	result, errExact := repo.ListUserRequestLogs(ctx, owner, UserRequestLogQuery{RequestID: "equest-4"})
	if errExact != nil || result.Total != 0 || len(result.Items) != 0 {
		t.Fatalf("eight-character suffix matched: %#v, %v", result, errExact)
	}
}
