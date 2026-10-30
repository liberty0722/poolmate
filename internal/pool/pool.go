// Package pool 账号池：三因子加权选号（余额×10 + 快过期×8×wasteBoost + 闲置补偿）、
// Top-5 短名单抽签、防惊群、在途租约、会话粘性、成本分层（免费>未知>收费+搭车探索）、
// 分级冷却（429 软冷却/402 硬冷却至次日 4 点/404 浅冷却/6004 模型级）、连败熔断。
package pool

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"poolmate/internal/auth"
	"poolmate/internal/config"
	"poolmate/internal/upstream"
)

// EventKind 池事件（供告警）。
type EventKind string

const (
	EvAccountDisabled EventKind = "account_disabled"
	EvBreakerOpen     EventKind = "breaker_open"
	EvAuthFailed      EventKind = "auth_failed"
)

// Event 池事件回调载荷。
type Event struct {
	Kind   EventKind
	UID    string
	Name   string
	Detail string
}

// Account 池内账号的运行态。
type Account struct {
	Auth *auth.Auth

	Balance    int64              `json:"balance"`
	Packages   []upstream.Package `json:"packages,omitempty"`
	BalanceAt  time.Time          `json:"balance_at"`
	BalanceErr string             `json:"balance_err,omitempty"`

	Disabled  bool   `json:"disabled"` // 系统自动禁用（token 失效等）
	DisReason string `json:"dis_reason,omitempty"`
	Paused    bool   `json:"paused"` // 运维手动暂停（只摘对话流量）
	PauseWhy  string `json:"pause_why,omitempty"`

	CoolUntil time.Time            `json:"cool_until"`
	CoolKind  string               `json:"cool_kind,omitempty"`
	SoftN     int                  `json:"soft_n"`
	ModelCool map[string]time.Time `json:"model_cool,omitempty"`

	FailN        int       `json:"fail_n"`
	BreakerUntil time.Time `json:"breaker_until"`

	InFlight    int       `json:"-"`
	LastUsed    time.Time `json:"last_used"`
	LastPick    time.Time `json:"-"`
	DailyCredit float64   `json:"daily_credit"`
	DailyDate   string    `json:"daily_date"`

	Ledger map[string]*CostObs `json:"ledger,omitempty"` // (模型) → EMA 单价

	Tags        []string  `json:"tags,omitempty"`         // 运维标签
	PaymentType string    `json:"payment_type,omitempty"` // 上游付费类型 free/pro/enterprise…
	DosageCode  int       `json:"dosage_code,omitempty"`  // 上游用量提醒码，0 = 无提醒
	DosageMsg   string    `json:"dosage_msg,omitempty"`
	WokeAt      time.Time `json:"woke_at,omitempty"` // 最近一次定时唤醒

	// 余额变动追踪：每次刷新余额时与上次比对，区分「增加 / 周期重置 / 到期作废 / 其他渠道消耗」。
	Changes       []BalanceChange `json:"changes,omitempty"`
	ChgDate       string          `json:"chg_date,omitempty"`
	TodayAdded    int64           `json:"today_added,omitempty"`    // 今日增加（签到/领奖/充值/周期重置）
	TodayExpired  int64           `json:"today_expired,omitempty"`  // 今日到期作废
	TodayExternal int64           `json:"today_external,omitempty"` // 今日非网关渠道消耗（如客户端直连）
	SyncDaily     float64         `json:"sync_daily,omitempty"`     // 上次刷新余额时的网关当日消耗
	SyncDate      string          `json:"sync_date,omitempty"`
}

// BalanceChange 一次余额变动记录（delta 正为增加，负为减少）。
type BalanceChange struct {
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"` // add / reset / expire / spend
	Delta   int64     `json:"delta"`
	Balance int64     `json:"balance"` // 变动后总余额
	Note    string    `json:"note,omitempty"`
}

const maxChanges = 60

// trackChange 用新旧余额/积分包对比，推算本次刷新期间的各类变动。调用方持锁。
func (a *Account) trackChange(remain int64, packs []upstream.Package, now time.Time) {
	day := now.Format("2006-01-02")
	if a.ChgDate != day {
		a.ChgDate = day
		a.TodayAdded, a.TodayExpired, a.TodayExternal = 0, 0, 0
	}
	defer func() { a.SyncDaily, a.SyncDate = a.DailyCredit, a.DailyDate }()
	if a.BalanceAt.IsZero() || a.SyncDate == "" {
		return // 首次拉取 / 升级后第一次：只建立基线，不推算变动
	}
	gw := a.DailyCredit
	if a.SyncDate == a.DailyDate {
		gw -= a.SyncDaily
	}
	if gw < 0 {
		gw = 0
	}
	var lost int64
	for _, old := range a.Packages {
		if !old.HasCycle || old.Remain <= 0 || old.CycleEnd.After(now.Add(2*time.Minute)) {
			continue
		}
		found := false
		for _, np := range packs {
			if np.Name == old.Name && np.HasCycle && np.CycleEnd.Equal(old.CycleEnd) {
				found = true
				break
			}
		}
		if !found {
			lost += old.Remain
		}
	}
	reset := false
	for _, old := range a.Packages {
		for _, np := range packs {
			if np.Name == old.Name && old.HasCycle && np.HasCycle && np.CycleEnd.After(old.CycleEnd) {
				reset = true
			}
		}
	}
	adj := (remain - a.Balance) + int64(gw+0.5) + lost
	if lost > 0 {
		a.TodayExpired += lost
		a.addChange(BalanceChange{Time: now, Kind: "expire", Delta: -lost, Balance: remain, Note: "积分包到期，剩余部分作废"})
	}
	switch {
	case adj >= 2:
		kind, note := "add", "签到 / 领奖 / 充值等"
		if reset {
			kind, note = "reset", "周期额度重置"
		}
		a.TodayAdded += adj
		a.addChange(BalanceChange{Time: now, Kind: kind, Delta: adj, Balance: remain, Note: note})
	case adj <= -2:
		a.TodayExternal += -adj
		a.addChange(BalanceChange{Time: now, Kind: "spend", Delta: adj, Balance: remain, Note: "非 PoolMate 网关的消耗（如客户端直连）"})
	}
}

func (a *Account) addChange(c BalanceChange) {
	a.Changes = append(a.Changes, c)
	if n := len(a.Changes); n > maxChanges {
		a.Changes = append([]BalanceChange(nil), a.Changes[n-maxChanges:]...)
	}
}

// CostObs 单条成本观测（每千 token 积分单价，EMA 平滑，6h 过期）。
type CostObs struct {
	Price   float64   `json:"price"`
	Samples int       `json:"samples"`
	Updated time.Time `json:"updated"`
}

func (o *CostObs) stale() bool { return time.Since(o.Updated) > 6*time.Hour }

// Snapshot 账号对外视图。
type Snapshot struct {
	UID, Name, Realm string
	Balance          int64
	Packages         []upstream.Package
	BalanceAt        time.Time
	BalanceErr       string
	Status           string // ready / cooling / breaker / disabled / paused / budget
	StatusWhy        string
	CoolUntil        time.Time
	Expiring7d       int64
	NextExpiry       *time.Time
	DailyCredit      float64
	Ledger           map[string]CostObs
	ExpiryDays       float64
	TokenExpiresAt   time.Time
	EnterpriseID     string
	Tags             []string
	PaymentType      string
	DosageCode       int
	DosageMsg        string
	WokeAt           time.Time
	TodayAdded       int64
	TodayExpired     int64
	TodayExternal    int64
	Changes          []BalanceChange
}

// UsageObs 一次成功请求的上游用量。
type UsageObs struct {
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	Credit           float64
	HasCredit        bool
}

// Pool 账号池。
type Pool struct {
	mu       sync.Mutex
	cfg      *config.Config
	accounts map[string]*Account
	sticky   map[string]stickyBind
	state    string
	onEvent  func(Event)
	explore  map[string]time.Time // (模型) → 上次探索时刻
	rnd      *rand.Rand
}

type stickyBind struct {
	UID     string    `json:"uid"`
	Expires time.Time `json:"expires"`
}

// New 建池并加载运行态。
func New(cfg *config.Config, onEvent func(Event)) *Pool {
	p := &Pool{
		cfg: cfg, accounts: map[string]*Account{}, sticky: map[string]stickyBind{},
		state:   filepath.Join(cfg.Snapshot().DataDir, "state.json"),
		onEvent: onEvent, explore: map[string]time.Time{},
		rnd: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	p.load()
	return p
}

// SyncAccounts 用凭证目录对齐池成员（新增加入、消失移除、保留运行态）。
func (p *Pool) SyncAccounts(auths map[string]*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for uid, a := range auths {
		if _, ok := p.accounts[uid]; !ok {
			p.accounts[uid] = &Account{Auth: a, Ledger: map[string]*CostObs{}}
		} else {
			p.accounts[uid].Auth = a
		}
	}
	for uid := range p.accounts {
		if _, ok := auths[uid]; !ok {
			delete(p.accounts, uid)
		}
	}
}

// Get 取账号。
func (p *Pool) Get(uid string) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accounts[uid]
}

// StickyKey 从请求体派生会话粘性键：
// conversation 四键 → prompt_cache_key → user_id（Anthropic）→ 首条 user 消息 sha256。
func StickyKey(body map[string]any) string {
	if md, ok := body["metadata"].(map[string]any); ok {
		for _, k := range []string{"conversation_id", "conversationId"} {
			if s, ok := md[k].(string); ok && s != "" {
				return "conv:" + s
			}
		}
	}
	for _, k := range []string{"conversation_id", "conversationId"} {
		if s, ok := body[k].(string); ok && s != "" {
			return "conv:" + s
		}
	}
	if s, ok := body["prompt_cache_key"].(string); ok && s != "" {
		return "pck:" + s
	}
	if md, ok := body["metadata"].(map[string]any); ok {
		if s, ok := md["user_id"].(string); ok && s != "" {
			return "user:" + s
		}
	}
	if s, ok := body["user"].(string); ok && s != "" {
		return "user:" + s
	}
	if msgs, ok := body["messages"].([]any); ok {
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok && mm["role"] == "user" {
				var text string
				switch c := mm["content"].(type) {
				case string:
					text = c
				case []any:
					for _, part := range c {
						if pm, ok := part.(map[string]any); ok {
							if t, ok := pm["text"].(string); ok {
								text += t
							}
						}
					}
				}
				if text != "" {
					sum := sha256.Sum256([]byte(text))
					return "sha:" + hex.EncodeToString(sum[:8])
				}
			}
		}
	}
	return ""
}

var ErrNoAccount = errors.New("no_available_account")

// Select 选号：粘性 → 域过滤 → 成本分层（含搭车探索）→ 三因子加权 Top-5 抽签。
// realm 为空不限域；否则只选该域账号（模型前缀 cn:/global: 路由）。
// 返回账号与选号理由（写进请求日志，选号可回放）。
func (p *Pool) Select(stickyKey, model, realm string) (*Account, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cfg := p.cfg.Snapshot().Pool
	now := time.Now()
	ttl := cfg.SessionStickyTTL.Duration
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	// 1. 粘性命中
	if stickyKey != "" {
		if b, ok := p.sticky[stickyKey]; ok && now.Before(b.Expires) {
			if a, ok := p.accounts[b.UID]; ok && p.selectable(a, model, now, cfg) && (realm == "" || a.Auth.Snapshot().Realm == realm) {
				a.InFlight++
				a.LastPick = now
				p.sticky[stickyKey] = stickyBind{UID: b.UID, Expires: now.Add(ttl)}
				return a, "会话粘性命中（" + shortKey(stickyKey) + "）", nil
			}
		}
	}
	// 2. 候选集
	var cands []*Account
	var coolFallback *Account
	for _, a := range p.accounts {
		if a.Auth == nil {
			continue
		}
		if realm != "" && a.Auth.Snapshot().Realm != realm {
			continue
		}
		if p.selectable(a, model, now, cfg) {
			cands = append(cands, a)
			continue
		}
		// 全冷却兜底：仅因冷却/模型冷却被摘的账号里取最早解冻者。
		if !a.Disabled && !a.Paused && (now.Before(a.CoolUntil) || modelCooled(a, model, now)) {
			if coolFallback == nil || a.CoolUntil.Before(coolFallback.CoolUntil) {
				coolFallback = a
			}
		}
	}
	if len(cands) == 0 {
		if coolFallback != nil {
			coolFallback.InFlight++
			coolFallback.LastPick = now
			return coolFallback, "全部账号冷却中，取最早解冻者兜底", nil
		}
		return nil, "", ErrNoAccount
	}
	// 3. 成本分层：tier0 免费 / tier1 未知 / tier2 收费
	tierOf := func(a *Account) int {
		obs, ok := a.Ledger[model]
		if !ok || obs.stale() {
			return 1
		}
		if obs.Price <= 0 {
			return 0
		}
		return 2
	}
	minTier := 3
	for _, a := range cands {
		if t := tierOf(a); t < minTier {
			minTier = t
		}
	}
	// 搭车探索：tier0 垄断（仅 1 个实测免费号）且 tier1 有成员，且距上次探索超间隔，
	// 把本次选号改道给一个未知号学价格（承接真实请求，零上游增量）。
	if minTier == 0 && cfg.CostExplore.Duration > 0 {
		var free, unknown []*Account
		for _, a := range cands {
			switch tierOf(a) {
			case 0:
				free = append(free, a)
			case 1:
				unknown = append(unknown, a)
			}
		}
		if len(free) == 1 && len(unknown) > 0 && now.Sub(p.explore[model]) >= cfg.CostExplore.Duration {
			p.explore[model] = now
			a := unknown[p.rnd.Intn(len(unknown))]
			a.InFlight++
			a.LastPick = now
			return a, "成本搭车探索：让未知账号学价格，破免费号垄断", nil
		}
	}
	var tierCands []*Account
	for _, a := range cands {
		if tierOf(a) == minTier {
			tierCands = append(tierCands, a)
		}
	}
	// 4. 三因子加权
	type wc struct {
		a      *Account
		w      float64
		factor string
	}
	items := make([]wc, 0, len(tierCands))
	for _, a := range tierCands {
		w, factor := p.weight(a, cfg, now)
		items = append(items, wc{a, w, factor})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].w > items[j].w })
	if len(items) > 5 {
		items = items[:5] // Top-5 短名单
	}
	// 等权重防惊群：打乱同分顺序
	for i := len(items) - 1; i > 0; i-- {
		j := p.rnd.Intn(i + 1)
		if items[i].w == items[j].w {
			items[i], items[j] = items[j], items[i]
		}
	}
	var total float64
	for _, it := range items {
		total += it.w
	}
	// 名单内加权抽签 + 100ms 防惊群跳过
	pick := p.rnd.Float64() * total
	var chosen *wc
	for i := range items {
		pick -= items[i].w
		if pick <= 0 {
			chosen = &items[i]
			break
		}
	}
	if chosen == nil {
		chosen = &items[0]
	}
	if now.Sub(chosen.a.LastPick) < 100*time.Millisecond {
		for i := range items {
			if now.Sub(items[i].a.LastPick) >= 100*time.Millisecond {
				chosen = &items[i]
				break
			}
		}
	}
	chosen.a.InFlight++
	chosen.a.LastPick = now
	if stickyKey != "" {
		p.sticky[stickyKey] = stickyBind{UID: chosen.a.Auth.Snapshot().UID, Expires: now.Add(ttl)}
	}
	return chosen.a, fmt.Sprintf("加权抽签：%s（权重 %.2f，Top-%d）", chosen.factor, chosen.w, len(items)), nil
}

func shortKey(k string) string {
	if len(k) > 16 {
		return k[:16] + "…"
	}
	return k
}

func modelCooled(a *Account, model string, now time.Time) bool {
	if a.ModelCool == nil {
		return false
	}
	t, ok := a.ModelCool[model]
	return ok && now.Before(t)
}

// selectable 判断账号当前能否承接 model 的对话流量。
func (p *Pool) selectable(a *Account, model string, now time.Time, cfg config.PoolCfg) bool {
	if a.Auth == nil || a.Disabled || a.Paused {
		return false
	}
	if now.Before(a.CoolUntil) || now.Before(a.BreakerUntil) {
		return false
	}
	if modelCooled(a, model, now) {
		return false
	}
	if cfg.MaxInFlight > 0 && a.InFlight >= cfg.MaxInFlight {
		return false
	}
	if cfg.CreditFloor > 0 && !a.BalanceAt.IsZero() && a.Balance < cfg.CreditFloor {
		return false
	}
	if cfg.DailyAccountBudget > 0 {
		p.rollDaily(a, now)
		if a.DailyCredit >= cfg.DailyAccountBudget {
			return false
		}
	}
	return true
}

func (p *Pool) rollDaily(a *Account, now time.Time) {
	day := now.Format("2006-01-02")
	if a.DailyDate != day {
		a.DailyDate = day
		a.DailyCredit = 0
	}
}

// weight 三因子：余额比例×10 + 快过期积分占比×8×wasteBoost + 闲置补偿（0.5/h 封顶 5）。
func (p *Pool) weight(a *Account, cfg config.PoolCfg, now time.Time) (float64, string) {
	var maxBal int64 = 1
	for _, o := range p.accounts {
		if o.Balance > maxBal {
			maxBal = o.Balance
		}
	}
	wBal := 10 * float64(a.Balance) / float64(maxBal)
	var wExp float64
	if cfg.PreferExpiring && a.Balance > 0 {
		expiring := expiringWithin(a, now.Add(cfg.ExpiringSoon.Duration))
		boost := cfg.WasteBoost
		if boost <= 0 {
			boost = 1
		}
		wExp = 8 * (float64(expiring) / float64(a.Balance)) * boost
	}
	var wIdle float64
	if !a.LastUsed.IsZero() {
		wIdle = cfg.IdleWeightPerHour * now.Sub(a.LastUsed).Hours()
		if wIdle > cfg.IdleWeightMax {
			wIdle = cfg.IdleWeightMax
		}
	} else {
		wIdle = cfg.IdleWeightMax // 新号优先热身
	}
	factor := fmt.Sprintf("余额%.1f+到期%.1f+闲置%.1f", wBal, wExp, wIdle)
	return wBal + wExp + wIdle + 0.1, factor
}

func expiringWithin(a *Account, before time.Time) int64 {
	var n int64
	for _, pk := range a.Packages {
		if pk.HasCycle && pk.CycleEnd.Before(before) {
			n += pk.Remain
		}
	}
	return n
}

// Release 归还在途租约。
func (p *Pool) Release(a *Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a.InFlight > 0 {
		a.InFlight--
	}
}

// ReportSuccess 成功：清零连败、记用量（成本账本 EMA、当日积分、最近使用）。
func (p *Pool) ReportSuccess(a *Account, model string, u UsageObs) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	a.FailN = 0
	a.SoftN = 0
	a.LastUsed = now
	p.rollDaily(a, now)
	if u.HasCredit {
		a.DailyCredit += u.Credit
		if a.BalanceAt.IsZero() {
			a.BalanceAt = now
		}
		a.Balance -= int64(u.Credit + 0.5)
		if a.Balance < 0 {
			a.Balance = 0
		}
		tokens := u.PromptTokens + u.CompletionTokens
		if tokens > 0 && u.Credit >= 0 {
			price := u.Credit / (float64(tokens) / 1000)
			obs, ok := a.Ledger[model]
			if !ok || obs.stale() {
				obs = &CostObs{}
				a.Ledger[model] = obs
			}
			if obs.Samples == 0 {
				obs.Price = price
			} else {
				obs.Price = 0.7*obs.Price + 0.3*price
			}
			obs.Samples++
			obs.Updated = now
		}
	}
	p.saveLocked()
}

// ReportFailure 失败分类治理：429 软冷却、402 硬冷却至次日 4 点、401 刷新失败禁用、
// 404 浅冷却、6004 模型级冷却、其余连败熔断。
func (p *Pool) ReportFailure(a *Account, model string, kind upstream.ErrKind, detail string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cfg := p.cfg.Snapshot().Pool
	now := time.Now()
	emit := func(k EventKind, d string) {
		if p.onEvent != nil {
			s := a.Auth.Snapshot()
			p.onEvent(Event{Kind: k, UID: s.UID, Name: s.Nickname, Detail: d})
		}
	}
	switch kind {
	case upstream.ErrRateLimit:
		a.SoftN++
		d := cfg.SoftRate.Duration << min(a.SoftN-1, 6)
		if d > cfg.SoftRateMax.Duration {
			d = cfg.SoftRateMax.Duration
		}
		a.CoolUntil, a.CoolKind = now.Add(d), "429 限流"
	case upstream.ErrNoCredit:
		next := time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, now.Location())
		if now.After(next) {
			next = next.Add(24 * time.Hour)
		}
		a.CoolUntil, a.CoolKind = next, "余额耗尽（402，次日 4 点解冻）"
	case upstream.ErrNotFound:
		a.CoolUntil, a.CoolKind = now.Add(5*time.Minute), "404 浅冷却"
	case upstream.ErrModelRateLimit:
		if a.ModelCool == nil {
			a.ModelCool = map[string]time.Time{}
		}
		a.ModelCool[model] = now.Add(cfg.SoftRate.Duration)
	case upstream.ErrAuth:
		if !a.Disabled {
			a.Disabled, a.DisReason = true, "凭证失效："+detail
			emit(EvAuthFailed, a.DisReason)
		}
	case upstream.ErrContentBlocked:
		// 内容拦截不冷却账号，由网关降级重试处理。
	default:
		a.FailN++
		if a.FailN >= cfg.BreakerThreshold {
			n := a.FailN - cfg.BreakerThreshold
			d := cfg.BreakerCooldown.Duration << min(n, 5)
			if d > cfg.BreakerCooldownMax.Duration {
				d = cfg.BreakerCooldownMax.Duration
			}
			a.BreakerUntil = now.Add(d)
			emit(EvBreakerOpen, fmt.Sprintf("连败 %d 次，熔断至 %s", a.FailN, a.BreakerUntil.Format("15:04:05")))
		}
	}
	p.saveLocked()
}

// SetBalance 更新余额与积分包（余额恢复自动解冻 402 硬冷却）。
func (p *Pool) SetBalance(a *Account, remain int64, packs []upstream.Package, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		a.BalanceErr = err.Error()
		return
	}
	wasNoCredit := a.CoolKind == "余额耗尽（402，次日 4 点解冻）" && time.Now().Before(a.CoolUntil)
	a.trackChange(remain, packs, time.Now())
	a.Balance, a.Packages, a.BalanceAt, a.BalanceErr = remain, packs, time.Now(), ""
	if wasNoCredit && remain > 0 {
		a.CoolUntil, a.CoolKind = time.Time{}, ""
	}
	p.saveLocked()
}

// SetTags 覆盖账号标签（去空、去重、限长）。
func (p *Pool) SetTags(uid string, tags []string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.accounts[uid]
	if !ok {
		return false
	}
	seen := map[string]bool{}
	var clean []string
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] || len([]rune(t)) > 20 {
			continue
		}
		seen[t] = true
		clean = append(clean, t)
		if len(clean) >= 10 {
			break
		}
	}
	a.Tags = clean
	p.saveLocked()
	return true
}

// TagsOf 返回账号标签副本。
func (p *Pool) TagsOf(uid string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a, ok := p.accounts[uid]; ok {
		return append([]string(nil), a.Tags...)
	}
	return nil
}

// SetMeta 记录上游付费类型与用量提醒。
func (p *Pool) SetMeta(a *Account, payment string, dosageCode int, dosageMsg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if payment != "" {
		a.PaymentType = payment
	}
	a.DosageCode, a.DosageMsg = dosageCode, dosageMsg
	p.saveLocked()
}

// MarkWoke 记录定时唤醒时间。
func (p *Pool) MarkWoke(a *Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a.WokeAt = time.Now()
	p.saveLocked()
}

// Pause/Resume/Revive 运维操作。
func (p *Pool) Pause(uid, why string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a, ok := p.accounts[uid]; ok {
		a.Paused, a.PauseWhy = true, why
		p.saveLocked()
		return true
	}
	return false
}

func (p *Pool) Resume(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a, ok := p.accounts[uid]; ok {
		a.Paused, a.PauseWhy = false, ""
		p.saveLocked()
		return true
	}
	return false
}

// Revive 解冻：清冷却/熔断/禁用三位。
func (p *Pool) Revive(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a, ok := p.accounts[uid]; ok {
		a.CoolUntil, a.CoolKind, a.BreakerUntil = time.Time{}, "", time.Time{}
		a.Disabled, a.DisReason, a.FailN, a.SoftN = false, "", 0, 0
		a.ModelCool = nil
		p.saveLocked()
		return true
	}
	return false
}

// Remove 从池中移除（凭证已删时调用）。
func (p *Pool) Remove(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.accounts, uid)
	p.saveLocked()
}

// List 输出账号快照（按余额降序）。
func (p *Pool) List() []Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	cfg := p.cfg.Snapshot().Pool
	out := make([]Snapshot, 0, len(p.accounts))
	for _, a := range p.accounts {
		s := a.Auth.Snapshot()
		snap := Snapshot{
			UID: s.UID, Name: s.Nickname, Realm: s.Realm,
			Balance: a.Balance, Packages: a.Packages, BalanceAt: a.BalanceAt, BalanceErr: a.BalanceErr,
			CoolUntil: a.CoolUntil, DailyCredit: a.DailyCredit,
			EnterpriseID: s.EnterpriseID, Tags: append([]string(nil), a.Tags...),
			PaymentType: a.PaymentType, DosageCode: a.DosageCode, DosageMsg: a.DosageMsg, WokeAt: a.WokeAt,
			Ledger: map[string]CostObs{},
		}
		snap.TokenExpiresAt, snap.ExpiryDays = a.Auth.Expiry()
		if a.ChgDate == now.Format("2006-01-02") {
			snap.TodayAdded, snap.TodayExpired, snap.TodayExternal = a.TodayAdded, a.TodayExpired, a.TodayExternal
		}
		snap.Changes = append([]BalanceChange(nil), a.Changes...)
		snap.Expiring7d = expiringWithin(a, now.Add(7*24*time.Hour))
		var next *time.Time
		for _, pk := range a.Packages {
			if pk.HasCycle && pk.Remain > 0 && (next == nil || pk.CycleEnd.Before(*next)) {
				t := pk.CycleEnd
				next = &t
			}
		}
		snap.NextExpiry = next
		for k, v := range a.Ledger {
			snap.Ledger[k] = *v
		}
		switch {
		case a.Disabled:
			snap.Status, snap.StatusWhy = "disabled", a.DisReason
		case a.Paused:
			snap.Status, snap.StatusWhy = "paused", a.PauseWhy
		case now.Before(a.BreakerUntil):
			snap.Status, snap.StatusWhy = "breaker", "熔断至 "+a.BreakerUntil.Format("15:04:05")
		case now.Before(a.CoolUntil):
			snap.Status, snap.StatusWhy = "cooling", a.CoolKind+" 至 "+a.CoolUntil.Format("15:04:05")
		case cfg.DailyAccountBudget > 0 && a.DailyCredit >= cfg.DailyAccountBudget:
			snap.Status, snap.StatusWhy = "budget", "已达当日预算"
		default:
			snap.Status = "ready"
		}
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Balance > out[j].Balance })
	return out
}

// Summary 池级聚合（供概览/状态）。
func (p *Pool) Summary() (ready, total int, balance int64, expiring7d int64) {
	for _, s := range p.List() {
		total++
		balance += s.Balance
		expiring7d += s.Expiring7d
		if s.Status == "ready" {
			ready++
		}
	}
	return
}

// 状态持久化：冷却/熔断/禁用/账本/当日积分，原子落盘。
type diskState struct {
	Accounts map[string]*diskAccount `json:"accounts"`
	Sticky   map[string]stickyBind   `json:"sticky,omitempty"`
	Saved    time.Time               `json:"saved"`
}

type diskAccount struct {
	Balance      int64                `json:"balance"`
	BalanceAt    time.Time            `json:"balance_at"`
	Packages     []upstream.Package   `json:"packages,omitempty"`
	Disabled     bool                 `json:"disabled"`
	DisReason    string               `json:"dis_reason,omitempty"`
	Paused       bool                 `json:"paused"`
	PauseWhy     string               `json:"pause_why,omitempty"`
	CoolUntil    time.Time            `json:"cool_until"`
	CoolKind     string               `json:"cool_kind,omitempty"`
	SoftN        int                  `json:"soft_n"`
	ModelCool    map[string]time.Time `json:"model_cool,omitempty"`
	FailN        int                  `json:"fail_n"`
	BreakerUntil time.Time            `json:"breaker_until"`
	LastUsed     time.Time            `json:"last_used"`
	DailyCredit  float64              `json:"daily_credit"`
	DailyDate    string               `json:"daily_date"`
	Ledger       map[string]*CostObs  `json:"ledger,omitempty"`
	Tags         []string             `json:"tags,omitempty"`
	PaymentType  string               `json:"payment_type,omitempty"`
	DosageCode   int                  `json:"dosage_code,omitempty"`
	DosageMsg    string               `json:"dosage_msg,omitempty"`
	WokeAt       time.Time            `json:"woke_at,omitempty"`

	Changes       []BalanceChange `json:"changes,omitempty"`
	ChgDate       string          `json:"chg_date,omitempty"`
	TodayAdded    int64           `json:"today_added,omitempty"`
	TodayExpired  int64           `json:"today_expired,omitempty"`
	TodayExternal int64           `json:"today_external,omitempty"`
	SyncDaily     float64         `json:"sync_daily,omitempty"`
	SyncDate      string          `json:"sync_date,omitempty"`
}

func (p *Pool) saveLocked() {
	ds := diskState{Accounts: map[string]*diskAccount{}, Sticky: p.sticky, Saved: time.Now()}
	for uid, a := range p.accounts {
		ds.Accounts[uid] = &diskAccount{
			Balance: a.Balance, BalanceAt: a.BalanceAt, Packages: a.Packages,
			Disabled: a.Disabled, DisReason: a.DisReason, Paused: a.Paused, PauseWhy: a.PauseWhy,
			CoolUntil: a.CoolUntil, CoolKind: a.CoolKind, SoftN: a.SoftN, ModelCool: a.ModelCool,
			FailN: a.FailN, BreakerUntil: a.BreakerUntil, LastUsed: a.LastUsed,
			DailyCredit: a.DailyCredit, DailyDate: a.DailyDate, Ledger: a.Ledger,
			Tags: a.Tags, PaymentType: a.PaymentType, DosageCode: a.DosageCode, DosageMsg: a.DosageMsg, WokeAt: a.WokeAt,
			Changes: a.Changes, ChgDate: a.ChgDate, TodayAdded: a.TodayAdded, TodayExpired: a.TodayExpired,
			TodayExternal: a.TodayExternal, SyncDaily: a.SyncDaily, SyncDate: a.SyncDate,
		}
	}
	raw, err := json.Marshal(ds)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p.state), 0o755)
	tmp := p.state + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		_ = os.Rename(tmp, p.state)
	}
}

func (p *Pool) load() {
	raw, err := os.ReadFile(p.state)
	if err != nil {
		return
	}
	var ds diskState
	if json.Unmarshal(raw, &ds) != nil {
		return
	}
	p.sticky = ds.Sticky
	if p.sticky == nil {
		p.sticky = map[string]stickyBind{}
	}
	for uid, da := range ds.Accounts {
		a := &Account{
			Balance: da.Balance, BalanceAt: da.BalanceAt, Packages: da.Packages,
			Disabled: da.Disabled, DisReason: da.DisReason, Paused: da.Paused, PauseWhy: da.PauseWhy,
			CoolUntil: da.CoolUntil, CoolKind: da.CoolKind, SoftN: da.SoftN, ModelCool: da.ModelCool,
			FailN: da.FailN, BreakerUntil: da.BreakerUntil, LastUsed: da.LastUsed,
			DailyCredit: da.DailyCredit, DailyDate: da.DailyDate,
			Ledger: da.Ledger,
			Tags:   da.Tags, PaymentType: da.PaymentType, DosageCode: da.DosageCode, DosageMsg: da.DosageMsg, WokeAt: da.WokeAt,
			Changes: da.Changes, ChgDate: da.ChgDate, TodayAdded: da.TodayAdded, TodayExpired: da.TodayExpired,
			TodayExternal: da.TodayExternal, SyncDaily: da.SyncDaily, SyncDate: da.SyncDate,
		}
		if a.Ledger == nil {
			a.Ledger = map[string]*CostObs{}
		}
		p.accounts[uid] = a // Auth 由 SyncAccounts 关联
	}
}

// SaveAuth 凭证变化后落盘 + 状态落盘（由面板/调度调用方做持久化，这里只存状态）。
func (p *Pool) Save() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.saveLocked()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
