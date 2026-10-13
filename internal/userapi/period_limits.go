package userapi

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"

	"github.com/gin-gonic/gin"
)

// ResetCurrentUserPeriodLimits consumes one allowance for all enabled windows.
func (h *Handler) ResetCurrentUserPeriodLimits(c *gin.Context) {
	ctx, cancel := requestContext(c)
	defer cancel()
	user, ok := h.authenticatedUser(c, ctx, authFields{})
	if !ok {
		return
	}
	if len(c.Request.URL.Query()) != 0 {
		respondError(c, http.StatusBadRequest, "invalid body", errors.New("reset accepts no selectors or body"))
		return
	}
	if c.Request.Body != nil {
		body, errRead := io.ReadAll(io.LimitReader(c.Request.Body, 1))
		if errRead != nil || len(body) != 0 {
			respondError(c, http.StatusBadRequest, "invalid body", errors.New("reset accepts no selectors or body"))
			return
		}
	}
	status, errReset := h.repo.ConsumeUserResetAllowance(ctx, user.ID, time.Now().UTC())
	if errReset != nil {
		if errors.Is(errReset, cluster.ErrResetAllowanceNoWindows) {
			respondError(c, http.StatusConflict, "reset_allowance_no_enabled_windows", errReset)
		} else if errors.Is(errReset, cluster.ErrResetAllowanceUnavailable) {
			respondError(c, http.StatusConflict, "reset_allowance_unavailable", errReset)
		} else {
			respondUserError(c, "period_limits_reset_failed", errReset)
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "limits": status})
}

// CurrentUserPeriodLimits returns period-limit status for the authenticated user.
func (h *Handler) CurrentUserPeriodLimits(c *gin.Context) {
	ctx, cancel := requestContext(c)
	defer cancel()

	user, ok := h.authenticatedUser(c, ctx, authFields{})
	if !ok {
		return
	}

	status, errStatus := h.repo.BuildUserPeriodLimitsStatus(ctx, user.ID, time.Now().UTC())
	if errStatus != nil {
		respondUserError(c, "period_limits_load_failed", errStatus)
		return
	}
	c.JSON(http.StatusOK, status)
}
