package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// ErrDeletedAccessGroup prevents stale clients from restoring deleted scope bindings.
var ErrDeletedAccessGroup = errors.New("cannot bind a deleted access group")

// ensureAPIKeyGroupsNotDeleted preserves legacy handling of unknown IDs, but rejects
// references to groups that have explicitly been deleted. Call under the API key mutation lock.
func ensureAPIKeyGroupsNotDeleted(tx *gorm.DB, channels, modelGroups *[]uint) error {
	for _, binding := range []struct {
		model any
		ids   *[]uint
	}{
		{&ChannelGroupRecord{}, channels},
		{&ModelGroupRecord{}, modelGroups},
	} {
		if binding.ids == nil || len(*binding.ids) == 0 {
			continue
		}
		var count int64
		if errCount := tx.Unscoped().Model(binding.model).
			Where("id IN ? AND deleted_at IS NOT NULL", *binding.ids).Count(&count).Error; errCount != nil {
			return errCount
		}
		if count > 0 {
			return ErrDeletedAccessGroup
		}
	}
	return nil
}

// removeAccessGroupBindings also cleans soft-deleted records so restoring a key
// cannot restore a stale restriction. Only the binding columns are updated.
func removeAccessGroupBindings(tx *gorm.DB, channelIDs, modelIDs []uint) error {
	changed := false
	for _, binding := range []struct {
		model  any
		column string
		ids    []uint
	}{
		{&APIKeyRecord{}, "channels", channelIDs},
		{&APIKeyRecord{}, "model_groups", modelIDs},
		{&ModelGroupDetailRecord{}, "channels", channelIDs},
	} {
		if len(binding.ids) == 0 {
			continue
		}
		removed := make(map[uint]struct{}, len(binding.ids))
		for _, id := range binding.ids {
			removed[id] = struct{}{}
		}
		var records []struct {
			ID       uint
			Bindings JSONB
		}
		if errFind := tx.Unscoped().Model(binding.model).
			Select("id", binding.column+" AS bindings").Find(&records).Error; errFind != nil {
			return errFind
		}
		for _, record := range records {
			ids, errIDs := apiKeyChannelsFromJSON(record.Bindings)
			if errIDs != nil {
				return fmt.Errorf("decode %s bindings for record %d: %w", binding.column, record.ID, errIDs)
			}
			remaining := make([]uint, 0, len(ids))
			for _, id := range ids {
				if _, deleted := removed[id]; !deleted {
					remaining = append(remaining, id)
				}
			}
			if len(remaining) == len(ids) {
				continue
			}
			raw, errJSON := json.Marshal(remaining)
			if errJSON != nil {
				return errJSON
			}
			if errUpdate := tx.Unscoped().Model(binding.model).Where("id = ?", record.ID).
				Update(binding.column, JSONB(raw)).Error; errUpdate != nil {
				return errUpdate
			}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return appendEvent(tx, "config", "upsert", configAPIKeysRootKey, time.Now().UTC().UnixNano())
}

// migrateDeletedAccessGroupBindings repairs historical references to soft-deleted
// groups without relaxing restrictions for disabled or unknown groups.
func migrateDeletedAccessGroupBindings(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if errLock := lockAPIKeyMutationTransaction(tx); errLock != nil {
			return errLock
		}
		var channelIDs, modelIDs []uint
		if errChannels := tx.Unscoped().Model(&ChannelGroupRecord{}).
			Where("deleted_at IS NOT NULL").Pluck("id", &channelIDs).Error; errChannels != nil {
			return errChannels
		}
		if errModels := tx.Unscoped().Model(&ModelGroupRecord{}).
			Where("deleted_at IS NOT NULL").Pluck("id", &modelIDs).Error; errModels != nil {
			return errModels
		}
		return removeAccessGroupBindings(tx, channelIDs, modelIDs)
	})
}
