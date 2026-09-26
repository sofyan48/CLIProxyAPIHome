package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"gorm.io/gorm/clause"
)

// CodexResetRedemptionRecord is an append-only reservation. An ambiguous POST
// must never be retried by Home, even after a process restart.
type CodexResetRedemptionRecord struct {
	CredentialID   string    `gorm:"column:credential_id;primaryKey;size:128"`
	RequestKeyHash string    `gorm:"column:request_key_hash;primaryKey;size:64"`
	CreatedAt      time.Time `gorm:"column:created_at;not null"`
}

func (CodexResetRedemptionRecord) TableName() string { return "codex_reset_redemptions" }

var ErrCodexResetRequestUsed = errors.New("Codex reset request key already used")

func (r *Repository) ReserveCodexResetRequest(ctx context.Context, credentialID, requestKey string) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	digest := sha256.Sum256([]byte(requestKey))
	result := db.WithContext(contextOrBackground(ctx)).Clauses(clause.OnConflict{DoNothing: true}).Create(&CodexResetRedemptionRecord{
		CredentialID: credentialID, RequestKeyHash: hex.EncodeToString(digest[:]), CreatedAt: time.Now().UTC(),
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrCodexResetRequestUsed
	}
	return nil
}
