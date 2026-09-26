package userapi

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

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
