package agent

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"v2panel/internal/common"
)

// ===========================================================================
// 233boy 节点驱动器
//
// 职责: 把「面板上点一下」翻译成对 233boy CLI 的安全调用序列, 并在每一步之后
// 用**独立的事实来源**校验, 而不是相信脚本的退出码。
//
// 建节点的事实链 (每一步都必须成立, 缺一步就回滚):
//
//	gen <完整参数>   →  dry-run, 拿到脚本"将"写入的 JSON。不落盘。
//	                     ⚠️ TLS 协议的内网口是脚本随机分配的, gen 与 add 各随机一次,
//	                     所以 gen 里的端口 ≠ add 后的端口 —— 只用于"参数能否被脚本接受"。
//	add <完整参数>   →  落盘 conf + 建 Caddy 站点 + 重启内核
//	conf 目录出现新文件 → 真实事实 (唯一可信的"建成"信号)
//	解析 conf        →  拿回**真实**内网口 / uuid / 协议
//	刷新 Caddy+url 层 →  拿回公网口 / path / 客户端链接
//
// 为什么不信退出码: 233boy 脚本全篇没有 set -e, 多处失败只 warn 不 exit
// (例如 Caddy 站点建失败、API 调用失败都只是打一行警告), 退出码仍是 0。
// ===========================================================================

// ErrProtected 受保护节点不允许改动 (tzq1 / ch / qy 这类线上节点)。
var ErrProtected = errors.New("该节点在保护名单内, 拒绝改动")

// Step 一次操作中的单个步骤记录, 用于前端展示"做了什么"。
type Step struct {
	Name     string        // 步骤名
	OK       bool          // 是否成功
	Detail   string        // 补充信息 (参数/错误)
	Duration time.Duration // 耗时
}

// CreateResult 建节点的完整结果。
type CreateResult struct {
	ConfName     string
	InternalPort int
	Address      string
	PublicPort   int
	Path         string
	URI          string
	CaddyFile    string
	Steps        []Step
	Warnings     []string
	RolledBack   bool // 失败后是否已回滚 (删除半成品 conf)
}

// Driver 233boy 驱动器。
//
// Protected: 受保护节点名列表, 用**子串**匹配 (给 "tzq1" 即可挡住
// "VLESS-WS-TLS-node1.example.com.json")。任何写操作前都过这道闸。
type Driver struct {
	Runner    *common.Runner
	Paths     LayerPaths
	Protected []string
}

// NewDriver 用默认路径构造驱动器。
func NewDriver() *Driver {
	return &Driver{
		Runner: common.NewRunner(),
		Paths:  DefaultLayerPaths(),
	}
}

// IsProtected 判断节点名是否命中保护名单。
func (d *Driver) IsProtected(confName string) bool {
	for _, p := range d.Protected {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.Contains(confName, p) {
			return true
		}
	}
	return false
}

// guard 写操作前的统一闸门。
func (d *Driver) guard(confName string) error {
	if d.IsProtected(confName) {
		return fmt.Errorf("%w: %s (保护名单 %v)", ErrProtected, confName, d.Protected)
	}
	return nil
}

func (d *Driver) runner() *common.Runner {
	if d.Runner != nil {
		return d.Runner
	}
	return common.NewRunner()
}

// ---------------------------------------------------------------- 只读

// Snapshot 采集当前机器的三层全景。
func (d *Driver) Snapshot() (*Snapshot, error) {
	return Collect(d.Paths, d.runner())
}

// Capabilities 运行时能力探测。
type Capabilities struct {
	Protocols []common.ProtocolDef
	// Reality v4.27 的 reality 分支在 add() 里被注释掉了, 实测
	// `gen vless-reality auto` → "错误! 无法识别 (vless-reality)"。
	// 这里如实上报 false, 由 UI 灰掉并说明原因 —— 绝不 patch 脚本去打开它。
	Reality    bool
	RealityWhy string
	HasCaddy   bool
}

// Capabilities 探测本机 233boy 支持的能力。
func (d *Driver) Capabilities() Capabilities {
	_, err := d.runner().GenRaw("vless-reality", "auto")
	caps := Capabilities{
		Protocols: common.ProtocolDefs(),
		Reality:   err == nil,
	}
	if !caps.Reality {
		caps.RealityWhy = "233boy v4.27 已注释掉 reality 分支 (add() 的 r|reality 分支不可达); 不要 patch 脚本, 升级会被覆盖"
	}
	if st, _, err := d.runner().Status(); err == nil && st != nil {
		caps.HasCaddy = st.CaddyRunning
	}
	return caps
}

// Preview 建节点前的预校验: 参数完整性 + 脚本能否接受。
//
// ⚠️ 返回的 JSON 里的端口是**这一次**随机的结果, 与真正 add 之后的端口不同
// (TLS 协议由脚本随机分配)。仅用于"参数是否合法", 不要拿它当最终配置。
func (d *Driver) Preview(spec common.AddSpec) (string, string, error) {
	confName, err := spec.ConfName()
	if err != nil {
		return "", "", err
	}
	if err := d.guard(confName); err != nil {
		return "", "", err
	}
	raw, _, err := d.runner().Gen(spec)
	if err != nil {
		return "", "", err
	}
	return confName, string(raw), nil
}

// ---------------------------------------------------------------- 写

// Create 建节点。返回前保证: conf 文件真实存在且可被解析。
//
// 任何一步失败都会尝试回滚 (删掉本次新产生的 conf), 避免留下半成品节点 ——
// 半成品会让 `v2ray url` 输出一个连不上的链接, 比直接失败更糟。
func (d *Driver) Create(spec common.AddSpec) (*CreateResult, error) {
	res := &CreateResult{}
	add := func(name string, ok bool, detail string, dur time.Duration) {
		res.Steps = append(res.Steps, Step{Name: name, OK: ok, Detail: detail, Duration: dur})
	}

	confName, err := spec.ConfName()
	if err != nil {
		add("推导配置名", false, err.Error(), 0)
		return res, err
	}
	res.ConfName = confName
	if err := d.guard(confName); err != nil {
		add("保护名单检查", false, err.Error(), 0)
		return res, err
	}

	// —— 步骤 1: gen dry-run (参数完整性与合法性) ——
	t0 := time.Now()
	if _, _, err := d.runner().Gen(spec); err != nil {
		add("gen 预校验", false, err.Error(), time.Since(t0))
		return res, fmt.Errorf("gen 预校验失败 (未做任何改动): %w", err)
	}
	add("gen 预校验", true, "脚本接受该参数组合", time.Since(t0))

	// —— 步骤 2: add ——
	before, _ := d.runner().ConfFiles()
	t1 := time.Now()
	addRes, addErr := d.runner().Add(spec)
	newFiles := []string(nil)
	if addRes != nil {
		newFiles = addRes.NewConfFiles
	}
	if addErr != nil {
		add("add 建节点", false, addErr.Error(), time.Since(t1))
		if len(newFiles) > 0 {
			res.RolledBack = d.rollback(newFiles)
			add("回滚", res.RolledBack,
				fmt.Sprintf("删除半成品 %v", newFiles), 0)
		}
		return res, addErr
	}
	add("add 建节点", true, fmt.Sprintf("新配置 %v", newFiles), time.Since(t1))

	// —— 步骤 3: conf 文件必须真实出现 (唯一可信的"建成"信号) ——
	t2 := time.Now()
	after, _ := d.runner().ConfFiles()
	if !containsStr(after, confName) {
		// 脚本退出码为 0 但没落盘 —— 说明走了某条只 warn 不 exit 的失败分支。
		// 常见原因: 域名解析不了 / 端口被占 / 该 host 已有同名配置。
		add("校验 conf 落盘", false, fmt.Sprintf("%s 未出现, 目录内: %v", confName, after), time.Since(t2))
		// 尝试用新出现的文件回滚 (名字可能与推导不同, 例如脚本改了后缀)
		if extra := diffFiles(before, after); len(extra) > 0 {
			res.RolledBack = d.rollback(extra)
			add("回滚", res.RolledBack, fmt.Sprintf("删除 %v", extra), 0)
		}
		return res, fmt.Errorf("add 退出码为 0 但 %s 未落盘 (脚本静默失败, 检查域名解析/端口占用/同名配置)", confName)
	}
	add("校验 conf 落盘", true, confName, time.Since(t2))

	// —— 步骤 4: 解析 conf 拿回真实内网口与凭据 ——
	t3 := time.Now()
	data, err := d.runner().ReadConf(confName)
	if err != nil {
		add("读取 conf", false, err.Error(), time.Since(t3))
		return res, err
	}
	cn, warns, err := ParseConfJSON(confName, d.Paths.ConfDir+"/"+confName, data)
	if err != nil {
		add("解析 conf", false, err.Error(), time.Since(t3))
		return res, fmt.Errorf("节点已建好但配置解析失败: %w", err)
	}
	res.InternalPort = cn.Port
	res.Warnings = append(res.Warnings, warns...)
	add("解析 conf", true, fmt.Sprintf("内网口 %d, 协议 %s", cn.Port, cn.Protocol), time.Since(t3))

	// —— 步骤 5: 刷新 Caddy + url 层, 拿公网口 / path / 客户端链接 ——
	t4 := time.Now()
	snap, err := d.Snapshot()
	if err != nil {
		add("刷新三层", false, err.Error(), time.Since(t4))
		res.Warnings = append(res.Warnings, "节点已建好, 但三层刷新失败: "+err.Error())
		return res, nil // 节点是好的, 只是拿不到展示信息
	}
	view := snap.FindNode(confName)
	if view == nil {
		add("刷新三层", false, "合并结果里找不到该节点", time.Since(t4))
		res.Warnings = append(res.Warnings, "节点已建好, 但三层合并未匹配到它")
		return res, nil
	}
	res.Address = view.Address
	res.PublicPort = view.PublicPort
	res.Path = view.Path
	res.URI = view.URI
	res.CaddyFile = view.CaddyFile
	res.Warnings = append(res.Warnings, view.Warnings...)
	add("刷新三层", true,
		fmt.Sprintf("%s:%d%s", view.Address, view.PublicPort, view.Path), time.Since(t4))

	// 需要前置反代的协议 (ws/h2/grpc/http, conf 监听 127.0.0.1) 必须有 Caddy 站点, 否则客户端连不上。
	// 脚本自身只 warn 不 exit, 所以这里用 conf 层 listen + Caddy 层命中情况独立复核一遍;
	// parser.Merge 对同一情形也会告警 (带端口细节), 用 hasWarnContaining 去重, 避免刷两条。
	if cn.Listen == common.ListenLoopback && !view.HasCaddy && !hasWarnContaining(res.Warnings, "没有公网入口") {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"该节点没有公网入口: conf 监听 %s 必须经 Caddy 反代, 但 Caddy 层没有对应站点 —— 通常是域名未解析到本机导致 Caddy 站点没建起来", cn.Listen))
	}
	return res, nil
}

// Delete 删节点 (conf + Caddy 站点由脚本一并清理), 删完校验文件确实消失。
func (d *Driver) Delete(confName string) (*CreateResult, error) {
	res := &CreateResult{ConfName: confName}
	add := func(name string, ok bool, detail string, dur time.Duration) {
		res.Steps = append(res.Steps, Step{Name: name, OK: ok, Detail: detail, Duration: dur})
	}
	confName = strings.TrimSpace(confName)
	if confName == "" {
		return res, errors.New("删除节点需要给出配置文件名")
	}
	if err := d.guard(confName); err != nil {
		add("保护名单检查", false, err.Error(), 0)
		return res, err
	}

	before, _ := d.runner().ConfFiles()
	if !containsStr(before, confName) {
		add("确认存在", false, fmt.Sprintf("%s 不在 conf 目录内", confName), 0)
		return res, fmt.Errorf("配置 %s 不存在", confName)
	}

	t0 := time.Now()
	// ⚠️ 不能拿退出码当判据: 233boy 的 del() 最后一句是
	// `[[ $is_dont_auto_exit ]] && unset is_config_file` (core.sh:767), CLI 路径上该测试为假
	// => 删除成功时退出码也是 1 (已实测)。这里把退出码降级成提示, 成功与否只看下面的 ConfFiles() 比对
	// (与 Create 只信「conf 文件出现」同一原则)。
	delNote := ""
	if _, err := d.runner().Del(confName); err != nil {
		delNote = "脚本退出码非 0 (233boy del 成功也返回 1, 属已知行为): " + err.Error()
	}
	add("del 删除", true, delNote, time.Since(t0))

	t1 := time.Now()
	after, _ := d.runner().ConfFiles()
	if containsStr(after, confName) {
		add("校验已删除", false, confName+" 仍在目录内", time.Since(t1))
		return res, fmt.Errorf("%s 删除后仍然存在 (脚本静默失败)", confName)
	}
	add("校验已删除", true, fmt.Sprintf("剩余 %d 个配置", len(after)), time.Since(t1))
	return res, nil
}

// rollback 尽力删除给定配置 (用于清理半成品)。返回是否全部清干净。
func (d *Driver) rollback(names []string) bool {
	ok := true
	for _, n := range names {
		if d.IsProtected(n) {
			ok = false
			continue
		}
		// 同 Delete: del 成功也可能返回非 0 退出码, 不作为失败判据;
		// 是否真清干净由下面的 ConfFiles() 比对决定。
		_, _ = d.runner().Del(n)
	}
	after, _ := d.runner().ConfFiles()
	for _, n := range names {
		if containsStr(after, n) {
			ok = false
		}
	}
	return ok
}

// ---------------------------------------------------------------- 公网可达性探测

// ProbeResult 一次公网探测的结果。
type ProbeResult struct {
	Reachable    bool
	StatusCode   int
	ServerName   string
	CertCN       string
	CertIssuer   string
	CertNotAfter time.Time
	Protocol     string
	Err          string
}

// ProbePublic 从**本机**出发, 按客户端的方式连一次公网入口:
// TLS 握手 + WebSocket Upgrade 请求, 断言拿到 101。
//
// 这是端到端验证里最硬的一环 —— 前面所有解析都只是"读文件", 只有这里证明
// 数据面真的活着 (实测 tzq1 就是这么确证 Caddy 才是活路径的)。
//
// host 用域名时走 SNI 与 Host 头; 直连 IP 时 host 传空。
func (d *Driver) ProbePublic(addr string, port int, host, path string, timeout time.Duration) ProbeResult {
	pr := ProbeResult{}
	if addr == "" || port <= 0 {
		pr.Err = "地址或端口为空"
		return pr
	}
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	// 自签/未就绪证书也要能连上, 否则探测本身就没意义 (我们看的是"能不能握手")
	tlsCfg := &tls.Config{InsecureSkipVerify: true} // #nosec G402 - 探测用途, 只为读回证书信息
	if host != "" {
		tlsCfg.ServerName = host
	}

	dialer := &net.Dialer{Timeout: timeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", net.JoinHostPort(addr, fmt.Sprint(port)), tlsCfg)
	if err != nil {
		pr.Err = err.Error()
		return pr
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	st := conn.ConnectionState()
	pr.ServerName = st.ServerName
	if len(st.PeerCertificates) > 0 {
		c := st.PeerCertificates[0]
		pr.CertCN = c.Subject.CommonName
		pr.CertIssuer = c.Issuer.CommonName
		if pr.CertIssuer == "" {
			pr.CertIssuer = strings.Join(c.Issuer.Organization, ",")
		}
		pr.CertNotAfter = c.NotAfter
	}

	hostHdr := host
	if hostHdr == "" {
		hostHdr = addr
	}
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", path, hostHdr)
	if _, err := io.WriteString(conn, req); err != nil {
		pr.Err = "发送探测请求失败: " + err.Error()
		return pr
	}

	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if n > 0 {
		line := string(buf[:n])
		if i := strings.Index(line, "\r\n"); i > 0 {
			line = line[:i]
		}
		pr.Protocol = line
		// "HTTP/1.1 101 Switching Protocols"
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			fmt.Sscanf(fields[1], "%d", &pr.StatusCode)
		}
		pr.Reachable = pr.StatusCode > 0
	}
	if err != nil && pr.StatusCode == 0 {
		pr.Err = err.Error()
	}
	return pr
}

// ProbeNode 按合并结果自动拼参数探测一个节点。
func (d *Driver) ProbeNode(n *NodeView, timeout time.Duration) ProbeResult {
	if n == nil {
		return ProbeResult{Err: "节点为空"}
	}
	addr := n.Address
	if addr == "" {
		addr = n.CaddyHost
	}
	path := n.Path
	if path == "" {
		path = n.CaddyPath
	}
	return d.ProbePublic(addr, n.PublicPort, n.Host, path, timeout)
}

// HTTPGetStatus 用 http.Client 走一次普通 GET (用于面板自身的健康检查)。
func HTTPGetStatus(url string, timeout time.Duration) (int, error) {
	c := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 - 仅健康检查
		},
	}
	resp, err := c.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, nil
}

// ---------------------------------------------------------------- 小工具

// hasWarnContaining 判断告警列表里是否已有包含 sub 的条目 (避免同一问题被重复提示)。
func hasWarnContaining(warns []string, sub string) bool {
	for _, w := range warns {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func diffFiles(before, after []string) []string {
	seen := make(map[string]struct{}, len(before))
	for _, n := range before {
		seen[n] = struct{}{}
	}
	var out []string
	for _, n := range after {
		if _, ok := seen[n]; !ok {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
