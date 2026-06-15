package monitor

import (
	"fmt"
	"log"
	"strconv"
	"time"

	"budgetbridge/internal/pool"

	bss "github.com/alibabacloud-go/bssopenapi-20171214/v6/client"
	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/client"
	"github.com/alibabacloud-go/tea/tea"
)

func CheckBalance(p *pool.Pool, idx int) error {
	acc := p.Get(idx)
	if acc == nil {
		return fmt.Errorf("invalid index %d", idx)
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

	var total float64
	count := 0
	for _, c := range resp.Body.Data.CashCoupon {
		if tea.StringValue(c.Status) == "Available" {
			if b, err := strconv.ParseFloat(tea.StringValue(c.Balance), 64); err == nil {
				total += b
				count++
			}
		}
	}

	p.SetBalance(idx, total, count)
	log.Printf("[monitor] %s: balance=%.2f, coupons=%d", acc.Alias, total, count)
	return nil
}

func Run(p *pool.Pool, idx int) {
	if err := CheckBalance(p, idx); err != nil {
		log.Printf("[monitor] initial check idx=%d: %v", idx, err)
	}
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for range t.C {
		if err := CheckBalance(p, idx); err != nil {
			log.Printf("[monitor] check idx=%d: %v", idx, err)
		}
	}
}
