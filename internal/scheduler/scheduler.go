// Package scheduler 定时任务：签到、token 保活、余额/积分包刷新、
// 凭证健康检查（到期预警）、积分浪费预测告警、自动备份。记录执行历史。
package scheduler

import (
	"context"
	"errors"
	"fmt"
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
	// due 返回下次执行时刻（每天整点类任务）或间隔（周期类任务）。
	next func(now time.Time, c config.ScheduleCfg) *time.Time
	run  func(s *Scheduler) TaskRun
	gate func(c config.ScheduleCfg) bool
}

// Scheduler 定时调度器。
type Scheduler struct {
	deps    Deps
	mu      sync.Mutex
	running map[string]bool
	history []TaskRun
	lastDay map[string]string    // 整点任务当日防重
	lastRun map[string]time.Time // 周期任务上次执行时刻
}

func New(deps Deps) *Scheduler {
	return &Scheduler{deps: deps, running: map[string]bool{}, lastDay: map[string]string{}, lastRun: map[string]time.Time{}}
}

func atHour(hour int) func(time.Time, config.ScheduleCfg) *time.Time {
	return func(now time.Time, _ config.ScheduleCfg) *time.Time {
		t := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		return &t
	}
}

func everyMinutes(n int) func(time.Time, config.ScheduleCfg) *time.Time {
	return func(now time.Time, _ config.ScheduleCfg) *time.Time {
		if n <= 0 {
			return nil
		}
		t := now.Add(time.Duration(n) * time.Minute)
		return &t
	}
}

func (s *Scheduler) defs() []taskDef {
	return []taskDef{
		{"checkin", "每日签到（配置的整点逐账号执行）", false, atHour(-1), runCheckin, func(c config.ScheduleCfg) bool { return c.CheckinEnabled }},
		{"keepalive", "token 保活（刷新全部账号 token）", false, atHour(-2), runKeepalive, func(c config.ScheduleCfg) bool { return c.KeepaliveEnabled }},
		{"balance", "余额与积分包刷新", true, everyMinutes(0), runBalance, func(c config.ScheduleCfg) bool { return c.BalanceRefreshMinutes > 0 }},
		{"growth_claim", "成长任务轮询（自动接受 + 达标自动领奖）", true, everyMinutes(0), runGrowthClaim, func(c config.ScheduleCfg) bool {
			return c.GrowthClaimEnabled && c.GrowthClaimMinutes > 0
		}},
		{"token_health", "凭证健康检查（到期预警）", false, atHour(-3), runTokenHealth, func(c config.ScheduleCfg) bool { return c.TokenHealthEnabled }},
		{"waste", "积分浪费预测告警", false, atHour(9) /* 签到后 */, runWasteCheck, func(c config.ScheduleCfg) bool { return true }},
		{"backup", "自动备份凭证与状态", false, atHour(-4), runBackup, func(c config.ScheduleCfg) bool { return c.AutoBackupEnabled }},
		{"wake", "定时唤醒（发最小请求，提前触发额度重置周期）", false, atHour(-5), runWake, func(c config.ScheduleCfg) bool { return c.WakeEnabled && len(c.WakeHours) > 0 }},
	}
}

// resolveNext 处理占位负小时（-1 签到首时点等）与周期任务的间隔推进。
// lastRun 仅周期任务使用：从未执行过时立即到期（启动即跑一轮）。
func (d taskDef) resolveNext(now time.Time, lastRun time.Time, c config.ScheduleCfg) *time.Time {
	switch d.name {
	case "checkin":
		var best *time.Time
		for _, h := range c.CheckinHours {
			t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
			if !t.After(now) {
				t = t.Add(24 * time.Hour)
			}
			if best == nil || t.Before(*best) {
				best = &t
			}
		}
		return best
	case "keepalive":
		return atHour(c.KeepaliveHour)(now, c)
	case "balance":
		return nextInterval(now, lastRun, time.Duration(c.BalanceRefreshMinutes)*time.Minute)
	case "growth_claim":
		return nextInterval(now, lastRun, time.Duration(c.GrowthClaimMinutes)*time.Minute)
	case "token_health":
		return atHour(c.TokenHealthHour)(now, c)
	case "backup":
		return atHour(c.AutoBackupHour)(now, c)
	case "wake":
		var best *time.Time
		for _, h := range c.WakeHours {
			t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
			if !t.After(now) {
				t = t.Add(24 * time.Hour)
			}
			if best == nil || t.Before(*best) {
				best = &t
			}
		}
		return best
	}
	return d.next(now, c)
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
			ti.NextRun = d.resolveNext(now, s.lastRun[d.name], c)
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

// Start 每分钟扫描一次，到点即跑（整点任务当日防重）。
func (s *Scheduler) Start(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			c := s.deps.Cfg.Snapshot().Schedule
			for _, d := range s.defs() {
				if !d.gate(c) || s.running[d.name] {
					continue
				}
				next := d.resolveNext(now, s.lastRun[d.name], c)
				if next == nil {
					continue
				}
				// 周期任务：到点即跑；整点任务：到点且当日未跑过
				dayKey := now.Format("2006-01-02")
				if now.After(*next) || now.Equal(*next) {
					if !d.periodic && s.lastDay[d.name] == dayKey {
						continue
					}
					s.lastDay[d.name] = dayKey
					s.lastRun[d.name] = now
					go s.execute(d, "schedule")
				}
			}
		}
	}
}

// RunNow 手动触发（同任务在跑返回错误）。
func (s *Scheduler) RunNow(name string) error {
	for _, d := range s.defs() {
		if d.name == name {
			s.mu.Lock()
			if s.running[name] {
				s.mu.Unlock()
				return fmt.Errorf("任务 %s 正在执行", name)
			}
			s.mu.Unlock()
			go s.execute(d, "manual")
			return nil
		}
	}
	return fmt.Errorf("未知任务 %s", name)
}

func (s *Scheduler) execute(d taskDef, source string) {
	s.mu.Lock()
	s.running[d.name] = true
	s.lastRun[d.name] = time.Now() // 手动触发同样重置周期计时
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running[d.name] = false
		s.mu.Unlock()
	}()
	tr := d.run(s)
	tr.Task, tr.Source = d.name, source
	tr.End = time.Now()
	s.mu.Lock()
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
		tr.OK++
	}
	return tr
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
		_, days := acc.Auth.Expiry()
		if days < warnDays {
			s.deps.Notifier.Emit(notify.Event{
				Kind: "token_expiring", Level: "warn", UID: a.UID,
				Title: fmt.Sprintf("账号「%s」凭证 %.1f 天后到期", a.Name, days),
				Body:  "建议在凭证到期前重新授权，避免服务中断",
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

// ensureToken 快到期先刷新。
func ensureToken(s *Scheduler, a *auth.Auth) {
	if a.NeedsRefresh(10 * time.Minute) {
		_ = s.deps.Client.RefreshToken(a)
	}
}
