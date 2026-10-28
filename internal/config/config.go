// Package config 负责 PoolMate 配置的加载、保存与默认值。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Duration 以字符串形式（"168h"、"10m"）序列化的时长。
type Duration struct{ time.Duration }

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" || s == "0" {
		d.Duration = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	d.Duration = v
	return nil
}

// ModelOverride 每模型可调参数；零值字段 = 跟随上游默认。
type ModelOverride struct {
	ContextWindow int    `json:"context_window,omitempty"` // 覆盖展示的上下文长度（tokens）
	MaxOutput     int    `json:"max_output,omitempty"`     // 钳制请求 max_tokens 不超过该值
	Effort        string `json:"effort,omitempty"`         // 客户端未指定 reasoning_effort 时网关注入的默认档位
}

type GatewayCfg struct {
	APIKey          string                   `json:"api_key"`      // 兼容单密钥；为空则只用密钥库
	ModelNaming     string                   `json:"model_naming"` // short / full / both
	CreditInName    bool                     `json:"credit_in_model_name"`
	AllowAnonymous  bool                     `json:"allow_anonymous"`
	UpstreamTimeout Duration                 `json:"upstream_timeout"`
	ModelAliases    map[string]string        `json:"model_aliases,omitempty"`   // 别名 → 上游模型名（如 claude-sonnet → glm-5.3）
	ModelHide       []string                 `json:"model_hide,omitempty"`      // /v1/models 中隐藏的模型，支持尾部 * 通配
	ModelOverrides  map[string]ModelOverride `json:"model_overrides,omitempty"` // 每模型上下文/输出/默认档位覆盖
	Proxy           string                   `json:"proxy,omitempty"`           // 出站代理，形如 http://127.0.0.1:7890 或 socks5://127.0.0.1:1080，空 = 直连
}

type PoolCfg struct {
	MaxInFlight        int      `json:"max_in_flight"`
	PreferExpiring     bool     `json:"prefer_expiring"`
	ExpiringSoon       Duration `json:"expiring_soon"`
	CreditFloor        int64    `json:"credit_floor"`
	WasteBoost         float64  `json:"waste_boost"`
	IdleWeightPerHour  float64  `json:"idle_weight_per_hour"`
	IdleWeightMax      float64  `json:"idle_weight_max"`
	BreakerThreshold   int      `json:"breaker_threshold"`
	SoftRate           Duration `json:"soft_rate"`
	SoftRateMax        Duration `json:"soft_rate_max"`
	BreakerCooldown    Duration `json:"breaker_cooldown"`
	BreakerCooldownMax Duration `json:"breaker_cooldown_max"`
	DailyAccountBudget float64  `json:"daily_account_budget"` // 单账号每日积分上限，0 = 不限
	SessionStickyTTL   Duration `json:"session_sticky_ttl"`
	CostExplore        Duration `json:"cost_explore_interval"`
	MaxRetries         int      `json:"max_retries"` // 单请求最多尝试的账号数
}

type ScheduleCfg struct {
	CheckinHours          []int  `json:"checkin_hours"`
	CheckinEnabled        bool   `json:"checkin_enabled"`
	KeepaliveEnabled      bool   `json:"keepalive_enabled"`
	KeepaliveHour         int    `json:"keepalive_hour"`
	BalanceRefreshMinutes int    `json:"balance_refresh_minutes"`
	TokenHealthEnabled    bool   `json:"token_health_enabled"`
	TokenHealthHour       int    `json:"token_health_hour"`
	AutoBackupEnabled     bool   `json:"auto_backup_enabled"`
	AutoBackupHour        int    `json:"auto_backup_hour"`
	AutoBackupKeep        int    `json:"auto_backup_keep"`
	GrowthClaimEnabled    bool   `json:"growth_claim_enabled"` // 成长任务轮询：自动接受并领取已达标的奖励
	GrowthClaimMinutes    int    `json:"growth_claim_minutes"` // 轮询间隔（分钟），0 = 关
	WakeEnabled           bool   `json:"wake_enabled"`         // 定时唤醒：提前触发额度重置周期
	WakeHours             []int  `json:"wake_hours"`           // 唤醒整点
	WakeModel             string `json:"wake_model"`           // 唤醒用模型，空 = 自动选最低倍率
}

type NotifyCfg struct {
	Enabled         bool   `json:"enabled"`
	WecomWebhook    string `json:"wecom_webhook"`  // 企业微信机器人
	BarkURL         string `json:"bark_url"`       // 如 https://api.day.app/KEY
	ServerChanKey   string `json:"serverchan_key"` // Server酱 SendKey
	TelegramToken   string `json:"telegram_token"`
	TelegramChatID  string `json:"telegram_chat_id"`
	CustomURL       string `json:"custom_url"`        // 自定义 JSON webhook
	MinBalance      int64  `json:"min_balance"`       // 余额低于此值告警，0 = 关闭
	WasteDays       int    `json:"waste_days"`        // N 天内到期且预计用不完告警，0 = 关闭
	TokenExpiryDays int    `json:"token_expiry_days"` // 凭证剩余不足 N 天告警，默认 7
	DedupMinutes    int    `json:"dedup_minutes"`     // 同类告警去重窗口
	DosageAlert     bool   `json:"dosage_alert"`      // 上游用量提醒（get-dosage-notify）非 0 时告警
}

type PromptCfg struct {
	Mode string `json:"mode"` // passthrough / custom / append
	Text string `json:"text"`
}

type SecurityCfg struct {
	VaultEnabled bool   `json:"vault_enabled"` // 凭证加密落盘
	VaultHint    string `json:"vault_hint"`    // 主密码提示（不存密码）
}

type Config struct {
	mu       *sync.Mutex `json:"-"`
	path     string      `json:"-"`
	Listen   string      `json:"listen"`
	DataDir  string      `json:"data_dir"`
	AuthDir  string      `json:"auth_dir"`
	AdminKey string      `json:"admin_key"`
	Gateway  GatewayCfg  `json:"gateway"`
	Pool     PoolCfg     `json:"pool"`
	Schedule ScheduleCfg `json:"schedule"`
	Notify   NotifyCfg   `json:"notify"`
	Prompt   PromptCfg   `json:"prompt"`
	Security SecurityCfg `json:"security"`
}

func defaults() *Config {
	return &Config{
		mu:      &sync.Mutex{},
		Listen:  "127.0.0.1:7864",
		DataDir: "./data",
		AuthDir: "./auths",
		Gateway: GatewayCfg{ModelNaming: "short", UpstreamTimeout: Duration{300 * time.Second}},
		Pool: PoolCfg{
			MaxInFlight: 3, PreferExpiring: true, ExpiringSoon: Duration{168 * time.Hour},
			WasteBoost: 4, IdleWeightPerHour: 0.5, IdleWeightMax: 5,
			BreakerThreshold: 3, SoftRate: Duration{600 * time.Second}, SoftRateMax: Duration{2 * time.Hour},
			BreakerCooldown: Duration{30 * time.Minute}, BreakerCooldownMax: Duration{6 * time.Hour},
			SessionStickyTTL: Duration{30 * time.Minute}, CostExplore: Duration{30 * time.Minute}, MaxRetries: 3,
		},
		Schedule: ScheduleCfg{
			CheckinHours: []int{9, 21}, CheckinEnabled: true, KeepaliveEnabled: true, KeepaliveHour: 22,
			BalanceRefreshMinutes: 5, TokenHealthEnabled: true, TokenHealthHour: 8,
			AutoBackupEnabled: true, AutoBackupHour: 3, AutoBackupKeep: 14,
			GrowthClaimEnabled: true, GrowthClaimMinutes: 30,
			WakeHours: []int{8},
		},
		Notify: NotifyCfg{TokenExpiryDays: 7, WasteDays: 7, DedupMinutes: 60, DosageAlert: true},
		Prompt: PromptCfg{Mode: "passthrough"},
	}
}

func Load(path string) (*Config, error) {
	cfg := defaults()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg.path = path
			cfg.ApplyEnv()
			return cfg, cfg.Save()
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.path = path
	cfg.ApplyEnv()
	if cfg.AdminKey == "" {
		cfg.AdminKey = "pm-adm-" + randHex(12)
		_ = cfg.Save()
		fmt.Printf("[poolmate] 已生成面板管理密钥: %s\n", cfg.AdminKey)
	}
	return cfg, nil
}

func (c *Config) ApplyEnv() {
	if v := os.Getenv("POOLMATE_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("POOLMATE_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("POOLMATE_ADMIN_KEY"); v != "" {
		c.AdminKey = v
	}
}

func (c *Config) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saveLocked()
}

func (c *Config) saveLocked() error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// Update 以 fn 修改配置并落盘（fn 内不要做耗时操作）。
func (c *Config) Update(fn func(*Config)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(c)
	return c.saveLocked()
}

// Snapshot 返回当前配置的浅拷贝（指针段共享，只读使用安全）。
func (c *Config) Snapshot() Config {
	c.mu.Lock()
	defer c.mu.Unlock()
	return *c
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// RandHex 导出给其它包生成密钥/ID。
func RandHex(n int) string { return randHex(n) }
