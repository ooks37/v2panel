package agent

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// 把分享链接里的客户端凭据换成**指定用户**的凭据。
//
// ─────────────────────────────────────────────────────────────────────────
// 为什么必须有这一步 (这是整个「真限流」链路的最后一环)
// ─────────────────────────────────────────────────────────────────────────
// 233boy 的 `v2ray url` 输出的链接里, 凭据是 **clients[0].id** ——
// 也就是 233boy 自己写进 conf 的那个原始 UUID。
//
// 而面板给每个订阅用户分配了各自的 UUID, 并让 agent 把它们写进
// clients[1..n]。如果直接把 `v2ray url` 的输出下发给用户:
//
//   用户拿到的 UUID = 11111111-...  (clients[0], 无 email)
//   conf 里 u1@v2panel 的 UUID = aaaaaaaa-...
//
//   客户端用 11111111 鉴权 → V2Ray 归到"无 email"的匿名桶
//   → `user>>>u1@v2panel>>>traffic>>>uplink` **永远是 0**
//   → User.UsedTraffic 永不增长 → 没人超额 → 没人被熔断
//
// 也就是说: 流量能连, 但**记不到任何人头上**。限流是假的。
//
// 所以下发前必须按用户重绑凭据。这一步做完, 链路才闭合:
//
//   订阅链接带 u1 的 UUID → 客户端用它鉴权 → V2Ray 按 email 计数
//   → agent 读回增量 → 面板累加 → 超额 → 摘掉 → 断线
//
// ⚠️ 只重绑 usersync 真正管理的协议 (vless/vmess/trojan/ss)。
// socks/http 在 conf 里用 accounts[] 而非 clients[], agent 的用户同步会跳过它们,
// 机器上根本没有"每个用户一条凭据"的概念 —— 硬换凭据只会把用户连不上。

// RebindableScheme 判断某协议能否按用户重绑凭据。
//
// 与 usersync.go 的 clientIDField 必须保持一致: 那里能写 clients[] 的协议,
// 这里才能重绑。改了其中一个必须同步改另一个。
func RebindableScheme(scheme string) bool {
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case "vless", "vmess", "trojan", "ss":
		return true
	}
	return false
}

// RebindShareURI 把分享链接里的客户端凭据替换成 credential。
//
// credential 一律取 User.UUID: usersync 写入 conf 的也是这个值
// (trojan/ss 写进 `password` 字段, vless/vmess 写进 `id` 字段),
// 两边必须同源, 否则用户连不上。
//
// 保留链接里除凭据外的一切 (地址/端口/path/host/sni/flow/tag/ss 的 method),
// 因为那些是 233boy 与 Caddy 层的事实, 面板无权改动。
func RebindShareURI(su *ShareURI, credential string) (string, error) {
	if su == nil {
		return "", fmt.Errorf("链接为空")
	}
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return "", fmt.Errorf("凭据为空")
	}
	switch strings.ToLower(su.Scheme) {
	case "vmess":
		return rebindVmess(su.URI, credential)
	case "ss":
		return rebindSS(su, credential)
	case "vless", "trojan":
		return rebindStd(su.URI, credential)
	default:
		return "", fmt.Errorf("协议 %q 不支持按用户重绑凭据", su.Scheme)
	}
}

// rebindStd 处理 `scheme://user@host:port?query#tag` 形态 (vless / trojan)。
//
// 用 url.Parse + 改 User 再 String(): 这样 query 的转义、fragment 的编码
// 都由标准库按原样保留, 不用手工拼串 (手工拼最容易把 path 里的 %2F 弄丢)。
func rebindStd(uri, credential string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("解析链接失败: %w", err)
	}
	// vless/trojan 的凭据放在 userinfo 的用户名位 (233boy 不给它配密码位)。
	u.User = url.User(credential)
	return u.String(), nil
}

// rebindVmess 处理 `vmess://base64(JSON)`。
//
// 解成 map 再改 id: 用 map 而不是结构体, 是为了**原样保留未知字段**
// (aid / scy / 客户端私有字段)。用结构体重编码会把它们静默丢掉,
// 那等于每次下发都在降级别人的配置。
func rebindVmess(uri, credential string) (string, error) {
	body := strings.TrimPrefix(uri, "vmess://")
	suffix := ""
	if i := strings.IndexAny(body, "#?"); i >= 0 {
		suffix = body[i:]
		body = body[:i]
	}
	raw, err := decodeB64Loose(body)
	if err != nil {
		return "", fmt.Errorf("vmess base64 解码失败: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", fmt.Errorf("vmess JSON 解析失败: %w", err)
	}
	m["id"] = credential
	out, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("vmess JSON 编码失败: %w", err)
	}
	// 用 StdEncoding + padding: v2rayN / v2rayNG / Shadowrocket 都认这个形态,
	// 而 233boy 自己产出的也是它。URL-safe 变体反而有客户端不认。
	return "vmess://" + base64.StdEncoding.EncodeToString(out) + suffix, nil
}

// rebindSS 处理 SIP002 的 `ss://base64(method:password)@host:port?query#tag`。
//
// ss 的凭据位是 password (usersync 写入 conf 的也是 password 字段),
// method 属于节点事实, 原样保留。
func rebindSS(su *ShareURI, credential string) (string, error) {
	if su.Address == "" || su.Port <= 0 {
		return "", fmt.Errorf("ss 链接缺少地址或端口")
	}
	if su.Method == "" {
		return "", fmt.Errorf("ss 链接缺少加密方式")
	}
	userinfo := base64.RawURLEncoding.EncodeToString([]byte(su.Method + ":" + credential))
	q := ""
	if len(su.Query) > 0 {
		q = "?" + su.Query.Encode()
	}
	tag := ""
	if su.Tag != "" {
		tag = "#" + url.QueryEscape(su.Tag)
	}
	return fmt.Sprintf("ss://%s@%s:%d%s%s", userinfo, su.Address, su.Port, q, tag), nil
}
