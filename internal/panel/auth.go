package panel

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// ============================================================================
// 一、JWT 鉴权
// ============================================================================

var (
	jwtSecretMu  sync.RWMutex
	jwtSecretKey []byte
	tokenIssuer  = "v2panel-panel"
)

// CookieNameToken 管理端 JWT 的 Cookie 名 (与 Authorization: Bearer 等价)。
const CookieNameToken = "v2panel_token"

// SetJWTSecret 设置进程内 JWT 密钥 (空值忽略, 交由 getJWTSecretKey 逐级回退)。
func SetJWTSecret(secret string) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return
	}
	jwtSecretMu.Lock()
	jwtSecretKey = []byte(secret)
	jwtSecretMu.Unlock()
}

// getJWTSecretKey 逐级取密钥: 进程内 -> 环境变量 -> 数据库 -> 随机生成并持久化。
//
// 最后一步是必须的: 没有它, 每次重启都会换密钥, 所有已登录会话立刻失效;
// 而把它写回数据库又能保证「重启后仍有效」且无需人工配置。
func getJWTSecretKey() []byte {
	jwtSecretMu.RLock()
	k := jwtSecretKey
	jwtSecretMu.RUnlock()
	if len(k) > 0 {
		return k
	}

	if env := strings.TrimSpace(os.Getenv("V2PANEL_JWT_SECRET")); env != "" {
		SetJWTSecret(env)
		return []byte(env)
	}

	if s := GetSetting("jwt_secret", ""); s != "" {
		SetJWTSecret(s)
		return []byte(s)
	}

	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	fallback := hex.EncodeToString(buf)
	SetJWTSecret(fallback)
	_ = SetSetting("jwt_secret", fallback)
	return []byte(fallback)
}

// Claims 管理端 JWT 载荷。
type Claims struct {
	AdminID  uint   `json:"admin_id"`
	Username string `json:"username"`
	jwt.RegisteredClaims
}

// GenerateToken 签发管理端 token。
func GenerateToken(adminID uint, username string, expireDuration time.Duration) (string, error) {
	now := time.Now()
	claims := Claims{
		AdminID:  adminID,
		Username: username,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tokenIssuer,
			Subject:   fmt.Sprintf("%d", adminID),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(expireDuration)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(getJWTSecretKey())
}

// ParseToken 校验并解析 token。
// 必须显式拒绝非 HMAC 的签名算法: 否则 alg=none / RS256 混淆攻击可伪造任意管理员身份。
func ParseToken(tokenStr string) (*Claims, error) {
	tokenStr = strings.TrimSpace(tokenStr)
	if tokenStr == "" {
		return nil, fmt.Errorf("token 为空")
	}
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("不接受的签名算法: %v", t.Header["alg"])
		}
		return getJWTSecretKey(), nil
	}, jwt.WithIssuer(tokenIssuer), jwt.WithValidMethods([]string{"HS256", "HS384", "HS512"}))
	if err != nil {
		return nil, err
	}
	if !token.Valid {
		return nil, fmt.Errorf("token 无效")
	}
	return claims, nil
}

// AdminAuthRequired 管理端鉴权中间件。
// 优先 Authorization: Bearer, 回退 Cookie (前端同源场景无需自己塞 header)。
func AdminAuthRequired() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := ""
		if h := c.GetHeader("Authorization"); h != "" {
			if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
				raw = strings.TrimSpace(h[7:])
			} else {
				raw = strings.TrimSpace(h)
			}
		}
		if raw == "" {
			if ck, err := c.Cookie(CookieNameToken); err == nil {
				raw = strings.TrimSpace(ck)
			}
		}

		claims, err := ParseToken(raw)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    401,
				"error":   "Unauthorized",
				"message": "登录状态已失效, 请重新登录",
			})
			return
		}
		c.Set("admin_id", claims.AdminID)
		c.Set("admin_username", claims.Username)
		c.Next()
	}
}

// ============================================================================
// 二、登录失败限流
// ============================================================================

// LoginLimiter 基于来源 IP 的登录失败限流 (内存态, 重启即清空)。
type LoginLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
}

// GlobalLoginLimiter 进程级登录限流器。
var GlobalLoginLimiter = &LoginLimiter{attempts: make(map[string][]time.Time)}

const (
	loginWindow   = 5 * time.Minute
	loginMaxFails = 5
)

// CheckAllowed 判断该 IP 当前是否允许尝试登录, 不允许时返回剩余锁定秒数。
func (l *LoginLimiter) CheckAllowed(ip string) (bool, int) {
	// 回环地址永不锁定: 否则本机 CLI/脚本调试会被自己关在门外。
	if ip == "" || isLoopbackHost(ip) {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	kept := l.attempts[ip][:0]
	var last time.Time
	for _, t := range l.attempts[ip] {
		if now.Sub(t) < loginWindow {
			kept = append(kept, t)
			last = t
		}
	}
	l.attempts[ip] = kept

	if len(kept) >= loginMaxFails {
		remain := int((loginWindow - now.Sub(last)).Seconds())
		if remain < 1 {
			remain = 1
		}
		return false, remain
	}
	return true, 0
}

// RecordFailure 记录一次登录失败。
func (l *LoginLimiter) RecordFailure(ip string) {
	if ip == "" || isLoopbackHost(ip) {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	// 防止 map 被大量伪造来源撑爆内存。
	if len(l.attempts) > 4096 {
		l.attempts = make(map[string][]time.Time)
	}
	l.attempts[ip] = append(l.attempts[ip], time.Now())
}

// ResetSuccess 登录成功后清空该 IP 的失败记录。
func (l *LoginLimiter) ResetSuccess(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, ip)
}

// isLoopbackHost 判断 host (可能带端口) 是否为回环地址。
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "::1" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ============================================================================
// 三、通用安全响应头 / CORS
// ============================================================================

// SecurityHeaders 基础安全响应头。
// CSP 里保留 'unsafe-inline'/'unsafe-eval': 前端是手写原生 JS, 且面板完全同源、无用户可控 HTML 注入点。
func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-XSS-Protection", "1; mode=block")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Content-Security-Policy", "default-src 'self' 'unsafe-inline' 'unsafe-eval' data: blob: https:")
		c.Next()
	}
}

// CORSMiddleware 只放行同源与本机回环来源 (面板无跨域开放需求)。
func CORSMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" && originAllowed(origin, c.Request.Host) {
			h := c.Writer.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Origin, Content-Type, Accept, Authorization, X-Safe-Entrance")
			h.Set("Vary", "Origin")
		}
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// originAllowed 判断 Origin 是否与当前 Host 同源, 或属于本机回环开发来源。
func originAllowed(origin, reqHost string) bool {
	o := strings.TrimSpace(origin)
	o = strings.TrimPrefix(o, "http://")
	o = strings.TrimPrefix(o, "https://")
	o = strings.TrimRight(o, "/")
	if o == "" {
		return false
	}
	if strings.EqualFold(o, reqHost) {
		return true
	}
	host := o
	if h, _, err := net.SplitHostPort(o); err == nil {
		host = h
	}
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

// ============================================================================
// 四、安全入口 (宝塔风格)
// ============================================================================

// CookieNameEntrance 安全入口通过凭证 Cookie。
const CookieNameEntrance = "v2panel_entrance_token"

const entranceCookieMaxAge = 30 * 24 * 3600

// 已登记的根目录静态资源 (由 server.go 在挂载前端时注入)。
var (
	staticAssetsMu      sync.RWMutex
	allowedStaticAssets = map[string]bool{}
	indexHTMLHandler    gin.HandlerFunc
)

// SetAllowedStaticAssets 登记允许绕过安全入口的根静态资源。
func SetAllowedStaticAssets(paths []string) {
	staticAssetsMu.Lock()
	defer staticAssetsMu.Unlock()
	allowedStaticAssets = make(map[string]bool, len(paths))
	for _, p := range paths {
		allowedStaticAssets[p] = true
	}
}

// IsAllowedStaticAsset 判断路径是否为已登记的根静态资源。
func IsAllowedStaticAsset(p string) bool {
	staticAssetsMu.RLock()
	defer staticAssetsMu.RUnlock()
	return allowedStaticAssets[p]
}

// SetIndexHTMLHandler 注入「直接就地输出 index.html」的渲染器。
// 安全入口命中时必须就地渲染面板, 绝不能用 302 跳转 —— 跳转会把真实入口路径
// 写进 Location 头与浏览器历史, 等于把安全入口公开。
func SetIndexHTMLHandler(h gin.HandlerFunc) { indexHTMLHandler = h }

// ServeIndexHTML 就地输出面板首页。
func ServeIndexHTML(c *gin.Context) {
	if indexHTMLHandler == nil {
		c.String(http.StatusInternalServerError, "面板前端未挂载")
		return
	}
	indexHTMLHandler(c)
}

// ValidateSafePath 校验安全入口路径。
// 安全入口路径本身会成为「唯一免检入口」, 因此必须杜绝能绕过前缀比较的字符
// (空白/引号/百分号/斜杠重复/..), 也要避开所有真实路由前缀。
func ValidateSafePath(p string) error {
	p = strings.TrimSpace(p)
	if p == "" {
		return fmt.Errorf("路径不能为空")
	}
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("路径必须以 / 开头")
	}
	if len(p) > 64 {
		return fmt.Errorf("路径长度不能超过 64 字符")
	}
	if strings.ContainsAny(p, " \t\r\n?#%&\"'<>\\") {
		return fmt.Errorf("路径包含非法字符")
	}
	if strings.Contains(p, "..") || strings.Contains(p, "//") {
		return fmt.Errorf("路径不能包含 .. 或 //")
	}
	body := strings.TrimPrefix(p, "/")
	if body == "" {
		return fmt.Errorf("路径不能只有 /")
	}
	if strings.Contains(body, ".") {
		return fmt.Errorf("路径不能包含扩展名 (会被静态资源逻辑误判)")
	}
	reserved := []string{"/api", "/sub", "/ws", "/download", "/install.sh", "/index.html", "/favicon.ico", "/login.html"}
	for _, r := range reserved {
		if p == r || strings.HasPrefix(p, r+"/") {
			return fmt.Errorf("路径 %s 为系统保留前缀", r)
		}
	}
	return nil
}

// SafeEntranceConfig 安全入口运行态。
type SafeEntranceConfig struct {
	mu      sync.RWMutex
	Enabled bool
	path    string
}

// GlobalSafeEntrance 进程级安全入口配置。
var GlobalSafeEntrance = &SafeEntranceConfig{Enabled: true, path: DefaultSafePath}

// SetSafePath 设置安全入口路径 (空字符串 = 关闭)。
func SetSafePath(p string) {
	GlobalSafeEntrance.mu.Lock()
	defer GlobalSafeEntrance.mu.Unlock()
	GlobalSafeEntrance.path = strings.TrimRight(strings.TrimSpace(p), "/")
	GlobalSafeEntrance.Enabled = GlobalSafeEntrance.path != ""
}

// SetSafeEntranceState 显式设置开关与路径。
func SetSafeEntranceState(enabled bool, p string) {
	GlobalSafeEntrance.mu.Lock()
	defer GlobalSafeEntrance.mu.Unlock()
	GlobalSafeEntrance.Enabled = enabled
	GlobalSafeEntrance.path = strings.TrimRight(strings.TrimSpace(p), "/")
}

// GetSafePath 返回 (是否启用, 路径)。
func GetSafePath() (bool, string) {
	GlobalSafeEntrance.mu.RLock()
	defer GlobalSafeEntrance.mu.RUnlock()
	return GlobalSafeEntrance.Enabled, GlobalSafeEntrance.path
}

// State 同上, 语义更贴近中间件内的用法。
func (s *SafeEntranceConfig) State() (bool, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Enabled, s.path
}

// computeSafeEntryToken 由「入口路径 + 数据库盐值」派生的通行 Cookie 值。
// 加盐是必须的: 否则 Cookie 值只依赖路径, 而路径一旦泄露给任何人,
// 对方在别的部署上算出的静态值可以直接复用。
func computeSafeEntryToken(p string) string {
	salt := GetSetting("entrance_salt", "")
	sum := sha256.Sum256([]byte(p + ":" + salt))
	return hex.EncodeToString(sum[:16])
}

// SetSafeEntranceCookie 下发通行 Cookie。
func SetSafeEntranceCookie(c *gin.Context) {
	_, p := GlobalSafeEntrance.State()
	if p == "" {
		return
	}
	secure := c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https")
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     CookieNameEntrance,
		Value:    computeSafeEntryToken(p),
		Path:     "/",
		MaxAge:   entranceCookieMaxAge,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// HandleSafeEntryVisit 访问安全入口: 下发 Cookie 并就地渲染面板。
func HandleSafeEntryVisit(c *gin.Context) {
	SetSafeEntranceCookie(c)
	ServeIndexHTML(c)
}

// matchSafePath 安全入口精确匹配 (容忍结尾斜杠差异)。
// matchSafePath 安全入口匹配。
//
// 除了入口本身, 还要接受它的**子路径** —— 前端是 SPA, 路由形如
// /<安全入口>/nodes、/<安全入口>/subscriptions。只做精确匹配的话,
// 用户在深层页面按 F5 会掉进拦截页。
//
// ⚠️ 前缀判断必须带上分隔符: 只写 strings.HasPrefix(p, safePath) 时,
// 入口为 /secret 会让 /secret_backup 也被放行 —— 那是越权。
func matchSafePath(reqPath, safePath string) bool {
	if safePath == "" {
		return false
	}
	if reqPath == safePath || strings.TrimRight(reqPath, "/") == safePath {
		return true
	}
	return strings.HasPrefix(reqPath, safePath+"/")
}

// isPublicPath 无需安全入口即可访问的通道。
//
//	/sub/     订阅链接 (客户端没有 Cookie, 靠订阅 Token 自行鉴权)
//	/ws/      agent 反向长连 (靠首包 Token 鉴权)
//	/download/、/install.sh  节点一键安装脚本与二进制产物
func isPublicPath(p string) bool {
	switch {
	case strings.HasPrefix(p, "/sub/"), p == "/sub":
		return true
	case strings.HasPrefix(p, "/ws/"):
		return true
	case strings.HasPrefix(p, "/download/"):
		return true
	case p == "/install.sh", p == "/api/v1/install.sh":
		return true
	case p == "/api/v1/qrcode":
		return true
	case p == "/api/v1/auth/login":
		return true
	}
	return false
}

// hasValidEntrance 校验通行凭证: Cookie / 显式请求头 / 来自入口页的 Referer。
func hasValidEntrance(c *gin.Context, safePath string) bool {
	want := computeSafeEntryToken(safePath)

	if ck, err := c.Cookie(CookieNameEntrance); err == nil && ck == want {
		return true
	}
	if v := strings.TrimSpace(c.GetHeader("X-Safe-Entrance")); v != "" && v == want {
		return true
	}
	// 前端资源由入口页加载时带 Referer, 保证「进入面板后点链接不被打回」。
	if ref := c.GetHeader("Referer"); ref != "" && strings.Contains(ref, safePath) {
		return true
	}
	return false
}

// SafeEntranceFilter 安全入口拦截中间件。
func SafeEntranceFilter() gin.HandlerFunc {
	return func(c *gin.Context) {
		p := c.Request.URL.Path

		// (0) 自主注册永久关闭: 账号只能由管理员在面板内创建。
		if p == "/register" || p == "/api/v1/register" || p == "/api/v1/auth/register" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"code":    403,
				"error":   "Forbidden",
				"message": "系统已关闭自主注册, 账号由管理员统一创建",
			})
			return
		}

		enabled, safePath := GlobalSafeEntrance.State()
		if !enabled || safePath == "" {
			c.Next()
			return
		}

		// (1) 命中安全入口: 就地渲染, 永不跳转。
		if matchSafePath(p, safePath) {
			HandleSafeEntryVisit(c)
			c.Abort()
			return
		}

		// (2) 已登记的静态资源放行 (否则入口页自身都加载不出来)。
		if IsAllowedStaticAsset(p) {
			c.Next()
			return
		}

		// (3) 公开通道放行。
		if isPublicPath(p) {
			c.Next()
			return
		}

		// (4) 根路径与 index.html 永远只显示拦截页 —— 绝不能在这里渲染登录表单。
		if p == "/" || p == "/index.html" {
			c.Header("Content-Type", "text/html; charset=utf-8")
			c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
			c.String(http.StatusOK, BaotaSafeEntranceHTML)
			c.Abort()
			return
		}

		// (5) 凭证校验通过则放行。
		if hasValidEntrance(c, safePath) {
			c.Next()
			return
		}

		// (6) 未过入口的 API 请求返回 JSON (前端能识别, 不会渲染出一坨 HTML)。
		if strings.HasPrefix(p, "/api/") {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"code":    403,
				"error":   "Forbidden",
				"message": "请先通过安全入口访问面板",
			})
			return
		}

		// (7) 其余一律拦截页。
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
		c.String(http.StatusOK, BaotaSafeEntranceHTML)
		c.Abort()
	}
}

// GetSafeEntranceURL 拼出可直接访问的安全入口地址。
func GetSafeEntranceURL(scheme, host string) string {
	enabled, p := GlobalSafeEntrance.State()
	if !enabled || p == "" {
		return fmt.Sprintf("%s://%s/", scheme, host)
	}
	return fmt.Sprintf("%s://%s%s", scheme, host, p)
}

// BaotaSafeEntranceHTML 安全拦截页 (浅色, 与面板同色系)。
// 只做一件事: 告诉访问者"这里不是入口", 不泄露任何版本/路径/是否存在面板的信息。
const BaotaSafeEntranceHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>403 Forbidden</title>
<style>
  *{margin:0;padding:0;box-sizing:border-box}
  body{min-height:100vh;display:flex;align-items:center;justify-content:center;
    background:#f5f5f7;color:#1d1d1f;
    font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif}
  .card{width:min(420px,92vw);background:#fff;border:1px solid #e5e5ea;border-radius:14px;
    padding:40px 32px;text-align:center;box-shadow:0 1px 3px rgba(0,0,0,.04),0 8px 24px rgba(0,0,0,.05)}
  .badge{width:56px;height:56px;margin:0 auto 20px;border-radius:50%;
    background:rgba(16,185,129,.1);display:flex;align-items:center;justify-content:center}
  .badge svg{width:28px;height:28px;stroke:#10b981}
  h1{font-size:19px;font-weight:600;letter-spacing:-.01em;margin-bottom:10px}
  p{font-size:14px;line-height:1.65;color:#86868b}
  .hint{margin-top:22px;padding-top:18px;border-top:1px solid #f0f0f2;
    font-size:12.5px;color:#a1a1a6;line-height:1.6}
</style>
</head>
<body>
  <div class="card">
    <div class="badge">
      <svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
        <rect x="3" y="11" width="18" height="11" rx="2"/>
        <path d="M7 11V7a5 5 0 0 1 10 0v4"/>
      </svg>
    </div>
    <h1>403 Forbidden</h1>
    <p>当前地址不是有效的访问入口。<br>请通过管理员分配的安全入口地址访问控制台。</p>
    <div class="hint">如需查询入口地址，请在服务器终端执行<br><code>v2panel entry</code></div>
  </div>
</body>
</html>`
