package panel

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	qrcode "github.com/skip2/go-qrcode"
)

// defaultInstallScript 是编译期内置的 agent 一键安装脚本 (兜底)。
//
// 运行时会优先读磁盘上的 scripts/install-agent.sh, 只有读不到才用这份。
// 这样「改脚本 → 重新编译」与「改脚本 → 直接放磁盘」两条路都能走通。
//
//go:embed install.sh
var defaultInstallScript string

// SetupRouter 注册面板全部路由。
//
// webFS 为 nil 时只提供 API (单元测试与纯 CLI 场景)。
func SetupRouter(webFS http.FileSystem) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()

	// 只信任本机反代 (宝塔 nginx 反代到 127.0.0.1:9000)。
	// 不能信任全部代理: 否则任何客户端都能伪造 X-Forwarded-For 绕过登录限流。
	_ = r.SetTrustedProxies([]string{"127.0.0.1", "::1"})

	r.Use(gin.Recovery())
	r.Use(CORSMiddleware())
	r.Use(SecurityHeaders())
	// 安全入口必须排在业务路由之前: 未过入口的请求根本走不到 handler。
	r.Use(SafeEntranceFilter())

	// ── 1. agent 反向长连 (免鉴权, 靠首包 Token 认证) ─────────────────
	r.GET("/ws/agent", func(c *gin.Context) {
		GlobalHub.HandleWebSocket(c.Writer, c.Request)
	})

	// ── 2. 订阅链接 (客户端无 Cookie, 靠订阅 Token 自行鉴权) ──────────
	r.Match([]string{"GET", "POST", "HEAD"}, "/sub/:token", HandleSubscription)
	r.Match([]string{"GET", "POST", "HEAD"}, "/sub", HandleSubscription)

	// ── 3. 节点一键安装脚本动态分发 ─────────────────────────────────
	r.Match([]string{"GET", "HEAD"}, "/install.sh", handleAgentInstallScript)
	r.Match([]string{"GET", "HEAD"}, "/api/v1/install.sh", handleAgentInstallScript)

	// ── 4. 二进制产物分发 (严格白名单, 杜绝任意文件读取) ──────────────
	r.GET("/download/:filename", handleDownload)

	// ── 5. 原生离线二维码 (无外网依赖, 不把节点链接送给第三方) ─────────
	r.GET("/api/v1/qrcode", func(c *gin.Context) {
		text := c.Query("text")
		if text == "" {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "缺少 text 参数"})
			return
		}
		if len(text) > 4096 {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "text 过长"})
			return
		}
		png, err := qrcode.Encode(text, qrcode.Medium, 256)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "二维码生成失败"})
			return
		}
		c.Header("Cache-Control", "public, max-age=86400")
		c.Data(http.StatusOK, "image/png", png)
	})

	// ── 6. REST API ────────────────────────────────────────────────
	v1 := r.Group("/api/v1")

	auth := v1.Group("/auth")
	{
		auth.POST("/login", HandleLogin)
		auth.POST("/logout", HandleLogout)
		auth.GET("/me", AdminAuthRequired(), HandleMe)
	}

	admin := v1.Group("/admin")
	admin.Use(AdminAuthRequired())
	{
		admin.POST("/change-password", HandleChangePassword)
		admin.GET("/dashboard", HandleGetDashboard)

		// 安全入口
		admin.GET("/safe-entrance", HandleGetSafeEntrance)
		admin.POST("/safe-entrance", HandleSetSafeEntrance)

		// 面板设置 (订阅密码开关 / 对外域名)
		admin.GET("/settings", HandleGetSettings)
		admin.POST("/settings", HandleUpdateSettings)

		// 套餐 (Plan) —— 开号模板: 流量额度 / 有效期 / 设备数 / 重置方式 / 节点白名单
		admin.GET("/plans", HandleListPlans)
		admin.POST("/plans", HandleCreatePlan)
		admin.GET("/plans/:id", HandleGetPlan)
		admin.PUT("/plans/:id", HandleUpdatePlan)
		admin.DELETE("/plans/:id", HandleDeletePlan)
		// 把套餐改动同步到该套餐下的存量用户 (显式动作, 不自动)
		admin.POST("/plans/:id/apply", HandleApplyPlan)

		// 用户名单下发 (真限流: 把「谁现在能用」推到节点, 摘掉超额用户)
		admin.POST("/nodes/:id/sync-users", HandleSyncNodeUsers)

		// 公告
		admin.GET("/announcements", HandleListAnnouncements)
		admin.POST("/announcements", HandleCreateAnnouncement)
		admin.PUT("/announcements/:id", HandleUpdateAnnouncement)
		admin.DELETE("/announcements/:id", HandleDeleteAnnouncement)

		// 订单 (记录开号/续费/重置, 不接支付)
		admin.GET("/orders", HandleListOrders)

		// 协议表 (由后端下发, 避免前后端两份定义漂移)
		admin.GET("/protocols", HandleListProtocols)

		// 节点
		admin.GET("/nodes", HandleListNodes)
		admin.POST("/nodes", HandleCreateNode)
		admin.GET("/nodes/:id", HandleGetNode)
		admin.PUT("/nodes/:id", HandleUpdateNode)
		admin.DELETE("/nodes/:id", HandleDeleteNode)
		admin.POST("/nodes/:id/sync", HandleSyncNode)
		admin.GET("/nodes/:id/capability", HandleGetNodeCapability)
		admin.POST("/nodes/:id/restart", HandleRestartNode)
		admin.GET("/nodes/:id/logs", HandleGetNodeLogs)

		// 节点实例 (inbound)
		admin.GET("/nodes/:id/inbounds", HandleListNodeInbounds)
		admin.POST("/nodes/:id/inbounds", HandleCreateNodeInbound)
		admin.PUT("/nodes/:id/inbounds", HandleUpdateNodeInbounds)
		admin.POST("/nodes/:id/inbounds/probe", HandleProbeNodeInbound)
		admin.DELETE("/nodes/:id/inbounds/:conf", HandleDeleteNodeInbound)

		// 订阅账号 (⚠️ 不是多租户用户: 233boy 服务端只有单个 client UUID,
		// 这里的"账号"承载的是订阅凭据 + 配额/到期, 详见 api_user.go 顶部说明)
		admin.GET("/users", HandleListUsers)
		admin.POST("/users", HandleCreateUser)
		admin.POST("/users/batch", HandleBatchCreateUsers)
		admin.POST("/users/reset-all", HandleBatchResetTraffic)
		admin.PUT("/users/:id", HandleUpdateUser)
		admin.DELETE("/users/:id", HandleDeleteUser)
		admin.POST("/users/:id/reset-traffic", HandleResetUserTraffic)
		admin.POST("/users/:id/regenerate", HandleRegenerateUserCredentials)

		// 流量统计
		admin.GET("/traffic/overview", HandleGetTrafficOverview)
		admin.GET("/traffic/series", HandleGetTrafficSeries)
		admin.GET("/traffic/inbounds", HandleGetTrafficInbounds)
		admin.POST("/traffic/purge", HandlePurgeTrafficSamples)
	}

	// ── 7. 前端静态资源 ────────────────────────────────────────────
	if webFS != nil {
		// 登记根目录静态资源: 只有真实存在的文件才允许在未过安全入口时访问。
		SetAllowedStaticAssets(scanStaticAssets(webFS))

		// 就地渲染首页。安全入口命中时绝不能 302 —— 跳转会把真实入口路径
		// 写进 Location 头与浏览器历史, 等于把入口公开。
		//
		// ⚠️ 前端是 SPA (Vue Router history 模式), 必须告诉它「自己挂在哪个路径下」。
		// 因为安全入口是就地渲染的, 浏览器地址栏里可能是 /a1b2c3d4e5f60718 这种前缀,
		// 若前端按 base="/" 解析, 它会拿这个前缀去匹配路由 → 命中 404 页。
		// 这里在 index.html 的 </head> 前注入一行全局变量, 前端据此初始化 router base。
		SetIndexHTMLHandler(func(c *gin.Context) {
			// webFS 是 http.FileSystem (Open(string) (http.File, error)),
			// 不是 fs.FS, 所以不能用 fs.ReadFile —— 用它的 Open + io.ReadAll。
			f, err := webFS.Open("/index.html")
			if err != nil {
				c.String(http.StatusInternalServerError, "index.html 缺失")
				return
			}
			defer f.Close()
			raw, err := io.ReadAll(f)
			if err != nil {
				c.String(http.StatusInternalServerError, "读取 index.html 失败")
				return
			}

			_, safePath := GlobalSafeEntrance.State()
			base := "/"
			if safePath != "" {
				base = strings.TrimRight(safePath, "/")
				if base == "" {
					base = "/"
				}
			}
			// safePath 已由 ValidateSafePath 限制过字符集 (无引号/空白/百分号),
			// 这里再用 json.Marshal 兜一层, 杜绝注入到 <script> 里的可能。
			b, _ := json.Marshal(base)
			inject := []byte("<script>window.__VP_BASE__=" + string(b) + ";</script>\n")

			out := raw
			if i := bytes.Index(raw, []byte("</head>")); i >= 0 {
				out = make([]byte, 0, len(raw)+len(inject))
				out = append(out, raw[:i]...)
				out = append(out, inject...)
				out = append(out, raw[i:]...)
			}

			c.Header("Content-Type", "text/html; charset=utf-8")
			c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
			c.Data(http.StatusOK, "text/html; charset=utf-8", out)
		})

		fileServer := http.FileServer(webFS)
		r.NoRoute(func(c *gin.Context) {
			path := c.Request.URL.Path
			if strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/ws/") || strings.HasPrefix(path, "/sub/") {
				c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "接口不存在"})
				return
			}
			if f, err := webFS.Open(strings.TrimPrefix(path, "/")); err == nil {
				f.Close()
				fileServer.ServeHTTP(c.Writer, c.Request)
				return
			}
			// 看起来像静态资源却不存在 => 直接 404, 不回落 index.html。
			// 否则未过安全入口的 /x.js 也能把面板 HTML 取走。
			if path != "/" && filepath.Ext(path) != "" {
				c.String(http.StatusNotFound, "404 page not found")
				return
			}
			// 其余情况是 SPA 的前端路由 (如 /a1b2c3d4e5f60718/nodes)。
			//
			// ⚠️ 必须走 ServeIndexHTML 而不是 fileServer:
			// 前者会注入 window.__VP_BASE__ (前端据此初始化 router base),
			// 后者吐的是原始 index.html —— 那样刷新深层路由时前端拿不到 base,
			// 会把 /a1b2c3d4e5f60718/nodes 当成未知路由, 直接显示 404 页。
			ServeIndexHTML(c)
		})
	}

	return r
}

// scanStaticAssets 列出前端产物里允许绕过安全入口的静态资源。
// index.html 不在免检之列 —— 它只能通过安全入口渲染。
//
// 为什么要递归子目录: Vite 构建产物把 JS/CSS 放在 /assets/ 下,
// 名字带内容哈希 (index-DZbogI2R.js)。只登记根级文件的话, 未过安全入口时
// /assets/*.js 会被 SafeEntranceFilter 拦成拦截页 —— 表现就是
// 「首页能打开但白屏」, 而且因为返回的是 HTML, 控制台只会报 MIME 类型错误,
// 极难定位。这里把 assets/ 一并登记。
//
// 安全性没有因此降低: 登记的仍然只是**构建产物**(带哈希的 js/css)与根级静态文件,
// 不含 index.html, 也不含任何后端数据。
func scanStaticAssets(webFS http.FileSystem) []string {
	names := make([]string, 0, 16)

	// ⚠️ 拼接子目录路径必须用 path.Join, 不能写 dir+"/"+name。
	// http.FS 的 Open 对尾斜杠是**不宽容**的: Open("/assets/") 直接返回
	// "invalid argument", 于是递归会静默地什么都收不到 ——
	// 表现就是 /assets/*.js 未过安全入口时被拦成拦截页 (前端白屏), 且没有任何报错。
	// 这个坑排查过一次: 逻辑看着完全正确, 单测也不报错, 只是永远返回空。
	//
	// 只下钻一层: Vite 的产物结构固定是 /assets/xxx, 没有更深的层级。
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		f, err := webFS.Open(dir)
		if err != nil {
			return
		}
		defer f.Close()
		entries, err := f.Readdir(-1)
		if err != nil {
			return
		}
		for _, e := range entries {
			full := path.Join(dir, e.Name())
			if e.IsDir() {
				if depth == 0 && e.Name() == "assets" {
					walk(full, depth+1)
				}
				continue
			}
			if e.Name() == "index.html" {
				continue
			}
			names = append(names, full)
		}
	}
	walk("/", 0)

	return names
}

// handleDownload 分发 agent / panel 二进制。
//
// 这是唯一把文件内容吐给公网的接口, 因此三重设防:
//  1. 前缀白名单 (只允许 v2panel-agent* / v2panel-panel*);
//  2. 子串黑名单 (数据库/配置/密钥/证书一律拒绝);
//  3. 字符集白名单 ([A-Za-z0-9._-]), 任何其它字符直接拒绝。
//
// 黑名单必须用「包含」而不是「后缀」: 只看后缀时 config.json.bak /
// v2panel.db.old 这类备份会把数据库(含管理员哈希与节点 Token)匿名泄露。
func handleDownload(c *gin.Context) {
	rawName := c.Param("filename")
	filename := filepath.Base(filepath.Clean(rawName))

	if filename == "." || filename == "/" || filename == "\\" || strings.Contains(rawName, "..") {
		c.String(http.StatusBadRequest, "Invalid filename")
		return
	}
	if filename == "install.sh" || filename == "install-agent.sh" {
		handleAgentInstallScript(c)
		return
	}

	allowedPrefixes := []string{"v2panel-agent", "v2panel-panel", "agent-", "panel-"}
	isAllowed := false
	for _, p := range allowedPrefixes {
		if strings.HasPrefix(filename, p) {
			isAllowed = true
			break
		}
	}

	blocked := strings.HasPrefix(filename, ".")
	for _, s := range []string{".db", ".env", ".go", ".yml", ".yaml", ".json", ".conf", ".pem", ".key", ".log", ".bak", ".sh", ".."} {
		if strings.Contains(filename, s) {
			blocked = true
			break
		}
	}
	for _, ch := range filename {
		safe := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') || ch == '.' || ch == '-' || ch == '_'
		if !safe {
			blocked = true
			break
		}
	}
	if !isAllowed || blocked {
		c.String(http.StatusForbidden, "Access denied")
		return
	}

	// 产物落盘位置: 打包脚本 / 手工 cp 都往 dist/ 放, 同时兼容工作目录与 /usr/local/bin。
	candidates := []string{
		filepath.Join("/opt/v2panel/dist", filename),
		filepath.Join("/opt/v2panel", filename),
		filepath.Join(".", filename),
		filepath.Join("dist", filename),
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			c.File(p)
			return
		}
	}
	c.String(http.StatusNotFound, "File not found")
}

// handleAgentInstallScript 动态生成 agent 一键安装脚本。
//
// 脚本即将以 root 身份在目标机上执行, 因此:
//   - Host 头完全由客户端控制, 必须 sanitizeHost 校验后才允许拼进去;
//   - 替换必须用 ReplaceAllLiteralString —— ReplaceAllString 会把替换串里的
//     $1/${name} 当捕获组展开, 是经典的注入点。
func handleAgentInstallScript(c *gin.Context) {
	panelURL, ok := panelBaseURL(c)
	if !ok {
		c.String(http.StatusBadRequest, "Invalid Host header")
		return
	}

	// 优先用配置的外网面板域名 (Host 头在纯 IP 访问时是 IP, 装出来的 agent 会连 IP;
	// 面板绑了域名就该连域名)。
	if d := strings.TrimSpace(GetSetting("panel_domain", "")); d != "" {
		if strings.HasPrefix(d, "http://") || strings.HasPrefix(d, "https://") {
			panelURL = strings.TrimRight(d, "/")
		} else {
			panelURL = "https://" + strings.TrimRight(d, "/")
		}
	}

	// 优先读磁盘上的最新脚本, 读不到才用编译期内置的那份。
	script := defaultInstallScript
	for _, p := range []string{
		"scripts/install-agent.sh",
		"/opt/v2panel/scripts/install-agent.sh",
		"internal/panel/install.sh",
		"install.sh",
	} {
		if content, err := os.ReadFile(p); err == nil && len(content) > 0 {
			script = string(content)
			break
		}
	}

	script = reDefaultServer.ReplaceAllLiteralString(script, fmt.Sprintf(`DEFAULT_SERVER="%s"`, panelURL))

	// 面板「添加节点」页给出的一次性命令里会带 ?token=xxx, 直接烤进脚本,
	// 用户复制一行就能装好, 不必再传参。
	//
	// ⚠️ 保护名单必须跟着 Token 一起下发: agent 的 guard() 只在名单非空时才拦得住
	// 写操作。若安装脚本不写 protected, 纳管一台已经在跑线上节点的机器就等于给了
	// 面板一个「删光该机全部 conf」的能力 (tzq1 就是这么被暴露的)。
	// 名单按 Token 反查节点取, 因此这里不需要额外的 query 参数。
	if tok := strings.TrimSpace(c.Query("token")); tok != "" {
		if !tokenRe.MatchString(tok) {
			c.String(http.StatusBadRequest, "Invalid token")
			return
		}
		script = reDefaultToken.ReplaceAllLiteralString(script, fmt.Sprintf(`DEFAULT_TOKEN="%s"`, tok))
		script = reDefaultProtected.ReplaceAllLiteralString(script, fmt.Sprintf(`DEFAULT_PROTECTED="%s"`, ProtectedListForToken(tok)))
	}

	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
	c.String(http.StatusOK, script)
}

// BuildAgentInstallCommand 生成给管理员复制的节点纳管命令。
//
// ⚠️ 明文 Token 只在这一处响应里出现一次, 绝不写日志 (journald 里留存等于泄露)。
func BuildAgentInstallCommand(c *gin.Context, token string) string {
	panelURL, ok := panelBaseURL(c)
	if !ok {
		panelURL = "http://127.0.0.1"
	}
	if d := strings.TrimSpace(GetSetting("panel_domain", "")); d != "" {
		if strings.HasPrefix(d, "http://") || strings.HasPrefix(d, "https://") {
			panelURL = strings.TrimRight(d, "/")
		} else {
			panelURL = "https://" + strings.TrimRight(d, "/")
		}
	}
	return fmt.Sprintf("curl -fsSL %s/install.sh?token=%s | bash", panelURL, token)
}

// panelBaseURL 由当前请求推导面板对外地址。
func panelBaseURL(c *gin.Context) (string, bool) {
	scheme := "http"
	if c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host, ok := sanitizeHost(c.Request.Host)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%s://%s", scheme, host), true
}

var (
	reDefaultServer    = regexp.MustCompile(`DEFAULT_SERVER="[^"]*"`)
	reDefaultToken     = regexp.MustCompile(`DEFAULT_TOKEN="[^"]*"`)
	reDefaultProtected = regexp.MustCompile(`DEFAULT_PROTECTED="[^"]*"`)
	// tokenRe 与 install.sh 里的字符集校验保持一致 (32 位十六进制)。
	tokenRe = regexp.MustCompile(`^[A-Za-z0-9]{8,64}$`)
	// protectedRe 与 install.sh 的名单校验保持一致: 逗号分隔的配置名子串。
	protectedRe = regexp.MustCompile(`^[A-Za-z0-9._,:/-]{1,512}$`)
)

// ProtectedListForToken 按 agent Token 反查该节点的保护名单, 返回逗号分隔串。
//
// 查不到节点 (或 Token 非法) 时返回空串 —— 安装脚本会把空串写成 `"protected": []`,
// 也就是「不保护任何配置」。这是**故意**的: 保护名单是给「纳管一台已在跑线上节点的
// 机器」用的安全阀, 而一台全新机器上没有任何东西需要保护。
func ProtectedListForToken(token string) string {
	if !tokenRe.MatchString(token) {
		return ""
	}
	var n Node
	if err := DB.Where("token = ?", token).First(&n).Error; err != nil {
		return ""
	}
	return NormalizeProtected(n.Protected)
}

// NormalizeProtected 把用户填的保护名单规范成逗号分隔的子串串。
//
// 名单会被烤进以 root 执行的安装脚本并写进 config.json, 所以字符集必须在这里
// 收口 —— 面板是唯一入口, 不能指望前端校验。允许: 字母/数字/._- 以及分隔符 , : /
// (冒号与斜杠用于匹配带域名的 conf 名, 如 VLESS-WS-TLS-node1.example.com.json)。
func NormalizeProtected(raw string) string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == ';'
	})
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		// 单个子串单独校验, 免得一个坏字符废掉整张名单。
		if !protectedRe.MatchString(p) {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return strings.Join(out, ",")
}

// sanitizeHost 校验并规范化 Host 头 (host[:port])。
// 只接受合法域名/IP + 数字端口; 引号/分号/$/空白/斜杠一律拒绝。
func sanitizeHost(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 255 {
		return "", false
	}
	host, port := raw, ""
	if h, p, err := net.SplitHostPort(raw); err == nil {
		host, port = h, p
	}
	host = strings.Trim(host, "[]") // IPv6
	if host == "" {
		return "", false
	}
	if net.ParseIP(host) == nil {
		for _, ch := range host {
			alnum := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')
			if alnum || ch == '.' || ch == '-' || ch == '_' {
				continue
			}
			return "", false
		}
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", false
		}
		return net.JoinHostPort(host, port), true
	}
	return host, true
}
