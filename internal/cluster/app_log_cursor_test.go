package cluster

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

func appLogRecordIDs(result AppLogQueryResult) []uint {
	ids := make([]uint, len(result.Records))
	for i, record := range result.Records {
		ids[i] = record.ID
	}
	return ids
}

func seedAppLogRecords(t *testing.T, db *gorm.DB, times []time.Time) []AppLogRecord {
	t.Helper()
	records := make([]AppLogRecord, len(times))
	for i, timestamp := range times {
		records[i] = AppLogRecord{Timestamp: timestamp, CreatedAt: timestamp, Line: fmt.Sprintf("line-%d", i+1)}
	}
	if errCreate := db.Create(&records).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	return records
}

func TestListAppLogsCursorsWithEqualTimestamps(t *testing.T) {
	for _, base := range []time.Time{
		time.Date(2026, 5, 29, 1, 2, 0, 0, time.UTC),
		time.Date(2026, 5, 29, 1, 2, 0, 123456789, time.UTC),
		time.Date(2026, 5, 29, 1, 2, 0, 123456789, time.FixedZone("CST", 8*3600)),
	} {
		t.Run(base.Format(time.RFC3339Nano), func(t *testing.T) {
			db := openAppLogCursorTestDB(t)
			records := seedAppLogRecords(t, db, []time.Time{base, base, base, base})
			repo := NewRepository(db)
			for _, tc := range []struct {
				name  string
				query AppLogQuery
				want  []uint
			}{
				{"anchor", AppLogQuery{AnchorID: records[2].ID}, []uint{records[2].ID, records[1].ID, records[0].ID}},
				{"before", AppLogQuery{BeforeID: records[2].ID}, []uint{records[1].ID, records[0].ID}},
				{"after", AppLogQuery{AfterID: records[1].ID}, []uint{records[3].ID, records[2].ID}},
			} {
				result, errList := repo.ListAppLogs(context.Background(), tc.query)
				if errList != nil {
					t.Fatal(errList)
				}
				if got := appLogRecordIDs(result); !reflect.DeepEqual(got, tc.want) {
					t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
				}
			}
		})
	}
}

func TestListAppLogsAnchorExcludesLateArrivals(t *testing.T) {
	db := openAppLogCursorTestDB(t)
	base := time.Date(2026, 5, 29, 1, 2, 0, 0, time.UTC)
	records := seedAppLogRecords(t, db, []time.Time{base.Add(time.Second), base.Add(2 * time.Second), base.Add(3 * time.Second), base.Add(4 * time.Second)})
	repo := NewRepository(db)
	first, errFirst := repo.ListAppLogs(context.Background(), AppLogQuery{Limit: 2})
	if errFirst != nil {
		t.Fatal(errFirst)
	}
	seedAppLogRecords(t, db, []time.Time{base.Add(3500 * time.Millisecond)})
	second, errSecond := repo.ListAppLogs(context.Background(), AppLogQuery{Limit: 2, Offset: 2, AnchorID: first.AnchorID})
	if errSecond != nil {
		t.Fatal(errSecond)
	}
	want := []uint{records[1].ID, records[0].ID}
	if got := appLogRecordIDs(second); second.Total != first.Total || !reflect.DeepEqual(got, want) {
		t.Errorf("snapshot drift: first=%v total=%d anchor=%d; second=%v total=%d, want %v total=%d", appLogRecordIDs(first), first.Total, first.AnchorID, got, second.Total, want, first.Total)
	}
}

func TestListAppLogsRejectsMissingCursor(t *testing.T) {
	db := openAppLogCursorTestDB(t)
	base := time.Date(2026, 5, 29, 1, 2, 0, 0, time.UTC)
	records := seedAppLogRecords(t, db, []time.Time{base.Add(4 * time.Second), base.Add(time.Second), base.Add(3 * time.Second), base.Add(2 * time.Second)})
	repo := NewRepository(db)
	query := AppLogQuery{BeforeID: records[2].ID}
	original, errOriginal := repo.ListAppLogs(context.Background(), query)
	if errOriginal != nil {
		t.Fatal(errOriginal)
	}
	if errDelete := db.Delete(&records[2]).Error; errDelete != nil {
		t.Fatal(errDelete)
	}
	for _, missingQuery := range []AppLogQuery{{BeforeID: records[2].ID}, {AfterID: records[2].ID}} {
		if _, errList := repo.ListAppLogs(context.Background(), missingQuery); !errors.Is(errList, ErrAppLogCursorNotFound) {
			t.Fatalf("missing cursor: got error %v, want ErrAppLogCursorNotFound", errList)
		}
	}
	if got := appLogRecordIDs(original); !reflect.DeepEqual(got, []uint{records[3].ID, records[1].ID}) {
		t.Fatalf("chronological before_id returned %v", got)
	}
	anchored, errAnchored := repo.ListAppLogs(context.Background(), AppLogQuery{AnchorID: records[2].ID})
	if errAnchored != nil || !reflect.DeepEqual(appLogRecordIDs(anchored), []uint{records[0].ID, records[1].ID}) {
		t.Fatalf("ingestion ceiling depends on a deleted anchor row: %+v, %v", anchored, errAnchored)
	}
}

func TestListAppLogsPropagatesCursorReadError(t *testing.T) {
	db := openAppLogCursorTestDB(t)
	base := time.Date(2026, 5, 29, 1, 2, 0, 0, time.UTC)
	records := seedAppLogRecords(t, db, []time.Time{base.Add(4 * time.Second), base.Add(time.Second), base.Add(3 * time.Second), base.Add(2 * time.Second)})
	injected := false
	injectedError := errors.New("injected transient cursor read failure")
	if errRegister := db.Callback().Row().Before("gorm:row").Register("test:cursor-error", func(tx *gorm.DB) {
		if !injected && tx.Statement.Table == "log" && len(tx.Statement.Selects) == 1 && tx.Statement.Selects[0] == "id, timestamp" {
			injected = true
			tx.AddError(injectedError)
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	result, errList := NewRepository(db).ListAppLogs(context.Background(), AppLogQuery{BeforeID: records[2].ID})
	if !injected {
		t.Fatal("cursor read failure was not injected")
	}
	if !errors.Is(errList, injectedError) {
		t.Errorf("cursor read error swallowed: returned IDs %v and error %v", appLogRecordIDs(result), errList)
	}
}

func openAppLogCursorTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, errOpen := OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "logs.db"))
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatal(errDB)
	}
	cleanup := func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Error(errClose)
		}
	}
	t.Cleanup(cleanup)
	if errMigrate := db.AutoMigrate(&AppLogRecord{}); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	return db
}

func TestListAppLogsAnchorUsesIngestionOrder(t *testing.T) {
	checkAppLogIngestionCeiling(t, openAppLogCursorTestDB(t))
}

func TestListAppLogsAnchorUsesIngestionOrderPostgres(t *testing.T) {
	checkAppLogIngestionCeiling(t, newPostgresQuiescenceRepository(t).db)
}

func checkAppLogIngestionCeiling(t *testing.T, db *gorm.DB) {
	t.Helper()
	base := time.Date(2026, 5, 29, 1, 2, 0, 0, time.UTC)
	records := seedAppLogRecords(t, db, []time.Time{base.Add(4 * time.Second), base.Add(time.Second), base.Add(3 * time.Second), base.Add(2 * time.Second)})
	repo := NewRepository(db)
	first, errFirst := repo.ListAppLogs(t.Context(), AppLogQuery{Limit: 2})
	if errFirst != nil {
		t.Fatal(errFirst)
	}
	if first.AnchorID != records[3].ID || !reflect.DeepEqual(appLogRecordIDs(first), []uint{records[0].ID, records[2].ID}) {
		t.Fatalf("first page did not separate ingestion ceiling from chronological order: %+v", first)
	}
	seedAppLogRecords(t, db, []time.Time{base.Add(3500 * time.Millisecond), base.Add(4 * time.Second), base.Add(5 * time.Second)})
	for _, query := range []AppLogQuery{
		{AnchorID: first.AnchorID, Offset: 2, Limit: 2},
		{AnchorID: first.AnchorID, BeforeID: records[2].ID, Limit: 2},
	} {
		result, errList := repo.ListAppLogs(t.Context(), query)
		if errList != nil {
			t.Fatal(errList)
		}
		if got := appLogRecordIDs(result); !reflect.DeepEqual(got, []uint{records[3].ID, records[1].ID}) {
			t.Fatalf("query %+v returned %v, want [%d %d]", query, got, records[3].ID, records[1].ID)
		}
	}
	after, errAfter := repo.ListAppLogs(t.Context(), AppLogQuery{AnchorID: first.AnchorID, AfterID: records[2].ID})
	if errAfter != nil || !reflect.DeepEqual(appLogRecordIDs(after), []uint{records[0].ID}) {
		t.Fatalf("after_id used ingestion order instead of timestamp order: %+v, %v", after, errAfter)
	}
}

func TestListAppLogsCountAndRowsShareSnapshot(t *testing.T) {
	checkAppLogReadSnapshot(t, openAppLogCursorTestDB(t))
}

func TestListAppLogsCountAndRowsShareSnapshotPostgres(t *testing.T) {
	checkAppLogReadSnapshot(t, newPostgresQuiescenceRepository(t).db)
}

func checkAppLogReadSnapshot(t *testing.T, db *gorm.DB) {
	t.Helper()
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatal(errDB)
	}
	sqlDB.SetMaxOpenConns(2)
	base := time.Date(2026, 5, 29, 1, 2, 0, 0, time.UTC)
	records := seedAppLogRecords(t, db, []time.Time{base, base.Add(time.Second)})
	mutated := false
	var errMutation error
	if errRegister := db.Callback().Query().After("gorm:query").Register("test:log-snapshot", func(tx *gorm.DB) {
		if !mutated && tx.Statement.Table == "log" && strings.HasPrefix(tx.Statement.SQL.String(), "SELECT count(*)") {
			mutated = true
			errMutation = db.Delete(&records[0]).Error
			if errMutation == nil {
				errMutation = db.Create(&AppLogRecord{Timestamp: base.Add(2 * time.Second), Line: "concurrent log"}).Error
			}
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	result, errList := NewRepository(db).ListAppLogs(t.Context(), AppLogQuery{})
	if errList != nil {
		t.Fatal(errList)
	}
	if !mutated {
		t.Fatal("concurrent write was not injected")
	}
	if errMutation != nil {
		t.Fatal(errMutation)
	}
	if result.Total != 2 || result.AnchorID != records[1].ID || !reflect.DeepEqual(appLogRecordIDs(result), []uint{records[1].ID, records[0].ID}) {
		t.Fatalf("inconsistent read snapshot: %+v", result)
	}
}
