package scheduler

import (
	"testing"
	"time"

	"poolmate/internal/config"
)

func findDef(s *Scheduler, name string) taskDef {
	for _, d := range s.defs() {
		if d.name == name {
			return d
		}
	}
	panic(name)
}

func TestDailyTaskFiresOncePerSlot(t *testing.T) {
	s := New(Deps{})
	c := config.ScheduleCfg{CheckinEnabled: true, CheckinHours: []int{8, 20}, AutoBackupEnabled: true, AutoBackupHour: 3}
	loc := time.Local
	ck := findDef(s, "checkin")

	before := time.Date(2026, 10, 30, 7, 59, 30, 0, loc)
	if s.dueLocked(ck, before, c) {
		t.Fatal("checkin should not be due before 08:00")
	}
	at := time.Date(2026, 10, 30, 8, 0, 20, 0, loc) // ticker 不对齐整分
	if !s.dueLocked(ck, at, c) {
		t.Fatal("checkin should be due right after 08:00")
	}
	s.lastFire[ck.name] = at
	if s.dueLocked(ck, at.Add(time.Minute), c) {
		t.Fatal("checkin must not re-fire within the same slot")
	}
	if !s.dueLocked(ck, time.Date(2026, 10, 30, 20, 0, 5, 0, loc), c) {
		t.Fatal("checkin should fire again at the 20:00 slot")
	}

	// 启动晚于签到时点：当天补签
	s2 := New(Deps{})
	if !s2.dueLocked(ck, time.Date(2026, 10, 30, 10, 30, 0, 0, loc), c) {
		t.Fatal("checkin should catch up on the same day")
	}
	// 次日 0 点后不补前一天 20 点的签到
	if s2.dueLocked(ck, time.Date(2026, 10, 31, 0, 30, 0, 0, loc), c) {
		t.Fatal("checkin must not catch up a previous-day slot")
	}

	// 其他整点任务仅 30 分钟内补跑
	bk := findDef(s2, "backup")
	if !s2.dueLocked(bk, time.Date(2026, 10, 30, 3, 10, 0, 0, loc), c) {
		t.Fatal("backup should be due shortly after its hour")
	}
	if s2.dueLocked(bk, time.Date(2026, 10, 30, 5, 0, 0, 0, loc), c) {
		t.Fatal("backup must not replay hours later")
	}
}

func TestPeriodicTaskDue(t *testing.T) {
	s := New(Deps{})
	c := config.ScheduleCfg{BalanceRefreshMinutes: 10}
	b := findDef(s, "balance")
	now := time.Now()
	if !s.dueLocked(b, now, c) {
		t.Fatal("periodic task should run immediately on first scan")
	}
	s.lastRun[b.name] = now
	if s.dueLocked(b, now.Add(5*time.Minute), c) {
		t.Fatal("not due before interval")
	}
	if !s.dueLocked(b, now.Add(10*time.Minute), c) {
		t.Fatal("due after interval")
	}
}
