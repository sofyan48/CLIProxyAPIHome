package cluster

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"gorm.io/gorm"
)

func TestApproveUserScopes(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			db, repo := openAPIKeyTestRepository(t)
			ctx := t.Context()
			name, pending := "pending", true
			user, errUser := repo.CreateUser(ctx, UserUpdate{Username: &name, ApprovalPending: &pending})
			if errUser != nil {
				t.Fatal(errUser)
			}
			group, errGroup := repo.CreateModelGroup(ctx, "scope", false)
			if errGroup != nil {
				t.Fatal(errGroup)
			}
			groups := []uint{group.ID}
			channels := []uint{41, 42}
			if existing {
				for _, key := range []string{"first", "second"} {
					if _, errKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &key, Channels: &channels}); errKey != nil {
						t.Fatal(errKey)
					}
				}
			}
			deleted := APIKeyRecord{APIKey: "deleted", UserID: &user.ID, ModelGroups: JSONB("[99]")}
			if errCreate := db.Create(&deleted).Error; errCreate != nil {
				t.Fatal(errCreate)
			}
			if errDelete := db.Delete(&deleted).Error; errDelete != nil {
				t.Fatal(errDelete)
			}
			beforeEvents := apiKeyEventCount(t, db)
			approved, errApprove := repo.ApproveUser(ctx, user.ID, &groups)
			if errApprove != nil || approved.ApprovalPending {
				t.Fatalf("approve failed: %v", errApprove)
			}
			var keys []APIKeyRecord
			if errFind := db.Where("user_id = ?", user.ID).Order("id").Find(&keys).Error; errFind != nil {
				t.Fatal(errFind)
			}
			wantCount := 1
			if existing {
				wantCount = 2
			}
			if len(keys) != wantCount {
				t.Fatalf("key count = %d", len(keys))
			}
			for _, key := range keys {
				got, errGroups := apiKeyModelGroupsFromJSON(key.ModelGroups)
				if errGroups != nil || !reflect.DeepEqual(got, groups) {
					t.Fatalf("groups = %v, error = %v", got, errGroups)
				}
				if existing && string(key.Channels) != "[41,42]" {
					t.Fatalf("channels changed: %s", key.Channels)
				}
				if !existing && (!strings.HasPrefix(key.APIKey, "sk-") || len(key.APIKey) != 67 || string(key.Channels) != "[]") {
					t.Fatal("invalid generated key")
				}
			}
			if errFind := db.Unscoped().First(&deleted, deleted.ID).Error; errFind != nil {
				t.Fatal(errFind)
			}
			if string(deleted.ModelGroups) != "[99]" || !deleted.DeletedAt.Valid {
				t.Fatal("deleted key changed")
			}
			if apiKeyEventCount(t, db) != beforeEvents+1 {
				t.Fatal("missing config event")
			}
			// Even a nonexistent scope must not change an already-approved user.
			other := []uint{999999}
			if _, errRetry := repo.ApproveUser(ctx, user.ID, &other); errRetry != nil {
				t.Fatal(errRetry)
			}
			var retry []APIKeyRecord
			if errFind := db.Where("user_id = ?", user.ID).Order("id").Find(&retry).Error; errFind != nil {
				t.Fatal(errFind)
			}
			if !reflect.DeepEqual(keys, retry) || apiKeyEventCount(t, db) != beforeEvents+1 {
				t.Fatal("retry mutated keys or events")
			}
			newKey := "inherited"
			inherited, errKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &newKey})
			if errKey != nil {
				t.Fatal(errKey)
			}
			if string(inherited.ModelGroups) != string(keys[0].ModelGroups) {
				t.Fatal("new user key did not inherit scope")
			}
		})
	}
}

func TestApproveUserInvalidScopes(t *testing.T) {
	db, repo := openAPIKeyTestRepository(t)
	ctx := t.Context()
	disabled, errGroup := repo.CreateModelGroup(ctx, "disabled", true)
	if errGroup != nil {
		t.Fatal(errGroup)
	}
	deleted, errDeleted := repo.CreateModelGroup(ctx, "deleted", false)
	if errDeleted != nil {
		t.Fatal(errDeleted)
	}
	if errDelete := db.Delete(deleted).Error; errDelete != nil {
		t.Fatal(errDelete)
	}
	for i, tc := range []struct {
		groups []uint
		want   error
	}{
		{[]uint{}, ErrApprovalModelGroups}, {[]uint{0}, ErrApprovalModelGroups},
		{[]uint{disabled.ID}, ErrApprovalModelGroups}, {[]uint{999999}, gorm.ErrRecordNotFound},
		{[]uint{deleted.ID}, gorm.ErrRecordNotFound},
	} {
		name, pending := fmt.Sprintf("pending-%d", i), true
		user, errUser := repo.CreateUser(ctx, UserUpdate{Username: &name, ApprovalPending: &pending})
		if errUser != nil {
			t.Fatal(errUser)
		}
		if _, errApprove := repo.ApproveUser(ctx, user.ID, &tc.groups); !errors.Is(errApprove, tc.want) {
			t.Fatalf("error = %v, want %v", errApprove, tc.want)
		}
		stored, errLoad := repo.GetUser(ctx, user.ID)
		if errLoad != nil || !stored.ApprovalPending {
			t.Fatal("invalid approval changed user")
		}
		var count int64
		if errCount := db.Model(&APIKeyRecord{}).Where("user_id = ?", user.ID).Count(&count).Error; errCount != nil || count != 0 {
			t.Fatal("invalid approval created keys")
		}
	}
}

func TestApproveUserRollback(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			db, repo := openAPIKeyTestRepository(t)
			ctx := t.Context()
			name, pending := "rollback", true
			user, errUser := repo.CreateUser(ctx, UserUpdate{Username: &name, ApprovalPending: &pending})
			if errUser != nil {
				t.Fatal(errUser)
			}
			group, errGroup := repo.CreateModelGroup(ctx, "scope", false)
			if errGroup != nil {
				t.Fatal(errGroup)
			}
			if existing {
				key := "preserved"
				if _, errKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &key}); errKey != nil {
					t.Fatal(errKey)
				}
			}
			var before []APIKeyRecord
			if errFind := db.Where("user_id = ?", user.ID).Find(&before).Error; errFind != nil {
				t.Fatal(errFind)
			}
			beforeEvents := apiKeyEventCount(t, db)
			// Fail the final user write after the key mutation and event insert.
			if errTrigger := db.Exec(`CREATE TRIGGER fail_approval BEFORE UPDATE OF approval_pending ON user BEGIN SELECT RAISE(ABORT, 'approval failure'); END`).Error; errTrigger != nil {
				t.Fatal(errTrigger)
			}
			groups := []uint{group.ID}
			if _, errApprove := repo.ApproveUser(ctx, user.ID, &groups); errApprove == nil {
				t.Fatal("expected rollback")
			}
			stored, errLoad := repo.GetUser(ctx, user.ID)
			if errLoad != nil || !stored.ApprovalPending {
				t.Fatal("user not pending after rollback")
			}
			var after []APIKeyRecord
			if errFind := db.Where("user_id = ?", user.ID).Find(&after).Error; errFind != nil {
				t.Fatal(errFind)
			}
			if !reflect.DeepEqual(before, after) || apiKeyEventCount(t, db) != beforeEvents {
				t.Fatal("keys or events escaped rollback")
			}
		})
	}
}

func TestApproveUserOmittedScopes(t *testing.T) {
	db, repo := openAPIKeyTestRepository(t)
	name, pending := "legacy", true
	user, errUser := repo.CreateUser(t.Context(), UserUpdate{Username: &name, ApprovalPending: &pending})
	if errUser != nil {
		t.Fatal(errUser)
	}
	beforeEvents := apiKeyEventCount(t, db)
	if _, errApprove := repo.ApproveUser(t.Context(), user.ID, nil); errApprove != nil {
		t.Fatal(errApprove)
	}
	var count int64
	if errCount := db.Model(&APIKeyRecord{}).Count(&count).Error; errCount != nil || count != 0 {
		t.Fatal("legacy approval generated keys")
	}
	if apiKeyEventCount(t, db) != beforeEvents {
		t.Fatal("legacy approval emitted key event")
	}
}
