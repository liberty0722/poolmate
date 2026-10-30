package pool

import (
	"testing"
	"time"

	"poolmate/internal/upstream"
)

// tracker 按真实流程驱动 trackChange：网关消耗时与 ReportSuccess 一样同步预扣本地余额，
// 刷新余额时与 SetBalance 一样先推算变动、再用上游余额覆盖本地余额。
type tracker struct {
	t     *testing.T
	a     *Account
	packs []upstream.Package
	now   time.Time
}

func newTracker(t *testing.T, balance int64) *tracker {
	now := time.Now()
	day := now.Format("2006-01-02")
	packs := []upstream.Package{{Name: "pack", Size: 10000, Remain: balance}}
	a := &Account{Balance: balance, BalanceAt: now.Add(-6 * time.Minute), Packages: packs,
		DailyDate: day, SyncDate: day, ChgDate: day}
	return &tracker{t: t, a: a, packs: packs, now: now}
}

// use 网关消耗 credit 积分（本地预扣）。
func (tr *tracker) use(credit int64) {
	tr.a.DailyCredit += float64(credit)
	tr.a.Balance -= credit
}

// sync 上游返回余额 remain。
func (tr *tracker) sync(remain int64) {
	tr.now = tr.now.Add(6 * time.Minute)
	tr.a.trackChange(remain, tr.packs, tr.now)
	tr.a.Balance, tr.a.BalanceAt = remain, tr.now
}

func (tr *tracker) expect(added, external int64, changes int) {
	tr.t.Helper()
	if tr.a.TodayAdded != added || tr.a.TodayExternal != external || len(tr.a.Changes) != changes {
		tr.t.Fatalf("期望 增加=%d 外部=%d 记录=%d，实际 增加=%d 外部=%d 记录=%d %v",
			added, external, changes, tr.a.TodayAdded, tr.a.TodayExternal, len(tr.a.Changes), tr.a.Changes)
	}
}

// 回归：上游扣费与网关记账一致时，不能因为重复计算网关消耗而凭空出现「增加」。
func TestTrackChangeNoDoubleCount(t *testing.T) {
	tr := newTracker(t, 10000)
	for i := 0; i < 10; i++ {
		tr.use(30)
		tr.sync(tr.a.Balance) // 上游同步扣掉同样的 30
	}
	tr.expect(0, 0, 0)
	if tr.a.Pending != 0 {
		t.Fatalf("pending 应为 0，实际 %d", tr.a.Pending)
	}
}

// 上游扣费滞后：先暂存，补扣时抵消，两边都不记录。
func TestTrackChangeLaggedDeduction(t *testing.T) {
	tr := newTracker(t, 500)
	tr.use(30)
	tr.sync(500) // 上游还没扣
	tr.expect(0, 0, 0)
	if tr.a.Pending != 30 {
		t.Fatalf("pending 应为 30，实际 %d", tr.a.Pending)
	}
	tr.use(10)
	tr.sync(460) // 上游一次补扣 30 + 10
	tr.expect(0, 0, 0)
	if tr.a.Pending != 0 {
		t.Fatalf("补扣后 pending 应清零，实际 %d", tr.a.Pending)
	}
}

// 上游先扣（记为外部消耗）、网关后记账：后到的差值冲回那笔外部消耗。
func TestTrackChangeSpendThenLag(t *testing.T) {
	tr := newTracker(t, 500)
	tr.sync(470) // 上游先扣 30，网关尚未记账
	tr.expect(0, 30, 1)
	tr.use(30)
	tr.sync(470)
	tr.expect(0, 0, 0)
}

// 真实外部消耗（客户端直连）仍应识别，且不被网关消耗干扰。
func TestTrackChangeRealExternalSpend(t *testing.T) {
	tr := newTracker(t, 500)
	tr.use(20)
	tr.sync(430) // 网关 20 + 客户端直连 50
	tr.expect(0, 50, 1)
	if tr.a.Changes[0].Kind != "spend" {
		t.Fatalf("应记为 spend，实际 %s", tr.a.Changes[0].Kind)
	}
}

// 余额接口短时回升、但没有任何入账证据：不记为「增加」。
func TestTrackChangeNoEvidenceNotAdd(t *testing.T) {
	tr := newTracker(t, 500)
	tr.sync(510)
	tr.expect(0, 0, 0)
}

// 有入账证据（领奖提示）时正常记为增加，并扣除同期网关消耗。
func TestTrackChangeRealAdd(t *testing.T) {
	tr := newTracker(t, 500)
	tr.use(20)
	tr.a.hint = "claim"
	tr.sync(530) // 500 − 20 + 50
	tr.expect(50, 0, 1)
	if tr.a.Changes[0].Kind != "claim" {
		t.Fatalf("应记为 claim，实际 %s", tr.a.Changes[0].Kind)
	}
}

// 新增积分包（如签到发的赠送包）即使没有提示也算入账证据。
func TestTrackChangeNewPackIsEvidence(t *testing.T) {
	tr := newTracker(t, 500)
	newPacks := append([]upstream.Package{}, tr.packs...)
	newPacks = append(newPacks, upstream.Package{Name: "gift", Size: 100, Remain: 100})
	tr.now = tr.now.Add(6 * time.Minute)
	tr.a.trackChange(600, newPacks, tr.now)
	tr.expect(100, 0, 1)
}

// 加载迁移：用 10/30 Liberty 的真实记录回放，误记的增加与随后的补扣一并清掉。
func TestSanitizeChanges(t *testing.T) {
	day := "2026-10-30"
	at := func(hm string) time.Time {
		tm, _ := time.Parse("2006-01-02 15:04 -0700", day+" "+hm+" +0800")
		return tm
	}
	bogus := "领奖 / 充值等；积分包总额度不变（额度回补到已有包，不新增总额度）"
	a := &Account{ChgDate: day, TodayAdded: 354, TodayExternal: 47, Pending: 19, Changes: []BalanceChange{
		{Time: at("12:04"), Kind: "checkin", Delta: 100, SizeDelta: 100, Note: "每日签到奖励；新增积分包「国内运营裂变包」+100"},
		{Time: at("12:15"), Kind: "add", Delta: 30, Note: bogus},
		{Time: at("12:21"), Kind: "add", Delta: 5, Note: bogus},
		{Time: at("12:27"), Kind: "add", Delta: 44, Note: bogus},
		{Time: at("12:33"), Kind: "spend", Delta: -23},
		{Time: at("14:03"), Kind: "add", Delta: 16, Note: bogus},
		{Time: at("14:09"), Kind: "add", Delta: 2, Note: bogus},
		{Time: at("14:15"), Kind: "spend", Delta: -5},
		{Time: at("14:39"), Kind: "add", Delta: 26, Note: bogus},
		{Time: at("14:45"), Kind: "add", Delta: 49, Note: bogus},
		{Time: at("14:51"), Kind: "add", Delta: 82, Note: bogus},
		{Time: at("14:54"), Kind: "spend", Delta: -19},
	}}
	a.sanitizeChanges()
	if a.TodayAdded != 100 || a.TodayExternal != 0 {
		t.Fatalf("今日应只剩签到 100、外部消耗 0，实际 增加=%d 外部=%d", a.TodayAdded, a.TodayExternal)
	}
	if len(a.Changes) != 1 || a.Changes[0].Kind != "checkin" {
		t.Fatalf("应只保留签到记录，实际 %v", a.Changes)
	}
	a.sanitizeChanges()
	if a.TodayAdded != 100 || len(a.Changes) != 1 {
		t.Fatal("清理不是幂等的")
	}
}

// 跨日：前一天暂存的滞后扣费在次日补扣时仍应抵消，不能记成外部消耗。
func TestTrackChangePendingAcrossMidnight(t *testing.T) {
	tr := newTracker(t, 500)
	tr.use(30)
	tr.sync(500) // 上游还没扣，暂存 30
	// 跳到次日
	tr.now = tr.now.Add(24 * time.Hour)
	next := tr.now.Format("2006-01-02")
	tr.a.DailyDate, tr.a.DailyCredit = next, 0
	tr.sync(470) // 次日上游补扣 30
	tr.expect(0, 0, 0)
	if tr.a.Pending != 0 {
		t.Fatalf("pending 应被补扣抵消，实际 %d", tr.a.Pending)
	}
}
