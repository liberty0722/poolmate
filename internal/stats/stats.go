// Package stats 请求日志与用量统计：内存环形缓冲（2000 条）+ 按日 JSONL 归档（30 天）、
// 按模型/账号/密钥/协议拆分、星期×小时热力图、近 24 小时趋势。
package stats

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"poolmate/internal/config"
)

// Entry 一条请求日志（不记录对话内容）。
type Entry struct {
	ID           string    `json:"id"`
	Time         time.Time `json:"time"`
	Model        string    `json:"model"`
	UID          string    `json:"uid"`
	AccountName  string    `json:"account_name"`
	KeyID        string    `json:"key_id"`
	KeyName      string    `json:"key_name"`
	Protocol     string    `json:"protocol"` // openai / anthropic
	Stream       bool      `json:"stream"`
	Status       string    `json:"status"` // ok / error
	ErrKind      string    `json:"err_kind,omitempty"`
	ErrMsg       string    `json:"err_msg,omitempty"`
	PromptTokens int64     `json:"prompt_tokens"`
	CompTokens   int64     `json:"completion_tokens"`
	CachedTokens int64     `json:"cached_tokens"`
	Credits      float64   `json:"credits"`
	FirstTokenMs int64     `json:"first_token_ms"`
	TotalMs      int64     `json:"total_ms"`
	Retries      int       `json:"retries"`
	SelectReason string    `json:"select_reason"`
}

const ringSize = 2000

// Store 日志与聚合存储。
type Store struct {
	mu    sync.Mutex
	dir   string
	ring  []Entry
	head  int
	count int
	// 聚合：按模型 / 账号 / 密钥 / 协议
	byModel   map[string]*Agg
	byAccount map[string]*Agg
	byKey     map[string]*Agg
	byProto   map[string]*Agg
	heat      [7][24]*heatCell
	hours     map[string]*Agg // "2006-01-02T15" → 近 24h 趋势
}

// Agg 聚合桶。
type Agg struct {
	Requests  int64   `json:"requests"`
	Errors    int64   `json:"errors"`
	Tokens    int64   `json:"tokens"`
	Credits   float64 `json:"credits"`
	FirstMs   int64   `json:"first_ms"`
	FirstCnt  int64   `json:"first_cnt"`
	CacheHits int64   `json:"cache_hits"`
}

type heatCell struct {
	Requests int64   `json:"requests"`
	Credits  float64 `json:"credits"`
}

// New 建库并从近 31 天归档重建聚合。
func New(dir string) *Store {
	s := &Store{
		dir: dir, ring: make([]Entry, ringSize),
		byModel: map[string]*Agg{}, byAccount: map[string]*Agg{},
		byKey: map[string]*Agg{}, byProto: map[string]*Agg{},
		hours: map[string]*Agg{},
	}
	for d := 0; d < 7; d++ {
		for h := 0; h < 24; h++ {
			s.heat[d][h] = &heatCell{}
		}
	}
	s.rebuild()
	return s
}

func (s *Store) rebuild() {
	cutoff := time.Now().AddDate(0, 0, -31)
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		f, err := os.Open(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			var en Entry
			if json.Unmarshal(sc.Bytes(), &en) == nil && en.Time.After(cutoff) {
				s.aggregate(en)
			}
		}
		f.Close()
	}
	s.cleanOld()
}

func (s *Store) cleanOld() {
	cutoff := time.Now().AddDate(0, 0, -30)
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		day, err := time.Parse("2006-01-02", strings.TrimSuffix(e.Name(), ".jsonl"))
		if err == nil && day.Before(cutoff) {
			_ = os.Remove(filepath.Join(s.dir, e.Name()))
		}
	}
}

func (s *Store) aggregate(e Entry) {
	add := func(m map[string]*Agg, k string) {
		if k == "" {
			k = "-"
		}
		a := m[k]
		if a == nil {
			a = &Agg{}
			m[k] = a
		}
		a.Requests++
		if e.Status != "ok" {
			a.Errors++
		}
		a.Tokens += e.PromptTokens + e.CompTokens
		a.Credits += e.Credits
		a.CacheHits += e.CachedTokens
		if e.FirstTokenMs > 0 {
			a.FirstMs += e.FirstTokenMs
			a.FirstCnt++
		}
	}
	add(s.byModel, e.Model)
	add(s.byAccount, e.AccountName)
	add(s.byKey, e.KeyName)
	add(s.byProto, e.Protocol)
	d, h := int(e.Time.Weekday()), e.Time.Hour()
	s.heat[d][h].Requests++
	s.heat[d][h].Credits += e.Credits
	add(s.hours, e.Time.Format("2006-01-02T15"))
}

// Log 记录一条请求（写环形缓冲 + 归档 + 聚合）。
func (s *Store) Log(e Entry) {
	if e.ID == "" {
		e.ID = config.RandHex(8)
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	s.mu.Lock()
	s.ring[s.head] = e
	s.head = (s.head + 1) % ringSize
	if s.count < ringSize {
		s.count++
	}
	s.aggregate(e)
	s.mu.Unlock()
	// 归档（锁外）
	_ = os.MkdirAll(s.dir, 0o755)
	f, err := os.OpenFile(filepath.Join(s.dir, e.Time.Format("2006-01-02")+".jsonl"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	raw, _ := json.Marshal(e)
	_, _ = f.Write(append(raw, '\n'))
}

// Filter 日志查询条件。
type Filter struct {
	Model, UID, KeyID, Status, Protocol string
	Limit                               int
}

// Query 倒序返回匹配日志。
func (s *Store) Query(f Filter) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	out := []Entry{}
	for i := 0; i < s.count && len(out) < f.Limit; i++ {
		e := s.ring[(s.head-1-i+ringSize)%ringSize]
		if f.Model != "" && e.Model != f.Model {
			continue
		}
		if f.UID != "" && e.UID != f.UID {
			continue
		}
		if f.KeyID != "" && e.KeyID != f.KeyID {
			continue
		}
		if f.Status != "" && e.Status != f.Status {
			continue
		}
		if f.Protocol != "" && e.Protocol != f.Protocol {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Today 今日聚合。
func (s *Store) Today() Agg {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out Agg
	day := time.Now().Format("2006-01-02")
	for hk, a := range s.hours {
		if strings.HasPrefix(hk, day) {
			out.Requests += a.Requests
			out.Errors += a.Errors
			out.Tokens += a.Tokens
			out.Credits += a.Credits
			out.CacheHits += a.CacheHits
			out.FirstMs += a.FirstMs
			out.FirstCnt += a.FirstCnt
		}
	}
	return out
}

// Last24h 近 24 小时逐时序列（补齐空小时）。
func (s *Store) Last24h() []Agg {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Agg, 24)
	now := time.Now().Truncate(time.Hour)
	for i := 0; i < 24; i++ {
		k := now.Add(-time.Duration(23-i) * time.Hour).Format("2006-01-02T15")
		if a := s.hours[k]; a != nil {
			out[i] = *a
		}
	}
	return out
}

// Heatmap 星期×小时矩阵（行=周日..周六）。
func (s *Store) Heatmap() [7][24]heatCell {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [7][24]heatCell
	for d := 0; d < 7; d++ {
		for h := 0; h < 24; h++ {
			out[d][h] = *s.heat[d][h]
		}
	}
	return out
}

// Breakdown 按维度拆分。
func (s *Store) Breakdown() map[string]map[string]Agg {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := func(m map[string]*Agg) map[string]Agg {
		out := map[string]Agg{}
		for k, v := range m {
			out[k] = *v
		}
		return out
	}
	return map[string]map[string]Agg{
		"model": cp(s.byModel), "account": cp(s.byAccount),
		"key": cp(s.byKey), "protocol": cp(s.byProto),
	}
}

// TopModels 供面板默认排序。
func (s *Store) TopModels(n int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	type kv struct {
		k string
		v int64
	}
	var arr []kv
	for k, a := range s.byModel {
		arr = append(arr, kv{k, a.Requests})
	}
	sort.Slice(arr, func(i, j int) bool { return arr[i].v > arr[j].v })
	var out []string
	for i, x := range arr {
		if i >= n {
			break
		}
		out = append(out, x.k)
	}
	return out
}
