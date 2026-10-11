package panel

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// 流量统计
//
// ⚠️ 数据来源与语义 (不要把这里的数字当成"客户端计费值"):
//
//	agent 每 60s 跑一次 `v2ray xapi stats -json`, 得到的是 **per-inbound 的累计计数器**
//	(inbound>>>TAG>>>traffic>>>uplink|downlink)。它:
//	  • 只有累计值, 没有历史  → 曲线靠面板定期落 TrafficSample 行, 再做差分
//	  • v2ray 重启会归零      → 差分为负时钳到 0 (否则会画出一个巨大的负峰)
//	  • 机器级共享一个 Caddy 公网口, 但计数器是按 conf (inbound tag) 分的
//	  • **永远不用 Total** (它含 outbound, 约等于真实流量的 2 倍)
//
//	所以面板展示的"流量"= Σ(采样增量)。这与 233boy 自己 `v2ray info` 里看到的
//	累计值在**总量**上一致, 但面板能给出曲线, 而 233boy 只能给一个当前值。
// ============================================================================

// resolveWindow 解析时间窗参数, 返回 (since, bucketSec)。
//
// 默认 24 小时 / 5 分钟一个点: 一天 288 个点, 折线图足够密且一次查询不会拉爆内存。
// hours 上限 720 (30 天) —— 再长就该导出而不是画图了。
func resolveWindow(c *gin.Context) (time.Time, int) {
	hours := 24
	if v := strings.TrimSpace(c.Query("hours")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			hours = n
		}
	}
	if hours > 720 {
		hours = 720
	}

	// bucket 不传就按窗口长度自适应, 保证点数落在 120..360 之间。
	bucket := 300
	if v := strings.TrimSpace(c.Query("bucket")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 60 {
			bucket = n
		}
	} else {
		total := hours * 3600
		for _, b := range []int{60, 300, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400} {
			if total/b <= 360 {
				bucket = b
				break
			}
			bucket = b
		}
	}
	return time.Now().Add(-time.Duration(hours) * time.Hour), bucket
}

// trafficOverview 是流量页顶部卡片的聚合结果。
type trafficOverview struct {
	WindowHours int              `json:"window_hours"`
	BucketSec   int              `json:"bucket_sec"`
	Up          int64            `json:"up"`
	Down        int64            `json:"down"`
	Total       int64            `json:"total"`
	UpText      string           `json:"up_text"`
	DownText    string           `json:"down_text"`
	TotalText   string           `json:"total_text"`
	TodayUp     int64            `json:"today_up"`
	TodayDown   int64            `json:"today_down"`
	TodayText   string           `json:"today_text"`
	MonthText   string           `json:"month_text"`
	Series      []TrafficPoint   `json:"series"`
	ByNode      []NodeTrafficRow `json:"by_node"`
	SampleCount int64            `json:"sample_count"`
	FirstSample *time.Time       `json:"first_sample"`
	LastSample  *time.Time       `json:"last_sample"`
}

// HandleGetTrafficOverview 流量总览 (卡片 + 曲线 + 分节点)。
func HandleGetTrafficOverview(c *gin.Context) {
	since, bucket := resolveWindow(c)
	now := time.Now()

	up, down, err := SumTrafficBetween(since, now)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "统计流量失败: " + err.Error()})
		return
	}
	series, err := TrafficSeries(0, since, bucket)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "读取流量曲线失败: " + err.Error()})
		return
	}
	byNode, err := TrafficByNode(since)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "按节点统计失败: " + err.Error()})
		return
	}

	// 今日 = 本地时区当天 00:00 起。用本地时间而不是 UTC:
	// 用户看的是"今天用了多少", 按 UTC 会在东八区把早 8 点前的用量算进昨天。
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	todayUp, todayDown, _ := SumTrafficBetween(dayStart, now)

	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	monthUp, monthDown, _ := SumTrafficBetween(monthStart, now)

	ov := trafficOverview{
		WindowHours: durationHours(now.Sub(since)),
		BucketSec:   bucket,
		Up:          up,
		Down:        down,
		Total:       up + down,
		UpText:      HumanBytes(up),
		DownText:    HumanBytes(down),
		TotalText:   HumanBytes(up + down),
		TodayUp:     todayUp,
		TodayDown:   todayDown,
		TodayText:   HumanBytes(todayUp + todayDown),
		MonthText:   HumanBytes(monthUp + monthDown),
		Series:      series,
		ByNode:      byNode,
	}
	if ov.Series == nil {
		ov.Series = []TrafficPoint{}
	}
	if ov.ByNode == nil {
		ov.ByNode = []NodeTrafficRow{}
	}

	// 采样范围: 让前端能明确区分"真的没流量"和"面板还没采到数据"。
	type span struct {
		Cnt   int64
		First *time.Time
		Last  *time.Time
	}
	var sp span
	DB.Model(&TrafficSample{}).Count(&sp.Cnt)
	if sp.Cnt > 0 {
		var first, last TrafficSample
		if err := DB.Order("sampled_at asc").First(&first).Error; err == nil {
			sp.First = &first.SampledAt
		}
		if err := DB.Order("sampled_at desc").First(&last).Error; err == nil {
			sp.Last = &last.SampledAt
		}
	}
	ov.SampleCount = sp.Cnt
	ov.FirstSample = sp.First
	ov.LastSample = sp.Last

	c.JSON(http.StatusOK, gin.H{"code": 0, "data": ov})
}

// durationHours 把时长转成整数小时 (向上取整), 用于回显窗口长度。
func durationHours(d time.Duration) int {
	h := int(d.Hours())
	if d > time.Duration(h)*time.Hour {
		h++
	}
	return h
}

// inboundTrafficRow 是"按节点实例"的流量排行。
//
// 这一层是 233boy 唯一真正能给出粒度的维度 (per-inbound tag),
// 也是排查"到底是哪个协议在跑流量"的唯一手段。
type inboundTrafficRow struct {
	NodeID    uint      `json:"node_id"`
	NodeName  string    `json:"node_name"`
	ConfName  string    `json:"conf_name"`
	Protocol  string    `json:"protocol"`
	Up        int64     `json:"up"`
	Down      int64     `json:"down"`
	Total     int64     `json:"total"`
	TotalText string    `json:"total_text"`
	LastAt    time.Time `json:"last_at"`
}

// HandleGetTrafficInbounds 按 (节点, 实例) 汇总窗口内流量。
func HandleGetTrafficInbounds(c *gin.Context) {
	since, _ := resolveWindow(c)

	type raw struct {
		NodeID   uint
		ConfName string
		Up       int64
		Down     int64
		LastAt   time.Time
	}
	var rows []raw
	if err := DB.Model(&TrafficSample{}).
		Select("node_id, conf_name, COALESCE(SUM(up),0) AS up, COALESCE(SUM(down),0) AS down, MAX(sampled_at) AS last_at").
		Where("sampled_at >= ?", since).
		Group("node_id, conf_name").
		Scan(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "统计失败: " + err.Error()})
		return
	}

	var nodes []Node
	DB.Find(&nodes)
	nameOf := make(map[uint]string, len(nodes))
	for _, n := range nodes {
		nameOf[n.ID] = n.Name
	}
	var inbounds []Inbound
	DB.Find(&inbounds)
	protoOf := make(map[string]string, len(inbounds))
	for _, ib := range inbounds {
		protoOf[keyOf(ib.NodeID, ib.ConfName)] = ib.Protocol
	}

	out := make([]inboundTrafficRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, inboundTrafficRow{
			NodeID:    r.NodeID,
			NodeName:  nameOf[r.NodeID],
			ConfName:  r.ConfName,
			Protocol:  protoOf[keyOf(r.NodeID, r.ConfName)],
			Up:        r.Up,
			Down:      r.Down,
			Total:     r.Up + r.Down,
			TotalText: HumanBytes(r.Up + r.Down),
			LastAt:    r.LastAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Total > out[j].Total })
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": out})
}

// keyOf 生成 (节点, conf) 复合键, 避免用 struct 做 map key 时的可比性顾虑。
func keyOf(nodeID uint, conf string) string {
	return itoa(int(nodeID)) + "\x00" + conf
}

// HandleGetTrafficSeries 只取曲线 (前端定时刷新用, 比 overview 轻)。
func HandleGetTrafficSeries(c *gin.Context) {
	since, bucket := resolveWindow(c)
	var nodeID uint
	if v := strings.TrimSpace(c.Query("node_id")); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			nodeID = uint(n)
		}
	}
	series, err := TrafficSeries(nodeID, since, bucket)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "读取曲线失败: " + err.Error()})
		return
	}
	if series == nil {
		series = []TrafficPoint{}
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"bucket_sec": bucket, "series": series}})
}

// HandlePurgeTrafficSamples 清理过期的采样行。
//
// 为什么需要: 每 60s × 每个 conf 一行, 10 个节点一年就是 ~500 万行,
// 在 82 这台 2 核 800MB 的机器上会让曲线查询从毫秒变成秒级。
// 只删采样, 不动账号已用流量 —— 那是账本, 删了就丢账。
func HandlePurgeTrafficSamples(c *gin.Context) {
	days := 90
	if v := strings.TrimSpace(c.Query("days")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 3650 {
			days = n
		}
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	res := DB.Where("sampled_at < ?", cutoff).Delete(&TrafficSample{})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "清理失败: " + res.Error.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "已清理 " + itoa(int(res.RowsAffected)) + " 条早于 " + cutoff.Format("2006-01-02") + " 的采样",
	})
}
