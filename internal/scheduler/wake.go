package scheduler

import (
	"fmt"
	"time"

	"poolmate/internal/notify"
	"poolmate/internal/pool"
)

// refreshMeta 余额刷新成功后顺带采集付费类型与用量提醒；
// 提醒码非 0 且开启告警时推送通知（由通知器按 DedupMinutes 去重）。
func refreshMeta(s *Scheduler, acc *pool.Account, name string) {
	pay, perr := s.deps.Client.PaymentType(acc.Auth)
	dn, derr := s.deps.Client.DosageNotify(acc.Auth)
	if perr != nil && derr != nil {
		return
	}
	code, msg := 0, ""
	if derr == nil && dn != nil {
		code = dn.Code
		msg = dn.Zh
		if msg == "" {
			msg = dn.En
		}
	}
	if perr != nil {
		pay = ""
	}
	s.deps.Pool.SetMeta(acc, pay, code, msg)
	n := s.deps.Cfg.Snapshot().Notify
	if code != 0 && n.Enabled && n.DosageAlert {
		if msg == "" {
			msg = fmt.Sprintf("上游返回用量提醒码 %d", code)
		}
		s.deps.Notifier.Emit(notify.Event{
			Kind: "dosage_notify", Level: "warn", UID: acc.Auth.Snapshot().UID,
			Title: fmt.Sprintf("账号「%s」用量提醒", name),
			Body:  msg,
		})
	}
}

// runWake 定时唤醒：对每个可用账号发一次 max_tokens=1 的最小对话，
// 让额度重置周期尽早起算（避免等到真正使用时才开始计周期）。
func runWake(s *Scheduler) TaskRun {
	tr := TaskRun{Start: time.Now()}
	model := s.deps.Cfg.Snapshot().Schedule.WakeModel
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
		used, err := s.deps.Client.WakeUp(acc.Auth, model)
		if err != nil {
			tr.Failed++
			tr.Detail += a.Name + "(" + used + "): " + err.Error() + "; "
			continue
		}
		tr.OK++
		s.deps.Pool.MarkWoke(acc)
		remain, packs, berr := s.deps.Client.Balance(acc.Auth)
		s.deps.Pool.SetBalance(acc, remain, packs, berr)
	}
	return tr
}
