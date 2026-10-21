package cluster

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestDeleteAccessGroupRemovesBindings(t *testing.T) {
	for _, kind := range []string{"channel", "model"} {
		t.Run(kind, func(t *testing.T) {
			repo, db := newUserSecurityTestRepository(t)
			ctx := context.Background()
			userID := uint(17)
			name := "preserved name"
			channel := ChannelGroupRecord{ID: 1, ChannelName: "removed"}
			model := ModelGroupRecord{ID: 1, GroupName: "removed"}
			channelDisabled := ChannelGroupRecord{ID: 2, ChannelName: "disabled", Disabled: true}
			modelDisabled := ModelGroupRecord{ID: 2, GroupName: "disabled", Disabled: true}
			channelDetail := ChannelGroupDetailRecord{ChannelGroupID: 1, AuthID: "auth-a"}
			modelDetail := ModelGroupDetailRecord{ModelGroupID: 1, ModelID: "model-a", Channels: JSONB(`[1,2]`)}
			inheritedDetail := ModelGroupDetailRecord{ModelGroupID: 2, ModelID: "model-b", Channels: JSONB(`[1]`)}
			deletedAt := gorm.DeletedAt{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}
			keys := []APIKeyRecord{
				{APIKey: "mixed", UserID: &userID, DisplayName: &name, Channels: JSONB(`[1,2]`), ModelGroups: JSONB(`[1,2]`)},
				{APIKey: "single", UserID: &userID, Channels: JSONB(`[1]`), ModelGroups: JSONB(`[1]`)},
				{APIKey: "untouched", Channels: JSONB(`[2]`), ModelGroups: JSONB(`[2]`)},
				{APIKey: "deleted", Channels: JSONB(`[1]`), ModelGroups: JSONB(`[1]`), DeletedAt: deletedAt},
			}
			for _, record := range []any{&channel, &channelDisabled, &model, &modelDisabled, &channelDetail, &modelDetail, &inheritedDetail, &keys} {
				if errCreate := db.Create(record).Error; errCreate != nil {
					t.Fatal(errCreate)
				}
			}
			var errDelete error
			if kind == "channel" {
				errDelete = repo.DeleteChannelGroup(ctx, 1)
			} else {
				errDelete = repo.DeleteModelGroup(ctx, 1)
			}
			if errDelete != nil {
				t.Fatal(errDelete)
			}
			for i, before := range keys {
				var after APIKeyRecord
				if errFirst := db.Unscoped().First(&after, before.ID).Error; errFirst != nil {
					t.Fatal(errFirst)
				}
				if !sameOptionalUint(after.UserID, before.UserID) || !sameOptionalString(after.DisplayName, before.DisplayName) || after.APIKey != before.APIKey || after.DeletedAt.Valid != before.DeletedAt.Valid {
					t.Fatalf("key identity or owner changed: before=%+v after=%+v", before, after)
				}
				binding, other, previousOther := after.Channels, after.ModelGroups, before.ModelGroups
				if kind == "model" {
					binding, other, previousOther = after.ModelGroups, after.Channels, before.Channels
				}
				want := "[]"
				if i == 0 || i == 2 {
					want = "[2]"
				}
				if string(binding) != want || string(other) != string(previousOther) {
					t.Fatalf("key %s bindings = %s / %s, want %s / %s", before.APIKey, binding, other, want, previousOther)
				}
				if i == 2 && !after.UpdatedAt.Equal(before.UpdatedAt) {
					t.Fatal("unrelated key was updated")
				}
				if i == 1 {
					if kind == "channel" {
						allowed, errAllowed := allowedAuthIDsForAPIKeyRecord(ctx, db, &after)
						if errAllowed != nil || allowed != nil {
							t.Fatalf("last channel removal remains restricted: %v, %v", allowed, errAllowed)
						}
					} else {
						allowed, errAllowed := allowedModelIDsForAPIKeyRecord(ctx, db, &after)
						if errAllowed != nil || allowed != nil {
							t.Fatalf("last model removal remains restricted: %v, %v", allowed, errAllowed)
						}
					}
				}
			}
			var details []ModelGroupDetailRecord
			if errFind := db.Unscoped().Order("id").Find(&details).Error; errFind != nil {
				t.Fatal(errFind)
			}
			if kind == "channel" {
				if string(details[0].Channels) != "[2]" || string(details[1].Channels) != "[]" || details[0].DeletedAt.Valid {
					t.Fatalf("model-specific channels not cleaned: %+v", details)
				}
				var deletedDetail ChannelGroupDetailRecord
				if errFirst := db.Unscoped().First(&deletedDetail, channelDetail.ID).Error; errFirst != nil || !deletedDetail.DeletedAt.Valid {
					t.Fatalf("channel detail not deleted: %+v, %v", deletedDetail, errFirst)
				}
			} else if !details[0].DeletedAt.Valid || details[1].DeletedAt.Valid {
				t.Fatalf("wrong model details deleted: %+v", details)
			}
			var events []ClusterEventRecord
			if errFind := db.Where("scope = ? AND entity_uuid = ?", "config", configAPIKeysRootKey).Find(&events).Error; errFind != nil || len(events) != 1 {
				t.Fatalf("config events = %v, error=%v", events, errFind)
			}
			// Disabled bindings must remain restrictive rather than becoming unrestricted.
			var disabledKey APIKeyRecord
			if errFirst := db.First(&disabledKey, keys[2].ID).Error; errFirst != nil {
				t.Fatal(errFirst)
			}
			authIDs, errAuth := allowedAuthIDsForAPIKeyRecord(ctx, db, &disabledKey)
			modelIDs, errModels := allowedModelIDsForAPIKeyRecord(ctx, db, &disabledKey)
			if errAuth != nil || errModels != nil || authIDs == nil || modelIDs == nil || len(authIDs) != 0 || len(modelIDs) != 0 {
				t.Fatalf("disabled restrictions changed: auth=%v models=%v errors=%v/%v", authIDs, modelIDs, errAuth, errModels)
			}
		})
	}
}

func TestDeletedAccessGroupMigrationVersionGate(t *testing.T) {
	store := newFakeSchemaMigrationStore(schemaMigrationState{Version: 8, Initialized: true})
	for i := 0; i < 2; i++ {
		result, errCoordinate := coordinateSchemaMigration(t.Context(), currentDatabaseVersion, time.Millisecond, time.Second, false, store.operations())
		if errCoordinate != nil {
			t.Fatal(errCoordinate)
		}
		if result.MigrationExecutor != (i == 0) {
			t.Fatalf("migration executor on startup %d = %v", i, result.MigrationExecutor)
		}
	}
	_, _, calls := store.snapshot()
	if calls != 1 {
		t.Fatalf("migration calls = %d, want 1", calls)
	}
}

func TestMigrateDeletedAccessGroupBindings(t *testing.T) {
	_, db := newUserSecurityTestRepository(t)
	deletedAt := gorm.DeletedAt{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}
	key := APIKeyRecord{APIKey: "legacy", Channels: JSONB(`[1,2,999]`), ModelGroups: JSONB(`[1]`)}
	detail := ModelGroupDetailRecord{ModelGroupID: 2, ModelID: "model", Channels: JSONB(`[1]`)}
	for _, record := range []any{
		&ChannelGroupRecord{ID: 1, ChannelName: "deleted", DeletedAt: deletedAt},
		&ChannelGroupRecord{ID: 2, ChannelName: "disabled", Disabled: true},
		&ModelGroupRecord{ID: 1, GroupName: "deleted", DeletedAt: deletedAt},
		&ModelGroupRecord{ID: 2, GroupName: "active"},
		&key, &detail,
	} {
		if errCreate := db.Create(record).Error; errCreate != nil {
			t.Fatal(errCreate)
		}
	}
	for i := 0; i < 2; i++ {
		if errMigrate := runCustomMigrations(db); errMigrate != nil {
			t.Fatal(errMigrate)
		}
	}
	var after APIKeyRecord
	if errFirst := db.First(&after, key.ID).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if string(after.Channels) != "[2,999]" || string(after.ModelGroups) != "[]" {
		t.Fatalf("migrated bindings = %s / %s", after.Channels, after.ModelGroups)
	}
	if errFirst := db.First(&detail, detail.ID).Error; errFirst != nil || string(detail.Channels) != "[]" {
		t.Fatalf("migrated detail channels = %s, error=%v", detail.Channels, errFirst)
	}
	var count int64
	if errCount := db.Model(&ClusterEventRecord{}).Where("scope = ? AND entity_uuid = ?", "config", configAPIKeysRootKey).Count(&count).Error; errCount != nil || count != 1 {
		t.Fatalf("migration was not idempotent: count=%d error=%v", count, errCount)
	}
}

func TestAPIKeyRejectsDeletedAccessGroups(t *testing.T) {
	repo, db := newUserSecurityTestRepository(t)
	ctx := context.Background()
	deletedAt := gorm.DeletedAt{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}
	user := UserRecord{Username: "owner"}
	if errCreate := db.Create(&user).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	key := APIKeyRecord{APIKey: "preserved", UserID: &user.ID, Channels: JSONB(`[]`), ModelGroups: JSONB(`[]`)}
	for _, record := range []any{
		&ChannelGroupRecord{ID: 1, ChannelName: "deleted", DeletedAt: deletedAt},
		&ModelGroupRecord{ID: 1, GroupName: "deleted", DeletedAt: deletedAt},
		&key,
	} {
		if errCreate := db.Create(record).Error; errCreate != nil {
			t.Fatal(errCreate)
		}
	}
	if errReload := db.First(&key, key.ID).Error; errReload != nil {
		t.Fatal(errReload)
	}
	for _, kind := range []string{"channel", "model"} {
		ids := []uint{1}
		var channels, models *[]uint
		if kind == "channel" {
			channels = &ids
		} else {
			models = &ids
		}
		newKey := "new-key"
		for name, mutate := range map[string]func() error{
			"create": func() error {
				_, errCreate := repo.CreateAPIKey(ctx, APIKeyEntryUpdate{APIKey: newKey, Channels: channels, ModelGroups: models})
				return errCreate
			},
			"patch": func() error {
				_, errUpdate := repo.UpdateAPIKey(ctx, APIKeySelector{ID: key.ID}, APIKeyAdminUpdate{Channels: channels, ModelGroups: models})
				return errUpdate
			},
			"bindings": func() error {
				_, errUpdate := repo.UpdateAPIKeyBindings(ctx, key.APIKey, nil, channels, models)
				return errUpdate
			},
			"replace": func() error {
				_, errReplace := repo.ReplaceAPIKeyEntries(ctx, []APIKeyEntryUpdate{{APIKey: newKey}, {APIKey: key.APIKey, Channels: channels, ModelGroups: models}})
				return errReplace
			},
			"user-create": func() error {
				_, errCreate := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &newKey, Channels: channels, ModelGroups: models})
				return errCreate
			},
			"user-update": func() error {
				_, errUpdate := repo.UpdateAPIKeyForUser(ctx, user.ID, key.ID, "", APIKeyUserUpdate{Channels: channels, ModelGroups: models})
				return errUpdate
			},
		} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				if errMutation := mutate(); !errors.Is(errMutation, ErrDeletedAccessGroup) {
					t.Fatalf("error = %v, want ErrDeletedAccessGroup", errMutation)
				}
				var keys []APIKeyRecord
				if errFind := db.Unscoped().Find(&keys).Error; errFind != nil || len(keys) != 1 || !reflect.DeepEqual(keys[0], key) {
					t.Fatalf("rejected mutation changed keys: %+v, error=%v", keys, errFind)
				}
			})
		}
	}
}

func TestDeleteAccessGroupRollsBackOnBindingFailure(t *testing.T) {
	for _, kind := range []string{"channel", "model"} {
		t.Run(kind, func(t *testing.T) {
			repo, db := newUserSecurityTestRepository(t)
			channel := ChannelGroupRecord{ID: 1, ChannelName: "channel"}
			model := ModelGroupRecord{ID: 1, GroupName: "model"}
			channelDetail := ChannelGroupDetailRecord{ChannelGroupID: 1, AuthID: "auth"}
			modelDetail := ModelGroupDetailRecord{ModelGroupID: 1, ModelID: "model"}
			key := APIKeyRecord{APIKey: "key", Channels: JSONB(`[1]`), ModelGroups: JSONB(`[1]`)}
			for _, record := range []any{&channel, &model, &channelDetail, &modelDetail, &key} {
				if errCreate := db.Create(record).Error; errCreate != nil {
					t.Fatal(errCreate)
				}
			}
			if errTrigger := db.Exec(`CREATE TRIGGER reject_binding_update BEFORE UPDATE ON api_key BEGIN SELECT RAISE(ABORT, 'injected failure'); END`).Error; errTrigger != nil {
				t.Fatal(errTrigger)
			}
			var errDelete error
			if kind == "channel" {
				errDelete = repo.DeleteChannelGroup(context.Background(), 1)
			} else {
				errDelete = repo.DeleteModelGroup(context.Background(), 1)
			}
			if errDelete == nil {
				t.Fatal("expected cleanup failure")
			}
			for _, record := range []any{&channel, &model, &channelDetail, &modelDetail} {
				if errFirst := db.First(record).Error; errFirst != nil {
					t.Fatalf("deletion was not rolled back: %v", errFirst)
				}
			}
			var count int64
			if errCount := db.Model(&ClusterEventRecord{}).Count(&count).Error; errCount != nil || count != 0 {
				t.Fatalf("failed deletion emitted events: %d, %v", count, errCount)
			}
		})
	}
}
