package panel

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// 用户名单下发 (真限流的执行入口)
// ============================================================================

// HandleSyncNodeUsers 把当前用户名单推到指定节点。
//
// 什么时候需要手动点:
//   - 刚纳管一台新机器, 想立刻把已有用户铺上去;
//   - 怀疑机器上的 clients[] 与面板不一致 (有人手工改过 conf);
//   - 排查"某个用户明明超额了却还能连"。
//
// 平时不需要: 配额翻转时 EnforceUserQuotas 会自动下发。
func HandleSyncNodeUsers(c *gin.Context) {
	id, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	var n Node
	if err := DB.First(&n, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "error": "NotFound", "message": "节点不存在"})
		return
	}
	if !GlobalHub.IsNodeOnline(n.ID) {
		c.JSON(http.StatusConflict, gin.H{"code": 409, "error": "Conflict", "message": "节点不在线, 无法下发"})
		return
	}

	res, err := PushUserSyncOne(n.ID)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"code": 502, "error": "BadGateway", "message": "下发失败: " + err.Error()})
		return
	}
	if res == nil {
		c.JSON(http.StatusBadGateway, gin.H{"code": 502, "error": "BadGateway", "message": "节点没有返回结果"})
		return
	}

	msg := "用户名单已同步"
	if !res.OK {
		msg = "同步未完全成功"
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": msg, "data": res})
}

// ============================================================================
// 公告
// ============================================================================

type announcementRequest struct {
	Title    string  `json:"title"`
	Body     string  `json:"body"`
	Level    *string `json:"level"`
	Pinned   *bool   `json:"pinned"`
	Enabled  *bool   `json:"enabled"`
	StartsAt *int64  `json:"starts_at"`
	EndsAt   *int64  `json:"ends_at"`
}

var validAnnounceLevels = map[string]bool{"info": true, "warn": true, "danger": true}

func HandleListAnnouncements(c *gin.Context) {
	var list []Announcement
	if err := DB.Order("pinned desc, id desc").Find(&list).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "读取公告失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": list})
}

func HandleCreateAnnouncement(c *gin.Context) {
	var req announcementRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "请求参数格式错误"})
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "请填写标题"})
		return
	}
	a := Announcement{Title: title, Body: strings.TrimSpace(req.Body), Level: "info", Enabled: true}
	if req.Level != nil {
		lv := strings.TrimSpace(*req.Level)
		if !validAnnounceLevels[lv] {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "级别只能是 info / warn / danger"})
			return
		}
		a.Level = lv
	}
	if req.Pinned != nil {
		a.Pinned = *req.Pinned
	}
	if req.Enabled != nil {
		a.Enabled = *req.Enabled
	}
	if req.StartsAt != nil {
		a.StartsAt = *req.StartsAt
	}
	if req.EndsAt != nil {
		a.EndsAt = *req.EndsAt
	}
	if err := DB.Create(&a).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "创建公告失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "公告已发布", "data": a})
}

func HandleUpdateAnnouncement(c *gin.Context) {
	id, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	var a Announcement
	if err := DB.First(&a, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "error": "NotFound", "message": "公告不存在"})
		return
	}
	var req announcementRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "请求参数格式错误"})
		return
	}

	updates := map[string]any{}
	if n := strings.TrimSpace(req.Title); n != "" {
		updates["title"] = n
	}
	if req.Body != "" {
		updates["body"] = strings.TrimSpace(req.Body)
	}
	if req.Level != nil {
		lv := strings.TrimSpace(*req.Level)
		if !validAnnounceLevels[lv] {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "级别只能是 info / warn / danger"})
			return
		}
		updates["level"] = lv
	}
	// pinned/enabled 用指针: 关掉公告 (false) 是常见操作, 值类型会被零值逻辑吞掉
	if req.Pinned != nil {
		updates["pinned"] = *req.Pinned
	}
	if req.Enabled != nil {
		updates["enabled"] = *req.Enabled
	}
	if req.StartsAt != nil {
		updates["starts_at"] = *req.StartsAt
	}
	if req.EndsAt != nil {
		updates["ends_at"] = *req.EndsAt
	}
	if len(updates) == 0 {
		c.JSON(http.StatusOK, gin.H{"code": 0, "message": "没有需要修改的字段", "data": a})
		return
	}
	if err := DB.Model(&Announcement{}).Where("id = ?", a.ID).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "更新公告失败"})
		return
	}
	DB.First(&a, a.ID)
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "公告已更新", "data": a})
}

func HandleDeleteAnnouncement(c *gin.Context) {
	id, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	if err := DB.Delete(&Announcement{}, id).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "删除公告失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "公告已删除"})
}

// ActiveAnnouncements 取当前对用户可见的公告 (供订阅门户使用)。
//
// 可见条件: enabled 且 (未设开始时间或已到) 且 (未设结束时间或未过)。
func ActiveAnnouncements() []Announcement {
	var list []Announcement
	now := time.Now().Unix()
	DB.Where("enabled = ?", true).
		Where("starts_at = 0 OR starts_at <= ?", now).
		Where("ends_at = 0 OR ends_at >= ?", now).
		Order("pinned desc, id desc").
		Limit(5).
		Find(&list)
	return list
}

// ============================================================================
// 订单
// ============================================================================

// HandleListOrders 订单列表。可按 user_id 过滤。
func HandleListOrders(c *gin.Context) {
	q := DB.Order("id desc").Limit(200)
	if uid := strings.TrimSpace(c.Query("user_id")); uid != "" {
		q = q.Where("user_id = ?", uid)
	}
	var list []Order
	if err := q.Find(&list).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "读取订单失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": list})
}

// RecordOrder 写一条订单记录。
//
// 面板不接支付, 订单纯粹是**审计日志**: 回答"这个号是谁什么时候给的、
// 到期推到哪天、加了多少流量"。手工开号时这个问题以前没人答得上来。
//
// 失败只记日志不阻断主流程 —— 开号成功比订单记录完整重要得多。
func RecordOrder(u *User, typ string, periodStart, periodEnd, trafficAdd int64, remark string) {
	if DB == nil || u == nil {
		return
	}
	o := Order{
		UserID:      u.ID,
		Username:    u.Username,
		PlanID:      u.PlanID,
		PlanName:    u.PlanName,
		Type:        typ,
		Status:      "paid",
		PaidAt:      time.Now().Unix(),
		PeriodStart: periodStart,
		PeriodEnd:   periodEnd,
		TrafficAdd:  trafficAdd,
		Remark:      remark,
	}
	if err := DB.Create(&o).Error; err != nil {
		// 不阻断: 订单只是审计, 开号本身已经成功
		return
	}
}
