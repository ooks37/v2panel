package panel

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// 订阅账号 CRUD
//
// ⚠️ 账号粒度说明 (不要被字段名误导):
//
//	233boy 的服务端 conf 里**只有一个** client UUID, 机器根本不知道"谁用了多少"。
//	所以这里的"用户"不是多租户用户, 而是**订阅账号**: 它承载
//	  UUID     —— 客户端鉴权凭据 (所有节点共用同一个, 这是 233boy 单用户模型的直接后果)
//	  SubToken —— 订阅链接的路径凭据
//	  配额/到期 —— 面板侧的唯一真实效果是「订阅接口停止下发链接」
//	面板无法"只掐掉某个账号的流量" (想做到必须换支持多用户的内核, 不在本项目范围)。
// ============================================================================

// userView 是返回给前端的账号视图。
//
// 单独定义一个结构而不是直接回 model.User: 面板需要额外附带**由订阅引擎算出来**的
// 展示字段 (订阅链接条数 / 是否已锁), 而且 Password 是否回显要能统一控制。
type userView struct {
	User

	LinkCount  int    `json:"link_count"` // 当前订阅会下发多少条链接
	Locked     bool   `json:"locked"`     // 到期或超额 (订阅接口已停止下发)
	LockWhy    string `json:"lock_why"`
	UsedText   string `json:"used_text"`
	LimitText  string `json:"limit_text"`
	ExpireText string `json:"expire_text"`

	// —— 套餐与重置 ——
	// ResetText  下一次重置的说明 (如「每月 1 号」/「每月 4 号」/「不重置」)
	// DeviceText 设备数展示
	ResetText  string `json:"reset_text"`
	DeviceText string `json:"device_text"`
}

func buildUserView(u *User, linkCount int) userView {
	expired, overQuota := subLimits(u)
	v := userView{
		User:      *u,
		LinkCount: linkCount,
		// Locked 以**数据库里的实际状态**为准: 那表示 agent 已经把该用户的
		// UUID 从机器上摘掉了, 客户端真的连不上。expired/overQuota 只是判据,
		// 判定还没落地时 (等下一轮同步) 两者会短暂不一致 —— 这时告诉管理员
		// 「还没断」比「已经断了」更诚实。
		Locked:     u.Locked,
		UsedText:   HumanBytes(u.UsedTraffic),
		LimitText:  humanLimit(u.TrafficLimit),
		ExpireText: "永久",
		ResetText:  resetModeText(u),
		DeviceText: deviceLimitText(u.DeviceLimit),
	}
	if u.ExpireAt > 0 {
		v.ExpireText = time.Unix(u.ExpireAt, 0).Format("2006-01-02 15:04")
	}
	switch {
	case u.Locked && overQuota:
		v.LockWhy = "已超出流量配额"
	case u.Locked && expired:
		if u.Status != "" && u.Status != "active" {
			v.LockWhy = "账号状态为 " + u.Status
		} else {
			v.LockWhy = "已过期"
		}
	case u.Locked:
		v.LockWhy = "已停用"
	case expired:
		v.LockWhy = "已到期（等待下发熔断）"
	case overQuota:
		v.LockWhy = "已超额（等待下发熔断）"
	}
	return v
}

// resetModeText 生成「下一次流量重置」的可读说明。
//
// 显示的是**下一次**而不是模式名: 用户关心的是"我什么时候能再用满额度",
// 而不是"monthly"这个词。
func resetModeText(u *User) string {
	mode := strings.TrimSpace(u.ResetMode)
	if mode == "" {
		mode = ResetModeMonthly
	}
	switch mode {
	case ResetModeNone:
		return "不重置（用完为止）"
	case ResetModeCycle:
		if u.ExpireAt > 0 {
			d := time.Unix(u.ExpireAt, 0).Day()
			return "每月 " + itoa(d) + " 号"
		}
		return "每月 1 号"
	default:
		return "每月 1 号"
	}
}

func deviceLimitText(n int) string {
	if n <= 0 {
		return "不限"
	}
	return itoa(n) + " 台"
}

// HandleListUsers 列出全部订阅账号。
//
// link_count 直接复用订阅引擎的 collectSubLinks —— 面板显示的数字与客户端真正拿到的
// 条数必须来自同一个函数, 否则会出现"面板说 5 条, 客户端拿到 3 条"这种没法解释的偏差。
func HandleListUsers(c *gin.Context) {
	var users []User
	if err := DB.Order("id asc").Find(&users).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "读取账号失败: " + err.Error()})
		return
	}
	// 条数对所有用户是一样的 (能否重绑取决于**协议**, 与具体凭据无关),
	// 所以随便取一个真实 UUID 算一次即可 —— 关键是必须传非空值,
	// 否则会把 socks/http 这类记不到账的线路也算进去, 于是"面板说 5 条、
	// 客户端只拿到 3 条"。没有用户时传空串, 反正也不会有订阅页。
	credSample := ""
	if len(users) > 0 {
		credSample = users[0].UUID
	}
	links, warns := collectSubLinks(credSample)
	out := make([]userView, 0, len(users))
	for i := range users {
		out = append(out, buildUserView(&users[i], len(links)))
	}
	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"users":       out,
			"total_links": len(links),
			"warnings":    warns,
			"base_url":    subBaseURL(c),
		},
	})
}

// userUpsertRequest 新建/修改账号的请求体。
//
// 指针字段: 修改时"没传"与"传 0"必须区分 —— 配额改成 0 表示不限量,
// 用非指针会把"没传"也当成 0, 一改备注就把配额清了。
type userUpsertRequest struct {
	Username     *string `json:"username"`
	Password     *string `json:"password"`
	UUID         *string `json:"uuid"`
	SubToken     *string `json:"sub_token"`
	TrafficLimit *int64  `json:"traffic_limit"`
	UsedTraffic  *int64  `json:"used_traffic"`
	ExpireAt     *int64  `json:"expire_at"`
	ExpireDate   *string `json:"expire_date"` // "2026-12-31" 或 "2026-12-31 23:59", 传 "" 表示永久
	Status       *string `json:"status"`
	Remark       *string `json:"remark"`

	// —— 套餐相关 ——
	// PlanID 传 0 表示「自定义」(不用套餐, 配额看下面各字段)。
	// 传非 0 时会用套餐的当前值**快照**进用户 (见 model.Plan 注释)。
	PlanID *uint `json:"plan_id"`
	// ResetMode 单独覆盖套餐的重置方式。不传则跟随套餐。
	ResetMode *string `json:"reset_mode"`
	// DeviceLimit 单独覆盖。不传则跟随套餐。
	DeviceLimit *int `json:"device_limit"`
	// Force 忽略套餐的 MaxUsers 上限 (管理员明知故犯时用)。
	Force *bool `json:"force"`
}

// applyTo 把请求体写进账号。
//
// isCreate=true 时会补齐缺失的必填项 (UUID / SubToken / Password), 保证建出来的账号
// 立刻就能订阅; isCreate=false 时只改传了的字段。
func (r *userUpsertRequest) applyTo(u *User, isCreate bool) error {
	if r.Username != nil {
		name := strings.TrimSpace(*r.Username)
		if name == "" {
			return errBadRequest("账号名不能为空")
		}
		if len(name) > 64 {
			return errBadRequest("账号名过长")
		}
		u.Username = name
	} else if isCreate {
		return errBadRequest("请填写账号名")
	}

	if r.UUID != nil {
		id := strings.TrimSpace(*r.UUID)
		if id != "" {
			u.UUID = id
		}
	}
	if isCreate && strings.TrimSpace(u.UUID) == "" {
		u.UUID = newUUID()
	}

	if r.SubToken != nil {
		tok := strings.TrimSpace(*r.SubToken)
		if tok != "" {
			// 订阅 Token 直接进 URL 路径, 字符集必须收紧:
			// 允许 / 会变成路径穿越, 允许 % 会让 gin 的解码结果与存储值不一致。
			if !tokenRe.MatchString(tok) || len(tok) > 64 {
				return errBadRequest("订阅 Token 只允许字母数字与 -_ , 且不超过 64 位")
			}
			u.SubToken = tok
		}
	}
	if isCreate && strings.TrimSpace(u.SubToken) == "" {
		u.SubToken = RandomToken(16)
	}

	if r.Password != nil {
		u.Password = *r.Password
	}
	if isCreate && u.Password == "" {
		// 默认口令取 UUID 前 8 位 (与 work-v 的历史行为一致, 便于用户从面板抄下来)。
		p := strings.ReplaceAll(u.UUID, "-", "")
		if len(p) > 8 {
			p = p[:8]
		}
		u.Password = p
	}

	if r.TrafficLimit != nil {
		if *r.TrafficLimit < 0 {
			return errBadRequest("流量配额不能为负数 (0 表示不限)")
		}
		u.TrafficLimit = *r.TrafficLimit
	}
	// 套餐: 传了 plan_id 就把套餐当前的值快照进用户。
	// 放在显式字段**之前**处理 —— 这样调用方同时传 plan_id 和 traffic_limit 时,
	// 显式值覆盖套餐值, 语义是"套这个套餐但这项我要单独调"。
	if r.PlanID != nil {
		u.PlanID = *r.PlanID
		if *r.PlanID > 0 {
			var p Plan
			if err := DB.First(&p, *r.PlanID).Error; err != nil {
				return errBadRequest("套餐不存在")
			}
			if isCreate && p.MaxUsers > 0 && (r.Force == nil || !*r.Force) {
				var cnt int64
				DB.Model(&User{}).Where("plan_id = ?", p.ID).Count(&cnt)
				if cnt >= int64(p.MaxUsers) {
					return errBadRequest("套餐「" + p.Name + "」已达用户上限 (" + itoa(int(p.MaxUsers)) + ")，可加 force=true 强制分配")
				}
			}
			u.PlanName = p.Name
			u.TrafficLimit = p.TrafficLimit
			u.DeviceLimit = p.DeviceLimit
			u.ResetMode = p.ResetMode
			// 到期时间: 只有创建时才按 duration_days 从今天推。
			// 续费/改套餐时不该重置到期日 —— 那会让用户"越续越短"。
			if isCreate && p.DurationDays > 0 {
				u.ExpireAt = time.Now().Unix() + int64(p.DurationDays)*86400
			}
		} else {
			// plan_id=0 → 自定义, 清掉套餐名免得显示错
			u.PlanName = ""
		}
	}
	if r.ResetMode != nil {
		m := strings.TrimSpace(*r.ResetMode)
		if !validResetModes[m] {
			return errBadRequest("重置方式只能是 none / monthly / cycle")
		}
		u.ResetMode = m
	}
	if r.DeviceLimit != nil {
		if *r.DeviceLimit < 0 {
			return errBadRequest("设备数不能为负 (0 表示不限)")
		}
		u.DeviceLimit = *r.DeviceLimit
	}
	if isCreate && u.ResetMode == "" {
		u.ResetMode = ResetModeMonthly
	}
	if r.UsedTraffic != nil {
		if *r.UsedTraffic < 0 {
			return errBadRequest("已用流量不能为负数")
		}
		u.UsedTraffic = *r.UsedTraffic
	}

	// 到期时间两种写法: 直接给时间戳, 或给日期字符串 (前端日期控件更自然)。
	if r.ExpireDate != nil {
		ts, err := parseExpireDate(*r.ExpireDate)
		if err != nil {
			return err
		}
		u.ExpireAt = ts
	} else if r.ExpireAt != nil {
		if *r.ExpireAt < 0 {
			return errBadRequest("到期时间不能为负数 (0 表示永久)")
		}
		u.ExpireAt = *r.ExpireAt
	}

	if r.Status != nil {
		st := strings.TrimSpace(*r.Status)
		switch st {
		case "active", "suspended", "expired":
			u.Status = st
		default:
			return errBadRequest("状态只能是 active / suspended / expired")
		}
	}
	if isCreate && u.Status == "" {
		u.Status = "active"
	}

	if r.Remark != nil {
		if len(*r.Remark) > 255 {
			return errBadRequest("备注过长")
		}
		u.Remark = *r.Remark
	}
	return nil
}

// parseExpireDate 解析前端传来的到期日期。
//
// 用本地时区的当天 23:59:59 作为到期时刻 —— 用户填 "2026-12-31" 的心智是
// "这天还能用", 按 00:00:00 算会让账号在当天一开始就失效。
func parseExpireDate(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" || s == "none" || s == "永久" {
		return 0, nil
	}
	layouts := []string{
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04",
		"2006-01-02T15:04",
		"2006-01-02",
	}
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			if l == "2006-01-02" {
				t = t.Add(24*time.Hour - time.Second)
			}
			return t.Unix(), nil
		}
	}
	return 0, errBadRequest("无法识别的到期日期: " + s + " (格式 2026-12-31 或 2026-12-31 23:59)")
}

// errBadRequest 是一个轻量错误类型, 只用来在 handler 里区分 400 与 500。
type badRequestError struct{ msg string }

func (e badRequestError) Error() string { return e.msg }
func errBadRequest(msg string) error    { return badRequestError{msg: msg} }

func writeUserErr(c *gin.Context, err error) {
	if be, ok := err.(badRequestError); ok {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": be.msg})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": err.Error()})
}

func HandleCreateUser(c *gin.Context) {
	var req userUpsertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "请求参数格式错误: " + err.Error()})
		return
	}
	var u User
	if err := req.applyTo(&u, true); err != nil {
		writeUserErr(c, err)
		return
	}

	// UUID / SubToken 都是唯一索引, 撞了要给出人能看懂的提示而不是 500。
	var dup int64
	DB.Model(&User{}).Where("username = ?", u.Username).Count(&dup)
	if dup > 0 {
		c.JSON(http.StatusConflict, gin.H{"code": 409, "error": "Conflict", "message": "账号名已存在: " + u.Username})
		return
	}
	DB.Model(&User{}).Where("sub_token = ?", u.SubToken).Count(&dup)
	if dup > 0 {
		c.JSON(http.StatusConflict, gin.H{"code": 409, "error": "Conflict", "message": "订阅 Token 已被占用"})
		return
	}
	DB.Model(&User{}).Where("uuid = ?", u.UUID).Count(&dup)
	if dup > 0 {
		c.JSON(http.StatusConflict, gin.H{"code": 409, "error": "Conflict", "message": "UUID 已被占用"})
		return
	}

	if err := DB.Create(&u).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "创建账号失败: " + err.Error()})
		return
	}

	// 记一条订单 —— 不收款, 但回答"这个号是谁什么时候给的、到期推到哪天"。
	// 创建时把周期起点记成现在, 周期终点记成到期时间 (0 表示永久)。
	RecordOrder(&u, "new", time.Now().Unix(), u.ExpireAt, u.TrafficLimit,
		"新建账号"+planSuffix(&u))

	// 新用户要立刻能用 —— 把名单推到各节点 (异步, 会重启内核)。
	PushUserSyncAll()

	links, _ := collectSubLinks(u.UUID)
	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "账号已创建",
		"data":    buildUserView(&u, len(links)),
	})
}

// planSuffix 拼出「(套餐: xxx)」这样的订单备注后缀。
func planSuffix(u *User) string {
	if u.PlanName != "" {
		return "（套餐: " + u.PlanName + "）"
	}
	return ""
}

func HandleUpdateUser(c *gin.Context) {
	id, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	var u User
	if err := DB.First(&u, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "error": "NotFound", "message": "账号不存在"})
		return
	}
	var req userUpsertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "请求参数格式错误: " + err.Error()})
		return
	}
	if err := req.applyTo(&u, false); err != nil {
		writeUserErr(c, err)
		return
	}

	var dup int64
	DB.Model(&User{}).Where("username = ? AND id <> ?", u.Username, u.ID).Count(&dup)
	if dup > 0 {
		c.JSON(http.StatusConflict, gin.H{"code": 409, "error": "Conflict", "message": "账号名已被其它账号使用"})
		return
	}
	DB.Model(&User{}).Where("sub_token = ? AND id <> ?", u.SubToken, u.ID).Count(&dup)
	if dup > 0 {
		c.JSON(http.StatusConflict, gin.H{"code": 409, "error": "Conflict", "message": "订阅 Token 已被其它账号占用"})
		return
	}
	DB.Model(&User{}).Where("uuid = ? AND id <> ?", u.UUID, u.ID).Count(&dup)
	if dup > 0 {
		c.JSON(http.StatusConflict, gin.H{"code": 409, "error": "Conflict", "message": "UUID 已被其它账号占用"})
		return
	}

	// 用 Select("*") 保存: GORM 的结构体 Updates 会跳过零值,
	// 于是"把配额改成 0 (不限)"和"把到期改成 0 (永久)"会静默失败。
	if err := DB.Model(&User{}).Where("id = ?", u.ID).Select("*").Omit("created_at", "deleted_at").Updates(&u).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "保存账号失败: " + err.Error()})
		return
	}

	// 改完配额/到期/状态要立刻重新判定熔断 —— 否则会出现「面板显示已超额、
	// 但客户端还能连」的空窗 (要等下一次流量入账或 10 分钟的定时巡检才纠正)。
	// 这是管理员改配额后最想立刻看到的结果, 不能拖。
	EnforceUserQuotas()

	DB.First(&u, u.ID) // 重新读回, 让 Locked 等派生字段是判定后的值
	links, _ := collectSubLinks(u.UUID)
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "已保存", "data": buildUserView(&u, len(links))})
}

func HandleDeleteUser(c *gin.Context) {
	id, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	var u User
	if err := DB.First(&u, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "error": "NotFound", "message": "账号不存在"})
		return
	}
	// 不允许删掉最后一个账号: 订阅链接必须有账号载体, 删光了 /sub/:token 会全部 404。
	var cnt int64
	DB.Model(&User{}).Count(&cnt)
	if cnt <= 1 {
		c.JSON(http.StatusBadRequest, gin.H{
			"code": 400, "error": "BadRequest",
			"message": "这是最后一个订阅账号, 不能删除 (订阅链接需要账号载体)",
		})
		return
	}
	if err := DB.Delete(&User{}, id).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "删除账号失败: " + err.Error()})
		return
	}
	// 删号必须下发 —— 否则他的 UUID 还留在机器上的 clients[] 里, 客户端继续能用。
	PushUserSyncAll()
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "账号已删除"})
}

// ---------------------------------------------------------------------------
// 批量运维
// ---------------------------------------------------------------------------

type batchCreateRequest struct {
	Count        int    `json:"count"`
	Prefix       string `json:"prefix"`
	TrafficLimit int64  `json:"traffic_limit"`
	ExpireAt     int64  `json:"expire_at"`
	ExpireDate   string `json:"expire_date"`
}

// HandleBatchCreateUsers 批量建号。
//
// 上限 200: 面板是个人自用工具, 一次要几百个账号只可能是误操作;
// 而每个账号都要做 3 次唯一性查询, 无上限会被一个请求拖住。
func HandleBatchCreateUsers(c *gin.Context) {
	var req batchCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "请求参数格式错误: " + err.Error()})
		return
	}
	if req.Count <= 0 || req.Count > 200 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "数量必须在 1..200 之间"})
		return
	}
	prefix := strings.TrimSpace(req.Prefix)
	if prefix == "" {
		prefix = "user"
	}

	if req.ExpireDate != "" {
		ts, err := parseExpireDate(req.ExpireDate)
		if err != nil {
			writeUserErr(c, err)
			return
		}
		req.ExpireAt = ts
	}

	created := make([]userView, 0, req.Count)
	for i := 0; i < req.Count; i++ {
		u := User{
			UUID:         newUUID(),
			SubToken:     RandomToken(16),
			TrafficLimit: req.TrafficLimit,
			ExpireAt:     req.ExpireAt,
			Status:       "active",
		}
		u.Password = strings.ReplaceAll(u.UUID, "-", "")[:8]
		// 账号名撞了就往后顺延, 不整批失败。
		for attempt := 0; ; attempt++ {
			name := prefix
			if attempt > 0 || req.Count > 1 {
				name = prefix + "_" + itoa(i+1)
				if attempt > 0 {
					name = prefix + "_" + itoa(i+1) + "_" + itoa(attempt+1)
				}
			}
			var dup int64
			DB.Model(&User{}).Where("username = ?", name).Count(&dup)
			if dup == 0 {
				u.Username = name
				break
			}
			if attempt > 50 {
				u.Username = prefix + "_" + RandomToken(4)
				break
			}
		}
		if err := DB.Create(&u).Error; err != nil {
			continue
		}
		created = append(created, buildUserView(&u, 0))
	}
	// 同上: 条数与具体凭据无关, 取一个刚建的号算一次。
	credSample := ""
	if len(created) > 0 {
		credSample = created[0].UUID
	}
	links, _ := collectSubLinks(credSample)
	for i := range created {
		created[i].LinkCount = len(links)
	}
	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "已创建 " + itoa(len(created)) + " 个账号",
		"data":    created,
	})
}

// HandleResetUserTraffic 重置单个账号的已用流量。
//
// 注意这里**不会**去动机器上的 233boy 计数器: 233boy 的 stats 是 per-inbound 累计值,
// 面板改不了也不该改 (改不了是因为 `v2ray xapi stats` 只有读; 不该改是因为重置它等于
// 抹掉机器事实)。面板重置的是**自己的账本**, 也就是配额判定所依据的那个数。
func HandleResetUserTraffic(c *gin.Context) {
	id, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	var u User
	if err := DB.First(&u, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "error": "NotFound", "message": "账号不存在"})
		return
	}
	updates := map[string]any{"used_traffic": 0}
	// 因为超额被自动置为 suspended 的, 重置流量后应当自动恢复。
	// 但**到期**置的 expired 不能恢复 —— 那需要改到期时间, 不是重置流量能解决的。
	if u.Status == "suspended" {
		updates["status"] = "active"
	}
	if err := DB.Model(&User{}).Where("id = ?", u.ID).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "重置流量失败: " + err.Error()})
		return
	}
	u.UsedTraffic = 0
	if s, ok := updates["status"].(string); ok {
		u.Status = s
	}
	links, _ := collectSubLinks(u.UUID)
	// 重置流量可能让熔断用户恢复 → 要重新下发, 让他重新能用。
	EnforceUserQuotas()
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "已重置流量", "data": buildUserView(&u, len(links))})
}

// HandleBatchResetTraffic 重置全部账号流量 (周期结算用)。
func HandleBatchResetTraffic(c *gin.Context) {
	// 全量重置同样只清面板账本, 理由见 HandleResetUserTraffic。
	// 采样表**不清**: 流量曲线是历史事实, 清掉就没有图可看了。
	res := DB.Model(&User{}).Where("used_traffic <> 0").Updates(map[string]any{"used_traffic": 0})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "批量重置失败: " + res.Error.Error()})
		return
	}
	// 超额熔断的账号一并恢复 (与单个重置同口径)。
	res2 := DB.Model(&User{}).Where("status = ?", "suspended").Update("status", "active")
	if res2.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "恢复熔断账号失败: " + res2.Error.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "已重置 " + itoa(int(res.RowsAffected)) + " 个账号, 恢复 " + itoa(int(res2.RowsAffected)) + " 个熔断账号",
	})
}

// HandleRegenerateUserCredentials 重新生成 UUID 与订阅 Token。
//
// ⚠️ 后果必须让用户知道: UUID 是所有节点共用的客户端鉴权凭据, 换掉之后
// **旧订阅立即全部失效**, 必须重新导入。所以这里不做静默处理, 响应里明确说明。
func HandleRegenerateUserCredentials(c *gin.Context) {
	id, ok := parseUintParam(c, "id")
	if !ok {
		return
	}
	var u User
	if err := DB.First(&u, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "error": "NotFound", "message": "账号不存在"})
		return
	}
	oldUUID := u.UUID
	u.UUID = newUUID()
	u.SubToken = RandomToken(16)
	if err := DB.Model(&User{}).Where("id = ?", u.ID).
		Updates(map[string]any{"uuid": u.UUID, "sub_token": u.SubToken}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "重新生成失败: " + err.Error()})
		return
	}
	links, _ := collectSubLinks(u.UUID)
	// 换 UUID 之后必须立刻下发: 否则新凭据没进 conf (用户连不上),
	// 而旧 UUID 还挂在 clients[] 里 (旧客户端照样能用) —— 两头都错。
	PushUserSyncAll()
	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "已重新生成凭据 (旧 UUID " + oldUUID + " 立即失效, 所有客户端需重新导入订阅)",
		"data":    buildUserView(&u, len(links)),
	})
}

// itoa 是 strconv.Itoa 的短别名, 让上面的字符串拼接不那么啰嗦。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
