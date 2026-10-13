package cluster

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestUserResetAllowanceMigration(t *testing.T) {
	db := openDatabaseSnapshotSQLiteRawTestDB(t, filepath.Join(t.TempDir(), "legacy.db"))
	if errMigrate := db.AutoMigrate(&databaseSnapshotV9UserRecord{}); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	old := databaseSnapshotV9UserRecord{Username: "legacy-reset", Credits: 42, Timezone: "Asia/Shanghai"}
	if errCreate := db.Create(&old).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	var user UserRecord
	if errFirst := db.First(&user, old.ID).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if user.Credits != 42 || user.ResetAllowanceMode != "" || user.ResetAllowanceAvailable != 0 || user.ResetAllowanceAmount != 0 || user.ResetAllowanceExpiresAt != nil || user.ResetAllowanceRenewsAt != nil {
		t.Fatalf("migrated user = %+v", user)
	}
	repo := NewRepository(db)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	if _, errSet := repo.SetUserResetAllowance(t.Context(), user.ID, ResetAllowanceMonthly, 3, nil, now); errSet != nil {
		t.Fatal(errSet)
	}
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	saved, errGet := repo.GetUser(t.Context(), user.ID)
	if errGet != nil || saved.ResetAllowanceAmount != 3 || saved.ResetAllowanceAvailable != 3 {
		t.Fatalf("repeat migration = %+v %v", saved, errGet)
	}
	snapshotPath := filepath.Join(t.TempDir(), "allowance.zip")
	if _, errExport := exportDatabaseSnapshotVersion(t.Context(), db, snapshotPath, currentDatabaseVersion); errExport != nil {
		t.Fatal(errExport)
	}
	snapshot, errOpen := OpenDatabaseSnapshot(t.Context(), snapshotPath)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	t.Cleanup(func() {
		if errClose := snapshot.Close(); errClose != nil {
			t.Error(errClose)
		}
	})
	target := openDatabaseSnapshotSQLiteRawTestDB(t, filepath.Join(t.TempDir(), "restored.db"))
	if _, errImport := ImportDatabaseSnapshot(t.Context(), target, snapshot, nil); errImport != nil {
		t.Fatal(errImport)
	}
	restored, errRestore := NewRepository(target).GetUser(t.Context(), user.ID)
	if errRestore != nil || restored.ResetAllowanceMode != ResetAllowanceMonthly || restored.ResetAllowanceAmount != 3 || restored.ResetAllowanceAvailable != 3 || restored.ResetAllowanceRenewsAt == nil || !restored.ResetAllowanceRenewsAt.Equal(*saved.ResetAllowanceRenewsAt) {
		t.Fatalf("restored allowance = %+v %v", restored, errRestore)
	}
	for version := 1; version <= currentDatabaseVersion; version++ {
		models, ok := databaseSnapshotModels(version)
		if !ok {
			t.Fatalf("unsupported version %d", version)
		}
		_, hasAllowance := databaseSnapshotModelType(t, models, "user").Elem().FieldByName("ResetAllowanceMode")
		if hasAllowance != (version >= 10) {
			t.Fatalf("snapshot v%d allowance shape changed", version)
		}
	}
}

func TestUserResetAllowanceOneTimeAllEnabled(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()
	user := createPeriodLimitUser(t, repo, ctx, "reset-all", 100)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	if errUpdate := db.Model(user).Updates(map[string]any{"limit_5h_credits": 0, "limit_1d_credits": 10, "limit_7d_credits": 20, "limit_30d_credits": 30, "window_mode_1d": "sliding", "window_mode_7d": "calendar"}).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	createCharge(t, repo, ctx, user.ID, 3, old)
	if _, errSet := repo.SetUserResetAllowance(ctx, user.ID, ResetAllowanceOneTime, 1, nil, now); errSet != nil {
		t.Fatal(errSet)
	}
	status, errConsume := repo.ConsumeUserResetAllowance(ctx, user.ID, now)
	if errConsume != nil {
		t.Fatal(errConsume)
	}
	if status.ResetAllowance.Available != 0 || status.Credits != 100 {
		t.Fatalf("status = %+v", status)
	}
	for _, window := range status.Windows {
		if !window.Enabled || window.Used != 0 || window.UsageEpoch == nil || !window.UsageEpoch.Equal(now) {
			t.Fatalf("window = %+v", window)
		}
	}
	saved, errGet := repo.GetUser(ctx, user.ID)
	if errGet != nil {
		t.Fatal(errGet)
	}
	for _, id := range []string{PeriodWindow5h, PeriodWindow1d, PeriodWindow7d, PeriodWindow30d} {
		if userWindowStart(saved, id) != nil {
			t.Fatalf("%s start was not cleared", id)
		}
	}
	sum, errSum := repo.SumUserChargeAmountSince(ctx, user.ID, old.Add(-time.Hour))
	if errSum != nil || sum != 3 {
		t.Fatalf("history sum=%v error=%v", sum, errSum)
	}
	if _, errAgain := repo.ConsumeUserResetAllowance(ctx, user.ID, now.Add(time.Second)); !errors.Is(errAgain, ErrResetAllowanceUnavailable) {
		t.Fatalf("second consume = %v", errAgain)
	}
}

func TestUserResetAllowanceDisabledAndFailures(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()
	user := createPeriodLimitUser(t, repo, ctx, "disabled", 50)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	if _, errSet := repo.SetUserResetAllowance(ctx, user.ID, ResetAllowanceOneTime, 2, nil, now); errSet != nil {
		t.Fatal(errSet)
	}
	if _, errConsume := repo.ConsumeUserResetAllowance(ctx, user.ID, now); !errors.Is(errConsume, ErrResetAllowanceNoWindows) {
		t.Fatalf("no windows error = %v", errConsume)
	}
	saved, errGet := repo.GetUser(ctx, user.ID)
	if errGet != nil || saved.ResetAllowanceAvailable != 2 {
		t.Fatalf("failed reset consumed allowance: %+v %v", saved, errGet)
	}
	if errUpdate := db.Model(user).Updates(map[string]any{"limit_5h_credits": 0, "period_window_start_1d": old, "usage_epoch_1d": old}).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if _, errConsume := repo.ConsumeUserResetAllowance(ctx, user.ID, now); errConsume != nil {
		t.Fatal(errConsume)
	}
	saved, errGet = repo.GetUser(ctx, user.ID)
	if errGet != nil {
		t.Fatal(errGet)
	}
	if saved.PeriodWindowStart1d == nil || !saved.PeriodWindowStart1d.Equal(old) || saved.UsageEpoch1d == nil || !saved.UsageEpoch1d.Equal(old) {
		t.Fatalf("disabled state changed: %+v", saved)
	}
	// Force post-update status construction to fail; both decrement and reset must roll back.
	if errDrop := db.Migrator().DropTable(&BillingChargeRecord{}); errDrop != nil {
		t.Fatal(errDrop)
	}
	if _, errConsume := repo.ConsumeUserResetAllowance(ctx, user.ID, now.Add(time.Minute)); errConsume == nil {
		t.Fatal("expected status query failure")
	}
	saved, errGet = repo.GetUser(ctx, user.ID)
	if errGet != nil || saved.ResetAllowanceAvailable != 1 || saved.UsageEpoch5h == nil || !saved.UsageEpoch5h.Equal(now) {
		t.Fatalf("rollback failed: %+v %v", saved, errGet)
	}
}

func TestUserResetAllowanceExpiryAndReplacement(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()
	user := createPeriodLimitUser(t, repo, ctx, "expiry", 100)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	if _, errUpdate := repo.UpdateUser(ctx, user.ID, UserUpdate{Limit5hCredits: OptionalFloatUpdate{Set: true, Value: 0}}); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	expiry := now.Add(time.Hour)
	for _, at := range []time.Time{expiry, expiry.Add(time.Second)} {
		if _, errSet := repo.SetUserResetAllowance(ctx, user.ID, ResetAllowanceOneTime, 2, &expiry, now); errSet != nil {
			t.Fatal(errSet)
		}
		if _, errConsume := repo.ConsumeUserResetAllowance(ctx, user.ID, at); !errors.Is(errConsume, ErrResetAllowanceUnavailable) {
			t.Fatalf("expiry error = %v", errConsume)
		}
		saved, errGet := repo.GetUser(ctx, user.ID)
		if errGet != nil || saved.ResetAllowanceAvailable != 2 || UserResetAllowanceStatusAt(saved, at).Available != 0 {
			t.Fatalf("expiry changed balance: %+v %v", saved, errGet)
		}
	}
	if _, errSet := repo.SetUserResetAllowance(ctx, user.ID, ResetAllowanceMonthly, 2, &expiry, now); errSet == nil {
		t.Fatal("monthly expiry accepted")
	}
	if _, errSet := repo.SetUserResetAllowance(ctx, user.ID, ResetAllowanceOneTime, -1, nil, now); errSet == nil {
		t.Fatal("negative amount accepted")
	}
	if _, errSet := repo.SetUserResetAllowance(ctx, user.ID, "invalid", 1, nil, now); errSet == nil {
		t.Fatal("invalid mode accepted")
	}
	saved, errSet := repo.SetUserResetAllowance(ctx, user.ID, ResetAllowanceOneTime, 0, nil, now)
	if errSet != nil || saved.ResetAllowanceAvailable != 0 || saved.ResetAllowanceExpiresAt != nil {
		t.Fatalf("replacement = %+v %v", saved, errSet)
	}
}

func TestUserResetAllowanceMonthlyTimezoneNoCarry(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()
	user := createPeriodLimitUser(t, repo, ctx, "monthly", 100)
	timezone := "America/New_York"
	if _, errUpdate := repo.UpdateUser(ctx, user.ID, UserUpdate{Timezone: &timezone, Limit5hCredits: OptionalFloatUpdate{Set: true, Value: 10}}); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	now := time.Date(2026, 2, 28, 23, 0, 0, 0, time.UTC)
	renewal := time.Date(2026, 3, 1, 5, 0, 0, 0, time.UTC)
	saved, errSet := repo.SetUserResetAllowance(ctx, user.ID, ResetAllowanceMonthly, 3, nil, now)
	if errSet != nil || saved.ResetAllowanceAvailable != 3 || saved.ResetAllowanceRenewsAt == nil || !saved.ResetAllowanceRenewsAt.Equal(renewal) {
		t.Fatalf("initial allowance = %+v %v", saved, errSet)
	}
	if _, errConsume := repo.ConsumeUserResetAllowance(ctx, user.ID, now); errConsume != nil {
		t.Fatal(errConsume)
	}
	saved, errGet := repo.GetUser(ctx, user.ID)
	if errGet != nil {
		t.Fatal(errGet)
	}
	if got := UserResetAllowanceStatusAt(saved, renewal.Add(-time.Nanosecond)); got.Available != 2 {
		t.Fatalf("premature renewal: %+v", got)
	}
	if got := UserResetAllowanceStatusAt(saved, renewal); got.Available != 3 {
		t.Fatalf("no renewal: %+v", got)
	}
	status, errConsume := repo.ConsumeUserResetAllowance(ctx, user.ID, renewal)
	next := time.Date(2026, 4, 1, 4, 0, 0, 0, time.UTC)
	if errConsume != nil || status.ResetAllowance.Available != 2 || status.ResetAllowance.RenewsAt == nil || !status.ResetAllowance.RenewsAt.Equal(next) {
		t.Fatalf("renewed status = %+v %v", status, errConsume)
	}
	// Multiple skipped months replenish once, never accumulate.
	status, errConsume = repo.ConsumeUserResetAllowance(ctx, user.ID, next.AddDate(0, 3, 1))
	if errConsume != nil || status.ResetAllowance.Available != 2 {
		t.Fatalf("carry accumulated: %+v %v", status, errConsume)
	}
}

func TestUserResetAllowanceConcurrentConsumption(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()
	testResetAllowanceConcurrentConsumption(t, repo, ctx)
}

func TestUserResetAllowancePostgresConcurrentConsumption(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	testResetAllowanceConcurrentConsumption(t, repo, ctx)
}

func testResetAllowanceConcurrentConsumption(t *testing.T, repo *Repository, ctx context.Context) {
	t.Helper()
	user := createPeriodLimitUser(t, repo, ctx, "concurrent", 100)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	if _, errUpdate := repo.UpdateUser(ctx, user.ID, UserUpdate{Limit5hCredits: OptionalFloatUpdate{Set: true, Value: 10}}); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if _, errSet := repo.SetUserResetAllowance(ctx, user.ID, ResetAllowanceOneTime, 1, nil, now); errSet != nil {
		t.Fatal(errSet)
	}
	start := make(chan struct{})
	results := make(chan error, 12)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errConsume := repo.ConsumeUserResetAllowance(ctx, user.ID, now)
			results <- errConsume
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for errConsume := range results {
		if errConsume == nil {
			successes++
		} else if !errors.Is(errConsume, ErrResetAllowanceUnavailable) {
			t.Fatalf("unexpected error: %v", errConsume)
		}
	}
	saved, errGet := repo.GetUser(ctx, user.ID)
	if errGet != nil || successes != 1 || saved.ResetAllowanceAvailable != 0 {
		t.Fatalf("successes=%d user=%+v error=%v", successes, saved, errGet)
	}
}
