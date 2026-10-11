package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"v2panel/internal/common"
)

// 本文件的两条纪律 (与 internal/common/cli_test.go 一致):
//
//  1. 全部测试对生产机 **只读**。tzq1 是线上节点, 任何写操作都禁止。
//     涉及真实 conf 目录的测试都断言「目录指纹前后一致」。
//  2. 三层解析器用 82 上的**真实输入**验证, 不用手写假数据糊过去 ——
//     假数据能过不代表真机能过 (本次会话已因「假想输出」踩过 ExtractJSONObject 的坑)。
const (
	realConfDir   = "/etc/v2ray/conf"
	realCaddyDir  = "/etc/caddy/233boy"
	realCaddyfile = "/etc/caddy/Caddyfile"
	realNginxDir  = "/www/server/panel/vhost/nginx"

	// 线上节点, 三层断言都围绕它
	tzqConfName   = "VLESS-WS-TLS-node1.example.com.json"
	tzqUUID       = "11111111-2222-4333-8444-555555555555"
	tzqDomain     = "node1.example.com"
	tzqPublicPort = 55509
	tzqInternal   = 45341
	tzqPath       = "/11111111-2222-4333-8444-555555555555"
)

// skipIfNoRealLayers 让"拿真实机器输入"的测试**默认跳过**。
//
// ⚠️ 光判断 realConfDir 存在是不够的: 任何装了 233boy 的机器都有那个目录,
// 但里面**不一定有本文件断言的那个 conf 名** (tzqConfName 是部署特有的)。
// 于是这些测试在别人机器上会以"未解析出 XXX.json"的形式失败 ——
// 看起来像解析器坏了, 实际只是环境里没有那个节点。这种误导比不跑更糟。
//
// 要跑就显式开 (机器上确实有那个节点时):
//
//	V2PANEL_REAL_TEST=1 go test ./internal/agent/ -run TestRealMachine -v
func skipIfNoRealLayers(t *testing.T) {
	t.Helper()
	if os.Getenv("V2PANEL_REAL_TEST") != "1" {
		t.Skip("真机输入测试默认跳过 (依赖部署特有的 conf 名)。要跑: V2PANEL_REAL_TEST=1")
	}
	if _, err := os.Stat(realConfDir); err != nil {
		t.Skipf("非 233boy 生产机, 跳过真实输入测试: %v", err)
	}
}

// ---------------------------------------------------------------- 纯函数层

func TestParseConfJSON_VLESSWS(t *testing.T) {
	// 与线上 tzq1 同构的输入 (含 warn 行不会出现, 因为走的是文件内容)
	raw := `{"inbounds":[{"tag":"VLESS-WS-TLS-node1.example.com.json","port":45341,
	"listen":"127.0.0.1","protocol":"vless","settings":{"clients":[{"id":"11111111-2222-4333-8444-555555555555"}],
	"decryption":"none"},"streamSettings":{"network":"ws","security":"none",
	"wsSettings":{"path":"/11111111-2222-4333-8444-555555555555","headers":{"Host":"node1.example.com"}}},
	"sniffing":{"enabled":true,"destOverride":["http","tls"]}}]}`
	n, warns, err := ParseConfJSON(tzqConfName, "/x/"+tzqConfName, []byte(raw))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("不应有警告, 得到: %v", warns)
	}
	if n.Port != tzqInternal {
		t.Errorf("Port = %d, 期望 %d", n.Port, tzqInternal)
	}
	if n.Listen != "127.0.0.1" {
		t.Errorf("Listen = %q, 期望 127.0.0.1", n.Listen)
	}
	if n.Protocol != "vless" {
		t.Errorf("Protocol = %q", n.Protocol)
	}
	if n.Network != "ws" {
		t.Errorf("Network = %q", n.Network)
	}
	if n.Security != "none" {
		t.Errorf("Security = %q, 期望 none (233boy 服务端恒为 none)", n.Security)
	}
	if n.UUID != tzqUUID {
		t.Errorf("UUID = %q, 期望 %q", n.UUID, tzqUUID)
	}
	if n.Path != tzqPath {
		t.Errorf("Path = %q, 期望 %q", n.Path, tzqPath)
	}
	if n.Host != tzqDomain {
		t.Errorf("Host = %q, 期望 %q", n.Host, tzqDomain)
	}
	if n.IsDynamicPort {
		t.Error("普通节点不应被标记为动态端口")
	}
}

func TestParseConfJSON_DynamicPortDetected(t *testing.T) {
	name := "VMess-TCP-dynamic-port-23497.json"
	raw := `{"inbounds":[{"tag":"` + name + `","port":23497,"listen":"127.0.0.1",
	"protocol":"vmess","settings":{"clients":[{"id":"` + tzqUUID + `"}]},
	"streamSettings":{"network":"tcp","security":"none"}}]}`
	n, _, err := ParseConfJSON(name, "/x/"+name, []byte(raw))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !n.IsDynamicPort {
		t.Error("应识别为动态端口节点")
	}
	if n.DynamicBase != "VMess-TCP" {
		t.Errorf("DynamicBase = %q, 期望 VMess-TCP", n.DynamicBase)
	}
}

func TestParseConfJSON_RejectsBroken(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"非 JSON", `not json at all`},
		{"无 inbound", `{"inbounds":[]}`},
	} {
		if _, _, err := ParseConfJSON("x.json", "/x/x.json", []byte(tc.raw)); err == nil {
			t.Errorf("%s: 期望报错但没有", tc.name)
		}
	}
}

// Caddy 层: 站点首行 + 反代路径 + upstream
func TestParseCaddySite_Real(t *testing.T) {
	content := `node1.example.com:55509 {
    reverse_proxy /11111111-2222-4333-8444-555555555555 127.0.0.1:45341
    import /etc/caddy/233boy/node1.example.com.conf.add
}`
	s, warns, err := ParseCaddySite("node1.example.com.conf", "/x/node1.example.com.conf", content)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("不应有警告: %v", warns)
	}
	if s.Host != tzqDomain || s.Port != tzqPublicPort {
		t.Errorf("站点 = %s:%d, 期望 %s:%d", s.Host, s.Port, tzqDomain, tzqPublicPort)
	}
	if len(s.Routes) != 1 {
		t.Fatalf("路由数 = %d, 期望 1", len(s.Routes))
	}
	r := s.Routes[0]
	if r.Path != tzqPath {
		t.Errorf("Path = %q", r.Path)
	}
	if r.UpstreamPort != tzqInternal {
		t.Errorf("UpstreamPort = %d, 期望 %d", r.UpstreamPort, tzqInternal)
	}
	if r.UpstreamIP != "127.0.0.1" {
		t.Errorf("UpstreamIP = %q", r.UpstreamIP)
	}
	// `import ...` 那行不能被误认成站点块或反代
	if s.SiteAddr != tzqDomain+":55509" {
		t.Errorf("SiteAddr = %q", s.SiteAddr)
	}
}

// import 行 / handle 块不能被当成站点首行
func TestParseCaddySite_IgnoresNonSiteBlocks(t *testing.T) {
	content := `tzq1.example.com:55509 {
    handle {
        reverse_proxy 127.0.0.1:45341
    }
    import /etc/caddy/233boy/tzq1.example.com.conf.add
}`
	s, _, err := ParseCaddySite("tzq1.example.com.conf", "/x", content)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if s.Host != "tzq1.example.com" {
		t.Errorf("Host = %q, 期望 tzq1.example.com (不能把 handle 当站点)", s.Host)
	}
	// 无 matcher 的反代 → 用 /* 表示
	if len(s.Routes) != 1 || s.Routes[0].Path != "/*" {
		t.Errorf("无 matcher 时应记为 /*, 得到 %+v", s.Routes)
	}
}

func TestParseCaddyGlobal(t *testing.T) {
	content := `{
  admin off
  http_port 51350
  https_port 55509
  acme_ca https://acme-v02.api.letsencrypt.org/directory
}
import /etc/caddy/233boy/*.conf`
	p := filepath.Join(t.TempDir(), "Caddyfile")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	hp, sp, err := ParseCaddyGlobal(p)
	if err != nil {
		t.Fatal(err)
	}
	if hp != 51350 || sp != 55509 {
		t.Errorf("http_port=%d https_port=%d, 期望 51350/55509", hp, sp)
	}
}

// .add 与 .bak.* 必须被跳过 —— 不跳会解析出幽灵节点
func TestParseCaddyDir_SkipsAddAndBak(t *testing.T) {
	dir := t.TempDir()
	body := "node1.example.com:55509 {\n    reverse_proxy /" + tzqUUID + " 127.0.0.1:45341\n}\n"
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("node1.example.com.conf", body)
	write("node1.example.com.conf.add", "# see https://233boy.com/v2ray/caddy-auto-tls/\n")
	write("node1.example.com.conf.bak.20260920", body)

	sites, warns, err := ParseCaddyDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 1 {
		t.Fatalf("站点数 = %d, 期望 1 (.add/.bak 必须跳过); 得到 %+v; warns=%v",
			len(sites), sites, warns)
	}
	if sites[0].FileName != "node1.example.com.conf" {
		t.Errorf("解析到了错误的文件: %s", sites[0].FileName)
	}
}

// ---------------------------------------------------------------- url 层

func TestParseShareURI_VLESS(t *testing.T) {
	uri := "vless://" + tzqUUID + "@" + tzqDomain + ":55509?encryption=none&security=tls&type=ws" +
		"&host=" + tzqDomain + "&path=%2F" + tzqUUID + "#233boy-ws-" + tzqDomain
	su, err := ParseShareURI(uri)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if su.Scheme != "vless" {
		t.Errorf("Scheme = %q", su.Scheme)
	}
	if su.UUID != tzqUUID {
		t.Errorf("UUID = %q", su.UUID)
	}
	if su.Address != tzqDomain || su.Port != tzqPublicPort {
		t.Errorf("地址 = %s:%d", su.Address, su.Port)
	}
	if su.Path != tzqPath {
		t.Errorf("Path = %q, 期望 %q (必须已 URL 解码)", su.Path, tzqPath)
	}
	if su.Security != "tls" {
		t.Errorf("客户端 security = %q, 期望 tls (与 conf 的 inbound none 不同, 这是设计如此)", su.Security)
	}
	if su.Network != "ws" {
		t.Errorf("Network = %q", su.Network)
	}
	if su.Host != tzqDomain {
		t.Errorf("Host = %q", su.Host)
	}
}

func TestParseShareURI_Trojan(t *testing.T) {
	uri := "trojan://somepass@" + tzqDomain + ":55509?security=tls&type=ws&host=" + tzqDomain +
		"&path=%2Fabc#trojan-ws"
	su, err := ParseShareURI(uri)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if su.UUID != "somepass" { // trojan 的 userinfo 就是密码
		t.Errorf("UUID = %q, 期望 somepass", su.UUID)
	}
	if su.Path != "/abc" {
		t.Errorf("Path = %q", su.Path)
	}
}

func TestParseShareURI_SS_SIP002(t *testing.T) {
	// ss://base64(aes-256-gcm:pass)@host:port#tag
	uri := "ss://YWVzLTI1Ni1nY206c2VjcmV0cGFzcw==@" + tzqDomain + ":55509#ss-node"
	su, err := ParseShareURI(uri)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if su.Method != "aes-256-gcm" {
		t.Errorf("Method = %q", su.Method)
	}
	if su.Password != "secretpass" {
		t.Errorf("Password = %q", su.Password)
	}
	if su.Address != tzqDomain || su.Port != tzqPublicPort {
		t.Errorf("地址 = %s:%d", su.Address, su.Port)
	}
	if su.Tag != "ss-node" {
		t.Errorf("Tag = %q", su.Tag)
	}
}

func TestParseShareURI_VMess(t *testing.T) {
	// {"v":"2","ps":"vmess-tcp","add":"node1.example.com","port":"23497","id":"<uuid>",
	//  "aid":"0","net":"tcp","type":"none","host":"","path":"","tls":""}
	payload := `{"v":"2","ps":"vmess-tcp","add":"` + tzqDomain + `","port":"23497","id":"` + tzqUUID +
		`","aid":"0","net":"tcp","type":"none","host":"","path":"","tls":""}`
	uri := "vmess://" + b64Std(payload)
	su, err := ParseShareURI(uri)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if su.UUID != tzqUUID {
		t.Errorf("UUID = %q", su.UUID)
	}
	if su.Address != tzqDomain || su.Port != 23497 {
		t.Errorf("地址 = %s:%d", su.Address, su.Port)
	}
	if su.Tag != "vmess-tcp" {
		t.Errorf("Tag = %q", su.Tag)
	}
	if su.Security != "none" {
		t.Errorf("Security = %q, 期望 none (tls 字段为空时兜底)", su.Security)
	}
}

func TestParseShareURI_RejectsGarbage(t *testing.T) {
	for _, uri := range []string{"", "not-a-uri", "https://t.me/tg2333", "vmess://@@@"} {
		if _, err := ParseShareURI(uri); err == nil {
			t.Errorf("%q: 期望报错但没有", uri)
		}
	}
}

func b64Std(s string) string {
	const tbl = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var b strings.Builder
	for i := 0; i < len(s); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], s[i:])
		b.WriteByte(tbl[chunk[0]>>2])
		b.WriteByte(tbl[(chunk[0]&0x03)<<4|chunk[1]>>4])
		if n > 1 {
			b.WriteByte(tbl[(chunk[1]&0x0f)<<2|chunk[2]>>6])
		} else {
			b.WriteByte('=')
		}
		if n > 2 {
			b.WriteByte(tbl[chunk[2]&0x3f])
		} else {
			b.WriteByte('=')
		}
	}
	return b.String()
}

// ---------------------------------------------------------------- 合并层

func TestMerge_JoinsByPortAndName(t *testing.T) {
	confs := []ConfNode{{
		ConfName: tzqConfName, Protocol: "vless", Port: tzqInternal, Listen: "127.0.0.1",
		Network: "ws", Security: "none", UUID: tzqUUID, Path: tzqPath, Host: tzqDomain,
	}}
	sites := []CaddySite{{
		FileName: "node1.example.com.conf", SiteAddr: tzqDomain + ":55509",
		Host: tzqDomain, Port: tzqPublicPort,
		Routes: []CaddyRoute{{Path: tzqPath, Upstream: "127.0.0.1:45341",
			UpstreamIP: "127.0.0.1", UpstreamPort: tzqInternal}},
	}}
	uris := []common.NodeURI{{ConfName: tzqConfName,
		URI: "vless://" + tzqUUID + "@" + tzqDomain + ":55509?encryption=none&security=tls&type=ws" +
			"&host=" + tzqDomain + "&path=%2F" + tzqUUID + "#233boy-ws-" + tzqDomain}}

	views, warns := Merge(confs, sites, uris, 55509)
	if len(warns) != 0 {
		t.Errorf("三层自洽时不应有警告, 得到: %v", warns)
	}
	if len(views) != 1 {
		t.Fatalf("节点数 = %d", len(views))
	}
	v := views[0]
	checks := []struct {
		name      string
		got, want any
	}{
		{"Address", v.Address, tzqDomain},
		{"PublicPort", v.PublicPort, tzqPublicPort},
		{"Path", v.Path, tzqPath},
		{"Host", v.Host, tzqDomain},
		{"InternalPort", v.InternalPort, tzqInternal},
		{"ClientSecurity", v.ClientSecurity, "tls"},
		{"InboundSec", v.InboundSec, "none"},
		{"UUID", v.UUID, tzqUUID},
		{"Protocol", v.Protocol, "vless"},
		{"CaddyUpstream", v.CaddyUpstream, tzqInternal},
		{"HasConf", v.HasConf, true},
		{"HasCaddy", v.HasCaddy, true},
		{"HasURL", v.HasURL, true},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, 期望 %v", c.name, c.got, c.want)
		}
	}
}

// 三层不一致时必须出警告, 且**以 url 层为准**
func TestMerge_PrefersURLLayerAndWarns(t *testing.T) {
	confs := []ConfNode{{ConfName: "n.json", Protocol: "vless", Port: 45341, Listen: "127.0.0.1",
		Network: "ws", Security: "none", UUID: tzqUUID}}
	sites := []CaddySite{{FileName: "d.conf", SiteAddr: "d.com:55509", Host: "d.com", Port: 55509,
		Routes: []CaddyRoute{{Path: "/a", Upstream: "127.0.0.1:45341", UpstreamPort: 45341}}}}
	// url 层说公网口是 8443 (与 Caddy 的 55509 不同)
	uris := []common.NodeURI{{ConfName: "n.json",
		URI: "vless://" + tzqUUID + "@d.com:8443?security=tls&type=ws&host=d.com&path=%2Fa"}}

	views, _ := Merge(confs, sites, uris, 55509)
	if views[0].PublicPort != 8443 {
		t.Errorf("PublicPort = %d, 期望 8443 (以 url 层为准)", views[0].PublicPort)
	}
	found := false
	for _, w := range views[0].Warnings {
		if strings.Contains(w, "公网口不一致") {
			found = true
		}
	}
	if !found {
		t.Errorf("应提示公网口不一致, 得到: %v", views[0].Warnings)
	}
}

// 孤儿反代 / 孤儿链接都要被点出来
func TestMerge_DetectsOrphans(t *testing.T) {
	sites := []CaddySite{{FileName: "ghost.conf", SiteAddr: "ghost.com:55509", Host: "ghost.com",
		Port: 55509, Routes: []CaddyRoute{{Path: "/dead", Upstream: "127.0.0.1:9999", UpstreamPort: 9999}}}}
	uris := []common.NodeURI{{ConfName: "gone.json", URI: "vless://x@y.com:55509?type=ws&path=%2Fz"}}

	views, warns := Merge(nil, sites, uris, 55509)
	if len(views) != 0 {
		t.Errorf("无 conf 时不应产出节点, 得到 %d 个", len(views))
	}
	var hasOrphanProxy, hasOrphanURI bool
	for _, w := range warns {
		if strings.Contains(w, "孤儿反代") {
			hasOrphanProxy = true
		}
		if strings.Contains(w, "没有该文件") {
			hasOrphanURI = true
		}
	}
	if !hasOrphanProxy {
		t.Errorf("应报孤儿反代, 得到: %v", warns)
	}
	if !hasOrphanURI {
		t.Errorf("应报孤儿链接, 得到: %v", warns)
	}
}

// ---------------------------------------------------------------- 真机只读验证

// ★ 计划中的验收断言: 拿 82 上真实 tzq1 的三份输入跑, 断言合并结果。
// 只读 —— 前后都校验 conf 目录指纹一致。
func TestRealMachine_Tzq1ThreeWayMerge(t *testing.T) {
	skipIfNoRealLayers(t)

	runner := common.NewRunner()
	before, err := runner.ConfDirDigest()
	if err != nil {
		t.Fatalf("取 conf 指纹失败: %v", err)
	}

	snap, err := Collect(DefaultLayerPaths(), runner)
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}

	after, err := runner.ConfDirDigest()
	if err != nil {
		t.Fatalf("取 conf 指纹失败: %v", err)
	}
	if before != after {
		t.Fatalf("❌ conf 目录被改动了! before=%s after=%s", before, after)
	}

	if snap.HTTPSPort != tzqPublicPort {
		t.Errorf("Caddy 全局 https_port = %d, 期望 %d", snap.HTTPSPort, tzqPublicPort)
	}

	v := snap.FindNode("tzq1")
	if v == nil {
		t.Fatalf("未找到 tzq1 节点; 全部节点: %+v; 警告: %v", snap.Nodes, snap.Warnings)
	}

	checks := []struct {
		name      string
		got, want any
	}{
		{"ConfName", v.ConfName, tzqConfName},
		{"Address", v.Address, tzqDomain},
		{"PublicPort", v.PublicPort, tzqPublicPort},
		{"Path", v.Path, tzqPath},
		{"InternalPort", v.InternalPort, tzqInternal},
		{"InboundSec", v.InboundSec, "none"},
		{"ListenAddr", v.ListenAddr, "127.0.0.1"},
		{"Protocol", v.Protocol, "vless"},
		{"UUID", v.UUID, tzqUUID},
		{"ClientSecurity", v.ClientSecurity, "tls"},
		{"Host", v.Host, tzqDomain},
		{"HasConf", v.HasConf, true},
		{"HasCaddy", v.HasCaddy, true},
		{"HasURL", v.HasURL, true},
		{"CaddyUpstream", v.CaddyUpstream, tzqInternal},
		{"CaddyHost", v.CaddyHost, tzqDomain},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, 期望 %v", c.name, c.got, c.want)
		}
	}

	// 真实数据上三层应当自洽, 节点自身不该有警告
	if len(v.Warnings) != 0 {
		t.Errorf("tzq1 三层应自洽, 但报了警告: %v", v.Warnings)
	}

	t.Logf("tzq1 合并结果: %s://%s@%s:%d?type=%s&security=%s&host=%s&path=%s → 127.0.0.1:%d",
		v.Protocol, v.UUID, v.Address, v.PublicPort, v.ClientNetwork, v.ClientSecurity,
		v.Host, v.Path, v.InternalPort)
}

// .add / .bak 在真机上确实存在, 必须没被解析成节点
func TestRealMachine_SkipsAddAndBak(t *testing.T) {
	skipIfNoRealLayers(t)
	if _, err := os.Stat(realCaddyDir); err != nil {
		t.Skip("无 Caddy 目录")
	}
	sites, _, err := ParseCaddyDir(realCaddyDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sites {
		if strings.Contains(s.FileName, ".add") || strings.Contains(s.FileName, ".bak.") {
			t.Errorf("解析到了应跳过的文件: %s", s.FileName)
		}
	}
	// 真机上 .conf 恰好 1 个, .add/.bak 各 1 个
	ents, _ := os.ReadDir(realCaddyDir)
	var conf, add, bak int
	for _, e := range ents {
		n := e.Name()
		switch {
		case strings.Contains(n, ".bak."):
			bak++
		case strings.HasSuffix(n, ".add"):
			add++
		case strings.HasSuffix(n, ".conf"):
			conf++
		}
	}
	t.Logf("真机 Caddy 目录: .conf=%d .conf.add=%d .conf.bak.*=%d → 解析出站点 %d 个",
		conf, add, bak, len(sites))
	if len(sites) != conf {
		t.Errorf("解析出的站点数 %d 应等于 .conf 文件数 %d", len(sites), conf)
	}
}

// nginx 遗留探测: 只读, 且必须能认出 tzq1 的那条 location
func TestRealMachine_NginxLegacyDetected(t *testing.T) {
	skipIfNoRealLayers(t)
	if _, err := os.Stat(realNginxDir); err != nil {
		t.Skip("无宝塔 nginx 目录")
	}
	legacy := ScanNginxLegacy(realNginxDir)
	var hit *NginxLegacy
	for i := range legacy {
		if legacy[i].Path == tzqPath {
			hit = &legacy[i]
		}
	}
	if hit == nil {
		t.Fatalf("未在 nginx 里找到 tzq1 的遗留 location=%s; 全部: %+v", tzqPath, legacy)
	}
	if hit.Upstream != "127.0.0.1:45341" {
		t.Errorf("Upstream = %q, 期望 127.0.0.1:45341", hit.Upstream)
	}
	// 这条只用于提示, 不能影响合并结果 (合并结果里 HasCaddy 走的是 Caddy 层)
	t.Logf("nginx 遗留: %s server_name=%s %s → %s (仅提示, 面板不改 nginx)",
		hit.FileName, hit.ServerName, hit.Path, hit.Upstream)
}

// 真实 conf 目录解析: 至少 tzq1 能被解出来
func TestRealMachine_ParseConfDir(t *testing.T) {
	skipIfNoRealLayers(t)
	confs, warns, err := ParseConfDir(realConfDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(confs) == 0 {
		t.Fatal("真实 conf 目录里一个节点都没解析出来")
	}
	var found bool
	for _, c := range confs {
		if c.ConfName == tzqConfName {
			found = true
			if c.Port != tzqInternal || c.UUID != tzqUUID {
				t.Errorf("tzq1 conf 解析异常: port=%d uuid=%s", c.Port, c.UUID)
			}
		}
	}
	if !found {
		t.Errorf("未解析出 %s; 得到 %d 个节点", tzqConfName, len(confs))
	}
	t.Logf("真实 conf 目录: 解析出 %d 个节点, 警告 %d 条", len(confs), len(warns))
}

// 真实 `v2ray url` 输出必须能被解出链接 (依赖 ANSI 剥离正确)
func TestRealMachine_URLOutputParses(t *testing.T) {
	skipIfNoRealLayers(t)
	runner := common.NewRunner()
	uris, warns, err := runner.URLs()
	if err != nil {
		t.Fatalf("采集客户端链接失败: %v", err)
	}
	if len(warns) > 0 {
		t.Logf("逐节点取链接的告警 (%d 条): %v", len(warns), warns)
	}
	if len(uris) == 0 {
		t.Fatalf("未解析出任何链接 (警告: %v)", warns)
	}
	var found bool
	for _, u := range uris {
		if u.ConfName == tzqConfName {
			found = true
			if !strings.HasPrefix(u.URI, "vless://"+tzqUUID+"@"+tzqDomain+":") {
				t.Errorf("tzq1 链接前缀异常: %s", truncate(u.URI, 120))
			}
			if strings.Contains(u.URI, "\x1b") {
				t.Error("链接里残留了 ANSI 转义")
			}
		}
	}
	if !found {
		t.Errorf("未找到 %s 的链接; 得到: %+v", tzqConfName, uris)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(截断)"
}

// ---------------------------------------------------------------- 假公网入口 (合成输入)

// ★ 233boy 的「假公网入口」陷阱 —— 用合成输入把这条分支钉死。
//
// 背景 (源码级):
//
//	init.sh:136 把**机器全局** https_port (82 上是 55509) 读进 is_https_port;
//	core.sh:1252 只在 `/etc/caddy/233boy/<host>.conf` 存在时才按节点覆盖它;
//	core.sh:1525/1533/1536 又把这个值**硬编码**进 ws/h2/grpc 的客户端链接。
//	于是「没有 Caddy 站点」的 *-tls 节点, 链接里照样写着 55509 —— 但 Caddy 里
//	根本没有它的 path 路由, 客户端拿这条链接必然连不上。
//
// 这个分支此前**永远不可达**: 老实现无参调用 `v2ray url` 会进选择菜单自旋, url 层
// 压根拿不到数据, 于是 `v.HasURL` 恒为 false。现在逐节点锚定调用能拿到链接了, 该分支
// 才真正被激活 —— 真机生命周期测试已实测触发, 这里再补一个零副作用的合成输入版本。
//
// 判据必须是 conf 层的 ListenAddr, 不是 HasCaddy / PublicPort == 0:
//
//	ws/h2/grpc/http → 127.0.0.1 (core.sh:337-343), 只能经 Caddy 转发, 没站点就是死链;
//	其余协议 → 0.0.0.0, 本身就是真实公网直连入口, 本就不该有站点, 不能误报。
func TestMerge_FlagsFakePublicEntryFromGlobalHTTPSPort(t *testing.T) {
	const orphan = "VLESS-WS-TLS-orphan.example.com.json"
	confs := []ConfNode{{
		ConfName: orphan, Protocol: "vless", Port: 45342, Listen: common.ListenLoopback,
		Network: "ws", Security: "none", UUID: tzqUUID,
	}}
	// Caddy 层完全没有该节点 (域名没解析到本机 / 站点没建起来), 但 url 层照旧输出 55509
	uris := []common.NodeURI{{ConfName: orphan,
		URI: "vless://" + tzqUUID + "@orphan.example.com:55509?encryption=none&security=tls" +
			"&type=ws&host=orphan.example.com&path=%2F" + tzqUUID + "#233boy-ws-orphan.example.com"}}

	views, warns := Merge(confs, nil, uris, 55509)
	if len(views) != 1 {
		t.Fatalf("节点数 = %d, 期望 1", len(views))
	}
	v := views[0]

	if v.PublicPort != 0 {
		t.Errorf("PublicPort = %d, 期望 0 (机器全局 https_port 不是本节点入口, 必须清掉)", v.PublicPort)
	}
	if !v.HasURL {
		t.Errorf("HasURL = false, 期望 true (url 层确实给了链接)")
	}
	if v.HasCaddy {
		t.Errorf("HasCaddy = true, 本用例的前提是 Caddy 层没有该节点")
	}

	var nodeWarn string
	for _, w := range v.Warnings {
		if strings.Contains(w, "没有公网入口") {
			nodeWarn = w
		}
	}
	if nodeWarn == "" {
		t.Fatalf("节点级应报「没有公网入口」, 得到: %v", v.Warnings)
	}
	if !strings.Contains(nodeWarn, "机器全局 https_port") {
		t.Errorf("应点明 55509 是机器全局 https_port, 得到: %s", nodeWarn)
	}
	if !strings.Contains(nodeWarn, "55509") {
		t.Errorf("告警应带上被误报的端口号, 得到: %s", nodeWarn)
	}
	if !strings.Contains(nodeWarn, common.ListenLoopback) {
		t.Errorf("告警应带上 conf 的 listen 作为判据依据, 得到: %s", nodeWarn)
	}
	if !hasWarnContaining(warns, "在 Caddy 层没有对应反代") {
		t.Errorf("总告警应报「Caddy 层没有对应反代」, 得到: %v", warns)
	}

	// 同一节点若 url 层完全没输出 (老实现的行为), 走的是另一条 why 文案,
	// 但「清端口 + 告警」的结论必须一致。
	views2, _ := Merge(confs, nil, nil, 55509)
	if len(views2) != 1 {
		t.Fatalf("无 url 时节点数 = %d", len(views2))
	}
	v2 := views2[0]
	if v2.PublicPort != 0 {
		t.Errorf("无 url 时 PublicPort = %d, 期望 0", v2.PublicPort)
	}
	if !hasWarnContaining(v2.Warnings, "没有公网入口") {
		t.Errorf("无 url 时也应报「没有公网入口」, 得到: %v", v2.Warnings)
	}
	if hasWarnContaining(v2.Warnings, "机器全局 https_port") {
		t.Errorf("无 url 时不该出现「机器全局 https_port」(url 层什么都没给), 得到: %v", v2.Warnings)
	}
}

// 反例保护: 监听 0.0.0.0 的直连协议 (tcp/kcp/quic/ss/socks/door) 本就该没有 Caddy 站点,
// 绝不能因为「没有站点」就误判成假公网入口、把真实端口清成 0。
func TestMerge_DoesNotFlagDirectListenNodes(t *testing.T) {
	const name = "VMess-TCP-23497.json"
	confs := []ConfNode{{
		ConfName: name, Protocol: "vmess", Port: 23497, Listen: common.ListenAny,
		Network: "tcp", Security: "none", UUID: tzqUUID,
	}}
	payload := `{"v":"2","ps":"vmess-tcp","add":"` + tzqDomain + `","port":"23497","id":"` + tzqUUID +
		`","aid":"0","net":"tcp","type":"none","host":"","path":"","tls":""}`
	uris := []common.NodeURI{{ConfName: name, URI: "vmess://" + b64Std(payload)}}

	views, warns := Merge(confs, nil, uris, 55509)
	if len(views) != 1 {
		t.Fatalf("节点数 = %d, 期望 1", len(views))
	}
	v := views[0]
	if v.PublicPort != 23497 {
		t.Errorf("PublicPort = %d, 期望 23497 (直连节点的真实端口不许被清掉)", v.PublicPort)
	}
	if hasWarnContaining(v.Warnings, "没有公网入口") {
		t.Errorf("0.0.0.0 直连节点不该报「没有公网入口」, 得到: %v", v.Warnings)
	}
	if len(warns) != 0 {
		t.Errorf("直连节点无站点属正常, 不该有总告警, 得到: %v", warns)
	}
}
