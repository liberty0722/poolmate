// Package scheduler 定时任务：签到、token 保活、余额/积分包刷新、
// 凭证健康检查（到期预警）、积分浪费预测告警、自动备份。记录执行历史。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"poolmate/internal/auth"
	"poolmate/internal/backup"
	"poolmate/internal/config"
	"poolmate/internal/growth"
	"poolmate/internal/notify"
	"poolmate/internal/pool"
	"poolmate/internal/upstream"
)

// Deps 运行依赖。
type Deps struct {
	Cfg      *config.Config
	Pool     *pool.Pool
	Client   *upstream.Client
	Notifier *notify.Notifier
	Growth   *growth.Service
	// ReloadAuths 重新加载凭证目录并同步池（登录/导入后调用）。
	ReloadAuths func()
	// AuthDir/StateFile/Password 供自动备份。
	AuthDir   string
	StateFile string
}

// TaskRun 一次任务执行记录。
type TaskRun struct {
	Task    string    `json:"task"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Source  string    `json:"source"` // schedule / manual
	OK      int       `json:"ok"`
	Already int       `json:"already"`
	Failed  int       `json:"failed"`
	Skipped int       `json:"skipped"`
	Detail  string    `json:"detail,omitempty"`
}

// TaskInfo 任务元数据与下次执行时间。
type TaskInfo struct {
	Name    string     `json:"name"`
	Desc    string     `json:"desc"`
	Enabled bool       `json:"enabled"`
	NextRun *time.Time `json:"next_run,omitempty"`
	Running bool       `json:"running"`
}

type taskDef struct {
	name     string
	desc     string
	periodic bool // true = 按间隔循环（balance/growth_claim），false = 整点每日任务
	// hours 整点每日任务的触发小时列表；interval 周期任务的间隔。
	hours    func(c config.ScheduleCfg) []int
	interval func(c config.ScheduleCfg) time.Duration
	run      func(s *Scheduler) TaskRun
	gate     func(c config.ScheduleCfg) bool
}

// Scheduler 定时调度器。
type Scheduler struct {
	deps     Deps
	mu       sync.Mutex
	running  map[string]bool
	history  []TaskRun
	lastFire map[string]time.Time // 整点任务上次被调度触发的时刻（防重）
	lastRun  map[string]time.Time // 周期任务上次执行时刻
}

func New(deps Deps) *Scheduler {
	return &Scheduler{deps: deps, running: map[string]bool{}, lastFire: map[string]time.Time{}, lastRun: map[string]time.Time{}}
}

func fixedHours(h ...int) func(config.ScheduleCfg) []int {
	return func(config.ScheduleCfg) []int { return h }
}

func minutes(f func(c config.ScheduleCfg) int) func(config.ScheduleCfg) time.Duration {
	return func(c config.ScheduleCfg) time.Duration { return time.Duration(f(c)) * time.Minute }
}

func (s *Scheduler) defs() []taskDef {
	return []taskDef{
		{name: "checkin", desc: "每日签到（配置的整点逐账号执行）", hours: func(c config.ScheduleCfg) []int { return c.CheckinHours },
			run: runCheckin, gate: func(c config.ScheduleCfg) bool { return c.CheckinEnabled && len(c.CheckinHours) > 0 }},
		{name: "keepalive", desc: "token 保活（刷新全部账号 token）", hours: func(c config.ScheduleCfg) []int { return []int{c.KeepaliveHour} },
			run: runKeepalive, gate: func(c config.ScheduleCfg) bool { return c.KeepaliveEnabled }},
		{name: "balance", desc: "余额与积分包刷新", periodic: true, interval: minutes(func(c config.ScheduleCfg) int { return c.BalanceRefreshMinutes }),
			run: runBalance, gate: func(c config.ScheduleCfg) bool { return c.BalanceRefreshMinutes > 0 }},
		{name: "growth_claim", desc: "成长任务轮询（自动接受 + 达标自动领奖）", periodic: true, interval: minutes(func(c config.ScheduleCfg) int { return c.GrowthClaimMinutes }),
			run: runGrowthClaim, gate: func(c config.ScheduleCfg) bool { return c.GrowthClaimEnabled && c.GrowthClaimMinutes > 0 }},
		{name: "token_health", desc: "凭证健康检查（到期预警）", hours: func(c config.ScheduleCfg) []int { return []int{c.TokenHealthHour} },
			run: runTokenHealth, gate: func(c config.ScheduleCfg) bool { return c.TokenHealthEnabled }},
		{name: "waste", desc: "积分浪费预测告警", hours: fixedHours(9) /* 签到后 */, run: runWasteCheck, gate: func(c config.ScheduleCfg) bool { return true }},
		{name: "backup", desc: "自动备份凭证与状态", hours: func(c config.ScheduleCfg) []int { return []int{c.AutoBackupHour} },
			run: runBackup, gate: func(c config.ScheduleCfg) bool { return c.AutoBackupEnabled }},
		{name: "wake", desc: "定时唤醒（发最小请求，提前触发额度重置周期）", hours: func(c config.ScheduleCfg) []int { return c.WakeHours },
			run: runWake, gate: func(c config.ScheduleCfg) bool { return c.WakeEnabled && len(c.WakeHours) > 0 }},
	}
}

func validHours(hs []int) []int {
	out := make([]int, 0, len(hs))
	for _, h := range hs {
		if h >= 0 && h <= 23 {
			out = append(out, h)
		}
	}
	return out
}

// nextSlot 严格晚于 now 的最近整点（用于展示下次执行时间）。
func nextSlot(now time.Time, hours []int) *time.Time {
	var best *time.Time
	for _, h := range validHours(hours) {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.AddDate(0, 0, 1)
		}
		if best == nil || t.Before(*best) {
			tt := t
			best = &tt
		}
	}
	return best
}

// prevSlot 不晚于 now 的最近整点（今天没有则取昨天）。
func prevSlot(now time.Time, hours []int) *time.Time {
	var best *time.Time
	for _, h := range validHours(hours) {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if t.After(now) {
			t = t.AddDate(0, 0, -1)
		}
		if best == nil || t.After(*best) {
			tt := t
			best = &tt
		}
	}
	return best
}

// dailyGrace 整点任务错过触发点后仍允许补跑的窗口：签到当天内都补（启动晚于签到时点也能签上），
// 其他任务仅补跑 30 分钟内（避免重启时把过期的备份/唤醒/告警全部重放一遍）。
func dailyGrace(name string, now, slot time.Time) bool {
	if name == "checkin" {
		return slot.Year() == now.Year() && slot.YearDay() == now.YearDay()
	}
	return now.Sub(slot) < 30*time.Minute
}

// nextInterval 周期任务下次执行时刻：lastRun+interval；已过期则立即到期。
func nextInterval(now, lastRun time.Time, iv time.Duration) *time.Time {
	if iv <= 0 {
		return nil
	}
	t := lastRun.Add(iv)
	if !t.After(now) {
		t = now
	}
	return &t
}

// dueLocked 判断任务此刻是否应被调度触发（调用方持有 s.mu）。
func (s *Scheduler) dueLocked(d taskDef, now time.Time, c config.ScheduleCfg) bool {
	if d.periodic {
		next := nextInterval(now, s.lastRun[d.name], d.interval(c))
		return next != nil && !now.Before(*next)
	}
	slot := prevSlot(now, d.hours(c))
	if slot == nil {
		return false
	}
	return slot.After(s.lastFire[d.name]) && dailyGrace(d.name, now, *slot)
}

// List 任务列表（含下次执行时间）。
func (s *Scheduler) List() []TaskInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.deps.Cfg.Snapshot().Schedule
	now := time.Now()
	var out []TaskInfo
	for _, d := range s.defs() {
		ti := TaskInfo{Name: d.name, Desc: d.desc, Enabled: d.gate(c), Running: s.running[d.name]}
		if ti.Enabled {
			if d.periodic {
				ti.NextRun = nextInterval(now, s.lastRun[d.name], d.interval(c))
			} else if s.dueLocked(d, now, c) {
				ti.NextRun = &now
			} else {
				ti.NextRun = nextSlot(now, d.hours(c))
			}
		}
		out = append(out, ti)
	}
	return out
}

// History 最近执行记录（新→旧，上限 100）。
func (s *Scheduler) History() []TaskRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TaskRun, len(s.history))
	copy(out, s.history)
	return out
}

// Start 每分钟扫描一次，到点即跑（整点任务按触发点防重）。
func (s *Scheduler) Start(ctx context.Context) {
	go s.catchUpTokens()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			s.scan(now)
		}
	}
}

func (s *Scheduler) scan(now time.Time) {
	c := s.deps.Cfg.Snapshot().Schedule
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.defs() {
		if !d.gate(c) || s.running[d.name] || !s.dueLocked(d, now, c) {
			continue
		}
		if !d.periodic {
			s.lastFire[d.name] = now
		}
		s.startLocked(d, "schedule")
	}
}

// RunNow 手动触发（同任务在跑返回错误）。
func (s *Scheduler) RunNow(name string) error {
	for _, d := range s.defs() {
		if d.name == name {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.running[name] {
				return fmt.Errorf("任务 %s 正在执行", name)
			}
			s.startLocked(d, "manual")
			return nil
		}
	}
	return fmt.Errorf("未知任务 %s", name)
}

// startLocked 标记运行并异步执行（调用方持有 s.mu，保证检查与置位在同一临界区）。
func (s *Scheduler) startLocked(d taskDef, source string) {
	s.running[d.name] = true
	s.lastRun[d.name] = time.Now() // 手动触发同样重置周期计时
	go s.execute(d, source)
}

func (s *Scheduler) execute(d taskDef, source string) {
	start := time.Now()
	var tr TaskRun
	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[scheduler] 任务 %s panic: %v", d.name, r)
				tr = TaskRun{Start: start, Failed: 1, Detail: fmt.Sprintf("panic: %v", r)}
			}
		}()
		tr = d.run(s)
	}()
	if tr.Start.IsZero() {
		tr.Start = start
	}
	tr.Task, tr.Source = d.name, source
	tr.End = time.Now()
	s.mu.Lock()
	s.running[d.name] = false
	s.history = append([]TaskRun{tr}, s.history...)
	if len(s.history) > 100 {
		s.history = s.history[:100]
	}
	s.mu.Unlock()
	// 全灭告警：一个都没做成说明上游整体不可达
	if tr.OK+tr.Already == 0 && tr.Failed > 0 {
		s.deps.Notifier.Emit(notify.Event{
			Kind: "task_failed", Level: "warn",
			Title: fmt.Sprintf("定时任务「%s」全部失败", d.desc),
			Body:  fmt.Sprintf("成功 %d / 失败 %d：%s", tr.OK, tr.Failed, tr.Detail),
		})
	}
}

func runCheckin(s *Scheduler) TaskRun {
	tr := TaskRun{Start: time.Now()}
	for _, a := range s.deps.Pool.List() {
		if a.Status == "disabled" {
			tr.Skipped++
			continue
		}
		acc := s.deps.Pool.Get(a.UID)
		if acc == nil || acc.Auth == nil {
			tr.Skipped++
			continue
		}
		ensureToken(s, acc.Auth)
		err := s.deps.Client.DailyCheckin(acc.Auth)
		switch {
		case err == nil:
			tr.OK++
		case upstream.IsAlreadyCheckin(err):
			tr.Already++
		default:
			tr.Failed++
			tr.Detail += a.Name + ": " + err.Error() + "; "
		}
		// 签到后顺手刷新余额
		s.deps.Pool.HintNext(acc, "checkin")
		remain, packs, err := s.deps.Client.Balance(acc.Auth)
		s.deps.Pool.SetBalance(acc, remain, packs, err)
	}
	return tr
}

func runKeepalive(s *Scheduler) TaskRun {
	tr := TaskRun{Start: time.Now()}
	for _, a := range s.deps.Pool.List() {
		acc := s.deps.Pool.Get(a.UID)
		if acc == nil || acc.Auth == nil {
			tr.Skipped++
			continue
		}
		if err := s.deps.Client.RefreshToken(acc.Auth); err != nil {
			tr.Failed++
			tr.Detail += a.Name + ": " + err.Error() + "; "
			continue
		}
		s.deps.Pool.ReviveAuthFailed(a.UID)
		tr.OK++
	}
	return tr
}

// catchUpTokens 启动补跑：服务停机期间错过了 22 点保活，启动时把 2 天内到期的 token 先续上。
func (s *Scheduler) catchUpTokens() {
	time.Sleep(5 * time.Second) // 等账号池加载完成
	var n, failed int
	for _, a := range s.deps.Pool.List() {
		acc := s.deps.Pool.Get(a.UID)
		if acc == nil || acc.Auth == nil || !acc.Auth.NeedsRefresh(48*time.Hour) {
			continue
		}
		n++
		if err := s.deps.Client.RefreshToken(acc.Auth); err != nil {
			failed++
			log.Printf("[poolmate] 启动续期失败 %s: %v", a.Name, err)
			continue
		}
		s.deps.Pool.ReviveAuthFailed(a.UID)
	}
	if n > 0 {
		log.Printf("[poolmate] 启动续期：%d 个账号 token 即将到期，已刷新 %d 个", n, n-failed)
	}
}

func runBalance(s *Scheduler) TaskRun {
	tr := TaskRun{Start: time.Now()}
	for _, a := range s.deps.Pool.List() {
		acc := s.deps.Pool.Get(a.UID)
		if acc == nil || acc.Auth == nil {
			tr.Skipped++
			continue
		}
		ensureToken(s, acc.Auth)
		remain, packs, err := s.deps.Client.Balance(acc.Auth)
		s.deps.Pool.SetBalance(acc, remain, packs, err)
		if err != nil {
			tr.Failed++
			tr.Detail += a.Name + ": " + err.Error() + "; "
		} else {
			tr.OK++
			refreshMeta(s, acc, a.Name)
		}
	}
	// 低余额告警
	c := s.deps.Cfg.Snapshot().Notify
	if c.Enabled && c.MinBalance > 0 {
		for _, a := range s.deps.Pool.List() {
			if a.Status != "disabled" && a.BalanceAt.After(time.Now().Add(-24*time.Hour)) && a.Balance < c.MinBalance {
				s.deps.Notifier.Emit(notify.Event{
					Kind: "low_balance", Level: "warn", UID: a.UID,
					Title: fmt.Sprintf("账号「%s」余额不足", a.Name),
					Body:  fmt.Sprintf("当前余额 %d，低于阈值 %d", a.Balance, c.MinBalance),
				})
			}
		}
	}
	return tr
}

// runGrowthClaim 成长任务轮询：先批量接受未接受任务（接受后进度才开始累计），
// 再领取已达标未领取的奖励；领奖成功刷新余额并通知。进度本身只能由
// 账号在官方客户端的真实行为产生，本任务不做任何行为上报。
func runGrowthClaim(s *Scheduler) TaskRun {
	tr := TaskRun{Start: time.Now()}
	for _, a := range s.deps.Pool.List() {
		if a.Status == "disabled" {
			tr.Skipped++
			continue
		}
		acc := s.deps.Pool.Get(a.UID)
		if acc == nil || acc.Auth == nil {
			tr.Skipped++
			continue
		}
		ensureToken(s, acc.Auth)
		ar, err := s.deps.Growth.AcceptAll(acc.Auth)
		if err != nil {
			if errors.Is(err, growth.ErrBusy) {
				tr.Skipped++
				continue
			}
			tr.Failed++
			tr.Detail += a.Name + " 接受失败: " + err.Error() + "; "
			continue
		}
		if ar.Accepted > 0 {
			tr.Detail += fmt.Sprintf("%s 接受 %d 项; ", a.Name, ar.Accepted)
		}
		res, err := s.deps.Growth.ClaimAll(acc.Auth, "auto")
		if err != nil {
			if errors.Is(err, growth.ErrBusy) {
				tr.Skipped++
				continue
			}
			tr.Failed++
			tr.Detail += a.Name + " 领奖失败: " + err.Error() + "; "
			continue
		}
		for _, it := range res.Items {
			if it.Status == "failed" {
				tr.Detail += a.Name + "/" + it.TaskCode + " 领奖失败: " + it.Error + "; "
			}
		}
		if res.Failed > 0 {
			tr.Failed++
		}
		if res.Done == 0 {
			if res.Failed == 0 {
				tr.Already++ // 无可领奖励
			}
			continue
		}
		tr.OK++
		s.deps.Pool.HintNext(acc, "claim")
		remain, packs, berr := s.deps.Client.Balance(acc.Auth)
		s.deps.Pool.SetBalance(acc, remain, packs, berr)
		body := fmt.Sprintf("领取 %d 项，+%d 积分", res.Done, res.Credit)
		if res.Energy > 0 {
			body += fmt.Sprintf(" · +%d 能量", res.Energy)
		}
		s.deps.Notifier.Emit(notify.Event{
			Kind: "growth_claim", Level: "info", UID: a.UID,
			Title: fmt.Sprintf("账号「%s」自动领取成长任务奖励", a.Name),
			Body:  body,
		})
	}
	return tr
}

func runTokenHealth(s *Scheduler) TaskRun {
	tr := TaskRun{Start: time.Now()}
	c := s.deps.Cfg.Snapshot().Notify
	warnDays := float64(c.TokenExpiryDays)
	if warnDays <= 0 {
		warnDays = 7
	}
	for _, a := range s.deps.Pool.List() {
		acc := s.deps.Pool.Get(a.UID)
		if acc == nil || acc.Auth == nil {
			tr.Skipped++
			continue
		}
		// 实际探测一次刷新，验证 refresh token 有效性
		if err := s.deps.Client.RefreshToken(acc.Auth); err != nil {
			tr.Failed++
			s.deps.Notifier.Emit(notify.Event{
				Kind: "auth_failed", Level: "critical", UID: a.UID,
				Title: fmt.Sprintf("账号「%s」凭证刷新失败", a.Name),
				Body:  "refresh token 可能已失效，请重新授权：" + err.Error(),
			})
			continue
		}
		tr.OK++
		s.deps.Pool.ReviveAuthFailed(a.UID)
		// 刷新成功即说明凭证健康，过期时间会随保活不断后移，不再按剩余天数告警；
		// 只有上游签发的有效期异常短（刷新后仍不足 warnDays 与 1 天中的较小者）才提醒。
		_, days := acc.Auth.Expiry()
		if limit := min(warnDays, 1); days < limit {
			s.deps.Notifier.Emit(notify.Event{
				Kind: "token_expiring", Level: "warn", UID: a.UID,
				Title: fmt.Sprintf("账号「%s」token 刷新后有效期仅 %.1f 小时", a.Name, days*24),
				Body:  "上游签发的有效期异常短，保活可能来不及续期，建议检查账号状态或重新授权",
			})
		}
	}
	return tr
}

func runWasteCheck(s *Scheduler) TaskRun {
	tr := TaskRun{Start: time.Now()}
	c := s.deps.Cfg.Snapshot().Notify
	if !c.Enabled || c.WasteDays <= 0 {
		tr.Skipped = -1
		return tr
	}
	window := time.Duration(c.WasteDays) * 24 * time.Hour
	deadline := time.Now().Add(window)
	for _, a := range s.deps.Pool.List() {
		var expiring int64
		for _, pk := range a.Packages {
			if pk.HasCycle && pk.CycleEnd.Before(deadline) {
				expiring += pk.Remain
			}
		}
		if expiring <= 0 {
			continue
		}
		// 用当日已消耗估算日消耗速度，预测能否用完
		daily := a.DailyCredit
		if daily <= 0 {
			daily = 50 // 无观测时按保守速度
		}
		usable := int64(daily * float64(c.WasteDays))
		if expiring > usable {
			s.deps.Notifier.Emit(notify.Event{
				Kind: "waste_forecast", Level: "warn", UID: a.UID,
				Title: fmt.Sprintf("账号「%s」预计浪费 %d 积分", a.Name, expiring-usable),
				Body: fmt.Sprintf("%d 天内到期 %d 积分，按当前消耗速度预计用不完；可加快消耗或调大到期优先权重",
					c.WasteDays, expiring),
			})
		}
	}
	tr.OK = 1
	return tr
}

func runBackup(s *Scheduler) TaskRun {
	tr := TaskRun{Start: time.Now()}
	c := s.deps.Cfg.Snapshot()
	target, err := backup.AutoBackup(s.deps.AuthDir, s.deps.StateFile,
		filepath.Join(c.DataDir, "backup"), c.Schedule.AutoBackupKeep)
	if err != nil {
		tr.Failed = 1
		tr.Detail = err.Error()
		return tr
	}
	tr.OK = 1
	tr.Detail = target
	return tr
}

// ensureToken 剩余不足 1 天就提前续期（各定时任务执行前调用；1 小时内刷过则跳过）。
func ensureToken(s *Scheduler, a *auth.Auth) {
	_ = s.deps.Client.EnsureFresh(a, 24*time.Hour)
}
