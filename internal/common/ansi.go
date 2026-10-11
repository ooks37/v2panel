package common

import (
	"regexp"
	"strings"
)

// ansiRe 匹配 CSI / OSC / 字符集切换等 ANSI 转义序列。
// 必要性: `v2ray url` 把分享链接包成 \x1b[45mvless://…\x1b[0m,
// 不剥离的话 `^[a-z0-9]*://` 这类匹配会全部落空 (实测 grep -c 返回 0)。
var ansiRe = regexp.MustCompile("\x1b(?:\\[[0-9;?]*[ -/]*[@-~]|\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|[()][A-B0-2])")

// StripANSI 剥离字符串里的全部 ANSI 转义序列。
func StripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	return ansiRe.ReplaceAllString(s, "")
}

// CleanOutput 剥离 ANSI 并统一换行 (233boy 输出混用 \r\n, 且进度条用 \r 覆写同行)。
func CleanOutput(s string) string {
	s = StripANSI(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}

// NodeURI 一条节点分享链接及其来源配置文件名
type NodeURI struct {
	ConfName string // VLESS-WS-TLS-node1.example.com.json
	URI      string // vless://11111111-...@node1.example.com:55509?...
}

var (
	// ------------- VLESS-WS-TLS-node1.example.com.json & URL 链接 -------------
	frameStartRe = regexp.MustCompile(`^-{5,}\s*(.+?)\s*&\s*URL\s*链接\s*-{5,}$`)
	// ------------- END -------------
	frameEndRe = regexp.MustCompile(`^-{5,}\s*END\s*-{5,}$`)
	// 只认这些 scheme, 避免把 footer 的 https://t.me/... 误当节点
	uriSchemeRe = regexp.MustCompile(`^(vless|vmess|trojan|ss|ssr|socks|http|hysteria2|hy2|tuic)://`)
)

// ParseURLOutput 解析 `v2ray url` 的整段输出为节点链接列表。
//
// 实测输出形态 (每个节点一帧):
//
//	------------- VLESS-WS-TLS-node1.example.com.json & URL 链接 -------------
//	vless://11111111-...@node1.example.com:55509?encryption=none&security=tls&...
//	------------- END -------------
//	关注(tg): https://t.me/tg2333
//	文档(doc): https://233boy.com/v2ray/v2ray-script/
//	推广(ads): https://getjms.com/
//
// 帧外的 footer 行一律忽略。多节点时按帧顺序返回。
func ParseURLOutput(raw string) []NodeURI {
	var out []NodeURI
	cur := ""
	inFrame := false

	for _, line := range strings.Split(CleanOutput(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if m := frameStartRe.FindStringSubmatch(line); m != nil {
			cur = strings.TrimSpace(m[1])
			inFrame = true
			continue
		}
		if frameEndRe.MatchString(line) {
			inFrame = false
			cur = ""
			continue
		}
		if !inFrame {
			continue
		}
		if uriSchemeRe.MatchString(line) {
			out = append(out, NodeURI{ConfName: cur, URI: line})
		}
	}
	return out
}

// FirstURIOf 返回指定配置文件名对应的第一条链接 (找不到返回空串)
func FirstURIOf(uris []NodeURI, confName string) string {
	for _, u := range uris {
		if u.ConfName == confName {
			return u.URI
		}
	}
	return ""
}

// URIScheme 取链接的 scheme (小写), 失败返回空串
func URIScheme(uri string) string {
	i := strings.Index(uri, "://")
	if i <= 0 {
		return ""
	}
	return strings.ToLower(uri[:i])
}

// URITag 取链接的 # 注释 (233boy 用它放 "233boy-ws-<域名>"), 无则空串
func URITag(uri string) string {
	if i := strings.Index(uri, "#"); i >= 0 {
		return uri[i+1:]
	}
	return ""
}
