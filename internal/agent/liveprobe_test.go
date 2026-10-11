package agent

import (
	"testing"
)

// 用**真实的** Runner 跑一次 stats 采集, 确认 agent 侧读得到计数器。
//
// 这是排障用的"活体"测试: 它依赖 82 上真有一个跑着的 v2ray 与 conf,
// 所以在没有环境的机器上会 skip。目的是把"面板显示 0"这件事定位到
// 具体是哪一层断的 (命令执行 / JSON 解析 / 增量算法 / WS 上报)。
//
//	go test ./internal/agent/ -run TestLiveStatsProbe -v
func TestLiveStatsProbe(t *testing.T) {
	d := NewDriver()
	r := d.runner()

	// ① 底层命令能不能跑通
	inbound, raw, err := r.Stats()
	if err != nil {
		t.Fatalf("Stats() 失败: %v\n原始输出:\n%s", err, raw.Stdout)
	}
	t.Logf("Stats() 返回 %d 个 inbound 计数器", len(inbound))
	for _, c := range inbound {
		t.Logf("  inbound %-50s up=%d down=%d", c.ConfName, c.Up, c.Down)
	}
	if len(inbound) == 0 {
		t.Errorf("inbound 计数器为空 —— 说明底层 xapi 命令或 JSON 解析有问题")
	}

	// ② per-user 解析
	users, err := r.UserStats()
	if err != nil {
		t.Fatalf("UserStats() 失败: %v", err)
	}
	t.Logf("UserStats() 返回 %d 个用户计数器", len(users))
	for _, u := range users {
		t.Logf("  user %-30s up=%d down=%d  (parseUserID=%d)",
			u.Email, u.Up, u.Down, parseUserIDFromEmail(u.Email))
	}

	// ③ 从 conf 反推的名单
	emails := d.managedEmailsFromConf()
	t.Logf("conf 里面板管理的 email: %v", emails)

	// ④ 增量算法: 模拟"基线为空 + 名单已知 + 现在有值"
	tr := NewUserTrafficTracker(r)
	tr.SetKnownUsers(emails)
	// 第一次 Collect 建基线 (会返回 nil)
	_ = tr.Collect()
	// 第二次: 如果期间有流量, 应当报出来
	got := tr.Collect()
	t.Logf("第二轮增量: %+v", got)
}
