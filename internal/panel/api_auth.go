package panel

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// tokenTTL 管理端登录有效期。
// 面板是个人自用工具, 7 天免登录比"每天重输一次"更符合实际使用习惯;
// 安全入口 + 失败限流 + 可选绑域名 HTTPS 已覆盖主要风险。
const tokenTTL = 7 * 24 * time.Hour

// dummyBcryptHash 用于「用户名不存在」时也走一次 bcrypt 比对。
//
// 不加这个, 不存在的用户名会立刻返回, 而存在的用户名要等 ~60ms 的 bcrypt ——
// 响应时间差就能被用来枚举有效用户名。
var dummyBcryptHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

type loginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// HandleLogin 管理员登录。
func HandleLogin(c *gin.Context) {
	ip := c.ClientIP()
	if ok, remain := GlobalLoginLimiter.CheckAllowed(ip); !ok {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"code":        429,
			"error":       "TooManyRequests",
			"message":     "登录失败次数过多, 请等待后重试",
			"retry_after": remain,
		})
		return
	}

	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "请填写用户名与密码"})
		return
	}
	req.Username = strings.TrimSpace(req.Username)

	var admin Admin
	err := DB.Where("username = ?", req.Username).First(&admin).Error
	hash := admin.PasswordHash
	if err != nil || hash == "" {
		hash = dummyBcryptHash
	}
	cmpErr := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password))
	if err != nil || cmpErr != nil {
		GlobalLoginLimiter.RecordFailure(ip)
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "error": "Unauthorized", "message": "用户名或密码错误"})
		return
	}

	GlobalLoginLimiter.ResetSuccess(ip)

	token, err := GenerateToken(admin.ID, admin.Username, tokenTTL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "签发登录凭证失败"})
		return
	}

	now := time.Now()
	DB.Model(&Admin{}).Where("id = ?", admin.ID).Updates(map[string]any{
		"last_login_at": &now,
		"last_login_ip": ip,
	})

	// 同时下发 Cookie: 前端多页跳转无需在每个页面手动管理 token。
	secure := c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https")
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     CookieNameToken,
		Value:    token,
		Path:     "/",
		MaxAge:   int(tokenTTL.Seconds()),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})

	c.JSON(http.StatusOK, gin.H{
		"code":     0,
		"message":  "登录成功",
		"token":    token,
		"expires":  now.Add(tokenTTL).Unix(),
		"username": admin.Username,
	})
}

// HandleLogout 退出登录 (清 Cookie)。
func HandleLogout(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     CookieNameToken,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
	})
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "已退出登录"})
}

// HandleMe 返回当前登录管理员信息 (前端用于判断是否已登录)。
func HandleMe(c *gin.Context) {
	id, _ := c.Get("admin_id")
	uid, _ := id.(uint)
	var admin Admin
	if err := DB.First(&admin, uid).Error; err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "error": "Unauthorized", "message": "账号不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"id":            admin.ID,
			"username":      admin.Username,
			"last_login_at": admin.LastLoginAt,
			"last_login_ip": admin.LastLoginIP,
		},
	})
}

type changePasswordRequest struct {
	OldPassword string `json:"old_password" binding:"required"`
	NewPassword string `json:"new_password" binding:"required"`
}

// HandleChangePassword 修改管理员密码。
func HandleChangePassword(c *gin.Context) {
	var req changePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "请填写原密码与新密码"})
		return
	}
	if len(req.NewPassword) < 6 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "新密码长度不能少于 6 位"})
		return
	}

	id, _ := c.Get("admin_id")
	uid, _ := id.(uint)
	var admin Admin
	if err := DB.First(&admin, uid).Error; err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "error": "Unauthorized", "message": "账号不存在"})
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(admin.PasswordHash), []byte(req.OldPassword)) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "原密码不正确"})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "密码加密失败"})
		return
	}
	if err := DB.Model(&Admin{}).Where("id = ?", admin.ID).Update("password_hash", string(hash)).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "保存新密码失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "密码修改成功, 请重新登录"})
}

// HandleGetSafeEntrance 查询安全入口配置与访问地址。
func HandleGetSafeEntrance(c *gin.Context) {
	enabled, path := GlobalSafeEntrance.State()
	scheme := "http"
	if c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"enabled":   enabled,
			"path":      path,
			"entry_url": GetSafeEntranceURL(scheme, c.Request.Host),
		},
	})
}

type safeEntranceRequest struct {
	Enabled bool   `json:"enabled"`
	Path    string `json:"path"`
}

// HandleSetSafeEntrance 修改安全入口。
func HandleSetSafeEntrance(c *gin.Context) {
	var req safeEntranceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "请求参数格式错误"})
		return
	}

	if !req.Enabled {
		// 用显式开关关闭, 不清空 safe_path ——
		// 空值无法与"从未配置"区分, 重启后会被默认值重新打开。
		if err := SetSetting("safe_entry_enabled", "0"); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "保存设置失败"})
			return
		}
		SetSafeEntranceState(false, req.Path)
		c.JSON(http.StatusOK, gin.H{"code": 0, "message": "安全入口已关闭", "data": gin.H{"enabled": false, "path": req.Path}})
		return
	}

	p := strings.TrimSpace(req.Path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if err := ValidateSafePath(p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": err.Error()})
		return
	}
	if err := SetSetting("safe_path", p); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "保存入口路径失败"})
		return
	}
	if err := SetSetting("safe_entry_enabled", "1"); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "保存开关失败"})
		return
	}
	SetSafeEntranceState(true, p)
	SetSafeEntranceCookie(c)

	scheme := "http"
	if c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "安全入口已更新",
		"data":    gin.H{"enabled": true, "path": p, "entry_url": GetSafeEntranceURL(scheme, c.Request.Host)},
	})
}

// HandleGetDashboard 面板首页概览。
func HandleGetDashboard(c *gin.Context) {
	var (
		nodeTotal    int64
		nodeOnline   int64
		inboundCnt   int64
		userTotal    int64
		userAbnormal int64
	)
	DB.Model(&Node{}).Count(&nodeTotal)
	DB.Model(&Node{}).Where("status = ?", "online").Count(&nodeOnline)
	DB.Model(&Inbound{}).Count(&inboundCnt)
	DB.Model(&User{}).Count(&userTotal)
	DB.Model(&User{}).Where("status <> ?", "active").Count(&userAbnormal)

	// 今日流量 (本地时区当日 0 点起)。
	now := time.Now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	up, down, _ := SumTrafficBetween(dayStart, now)

	var nodeList []Node
	DB.Order("id asc").Find(&nodeList)

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"node_total":    nodeTotal,
			"node_online":   nodeOnline,
			"inbound_total": inboundCnt,
			"user_total":    userTotal,
			"user_abnormal": userAbnormal,
			"today_up":      up,
			"today_down":    down,
			"today_total":   up + down,
			"today_up_h":    HumanBytes(up),
			"today_down_h":  HumanBytes(down),
			"today_total_h": HumanBytes(up + down),
			"nodes":         nodeList,
		},
	})
}
