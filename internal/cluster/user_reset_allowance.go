package cluster

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	ResetAllowanceOneTime = "one_time"
	ResetAllowanceMonthly = "monthly"
)

var (
	ErrResetAllowanceUnavailable = errors.New("reset allowance is expired or exhausted")
	ErrResetAllowanceNoWindows   = errors.New("no period-limit windows are enabled")
)

// UserResetAllowanceStatus describes the effective allowance, including lazy renewal.
type UserResetAllowanceStatus struct {
	Mode      string     `json:"mode"`
	Amount    int64      `json:"amount"`
	Available int64      `json:"available"`
	ExpiresAt *time.Time `json:"expires_at"`
	RenewsAt  *time.Time `json:"renews_at"`
}

// UserResetAllowanceStatusAt is read-only; monthly renewal is persisted on consumption.
func UserResetAllowanceStatusAt(user *UserRecord, now time.Time) UserResetAllowanceStatus {
	status := UserResetAllowanceStatus{Mode: user.ResetAllowanceMode, Amount: user.ResetAllowanceAmount, Available: user.ResetAllowanceAvailable, ExpiresAt: cloneTimePtr(user.ResetAllowanceExpiresAt), RenewsAt: cloneTimePtr(user.ResetAllowanceRenewsAt)}
	switch status.Mode {
	case ResetAllowanceMonthly:
		next := startOfMonthInLocation(now, loadUserLocation(user.Timezone)).AddDate(0, 1, 0).UTC()
		if status.RenewsAt == nil || !now.Before(*status.RenewsAt) {
			status.Available = status.Amount
		}
		status.RenewsAt = &next
		status.ExpiresAt = nil
	case ResetAllowanceOneTime:
		status.RenewsAt = nil
		if status.ExpiresAt != nil && !now.Before(*status.ExpiresAt) {
			status.Available = 0
		}
	default:
		status = UserResetAllowanceStatus{}
	}
	return status
}

// SetUserResetAllowance replaces the policy and immediately initializes its balance.
func (r *Repository) SetUserResetAllowance(ctx context.Context, userID uint, mode string, amount int64, expiresAt *time.Time, now time.Time) (*UserRecord, error) {
	if mode != ResetAllowanceOneTime && mode != ResetAllowanceMonthly {
		return nil, periodLimitFieldError("mode", "invalid_reset_allowance_mode", "mode must be one_time or monthly")
	}
	if amount < 0 {
		return nil, periodLimitFieldError("amount", "invalid_reset_allowance_amount", "amount must be a non-negative integer")
	}
	if mode == ResetAllowanceMonthly && expiresAt != nil {
		return nil, periodLimitFieldError("expires_at", "invalid_reset_allowance_expiry", "monthly allowance cannot expire")
	}
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	var user *UserRecord
	errTx := db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		user = &UserRecord{}
		if errFirst := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(user, userID).Error; errFirst != nil {
			return errFirst
		}
		var renewsAt *time.Time
		if mode == ResetAllowanceMonthly {
			next := startOfMonthInLocation(now, loadUserLocation(user.Timezone)).AddDate(0, 1, 0).UTC()
			renewsAt = &next
		}
		updates := map[string]any{"reset_allowance_mode": mode, "reset_allowance_amount": amount, "reset_allowance_available": amount, "reset_allowance_expires_at": expiresAt, "reset_allowance_renews_at": renewsAt}
		if errUpdate := tx.Model(user).Updates(updates).Error; errUpdate != nil {
			return errUpdate
		}
		user = &UserRecord{}
		return tx.First(user, userID).Error
	})
	return user, errTx
}

func userPeriodResetUpdates(user *UserRecord, windows []string, mode string, now time.Time) map[string]any {
	updates := map[string]any{}
	for _, id := range windows {
		updates[windowStartColumn(id)] = nil
		windowMode, errMode := normalizePeriodWindowMode(userWindowMode(user, id), id != PeriodWindow5h)
		if errMode != nil {
			windowMode = DefaultPeriodWindowMode
		}
		// Preserve admin window_only behavior; user consumption always resets counters.
		if mode == PeriodResetModeCounter || windowMode == PeriodWindowModeSliding || windowMode == PeriodWindowModeCalendar {
			updates[usageEpochColumn(id)] = now
		}
	}
	return updates
}

// ConsumeUserResetAllowance decrements and resets all enabled windows in one transaction.
func (r *Repository) ConsumeUserResetAllowance(ctx context.Context, userID uint, now time.Time) (UserPeriodLimitsStatus, error) {
	db, errDB := r.database()
	if errDB != nil {
		return UserPeriodLimitsStatus{}, errDB
	}
	now = now.UTC()
	var status UserPeriodLimitsStatus
	errTx := db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		user := &UserRecord{}
		if errFirst := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(user, userID).Error; errFirst != nil {
			return errFirst
		}
		windows := []string{}
		for _, window := range []struct {
			id    string
			limit *float64
		}{{PeriodWindow5h, user.Limit5hCredits}, {PeriodWindow1d, user.Limit1dCredits}, {PeriodWindow7d, user.Limit7dCredits}, {PeriodWindow30d, user.Limit30dCredits}} {
			if window.limit != nil {
				windows = append(windows, window.id)
			}
		}
		if len(windows) == 0 {
			return ErrResetAllowanceNoWindows
		}
		allowance := UserResetAllowanceStatusAt(user, now)
		if allowance.Available <= 0 {
			return ErrResetAllowanceUnavailable
		}
		if allowance.Mode == ResetAllowanceMonthly && (user.ResetAllowanceRenewsAt == nil || !now.Before(*user.ResetAllowanceRenewsAt)) {
			if errRenew := tx.Model(user).Updates(map[string]any{"reset_allowance_available": allowance.Amount, "reset_allowance_renews_at": allowance.RenewsAt}).Error; errRenew != nil {
				return errRenew
			}
		}
		updates := userPeriodResetUpdates(user, windows, PeriodResetModeCounter, now)
		updates["reset_allowance_available"] = gorm.Expr("reset_allowance_available - 1")
		result := tx.Model(&UserRecord{}).Where("id = ? AND reset_allowance_available > 0", userID).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrResetAllowanceUnavailable
		}
		user = &UserRecord{}
		if errReload := tx.First(user, userID).Error; errReload != nil {
			return errReload
		}
		var errStatus error
		status, errStatus = buildUserPeriodLimitsStatusDB(ctx, tx, user, now)
		return errStatus
	})
	if errTx != nil {
		return UserPeriodLimitsStatus{}, errTx
	}
	return status, nil
}
