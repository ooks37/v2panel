package agent

import (
	"testing"

	"v2panel/internal/common"
)

// 这组测试锁的是「用户刚开始产生流量时那一段 0→N 有没有被记账」。
//
// 实测的 V2Ray 行为: `xapi stats -json` 对 value 为 0 的计数器**整条不输出**。
// 于是"一个还没跑过流量的用户"在 stats 里根本不存在, 而它跑出第一段流量后
// 才第一次出现。早期实现把所有"基线里没有的 email"都当成新 client 丢弃,
// 结果这一整段被静默吞掉 —— 表现为「用户跑了几百 MB, 面板显示 0 B」。
//
// 因为用户通常连续使用, 丢失只发生在从无到有的那一轮, 所以现象像是
// "偶尔少记一点", 极难定位。这里用假 runner 把那个序列固定下来。

// fakeUserStats 按预置序列返回 stats, 用来模拟 V2Ray 的"零值条目不存在"。
type fakeUserStats struct {
	// 每轮返回的计数器。空 map 表示这一轮 stats 里没有任何 user>>> 条目
	// (正是"用户还没跑流量"的真实形态)。
	rounds []map[string]common.UserStatsCounter
	n      int
}

func (f *fakeUserStats) next() []common.UserStatsCounter {
	if f.n >= len(f.rounds) {
		return nil
	}
	m := f.rounds[f.n]
	f.n++
	out := make([]common.UserStatsCounter, 0, len(m))
	for _, c := range m {
		out = append(out, c)
	}
	return out
}

// collectWith 跑一轮 Collect, 用注入的 stats 序列替代真实 runner。
//
// 直接构造 tracker 并手工驱动 prev/known, 避免依赖 Runner 的进程调用 ——
// 被测的是增量算法本身, 不是 xapi 命令。
func collectWith(t *UserTrafficTracker, cur []common.UserStatsCounter) []common.UserStatsCounter {
	next := make(map[string]common.UserStatsCounter, len(cur))
	for _, c := range cur {
		next[c.Email] = c
	}
	return t.apply(next)
}

func newTestTracker() *UserTrafficTracker {
	return &UserTrafficTracker{
		prev:  map[string]common.UserStatsCounter{},
		known: map[string]bool{},
	}
}

// TestUserTraffic_FirstBurstIsNotLost 是本文件的核心用例。
//
// 序列: 用户已在名单里, 但还没跑过流量 (stats 里没有它的条目)
//   → 跑出 116MB
// 期望: 那 116MB 必须被上报, 而不是当成"新 client"丢掉。
func TestUserTraffic_FirstBurstIsNotLost(t *testing.T) {
	tr := newTestTracker()
	tr.SetKnownUsers([]string{"u1@v2panel"})

	// 第一轮: 用户还没流量 → stats 里完全没有 user>>> 条目
	if got := collectWith(tr, nil); len(got) != 0 {
		t.Fatalf("首轮不该上报任何增量, 得到 %+v", got)
	}
	if tr.stale {
		t.Fatalf("首轮之后 stale 应为 false")
	}

	// 第二轮: 用户跑出 116167749 字节下行
	got := collectWith(tr, []common.UserStatsCounter{
		{Email: "u1@v2panel", Up: 2613, Down: 116167749},
	})
	if len(got) != 1 {
		t.Fatalf("用户的第一段流量被丢掉了 (这正是要修的 bug), 得到 %+v", got)
	}
	if got[0].Down != 116167749 || got[0].Up != 2613 {
		t.Fatalf("增量不对: %+v", got[0])
	}
}

// 不在名单里的 email (手工加的第三方 client) 的第一段仍然要丢弃 ——
// 它的累计值不是"本轮增量", 记进去会凭空多算。
func TestUserTraffic_UnknownEmailStillDropped(t *testing.T) {
	tr := newTestTracker()
	tr.SetKnownUsers([]string{"u1@v2panel"})
	collectWith(tr, nil)

	got := collectWith(tr, []common.UserStatsCounter{
		{Email: "someone-elses-client", Up: 10, Down: 999999},
	})
	if len(got) != 0 {
		t.Fatalf("非面板管理的 client 不该被记账, 得到 %+v", got)
	}
}

// 正常累加: 基线已建立后的增量要算准。
func TestUserTraffic_NormalIncrement(t *testing.T) {
	tr := newTestTracker()
	tr.SetKnownUsers([]string{"u1@v2panel"})

	collectWith(tr, []common.UserStatsCounter{{Email: "u1@v2panel", Up: 100, Down: 200}})
	got := collectWith(tr, []common.UserStatsCounter{{Email: "u1@v2panel", Up: 150, Down: 500}})
	if len(got) != 1 || got[0].Up != 50 || got[0].Down != 300 {
		t.Fatalf("增量算错: %+v", got)
	}
}

// 计数器重启归零 (v2ray 重启) 时不能算出负数增量。
func TestUserTraffic_CounterResetClampedToZero(t *testing.T) {
	tr := newTestTracker()
	tr.SetKnownUsers([]string{"u1@v2panel"})

	collectWith(tr, []common.UserStatsCounter{{Email: "u1@v2panel", Up: 5000, Down: 9000}})
	// 内核重启 → 计数归零后重新累积到很小的值
	got := collectWith(tr, []common.UserStatsCounter{{Email: "u1@v2panel", Up: 10, Down: 20}})
	for _, g := range got {
		if g.Up < 0 || g.Down < 0 {
			t.Fatalf("计数器归零后算出负数增量: %+v", g)
		}
	}
}

// 名单刷新后, 之前被丢弃的 email 若被纳入名单, 其首段流量应被承认。
func TestUserTraffic_KnownListRefresh(t *testing.T) {
	tr := newTestTracker()
	// 一开始名单是空的 (agent 刚起来, conf 里还没用户)
	collectWith(tr, nil)

	// 面板下发名单
	tr.SetKnownUsers([]string{"u2@v2panel"})

	got := collectWith(tr, []common.UserStatsCounter{
		{Email: "u2@v2panel", Up: 1, Down: 1048576},
	})
	if len(got) != 1 || got[0].Down != 1048576 {
		t.Fatalf("新纳入名单的用户首段流量应被记账, 得到 %+v", got)
	}
}
