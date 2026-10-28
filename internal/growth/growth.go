// Package growth 成长任务服务：封装上游 列表/接受/领奖 接口，提供
// 短缓存（领奖/接受后失效）、同账号互斥、领奖队列（重试+明细）与当日领取统计。
// 面板与调度器共用，保证行为一致。仅调用官方接口，不做任何行为上报。
package growth

import (
	"errors"
	"sync"
	"time"

	"poolmate/internal/auth"
	"poolmate/internal/upstream"
)

const cacheTTL = 45 * time.Second

// ErrBusy 同一账号已有领奖/接受操作在执行。
var ErrBusy = errors.New("该账号正在执行任务操作，请稍后")

// 任务状态分类（与前端一致）。
const (
	StClaimable  = "claimable"
	StAcceptable = "acceptable"
	StDoing      = "doing"
	StLocked     = "locked"
	StDone       = "done"
)

// State 任务所处状态。
func State(t upstream.GrowthTask) string {
	switch {
	case t.AcceptStatus == "claimed":
		return StDone
	case t.Locked:
		return StLocked
	case t.Target > 0 && t.Current >= t.Target:
		return StClaimable
	case t.AcceptStatus == "accepted":
		return StDoing
	}
	return StAcceptable
}

// ClaimItem 单项领奖结果。
type ClaimItem struct {
	TaskCode string `json:"task_code"`
	Title    string `json:"title"`
	Status   string `json:"status"` // claimed / already / failed
	Credit   int64  `json:"credit"`
	Energy   int64  `json:"energy"`
	Error    string `json:"error,omitempty"`
}

// ClaimResult 一键领奖汇总。
type ClaimResult struct {
	Items  []ClaimItem `json:"results"`
	Done   int         `json:"done"`
	Failed int         `json:"failed"`
	Credit int64       `json:"credit"`
	Energy int64       `json:"energy"`
}

// AcceptResult 批量接受汇总。
type AcceptResult struct {
	Accepted int               `json:"done"`
	Skipped  int               `json:"skipped"`
	Statuses map[string]string `json:"statuses,omitempty"` // task_code → 上游返回状态
}

// Stat 账号当日领取统计（内存，跨天自动清零；Total 为进程运行期累计）。
type Stat struct {
	Day         string `json:"day"`
	TodayCredit int64  `json:"today_credit"`
	TodayEnergy int64  `json:"today_energy"`
	TodayCount  int    `json:"today_count"`
	AutoCredit  int64  `json:"auto_credit"` // 其中由定时轮询自动领取的积分
	TotalCredit int64  `json:"total_credit"`
}

// Summary 账号任务汇总（总览用）。
type Summary struct {
	Claimable       int   `json:"claimable"`
	ClaimableCredit int64 `json:"claimable_credit"`
	Acceptable      int   `json:"acceptable"`
	Doing           int   `json:"doing"`
	Locked          int   `json:"locked"`
	Done            int   `json:"done"`
}

type cacheEntry struct {
	at    time.Time
	tasks []upstream.GrowthTask
}

// Service 成长任务服务。
type Service struct {
	client *upstream.Client
	mu     sync.Mutex
	cache  map[string]cacheEntry
	locks  map[string]*sync.Mutex
	stats  map[string]*Stat
}

func New(c *upstream.Client) *Service {
	return &Service{client: c, cache: map[string]cacheEntry{}, locks: map[string]*sync.Mutex{}, stats: map[string]*Stat{}}
}

func (s *Service) lockFor(uid string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.locks[uid]
	if l == nil {
		l = &sync.Mutex{}
		s.locks[uid] = l
	}
	return l
}

func (s *Service) invalidate(uid string) {
	s.mu.Lock()
	delete(s.cache, uid)
	s.mu.Unlock()
}

// Tasks 返回账号任务列表；force=false 时 45s 内走缓存。
func (s *Service) Tasks(a *auth.Auth, force bool) ([]upstream.GrowthTask, error) {
	if !force {
		s.mu.Lock()
		e, ok := s.cache[a.UID]
		s.mu.Unlock()
		if ok && time.Since(e.at) < cacheTTL {
			return append([]upstream.GrowthTask(nil), e.tasks...), nil
		}
	}
	tasks, err := s.client.GrowthTasks(a)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cache[a.UID] = cacheEntry{at: time.Now(), tasks: tasks}
	s.mu.Unlock()
	return append([]upstream.GrowthTask(nil), tasks...), nil
}

// Summarize 统计任务各状态数量。
func Summarize(tasks []upstream.GrowthTask) Summary {
	var sm Summary
	for _, t := range tasks {
		switch State(t) {
		case StClaimable:
			sm.Claimable++
			sm.ClaimableCredit += t.RewardCredit
		case StAcceptable:
			sm.Acceptable++
		case StDoing:
			sm.Doing++
		case StLocked:
			sm.Locked++
		case StDone:
			sm.Done++
		}
	}
	return sm
}

// Accept 接受单个任务。
func (s *Service) Accept(a *auth.Auth, code string) (map[string]string, error) {
	l := s.lockFor(a.UID)
	if !l.TryLock() {
		return nil, ErrBusy
	}
	defer l.Unlock()
	defer s.invalidate(a.UID)
	return s.client.AcceptGrowthTasks(a, []string{code})
}

// AcceptAll 接受全部可接受（未接受且未锁定）的任务。
func (s *Service) AcceptAll(a *auth.Auth) (AcceptResult, error) {
	l := s.lockFor(a.UID)
	if !l.TryLock() {
		return AcceptResult{}, ErrBusy
	}
	defer l.Unlock()
	defer s.invalidate(a.UID)
	tasks, err := s.Tasks(a, true)
	if err != nil {
		return AcceptResult{}, err
	}
	var codes []string
	for _, t := range tasks {
		if State(t) == StAcceptable {
			codes = append(codes, t.TaskCode)
		}
	}
	res := AcceptResult{Statuses: map[string]string{}}
	if len(codes) == 0 {
		return res, nil
	}
	st, err := s.client.AcceptGrowthTasks(a, codes)
	if err != nil {
		return res, err
	}
	for code, v := range st {
		res.Statuses[code] = v
		if v == "accepted" {
			res.Accepted++
		} else {
			res.Skipped++
		}
	}
	return res, nil
}

// Claim 领取单个任务奖励。
func (s *Service) Claim(a *auth.Auth, code, source string) (credit, energy int64, already bool, err error) {
	l := s.lockFor(a.UID)
	if !l.TryLock() {
		return 0, 0, false, ErrBusy
	}
	defer l.Unlock()
	defer s.invalidate(a.UID)
	credit, energy, already, err = s.client.ClaimGrowthTask(a, code)
	if err == nil && !already {
		s.record(a.UID, credit, energy, 1, source)
	}
	return
}

// ClaimAll 队列式串行领取全部达标奖励：单项失败重试一次，项间 200ms 间隔。
// source: "manual" / "auto"（auto 计入自动领取统计）。
func (s *Service) ClaimAll(a *auth.Auth, source string) (ClaimResult, error) {
	l := s.lockFor(a.UID)
	if !l.TryLock() {
		return ClaimResult{}, ErrBusy
	}
	defer l.Unlock()
	defer s.invalidate(a.UID)
	tasks, err := s.Tasks(a, true)
	if err != nil {
		return ClaimResult{}, err
	}
	var res ClaimResult
	var count int
	for _, t := range tasks {
		if State(t) != StClaimable {
			continue
		}
		it := ClaimItem{TaskCode: t.TaskCode, Title: t.Title, Status: "claimed"}
		credit, energy, already, cerr := s.client.ClaimGrowthTask(a, t.TaskCode)
		if cerr != nil {
			time.Sleep(500 * time.Millisecond)
			credit, energy, already, cerr = s.client.ClaimGrowthTask(a, t.TaskCode)
		}
		switch {
		case cerr != nil:
			it.Status, it.Error = "failed", cerr.Error()
			res.Failed++
		case already:
			it.Status = "already"
		default:
			it.Credit, it.Energy = credit, energy
			res.Done++
			res.Credit += credit
			res.Energy += energy
			count++
		}
		res.Items = append(res.Items, it)
		time.Sleep(200 * time.Millisecond)
	}
	if count > 0 {
		s.record(a.UID, res.Credit, res.Energy, count, source)
	}
	return res, nil
}

func (s *Service) record(uid string, credit, energy int64, count int, source string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := time.Now().Format("2006-01-02")
	st := s.stats[uid]
	if st == nil {
		st = &Stat{}
		s.stats[uid] = st
	}
	if st.Day != day {
		st.Day, st.TodayCredit, st.TodayEnergy, st.TodayCount, st.AutoCredit = day, 0, 0, 0, 0
	}
	st.TodayCredit += credit
	st.TodayEnergy += energy
	st.TodayCount += count
	st.TotalCredit += credit
	if source == "auto" {
		st.AutoCredit += credit
	}
}

// Stats 返回全部账号的领取统计快照（跨天的条目视为今日 0）。
func (s *Service) Stats() map[string]Stat {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := time.Now().Format("2006-01-02")
	out := make(map[string]Stat, len(s.stats))
	for uid, st := range s.stats {
		c := *st
		if c.Day != day {
			c.TodayCredit, c.TodayEnergy, c.TodayCount, c.AutoCredit = 0, 0, 0, 0
		}
		out[uid] = c
	}
	return out
}
