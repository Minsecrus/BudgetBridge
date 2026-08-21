package monitor

import (
	"math"
	"testing"

	bss "github.com/alibabacloud-go/bssopenapi-20171214/v6/client"
	tea "github.com/alibabacloud-go/tea/tea"
)

func ptr(s string) *string { return tea.String(s) }

// TestUsableCouponsExcludesThresholdDiscount guards the 代金券-vs-满减券 filter.
// Coupon shapes are copied from a real QueryCashCoupons response of an account
// that mixes both: six 满减券 ("满X减Y", scoped to 新购/升级, no 按量付费 scenario)
// plus one 代金券 (通用, includes 按量付费). Only the 代金券 must be counted — the
// 满减券 can never offset usage-based bills, so their face value must not inflate
// the reported balance.
func TestUsableCouponsExcludesThresholdDiscount(t *testing.T) {
	manjian := "阿里云新购,阿里云升级,万网购买,万网升级" // 满减券: no pay-as-you-go
	voucher := "其他,阿里云新购,阿里云续费,阿里云升级,阿里云按量付费账单,万网购买,阿里云降级"

	coupons := []*bss.QueryCashCouponsResponseBodyDataCashCoupon{
		{Status: ptr("Available"), ApplicableScenarios: ptr(manjian), Balance: ptr("150.00"), Description: ptr("个人满3000元减150元")},
		{Status: ptr("Available"), ApplicableScenarios: ptr(manjian), Balance: ptr("100.00"), Description: ptr("个人满2000元减100元")},
		{Status: ptr("Available"), ApplicableScenarios: ptr(manjian), Balance: ptr("50.00"), Description: ptr("个人满1000元减50元")},
		{Status: ptr("Available"), ApplicableScenarios: ptr(manjian), Balance: ptr("10.00"), Description: ptr("个人满200元减10元")},
		{Status: ptr("Available"), ApplicableScenarios: ptr(manjian), Balance: ptr("20.00"), Description: ptr("个人满400元减20元")},
		{Status: ptr("Available"), ApplicableScenarios: ptr(manjian), Balance: ptr("30.00"), Description: ptr("个人满600元减30元")},
		{Status: ptr("Available"), ApplicableScenarios: ptr(voucher), Balance: ptr("296.87"), Description: ptr("阿里云云工开物学生专属代金券")},
		// Expired but pay-as-you-go-scoped: still excluded (status not Available).
		{Status: ptr("Expired"), ApplicableScenarios: ptr("阿里云按量付费账单"), Balance: ptr("999.00")},
	}

	total, count := usableCoupons(coupons)
	if count != 1 {
		t.Fatalf("count = %d, want 1 (only the 代金券; all 满减券 must be excluded)", count)
	}
	if math.Abs(total-296.87) > 0.001 {
		t.Fatalf("total = %.4f, want 296.87 (the single 代金券's balance)", total)
	}
}
