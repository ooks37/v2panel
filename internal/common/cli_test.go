package common

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ===========================================================================
// 测试纪律 (红线)
//
// 1. 绝不调用 add 的成功路径 —— 那会在生产机 /etc/v2ray/conf/ 真的建节点。
//    add 的安全性由两层分别证明:
//       * BuildArgs 闸门: 纯函数, 缺参直接返回 error, 脚本根本不会被调用。
//       * 脚本层自旋检测: 用 GenRaw 走同一条 ask() 自旋路径证明 (gen 是 dry-run)。
// 2. 所有涉及真机 conf 目录的测试, 都断言指纹前后一致。
// 3. 机器上第一个已存在的 conf 单独断言 md5 不变 (它就是"保护节点")。
// ===========================================================================

// pickProtectedConf 找出机器上第一个 conf 文件名, 用作"不许被改动"的哨兵。
//
// ⚠️ 不能写死文件名。早前这里硬编码了某个线上节点的 conf 名 —— 那是**部署特有**的,
// 换一台机器 / 换个节点名 / 在干净环境跑 `go test` 就必然报
// "no such file or directory" 而不是 skip, 让人以为是代码坏了。
// 这个测试要验证的是"gen 是 dry-run, 不落盘", 具体哪个 conf 无关紧要。
func pickProtectedConf(t *testing.T, r *Runner) string {
	t.Helper()
	files, err := r.ConfFiles()
	if err != nil || len(files) == 0 {
		t.Skip("机器上没有任何 233boy conf, 跳过 (无法验证 gen 不落盘)")
	}
	return files[0]
}

func md5File(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

func skipIfNoScript(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(DefaultBinPath); err != nil {
		t.Skipf("未找到 233boy 入口 %s, 跳过真机测试", DefaultBinPath)
	}
}

// ---------------------------------------------------------------------------
// 纯函数: 参数校验 (逐条对齐脚本 is_test())
// ---------------------------------------------------------------------------

func TestValidatePort(t *testing.T) {
	// 实测: 脚本的 is_test number 是 ^[1-9][0-9]?+$, 拒绝 0 与前导零。
	// 之前用 ^[0-9]{1,5}$ 会放过 09999 —— 那是个真 bug, 这里锁死。
	bad := []string{"0", "09999", "00080", "65536", "99999999999", "", "abc", "80a", "-1", " 80", "8 0"}
	for _, s := range bad {
		if err := ValidatePort(s); err == nil {
			t.Errorf("ValidatePort(%q) 应当报错, 却通过了", s)
		}
	}
	good := []string{"1", "80", "443", "8080", "65535", "23456"}
	for _, s := range good {
		if err := ValidatePort(s); err != nil {
			t.Errorf("ValidatePort(%q) 应当通过, 却报错: %v", s, err)
		}
	}
}

func TestValidateDomain(t *testing.T) {
	// 实测: 脚本 is_test domain 是 ^\w(\w|\-|\.)?+\.\w+$, 允许中间带点。
	// 之前只允许一个点, 会拒绝合法的 a.b.c.com —— 也是个真 bug。
	good := []string{
		"a.com", "node1.example.com", "a.b.c.com", "x1.example.co.uk",
		"my-host.example.com", "a1.com",
	}
	for _, s := range good {
		if err := ValidateDomain(s); err != nil {
			t.Errorf("ValidateDomain(%q) 应当通过, 却报错: %v", s, err)
		}
	}
	bad := []string{"", "com", "a.", ".com", "a..com", "-a.com", "a.com/", "a b.com", "http://a.com"}
	for _, s := range bad {
		if err := ValidateDomain(s); err == nil {
			t.Errorf("ValidateDomain(%q) 应当报错, 却通过了", s)
		}
	}
}

func TestValidatePath(t *testing.T) {
	// 实测: gen vws a.com <uuid> /x1 -> 接受; /x 与 /x1/ -> 拒绝 (脚本要求首尾都是 \w)。
	good := []string{"/x1", "/11111111-2222-4333", "/abc/def", "/a1-b2/c3"}
	for _, s := range good {
		if err := ValidatePath(s); err != nil {
			t.Errorf("ValidatePath(%q) 应当通过, 却报错: %v", s, err)
		}
	}
	bad := []string{"", "/", "/x", "/x1/", "x1", "//x", "/x y"}
	for _, s := range bad {
		if err := ValidatePath(s); err == nil {
			t.Errorf("ValidatePath(%q) 应当报错, 却通过了", s)
		}
	}
}

func TestValidateUUID(t *testing.T) {
	good := []string{
		"11111111-2222-4333-8444-555555555555",
		"C3F5892E-9540-49EE-939E-9A71DA5F45A3",
	}
	for _, s := range good {
		if err := ValidateUUID(s); err != nil {
			t.Errorf("ValidateUUID(%q) 应当通过: %v", s, err)
		}
	}
	bad := []string{"", "not-a-uuid", "11111111-2222-4333-939e", "11111111222243338444555555555555"}
	for _, s := range bad {
		if err := ValidateUUID(s); err == nil {
			t.Errorf("ValidateUUID(%q) 应当报错", s)
		}
	}
}

func TestValidateHeaderType(t *testing.T) {
	// tcp 只接受 none|http (脚本 core.sh:223 覆盖 is_tmp_list)
	if err := ValidateHeaderType("tcp", "none"); err != nil {
		t.Errorf("tcp/none 应当通过: %v", err)
	}
	if err := ValidateHeaderType("tcp", "http"); err != nil {
		t.Errorf("tcp/http 应当通过: %v", err)
	}
	if err := ValidateHeaderType("tcp", "wireguard"); err == nil {
		t.Error("tcp/wireguard 应当报错 (tcp 不支持)")
	}
	// kcp/quic 用全表
	for _, v := range []string{"none", "srtp", "utp", "wechat-video", "dtls", "wireguard"} {
		if err := ValidateHeaderType("kcp", v); err != nil {
			t.Errorf("kcp/%s 应当通过: %v", v, err)
		}
	}
}

func TestValidateSSMethod(t *testing.T) {
	for _, m := range SSMethods {
		if err := ValidateSSMethod(m); err != nil {
			t.Errorf("ValidateSSMethod(%q) 应当通过: %v", m, err)
		}
	}
	if err := ValidateSSMethod("rc4-md5"); err == nil {
		t.Error("rc4-md5 应当被拒绝 (脚本已注释掉)")
	}
}

// ---------------------------------------------------------------------------
// BuildArgs 闸门: 防"静默随机配置"
// ---------------------------------------------------------------------------

func TestBuildArgsRejectsIncomplete(t *testing.T) {
	cases := []struct {
		name string
		spec AddSpec
	}{
		{"vws 缺 host", AddSpec{Token: "vws", UUID: "11111111-2222-4333-8444-555555555555", Path: "/x1"}},
		{"vws 缺 uuid", AddSpec{Token: "vws", Host: "a.com", Path: "/x1"}},
		{"vws 缺 path", AddSpec{Token: "vws", Host: "a.com", UUID: "11111111-2222-4333-8444-555555555555"}},
		// kcp/quic 的伪装类型省略时脚本随机取值 (实测 wechat-video/wireguard/srtp 轮换)
		{"kcp 缺伪装类型", AddSpec{Token: "kcp", Port: "23499", UUID: "11111111-2222-4333-8444-555555555555"}},
		{"quic 缺伪装类型", AddSpec{Token: "quic", Port: "23498", UUID: "11111111-2222-4333-8444-555555555555"}},
		{"tcp 缺伪装类型", AddSpec{Token: "tcp", Port: "23497", UUID: "11111111-2222-4333-8444-555555555555"}},
		// ss 缺 method / password 都会静默随机 (实测 exit=0 且生成了随机 method)
		{"ss 缺 method", AddSpec{Token: "ss", Port: "23496", Password: "pw1234"}},
		{"ss 缺 password", AddSpec{Token: "ss", Port: "23495", Method: "aes-128-gcm"}},
		// socks 省略凭据时脚本自动生成 233boy + 随机 UUID
		{"socks 缺凭据", AddSpec{Token: "socks", Port: "23494"}},
		// door 缺 addr 与 dest_port 都会自旋 (实测: 提示"请输入目标地址"/"请输入目标端口")
		{"door 缺 dest_port", AddSpec{Token: "door", Port: "23493", Addr: "1.1.1.1"}},
		{"door 缺 addr", AddSpec{Token: "door", Port: "23492", DestPort: "443"}},
		{"未知协议", AddSpec{Token: "reality", Host: "a.com"}},
	}
	for _, c := range cases {
		if _, _, err := c.spec.BuildArgs(); err == nil {
			t.Errorf("[%s] 应当被闸门拦下, 却通过了", c.name)
		}
	}
}

func TestBuildArgsRejectsAuto(t *testing.T) {
	// auto 会被脚本 unset 掉走随机值 (core.sh:1026), 面板拿不到确切配置 -> 必须拒绝。
	specs := []AddSpec{
		{Token: "vws", Host: "auto", UUID: "11111111-2222-4333-8444-555555555555", Path: "/x1"},
		{Token: "tcp", Port: "23491", UUID: "auto", Header: "none"},
		{Token: "ss", Port: "auto", Password: "pw1234", Method: "auto"},
	}
	for i, s := range specs {
		if _, _, err := s.BuildArgs(); err == nil {
			t.Errorf("specs[%d] 用了 auto, 应当被拒绝", i)
		}
	}
}

func TestBuildArgsAcceptsComplete(t *testing.T) {
	u := "11111111-2222-4333-8444-555555555555"
	cases := []struct {
		spec     AddSpec
		wantArgs []string
	}{
		{AddSpec{Token: "vws", Host: "a.b.com", UUID: u, Path: "/x1"}, []string{"vws", "a.b.com", u, "/x1"}},
		{AddSpec{Token: "tcp", Port: "23490", UUID: u, Header: "none"}, []string{"tcp", "23490", u, "none"}},
		{AddSpec{Token: "kcp", Port: "23489", UUID: u, Header: "srtp"}, []string{"kcp", "23489", u, "srtp"}},
		{AddSpec{Token: "ss", Port: "23488", Password: "pw1234", Method: "aes-256-gcm"}, []string{"ss", "23488", "pw1234", "aes-256-gcm"}},
		{AddSpec{Token: "socks", Port: "23487", User: "u1", Pass: "p1"}, []string{"socks", "23487", "u1", "p1"}},
		{AddSpec{Token: "door", Port: "23486", Addr: "1.1.1.1", DestPort: "443"}, []string{"door", "23486", "1.1.1.1", "443"}},
		{AddSpec{Token: "http", Port: "23485"}, []string{"http", "23485"}},
	}
	for _, c := range cases {
		args, _, err := c.spec.BuildArgs()
		if err != nil {
			t.Errorf("%s: 应当通过, 却报错: %v", c.spec.Token, err)
			continue
		}
		if strings.Join(args, " ") != strings.Join(c.wantArgs, " ") {
			t.Errorf("%s: 参数映射错误\n  got  %v\n  want %v", c.spec.Token, args, c.wantArgs)
		}
	}
}

func TestNoAutoTLSOnlyForTLSProtocols(t *testing.T) {
	u := "11111111-2222-4333-8444-555555555555"
	// 脚本: [[ $is_no_auto_tls && ! $is_use_tls ]] && err "... 不支持手动配置 tls."
	spec := AddSpec{Token: "tcp", Port: "23484", UUID: u, Header: "none", NoAutoTLS: true}
	if _, _, err := spec.BuildArgs(); err == nil {
		t.Error("非 TLS 协议带 no-auto-tls 应当被拒绝 (脚本会 exit 1)")
	}
	ok := AddSpec{Token: "vws", Host: "a.com", UUID: u, Path: "/x1", NoAutoTLS: true}
	if _, _, err := ok.BuildArgs(); err != nil {
		t.Errorf("TLS 协议带 no-auto-tls 应当通过: %v", err)
	}
}

func TestProtocolTableIntegrity(t *testing.T) {
	for _, d := range ProtocolDefs() {
		if d.Token == "" || d.Name == "" {
			t.Errorf("协议表存在空字段: %+v", d)
		}
		if d.Required < 1 {
			t.Errorf("%s: Required 必须 >= 1", d.Token)
		}
		if d.Required > len(d.ArgNames) {
			t.Errorf("%s: Required=%d 超过 ArgNames 长度 %d", d.Token, d.Required, len(d.ArgNames))
		}
		if d.ListenAddr != ListenLoopback && d.ListenAddr != ListenAny {
			t.Errorf("%s: ListenAddr=%q 非法", d.Token, d.ListenAddr)
		}
		if d.IsTLS && d.ListenAddr != ListenLoopback {
			t.Errorf("%s: TLS 协议必须 listen 127.0.0.1 (实测)", d.Token)
		}
	}
	if RealitySupported() {
		t.Error("v4.27 的 reality 分支被注释掉了, RealitySupported 必须为 false")
	}
	if _, ok := LookupProtocol("reality"); ok {
		t.Error("reality 不应出现在协议表里")
	}
}

func TestConfName(t *testing.T) {
	u := "11111111-2222-4333-8444-555555555555"
	n, err := AddSpec{Token: "vws", Host: "node1.example.com", UUID: u, Path: "/x1"}.ConfName()
	if err != nil || n != "VLESS-WS-TLS-node1.example.com.json" {
		t.Errorf("TLS 系文件名推导错误: %q, %v", n, err)
	}
	n, err = AddSpec{Token: "tcp", Port: "23483", UUID: u, Header: "none"}.ConfName()
	if err != nil || n != "VMess-TCP-23483.json" {
		t.Errorf("非 TLS 系文件名推导错误: %q, %v", n, err)
	}
}

// ---------------------------------------------------------------------------
// 输出解析 (纯函数, 样本格式取自 82 上真实输出)
// ---------------------------------------------------------------------------

func TestStripANSIAndParseURL(t *testing.T) {
	// 真实形态: 链接被 \x1b[45m ... \x1b[0m 包住, 导致 grep -c '^[a-z]*://' 恒为 0。
	raw := "\x1b[36m------------- VLESS-WS-TLS-a.com.json & URL 链接 -------------\x1b[0m\n" +
		"\x1b[45mvless://11111111-2222-4333-8444-555555555555@a.com:55509?encryption=none&security=tls&type=ws&host=a.com&path=%2Fx1#a.com\x1b[0m\n" +
		"\x1b[36m------------- END -------------\x1b[0m\n" +
		"关注: https://t.me/xxx\n"

	clean := CleanOutput(raw)
	if strings.Contains(clean, "\x1b") {
		t.Error("CleanOutput 未剥离 ANSI")
	}
	uris := ParseURLOutput(clean)
	if len(uris) != 1 {
		t.Fatalf("应当解析出 1 条链接, 实际 %d 条: %+v", len(uris), uris)
	}
	if uris[0].ConfName != "VLESS-WS-TLS-a.com.json" {
		t.Errorf("分帧名解析错误: %q", uris[0].ConfName)
	}
	if URIScheme(uris[0].URI) != "vless" {
		t.Errorf("scheme 解析错误: %q", URIScheme(uris[0].URI))
	}
}

func TestExtractJSONObject(t *testing.T) {
	// 脚本输出里 JSON 前面常夹带 core 的 warn 行
	s := "{\"level\":\"warn\",\"msg\":\"...\"}\n\n使用协议: X\n{\n  \"inbounds\": []\n}\n"
	raw, err := ExtractJSONObject(s)
	if err != nil {
		t.Fatalf("应当能截取到 JSON: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("截取结果不是合法 JSON: %v", err)
	}
	if _, ok := v["inbounds"]; !ok {
		t.Error("截取到了错误的对象 (拿到了 warn 行那个)")
	}
}

func TestStatsParseShape(t *testing.T) {
	// 实测形态: 顶层是对象; value 是字符串; 零流量条目整条没有 value 字段。
	payload := `{"stat":[
	  {"name":"inbound>>>VMess-TCP-23483>>>traffic>>>uplink","value":"1024"},
	  {"name":"inbound>>>VMess-TCP-23483>>>traffic>>>downlink","value":"2048"},
	  {"name":"inbound>>>api>>>traffic>>>uplink","value":"999999"},
	  {"name":"inbound>>>Idle>>>traffic>>>uplink"}
	]}`
	raw, err := ExtractJSONObject(payload)
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Stat []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"stat"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Stat) != 4 {
		t.Fatalf("应当解析出 4 条, 实际 %d", len(p.Stat))
	}
	if p.Stat[3].Value != "" {
		t.Error("零流量条目应当没有 value 字段")
	}
}

// ---------------------------------------------------------------------------
// 真机测试: gen dry-run 不落盘 + 缺参自旋能被检测
// ---------------------------------------------------------------------------

func TestGenDryRunLeavesConfUnchanged(t *testing.T) {
	skipIfNoScript(t)
	r := NewRunner()

	before, err := r.ConfDirDigest()
	if err != nil {
		t.Fatalf("读取 conf 指纹失败: %v", err)
	}
	protectedConf := pickProtectedConf(t, r)
	protBefore := md5File(t, r.confDir()+"/"+protectedConf)

	u := "11111111-2222-4333-8444-555555555555"
	specs := []AddSpec{
		{Token: "vws", Host: "v2p-test.example.com", UUID: u, Path: "/v2ptest"},
		{Token: "tcp", Port: "23482", UUID: u, Header: "none"},
		{Token: "kcp", Port: "23481", UUID: u, Header: "srtp"},
		{Token: "ss", Port: "23480", Password: "pw1234", Method: "aes-128-gcm"},
	}
	for _, s := range specs {
		raw, res, err := r.Gen(s)
		if err != nil {
			t.Errorf("%s: gen 失败: %v (输出: %s)", s.Token, err, res.Stdout)
			continue
		}
		var v map[string]any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Errorf("%s: gen 输出不是合法 JSON: %v", s.Token, err)
			continue
		}
		if _, ok := v["inbounds"]; !ok {
			t.Errorf("%s: gen 输出缺少 inbounds 字段: %s", s.Token, string(raw))
		}
	}

	after, err := r.ConfDirDigest()
	if err != nil {
		t.Fatalf("读取 conf 指纹失败: %v", err)
	}
	if before != after {
		t.Errorf("gen 竟然写盘了! conf 目录指纹发生变化\n before=%q\n after =%q", before, after)
	}
	if md5File(t, r.confDir()+"/"+protectedConf) != protBefore {
		t.Errorf("保护节点 %s 被改动!", protectedConf)
	}
}

func TestMissingArgsDetectedAsInteractive(t *testing.T) {
	skipIfNoScript(t)
	r := NewRunner()
	r.Timeout = 15 * time.Second

	before, _ := r.ConfDirDigest()

	// 这三条都是实测确认会进 ask() 自旋的形态 (gen 是 dry-run, 不会落盘):
	//   vws 缺 host      -> "请输入域名:"
	//   door 缺 addr     -> "请输入目标地址:"
	//   door 缺 dest_port-> "请输入目标端口:"
	cases := [][]string{
		{"vws"},
		{"door", "23479"},
		{"door", "23478", "1.1.1.1"},
	}
	for _, args := range cases {
		start := time.Now()
		res, err := r.GenRaw(args...)
		elapsed := time.Since(start)

		if err == nil {
			t.Errorf("gen %v: 应当返回 InteractiveError, 却成功了", args)
			continue
		}
		if !errors.Is(err, ErrInteractive) {
			t.Errorf("gen %v: 应当返回 ErrInteractive, 实际: %v", args, err)
			continue
		}
		var ie *InteractiveError
		if !errors.As(err, &ie) {
			t.Errorf("gen %v: 错误类型不是 *InteractiveError", args)
		}
		if !res.Prompted {
			t.Errorf("gen %v: res.Prompted 应为 true", args)
		}
		if elapsed > 10*time.Second {
			t.Errorf("gen %v: 中断太慢 (%s), 提示检测可能没生效", args, elapsed)
		}
		t.Logf("gen %v -> 在 %s 内被中断, 等待=%q", args, elapsed.Round(time.Millisecond), ie.Prompt)
	}

	after, _ := r.ConfDirDigest()
	if before != after {
		t.Errorf("自旋被中断的 gen 竟然写盘了!\n before=%q\n after =%q", before, after)
	}
}

func TestBadValueFailsFastNotSpin(t *testing.T) {
	skipIfNoScript(t)
	r := NewRunner()
	r.Timeout = 15 * time.Second

	// 实测: 端口/uuid 非法时脚本走 err() -> exit 1, 不会自旋 (core.sh 1030-1055)。
	// 这条用来证明"自旋集合"确实只有缺参一种。
	_, err := r.GenRaw("tcp", "0", "11111111-2222-4333-8444-555555555555", "none")
	if err == nil {
		t.Fatal("端口 0 应当被脚本拒绝")
	}
	if errors.Is(err, ErrInteractive) {
		t.Errorf("端口 0 应当是快速失败 (ExitError), 却变成自旋: %v", err)
	}
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Errorf("应当返回 *ExitError, 实际: %T %v", err, err)
	}
}

func TestStatusParse(t *testing.T) {
	// 样本取自 82 上真实 `v2ray status` 输出形态
	sample := "V2Ray 5.53.0: running\n\nCaddy v2.11.4: running\n"
	st := &ServiceStatus{}
	for _, line := range strings.Split(sample, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m := reStatusLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		label := strings.TrimSpace(m[1])
		running := m[2] == "running"
		switch {
		case strings.HasPrefix(strings.ToLower(label), "v2ray"):
			st.CoreVersion = strings.TrimSpace(strings.TrimPrefix(label, "V2Ray"))
			st.CoreRunning = running
		case strings.HasPrefix(strings.ToLower(label), "caddy"):
			st.CaddyVersion = strings.TrimSpace(strings.TrimPrefix(label, "Caddy"))
			st.CaddyRunning = running
		}
	}
	if st.CoreVersion != "5.53.0" || !st.CoreRunning {
		t.Errorf("V2Ray 解析错误: %+v", st)
	}
	if st.CaddyVersion != "v2.11.4" || !st.CaddyRunning {
		t.Errorf("Caddy 解析错误: %+v", st)
	}
}

func TestDelAnchorsPattern(t *testing.T) {
	// del 走 `ls | grep -E -i "$2"`, 不锚定会匹配到多个文件并进 ask get_config_file 自旋。
	// 这里只验证锚定逻辑本身, 不真删。
	name := "VMess-TCP-23483.json"
	pattern := "^" + regexpQuoteMeta(name) + "$"
	if pattern != "^VMess-TCP-23483\\.json$" {
		t.Errorf("锚定结果异常: %q", pattern)
	}
}

// ---------------------------------------------------------------------------
// url 逐节点锚定 (回归锁)
//
// 背景: `v2ray url` **无参时不是批量导出**, 而是进配置选择菜单自旋。
// 调用链 url_qr $@ -> info $2 -> get info $1 -> `get file $2`, 其中 $2 是
// `ls $is_conf_dir | grep -E -i "$is_file_str"` 的**模式** (core.sh:1198-1208):
//   命中恰好 1 个 => 自动选中 (core.sh:1205), 命中 >=2 个 => ask get_config_file
//   => `while :; do echo -ne $提示; read REPLY; done`; Stdin 是 /dev/null,
//   EOF 下空转刷屏, 直到超时才被整组杀掉。
// 真机实测 (82, 双节点): 锚定 exit=0 / 314~340ms / 10 行; 无参 exit=124 /
// 6004ms / 1742 行 83463B 全是 "请选择配置:"。
//
// 真机跑一次要 20s+ 且结果随机器上的节点数漂移, 不适合当回归门。这里用桩把
// core.sh 那段 `ls | grep -E -i` 的**判定逻辑**照搬过来, 于是"必须锚定"这件事
// 变成因果必然: 不锚定 -> 命中 2 个 -> 走自旋 -> 超时。毫秒级、零副作用。
// ---------------------------------------------------------------------------

// urlStub 造一个假 v2ray 入口, 行为对齐 core.sh 的 get file / url_qr:
//   - 记录每次 argv 到 argv.log (供审计"到底带没带锚定模式");
//   - 按 $2 当 `grep -E -i` 模式去列 conf 目录, 命中数 != 1 就复现选择菜单自旋;
//   - 命中 1 个 => 输出真机形态的单节点帧 (含 ANSI 上色, 逼 ParseURLOutput 走真路径)。
func urlStub(t *testing.T, dir, confDir string) string {
	t.Helper()
	argvLog := filepath.Join(dir, "argv.log")
	body := `#!/bin/sh
printf '%s\n' "$*" >> '` + argvLog + `'
CONF='` + confDir + `'
if [ "$1" = "url" ]; then
	pat="$2"
	[ -z "$pat" ] && pat='.json$'
	matches=$(ls "$CONF" | grep -E -i "$pat" | sed '/dynamic-port-.*-link/d' | head -233)
	n=$(printf '%s\n' "$matches" | grep -c .)
	if [ "$n" -ne 1 ]; then
		i=0
		while [ $i -lt 60 ]; do
			printf '\n请选择配置:\n'
			printf '请选择 [1-2]:'
			sleep 0.1
			i=$((i+1))
		done
		exit 124
	fi
	printf '\n------------- %s & URL 链接 -------------\n\n' "$matches"
	printf '\033[45mvless://11111111-2222-4333-8444-555555555555@node1.example.com:55509?encryption=none&security=tls&type=ws&host=node1.example.com&path=%%2F11111111-2222-4333-8444-555555555555#233boy-ws-node1.example.com\033[0m\n'
	printf '\n------------- END -------------\n'
	exit 0
fi
exit 0
`
	p := filepath.Join(dir, "v2ray-stub")
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatalf("写桩二进制失败: %v", err)
	}
	return p
}

// TestURLsAnchorsPerNodeAndNeverSpins 是这次修复的回归锁:
// URLs() 必须**逐节点**用锚定模式调用, 且总耗时远低于 Timeout。
func TestURLsAnchorsPerNodeAndNeverSpins(t *testing.T) {
	dir := t.TempDir()
	confDir := filepath.Join(dir, "conf")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatalf("建 conf 目录失败: %v", err)
	}
	// 两个真节点 + 一个 dynamic-port 文件 (后者必须被 ConfFiles 过滤掉)
	for _, n := range []string{
		"VLESS-WS-TLS-node1.example.com.json",
		"VMess-TCP-23483.json",
		"dynamic-port-VMess-TCP-11111.json",
	} {
		if err := os.WriteFile(filepath.Join(confDir, n), []byte("{}"), 0o644); err != nil {
			t.Fatalf("写 %s 失败: %v", n, err)
		}
	}

	r := &Runner{Bin: urlStub(t, dir, confDir), ConfDir: confDir, Timeout: 5 * time.Second, Env: DefaultEnv()}

	start := time.Now()
	uris, warns, err := r.URLs()
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("URLs 返回错误: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("正常路径不应有告警, 得到: %v", warns)
	}
	// dynamic-port- 文件必须被跳过 => 恰好 2 条
	if len(uris) != 2 {
		t.Fatalf("应逐节点取到 2 条链接 (dynamic-port 被过滤), 实际 %d 条: %+v", len(uris), uris)
	}
	// 关键断言: 绝不能挂满 Timeout。桩里不锚定就会自旋 6s > 5s Timeout。
	if elapsed > 2*time.Second {
		t.Errorf("URLs 耗时 %s —— 已接近/超过 Timeout(%s), 说明走了无参自旋分支", elapsed, r.Timeout)
	}

	// —— argv 审计: 每次调用都必须带锚定模式 ——
	raw, err := os.ReadFile(filepath.Join(dir, "argv.log"))
	if err != nil {
		t.Fatalf("读取 argv 记录失败: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("应恰好调用 2 次 (每节点一次), 实际 %d 次: %q", len(lines), lines)
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "url ") {
			t.Errorf("argv 不是 url 调用: %q", line)
			continue
		}
		pat := strings.TrimPrefix(line, "url ")
		if !strings.HasPrefix(pat, "^") || !strings.HasSuffix(pat, "$") {
			t.Errorf("❌ url 的模式没有锚定 (会命中多个文件 -> ask 自旋): %q", pat)
		}
	}
	// 锚定内容必须是正则转义后的文件名 (点号要转义, 否则 `.` 会当通配符)
	if !strings.Contains(string(raw), `^VLESS-WS-TLS-node1\.example\.com\.json$`) {
		t.Errorf("未看到期望的锚定模式, argv 实际为: %q", lines)
	}

	// —— 解析结果复核: 帧头 + ANSI 上色的 vless 链接都要被正确取到 ——
	for _, u := range uris {
		if u.ConfName == "" {
			t.Errorf("ConfName 为空: %+v", u)
		}
		if !strings.HasPrefix(u.URI, "vless://") {
			t.Errorf("URI 未解析出 vless 链接 (ANSI 没剥干净?): %q", u.URI)
		}
		if strings.Contains(u.URI, "\x1b") {
			t.Errorf("URI 里还残留 ANSI 转义: %q", u.URI)
		}
	}
	if uris[0].ConfName != "VLESS-WS-TLS-node1.example.com.json" {
		t.Errorf("ConfName = %q (应为帧头里的真实文件名, 不是转义后的模式)", uris[0].ConfName)
	}
	t.Logf("✅ %d 个节点逐条锚定取链接, 耗时 %s (Timeout=%s)", len(uris), elapsed.Round(time.Millisecond), r.Timeout)
}

// TestURLNoArgSpinsAndIsKilledNow 是上面那条的**负控**: 证明"无参 url 会自旋"
// 这个前提仍然成立, 且 URL() 开启的 detectPrompt 能在提示出现后立刻杀掉进程组,
// 而不是白等满 Timeout。哪天 detectPrompt 被改回 false, 这条会立刻红。
func TestURLNoArgSpinsAndIsKilledNow(t *testing.T) {
	dir := t.TempDir()
	confDir := filepath.Join(dir, "conf")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatalf("建 conf 目录失败: %v", err)
	}
	// 双节点: 无参时命中 2 个 -> 必然自旋
	for _, n := range []string{"VLESS-WS-TLS-node1.example.com.json", "VMess-TCP-23483.json"} {
		if err := os.WriteFile(filepath.Join(confDir, n), []byte("{}"), 0o644); err != nil {
			t.Fatalf("写 %s 失败: %v", n, err)
		}
	}
	r := &Runner{Bin: urlStub(t, dir, confDir), ConfDir: confDir, Timeout: 10 * time.Second, Env: DefaultEnv()}

	// 参数闸门: 空文件名必须在调用脚本前就被拦住 (连桩都不该被碰)
	if _, _, err := r.URL("  "); err == nil {
		t.Error("URL(\"  \") 应当直接报错, 却放行了")
	} else if !strings.Contains(err.Error(), "配置文件名") {
		t.Errorf("错误信息不明确: %v", err)
	}

	// 真的走一次无参调用 (exec 是本包私有, 测试同包可直接调)
	start := time.Now()
	res, err := r.exec([]string{"url"}, true)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrInteractive) {
		t.Fatalf("无参 url 应当被判为自旋 (ErrInteractive), 实际: %T %v", err, err)
	}
	if !res.Prompted {
		t.Errorf("res.Prompted 应为 true: %+v", res)
	}
	// promptBurstHits=2, 桩每轮写两次提示 => 第一轮内就该被杀掉, 远早于 10s。
	if elapsed > 3*time.Second {
		t.Errorf("自旋未被及时打断, 耗时 %s (Timeout=%s)", elapsed, r.Timeout)
	}
	t.Logf("✅ 无参 url 在 %s 内被识别为自旋并杀掉 (Timeout=%s), 未白等", elapsed.Round(time.Millisecond), r.Timeout)
}
