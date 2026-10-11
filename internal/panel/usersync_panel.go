package panel

import (
	"log"
	"strings"
	"time"

	"v2panel/internal/agent"
	"v2panel/internal/common"

	"gorm.io/gorm"
)

// 用户名单下发 —— 「真限流」的执行端。
//
// 判定与执行分离:
//
//	EnforceUserQuotas  (service.go) —— 只看数据库, 决定谁该被锁
//	PushUserSyncAll    (这里)      —— 把「谁现在能用」的全量名单推到各节点
//
// agent 收到名单后重写 conf 的 clients[] 并重启内核。不在名单里的用户会被摘掉,
// 客户端立刻断线 —— 这才是真的停, 而不是订阅页上写一句"已熔断"却还能连。

// buildUserCredentials 生成某个节点应该有哪些用户。
//
// 过滤规则 (顺序重要):
//  1. 已删除的用户不出现 (GORM 软删, 默认查询已排除);
//  2. Locked 的用户不出现 —— 这就是熔断的执行方式;
//  3. 套餐限制了节点白名单的, 不在白名单的节点上不给这个用户;
//  4. 其他一律出现。
//
// 返回的 Enabled 恒为 true: 因为不可用的用户直接被**排除**在名单外,
// 而不是放进去再标记 disabled —— 名单是"允许列表", 少一个就是没权限。
// (协议里保留 Enabled 字段是为了将来支持"预置但不启用"的场景。)
func buildUserCredentials(nodeID uint) []common.UserCredential {
	var users []User
	if err := DB.Order("id asc").Find(&users).Error; err != nil {
		log.Printf("[Panel] 读取用户失败: %v", err)
		return nil
	}

	// 套餐节点白名单: 只在用户有套餐且套餐限了节点时才查。
	planCache := map[uint][]uint{}
	planAllowed := func(planID uint) []uint {
		if planID == 0 {
			return nil
		}
		if ids, ok := planCache[planID]; ok {
			return ids
		}
		var p Plan
		var ids []uint
		if err := DB.First(&p, planID).Error; err == nil {
			ids = p.PlanNodeIDs()
		}
		planCache[planID] = ids
		return ids
	}

	out := make([]common.UserCredential, 0, len(users))
	for i := range users {
		u := &users[i]
		if u.Locked {
			continue
		}
		if ids := planAllowed(u.PlanID); len(ids) > 0 {
			found := false
			for _, id := range ids {
				if id == nodeID {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		out = append(out, common.UserCredential{
			UserID:   u.ID,
			Email:    agent.UserEmailFor(u.ID),
			UUID:     u.UUID,
			Password: u.Password,
			Enabled:  true,
		})
	}
	return out
}

// PushUserSyncAll 把最新用户名单下发到所有在线节点。
//
// 异步执行: 每个节点的同步都会重启它的内核 (秒级), 串行等待会让 HTTP 请求
// 挂很久。而且这是"尽力而为"的操作 —— 某个节点离线不该影响其他节点。
//
// 防抖: 短时间内多次调用 (比如批量导入 100 个用户) 只跑最后一次,
// 否则会向同一节点连续下发 100 次同步、重启 100 次内核。
var userSyncDebounce = make(chan struct{}, 1)

func PushUserSyncAll() {
	select {
	case userSyncDebounce <- struct{}{}:
		go func() {
			// 合并 1.5 秒内的重复请求 —— 够合并批量操作, 又不至于让
			// 管理员改完配额后等太久才看到机器上的变化。
			time.Sleep(1500 * time.Millisecond)
			<-userSyncDebounce
			doPushUserSyncAll()
		}()
	default:
		// 已有一轮在排队, 直接返回 (那一轮会读到最新数据, 因为它是在
		// sleep 之后才查库的 —— 这点很重要, 否则会漏掉刚改的用户)
	}
}

func doPushUserSyncAll() {
	if DB == nil {
		return
	}
	var nodes []Node
	if err := DB.Find(&nodes).Error; err != nil {
		log.Printf("[Panel] 下发用户名单: 读取节点失败: %v", err)
		return
	}

	for i := range nodes {
		n := &nodes[i]
		if !GlobalHub.IsNodeOnline(n.ID) {
			continue
		}
		creds := buildUserCredentials(n.ID)
		// 每个节点单独开 goroutine: 一个节点重启内核要几秒, 串行会把
		// 多节点场景拖成分钟级。各节点互相独立, 没有共享状态。
		go syncUsersToNode(n.ID, n.Name, creds)
	}
}

// syncUsersToNode 向单个节点下发名单。失败只记日志 —— 下轮会重试。
func syncUsersToNode(nodeID uint, nodeName string, creds []common.UserCredential) {
	// 超时给足: agent 侧要改 conf + 重启 v2ray + 等 MainPID。
	res, err := GlobalHub.SendNodeOp(nodeID, &common.NodeOpPayload{
		Op:    common.NodeOpSyncUsers,
		Users: creds,
	}, 90*time.Second)
	if err != nil {
		log.Printf("[Panel] 节点 %s 下发用户名单失败: %v", nodeName, err)
		return
	}
	if res == nil {
		return
	}
	if !res.OK {
		log.Printf("[Panel] 节点 %s 同步用户未成功: %s", nodeName, res.Error)
		return
	}
	log.Printf("[Panel] 节点 %s 用户名单已同步: %d 个用户 (%s)", nodeName, len(creds), res.Output)
}

// PushUserSyncOne 只推一个节点 (面板上点「立即同步」时用)。
func PushUserSyncOne(nodeID uint) (*common.NodeOpResultPayload, error) {
	var n Node
	if err := DB.First(&n, nodeID).Error; err != nil {
		return nil, err
	}
	creds := buildUserCredentials(nodeID)
	return GlobalHub.SendNodeOp(nodeID, &common.NodeOpPayload{
		Op:    common.NodeOpSyncUsers,
		Users: creds,
	}, 90*time.Second)
}

// ============================================================================
// 流量周期重置
// ============================================================================

// ResetMode 常量 (与 Plan.ResetMode / User.ResetMode 同义)。
const (
	ResetModeNone    = "none"    // 不自动重置 (一次性流量包)
	ResetModeMonthly = "monthly" // 每月 1 号 0 点
	ResetModeCycle   = "cycle"   // 按到期日 (4 号到期 → 每月 4 号)
)

// ShouldReset 判断某个用户在现在这个时刻是否该重置流量。
//
// 三种模式的判据:
//
//	none     —— 永不自动重置。
//	monthly  —— 本月 1 号 0 点之后还没重置过。
//	cycle    —— 到达期日那天 (按 ExpireAt 的日号) 之后还没重置过。
//
// ⚠️ 用 LastResetAt 而不是"下次重置时间": 后者在改到期日/改套餐时会失效,
// 需要额外同步逻辑; 而"上次什么时候重置的"是既成事实, 判据永远是自洽的。
func ShouldReset(u *User, now time.Time) bool {
	mode := strings.TrimSpace(u.ResetMode)
	if mode == "" {
		mode = ResetModeMonthly // 历史数据兼容: 空值按最常见的处理
	}
	if mode == ResetModeNone {
		return false
	}
	if u.TrafficLimit <= 0 {
		// 不限量的账号没有"重置"的意义 (重置了也是 0/不限)。
		return false
	}

	last := time.Unix(u.LastResetAt, 0)
	if u.LastResetAt == 0 {
		// 从未重置过: 用创建时间作为基准, 避免"刚开的号立刻被重置"。
		last = u.CreatedAt
	}

	switch mode {
	case ResetModeCycle:
		if u.ExpireAt <= 0 {
			// 永久账号没有"到期日"可对齐, 退化成月初重置。
			return crossedMonthStart(last, now)
		}
		exp := time.Unix(u.ExpireAt, 0)
		day := exp.Day()
		// 本月的重置日 (处理 31 号在小月的情况: 落到该月最后一天)
		resetDay := clampDay(now.Year(), now.Month(), day)
		thisMonth := time.Date(now.Year(), now.Month(), resetDay, 0, 0, 0, 0, now.Location())
		if now.Before(thisMonth) {
			// 本月重置日还没到 → 看上月那次
			prevMonth := now.AddDate(0, -1, 0)
			prevDay := clampDay(prevMonth.Year(), prevMonth.Month(), day)
			prev := time.Date(prevMonth.Year(), prevMonth.Month(), prevDay, 0, 0, 0, 0, now.Location())
			return last.Before(prev)
		}
		return last.Before(thisMonth)

	default: // monthly
		return crossedMonthStart(last, now)
	}
}

// crossedMonthStart 判断 last 与 now 之间是否跨过了某个"月初 0 点"。
func crossedMonthStart(last, now time.Time) bool {
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	if now.Before(start) {
		// 理论上不会发生 (start 就是本月起点), 保险处理
		start = start.AddDate(0, -1, 0)
	}
	return last.Before(start)
}

// clampDay 把"日号"夹到该月合法范围 (2 月没有 31 号 → 用 28/29)。
func clampDay(year int, month time.Month, day int) int {
	lastDay := time.Date(year, month+1, 0, 0, 0, 0, 0, time.Local).Day()
	if day > lastDay {
		return lastDay
	}
	if day < 1 {
		return 1
	}
	return day
}

// ResetDueUsers 执行到期的流量重置。返回重置了几个账号。
//
// 重置动作: UsedTraffic 归零 + LastResetAt 更新。
// **不动 Locked**: 重置后用户可能恢复, 但那由 EnforceUserQuotas 判定 ——
// 这里只负责"流量清零"这一件事, 保持单一职责。
func ResetDueUsers(now time.Time) int {
	if DB == nil {
		return 0
	}
	var users []User
	if err := DB.Find(&users).Error; err != nil {
		log.Printf("[Panel] 流量重置: 读取用户失败: %v", err)
		return 0
	}

	n := 0
	for i := range users {
		u := &users[i]
		if !ShouldReset(u, now) {
			continue
		}
		before := u.UsedTraffic
		if err := DB.Model(&User{}).Where("id = ?", u.ID).Updates(map[string]any{
			"used_traffic":  0,
			"last_reset_at": now.Unix(),
		}).Error; err != nil {
			log.Printf("[Panel] 用户 %s 流量重置失败: %v", u.Username, err)
			continue
		}
		n++
		log.Printf("[Panel] 用户 %s 流量已重置 (原用量 %s, 模式 %s)",
			u.Username, HumanBytes(before), u.ResetMode)
	}

	if n > 0 {
		// 重置会让一部分熔断用户恢复 → 重新判定并下发。
		EnforceUserQuotas()
	}
	return n
}

// StartUserSyncScheduler 启动周期性任务:
//   - 每小时检查一次流量重置 (月/周期)
//   - 每 10 分钟重新判定配额 (兜住"agent 没上报但已经到期"的情况)
//
// 每小时而不是每分钟: 重置是"月初 0 点"这种粗粒度事件, 早一分钟晚一分钟
// 对用户没有区别, 而每分钟扫全表在用户多时是白费。
func StartUserSyncScheduler() {
	go func() {
		resetTicker := time.NewTicker(time.Hour)
		quotaTicker := time.NewTicker(10 * time.Minute)
		defer resetTicker.Stop()
		defer quotaTicker.Stop()

		// 启动时先跑一次 —— 面板可能刚重启, 错过了月初那一刻。
		// 用 LastResetAt 判定, 所以补跑是安全且幂等的。
		time.Sleep(20 * time.Second) // 等 hub 把节点连上
		if n := ResetDueUsers(time.Now()); n > 0 {
			log.Printf("[Panel] 启动补跑: 重置了 %d 个账号的流量", n)
		}

		for {
			select {
			case <-resetTicker.C:
				if n := ResetDueUsers(time.Now()); n > 0 {
					log.Printf("[Panel] 周期重置: %d 个账号", n)
				}
			case <-quotaTicker.C:
				// 到期判定不依赖流量上报, 所以要定期跑。
				// 同时兜住"用户改了配额但没触发入账"的情况。
				EnforceUserQuotas()
			}
		}
	}()
}

// 保证 gorm 被引用 (UpdateColumn 里用到 gorm.Expr)。
var _ = gorm.Expr
