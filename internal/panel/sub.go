package panel

import (
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"v2panel/internal/agent"
	"v2panel/internal/common"

	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------- 订阅渲染
//
// 233boy v4.27 真正能产出的客户端链接只有下面几类（逐字对照 /etc/v2ray/sh/src/core.sh info()）：
//
//	vmess://base64(JSON)   tcp / kcp / quic / tcpd / kcpd / quicd，以及 ws|h2|grpc 的 vmess
//	vless://               ws|h2|grpc（URL 里 security=tls 是硬编码）
//	trojan://              同上
//	ss://base64(m:p)@h:p   shadowsocks
//	socks://base64(u:p)@h:p
//	door / http            **不产出任何链接**（local-http 只监听回环，dokodemo-door 没有客户端形态）
//
// reality / hysteria2 / tuic / anytls 在 v4.27 里根本不存在，这里一律不实现。
//
// ⚠️ 链接里的 security=tls 与服务端 conf 里的 security=none 是**设计如此**，不是 bug：
// 客户端 TLS → Caddy（机器级 https_port，持真证书）→ 明文回环到各节点自己的内网口。
// 真实链路里 TLS 在 Caddy 终结，所以入站侧本来就是 none。**永远不要"修正"它。**

const (
	subAuthCookie   = "v2panel_sub_auth"
	subCookieMaxAge = 30 * 24 * 3600
	subUpdateHours  = 24
)

// subLink 一条可下发的客户端链接。
type subLink struct {
	Name        string // 去重后的展示名（客户端里唯一）
	NodeName    string
	ConfName    string
	Protocol    string // VLESS-WS-TLS 这类标签
	Link        string
	Address     string
	Port        int
	PortType    string // "公网直连" / "Caddy 反代"
	Description string
	Raw         *agent.ShareURI
}

// ---------------------------------------------------------------- 入口

// HandleSubscription 多端自适应订阅出口。
//
// 安全姿态（沿用 work-v 的结论，不要退化）：
//   - 节点地址只取库里存的 Address（agent 上报的事实），**绝不回落到请求 Host** ——
//     Host 头由客户端完全控制，一旦回落，伪造 Host 就能把所有节点地址改写到攻击者机器。
//   - token 只从路径或 query 取，长度与字符集先校验，再查库。
//   - 浏览器 UA → 密码门户；非浏览器 → 401 Basic，避免把订阅明文甩给随手点开链接的人。
func HandleSubscription(c *gin.Context) {
	token := strings.TrimSpace(c.Param("token"))
	if token == "" {
		token = strings.TrimSpace(c.Query("token"))
	}
	if token == "" || len(token) > 64 || !tokenRe.MatchString(token) {
		c.String(http.StatusNotFound, "Subscription not found")
		return
	}

	var user User
	if err := DB.Where("sub_token = ?", token).First(&user).Error; err != nil {
		c.String(http.StatusNotFound, "Subscription not found")
		return
	}

	// 订阅 Token 本身就是 128 位随机凭据, 对绝大多数用户这就是唯一需要的凭证。
	//
	// 额外的订阅密码是**可选**的第二层, 默认关闭:
	//   关闭 → 「拿到链接就能用」, 客户端里粘贴 /sub/<token> 直接成功 (小白形态);
	//   打开 → 门户要求输密码, 链接要带 ?pwd=xxx 才能在客户端用 (防链接外泄)。
	//
	// 早期实现是强制密码, 结果是用户把 /sub/<token> 粘进 Clash Verge 只会拿到 401,
	// 而界面上没有任何地方告诉他要加 ?pwd= —— 这是最劝退的一处。
	requirePwd := strings.TrimSpace(GetSetting("sub_require_password", "0")) == "1"
	isAuth, authErr := checkSubscriptionAuth(c, &user)
	if !requirePwd {
		isAuth, authErr = true, false
	}
	if isAuth && c.Query("pwd") != "" {
		setSubAuthCookie(c, &user)
	}

	expired, overQuota := subLimits(&user)
	locked := expired || overQuota

	// 浏览器永远拿门户页，不直接给配置正文（门户里有密码解锁与各格式的下载按钮）。
	if isBrowserRequest(c) {
		links, warns := collectSubLinks(user.UUID)
		renderSubPortal(c, &user, links, warns, isAuth, authErr, expired, overQuota)
		return
	}

	if !isAuth {
		c.Header("WWW-Authenticate", `Basic realm="v2panel Subscription"`)
		c.String(http.StatusUnauthorized, "Unauthorized")
		return
	}
	if locked {
		c.String(http.StatusForbidden, "Subscription expired or quota exceeded")
		return
	}

	// 凭据用该用户自己的 UUID —— agent 已把它写进各节点 conf 的 clients[],
	// 这样 V2Ray 才会按 email 单独计数。见 collectSubLinks 的注释。
	links, _ := collectSubLinks(user.UUID)

	// 用量头：233boy 没有按用户计费，UsedTraffic 是面板把各节点上报的增量累加到订阅账号上的。
	// 个人单用户场景正确；它不是多租户计量。
	used := user.UsedTraffic
	if used < 0 {
		used = 0
	}
	total := user.TrafficLimit
	if total < 0 {
		total = 0
	}
	expire := user.ExpireAt
	if expire < 0 {
		expire = 0
	}
	c.Header("Subscription-Userinfo", fmt.Sprintf("upload=0; download=%d; total=%d; expire=%d", used, total, expire))
	c.Header("Profile-Update-Interval", strconv.Itoa(subUpdateHours))
	c.Header("Cache-Control", "no-store, no-cache, must-revalidate")
	c.Header("X-Content-Type-Options", "nosniff")

	kind := detectSubFormat(c)
	var (
		body string
		ct   string
		ext  string
	)
	switch kind {
	case subFormatClash:
		body, ct, ext = generateClashYAML(links), "text/yaml; charset=utf-8", "yaml"
	case subFormatSingbox:
		body, ct, ext = generateSingboxJSON(links), "application/json; charset=utf-8", "json"
	default:
		body, ct, ext = generateBase64Links(links), "text/plain; charset=utf-8", "txt"
	}
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"v2panel-%s.%s\"", kind, ext))
	c.Data(http.StatusOK, ct, []byte(body))
}

// ---------------------------------------------------------------- 鉴权

func subAuthCookieValue(u *User) string {
	sum := sha256.Sum256([]byte(u.SubToken + ":" + u.Password + ":" + u.UUID))
	return hex.EncodeToString(sum[:])
}

func setSubAuthCookie(c *gin.Context, u *User) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(subAuthCookie, subAuthCookieValue(u), subCookieMaxAge, "/", "", c.Request.TLS != nil, true)
}

// checkSubscriptionAuth 四个来源：?logout=1 / Basic Auth / ?pwd=|?password= / 表单，外加 Cookie。
// hasError 表示"用户确实提供了凭证但是错的"，门户页据此提示；什么都没提供时是 (false,false)。
func checkSubscriptionAuth(c *gin.Context, u *User) (isAuth bool, hasError bool) {
	if v := c.Query("logout"); v == "1" || v == "true" {
		c.SetCookie(subAuthCookie, "", -1, "/", "", c.Request.TLS != nil, true)
		return false, false
	}

	want := subAuthCookieValue(u)
	if ck, err := c.Cookie(subAuthCookie); err == nil && ck != "" {
		if subtle.ConstantTimeCompare([]byte(ck), []byte(want)) == 1 {
			return true, false
		}
	}

	if _, pw, ok := c.Request.BasicAuth(); ok {
		if pw == u.Password || pw == u.UUID {
			return true, false
		}
		return false, true
	}

	for _, k := range []string{"pwd", "password"} {
		if v := c.Query(k); v != "" {
			if v == u.Password || v == u.UUID {
				return true, false
			}
			return false, true
		}
	}

	if c.Request.Method == http.MethodPost {
		_ = c.Request.ParseForm()
		for _, k := range []string{"password", "pwd"} {
			if v := c.Request.PostFormValue(k); v != "" {
				if v == u.Password || v == u.UUID {
					return true, false
				}
				return false, true
			}
		}
	}

	return false, false
}

func subLimits(u *User) (expired bool, overQuota bool) {
	if u.Status != "" && u.Status != "active" {
		expired = true
	}
	if u.ExpireAt > 0 && time.Now().Unix() > u.ExpireAt {
		expired = true
	}
	if u.TrafficLimit > 0 && u.UsedTraffic >= u.TrafficLimit {
		overQuota = true
	}
	return expired, overQuota
}

// ---------------------------------------------------------------- UA 分流

const (
	subFormatClash   = "clash"
	subFormatSingbox = "singbox"
	subFormatBase64  = "base64"
)

var (
	clashUAs   = []string{"clash", "mihomo", "verge", "stash", "surfboard", "clashx", "clash-verge", "flclash", "openclash", "shadowrocket"}
	singboxUAs = []string{"sing-box", "singbox", "s-box", "sfi", "sfa", "hiddify", "karing", "nekobox", "nekoray", "streisand", "exclave"}
	// 非浏览器客户端标识：命中这些一律当"要配置正文"，哪怕 UA 里带 mozilla。
	clientUAs = []string{
		"clash", "mihomo", "verge", "stash", "surfboard", "sing-box", "singbox", "s-box",
		"hiddify", "karing", "shadowrocket", "v2ray", "v2rayng", "v2rayn", "xray", "qv2ray",
		"nekoray", "nekobox", "surge", "loon", "quantumult", "quanx", "streisand", "exclave",
		"curl", "wget", "python", "go-http", "okhttp", "libwww", "aria2",
	}
)

func detectSubFormat(c *gin.Context) string {
	t := strings.ToLower(strings.TrimSpace(c.Query("type")))
	switch t {
	case "clash", "mihomo", "clashmeta", "clash.meta", "yaml", "yml":
		return subFormatClash
	case "singbox", "sing-box", "sing_box", "sbox", "json":
		return subFormatSingbox
	case "base64", "v2ray", "v2rayn", "txt", "links":
		return subFormatBase64
	}
	ua := strings.ToLower(c.GetHeader("User-Agent"))
	for _, k := range clashUAs {
		if strings.Contains(ua, k) {
			return subFormatClash
		}
	}
	for _, k := range singboxUAs {
		if strings.Contains(ua, k) {
			return subFormatSingbox
		}
	}
	return subFormatBase64
}

// isBrowserRequest 判断是不是"人在浏览器里点开的"。
// 只要显式带了 type 参数，或者 UA 命中客户端标识，就一律当客户端处理。
func isBrowserRequest(c *gin.Context) bool {
	if strings.TrimSpace(c.Query("type")) != "" {
		return false
	}
	ua := strings.ToLower(c.GetHeader("User-Agent"))
	for _, k := range clientUAs {
		if strings.Contains(ua, k) {
			return false
		}
	}
	if strings.Contains(ua, "mozilla") || strings.Contains(ua, "applewebkit") ||
		strings.Contains(ua, "chrome") || strings.Contains(ua, "safari") ||
		strings.Contains(ua, "firefox") || strings.Contains(ua, "edg") {
		return true
	}
	return strings.Contains(strings.ToLower(c.GetHeader("Accept")), "text/html")
}

// ---------------------------------------------------------------- 链接收集

// collectSubLinks 把库里所有「可见节点 × 启用入站」翻成客户端链接。
// 地址只信库里的事实（ib.Address / node.Address），不回落到请求 Host。
// CollectSubLinks 是 collectSubLinks 的导出包装, 供 cmd/panel 的 CLI 子命令
// (`v2panel -url`) 打印链接全景用。返回的 subLink 字段全部是导出的,
// 因此包外可以正常读取 —— 只是类型名本身不需要暴露。
//
// 空凭据 = 原样返回 233boy 的链接 (CLI 全景视图用, 不是给某个用户的)。
func CollectSubLinks() ([]subLink, []string) { return collectSubLinks("") }

// collectSubLinks 把库里所有「可见节点 × 启用入站」翻成客户端链接。
// 地址只信库里的事实（ib.Address / node.Address），不回落到请求 Host。
//
// credential 非空时, 会把每条链接里的客户端凭据换成它 —— 见 agent.RebindShareURI
// 的注释: 233boy 的 `v2ray url` 给的是 clients[0] 的 UUID, 直接下发会让所有用户
// 都用同一个匿名凭据, per-user 计数器永远是 0, 限流形同虚设。
func collectSubLinks(credential string) ([]subLink, []string) {
	var nodes []Node
	DB.Where("hidden = ?", false).Order("id asc").Find(&nodes)

	var (
		out   []subLink
		warns []string
		used  = map[string]int{}
	)

	for _, n := range nodes {
		var ibs []Inbound
		DB.Where("node_id = ? AND enabled = ?", n.ID, true).Order("sort_idx asc, id asc").Find(&ibs)

		for i := range ibs {
			ib := ibs[i]
			uri := strings.TrimSpace(ib.URI)
			if uri == "" {
				// dokodemo-door / local-http 本来就没有客户端链接；也可能是还没同步过。
				continue
			}
			su, err := agent.ParseShareURI(uri)
			if err != nil {
				warns = append(warns, fmt.Sprintf("%s / %s 链接解析失败：%v", n.Name, ib.ConfName, err))
				continue
			}

			// —— 按用户重绑凭据 (真限流的最后一环) ——
			//
			// 不做这一步的话, 下发的 UUID 是 clients[0] 的, 所有用户共用一个匿名
			// 凭据, per-user 计数器永远是 0 —— 流量能连但记不到人头上, 限流是假的。
			// 详见 agent.RebindShareURI 顶部注释。
			//
			// 不支持的协议 (socks/http 这类 conf 里用 accounts[] 的) 直接**跳过**:
			// 机器上根本没有"每用户一条凭据"的概念, 发出去也记不到账,
			// 留着它只会让用户以为自己有条能计量的线路。
			if credential != "" {
				if !agent.RebindableScheme(su.Scheme) {
					continue
				}
				rebound, rerr := agent.RebindShareURI(su, credential)
				if rerr != nil {
					warns = append(warns, fmt.Sprintf("%s / %s 按用户重绑凭据失败，已跳过：%v", n.Name, ib.ConfName, rerr))
					continue
				}
				uri = rebound
				// Raw 也要跟着换 —— clashProxy / singbox 渲染读的是 Raw.UUID 等字段,
				// 只换 Link 字符串会让 YAML/JSON 里还是老凭据 (两套输出不一致)。
				su.UUID = credential
				su.Password = credential
			}

			addr := firstNonEmpty(strings.TrimSpace(ib.Address), strings.TrimSpace(su.Address), strings.TrimSpace(n.Address))
			port := ib.PublicPort
			if port <= 0 {
				port = su.Port
			}
			if addr == "" || port <= 0 || port > 65535 {
				warns = append(warns, fmt.Sprintf("%s / %s 缺少可用的公网地址或端口，已跳过", n.Name, ib.ConfName))
				continue
			}

			portType := "公网直连"
			if ib.ListenAddr == common.ListenLoopback {
				portType = "Caddy 反代"
			}

			proto := subProtoLabel(ib, su)
			name := uniqueName(used, n.Name+" · "+proto)

			l := subLink{
				Name:     name,
				NodeName: n.Name,
				ConfName: ib.ConfName,
				Protocol: proto,
				Link:     uri,
				Address:  addr,
				Port:     port,
				PortType: portType,
				Raw:      su,
			}
			if ws := ib.Warnings(); len(ws) > 0 {
				l.Description = strings.Join(ws, "；")
			}
			if n.Status != "online" {
				l.Description = strings.TrimSpace("节点当前离线（配置仍可导入）；" + l.Description)
			}
			out = append(out, l)
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].NodeName != out[j].NodeName {
			return out[i].NodeName < out[j].NodeName
		}
		return out[i].ConfName < out[j].ConfName
	})
	return out, warns
}

func subProtoLabel(ib Inbound, su *agent.ShareURI) string {
	p := strings.ToUpper(firstNonEmpty(ib.Protocol, su.Scheme))
	if p == "" {
		p = "NODE"
	}
	net := strings.ToLower(firstNonEmpty(ib.Network, su.Network))
	if net != "" && net != "tcp" {
		p += "-" + strings.ToUpper(net)
	}
	if strings.EqualFold(su.Security, "tls") || strings.EqualFold(ib.ClientSecurity, "tls") {
		p += "-TLS"
	}
	return p
}

// uniqueName 同名节点会被 Clash / sing-box 直接拒绝整份配置，必须去重。
func uniqueName(used map[string]int, base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "node"
	}
	if n, ok := used[base]; ok {
		used[base] = n + 1
		return fmt.Sprintf("%s #%d", base, n+1)
	}
	used[base] = 1
	return base
}

// ---------------------------------------------------------------- Base64 出口

func generateBase64Links(links []subLink) string {
	var b strings.Builder
	for _, l := range links {
		b.WriteString(l.Link)
		b.WriteString("\n")
	}
	return base64.StdEncoding.EncodeToString([]byte(b.String()))
}

// ---------------------------------------------------------------- 极简 YAML 发射器
//
// Clash 配置用 encoding/json 出不来（要 YAML），引第三方 yaml 库又得改 go.mod。
// 这里只需要输出自己构造的结构，一个 60 行的发射器足够，且字符串一律双引号包裹，不会踩 YAML 的坑。

type yamlKV struct {
	K string
	V any
}

type yamlMap []yamlKV

func quoteYAML(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", "", "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

func yamlScalar(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return quoteYAML(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return quoteYAML(fmt.Sprint(t))
	}
}

// emitNode 渲染一个值。调用方已经写完 "key:" 且缓冲区末尾没有换行。
// indent 是"当前 key 所在层级"，顶层传 -1 即可让顶层 key 落在第 0 列。
func emitNode(b *strings.Builder, indent int, v any) {
	switch t := v.(type) {
	case yamlMap:
		if len(t) == 0 {
			b.WriteString(" {}\n")
			return
		}
		b.WriteString("\n")
		for _, kv := range t {
			b.WriteString(strings.Repeat("  ", indent+1))
			b.WriteString(kv.K)
			b.WriteString(":")
			emitNode(b, indent+1, kv.V)
		}
	case []yamlMap:
		if len(t) == 0 {
			b.WriteString(" []\n")
			return
		}
		b.WriteString("\n")
		pad := strings.Repeat("  ", indent+1)
		for _, m := range t {
			if len(m) == 0 {
				b.WriteString(pad + "- {}\n")
				continue
			}
			b.WriteString(pad + "- ")
			for i, kv := range m {
				if i > 0 {
					b.WriteString(pad + "  ")
				}
				b.WriteString(kv.K)
				b.WriteString(":")
				emitNode(b, indent+2, kv.V)
			}
		}
	case []string:
		if len(t) == 0 {
			b.WriteString(" []\n")
			return
		}
		b.WriteString("\n")
		pad := strings.Repeat("  ", indent+1)
		for _, s := range t {
			b.WriteString(pad + "- ")
			b.WriteString(yamlScalar(s))
			b.WriteString("\n")
		}
	default:
		b.WriteString(" ")
		b.WriteString(yamlScalar(v))
		b.WriteString("\n")
	}
}

// ---------------------------------------------------------------- Clash / Mihomo

func clashProxy(l subLink) (yamlMap, bool) {
	su := l.Raw
	if su == nil || l.Address == "" || l.Port <= 0 {
		return nil, false
	}
	net := strings.ToLower(firstNonEmpty(su.Network, ""))
	tlsOn := strings.EqualFold(su.Security, "tls")
	host := firstNonEmpty(su.Host, su.SNI)

	// ws / h2 / grpc 三个走 Caddy 的传输层选项是共用的。
	netOpts := func() (yamlMap, bool) {
		switch net {
		case "ws":
			ws := yamlMap{{"path", su.Path}}
			if host != "" {
				ws = append(ws, yamlKV{"headers", yamlMap{{"Host", host}}})
			}
			return yamlMap{{"network", "ws"}, {"ws-opts", ws}}, true
		case "h2", "http":
			h2 := yamlMap{{"path", su.Path}}
			if host != "" {
				h2 = append(h2, yamlKV{"host", []string{host}})
			}
			return yamlMap{{"network", "h2"}, {"h2-opts", h2}}, true
		case "grpc":
			return yamlMap{{"network", "grpc"}, {"grpc-opts", yamlMap{{"grpc-service-name", su.Path}}}}, true
		}
		return nil, false
	}

	switch su.Scheme {
	case "vmess":
		m := yamlMap{
			{"name", l.Name}, {"type", "vmess"}, {"server", l.Address}, {"port", l.Port},
			{"uuid", su.UUID}, {"alterId", 0}, {"cipher", "auto"}, {"udp", true},
		}
		switch net {
		case "ws", "h2", "http", "grpc":
			opts, _ := netOpts()
			m = append(m, opts...)
			m = append(m, yamlKV{"tls", true})
			if host != "" {
				m = append(m, yamlKV{"servername", host})
			}
		case "kcp":
			// ⚠️ 233boy 给 kcp 的 seed 是随机值，conf 层才有；解析器只取了 header.type。
			// seed 不匹配就握手失败，所以这里只能给出 header，能连上算运气。
			m = append(m, yamlKV{"network", "kcp"}, yamlKV{"kcp-opts", yamlMap{{"header", yamlMap{{"type", firstNonEmpty(su.Path, "none")}}}}})
		case "quic":
			m = append(m, yamlKV{"network", "quic"})
		default:
			m = append(m, yamlKV{"network", "tcp"})
		}
		m = append(m, yamlKV{"skip-cert-verify", false})
		return m, true

	case "vless":
		m := yamlMap{
			{"name", l.Name}, {"type", "vless"}, {"server", l.Address}, {"port", l.Port},
			{"uuid", su.UUID}, {"udp", true},
		}
		if opts, ok := netOpts(); ok {
			m = append(m, opts...)
			m = append(m, yamlKV{"tls", true})
		} else {
			m = append(m, yamlKV{"network", "tcp"}, yamlKV{"tls", tlsOn})
		}
		if host != "" {
			m = append(m, yamlKV{"servername", host})
		}
		m = append(m, yamlKV{"skip-cert-verify", false})
		return m, true

	case "trojan":
		m := yamlMap{
			{"name", l.Name}, {"type", "trojan"}, {"server", l.Address}, {"port", l.Port},
			{"password", firstNonEmpty(su.UUID, su.Password)}, {"udp", true},
		}
		if opts, ok := netOpts(); ok {
			m = append(m, opts...)
		}
		if host != "" {
			m = append(m, yamlKV{"sni", host})
		}
		m = append(m, yamlKV{"skip-cert-verify", false})
		return m, true

	case "ss":
		return yamlMap{
			{"name", l.Name}, {"type", "ss"}, {"server", l.Address}, {"port", l.Port},
			{"cipher", su.Method}, {"password", su.Password}, {"udp", true},
		}, true

	case "socks":
		m := yamlMap{
			{"name", l.Name}, {"type", "socks5"}, {"server", l.Address}, {"port", l.Port},
			{"udp", true},
		}
		if su.UUID != "" {
			m = append(m, yamlKV{"username", su.UUID})
		}
		if su.Password != "" {
			m = append(m, yamlKV{"password", su.Password})
		}
		return m, true
	}
	return nil, false
}

func generateClashYAML(links []subLink) string {
	var proxies []yamlMap
	var names []string
	for _, l := range links {
		p, ok := clashProxy(l)
		if !ok {
			continue
		}
		proxies = append(proxies, p)
		names = append(names, l.Name)
	}

	root := yamlMap{
		{"port", 7890},
		{"socks-port", 7891},
		{"allow-lan", false},
		{"mode", "rule"},
		{"log-level", "info"},
		{"external-controller", "127.0.0.1:9090"},
		{"dns", yamlMap{
			{"enable", true},
			{"ipv6", false},
			{"enhanced-mode", "fake-ip"},
			{"fake-ip-range", "198.18.0.1/16"},
			{"nameserver", []string{"223.5.5.5", "119.29.29.29", "8.8.8.8"}},
		}},
	}

	if len(proxies) == 0 {
		root = append(root,
			yamlKV{"proxies", []yamlMap{}},
			yamlKV{"proxy-groups", []yamlMap{}},
			yamlKV{"rules", []string{"MATCH,DIRECT"}},
		)
	} else {
		groupMembers := append([]string{"AUTO"}, names...)
		root = append(root,
			yamlKV{"proxies", proxies},
			yamlKV{"proxy-groups", []yamlMap{
				{{"name", "PROXIES"}, {"type", "select"}, {"proxies", groupMembers}},
				{{"name", "AUTO"}, {"type", "url-test"}, {"url", "http://www.gstatic.com/generate_204"},
					{"interval", 300}, {"tolerance", 50}, {"proxies", names}},
			}},
			yamlKV{"rules", []string{"GEOIP,CN,DIRECT", "MATCH,PROXIES"}},
		)
	}

	var b strings.Builder
	b.WriteString("# v2panel subscription (233boy v2ray)\n")
	b.WriteString("# generated: " + time.Now().Format(time.RFC3339) + "\n")
	emitNode(&b, -1, root)
	return b.String()
}

// ---------------------------------------------------------------- sing-box

type sbTransport struct {
	Type        string            `json:"type"`
	Path        string            `json:"path,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Host        []string          `json:"host,omitempty"`
	ServiceName string            `json:"service_name,omitempty"`
}

type sbTLS struct {
	Enabled    bool   `json:"enabled"`
	ServerName string `json:"server_name,omitempty"`
	Insecure   bool   `json:"insecure,omitempty"`
}

type sbOutbound struct {
	Type       string       `json:"type"`
	Tag        string       `json:"tag"`
	Server     string       `json:"server,omitempty"`
	ServerPort int          `json:"server_port,omitempty"`
	UUID       string       `json:"uuid,omitempty"`
	Password   string       `json:"password,omitempty"`
	Method     string       `json:"method,omitempty"`
	Username   string       `json:"username,omitempty"`
	Version    string       `json:"version,omitempty"`
	Security   string       `json:"security,omitempty"`
	AlterID    *int         `json:"alter_id,omitempty"`
	TLS        *sbTLS       `json:"tls,omitempty"`
	Transport  *sbTransport `json:"transport,omitempty"`
	Outbounds  []string     `json:"outbounds,omitempty"`
	URL        string       `json:"url,omitempty"`
	Interval   string       `json:"interval,omitempty"`
	Tolerance  int          `json:"tolerance,omitempty"`
	Default    string       `json:"default,omitempty"`
}

func singboxTransport(su *agent.ShareURI) (*sbTransport, bool) {
	net := strings.ToLower(su.Network)
	host := firstNonEmpty(su.Host, su.SNI)
	switch net {
	case "ws":
		t := &sbTransport{Type: "ws", Path: su.Path}
		if host != "" {
			t.Headers = map[string]string{"Host": host}
		}
		return t, true
	case "h2", "http":
		t := &sbTransport{Type: "http", Path: su.Path}
		if host != "" {
			t.Host = []string{host}
		}
		return t, true
	case "grpc":
		return &sbTransport{Type: "grpc", ServiceName: su.Path}, true
	case "quic":
		return &sbTransport{Type: "quic"}, true
	}
	return nil, false
}

// singboxOutbound 返回 (出站, 是否可表达)。
// sing-box 没有 mKCP 传输，233boy 的 kcp 节点在这里表达不出来 —— 返回 false 跳过。
func singboxOutbound(l subLink) (*sbOutbound, bool) {
	su := l.Raw
	if su == nil || l.Address == "" || l.Port <= 0 {
		return nil, false
	}
	net := strings.ToLower(su.Network)
	host := firstNonEmpty(su.Host, su.SNI)
	alterID := 0

	switch su.Scheme {
	case "vmess":
		o := &sbOutbound{
			Type: "vmess", Tag: l.Name, Server: l.Address, ServerPort: l.Port,
			UUID: su.UUID, Security: "auto", AlterID: &alterID,
		}
		switch net {
		case "ws", "h2", "http", "grpc", "quic":
			tr, _ := singboxTransport(su)
			o.Transport = tr
			o.TLS = &sbTLS{Enabled: true, ServerName: host}
		case "kcp":
			return nil, false // sing-box 无 mKCP 传输
		}
		return o, true

	case "vless":
		o := &sbOutbound{Type: "vless", Tag: l.Name, Server: l.Address, ServerPort: l.Port, UUID: su.UUID}
		if tr, ok := singboxTransport(su); ok {
			o.Transport = tr
			o.TLS = &sbTLS{Enabled: true, ServerName: host}
		}
		return o, true

	case "trojan":
		o := &sbOutbound{
			Type: "trojan", Tag: l.Name, Server: l.Address, ServerPort: l.Port,
			Password: firstNonEmpty(su.UUID, su.Password),
			TLS:      &sbTLS{Enabled: true, ServerName: host},
		}
		if tr, ok := singboxTransport(su); ok {
			o.Transport = tr
		}
		return o, true

	case "ss":
		return &sbOutbound{
			Type: "shadowsocks", Tag: l.Name, Server: l.Address, ServerPort: l.Port,
			Method: su.Method, Password: su.Password,
		}, true

	case "socks":
		o := &sbOutbound{
			Type: "socks", Tag: l.Name, Server: l.Address, ServerPort: l.Port,
			Version: "5", Username: su.UUID, Password: su.Password,
		}
		return o, true
	}
	return nil, false
}

func generateSingboxJSON(links []subLink) string {
	var (
		proxies []*sbOutbound
		names   []string
	)
	for _, l := range links {
		o, ok := singboxOutbound(l)
		if !ok {
			continue
		}
		proxies = append(proxies, o)
		names = append(names, l.Name)
	}

	outbounds := []any{}
	final := "direct"
	if len(proxies) > 0 {
		groupMembers := append([]string{"AUTO"}, names...)
		outbounds = append(outbounds,
			map[string]any{"type": "selector", "tag": "PROXIES", "outbounds": groupMembers, "default": "AUTO"},
			map[string]any{"type": "urltest", "tag": "AUTO", "outbounds": names,
				"url": "http://www.gstatic.com/generate_204", "interval": "5m", "tolerance": 50},
		)
		for _, p := range proxies {
			outbounds = append(outbounds, p)
		}
		final = "PROXIES"
	}
	outbounds = append(outbounds, map[string]any{"type": "direct", "tag": "direct"})

	cfg := map[string]any{
		"log": map[string]any{"level": "info", "timestamp": true},
		"dns": map[string]any{"servers": []string{"223.5.5.5", "119.29.29.29"}, "strategy": "prefer_ipv4"},
		"inbounds": []map[string]any{
			{"type": "mixed", "tag": "mixed-in", "listen": "127.0.0.1", "listen_port": 2080},
		},
		"outbounds": outbounds,
		"route": map[string]any{
			"auto_detect_interface": true,
			"rules": []map[string]any{
				{"ip_is_private": true, "outbound": "direct"},
			},
			"final": final,
		},
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return `{"error":"failed to build sing-box config"}`
	}
	return string(data) + "\n"
}

// ---------------------------------------------------------------- 门户页

type subPortalData struct {
	Username   string
	SubToken   string
	IsAuth     bool
	HasError   bool
	RequirePwd bool
	Expired    bool
	OverQuota  bool
	UsedText   string
	LimitText  string
	ExpireText string
	Percent    float64
	// 套餐与重置信息 (真限流后用户需要知道"什么时候流量会回来")
	PlanName   string
	ResetText  string
	DeviceText string
	// Announcements 当前生效的公告
	Announcements []Announcement
	Over          bool
	Links         []subLink
	Warnings      []string
	NodeCount     int
	BaseURL       string
	SubURL        string
	PwdEsc        string
	Now           string
	// 服务器信息, 让用户确认"我连的是哪台"。
	Host string
}

func subBaseURL(c *gin.Context) string {
	if d := strings.TrimSpace(GetSetting("panel_domain", "")); d != "" {
		if strings.HasPrefix(d, "http://") || strings.HasPrefix(d, "https://") {
			return strings.TrimRight(d, "/")
		}
		return "https://" + strings.TrimRight(d, "/")
	}
	if b, ok := panelBaseURL(c); ok {
		return b
	}
	return "http://127.0.0.1"
}

func renderSubPortal(c *gin.Context, u *User, links []subLink, warns []string, isAuth, authErr, expired, overQuota bool) {
	limitText := humanLimit(u.TrafficLimit)
	usedText := HumanBytes(u.UsedTraffic)
	expireText := "永久"
	if u.ExpireAt > 0 {
		expireText = time.Unix(u.ExpireAt, 0).Format("2006-01-02")
	}
	var pct float64
	if u.TrafficLimit > 0 {
		pct = float64(u.UsedTraffic) / float64(u.TrafficLimit) * 100
		if pct > 100 {
			pct = 100
		}
	}

	base := subBaseURL(c)
	subURL := base + "/sub/" + u.SubToken
	data := subPortalData{
		Username:      u.Username,
		SubToken:      u.SubToken,
		IsAuth:        isAuth,
		HasError:      authErr,
		RequirePwd:    strings.TrimSpace(GetSetting("sub_require_password", "0")) == "1",
		Expired:       expired,
		OverQuota:     overQuota,
		UsedText:      usedText,
		LimitText:     limitText,
		ExpireText:    expireText,
		Percent:       pct,
		PlanName:      u.PlanName,
		ResetText:     resetModeText(u),
		DeviceText:    deviceLimitText(u.DeviceLimit),
		Announcements: ActiveAnnouncements(),
		Over:          pct >= 80,
		Links:         links,
		Warnings:      warns,
		NodeCount:     len(links),
		BaseURL:       base,
		SubURL:        subURL,
		PwdEsc:        url.QueryEscape(u.Password),
		Now:           time.Now().Format("2006-01-02 15:04:05"),
		Host:          subPortalHost(links),
	}

	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store, no-cache, must-revalidate")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Referrer-Policy", "no-referrer")
	c.Status(http.StatusOK)
	if err := subPortalTmpl.Execute(c.Writer, data); err != nil {
		// 模板已经写出部分内容，只能记日志，不能再改状态码。
		fmt.Fprintf(c.Writer, "\n<!-- render error: %v -->\n", err)
	}
}

var subPortalTmpl = template.Must(template.New("sub-portal").Parse(subPortalHTML))

// subPortalHost 取一个节点地址做展示, 让用户能确认"我连的是哪台机器"。
// 全部节点地址一致时给单个值, 否则给"N 台服务器"。
func subPortalHost(links []subLink) string {
	set := map[string]bool{}
	for _, l := range links {
		if a := strings.TrimSpace(l.Address); a != "" {
			set[a] = true
		}
	}
	if len(set) == 1 {
		for a := range set {
			return a
		}
	}
	if len(set) > 1 {
		return fmt.Sprintf("%d 台服务器", len(set))
	}
	return "—"
}

// subPortalHTML 从 sub_portal.html 嵌入。
//
// 单独放一个文件而不是写成 Go 字符串字面量: 这页有 400 多行 HTML/CSS/JS,
// 塞进反引号字符串里没法高亮、没法格式化, 编辑一次错一次。
//
//go:embed sub_portal.html
var subPortalHTML string
