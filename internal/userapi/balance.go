package userapi

import (
	"fmt"
	"math"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

// ListCurrentUserBillingBalanceRecords returns only the authenticated user's balance ledger.
func (h *Handler) ListCurrentUserBillingBalanceRecords(c *gin.Context) {
	ctx, cancel := requestContext(c)
	defer cancel()
	user, ok := h.authenticatedUser(c, ctx, authFields{})
	if !ok {
		return
	}
	from, to, ok := userBillingDateRangeFromRequest(c)
	if !ok {
		return
	}
	limit, offset, ok := userBillingPaginationFromRequest(c)
	if !ok {
		return
	}
	result, errRecords := h.repo.ListBillingBalanceRecords(ctx, cluster.BillingBalanceQuery{
		From: from, ToExclusive: to, UserID: &user.ID, Limit: limit, Offset: offset,
	})
	if errRecords != nil {
		respondError(c, http.StatusInternalServerError, "billing_balance_record_load_failed", errRecords)
		return
	}
	items := make([]gin.H, 0, len(result.Records))
	for index := range result.Records {
		items = append(items, currentUserBillingBalanceRecordResponse(&result.Records[index]))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": result.Total, "limit": limit, "offset": offset})
}

// RechargeCurrentUserBillingBalance permits unrestricted self-credit without payment verification.
func (h *Handler) RechargeCurrentUserBillingBalance(c *gin.Context) {
	ctx, cancel := requestContext(c)
	defer cancel()
	user, ok := h.authenticatedUser(c, ctx, authFields{})
	if !ok {
		return
	}
	var body struct {
		Amount *float64 `json:"amount"`
		Note   string   `json:"note"`
	}
	if !decodeJSONBody(c, &body) {
		return
	}
	if body.Amount == nil || *body.Amount <= 0 || math.IsNaN(*body.Amount) || math.IsInf(*body.Amount, 0) {
		respondError(c, http.StatusBadRequest, "invalid_amount", fmt.Errorf("amount must be a positive finite number"))
		return
	}
	record, errRecharge := h.repo.ApplyBillingBalanceRecord(ctx, cluster.BillingBalanceUpdate{
		UserID: user.ID, Type: cluster.BillingBalanceTypeRecharge, Amount: *body.Amount,
		Operator: fmt.Sprintf("user:%d", user.ID), Note: body.Note,
	})
	if errRecharge != nil {
		respondError(c, http.StatusInternalServerError, "billing_recharge_failed", errRecharge)
		return
	}
	c.JSON(http.StatusOK, gin.H{"record": currentUserBillingBalanceRecordResponse(record), "current_balance": record.BalanceAfter})
}

func currentUserBillingBalanceRecordResponse(record *cluster.BillingBalanceRecord) gin.H {
	return gin.H{
		"id": record.ID, "type": record.Type, "amount": record.Amount,
		"balance_before": record.BalanceBefore, "balance_after": record.BalanceAfter,
		"created_at": record.CreatedAt,
	}
}
