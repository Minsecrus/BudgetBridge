package main

import (
	"log"
	"os"

	"budgetbridge/internal/monitor"
	"budgetbridge/internal/pool"
	"budgetbridge/internal/proxy"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen      string             `yaml:"listen"`
	UpstreamURL string             `yaml:"upstream_url"`
	Accounts    []pool.AccountConfig `yaml:"accounts"`
}

func main() {
	data, err := os.ReadFile("config.yaml")
	if err != nil {
		log.Fatalf("config.yaml: %v", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("parse config: %v", err)
	}
	if cfg.UpstreamURL == "" {
		cfg.UpstreamURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	}
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}

	p := pool.New(cfg.Accounts)
	for i := range cfg.Accounts {
		go monitor.Run(p, i)
	}

	// saver writes pool accounts back to config.yaml on add
	saver := func(cfgs []pool.AccountConfig) error {
		cfg.Accounts = cfgs
		out, err := yaml.Marshal(cfg)
		if err != nil {
			return err
		}
		return os.WriteFile("config.yaml", out, 0644)
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery(), cors.Default())

	r.POST("/v1/chat/completions", proxy.Handler(p, cfg.UpstreamURL))

	adm := r.Group("/admin")
	adm.GET("/accounts", proxy.ListAccounts(p))
	adm.POST("/accounts", proxy.AddAccount(p, saver))
	adm.DELETE("/accounts", proxy.ClearAccounts(p, saver))
	adm.POST("/accounts/:index/toggle", proxy.ToggleAccount(p))
	adm.POST("/accounts/:index/refresh", proxy.RefreshAccount(p))
	adm.POST("/accounts/:index/cooldown/clear", proxy.ClearCooldown(p))

	log.Printf("BudgetBridge on %s (upstream: %s)", cfg.Listen, cfg.UpstreamURL)
	log.Fatal(r.Run(cfg.Listen))
}
