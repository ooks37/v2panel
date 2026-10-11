// Package agent 实现 v2panel 的节点侧能力。
//
// 本文件 (parser.go) 是 M1 的「三层合并解析器」。
// 233boy 的一个节点, 信息分散在三个互不重叠的来源里, 任何单一来源都不完整:
//
//	① conf 层  /etc/v2ray/conf/<name>.json
//	   有: 内网回环口 (listen 127.0.0.1:<port>)、协议、UUID/密码/加密方式、
//	       入站 streamSettings (network / security / ws path / Host)
//	   缺: 公网入口 —— 只有回环口, 客户端根本连不上。
//
//	② Caddy 层 /etc/caddy/233boy/<域名>.conf
//	   有: 公网域名 + 公网端口 + 「路径 → 内网口」的反代映射
//	   缺: 协议与凭据 —— 只有一条路由。
//
//	③ url 层   `v2ray url`
//	   有: 客户端分享链接 —— Address / PublicPort / Host / Path / security 的权威值
//	   缺: 内网口与 listen 地址 (链接里不体现)。
//
// 合并规则因此是固定的:
//
//	Address / PublicPort / Host / Path / ClientSecurity  ← 以 url 层为准
//	UUID / Password / Method / InternalPort / ListenAddr ← 以 conf 层为准
//	「公网口 ↔ 内网口」的映射 (用于校验与排障)            ← 以 Caddy 层为准
//
// # 三层的连接键各不相同 (这是本文件最容易写错的地方)
//
//	conf ↔ caddy  按「内网端口」join (caddy reverse_proxy 的 upstream 口 == conf 的 port)
//	conf ↔ url    按「conf 文件名」join (url 输出里 `--- <name>.json & URL 链接 ---` 的帧头)
//	caddy ↔ url   按「路径」join (caddy matcher 的 /<uuid> == url 链接的 path=)
//
// 不能用域名 join conf↔caddy: caddy 文件名是域名, 而 conf 文件名是协议+域名,
// 两者虽相关但随协议变化 (VMess-WS-tls 与 VLESS-WS-TLS 同域不同名), 端口才是稳定键。
//
// # ⚠️ 公网端口是「机器级」的, 不是节点级
//
// Caddy 的 https_port 整机唯一 (实测 82 上为 55509)。同机多节点共享 host:55509,
// 只靠 path 区分。所以 **path 才是这里的真正主键**, 绝不能假设「一节点一公网口」。
//
// # ⚠️ conf 的 security:none 与 URL 的 security=tls 是设计如此, 不是 bug
//
// 233boy 的 create() 服务端分支无条件置 is_tls=none, 只有 client 分支才置 tls;
// 入站 streamSettings.security 与客户端链接里的 security= 是两回事 ——
// 真实链路是「客户端 TLS → Caddy:55509 (真证书) → 明文回环到内网口」, TLS 在 Caddy 终结。
// 因此本文件把两者分别存为 InboundSecurity / ClientSecurity, 不合并成一个字段。
package agent

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"v2panel/internal/common"
)

// ---------------------------------------------------------------- 路径

// LayerPaths 三层来源在磁盘上的位置 (可注入, 便于测试与多机复用)
type LayerPaths struct {
	ConfDir   string // /etc/v2ray/conf
	CaddyDir  string // /etc/caddy/233boy
	Caddyfile string // /etc/caddy/Caddyfile (取全局 https_port/http_port)
	NginxDir  string // /www/server/panel/vhost/nginx (仅探测遗留, 不写入)
}

// DefaultLayerPaths 返回 82 上实测的标准位置
func DefaultLayerPaths() LayerPaths {
	return LayerPaths{
		ConfDir:   common.DefaultConfDir,
		CaddyDir:  "/etc/caddy/233boy",
		Caddyfile: "/etc/caddy/Caddyfile",
		NginxDir:  "/www/server/panel/vhost/nginx",
	}
}

// ---------------------------------------------------------------- conf 层

// ConfNode conf 层解析结果 (一个 conf 文件恰好一个 inbound)
type ConfNode struct {
	ConfName string // 文件名, 同时也是 v2ray 的 inbound tag
	FilePath string
	Tag      string
	Listen   string // conf 的 inbound listen: ws/h2/grpc/http 为 127.0.0.1, 其余为 0.0.0.0 (core.sh:337-343)
	Port     int    // 内网口, 实测 45341
	Protocol string // vless / vmess / trojan / shadowsocks / socks / http / dokodemo-door

	Network  string // streamSettings.network: ws / tcp / grpc / h2 / kcp / quic
	Security string // streamSettings.security: 233boy 服务端恒为 "none"
	Host     string // wsSettings.headers.Host
	Path     string // wsSettings.path / httpSettings.path / grpcSettings.serviceName

	UUID     string
	Password string
	Method   string
	User     string // socks/http 的账号

	// 动态端口协议 (tcpd/kcpd/quicd) 会生成多个 <name>-dynamic-port-<n>.json,
	// 这些节点直连内网口对外暴露, 不经 Caddy —— 合并阶段据此标记, 不能当作错误。
	IsDynamicPort bool
	DynamicBase   string // 去掉 -dynamic-port-<n> 后的基名
}

// v2rayConfFile 只声明解析器关心的字段; 其余原样忽略 (不做完整 schema 校验,
// 因为 233boy 升级可能增字段, 我们不该因未知字段而失败)。
type v2rayConfFile struct {
	Inbounds []struct {
		Tag      string `json:"tag"`
		Port     any    `json:"port"`
		Listen   any    `json:"listen"`
		Protocol string `json:"protocol"`
		Settings struct {
			Clients []struct {
				ID       string `json:"id"`
				AlterID  any    `json:"alterId"`
				Password string `json:"password"`
				Method   string `json:"method"`
				Email    string `json:"email"`
				User     string `json:"user"`
				Pass     string `json:"pass"`
			} `json:"clients"`
			Accounts []struct {
				User string `json:"user"`
				Pass string `json:"pass"`
			} `json:"accounts"`
			Method     string `json:"method"`
			Password   string `json:"password"`
			Network    string `json:"network"`
			Address    string `json:"address"`
			Decryption string `json:"decryption"`
		} `json:"settings"`
		StreamSettings struct {
			Network    string `json:"network"`
			Security   string `json:"security"`
			WSSettings struct {
				Path    string            `json:"path"`
				Headers map[string]string `json:"headers"`
			} `json:"wsSettings"`
			HTTPSettings struct {
				Path string   `json:"path"`
				Host []string `json:"host"`
			} `json:"httpSettings"`
			GRPCSettings struct {
				ServiceName string `json:"serviceName"`
			} `json:"grpcSettings"`
			KCPSettings struct {
				Header struct {
					Type string `json:"type"`
				} `json:"header"`
				Seed string `json:"seed"`
			} `json:"kcpSettings"`
			TCPSettings struct {
				Header struct {
					Type string `json:"type"`
				} `json:"header"`
			} `json:"tcpSettings"`
			TLSSettings struct {
				ServerName string   `json:"serverName"`
				ALPN       []string `json:"alpn"`
			} `json:"tlsSettings"`
		} `json:"streamSettings"`
	} `json:"inbounds"`
}

// confNameToDynamicBase 拆出动态端口节点基名:
// "VMess-TCP-dynamic-port-23497.json" → ("VMess-TCP", true)
var dynamicPortRe = regexp.MustCompile(`^(.+)-dynamic-port-\d+\.json$`)

// ParseConfJSON 解析单个 conf 文件内容。
// confName 为文件名 (即 tag)。返回的 ConfNode 只含第一个 inbound —— 233boy 每个
// conf 文件恰好一个 inbound, 若出现多个则取第一个并在警告里提示。
func ParseConfJSON(confName, filePath string, data []byte) (*ConfNode, []string, error) {
	var f v2rayConfFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, nil, fmt.Errorf("解析 %s 失败: %w", confName, err)
	}
	if len(f.Inbounds) == 0 {
		return nil, nil, fmt.Errorf("%s 中没有 inbound", confName)
	}
	var warns []string
	if len(f.Inbounds) > 1 {
		warns = append(warns, fmt.Sprintf("%s 含 %d 个 inbound, 仅取第一个 (233boy 正常只写一个)", confName, len(f.Inbounds)))
	}
	ib := f.Inbounds[0]

	n := &ConfNode{
		ConfName: confName,
		FilePath: filePath,
		Tag:      ib.Tag,
		Protocol: strings.ToLower(ib.Protocol),
		Network:  strings.ToLower(ib.StreamSettings.Network),
		Security: strings.ToLower(ib.StreamSettings.Security),
	}
	if n.Tag == "" {
		n.Tag = confName // 233boy 的 tag 恒等于文件名, 缺失时兜底
	}
	n.Listen = anyToString(ib.Listen)
	n.Port = anyToInt(ib.Port)

	// 网络层伪装参数
	switch n.Network {
	case "ws":
		n.Path = ib.StreamSettings.WSSettings.Path
		if h, ok := ib.StreamSettings.WSSettings.Headers["Host"]; ok {
			n.Host = h
		}
	case "h2", "http":
		n.Path = ib.StreamSettings.HTTPSettings.Path
		if len(ib.StreamSettings.HTTPSettings.Host) > 0 {
			n.Host = ib.StreamSettings.HTTPSettings.Host[0]
		}
	case "grpc":
		n.Path = ib.StreamSettings.GRPCSettings.ServiceName
	case "kcp":
		n.Path = ib.StreamSettings.KCPSettings.Header.Type
	case "tcp":
		n.Path = ib.StreamSettings.TCPSettings.Header.Type
	}

	// 凭据: 各协议位置不同
	if len(ib.Settings.Clients) > 0 {
		c := ib.Settings.Clients[0]
		n.UUID = c.ID
		if c.Password != "" {
			n.Password = c.Password
		}
		if c.Method != "" {
			n.Method = c.Method
		}
		if c.User != "" {
			n.User = c.User
		}
		if n.UUID == "" && c.Pass != "" {
			n.Password = c.Pass
		}
	}
	if len(ib.Settings.Accounts) > 0 { // socks / http
		n.User = ib.Settings.Accounts[0].User
		n.Password = ib.Settings.Accounts[0].Pass
	}
	if ib.Settings.Password != "" { // shadowsocks 的顶层 password
		n.Password = ib.Settings.Password
	}
	if ib.Settings.Method != "" { // shadowsocks 的顶层 method
		n.Method = ib.Settings.Method
	}
	if n.UUID == "" && n.Password == "" {
		warns = append(warns, fmt.Sprintf("%s (%s) 未解析出任何凭据, 可能是无认证协议", confName, n.Protocol))
	}

	if m := dynamicPortRe.FindStringSubmatch(confName); m != nil {
		n.IsDynamicPort = true
		n.DynamicBase = m[1]
	}
	return n, warns, nil
}

// ParseConfDir 读取目录下全部 *.json。
// 刻意不过滤 dynamic-port-*: 那些是真实节点 (直连口), 由合并阶段归类, 不是垃圾文件。
func ParseConfDir(dir string) ([]ConfNode, []string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("读取 conf 目录 %s 失败: %w", dir, err)
	}
	var (
		nodes []ConfNode
		warns []string
		errs  []string
	)
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		// 233boy 的备份命名 <name>.json.bak.<时间戳> 不以 .json 结尾, 天然被排除。
		p := filepath.Join(dir, name)
		data, err := os.ReadFile(p)
		if err != nil {
			errs = append(errs, fmt.Sprintf("读取 %s 失败: %v", name, err))
			continue
		}
		n, w, err := ParseConfJSON(name, p, data)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		warns = append(warns, w...)
		nodes = append(nodes, *n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ConfName < nodes[j].ConfName })
	if len(errs) > 0 {
		warns = append(warns, errs...)
	}
	return nodes, warns, nil
}

// ---------------------------------------------------------------- Caddy 层

// CaddyRoute 一条「路径 → 内网口」的反代规则
type CaddyRoute struct {
	Path         string // /11111111-2222-4333-8444-555555555555
	Upstream     string // 127.0.0.1:45341
	UpstreamIP   string
	UpstreamPort int
}

// CaddySite 一个 Caddy 站点块 (= 一个域名, 通常对应一个节点)
type CaddySite struct {
	FileName string // node1.example.com.conf
	FilePath string
	SiteAddr string // node1.example.com:55509 (原样)
	Host     string // node1.example.com
	Port     int    // 55509 (机器级公网口; 缺省时由全局 https_port 兜底)
	Routes   []CaddyRoute
}

var (
	// 站点块首行: `host:port {` / `https://host:port {`
	caddySiteRe = regexp.MustCompile(`^\s*(?:(?:https?)://)?([^\s{]+)\s*\{\s*$`)
	// 反代行: `reverse_proxy <matcher?> <upstream...>`
	caddyProxyRe = regexp.MustCompile(`^\s*reverse_proxy\s+(.+?)\s*$`)
	// 从任意 token 里取 host:port
	caddyHostPortRe = regexp.MustCompile(`^([A-Za-z0-9._\-\[\]]+):(\d{1,5})$`)
	// 全局选项 https_port / http_port
	caddyGlobalPortRe = regexp.MustCompile(`(?m)^\s*(https_port|http_port)\s+(\d{1,5})\s*$`)
)

// ParseCaddyGlobal 从 /etc/caddy/Caddyfile 取全局端口。
// 必要性: 公网口是机器级的, 站点块里没写端口时 (或 url 层缺失时) 得靠它兜底。
func ParseCaddyGlobal(path string) (httpPort, httpsPort int, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	for _, m := range caddyGlobalPortRe.FindAllStringSubmatch(string(data), -1) {
		v, _ := strconv.Atoi(m[2])
		switch m[1] {
		case "https_port":
			httpsPort = v
		case "http_port":
			httpPort = v
		}
	}
	return httpPort, httpsPort, nil
}

// ParseCaddySite 解析单个 233boy 生成的站点 conf。
func ParseCaddySite(fileName, filePath, content string) (*CaddySite, []string, error) {
	site := &CaddySite{FileName: fileName, FilePath: filePath}
	var warns []string

	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if site.SiteAddr == "" {
			if m := caddySiteRe.FindStringSubmatch(line); m != nil {
				addr := m[1]
				// 排除 `handle {` / `encode {` 这类非站点块: 站点地址必须含 . 或 :
				if !strings.ContainsAny(addr, ".:") {
					continue
				}
				site.SiteAddr = addr
				host, port := splitHostPort(addr)
				site.Host, site.Port = host, port
				continue
			}
		}
		if m := caddyProxyRe.FindStringSubmatch(line); m != nil {
			fields := strings.Fields(m[1])
			var matchers, upstreams []string
			for i, f := range fields {
				// 首个以 / 或 @ 开头的 token 是 matcher, 其余是 upstream
				if i == 0 && (strings.HasPrefix(f, "/") || strings.HasPrefix(f, "@")) {
					matchers = append(matchers, f)
					continue
				}
				upstreams = append(upstreams, f)
			}
			if len(upstreams) == 0 {
				warns = append(warns, fmt.Sprintf("%s: reverse_proxy 无 upstream, 已跳过该行", fileName))
				continue
			}
			if len(matchers) == 0 {
				// 无 matcher = 该站点全部路径都转发, 用 "/*" 表示
				matchers = []string{"/*"}
			}
			for _, mt := range matchers {
				r := CaddyRoute{Path: mt, Upstream: upstreams[0]}
				if hm := caddyHostPortRe.FindStringSubmatch(upstreams[0]); hm != nil {
					r.UpstreamIP = hm[1]
					r.UpstreamPort, _ = strconv.Atoi(hm[2])
				} else {
					warns = append(warns, fmt.Sprintf("%s: 无法解析 upstream %q", fileName, upstreams[0]))
				}
				site.Routes = append(site.Routes, r)
			}
			if len(upstreams) > 1 {
				warns = append(warns, fmt.Sprintf("%s: 有 %d 个 upstream, 仅取第一个 (%s)", fileName, len(upstreams), upstreams[0]))
			}
		}
	}

	if site.SiteAddr == "" {
		return nil, warns, fmt.Errorf("%s: 未找到站点块首行", fileName)
	}
	if len(site.Routes) == 0 {
		warns = append(warns, fmt.Sprintf("%s: 站点 %s 没有任何反代规则 (可能是纯静态站点)", fileName, site.SiteAddr))
	}
	return site, warns, nil
}

// ParseCaddyDir 读取 /etc/caddy/233boy/ 下的站点。
//
// 必须跳过的两类文件 (实测 82 上同时存在, 不跳过会解析出幽灵节点):
//   - *.conf.add  内容是 `# see https://...` 一行注释 (被 .conf 用 import 引入)
//   - *.conf.bak.* 233boy 改配置时的备份
func ParseCaddyDir(dir string) ([]CaddySite, []string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("读取 Caddy 目录 %s 失败: %w", dir, err)
	}
	var sites []CaddySite
	var warns []string
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".conf") {
			continue
		}
		// 双保险: 后缀判断已排除 .conf.add / .conf.bak.*, 这里再挡一次显式命名
		if strings.Contains(name, ".bak.") || strings.HasSuffix(name, ".add") {
			continue
		}
		p := filepath.Join(dir, name)
		data, err := os.ReadFile(p)
		if err != nil {
			warns = append(warns, fmt.Sprintf("读取 %s 失败: %v", name, err))
			continue
		}
		s, w, err := ParseCaddySite(name, p, string(data))
		warns = append(warns, w...)
		if err != nil {
			warns = append(warns, err.Error())
			continue
		}
		sites = append(sites, *s)
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].FileName < sites[j].FileName })
	return sites, warns, nil
}

// ---------------------------------------------------------------- url 层

// ShareURI 一条分享链接的结构化视图
type ShareURI struct {
	Scheme   string
	URI      string
	UUID     string // vless/vmess 的 id, trojan 的密码
	Password string // ss 密码 / socks 密码
	Method   string // ss 加密方式
	Address  string
	Port     int
	Tag      string
	Query    url.Values

	Network  string // type= 参数
	Security string // security= 参数 (客户端视角, 与 conf 的 inbound security 不同)
	Host     string // host= 参数
	Path     string // path= 参数 (已 URL 解码)
	SNI      string
	Flow     string
}

// ParseShareURI 解析 233boy 产出的分享链接。
//
// 覆盖 vless/trojan/vmess/ss/socks: 前三者走标准 `scheme://user@host:port?q#tag`,
// vmess 走 base64(JSON), ss 走 SIP002 (base64 的 userinfo 或整体 base64)。
func ParseShareURI(uri string) (*ShareURI, error) {
	uri = strings.TrimSpace(uri)
	scheme := common.URIScheme(uri)
	if scheme == "" {
		return nil, fmt.Errorf("无法识别 scheme: %q", uri)
	}
	switch scheme {
	case "vmess":
		return parseVmessURI(uri)
	case "ss":
		return parseSSURI(uri)
	case "vless", "trojan", "socks", "http", "hysteria2", "hy2", "tuic":
		return parseStdURI(uri, scheme)
	default:
		return nil, fmt.Errorf("暂不支持的 scheme %q", scheme)
	}
}

func parseStdURI(uri, scheme string) (*ShareURI, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("解析链接失败: %w", err)
	}
	out := &ShareURI{
		Scheme:   scheme,
		URI:      uri,
		Address:  u.Hostname(),
		Query:    u.Query(),
		Tag:      u.Fragment,
		Network:  u.Query().Get("type"),
		Security: u.Query().Get("security"),
		Host:     u.Query().Get("host"),
		Path:     u.Query().Get("path"),
		SNI:      u.Query().Get("sni"),
		Flow:     u.Query().Get("flow"),
	}
	if p := u.Port(); p != "" {
		out.Port, _ = strconv.Atoi(p)
	}
	if u.User != nil {
		out.UUID = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			out.Password = pw
		}
	}
	if out.Path == "" {
		// 部分协议把 path 放在 URL 的 path 段而非 query
		out.Path = u.Path
	}
	return out, nil
}

// vmess 链接是 base64 的标准 JSON (v2rayN 格式)
func parseVmessURI(uri string) (*ShareURI, error) {
	body := strings.TrimPrefix(uri, "vmess://")
	if i := strings.IndexAny(body, "#?"); i >= 0 {
		body = body[:i]
	}
	raw, err := decodeB64Loose(body)
	if err != nil {
		return nil, fmt.Errorf("vmess base64 解码失败: %w", err)
	}
	var v struct {
		V    string `json:"v"`
		PS   string `json:"ps"`
		Add  string `json:"add"`
		Port any    `json:"port"`
		ID   string `json:"id"`
		Aid  any    `json:"aid"`
		Net  string `json:"net"`
		Type string `json:"type"`
		Host string `json:"host"`
		Path string `json:"path"`
		TLS  string `json:"tls"`
		SNI  string `json:"sni"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("vmess JSON 解析失败: %w", err)
	}
	out := &ShareURI{
		Scheme:   "vmess",
		URI:      uri,
		UUID:     v.ID,
		Address:  v.Add,
		Port:     anyToInt(v.Port),
		Tag:      v.PS,
		Network:  v.Net,
		Security: v.TLS,
		Host:     v.Host,
		Path:     v.Path,
		SNI:      v.SNI,
	}
	if out.Security == "" {
		out.Security = "none"
	}
	return out, nil
}

// ss 链接两种形态:
//
//	ss://base64(method:password)@host:port#tag      (SIP002)
//	ss://base64(method:password@host:port)#tag      (旧式)
func parseSSURI(uri string) (*ShareURI, error) {
	out := &ShareURI{Scheme: "ss", URI: uri, Security: "none"}
	body := strings.TrimPrefix(uri, "ss://")
	if i := strings.Index(body, "#"); i >= 0 {
		out.Tag, _ = url.QueryUnescape(body[i+1:])
		body = body[:i]
	}
	if i := strings.Index(body, "?"); i >= 0 {
		q, _ := url.ParseQuery(body[i+1:])
		out.Query = q
		body = body[:i]
	}
	if at := strings.LastIndex(body, "@"); at >= 0 {
		// SIP002: userinfo 是 base64(method:password)
		userinfo := body[:at]
		hostport := body[at+1:]
		if dec, err := decodeB64Loose(userinfo); err == nil {
			if c := strings.Index(string(dec), ":"); c >= 0 {
				out.Method = string(dec[:c])
				out.Password = string(dec[c+1:])
			}
		}
		out.Address, out.Port = splitHostPort(hostport)
		return out, nil
	}
	// 旧式: 整体 base64
	dec, err := decodeB64Loose(body)
	if err != nil {
		return nil, fmt.Errorf("ss 链接解码失败: %w", err)
	}
	s := string(dec)
	at := strings.LastIndex(s, "@")
	if at < 0 {
		return nil, fmt.Errorf("ss 链接格式无法识别: %q", uri)
	}
	cred, hostport := s[:at], s[at+1:]
	if c := strings.Index(cred, ":"); c >= 0 {
		out.Method, out.Password = cred[:c], cred[c+1:]
	}
	out.Address, out.Port = splitHostPort(hostport)
	return out, nil
}

// ---------------------------------------------------------------- 合并结果

// NodeView 三层合并后的一个完整节点视图。
// 每个字段都标注了权威来源, 便于排障时判断「这个值是谁给的」。
type NodeView struct {
	ConfName string
	ConfFile string

	// —— 以 conf 层为准 ——
	Protocol      string
	UUID          string
	Password      string
	Method        string
	User          string
	InternalPort  int    // 内网口 (conf 的 port)
	ListenAddr    string // conf 的 inbound listen: ws/h2/grpc/http 为 127.0.0.1, 其余为 0.0.0.0 (core.sh:337-343)
	InboundNet    string // conf 的 streamSettings.network
	InboundSec    string // conf 的 streamSettings.security (233boy 服务端恒为 none)
	IsDynamicPort bool

	// —— 以 url 层为准 ——
	Address        string // 客户端应填的地址
	PublicPort     int
	Host           string
	Path           string
	ClientSecurity string
	ClientNetwork  string
	URI            string

	// —— 以 Caddy 层为准 (公网↔内网的映射, 校验用) ——
	CaddyFile     string
	CaddySite     string
	CaddyHost     string
	CaddyPort     int
	CaddyUpstream int
	CaddyPath     string

	HasConf, HasCaddy, HasURL bool

	// 三层不一致时记录 (不阻断, 只提示 —— 233boy 允许手工改过 conf)
	Warnings []string
}

// Snapshot 一次完整采集的结果
type Snapshot struct {
	Nodes     []NodeView
	URIs      []common.NodeURI
	HTTPPort  int
	HTTPSPort int
	Warnings  []string
}

// Merge 三层合并。httpsPort 用于 Caddy 站点未显式写端口时兜底。
//
// 连接键: conf↔caddy 按内网端口, conf↔url 按 conf 文件名, caddy↔url 按路径。
func Merge(confs []ConfNode, sites []CaddySite, uris []common.NodeURI, httpsPort int) ([]NodeView, []string) {
	var warns []string

	// caddy 索引: 内网口 → 站点+路由; 路径 → 站点+路由
	type caddyHit struct {
		site  CaddySite
		route CaddyRoute
	}
	byUpstream := map[int]caddyHit{}
	byPath := map[string]caddyHit{}
	for _, s := range sites {
		for _, r := range s.Routes {
			if r.UpstreamPort > 0 {
				if _, dup := byUpstream[r.UpstreamPort]; dup {
					warns = append(warns, fmt.Sprintf("Caddy 内网口 %d 被多个站点反代 (可能重复配置)", r.UpstreamPort))
				}
				byUpstream[r.UpstreamPort] = caddyHit{s, r}
			}
			byPath[r.Path] = caddyHit{s, r}
		}
	}

	// url 索引: conf 文件名 → 链接
	byConf := map[string]common.NodeURI{}
	for _, u := range uris {
		byConf[u.ConfName] = u
	}

	// conf 内网口集合 —— 孤儿判定必须用它, 不能用 byUpstream。
	// ⚠️ byUpstream 是从 Caddy 站点自身建的, 拿它查 `byUpstream[r.UpstreamPort]` 恒为真,
	// 会让「孤儿反代」分支变成死代码 (单测 TestMerge_DetectsOrphans 已抓到)。
	confPorts := map[int]bool{}
	for _, c := range confs {
		if c.Port > 0 {
			confPorts[c.Port] = true
		}
	}

	usedCaddy := map[string]bool{}
	usedURI := map[string]bool{}

	var views []NodeView
	for _, c := range confs {
		v := NodeView{
			ConfName:      c.ConfName,
			ConfFile:      c.FilePath,
			Protocol:      c.Protocol,
			UUID:          c.UUID,
			Password:      c.Password,
			Method:        c.Method,
			User:          c.User,
			InternalPort:  c.Port,
			ListenAddr:    c.Listen,
			InboundNet:    c.Network,
			InboundSec:    c.Security,
			IsDynamicPort: c.IsDynamicPort,
			HasConf:       true,
		}

		// —— Caddy 层: 按内网口 join ——
		if hit, ok := byUpstream[c.Port]; ok {
			v.HasCaddy = true
			v.CaddyFile = hit.site.FileName
			v.CaddySite = hit.site.SiteAddr
			v.CaddyHost = hit.site.Host
			v.CaddyPort = hit.site.Port
			v.CaddyUpstream = hit.route.UpstreamPort
			v.CaddyPath = hit.route.Path
			usedCaddy[hit.site.FileName] = true
		} else if !c.IsDynamicPort && c.Listen == common.ListenLoopback {
			// 只有「必须经 Caddy」的协议 (ws/h2/grpc/http) 缺站点才算故障;
			// tcp/ss/socks/door 等监听 0.0.0.0, 本来就不该有 Caddy 站点, 不报。
			warns = append(warns, fmt.Sprintf("%s: 内网口 %d 在 Caddy 层没有对应反代 —— 该协议必须经 Caddy 转发, 客户端连不上", c.ConfName, c.Port))
		}

		// —— url 层: 按 conf 文件名 join ——
		if u, ok := byConf[c.ConfName]; ok {
			usedURI[c.ConfName] = true
			v.HasURL = true
			v.URI = u.URI
			if su, err := ParseShareURI(u.URI); err == nil {
				v.Address = su.Address
				v.PublicPort = su.Port
				v.ClientSecurity = su.Security
				v.ClientNetwork = su.Network
				v.Host = su.Host
				v.Path = su.Path
				// url 层是权威值, 但若链接里缺 (如 ss 不带 path), 用 conf/Caddy 兜底
				if v.Path == "" {
					v.Path = firstNonEmpty(c.Path, v.CaddyPath)
				}
				if v.Host == "" {
					v.Host = firstNonEmpty(c.Host, v.CaddyHost)
				}
			} else {
				v.Warnings = append(v.Warnings, fmt.Sprintf("分享链接解析失败: %v", err))
			}
		} else {
			warns = append(warns, fmt.Sprintf("%s: `v2ray url` 未输出该节点的链接", c.ConfName))
		}

		// —— 公网入口判定 ——
		//
		// conf 的 listen 决定该节点是否「必须经前置反代」:
		//   ws/h2/grpc/http → 127.0.0.1 (core.sh:337-343), 只能由 Caddy 转发, 没有站点就是死链;
		//   其余 (tcp/kcp/quic/tcpd/kcpd/quicd/ss/door/socks) → 0.0.0.0, 本身就是真实的公网直连入口, 本就没有站点。
		// 所以「有没有公网入口」的正确判据是 ListenAddr, 而不是 HasCaddy, 更不是 PublicPort == 0。
		needsProxy := c.Listen == common.ListenLoopback

		// 兜底: url 层没给端口时, 只对「需要反代」的节点用 Caddy 层补端口;
		// 对 0.0.0.0 直连节点补机器全局 https_port 是错的 (那个口上根本没有它的路由)。
		if v.PublicPort == 0 && needsProxy {
			if v.CaddyPort > 0 {
				v.PublicPort = v.CaddyPort
			} else if v.HasCaddy && httpsPort > 0 {
				v.PublicPort = httpsPort
			}
		}
		if v.Address == "" {
			v.Address = firstNonEmpty(v.CaddyHost, c.Host)
		}
		if v.Path == "" {
			v.Path = firstNonEmpty(v.CaddyPath, c.Path)
		}

		// ⚠️ 233boy 对「没有 Caddy 站点」的 *-tls 节点会在 url 层谎报端口:
		// init.sh:136 把机器全局 https_port (82 上 55509) 读进 is_https_port; core.sh:1252 只在
		// /etc/caddy/233boy/<host>.conf 存在时才按节点覆盖它; 而 core.sh:1525/1533/1536 又把这个值
		// 硬编码进 ws/h2/grpc 的客户端链接 —— 于是没有站点的节点, 链接里照样是 55509,
		// 但 Caddy 里没有它的 path 路由, 客户端拿这条链接必然连不上。
		// 这种「假公网入口」必须显式告警并清掉端口, 否则 UI 会把它当成可用节点展示。
		if needsProxy && !v.HasCaddy {
			why := "url 层没有该节点的链接"
			if v.HasURL && v.PublicPort > 0 {
				why = fmt.Sprintf("url 层给出的 %s:%d 是机器全局 https_port 而非本节点入口, 客户端连不上", v.Address, v.PublicPort)
			}
			v.Warnings = append(v.Warnings, fmt.Sprintf(
				"该节点没有公网入口: conf 监听 %s (必须经 Caddy 反代), 但 Caddy 层没有对应站点 —— %s (通常是域名未解析到本机, Caddy 站点没建起来)",
				c.Listen, why))
			v.PublicPort = 0
		}

		// —— 一致性校验 (只提示, 不改值) ——
		if v.HasURL && v.HasCaddy {
			if v.PublicPort != v.CaddyPort && v.CaddyPort > 0 {
				v.Warnings = append(v.Warnings, fmt.Sprintf(
					"公网口不一致: url 层 %d vs Caddy 层 %d (以 url 层为准)", v.PublicPort, v.CaddyPort))
			}
			// url 的 path 与 caddy matcher 的 path 比对 (caddy 可能是 /path* 通配)
			cp := strings.TrimSuffix(v.CaddyPath, "*")
			if v.Path != "" && cp != "" && !strings.HasPrefix(v.Path, cp) {
				v.Warnings = append(v.Warnings, fmt.Sprintf(
					"路径不一致: url 层 %q vs Caddy 层 %q", v.Path, v.CaddyPath))
			}
		}
		if v.InternalPort != 0 && v.CaddyUpstream != 0 && v.InternalPort != v.CaddyUpstream {
			v.Warnings = append(v.Warnings, fmt.Sprintf(
				"内网口不一致: conf %d vs Caddy upstream %d", v.InternalPort, v.CaddyUpstream))
		}
		// 233boy 服务端 inbound security 恒为 none; 非 none 说明被手工改过
		if v.InboundSec != "" && v.InboundSec != "none" && !c.IsDynamicPort {
			v.Warnings = append(v.Warnings, fmt.Sprintf(
				"conf 的入站 security=%q (233boy 通常为 none, 可能被手工改过)", v.InboundSec))
		}

		views = append(views, v)
	}

	// —— 孤儿: Caddy 有路由但 conf 里没有对应内网口 ——
	for _, s := range sites {
		for _, r := range s.Routes {
			if r.UpstreamPort > 0 {
				if confPorts[r.UpstreamPort] {
					if !usedCaddy[s.FileName] {
						warns = append(warns, fmt.Sprintf(
							"Caddy %s 的站点未被任何 conf 使用 (upstream %s)", s.FileName, r.Upstream))
					}
				} else {
					warns = append(warns, fmt.Sprintf(
						"孤儿反代: %s 把 %s 转发到 %s, 但没有对应的 conf 文件 (节点可能已删)", s.FileName, r.Path, r.Upstream))
				}
			}
		}
	}
	// —— 孤儿: url 有链接但 conf 文件不存在 ——
	for _, u := range uris {
		if !usedURI[u.ConfName] {
			warns = append(warns, fmt.Sprintf("`v2ray url` 输出了 %s 的链接, 但 conf 目录里没有该文件", u.ConfName))
		}
	}

	sort.Slice(views, func(i, j int) bool { return views[i].ConfName < views[j].ConfName })
	return views, warns
}

// ---------------------------------------------------------------- nginx 遗留探测

// NginxLegacy 宝塔 nginx 里的遗留反代块。
//
// ⚠️ 实测结论: 82 上 nginx:443 服务的是 CloudFlare Origin 证书, 而 tzq1 的 A 记录
// 直接指向本机、真实数据面是 Caddy:55509 (带 Upgrade 请求返回 101)。所以 nginx 层
// **不是活路径**, 面板绝不写它。这里只做只读探测, 用于向用户提示「历史残留」。
type NginxLegacy struct {
	FileName   string
	ServerName string
	Path       string
	Upstream   string
}

var (
	// location 行, 含 `location = /x {` / `location ^~ /y {` / `location /z {`
	nginxLocationRe   = regexp.MustCompile(`^\s*location\s+(?:=\s*|\^~\s*|~\*?\s*)?(\S+)\s*\{`)
	nginxServerNameRe = regexp.MustCompile(`(?m)^\s*server_name\s+([^;]+);`)
	// proxy_pass 的值, 只取 http://host:port 形态
	nginxProxyPassRe = regexp.MustCompile(`^\s*proxy_pass\s+https?://([0-9A-Za-z._\-]+):(\d{1,5})\s*;`)
)

// ScanNginxLegacy 只读扫描 nginx 站点目录, 找出形如 `location = /<uuid>` 的遗留反代。
//
// ⚠️ 必须按 **location 块作用域** 取 proxy_pass, 不能全文取第一个 ——
// 实测 tzq1 的 conf 里第一个 proxy_pass 是 ACME challenge 的
// `location ^~ /.well-known/acme-challenge/ { proxy_pass http://127.0.0.1:51350; }`,
// 全文取第一个会把 upstream 错记成 51350 (Caddy 的 http_port) 而不是真正的 45341。
// 因此这里做花括号配对的块级解析。
//
// 目录不存在时返回空 (非宝塔机器属正常)。
func ScanNginxLegacy(dir string) []NginxLegacy {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []NginxLegacy
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".conf") {
			continue
		}
		if strings.Contains(name, ".bak.") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		content := string(data)
		serverName := ""
		if m := nginxServerNameRe.FindStringSubmatch(content); m != nil {
			serverName = strings.TrimSpace(strings.Split(m[1], " ")[0])
		}
		for _, blk := range scanNginxLocationBlocks(content) {
			// 只收「精确匹配」的 location = /xxx (233boy 生成的形态),
			// 以及任何带 http:// upstream 的块 —— 但排除 ACME 路径。
			if strings.HasPrefix(blk.path, "/.well-known/") {
				continue
			}
			if blk.upstream == "" {
				continue
			}
			out = append(out, NginxLegacy{
				FileName:   name,
				ServerName: serverName,
				Path:       blk.path,
				Upstream:   blk.upstream,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FileName != out[j].FileName {
			return out[i].FileName < out[j].FileName
		}
		return out[i].Path < out[j].Path
	})
	return out
}

type nginxBlock struct {
	path     string
	upstream string
}

// scanNginxLocationBlocks 做花括号配对, 返回每个 location 块及其块内的 proxy_pass。
func scanNginxLocationBlocks(content string) []nginxBlock {
	lines := strings.Split(content, "\n")
	var out []nginxBlock
	var cur *nginxBlock
	depth := 0     // 当前 location 块内的嵌套深度 (0 = 块首行之后、未进入内层)
	curDepth := -1 // cur 所在块的外层深度

	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if cur == nil {
			if m := nginxLocationRe.FindStringSubmatch(line); m != nil {
				cur = &nginxBlock{path: m[1]}
				curDepth = depth
				depth++ // 进入该 location 块
			}
			continue
		}
		// 块内: 先看 proxy_pass (只在最外层, 即 depth-1 == curDepth 时取)
		if depth-1 == curDepth {
			if m := nginxProxyPassRe.FindStringSubmatch(line); m != nil && cur.upstream == "" {
				cur.upstream = m[1] + ":" + m[2]
			}
		}
		// 花括号计数 (location 行本身已在上面处理过, 这里只数后续行)
		depth += strings.Count(line, "{")
		depth -= strings.Count(line, "}")
		if depth <= curDepth {
			out = append(out, *cur)
			cur = nil
			depth = curDepth
		}
	}
	if cur != nil { // 文件末尾未闭合 (不该发生, 但容错)
		out = append(out, *cur)
	}
	return out
}

// ---------------------------------------------------------------- 顶层只读采集

// Collect 三层只读采集 + 合并。runner 可为 nil (此时跳过 url 层, 走 Caddy/conf 兜底)。
// 本函数**不写任何文件**, 可以安全地在生产机上对真实节点调用。
func Collect(paths LayerPaths, runner *common.Runner) (*Snapshot, error) {
	snap := &Snapshot{}

	confs, w1, err := ParseConfDir(paths.ConfDir)
	if err != nil {
		return nil, err
	}
	snap.Warnings = append(snap.Warnings, w1...)

	sites, w2, err := ParseCaddyDir(paths.CaddyDir)
	if err != nil {
		// Caddy 目录缺失不算致命: 可能该机还没装 Caddy (纯动态端口节点)
		snap.Warnings = append(snap.Warnings, fmt.Sprintf("Caddy 目录不可用: %v", err))
	}
	snap.Warnings = append(snap.Warnings, w2...)

	if paths.Caddyfile != "" {
		hp, sp, err := ParseCaddyGlobal(paths.Caddyfile)
		if err == nil {
			snap.HTTPPort, snap.HTTPSPort = hp, sp
		} else {
			snap.Warnings = append(snap.Warnings, fmt.Sprintf("读取 Caddyfile 全局端口失败: %v", err))
		}
	}

	if runner != nil {
		// 逐节点锚定调用 (见 common.Runner.URL): 无参 `v2ray url` 在多节点机器上
		// 会进配置选择菜单自旋, 白等满 Timeout 且拿不到任何链接。
		uris, warns, err := runner.URLs()
		if err != nil {
			snap.Warnings = append(snap.Warnings, fmt.Sprintf("采集客户端链接失败: %v", err))
		}
		snap.Warnings = append(snap.Warnings, warns...)
		snap.URIs = uris
	}

	views, w3 := Merge(confs, sites, snap.URIs, snap.HTTPSPort)
	snap.Nodes = views
	snap.Warnings = append(snap.Warnings, w3...)

	for _, l := range ScanNginxLegacy(paths.NginxDir) {
		snap.Warnings = append(snap.Warnings, fmt.Sprintf(
			"nginx 遗留反代 (非活路径, 面板不会改它): %s server_name=%s location=%s → %s",
			l.FileName, l.ServerName, l.Path, l.Upstream))
	}
	return snap, nil
}

// FindNode 按 conf 名 (或域名子串) 找节点, 供上层按名操作时复用
func (s *Snapshot) FindNode(key string) *NodeView {
	for i := range s.Nodes {
		n := &s.Nodes[i]
		if n.ConfName == key || n.ConfName == key+".json" {
			return n
		}
	}
	for i := range s.Nodes {
		n := &s.Nodes[i]
		if strings.Contains(n.ConfName, key) || (n.Address != "" && strings.Contains(n.Address, key)) {
			return n
		}
	}
	return nil
}

// ---------------------------------------------------------------- 小工具

func splitHostPort(s string) (string, int) {
	s = strings.TrimSpace(s)
	if m := caddyHostPortRe.FindStringSubmatch(s); m != nil {
		p, _ := strconv.Atoi(m[2])
		return m[1], p
	}
	return s, 0
}

func anyToString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

func anyToInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case json.Number:
		i, _ := t.Int64()
		return int(i)
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(t))
		return i
	default:
		return 0
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// decodeB64Loose 容忍标准/URL-safe 两种字母表与缺失的 padding。
// 必要性: vmess 链接用标准 base64, ss 的 SIP002 用 URL-safe, 而两者都可能被
// 各种客户端去掉末尾 '='。
func decodeB64Loose(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("空 base64")
	}
	cands := []string{s}
	if pad := len(s) % 4; pad != 0 {
		cands = append(cands, s+strings.Repeat("=", 4-pad))
	}
	var lastErr error
	for _, c := range cands {
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding,
			base64.URLEncoding, base64.RawURLEncoding} {
			if b, err := enc.DecodeString(c); err == nil {
				return b, nil
			} else {
				lastErr = err
			}
		}
	}
	return nil, lastErr
}
