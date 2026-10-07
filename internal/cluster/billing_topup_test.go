package cluster

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"sync"
	"testing"

	"gorm.io/gorm"
)

func TestBillingTopupApproval(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	name, otherName, credits := "topup-user", "other", 10.0
	user, errCreate := repo.CreateUser(ctx, UserUpdate{Username: &name, Credits: &credits})
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	other, errOther := repo.CreateUser(ctx, UserUpdate{Username: &otherName})
	if errOther != nil {
		t.Fatal(errOther)
	}
	request, balance, errRequest := repo.CreateBillingRechargeRequest(ctx, user.ID, 12.5, " note ")
	if errRequest != nil || balance != 10 || request.Status != "pending" || request.Note != "note" {
		t.Fatalf("request=%+v balance=%v error=%v", request, balance, errRequest)
	}
	ledger, errLedger := repo.ListBillingBalanceRecords(ctx, BillingBalanceQuery{UserID: &user.ID})
	if errLedger != nil || ledger.Total != 0 {
		t.Fatalf("ledger=%+v error=%v", ledger, errLedger)
	}
	stored, errStored := repo.GetUser(ctx, user.ID)
	if errStored != nil || stored.Credits != 10 {
		t.Fatalf("user=%+v error=%v", stored, errStored)
	}
	if _, _, errDuplicate := repo.CreateBillingRechargeRequest(ctx, user.ID, 1, ""); !errors.Is(errDuplicate, ErrTopupPending) {
		t.Fatalf("duplicate=%v", errDuplicate)
	}
	for _, pair := range [][2]uint{{other.ID, request.ID}, {user.ID, request.ID + 1}, {user.ID, 0}} {
		if _, _, errWrong := repo.ApproveBillingRechargeRequest(ctx, pair[0], pair[1]); !errors.Is(errWrong, gorm.ErrRecordNotFound) {
			t.Fatalf("wrong approval=%v", errWrong)
		}
	}
	approvedUser, record, errApprove := repo.ApproveBillingRechargeRequest(ctx, user.ID, request.ID)
	if errApprove != nil || approvedUser.Credits != 22.5 || record.Amount != 12.5 || record.Note != "note" || record.Operator != "admin" {
		t.Fatalf("user=%+v record=%+v error=%v", approvedUser, record, errApprove)
	}
	next, _, errNext := repo.CreateBillingRechargeRequest(ctx, user.ID, 2, "next")
	if errNext != nil {
		t.Fatal(errNext)
	}
	replayUser, replay, errReplay := repo.ApproveBillingRechargeRequest(ctx, user.ID, request.ID)
	if errReplay != nil || replay.ID != record.ID || replayUser.Credits != 22.5 {
		t.Fatalf("replay=%+v user=%+v error=%v", replay, replayUser, errReplay)
	}
	pending, errPending := repo.PendingBillingRechargeRequests(ctx, []uint{user.ID, other.ID})
	if errPending != nil || len(pending) != 1 || pending[user.ID].ID != next.ID {
		t.Fatalf("pending=%+v error=%v", pending, errPending)
	}
	ledger, errLedger = repo.ListBillingBalanceRecords(ctx, BillingBalanceQuery{UserID: &user.ID})
	if errLedger != nil || ledger.Total != 1 {
		t.Fatalf("ledger=%+v error=%v", ledger, errLedger)
	}
}

func TestBillingTopupConcurrentRequestsAndApprovals(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	testBillingTopupConcurrentRequestsAndApprovals(t, repo)
}

func TestBillingTopupConcurrentRequestsAndApprovalsPostgres(t *testing.T) {
	testBillingTopupConcurrentRequestsAndApprovals(t, newPostgresQuiescenceRepository(t))
}

func testBillingTopupConcurrentRequestsAndApprovals(t *testing.T, repo *Repository) {
	t.Helper()
	ctx := context.Background()
	name := "concurrent-topup"
	user, errCreate := repo.CreateUser(ctx, UserUpdate{Username: &name})
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errRequest := repo.CreateBillingRechargeRequest(ctx, user.ID, 3, "")
			results <- errRequest
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for errResult := range results {
		if errResult == nil {
			successes++
		} else if !errors.Is(errResult, ErrTopupPending) {
			t.Fatal(errResult)
		}
	}
	if successes != 1 {
		t.Fatalf("successes=%d", successes)
	}
	pending, errPending := repo.PendingBillingRechargeRequests(ctx, []uint{user.ID})
	if errPending != nil {
		t.Fatal(errPending)
	}
	results = make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errApprove := repo.ApproveBillingRechargeRequest(ctx, user.ID, pending[user.ID].ID)
			results <- errApprove
		}()
	}
	wg.Wait()
	close(results)
	for errResult := range results {
		if errResult != nil {
			t.Fatal(errResult)
		}
	}
	stored, errStored := repo.GetUser(ctx, user.ID)
	ledger, errLedger := repo.ListBillingBalanceRecords(ctx, BillingBalanceQuery{UserID: &user.ID})
	if errStored != nil || errLedger != nil || stored.Credits != 3 || ledger.Total != 1 {
		t.Fatalf("user=%+v ledger=%+v errors=%v %v", stored, ledger, errStored, errLedger)
	}
}

func TestBillingTopupValidationAndAtomicRollback(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	name, credits := "overflow-topup", math.MaxFloat64
	user, errCreate := repo.CreateUser(ctx, UserUpdate{Username: &name, Credits: &credits})
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	for _, amount := range []float64{0, -1, math.NaN(), math.Inf(1), math.MaxFloat64, 1} {
		if _, _, errRequest := repo.CreateBillingRechargeRequest(ctx, user.ID, amount, ""); !errors.Is(errRequest, ErrInvalidTopupAmount) {
			t.Fatalf("amount=%v error=%v", amount, errRequest)
		}
	}
	zero := 0.0
	if _, errUpdate := repo.UpdateUser(ctx, user.ID, UserUpdate{Credits: &zero}); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	request, _, errRequest := repo.CreateBillingRechargeRequest(ctx, user.ID, math.MaxFloat64, "")
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	if _, errUpdate := repo.UpdateUser(ctx, user.ID, UserUpdate{Credits: &credits}); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if _, _, errApprove := repo.ApproveBillingRechargeRequest(ctx, user.ID, request.ID); !errors.Is(errApprove, ErrInvalidTopupAmount) {
		t.Fatal(errApprove)
	}
	pending, errPending := repo.PendingBillingRechargeRequests(ctx, []uint{user.ID})
	ledger, errLedger := repo.ListBillingBalanceRecords(ctx, BillingBalanceQuery{UserID: &user.ID})
	if errPending != nil || errLedger != nil || pending[user.ID] == nil || ledger.Total != 0 {
		t.Fatalf("pending=%+v ledger=%+v errors=%v %v", pending, ledger, errPending, errLedger)
	}
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	duplicate := BillingRechargeRequestRecord{UserID: user.ID, Amount: 1, Status: "pending"}
	if errDuplicate := db.Create(&duplicate).Error; errDuplicate == nil {
		t.Fatal("database allowed duplicate pending request")
	}
}

func TestBillingTopupApprovalRollsBackLedgerAndCredits(t *testing.T) {
	repo, closeRepo := newBillingTestRepository(t, t.Context())
	defer closeRepo()
	name := "rollback-topup"
	user, errCreate := repo.CreateUser(t.Context(), UserUpdate{Username: &name})
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	request, _, errRequest := repo.CreateBillingRechargeRequest(t.Context(), user.ID, 5, "")
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	if errTrigger := db.Exec("CREATE TRIGGER fail_topup_approval BEFORE UPDATE ON billing_recharge_request BEGIN SELECT RAISE(ABORT, 'forced approval failure'); END").Error; errTrigger != nil {
		t.Fatal(errTrigger)
	}
	if _, _, errApprove := repo.ApproveBillingRechargeRequest(t.Context(), user.ID, request.ID); errApprove == nil {
		t.Fatal("approval should fail")
	}
	stored, errStored := repo.GetUser(t.Context(), user.ID)
	ledger, errLedger := repo.ListBillingBalanceRecords(t.Context(), BillingBalanceQuery{UserID: &user.ID})
	pending, errPending := repo.PendingBillingRechargeRequests(t.Context(), []uint{user.ID})
	if errStored != nil || errLedger != nil || errPending != nil || stored.Credits != 0 || ledger.Total != 0 || pending[user.ID] == nil {
		t.Fatalf("rollback user=%+v ledger=%+v pending=%+v errors=%v %v %v", stored, ledger, pending, errStored, errLedger, errPending)
	}
	if errDrop := db.Exec("DROP TRIGGER fail_topup_approval").Error; errDrop != nil {
		t.Fatal(errDrop)
	}
	if _, _, errApprove := repo.ApproveBillingRechargeRequest(t.Context(), user.ID, request.ID); errApprove != nil {
		t.Fatal(errApprove)
	}
}

func TestBillingTopupUnlimitedPreservesExistingBillingBehavior(t *testing.T) {
	repo, closeRepo := newBillingTestRepository(t, t.Context())
	defer closeRepo()
	name, unlimited, credits := "unlimited-topup", true, 10.0
	user, errCreate := repo.CreateUser(t.Context(), UserUpdate{Username: &name, CreditsUnlimited: &unlimited, Credits: &credits})
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	request, balance, errRequest := repo.CreateBillingRechargeRequest(t.Context(), user.ID, 5, "")
	if errRequest != nil || balance != 10 {
		t.Fatalf("request=%+v balance=%v error=%v", request, balance, errRequest)
	}
	approved, ledger, errApprove := repo.ApproveBillingRechargeRequest(t.Context(), user.ID, request.ID)
	if errApprove != nil || approved.Credits != 10 || ledger.Amount != 5 || ledger.BalanceBefore != 10 || ledger.BalanceAfter != 10 {
		t.Fatalf("user=%+v ledger=%+v error=%v", approved, ledger, errApprove)
	}
}

func TestBillingTopupMigrationAndSnapshots(t *testing.T) {
	ctx := context.Background()
	source := openDatabaseSnapshotSQLiteRawTestDB(t, filepath.Join(t.TempDir(), "source.db"))
	// Simulate the previous schema and then perform the current migration.
	for _, model := range databaseSnapshotV8Models {
		if errMigrate := source.AutoMigrate(model.newRecord()); errMigrate != nil {
			t.Fatal(errMigrate)
		}
	}
	if source.Migrator().HasTable(&BillingRechargeRequestRecord{}) {
		t.Fatal("v8 contains topups")
	}
	if errMigrate := AutoMigrate(source); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	repo := NewRepository(source)
	name := "snapshot-topup"
	user, errCreate := repo.CreateUser(ctx, UserUpdate{Username: &name})
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	approved, _, errRequest := repo.CreateBillingRechargeRequest(ctx, user.ID, 5, "approved")
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	_, record, errApprove := repo.ApproveBillingRechargeRequest(ctx, user.ID, approved.ID)
	if errApprove != nil {
		t.Fatal(errApprove)
	}
	pending, _, errPending := repo.CreateBillingRechargeRequest(ctx, user.ID, 7, "pending")
	if errPending != nil {
		t.Fatal(errPending)
	}
	if errMigrate := AutoMigrate(source); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	for _, version := range []int{8, 9} {
		path := filepath.Join(t.TempDir(), "snapshot.zip")
		if _, errExport := exportDatabaseSnapshotVersion(ctx, source, path, version); errExport != nil {
			t.Fatal(errExport)
		}
		snapshot, errOpen := OpenDatabaseSnapshot(ctx, path)
		if errOpen != nil {
			t.Fatal(errOpen)
		}
		target := openDatabaseSnapshotSQLiteRawTestDB(t, filepath.Join(t.TempDir(), "target.db"))
		_, errImport := ImportDatabaseSnapshot(ctx, target, snapshot, nil)
		if errClose := snapshot.Close(); errClose != nil {
			t.Fatal(errClose)
		}
		if errImport != nil {
			t.Fatal(errImport)
		}
		restored := NewRepository(target)
		requests, errRequests := restored.PendingBillingRechargeRequests(ctx, []uint{user.ID})
		if errRequests != nil {
			t.Fatal(errRequests)
		}
		if version == 8 {
			if len(requests) != 0 {
				t.Fatal("legacy snapshot created requests")
			}
			continue
		}
		if requests[user.ID] == nil || requests[user.ID].ID != pending.ID {
			t.Fatalf("restored=%+v", requests)
		}
		restoredUser, replay, errReplay := restored.ApproveBillingRechargeRequest(ctx, user.ID, approved.ID)
		if errReplay != nil || restoredUser.Credits != 5 || replay.ID != record.ID {
			t.Fatalf("restored user=%+v ledger=%+v error=%v", restoredUser, replay, errReplay)
		}
		subsequent, _, errSubsequent := restored.CreateBillingRechargeRequest(ctx, user.ID, 1, "")
		if !errors.Is(errSubsequent, ErrTopupPending) || subsequent != nil {
			t.Fatal(errSubsequent)
		}
		if _, _, errApprovePending := restored.ApproveBillingRechargeRequest(ctx, user.ID, pending.ID); errApprovePending != nil {
			t.Fatal(errApprovePending)
		}
		next, _, errNext := restored.CreateBillingRechargeRequest(ctx, user.ID, 1, "next")
		if errNext != nil || next.ID <= pending.ID {
			t.Fatalf("restored sequence request=%+v error=%v", next, errNext)
		}
	}
}
