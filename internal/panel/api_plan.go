package panel

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// 套餐 (Plan) 管理。
//
// 套餐是「开号模板」: 流量额度 / 有效期 / 设备数 / 节点白名单 / 重置方式。
// 开号时把套餐的值**快照**进用户, 之后用户与套餐各自演进 —— 见 model.Plan 注释。

type planRequest struct {
	Name string `json:"name"`
	Desc string `json:"desc"`
	Sort *int   `json:"sort"`

	TrafficLimit *int64  `json:"traffic_limit"`
	DurationDays *int    `json:"duration_days"`
	DeviceLimit  *int    `json:"device_limit"`
	ResetMode    *string `json:"reset_mode"`

	NodeIDs []uint `json:"node_ids"`

	PriceCents *int  `json:"price_cents"`
	ForSale    *bool `json:"for_sale"`
	MaxUsers   *int  `json:"max_users"`
}

// planView 是套餐的回显形态 —— 比 Plan 多几个派生字段。
type planView struct {
	Plan
	// UserCount 当前属于该套餐的用户数 (算 MaxUsers 是否已满)
	UserCount int64 `json:"user_count"`
	// NodeIDsParsed 把 Plan.NodeIDs 解成数组回给前端 (Plan 里那个字段是 json:"-")
	NodeIDsParsed []uint `json:"node_ids"`
	// TrafficText 人类可读的额度, 省得前端再算一遍
	TrafficText string `json:"traffic_text"`
}

func buildPlanView(p *Plan) planView {
	var cnt int64
	DB.Model(&User{}).Where("plan_id = ?", p.ID).Count(&cnt)
	return planView{
		Plan:          *p,
		UserCount:     cnt,
		NodeIDsParsed: p.PlanNodeIDs(),
		TrafficText:   humanLimit(p.TrafficLimit),
	}
}

// 允许的重置模式。白名单而不是"任意字符串" —— 拼错一个字母会让重置逻辑
// 静默走 default 分支, 用户以为买了月付却永远不重置。
var validResetModes = map[string]bool{
	ResetModeNone:    true,
	ResetModeMonthly: true,
	ResetModeCycle:   true,
}

func HandleListPlans(c *gin.Context) {
	var plans []Plan
	if err := DB.Order("sort asc, id asc").Find(&plans).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "读取套餐失败"})
		return
	}
	out := make([]planView, 0, len(plans))
	for i := range plans {
		out = append(out, buildPlanView(&plans[i]))
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": out})
}

func HandleGetPlan(c *gin.Context) {
	id, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	var p Plan
	if err := DB.First(&p, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "error": "NotFound", "message": "套餐不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": buildPlanView(&p)})
}

func HandleCreatePlan(c *gin.Context) {
	var req planRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "请求参数格式错误"})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "请填写套餐名称"})
		return
	}

	p := Plan{Name: name, Desc: strings.TrimSpace(req.Desc), ResetMode: ResetModeMonthly}
	if req.Sort != nil {
		p.Sort = *req.Sort
	}
	if req.TrafficLimit != nil {
		if *req.TrafficLimit < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "流量额度不能为负"})
			return
		}
		p.TrafficLimit = *req.TrafficLimit
	}
	if req.DurationDays != nil {
		if *req.DurationDays < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "有效期不能为负"})
			return
		}
		p.DurationDays = *req.DurationDays
	}
	if req.DeviceLimit != nil {
		p.DeviceLimit = *req.DeviceLimit
	}
	if req.ResetMode != nil {
		m := strings.TrimSpace(*req.ResetMode)
		if !validResetModes[m] {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "重置方式只能是 none / monthly / cycle"})
			return
		}
		p.ResetMode = m
	}
	if req.PriceCents != nil {
		p.PriceCents = *req.PriceCents
	}
	if req.ForSale != nil {
		p.ForSale = *req.ForSale
	}
	if req.MaxUsers != nil {
		p.MaxUsers = *req.MaxUsers
	}
	if err := p.SetPlanNodeIDs(req.NodeIDs); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "节点列表格式错误"})
		return
	}

	if err := DB.Create(&p).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "创建套餐失败: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "套餐已创建", "data": buildPlanView(&p)})
}

func HandleUpdatePlan(c *gin.Context) {
	id, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	var p Plan
	if err := DB.First(&p, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "error": "NotFound", "message": "套餐不存在"})
		return
	}

	var req planRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "请求参数格式错误"})
		return
	}

	// 用 map 更新: GORM 的 struct 更新会跳过零值, 于是
	// traffic_limit=0 (不限量) 这类"有意义的零值"会被静默丢弃。
	updates := map[string]any{}
	if n := strings.TrimSpace(req.Name); n != "" {
		updates["name"] = n
	}
	if req.Desc != "" {
		updates["desc"] = strings.TrimSpace(req.Desc)
	}
	if req.Sort != nil {
		updates["sort"] = *req.Sort
	}
	if req.TrafficLimit != nil {
		if *req.TrafficLimit < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "流量额度不能为负"})
			return
		}
		updates["traffic_limit"] = *req.TrafficLimit
	}
	if req.DurationDays != nil {
		updates["duration_days"] = *req.DurationDays
	}
	if req.DeviceLimit != nil {
		updates["device_limit"] = *req.DeviceLimit
	}
	if req.ResetMode != nil {
		m := strings.TrimSpace(*req.ResetMode)
		if !validResetModes[m] {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "重置方式只能是 none / monthly / cycle"})
			return
		}
		updates["reset_mode"] = m
	}
	if req.PriceCents != nil {
		updates["price_cents"] = *req.PriceCents
	}
	if req.ForSale != nil {
		updates["for_sale"] = *req.ForSale
	}
	if req.MaxUsers != nil {
		updates["max_users"] = *req.MaxUsers
	}
	if req.NodeIDs != nil {
		np := Plan{}
		if err := np.SetPlanNodeIDs(req.NodeIDs); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "节点列表格式错误"})
			return
		}
		updates["node_ids"] = np.NodeIDs
	}

	if len(updates) == 0 {
		c.JSON(http.StatusOK, gin.H{"code": 0, "message": "没有需要修改的字段", "data": buildPlanView(&p)})
		return
	}
	if err := DB.Model(&Plan{}).Where("id = ?", p.ID).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "更新套餐失败"})
		return
	}
	DB.First(&p, p.ID)
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "套餐已更新", "data": buildPlanView(&p)})
}

// HandleDeletePlan 删除套餐。
//
// ⚠️ 只删套餐本身, **不动已开出去的用户** —— 用户的配额是开号时的快照,
// 删套餐不该让老用户突然失去额度 (那等于把人踢下线)。
// 用户会保留 PlanName 快照, 面板上仍能看出他原来是什么套餐。
func HandleDeletePlan(c *gin.Context) {
	id, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	var p Plan
	if err := DB.First(&p, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "error": "NotFound", "message": "套餐不存在"})
		return
	}

	var cnt int64
	DB.Model(&User{}).Where("plan_id = ?", p.ID).Count(&cnt)

	if err := DB.Delete(&Plan{}, p.ID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "删除套餐失败"})
		return
	}

	msg := "套餐已删除"
	if cnt > 0 {
		msg = "套餐已删除；" + strconv.FormatInt(cnt, 10) + " 个已有账号保留原配额不变"
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": msg})
}

// HandleApplyPlan 把套餐当前的值**同步**到该套餐下的所有用户。
//
// 这是唯一会覆盖已有用户配额的入口, 因此必须由管理员显式点击 ——
// 改套餐本身不会影响存量用户 (见 model.Plan 注释: 快照语义)。
//
// 参数:
//
//	apply_traffic  同步流量额度
//	apply_reset    同步重置方式
//	apply_devices  同步设备数
//	apply_expire   同步到期时间 (按新的 duration_days 从**今天**重算)
//
// 默认 (都不传) 只同步流量额度 —— 那是最常改的一项。
type applyPlanRequest struct {
	ApplyTraffic *bool `json:"apply_traffic"`
	ApplyReset   *bool `json:"apply_reset"`
	ApplyDevices *bool `json:"apply_devices"`
	ApplyExpire  *bool `json:"apply_expire"`
}

func HandleApplyPlan(c *gin.Context) {
	id, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	var p Plan
	if err := DB.First(&p, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "error": "NotFound", "message": "套餐不存在"})
		return
	}

	var req applyPlanRequest
	_ = c.ShouldBindJSON(&req)

	doTraffic := req.ApplyTraffic == nil || *req.ApplyTraffic // 默认同步流量
	doReset := req.ApplyReset != nil && *req.ApplyReset
	doDevices := req.ApplyDevices != nil && *req.ApplyDevices
	doExpire := req.ApplyExpire != nil && *req.ApplyExpire

	var users []User
	if err := DB.Where("plan_id = ?", p.ID).Find(&users).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "读取用户失败"})
		return
	}
	if len(users) == 0 {
		c.JSON(http.StatusOK, gin.H{"code": 0, "message": "该套餐下没有用户"})
		return
	}

	now := time.Now().Unix()
	n := 0
	for i := range users {
		u := &users[i]
		updates := map[string]any{"plan_name": p.Name}
		if doTraffic {
			updates["traffic_limit"] = p.TrafficLimit
		}
		if doReset {
			updates["reset_mode"] = p.ResetMode
		}
		if doDevices {
			updates["device_limit"] = p.DeviceLimit
		}
		if doExpire && p.DurationDays > 0 {
			updates["expire_at"] = now + int64(p.DurationDays)*86400
		}
		if err := DB.Model(&User{}).Where("id = ?", u.ID).Updates(updates).Error; err != nil {
			continue
		}
		n++
	}

	// 配额可能被调小 → 有人立刻超额 → 需要重新判定并下发。
	EnforceUserQuotas()

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "已同步到 " + strconv.Itoa(n) + " 个账号",
		"data":    buildPlanView(&p),
	})
}
