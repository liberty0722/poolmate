// Package keys 下游 API 密钥库：模型白名单（通配）、RPM 限速、
// 每日/累计积分预算、过期时间、启停与轮换；独立统计用量。
package keys

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"poolmate/internal/config"
)

// APIKey 一把下游密钥。
type APIKey struct {
	ID          string     `json:"id"`
	Key         string     `json:"key"`
	Name        string     `json:"name"`
	Models      []string   `json:"models"` // 空 = 全部；支持 glm-* 通配
	RPM         int        `json:"rpm"`    // 0 = 不限
	DailyBudget float64    `json:"daily_budget"`
	TotalBudget float64    `json:"total_budget"`
	UsedDaily   float64    `json:"used_daily"`
	DailyDate   string     `json:"daily_date"`
	UsedTotal   float64    `json:"used_total"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	Disabled    bool       `json:"disabled"`
	CreatedAt   time.Time  `json:"created_at"`

	hits []time.Time `json:"-"` // RPM 滑动窗口
}

// Store 密钥库，JSON 原子落盘。
type Store struct {
	mu   sync.Mutex
	path string
	Keys map[string]*APIKey `json:"keys"` // key 字符串 → APIKey
}

type diskStore struct {
	Keys map[string]*APIKey `json:"keys"`
}

var (
	ErrInvalid  = errors.New("密钥无效")
	ErrDisabled = errors.New("密钥已停用")
	ErrExpired  = errors.New("密钥已过期")
	ErrModel    = errors.New("模型不在白名单")
	ErrRPM      = errors.New("超出每分钟速率限制")
	ErrDaily    = errors.New("超出每日积分预算")
	ErrTotal    = errors.New("超出累计积分预算")
)

func Load(path string) (*Store, error) {
	s := &Store{path: path, Keys: map[string]*APIKey{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var ds diskStore
	if err := json.Unmarshal(raw, &ds); err == nil && ds.Keys != nil {
		s.Keys = ds.Keys
	}
	return s, nil
}

func (s *Store) saveLocked() {
	raw, err := json.MarshalIndent(diskStore{Keys: s.Keys}, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.path), 0o755)
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		_ = os.Rename(tmp, s.path)
	}
}

// Ping 轻量校验：只查存在性/停用/过期，不扣 RPM、不查白名单与预算。
// 用于 /v1/models 这类无具体模型上下文的端点鉴权。
func (s *Store) Ping(keyStr string) error {
	_, err := s.Lookup(keyStr)
	return err
}

// Validate 校验密钥并扣 RPM（预算在实际产生积分后由 AddUsage 记账）。
func (s *Store) Validate(keyStr, model string) (*APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.Keys[keyStr]
	if !ok {
		return nil, ErrInvalid
	}
	if k.Disabled {
		return nil, ErrDisabled
	}
	if k.ExpiresAt != nil && time.Now().After(*k.ExpiresAt) {
		return nil, ErrExpired
	}
	if len(k.Models) > 0 && !matchAny(k.Models, model) {
		return nil, fmt.Errorf("%w：%s", ErrModel, model)
	}
	s.rollDaily(k)
	if k.DailyBudget > 0 && k.UsedDaily >= k.DailyBudget {
		return nil, ErrDaily
	}
	if k.TotalBudget > 0 && k.UsedTotal >= k.TotalBudget {
		return nil, ErrTotal
	}
	// RPM 放最后：被预算等拒绝的请求不占用每分钟配额
	if k.RPM > 0 {
		now := time.Now()
		cut := now.Add(-time.Minute)
		keep := k.hits[:0]
		for _, t := range k.hits {
			if t.After(cut) {
				keep = append(keep, t)
			}
		}
		k.hits = keep
		if len(k.hits) >= k.RPM {
			return nil, ErrRPM
		}
		k.hits = append(k.hits, now)
	}
	return k, nil
}

// Lookup 同 Ping，额外返回密钥对象。
func (s *Store) Lookup(keyStr string) (*APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.Keys[keyStr]
	if !ok {
		return nil, ErrInvalid
	}
	if k.Disabled {
		return nil, ErrDisabled
	}
	if k.ExpiresAt != nil && time.Now().After(*k.ExpiresAt) {
		return nil, ErrExpired
	}
	return k, nil
}

func (s *Store) rollDaily(k *APIKey) {
	day := time.Now().Format("2006-01-02")
	if k.DailyDate != day {
		k.DailyDate, k.UsedDaily = day, 0
	}
}

// AddUsage 给密钥记积分消耗。
func (s *Store) AddUsage(k *APIKey, credit float64) {
	if credit <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollDaily(k)
	k.UsedDaily += credit
	k.UsedTotal += credit
	s.saveLocked()
}

// Create 新建密钥，返回完整对象（key 明文仅此可见于返回与列表）。
func (s *Store) Create(name string, models []string, rpm int, daily, total float64, expires *time.Time) *APIKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := &APIKey{
		ID: config.RandHex(8), Key: "sk-pm-" + config.RandHex(16), Name: name,
		Models: models, RPM: rpm, DailyBudget: daily, TotalBudget: total,
		ExpiresAt: expires, CreatedAt: time.Now(),
	}
	s.Keys[k.Key] = k
	s.saveLocked()
	return k
}

// Update 修改密钥属性（按 ID）。
func (s *Store) Update(id string, fn func(*APIKey)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.Keys {
		if k.ID == id {
			fn(k)
			s.saveLocked()
			return true
		}
	}
	return false
}

// Delete 按 ID 删除。
func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, k := range s.Keys {
		if k.ID == id {
			delete(s.Keys, key)
			s.saveLocked()
			return true
		}
	}
	return false
}

// Rotate 换发新密钥字符串（用量与配置保留）。
func (s *Store) Rotate(id string) (*APIKey, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, k := range s.Keys {
		if k.ID == id {
			delete(s.Keys, key)
			k.Key = "sk-pm-" + config.RandHex(16)
			s.Keys[k.Key] = k
			s.saveLocked()
			return k, true
		}
	}
	return nil, false
}

// List 输出全部密钥（按创建时间）。
func (s *Store) List() []*APIKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*APIKey, 0, len(s.Keys))
	for _, k := range s.Keys {
		out = append(out, k)
	}
	return out
}

func matchAny(patterns []string, model string) bool {
	for _, p := range patterns {
		if globMatch(p, model) {
			return true
		}
	}
	return false
}

// globMatch 仅支持尾部 * 通配（glm-* 匹配 glm-5.2）。
func globMatch(pattern, s string) bool {
	if pattern == "*" || pattern == s {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(s, strings.TrimSuffix(pattern, "*"))
	}
	return false
}
