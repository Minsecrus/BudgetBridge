package monitor

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"budgetbridge/internal/pool"
	"budgetbridge/internal/stats"

	bss "github.com/alibabacloud-go/bssopenapi-20171214/v6/client"
	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/client"
	"github.com/alibabacloud-go/tea/tea"
)

func CheckBalance(p *pool.Pool, acc *pool.Account, st *stats.Store) error {
	if acc == nil {
		return fmt.Errorf("nil account")
	}

	client, err := bss.NewClient(&openapi.Config{
		AccessKeyId:     tea.String(acc.AKId),
		AccessKeySecret: tea.String(acc.AKSecret),
		Endpoint:        tea.String("business.aliyuncs.com"),
	})
	if err != nil {
		return err
	}

	resp, err := client.QueryCashCoupons(&bss.QueryCashCouponsRequest{})
	if err != nil {
		return err
	}
	if !tea.BoolValue(resp.Body.Success) {
		return fmt.Errorf("BSS: %s", tea.StringValue(resp.Body.Message))
	}

	total, count := usableCoupons(resp.Body.Data.CashCoupon)

	acc.SetBalance(total, count)
	if st != nil {
		st.RecordBalance(acc.ID, total, count)
	}
	log.Printf("[monitor] %s: balance=%.2f, coupons=%d/%d available (按量付费)", acc.Alias, total, count, len(resp.Body.Data.CashCoupon))
	return nil
}

// payAsYouGoScenario marks a coupon that can offset usage-based (pay-as-you-go)
// bills — the only kind that can actually pay for DashScope model traffic.
const payAsYouGoScenario = "阿里云按量付费账单"

// usableCoupons sums the remaining balance of coupons that can offset
// pay-as-you-go bills (real 代金券). Aliyun's QueryCashCoupons returns 代金券
// and 满减券 ("满X减Y" threshold-discount coupons) side by side with NO type
// field. The 满减券 are scoped to 新购/升级 of specific products and never carry
// the 按量付费 scenario, so they can never offset usage bills — counting their
// face value would grossly inflate the reported balance. Filter on the 按量付费
// scenario: wording-independent, and directly tied to "can offset my traffic".
func usableCoupons(coupons []*bss.QueryCashCouponsResponseBodyDataCashCoupon) (total float64, count int) {
	for _, c := range coupons {
		if tea.StringValue(c.Status) != "Available" {
			continue
		}
		if !strings.Contains(tea.StringValue(c.ApplicableScenarios), payAsYouGoScenario) {
			continue
		}
		if b, err := strconv.ParseFloat(tea.StringValue(c.Balance), 64); err == nil {
			total += b
			count++
		}
	}
	return total, count
}

// Run polls the account balance every 5 minutes until ctx is cancelled.
func Run(ctx context.Context, p *pool.Pool, acc *pool.Account, st *stats.Store) {
	if err := CheckBalance(p, acc, st); err != nil {
		log.Printf("[monitor] initial check %s: %v", acc.Alias, err)
	}
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := CheckBalance(p, acc, st); err != nil {
				log.Printf("[monitor] check %s: %v", acc.Alias, err)
			}
		}
	}
}
