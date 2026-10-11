package panel

import (
	"fmt"
	"log"
	"sort"
	"time"

	"v2panel/internal/common"

	"gorm.io/gorm"
)

// ============================================================================
// Hub 回调实现: 流量入账 / 清单入库 / 能力入库
// ============================================================================

// RecordTraffic 把 agent 上报的 per-inbound **增量** 流量入账。
//
// ⚠️ 这个函数现在**只负责机器视角**: 落采样行 (曲线图/节点排行用)。
// 它**不再**把流量归到订阅账号上 —— 那是早期实现的近似做法 (整机增量摊给
// 某个账号), 多用户时完全失真。账号计量现在走 RecordUserTraffic (见下),
// 数据来源是 V2Ray 的 per-user 计数器, 精确到人。
//
// 保留这一层的理由: 节点排行、容量规划、以及"某个 conf 吃了多少"这类问题
// 只有机器视角能回答, 用户视角回答不了 (一个 conf 上挂着所有用户)。
//
// ⚠️ 隐藏节点仍然落采样 (面板可查), 但标记在 node.Hidden 上, 由查询侧过滤。
func RecordTraffic(nodeID uint, deltas []common.InboundDeltaTraffic) error {
	if DB == nil {
		return fmt.Errorf("数据库未初始化")
	}
	if len(deltas) == 0 {
		return nil
	}

	var node Node
	if err := DB.First(&node, nodeID).Error; err != nil {
		return fmt.Errorf("节点 %d 不存在: %w", nodeID, err)
	}
	rate := node.Rate
	if rate <= 0 {
		rate = 1.0
	}

	now := time.Now()
	samples := make([]TrafficSample, 0, len(deltas))

	for _, d := range deltas {
		if d.ConfName == "" {
			continue
		}
		// 负增量在 agent 侧已钳过, 这里再防一次: 计数器重启归零会算出负数。
		if d.Up < 0 {
			d.Up = 0
		}
		if d.Down < 0 {
			d.Down = 0
		}
		if d.Up == 0 && d.Down == 0 {
			continue
		}
		up := int64(float64(d.Up) * rate)
		down := int64(float64(d.Down) * rate)
		samples = append(samples, TrafficSample{
			NodeID:    nodeID,
			ConfName:  d.ConfName,
			Up:        up,
			Down:      down,
			SampledAt: now,
		})
	}
	if len(samples) == 0 {
		return nil
	}

	if err := DB.Create(&samples).Error; err != nil {
		return fmt.Errorf("写入流量采样失败: %w", err)
	}
	return nil
}

// RecordUserTraffic 把 agent 上报的 **per-user** 增量入账到订阅账号。
//
// 这是「真限流」的入账端。链路:
//
//	V2Ray user>>>EMAIL>>>traffic>>>uplink  (agent 读)
//	  → UserTrafficPayload.Entries         (WS)
//	  → 这里累加到 User.UsedTraffic
//	  → EnforceUserQuotas 检查超额, 超额者标 Locked
//	  → 下次 PushUserSync 时 agent 把它从 clients[] 摘掉, 客户端断线
//
// 倍率: 按**节点**的 Rate 乘 (与机器视角一致)。同一用户在多台机器上的流量
// 各自按所在机器倍率折算, 累加到同一个 UsedTraffic。
func RecordUserTraffic(nodeID uint, rep *common.UserTrafficPayload) error {
	if DB == nil {
		return fmt.Errorf("数据库未初始化")
	}
	if rep == nil || len(rep.Entries) == 0 {
		return nil
	}

	var node Node
	if err := DB.First(&node, nodeID).Error; err != nil {
		return fmt.Errorf("节点 %d 不存在: %w", nodeID, err)
	}
	rate := node.Rate
	if rate <= 0 {
		rate = 1.0
	}

	now := time.Now()
	samples := make([]UserTrafficSample, 0, len(rep.Entries))
	// 先按用户聚合本次增量, 再一次性 UPDATE —— 一个用户可能在多台机器上
	// 同时有流量, 逐条 UPDATE 会写很多次 (SQLite 单写句柄, 代价明显)。
	delta := map[uint]int64{}

	for _, e := range rep.Entries {
		if e.UserID == 0 {
			continue
		}
		if e.Up < 0 {
			e.Up = 0
		}
		if e.Down < 0 {
			e.Down = 0
		}
		if e.Up == 0 && e.Down == 0 {
			continue
		}
		up := int64(float64(e.Up) * rate)
		down := int64(float64(e.Down) * rate)
		delta[e.UserID] += up + down

		samples = append(samples, UserTrafficSample{
			UserID: e.UserID, NodeID: nodeID, ConfName: e.Email,
			Up: up, Down: down, SampledAt: now,
		})
	}

	if len(samples) == 0 {
		return nil
	}
	if err := DB.Create(&samples).Error; err != nil {
		return fmt.Errorf("写入用户流量采样失败: %w", err)
	}

	for uid, add := range delta {
		if add <= 0 {
			continue
		}
		// 用 SQL 表达式自增而不是「读出来+加+写回」: 后者在并发上报下会丢更新
		// (两个节点同时上报同一用户时, 双方都读到旧值)。
		if err := DB.Model(&User{}).Where("id = ?", uid).
			UpdateColumn("used_traffic", gorm.Expr("used_traffic + ?", add)).Error; err != nil {
			log.Printf("[Panel] 用户 %d 流量入账失败: %v", uid, err)
		}
	}

	// 入账后立刻检查是否有人超额 —— 超额的要在下一轮同步里被摘掉。
	// 放在这里而不是等定时任务: 用户跑满的那一刻就该断, 不该等到下一分钟。
	go EnforceUserQuotas()
	return nil
}

// EnforceUserQuotas 检查所有用户, 把超额/到期的标为 Locked, 恢复的取消 Locked。
//
// 返回本次发生状态变化的用户数。变化会触发一次用户同步下发。
//
// ⚠️ 只改 Locked 标记, 不直接动机器上的 conf —— 那是 PushUserSync 的职责。
// 分开的理由: 入账是高频路径 (每 30s), 而下发要重启内核 (重); 把"判定"和
// "执行"分开, 才能只在真的有人状态翻转时才重启内核。
func EnforceUserQuotas() int {
	if DB == nil {
		return 0
	}
	var users []User
	if err := DB.Find(&users).Error; err != nil {
		log.Printf("[Panel] 读取用户失败: %v", err)
		return 0
	}

	changed := 0
	now := time.Now().Unix()
	for i := range users {
		u := &users[i]
		expired, overQuota := subLimits(u)
		shouldLock := expired || overQuota

		if shouldLock == u.Locked {
			continue // 状态没变, 不写库也不下发
		}

		updates := map[string]any{"locked": shouldLock}
		if shouldLock {
			updates["locked_at"] = now
		} else {
			updates["locked_at"] = int64(0)
		}
		if err := DB.Model(&User{}).Where("id = ?", u.ID).Updates(updates).Error; err != nil {
			log.Printf("[Panel] 更新用户 %d 锁定状态失败: %v", u.ID, err)
			continue
		}
		changed++
		reason := "恢复"
		if shouldLock {
			if overQuota {
				reason = "流量超额"
			} else {
				reason = "已到期"
			}
		}
		log.Printf("[Panel] 用户 %s %s (已用 %s / 配额 %s)",
			u.Username, reason, HumanBytes(u.UsedTraffic), humanLimit(u.TrafficLimit))
	}

	if changed > 0 {
		// 有人状态翻转 → 需要把新名单下发到所有在线节点。
		PushUserSyncAll()
	}
	return changed
}

// HandleInventory 处理整机清单上报 (落 Inbound 表)。
func HandleInventory(nodeID uint, rep *common.InventoryReportPayload) error {
	return UpsertInventory(nodeID, rep)
}

// HandleCapability 处理能力上报 (协议表 / 内核版本 / reality 可用性)。
func HandleCapability(nodeID uint, rep *common.CapabilityReportPayload) error {
	return SaveCapability(nodeID, rep)
}

// ============================================================================
// 后台巡检
// ============================================================================

// StartCollector 启动后台巡检:
//   - 每 30s 把心跳超时的节点标记为离线 (面板不会一直显示"在线")
//   - 每 3min 向在线节点要一次清单 (机器上被手工 233boy add 的 conf 也能被面板发现)
//   - 每 60s 重新判定一次配额/到期 (兜住"没有流量入账但已经到期"的情况)
func StartCollector() {
	go func() {
		offlineTick := time.NewTicker(30 * time.Second)
		inventoryTick := time.NewTicker(3 * time.Minute)
		quotaTick := time.NewTicker(60 * time.Second)
		defer func() {
			offlineTick.Stop()
			inventoryTick.Stop()
			quotaTick.Stop()
		}()

		// 启动时立刻拉一次: 面板重启期间机器上的改动需要马上对齐。
		time.Sleep(2 * time.Second)
		GlobalHub.BroadcastInventoryReq()

		for {
			select {
			case <-offlineTick.C:
				GlobalHub.SyncOfflineNodes()
			case <-inventoryTick.C:
				GlobalHub.BroadcastInventoryReq()
			case <-quotaTick.C:
				// ⚠️ 这里以前调 RunQuotaCheck (改 User.Status), 那与 EnforceUserQuotas
				// 是两套互相干扰的机制 —— 见 RunQuotaCheck 上的注释。限流的唯一判据
				// 与唯一动作都在 EnforceUserQuotas。
				EnforceUserQuotas()
			}
		}
	}()
}

// RunQuotaCheck 是**已废弃**的旧配额巡检, 保留仅为兼容调用点, 恒返回 0。
//
// ⚠️ 不要重新启用它。它做的事 (按超额把 User.Status 改成 "suspended",
// 按到期改成 "expired") 与 EnforceUserQuotas 是**两套互相干扰的机制**:
//
//	EnforceUserQuotas 写 Locked  —— "机器上还在不在", 是限流的事实
//	RunQuotaCheck     写 Status  —— 管理员意图, 但被 subLimits 当成"已到期"
//
// 一个用户纯粹因为**流量超额**被 RunQuotaCheck 置成 suspended 之后,
// subLimits 会同时报 expired=true (因为 Status != active), 于是:
//   - 面板的 lock_why 显示成"已到期"而不是"已超额", 误导管理员;
//   - HandleResetUserTraffic 里的"恢复 suspended"逻辑与它纠缠在一起。
//
// 限流的唯一判据是 subLimits (看 ExpireAt / TrafficLimit / UsedTraffic),
// 唯一动作是 EnforceUserQuotas。这里只留一个恒空的壳子, 免得将来有人
// 看到函数名以为是它在做配额检查。
func RunQuotaCheck() int {
	return 0
}

func humanLimit(v int64) string {
	if v <= 0 {
		return "不限"
	}
	return HumanBytes(v)
}

// ============================================================================
// 通用小工具
// ============================================================================

// HumanBytes 1024 进制的人类可读体积。
// 注意: 233boy 的 stats 输出标签写的是 KB/MB/GB 但实际按 1024 换算, 面板统一按 1024 显示, 保持一致。
func HumanBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "KMGTP"[exp])
}

// SumTrafficBetween 统计某时间窗内所有节点的流量合计。
func SumTrafficBetween(since, until time.Time) (up, down int64, err error) {
	type row struct {
		Up   int64
		Down int64
	}
	var r row
	err = DB.Model(&TrafficSample{}).
		Select("COALESCE(SUM(up),0) AS up, COALESCE(SUM(down),0) AS down").
		Where("sampled_at >= ? AND sampled_at < ?", since, until).
		Scan(&r).Error
	return r.Up, r.Down, err
}

// NodeTrafficSeries 返回某节点在窗口内的按采样点聚合序列 (供折线图)。
type TrafficPoint struct {
	T    time.Time `json:"t"`
	Up   int64     `json:"up"`
	Down int64     `json:"down"`
}

// TrafficSeries 取时间窗内按 bucket 聚合的上下行序列。
// 用 SQL 侧的时间截断做聚合, 避免把海量采样行拉进内存 (面板是 2 核 800MB 的机器)。
func TrafficSeries(nodeID uint, since time.Time, bucketSec int) ([]TrafficPoint, error) {
	if bucketSec <= 0 {
		bucketSec = 300
	}
	type raw struct {
		Bucket int64 `gorm:"column:bucket"`
		Up     int64
		Down   int64
	}
	q := DB.Model(&TrafficSample{}).
		Select(fmt.Sprintf("(CAST(strftime('%%s', sampled_at) AS INTEGER) / %d) * %d AS bucket, COALESCE(SUM(up),0) AS up, COALESCE(SUM(down),0) AS down", bucketSec, bucketSec)).
		Where("sampled_at >= ?", since)
	if nodeID > 0 {
		q = q.Where("node_id = ?", nodeID)
	}
	var rows []raw
	if err := q.Group("bucket").Order("bucket asc").Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]TrafficPoint, 0, len(rows))
	for _, r := range rows {
		out = append(out, TrafficPoint{
			T:    time.Unix(r.Bucket, 0),
			Up:   r.Up,
			Down: r.Down,
		})
	}
	return out, nil
}

// PerNodeTraffic 汇总窗口内每台机器的流量 (顶部卡片用)。
type NodeTrafficRow struct {
	NodeID   uint   `json:"node_id"`
	NodeName string `json:"node_name"`
	Up       int64  `json:"up"`
	Down     int64  `json:"down"`
}

// TrafficByNode 按节点汇总时间窗内的流量。
func TrafficByNode(since time.Time) ([]NodeTrafficRow, error) {
	type raw struct {
		NodeID uint
		Up     int64
		Down   int64
	}
	var rows []raw
	if err := DB.Model(&TrafficSample{}).
		Select("node_id, COALESCE(SUM(up),0) AS up, COALESCE(SUM(down),0) AS down").
		Where("sampled_at >= ?", since).
		Group("node_id").Scan(&rows).Error; err != nil {
		return nil, err
	}
	var nodes []Node
	if err := DB.Find(&nodes).Error; err != nil {
		return nil, err
	}
	nameOf := make(map[uint]string, len(nodes))
	for _, n := range nodes {
		nameOf[n.ID] = n.Name
	}
	out := make([]NodeTrafficRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, NodeTrafficRow{
			NodeID:   r.NodeID,
			NodeName: nameOf[r.NodeID],
			Up:       r.Up,
			Down:     r.Down,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Up+out[i].Down > out[j].Up+out[j].Down
	})
	return out, nil
}
