package agent

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"v2panel/internal/common"
)

// ===========================================================================
// M1 真机生命周期测试 —— Bug A / Bug B 的证明
//
// 本文件对生产机**有写操作**, 但只在**自己新建的临时节点**上做, 用完立刻删掉:
//
//	Create(vws + no-auto-tls, host=v2p-e2e.example.com) → Delete
//
// 为什么必须真机跑: 两个 bug 的成因都在 233boy 的 shell 分支上, 手写假数据
// 完全绕开了它们 —— 这正是它们此前零测试覆盖的原因。
//
//	Bug A: `del` CLI 路径上 is_dont_auto_exit 未置位, del() 最后一句
//	       `[[ $is_dont_auto_exit ]] && unset is_config_file` (core.sh:767) 测试为假,
//	       函数返回 1 => 删除成功也报错。Driver.Delete 必须只信 conf 文件消失。
//	Bug B: no-auto-tls 节点没有 Caddy 站点, 但 url 层会把机器全局 https_port
//	       (82 上 55509) 硬编码进客户端链接 (core.sh:1252/1525/1533/1536),
//	       于是 PublicPort 非 0、旧的告警分支成了死代码。Driver/parser 必须用
//	       conf 层 listen 判定「必须经 Caddy」, 并清掉这个假端口 + 告警。
//
// 红线: 不碰 tzq1 / ch / qy。host-test 在 is_no_auto_tls 时直接 return
// (core.sh:1402-1403), 所以这个不存在的域名不会引起 DNS 交互自旋。
// ===========================================================================
const (
	// 刻意选一个**不解析**的域名: 这是 Bug B 的复现条件 (Caddy 站点建不起来)。
	// 实测 `getent hosts v2p-e2e.example.com` 无结果, 但 reDomain/ValidateDomain 接受它。
	e2eHost = "v2p-e2e.example.com"
	// 82 上没有 uuidgen, 只能写死一个字面量 (形状匹配 common 的 reUUID)。
	e2eUUID = "8f0c1d2e-3a4b-4c5d-8e9f-0a1b2c3d4e5f"
	e2ePath = "/v2p-e2e-probe"

	e2eConfName = "VLESS-WS-TLS-v2p-e2e.example.com.json"
)

// e2eSpec 本次生命周期用的建节点参数 (TLS 协议 + 手动 tls = 不建 Caddy 站点)。
func e2eSpec() common.AddSpec {
	return common.AddSpec{
		Token:     "vws",
		Host:      e2eHost,
		UUID:      e2eUUID,
		Path:      e2ePath,
		NoAutoTLS: true,
	}
}

// digestDir 与 Runner.ConfDirDigest 同构的目录指纹 (common 里没有 Caddy 版,
// 所以这里本地实现一份, 用于断言「Caddy 目录零污染」)。
func digestDir(dir string) string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "ERR:" + err.Error()
	}
	lines := make([]string, 0, len(ents))
	for _, e := range ents {
		info, err := e.Info()
		if err != nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s|%d|%d", e.Name(), info.Size(), info.ModTime().UnixNano()))
	}
	sort.Strings(lines)
	sum := md5.Sum([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// stepByName 按步骤名取回步骤记录 (找不到返回 nil)。
func stepByName(res *CreateResult, name string) *Step {
	if res == nil {
		return nil
	}
	for i := range res.Steps {
		if res.Steps[i].Name == name {
			return &res.Steps[i]
		}
	}
	return nil
}

// dumpSteps 失败时把完整步骤链打出来, 免去再上机复现。
func dumpSteps(t *testing.T, res *CreateResult) {
	t.Helper()
	if res == nil {
		t.Logf("  (结果为 nil)")
		return
	}
	for i, s := range res.Steps {
		mark := "✅"
		if !s.OK {
			mark = "❌"
		}
		t.Logf("  %s [%d] %-14s %-8s %s", mark, i, s.Name, s.Duration.Round(time.Millisecond), truncate(s.Detail, 200))
	}
	for _, w := range res.Warnings {
		t.Logf("  ⚠️  %s", truncate(w, 200))
	}
}

// ---------------------------------------------------------------- 真机测试闸门

// requireRealMachine 让整个文件的真机测试**默认跳过**。
//
// 为什么默认跳过而不是默认跑:
//   - 这些测试**对生产机有写操作** (会在 /etc/v2ray/conf 建临时节点再删),
//     别人 clone 下来 `go test ./...` 不该被写机器;
//   - 它们还依赖**本机部署特有**的东西: 已存在的 conf 名、能解析的域名、
//     保护名单里的具体节点名。换个环境跑必然失败, 而失败信息看起来像"代码坏了",
//     实际只是"环境不对" —— 这种误导比不跑更糟。
//
// 要跑就显式开:
//
//	V2PANEL_REAL_TEST=1 go test ./internal/agent/ -run TestRealMachine -v
func requireRealMachine(t *testing.T) {
	t.Helper()
	if os.Getenv("V2PANEL_REAL_TEST") != "1" {
		t.Skip("真机测试默认跳过 (会写 /etc/v2ray/conf)。要跑: V2PANEL_REAL_TEST=1 go test ./internal/agent/ -run TestRealMachine -v")
	}
	if _, err := os.Stat(common.DefaultBinPath); err != nil {
		t.Skipf("未找到 233boy 入口 %s", common.DefaultBinPath)
	}
}

// ---------------------------------------------------------------- 保护名单 (纯函数, 无副作用)

// TestRealMachine_ProtectedGuard 只验证 guard 本身, 不走 Create。
//
// ⚠️ 刻意不通过 Create 来测: 万一 guard 的调用顺序被人改到 gen 之后, 这个测试
// 就会真的对 tzq1 执行 `add`, 而 create() 会先 `del $is_config_file` 再写新配置
// —— 那会把线上节点删掉。直接调 guard 是零风险的等价证明。
func TestRealMachine_ProtectedGuard(t *testing.T) {
	requireRealMachine(t)
	d := NewDriver()
	// 保护名单用**通用形态**而不是某台机器的真实节点名 —— 这个测试验证的是
	// guard 的子串匹配逻辑, 与具体叫什么无关。
	d.Protected = []string{"protected-a", "protected-b"}

	protected := []string{
		tzqConfName,
		"VLESS-WS-TLS-node1.example.com.json",
		"VMess-WS-TLS-ch.wael.top.json",
		"Trojan-gRPC-TLS-qy.wael.top.json",
	}
	for _, name := range protected {
		err := d.guard(name)
		if err == nil {
			t.Errorf("❌ %s 应当被保护名单拦住, 但 guard 放行了", name)
			continue
		}
		if !errors.Is(err, ErrProtected) {
			t.Errorf("❌ %s 的错误不是 ErrProtected: %v", name, err)
		}
		if !strings.Contains(err.Error(), "保护名单") {
			t.Errorf("❌ %s 的错误信息缺少「保护名单」字样: %v", name, err)
		}
	}

	// 反向: 本次要建的临时节点必须**不被**拦截, 否则整个生命周期测试跑不起来。
	if err := d.guard(e2eConfName); err != nil {
		t.Errorf("❌ 临时测试节点 %s 被误拦: %v", e2eConfName, err)
	}
	// 空名单 = 全部放行 (面板默认不设保护时不应误伤)
	empty := NewDriver()
	empty.Protected = []string{"", "   "}
	if err := empty.guard(tzqConfName); err != nil {
		t.Errorf("❌ 空白保护项不应拦截任何节点, 却拦了 %s: %v", tzqConfName, err)
	}
	t.Logf("✅ 保护名单闸门正确: %d 个线上节点被拦, 临时节点放行", len(protected))
}

// ---------------------------------------------------------------- 真机生命周期 (Bug A + Bug B)

// TestRealMachine_DriverLifecycle 建一个 no-auto-tls 的临时节点再删掉,
// 用真实机器证明 Bug A / Bug B 两个修复都生效。
//
// 单独跑: go test ./internal/agent/ -v -run 'TestRealMachine_DriverLifecycle'
func TestRealMachine_DriverLifecycle(t *testing.T) {
	requireRealMachine(t)
	skipIfNoRealLayers(t)

	d := NewDriver()
	d.Protected = []string{"tzq1", "ch", "qy"}

	// —— 前置: 确认参数推导与脚本口径一致 (ConfName 决定后面所有断言的名字) ——
	spec := e2eSpec()
	confName, err := spec.ConfName()
	if err != nil {
		t.Fatalf("ConfName 推导失败: %v", err)
	}
	if confName != e2eConfName {
		t.Fatalf("配置名推导结果 %q 与预期 %q 不符", confName, e2eConfName)
	}
	args, def, err := spec.BuildArgs()
	if err != nil {
		t.Fatalf("BuildArgs 失败: %v", err)
	}
	t.Logf("即将执行: v2ray no-auto-tls %s (协议 %s, IsTLS=%v)", strings.Join(args, " "), def.Name, def.IsTLS)
	if !def.IsTLS {
		t.Fatalf("vws 应当是 TLS 协议 (no-auto-tls 只对 TLS 协议有意义)")
	}

	// —— 前置: 清掉上一次失败运行可能残留的同名 conf ——
	pre, err := d.runner().ConfFiles()
	if err != nil {
		t.Fatalf("读取 conf 目录失败: %v", err)
	}
	if containsStr(pre, confName) {
		t.Logf("⚠️  发现上次运行残留的 %s, 先清掉再开始", confName)
		if _, derr := d.Delete(confName); derr != nil {
			t.Fatalf("清理残留配置失败, 中止以免污染生产: %v", derr)
		}
	}

	// —— 基线快照 ——
	beforeConf, err := d.runner().ConfDirDigest()
	if err != nil {
		t.Fatalf("conf 目录指纹失败: %v", err)
	}
	beforeCaddy := digestDir(realCaddyDir)
	t.Logf("基线: conf=%s caddy=%s", beforeConf, beforeCaddy)

	// 无论测试怎么退出, 都要保证不留半成品 (这是对生产机的硬承诺)。
	t.Cleanup(func() {
		files, ferr := d.runner().ConfFiles()
		if ferr != nil {
			t.Errorf("cleanup: 读取 conf 目录失败: %v", ferr)
			return
		}
		if !containsStr(files, confName) {
			return
		}
		t.Logf("🧹 cleanup: 测试未正常删除 %s, 补删", confName)
		if _, derr := d.Delete(confName); derr != nil {
			t.Errorf("🧹 cleanup 删除失败, 生产机上残留了 %s: %v", confName, derr)
		}
	})

	// ===================== Create =====================
	t.Log("—— 步骤 1/2: Create (no-auto-tls, 域名不解析) ——")
	res, err := d.Create(spec)
	dumpSteps(t, res)
	if err != nil {
		t.Fatalf("❌ Create 失败: %v", err)
	}

	// 每一步都必须成功 (gen / add / 落盘 / 读 conf / 解析 / 刷新三层)
	for _, s := range res.Steps {
		if !s.OK {
			t.Errorf("❌ Create 步骤 %q 失败: %s", s.Name, s.Detail)
		}
	}

	// —— 真实落盘 ——
	afterCreate, err := d.runner().ConfDirDigest()
	if err != nil {
		t.Fatalf("Create 后 conf 指纹失败: %v", err)
	}
	if afterCreate == beforeConf {
		t.Errorf("❌ conf 目录指纹没变, 说明节点根本没落盘")
	}
	files, _ := d.runner().ConfFiles()
	if !containsStr(files, confName) {
		t.Fatalf("❌ %s 未出现在 conf 目录: %v", confName, files)
	}

	// —— 内网口是脚本随机分配的, 必须从 conf 层读回, 且不能撞上 tzq1 的 45341 ——
	if res.InternalPort <= 0 {
		t.Errorf("❌ 内网口未读回 (InternalPort=%d)", res.InternalPort)
	}
	if res.InternalPort == tzqInternal {
		t.Errorf("❌ 内网口 %d 与 tzq1 的 %d 撞了, 说明没按 conf 层读回", res.InternalPort, tzqInternal)
	}
	// conf 内容复核: 必须是 127.0.0.1 监听 (ws 系必须经 Caddy) + 我们给的 uuid
	raw, err := d.runner().ReadConf(confName)
	if err != nil {
		t.Fatalf("读取新 conf 失败: %v", err)
	}
	cn, _, err := ParseConfJSON(confName, d.Paths.ConfDir+"/"+confName, raw)
	if err != nil {
		t.Fatalf("解析新 conf 失败: %v", err)
	}
	if cn.Listen != common.ListenLoopback {
		t.Errorf("❌ ws 协议 conf 的 listen 应为 %s, 实际 %q", common.ListenLoopback, cn.Listen)
	}
	if cn.UUID != e2eUUID {
		t.Errorf("❌ conf 里的 uuid 应为 %s, 实际 %q", e2eUUID, cn.UUID)
	}

	// ===================== Bug B =====================
	// 该节点没有 Caddy 站点 (域名不解析), 但 url 层仍会谎报机器全局 https_port。
	// 修复后的正确行为: 明确告警 + 把假端口清成 0。
	if !hasWarnContaining(res.Warnings, "没有公网入口") {
		t.Errorf("❌ [Bug B] 未给出「没有公网入口」告警; Warnings=%v", res.Warnings)
	}
	if res.PublicPort != 0 {
		t.Errorf("❌ [Bug B] 假公网口未被清掉: PublicPort=%d (应为 0, 否则 UI 会把它当可用节点)", res.PublicPort)
	}
	if !hasWarnContaining(res.Warnings, "机器全局 https_port") {
		t.Logf("ℹ️  [Bug B] 未命中「机器全局 https_port」细化说明 (url 层可能没输出该节点), 主告警已成立")
	}
	// url 层确实给了个端口 (55509), 才叫「谎报」; 这里把事实记下来做证据。
	t.Logf("ℹ️  [Bug B] 修复后 PublicPort=%d Address=%q Path=%q", res.PublicPort, res.Address, res.Path)
	if res.Address != e2eHost {
		t.Errorf("❌ Address 应为 %q, 实际 %q", e2eHost, res.Address)
	}

	// —— Caddy 目录零污染: no-auto-tls 的整个意义就是不走 Caddy ——
	afterCaddy := digestDir(realCaddyDir)
	if afterCaddy != beforeCaddy {
		t.Errorf("❌ [Bug B] no-auto-tls 却动了 Caddy 目录: before=%s after=%s", beforeCaddy, afterCaddy)
	}

	// —— 线上节点不受影响 ——
	tzqRaw, err := d.runner().ReadConf(tzqConfName)
	if err != nil {
		t.Fatalf("❌ 线上 tzq1 配置读取失败 (被误伤?): %v", err)
	}
	if !strings.Contains(string(tzqRaw), tzqUUID) {
		t.Errorf("❌ 线上 tzq1 配置内容变了 (uuid 不见了)")
	}

	// ===================== Delete (Bug A) =====================
	t.Log("—— 步骤 3: Delete (233boy del 成功也返回非 0 退出码) ——")
	res2, err := d.Delete(confName)
	dumpSteps(t, res2)
	if err != nil {
		t.Fatalf("❌ [Bug A] Delete 报错, 但文件很可能已删掉 —— 正是被修复的那个 bug: %v", err)
	}

	// 「del 删除」这一步必须记录到非 0 退出码, 才算真的踩到了这个 quirk
	// (如果 Detail 为空, 说明这次没复现, 断言就是空转, 必须报出来)。
	delStep := stepByName(res2, "del 删除")
	if delStep == nil {
		t.Fatalf("❌ 缺少「del 删除」步骤记录: %+v", res2.Steps)
	}
	if !delStep.OK {
		t.Errorf("❌ 「del 删除」步骤被标记为失败: %s", delStep.Detail)
	}
	if delStep.Detail == "" {
		t.Errorf("❌ [Bug A] 「del 删除」的 Detail 为空 —— 本次没复现非 0 退出码, 该断言失去意义 (退出码被当成判据了?)")
	} else if !strings.Contains(delStep.Detail, "退出码非 0") {
		t.Errorf("❌ [Bug A] Detail 未说明退出码问题: %q", delStep.Detail)
	}
	verifyStep := stepByName(res2, "校验已删除")
	if verifyStep == nil || !verifyStep.OK {
		t.Errorf("❌ 「校验已删除」步骤缺失或失败: %+v", verifyStep)
	}

	// —— 真实消失 + 目录回到基线 ——
	afterDel, err := d.runner().ConfDirDigest()
	if err != nil {
		t.Fatalf("Delete 后 conf 指纹失败: %v", err)
	}
	files, _ = d.runner().ConfFiles()
	if containsStr(files, confName) {
		t.Fatalf("❌ %s 删除后仍在目录内: %v", confName, files)
	}
	if afterDel != beforeConf {
		t.Errorf("❌ [Bug A] 删除后 conf 目录未回到基线: before=%s after=%s (可能残留了 .json 之外的文件)", beforeConf, afterDel)
	}
	if digestDir(realCaddyDir) != beforeCaddy {
		t.Errorf("❌ 删除后 Caddy 目录变了")
	}
	if !containsStr(files, tzqConfName) {
		t.Fatalf("❌ 线上 tzq1 配置不见了! 当前目录: %v", files)
	}

	t.Logf("✅ 生命周期通过: 建→删→目录回到基线; 剩余 %d 个配置 (线上节点完好)", len(files))
}

// ---------------------------------------------------------------- 公网探测 (只读)

// TestRealMachine_ProbeTzq1 从本机按客户端方式连一次线上入口, 必须拿到 101。
// 只读: 不发任何写命令。
func TestRealMachine_ProbeTzq1(t *testing.T) {
	requireRealMachine(t)
	skipIfNoRealLayers(t)

	d := NewDriver()
	t0 := time.Now()
	pr := d.ProbePublic("127.0.0.1", tzqPublicPort, tzqDomain, tzqPath, 8*time.Second)
	t.Logf("探测 tzq1: %+v (耗时 %s)", pr, time.Since(t0).Round(time.Millisecond))

	if pr.Err != "" {
		t.Errorf("❌ 探测出错: %s", pr.Err)
	}
	if pr.StatusCode != 101 {
		t.Errorf("❌ 期望 101 Switching Protocols, 实际 %d (协议行 %q)", pr.StatusCode, pr.Protocol)
	}
	if !pr.Reachable {
		t.Errorf("❌ Reachable=false")
	}
	// 证书应当是 Let's Encrypt 给 tzq1 签的, 不是 CF Origin
	if pr.CertCN != tzqDomain {
		t.Errorf("❌ 证书 CN 应为 %q, 实际 %q", tzqDomain, pr.CertCN)
	}
	if !strings.Contains(strings.ToLower(pr.CertIssuer), "let's encrypt") &&
		!strings.Contains(strings.ToLower(pr.CertIssuer), "r3") &&
		!strings.Contains(strings.ToLower(pr.CertIssuer), "r1") &&
		!strings.Contains(strings.ToLower(pr.CertIssuer), "e5") &&
		!strings.Contains(strings.ToLower(pr.CertIssuer), "e6") {
		t.Logf("ℹ️  证书签发者: %q (若不是 LE 请留意)", pr.CertIssuer)
	}
	if pr.CertNotAfter.IsZero() {
		t.Errorf("❌ 未读回证书有效期")
	} else {
		t.Logf("ℹ️  证书: CN=%s 签发者=%s 到期=%s", pr.CertCN, pr.CertIssuer, pr.CertNotAfter.Format(time.RFC3339))
	}
}
