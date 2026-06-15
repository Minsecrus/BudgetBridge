package pool

import (
	"sync"
	"sync/atomic"
	"time"
)

type AccountConfig struct {
	Alias    string `yaml:"alias"`
	APIKey   string `yaml:"api_key"`
	AKId     string `yaml:"ak_id"`
	AKSecret string `yaml:"ak_secret"`
}

type Account struct {
	AccountConfig
	mu            sync.RWMutex
	Enabled       bool
	Balance       float64
	CouponCount   int
	LastChecked   time.Time
	CooldownUntil time.Time
	RequestCount  int64
}

func (a *Account) IsAvailable() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Enabled && time.Now().After(a.CooldownUntil)
}

type Pool struct {
	accounts []*Account
	counter  atomic.Int64
}

func New(cfgs []AccountConfig) *Pool {
	p := &Pool{}
	for i := range cfgs {
		p.accounts = append(p.accounts, &Account{AccountConfig: cfgs[i], Enabled: true})
	}
	return p
}

func (p *Pool) Len() int { return len(p.accounts) }

func (p *Pool) Get(idx int) *Account {
	if idx < 0 || idx >= len(p.accounts) {
		return nil
	}
	return p.accounts[idx]
}

func (p *Pool) Next() *Account {
	n := len(p.accounts)
	if n == 0 {
		return nil
	}
	start := int(p.counter.Add(1)-1) % n
	for i := range n {
		acc := p.accounts[(start+i)%n]
		if acc.IsAvailable() {
			atomic.AddInt64(&acc.RequestCount, 1)
			return acc
		}
	}
	return nil
}

func (p *Pool) Cooldown(acc *Account, d time.Duration) {
	acc.mu.Lock()
	acc.CooldownUntil = time.Now().Add(d)
	acc.mu.Unlock()
}

func (p *Pool) SetBalance(idx int, balance float64, couponCount int) {
	acc := p.Get(idx)
	if acc == nil {
		return
	}
	acc.mu.Lock()
	acc.Balance = balance
	acc.CouponCount = couponCount
	acc.LastChecked = time.Now()
	if balance < 3.0 {
		acc.Enabled = false
	}
	acc.mu.Unlock()
}

func (p *Pool) Toggle(idx int) bool {
	acc := p.Get(idx)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	acc.Enabled = !acc.Enabled
	acc.mu.Unlock()
	return true
}

func (p *Pool) ClearCooldown(idx int) bool {
	acc := p.Get(idx)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	acc.CooldownUntil = time.Time{}
	acc.mu.Unlock()
	return true
}

func (p *Pool) Add(cfg AccountConfig) int {
	p.accounts = append(p.accounts, &Account{AccountConfig: cfg, Enabled: true})
	return len(p.accounts) - 1
}

func (p *Pool) Configs() []AccountConfig {
	cfgs := make([]AccountConfig, len(p.accounts))
	for i, acc := range p.accounts {
		acc.mu.RLock()
		cfgs[i] = acc.AccountConfig
		acc.mu.RUnlock()
	}
	return cfgs
}

type Status struct {
	Index        int     `json:"index"`
	Alias        string  `json:"alias"`
	Enabled      bool    `json:"enabled"`
	Available    bool    `json:"available"`
	Balance      float64 `json:"balance"`
	CouponCount  int     `json:"coupon_count"`
	LastChecked  string  `json:"last_checked"`
	CooldownSecs int     `json:"cooldown_secs"`
	RequestCount int64   `json:"request_count"`
}

func (p *Pool) All() []Status {
	result := make([]Status, len(p.accounts))
	for i, acc := range p.accounts {
		acc.mu.RLock()
		cooldown := 0
		if rem := time.Until(acc.CooldownUntil); rem > 0 {
			cooldown = int(rem.Seconds())
		}
		lc := ""
		if !acc.LastChecked.IsZero() {
			lc = acc.LastChecked.Format(time.RFC3339)
		}
		result[i] = Status{
			Index:        i,
			Alias:        acc.Alias,
			Enabled:      acc.Enabled,
			Available:    acc.Enabled && time.Now().After(acc.CooldownUntil),
			Balance:      acc.Balance,
			CouponCount:  acc.CouponCount,
			LastChecked:  lc,
			CooldownSecs: cooldown,
			RequestCount: acc.RequestCount,
		}
		acc.mu.RUnlock()
	}
	return result
}
