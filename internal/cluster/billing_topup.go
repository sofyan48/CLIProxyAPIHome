package cluster

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrTopupPending       = errors.New("a topup request is already pending")
	ErrInvalidTopupAmount = errors.New("amount must be positive and finite, and must increase the finite balance without overflow")
)

// BillingRechargeRequestRecord is an immutable self-topup amount awaiting administrator approval.
type BillingRechargeRequestRecord struct {
	ID         uint       `gorm:"column:id;primaryKey;autoIncrement" json:"id"`
	UserID     uint       `gorm:"column:user_id;not null;index;uniqueIndex:idx_topup_pending_user,where:status = 'pending'" json:"user_id"`
	Amount     float64    `gorm:"column:amount;not null" json:"amount"`
	Note       string     `gorm:"column:note;type:text" json:"note"`
	Status     string     `gorm:"column:status;not null;check:chk_topup_status,status IN ('pending','approved')" json:"status"`
	LedgerID   *string    `gorm:"column:ledger_id;uniqueIndex" json:"ledger_id"`
	CreatedAt  time.Time  `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt  time.Time  `gorm:"column:updated_at;not null" json:"updated_at"`
	ApprovedAt *time.Time `gorm:"column:approved_at" json:"approved_at"`
}

func (BillingRechargeRequestRecord) TableName() string { return "billing_recharge_request" }

// SQLite must acquire its write lock before any reads; PostgreSQL locks the user row.
func lockBillingUserTx(tx *gorm.DB, userID uint) (*UserRecord, error) {
	if tx.Dialector.Name() == "sqlite" {
		if errLock := tx.Model(&UserRecord{}).Where("id = ?", userID).UpdateColumn("credits", gorm.Expr("credits")).Error; errLock != nil {
			return nil, errLock
		}
	}
	user := &UserRecord{}
	if errLoad := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(user, "id = ?", userID).Error; errLoad != nil {
		return nil, errLoad
	}
	return user, nil
}

func validateTopupAmount(user *UserRecord, amount float64) error {
	if amount <= 0 || math.IsNaN(amount) || math.IsInf(amount, 0) {
		return ErrInvalidTopupAmount
	}
	if !user.CreditsUnlimited {
		after := user.Credits + amount
		if math.IsNaN(after) || math.IsInf(after, 0) || after <= user.Credits {
			return ErrInvalidTopupAmount
		}
	}
	return nil
}

func (r *Repository) CreateBillingRechargeRequest(ctx context.Context, userID uint, amount float64, note string) (*BillingRechargeRequestRecord, float64, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, 0, errDB
	}
	var request BillingRechargeRequestRecord
	var balance float64
	errTransaction := db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		user, errLock := lockBillingUserTx(tx, userID)
		if errLock != nil {
			return errLock
		}
		var count int64
		if errCount := tx.Model(&BillingRechargeRequestRecord{}).Where("user_id = ? AND status = ?", userID, "pending").Count(&count).Error; errCount != nil {
			return errCount
		}
		if count != 0 {
			return ErrTopupPending
		}
		if errAmount := validateTopupAmount(user, amount); errAmount != nil {
			return errAmount
		}
		now := time.Now().UTC()
		request = BillingRechargeRequestRecord{UserID: userID, Amount: amount, Note: strings.TrimSpace(note), Status: "pending", CreatedAt: now, UpdatedAt: now}
		balance = user.Credits
		return tx.Create(&request).Error
	})
	if errTransaction != nil {
		return nil, 0, errTransaction
	}
	return &request, balance, nil
}

// PendingBillingRechargeRequests uses one query for the entire management user list.
func (r *Repository) PendingBillingRechargeRequests(ctx context.Context, userIDs []uint) (map[uint]*BillingRechargeRequestRecord, error) {
	result := make(map[uint]*BillingRechargeRequestRecord)
	if len(userIDs) == 0 {
		return result, nil
	}
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	var requests []BillingRechargeRequestRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).Where("user_id IN ? AND status = ?", userIDs, "pending").Find(&requests).Error; errFind != nil {
		return nil, errFind
	}
	for index := range requests {
		result[requests[index].UserID] = &requests[index]
	}
	return result, nil
}

func (r *Repository) ApproveBillingRechargeRequest(ctx context.Context, userID, requestID uint) (*UserRecord, *BillingBalanceRecord, error) {
	if requestID == 0 || uint64(requestID) > math.MaxInt64 {
		return nil, nil, gorm.ErrRecordNotFound
	}
	db, errDB := r.database()
	if errDB != nil {
		return nil, nil, errDB
	}
	var user *UserRecord
	var record *BillingBalanceRecord
	errTransaction := db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		lockedUser, errLock := lockBillingUserTx(tx, userID)
		if errLock != nil {
			return errLock
		}
		user = lockedUser
		var request BillingRechargeRequestRecord
		if errLoad := tx.First(&request, "id = ? AND user_id = ?", requestID, userID).Error; errLoad != nil {
			return errLoad
		}
		if request.Status == "approved" {
			record = &BillingBalanceRecord{}
			return tx.First(record, "id = ?", request.LedgerID).Error
		}
		if errAmount := validateTopupAmount(user, request.Amount); errAmount != nil {
			return errAmount
		}
		appliedRecord, errApply := applyBillingBalanceRecordTx(ctx, tx, BillingBalanceUpdate{UserID: userID, Type: BillingBalanceTypeRecharge, Amount: request.Amount, Note: request.Note, Operator: "admin"})
		if errApply != nil {
			return errApply
		}
		record = appliedRecord
		user.Credits = record.BalanceAfter
		now := time.Now().UTC()
		return tx.Model(&request).Updates(map[string]any{"status": "approved", "ledger_id": record.ID, "approved_at": now, "updated_at": now}).Error
	})
	if errTransaction != nil {
		return nil, nil, errTransaction
	}
	return user, record, nil
}
