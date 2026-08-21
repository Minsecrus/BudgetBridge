package proxy

import (
	"net/http"
	"time"

	"budgetbridge/internal/pool"

	"github.com/gin-gonic/gin"
)

// This file implements the two OpenAI "billing" endpoints that new-api / one-api
// poll to read a channel's remaining quota:
//
//	GET /v1/dashboard/billing/subscription  → OpenAISubscriptionResponse
//	GET /v1/dashboard/billing/usage         → OpenAIUsageResponse
//
// new-api computes channel balance as:
//
//	balance = hard_limit_usd - total_usage/100
//
// BudgetBridge doesn't track dollar usage, so total_usage is always 0 and the
// live aggregate balance is reported straight through hard_limit_usd. The
// currency (CNY vouchers) is reported as-is — the operator is expected to set
// new-api's exchange rate to 1 so the number reads correctly there.
//
// These are the only two shapes new-api unmarshals (controller/channel-billing.go),
// so we match them field-for-field; extra keys are ignored by its struct decode.

// accessUntil is a far-future timestamp returned in the subscription response.
// new-api treats access_until as an expiry: a value in the past would make the
// channel look expired, so we set it ~1 year out (recomputed each call so a
// long-running process never drifts into "expired").
func accessUntil() int64 {
	return time.Now().Add(365 * 24 * time.Hour).Unix()
}

// SubscriptionHandler reports the current aggregate balance as hard_limit_usd.
// Guards (keyAuth) are applied at route registration in main.go.
func SubscriptionHandler(p *pool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		balance := p.TotalBalance()
		c.JSON(http.StatusOK, gin.H{
			"object":                "billing_subscription",
			"has_payment_method":    true,
			"soft_limit_usd":        balance,
			"hard_limit_usd":        balance,
			"system_hard_limit_usd": balance,
			"access_until":          accessUntil(),
			// plan/title placate some clients that read these fields.
			"plan": gin.H{"id": "budgetbridge", "title": "BudgetBridge"},
		})
	}
}

// UsageHandler reports zero usage: BudgetBridge is a balance aggregator, not a
// metering gateway, so it can't reconstruct OpenAI-style dollar consumption.
// With total_usage=0, new-api's balance formula collapses to hard_limit_usd,
// which is exactly the live balance we report in the subscription response.
func UsageHandler(p *pool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"object":      "list",
			"total_usage": 0,
		})
	}
}
