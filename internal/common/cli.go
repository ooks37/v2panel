package common

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ===========================================================================
// 233boy/v2ray CLI 安全封装
//
// 为什么需要这一层: 233boy 脚本是为人手敲菜单设计的, 直接脚本化有三个坑,
// 全部在 82 上对 v4.27 实测确认:
//
//  1. 缺参 = 死循环。ask() 是 `while :; do echo -ne "$提示"; read REPLY; done`,
//     从 /dev/null 读会立刻 EOF, REPLY 为空且不触发任何退出分支 → 空转刷屏
//     (实测 6s 刷 661 行)。触发条件已实测收敛为两种:
//       * `*-tls` 系缺 host  → 自旋在 "请输入域名:"
//       * `door`   缺 addr   → 自旋在 "请输入目标地址:"
//     所以本层必须主动检测提示词并尽早 SIGKILL 整个进程组。
//     另: core.sh:1110 那批 `ask` 兜底全部被 `[[ $is_main_start ]]` 包住,
//     而 CLI 调用路径不会置位 is_main_start, 故不会触发 —— 自旋集合就上面两种。
//
//  2. 另一种更隐蔽的坑: 缺参不报错, 直接静默取随机值。
//     实测无参仍成功并随机化的有:
//       ss      缺 method  -> 随机加密方式; 缺 password -> 随机密码
//       socks   缺凭据     -> 自动生成随机 user/pass (脚本内置 233boy + 随机 UUID)
//       kcp/quic 缺第3参   -> 随机伪装类型 (实测 wechat-video/wireguard/srtp 轮换;
//                               core.sh:103 注释明写 "random dont use none")
//       tls 系  缺端口     -> 随机高位端口
//     对面板而言 "随机" = 拿到不可知的配置, 订阅链接就是错的。因此 Add() 强制
//     参数完整, 绝不把裸参数交给脚本。
//
//  3. 输出带 ANSI。`v2ray url` 把链接包成 \x1b[45m...\x1b[0m, 导致
//     `grep -c '^[a-z]*://'` 恒为 0。统一走 StripANSI/CleanOutput。
//
// 另: `gen` 是纯 dry-run (create() 的 is_gen 分支在 `cat > $is_json_file` 之前 return),
// 实测连续多次 gen 后 /etc/v2ray/conf 内容指纹完全不变 —— 因此 Gen() 可以安全地
// 当作预校验使用, 但前提同样是"参数完整", 否则一样会自旋。
// ===========================================================================

const (
	// DefaultBinPath 233boy 入口 (软链 -> /etc/v2ray/sh/v2ray.sh)
	DefaultBinPath = "/usr/local/bin/v2ray"
	// DefaultConfDir 节点配置目录, 每节点一个 <协议全名>-<host|port>.json
	DefaultConfDir = "/etc/v2ray/conf"
	// DefaultTimeout 单次调用上限。参数完整时 gen ≈0.2s、add ≈1~3s;
	// 缺参场景靠提示检测提前中断, 这个超时是最后的兜底。
	DefaultTimeout = 20 * time.Second
	// maxOutputBytes 输出上限, 防止自旋把内存打满。
	maxOutputBytes = 1 << 20
	// promptBurstHits 提示词连续命中阈值。EOF 下每次循环都重打提示,
	// 命中 2 次即可确定脚本在等人输入。
	promptBurstHits = 2
)

// promptRe 交互式等待的特征词。
var promptRe = regexp.MustCompile(`请输入|请选择|我已经确定解析|是否删除配置文件`)

// DefaultEnv 固定环境变量。
// HOME/XDG_CONFIG_HOME 必须显式给: 缺省时 V2Ray core 会先吐一行
// `unable to determine directory for user configuration; falling back to current
// directory` 的 warn (实测, 因为 MCP/CI 环境下这两个变量为空), 污染 JSON 前面的输出。
// TERM=dumb 抑制光标控制序列。
func DefaultEnv() []string {
	return []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"XDG_CONFIG_HOME=/root/.config",
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"TERM=dumb",
	}
}

// ---------------------------------------------------------------------------
// 错误类型
// ---------------------------------------------------------------------------

// ErrInteractive 供 errors.Is 判定的哨兵: CLI 进入交互式等待被中断。
var ErrInteractive = errors.New("233boy CLI 进入交互式等待")

// InteractiveError 脚本在等人输入 (参数不全)。
type InteractiveError struct {
	Args   []string
	Prompt string
}

func (e *InteractiveError) Error() string {
	return fmt.Sprintf("233boy CLI 进入交互式等待, 已强制中断 (参数=%v, 等待=%q); 请补全参数后重试",
		e.Args, e.Prompt)
}

func (e *InteractiveError) Is(target error) bool { return target == ErrInteractive }

// ErrTimeout 供 errors.Is 判定的哨兵: CLI 超时被中断。
var ErrTimeout = errors.New("233boy CLI 执行超时")

// TimeoutError 脚本超时。
type TimeoutError struct {
	Args    []string
	Timeout time.Duration
	Tail    string
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("233boy CLI 执行超时 (%s, 参数=%v); 末尾输出: %s",
		e.Timeout, e.Args, e.Tail)
}

func (e *TimeoutError) Is(target error) bool { return target == ErrTimeout }

// ExitError 脚本以非零码退出 (err() 走的是 exit 1)。
type ExitError struct {
	Args     []string
	ExitCode int
	Output   string
}

func (e *ExitError) Error() string {
	msg := lastMeaningfulLine(e.Output)
	if msg == "" {
		msg = "(无输出)"
	}
	return fmt.Sprintf("233boy CLI 退出码 %d (参数=%v): %s", e.ExitCode, e.Args, msg)
}

// ---------------------------------------------------------------------------
// 输出采集器: 限流 + 提示词检测 + 触发中断
// ---------------------------------------------------------------------------

type watchWriter struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	tail     []byte // 滚动窗口, 处理提示词跨 write 边界的情况
	limit    int
	trunc    bool
	hits     int
	fired    bool
	promptRe *regexp.Regexp
	onPrompt func()
}

const tailWindow = 256

func (w *watchWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if w.buf.Len() < w.limit {
		room := w.limit - w.buf.Len()
		if len(p) <= room {
			w.buf.Write(p)
		} else {
			w.buf.Write(p[:room])
			w.trunc = true
		}
	} else {
		w.trunc = true
	}

	fire := false
	if w.promptRe != nil {
		probe := make([]byte, 0, len(w.tail)+len(p))
		probe = append(probe, w.tail...)
		probe = append(probe, p...)
		if w.promptRe.Match(probe) {
			w.hits++
			if w.hits >= promptBurstHits && !w.fired {
				w.fired = true
				fire = true
			}
		}
		if len(probe) > tailWindow {
			probe = probe[len(probe)-tailWindow:]
		}
		w.tail = append(w.tail[:0], probe...)
	}

	cb := w.onPrompt
	w.mu.Unlock()

	if fire && cb != nil {
		cb()
	}
	return len(p), nil
}

func (w *watchWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func (w *watchWriter) state() (prompted bool, truncated bool, tail string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fired, w.trunc, string(w.tail)
}

// ---------------------------------------------------------------------------
// Runner
// ---------------------------------------------------------------------------

// Result 一次 CLI 调用的完整结果 (无论成败都返回, 便于调用方记日志)。
type Result struct {
	Args      []string
	ExitCode  int
	Stdout    string // 已去 ANSI、已归一化换行
	TimedOut  bool
	Prompted  bool
	Truncated bool
	Duration  time.Duration
	// LenientExit 为真表示: 退出码非零, 但 stdout 里拿到了合法 JSON,
	// 调用方 (execLenientJSON) 判定为成功。见 execLenientJSON 的注释 ——
	// 233boy v4.27 的 `xapi stats` 恒返回 1, 只有产物能作为成功判据。
	LenientExit bool
	// NewConfFiles 仅 Add() 填充: 本次调用后 conf 目录新出现的 .json。
	NewConfFiles []string
}

// Runner 233boy CLI 调用器。
//
// 所有调用串行化 (mu): 脚本会改动共享状态 (动态端口文件、服务重启、Caddy 配置),
// 并发执行会互相踩。面板侧调用频率低 (加节点 / 流量轮询), 串行没有性能问题。
type Runner struct {
	Bin     string
	ConfDir string
	Timeout time.Duration
	Env     []string

	mu sync.Mutex
}

// NewRunner 返回使用默认路径的调用器。
func NewRunner() *Runner {
	return &Runner{
		Bin:     DefaultBinPath,
		ConfDir: DefaultConfDir,
		Timeout: DefaultTimeout,
		Env:     DefaultEnv(),
	}
}

func (r *Runner) bin() string {
	if r.Bin != "" {
		return r.Bin
	}
	return DefaultBinPath
}

func (r *Runner) confDir() string {
	if r.ConfDir != "" {
		return r.ConfDir
	}
	return DefaultConfDir
}

func (r *Runner) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return DefaultTimeout
}

func (r *Runner) env() []string {
	if len(r.Env) > 0 {
		return r.Env
	}
	return DefaultEnv()
}

// exec 执行一次 CLI。
// detectPrompt 仅对可能进 ask() 的子命令开启 (gen/add/del); url/stats/status/help
// 是纯输出命令, 文案里可能天然含"请输入", 开了会误杀。
func (r *Runner) exec(args []string, detectPrompt bool) (*Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	start := time.Now()
	timeout := r.timeout()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.Command(r.bin(), args...)
	cmd.Env = r.env()
	cmd.Dir = "/root"
	cmd.Stdin = nil // nil => 子进程从 /dev/null 读, 这是自旋的根因也是快速暴露缺参的手段
	// 独立进程组: 脚本会 fork (curl/systemctl/manage restart), 超时必须整组杀,
	// 否则会留下后台残留把后续调用搅乱。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	w := &watchWriter{limit: maxOutputBytes}
	if detectPrompt {
		w.promptRe = promptRe
		w.onPrompt = func() {
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		}
	}
	cmd.Stdout = w
	cmd.Stderr = w

	if err := cmd.Start(); err != nil {
		return &Result{Args: args, ExitCode: -1, Duration: time.Since(start)}, fmt.Errorf("启动 233boy CLI 失败: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var waitErr error
	timedOut := false
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		timedOut = true
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		waitErr = <-done
	}

	prompted, truncated, tail := w.state()
	res := &Result{
		Args:      args,
		Stdout:    CleanOutput(w.String()),
		TimedOut:  timedOut,
		Prompted:  prompted,
		Truncated: truncated,
		Duration:  time.Since(start),
	}

	switch {
	case prompted:
		res.ExitCode = -1
		prompt := lastMeaningfulLine(res.Stdout)
		if prompt == "" {
			prompt = strings.TrimSpace(tail)
		}
		return res, &InteractiveError{Args: args, Prompt: prompt}
	case timedOut:
		res.ExitCode = -1
		return res, &TimeoutError{Args: args, Timeout: timeout, Tail: lastMeaningfulLine(res.Stdout)}
	}

	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			res.ExitCode = -1
			return res, fmt.Errorf("执行 233boy CLI 失败: %w", waitErr)
		}
		return res, &ExitError{Args: args, ExitCode: res.ExitCode, Output: res.Stdout}
	}
	res.ExitCode = 0
	return res, nil
}

// execLenientJSON 执行一条**只读的、输出 JSON** 的 233boy 命令, 并容忍非零退出码。
//
// ─────────────────────────────────────────────────────────────────────────
// 为什么必须容忍 (这是 233boy v4.27 自身的一个 bug, 不是我们的)
// ─────────────────────────────────────────────────────────────────────────
// `/etc/v2ray/sh/src/core.sh:846` 的 api() 函数结尾是:
//
//	$is_core_bin api $is_api_do --server=127.0.0.1:$is_api_port ${@:2}
//	[[ $? != 0 ]] && {
//	    is_api_fail=1
//	}
//
// 核心命令 exit 0、stdout 输出完全正确的 JSON —— 但那个 `[[ ]]` 测试为假,
// 于是整个函数返回 1, 连 `xapi stats -json` 也**恒返回退出码 1**。
// 实测:
//
//	$ /usr/local/bin/v2ray xapi stats -json ; echo $?
//	{"stat":[...正确的 JSON...]}
//	1
//
// 而 `xapi add/del` 同样返回 1, 但那时 core.sh 会在调用点 379/747/1823
// 顺手 `manage restart` —— 所以"改配置"是生效的, 只有**读取**路径被这个
// 非零退出码坑死。
//
// 后果 (修复前): Stats()/UserStats() 一律返回 error → agent 什么都不上报
// → 面板流量永远是 0, 曲线永远空白, 限流永远不触发。
// **不是"偶尔少记", 是整条计量链路从未工作过。**
//
// 红线: 绝不 patch /etc/v2ray/sh/ (233boy 升级会覆盖, 且属于注入)。
// 所以在调用侧放宽: **只要 stdout 里能解析出 JSON, 就认为成功**。
// 判断依据是产物而不是退出码 —— 这正是"读"与"写"该有的区别。
//
// ⚠️ 只给只读命令用。写操作必须继续严格检查退出码, 否则会把
// "改配置失败"当成成功 (那才是真正危险的静默失败)。
func (r *Runner) execLenientJSON(args []string) (*Result, error) {
	res, err := r.exec(args, false)
	if err == nil {
		return res, nil
	}

	// 只有「拿到了合法 JSON 输出」才豁免退出码。超时 / 交互自旋 / 启动失败
	// 一律照旧报错 —— 那些情况下 stdout 里不会有完整的 JSON。
	if _, ok := err.(*ExitError); !ok {
		return res, err
	}
	if res == nil {
		return res, err
	}
	if _, jerr := ExtractJSONObject(res.Stdout); jerr != nil {
		// 没有可用 JSON, 这个非零退出码是真实错误, 不能吞。
		return res, err
	}
	res.LenientExit = true
	return res, nil
}

// ---------------------------------------------------------------------------
// 协议表 (全名、参数位、监听地址均来自 82 上 v4.27 的实测输出, 非手推)
// ---------------------------------------------------------------------------

// ListenLoopback / ListenAny 是脚本生成的 inbound listen 值。
// 它决定"这个节点能否被公网直连": 只有 127.0.0.1 的必须经前置反代 (Caddy/nginx)。
const (
	ListenLoopback = "127.0.0.1"
	ListenAny      = "0.0.0.0"
)

// ProtocolDef 单个协议简写的定义。
// ProtocolDef 一个 233boy 协议的定义。
//
// 带 json tag 是因为它会被 CapabilityReportPayload 原样下发到面板 ——
// 面板前端据此渲染协议下拉框与参数表单, 不必在前端硬编码一份协议表
// (两份表迟早会不一致: 比如 reality 在 v4.27 是死的, 只有后端知道)。
type ProtocolDef struct {
	Token string `json:"token"`  // 传给脚本的简写
	Name  string `json:"name"`   // 脚本内部使用的协议全名 (决定 conf 文件名前缀)
	IsTLS bool   `json:"is_tls"` // 走 *-tls 分支: 必须给 host, 否则自旋
	// ArgNames 位置参数名 (按脚本顺序), 长度即该协议接受的最大参数个数。
	ArgNames []string `json:"arg_names"`
	// Required 本层强制要求非空的参数个数。取值依据:
	//   * 少于该数量会自旋 (host / door addr) -> 必须给
	//   * 少于该数量会静默取随机值 (端口/密码/方法/伪装类型) -> 面板必须知道确切值, 必须给
	Required int `json:"required"`
	// ListenAddr 脚本为该协议生成的 inbound listen (实测值)。
	ListenAddr string `json:"listen_addr"`
	Note       string `json:"note,omitempty"`
}

// protocolDefs 实测表。注意 vws/h2 等的归属与直觉相反:
// `ws` 是 VMess-WS-TLS, `vws` 才是 VLESS-WS-TLS (脚本用 sed 改前缀)。
var protocolDefs = []ProtocolDef{
	// VMess 系: 第 3 参是伪装类型。tcp 默认 none 但 kcp/quic 是随机,
	// 为了配置可复现, 三种一律要求显式给出。
	{Token: "tcp", Name: "VMess-TCP", ArgNames: []string{"port", "uuid", "type"}, Required: 3, ListenAddr: ListenAny},
	{Token: "kcp", Name: "VMess-mKCP", ArgNames: []string{"port", "uuid", "type"}, Required: 3, ListenAddr: ListenAny,
		Note: "type 省略时脚本随机取 srtp/utp/wechat-video/dtls/wireguard"},
	{Token: "quic", Name: "VMess-QUIC", ArgNames: []string{"port", "uuid", "type"}, Required: 3, ListenAddr: ListenAny,
		Note: "type 省略时脚本随机取值"},
	{Token: "tcpd", Name: "VMess-TCP-dynamic-port", ArgNames: []string{"port", "uuid", "type", "start_port", "end_port"}, Required: 3, ListenAddr: ListenAny},
	{Token: "kcpd", Name: "VMess-mKCP-dynamic-port", ArgNames: []string{"port", "uuid", "type", "start_port", "end_port"}, Required: 3, ListenAddr: ListenAny},
	{Token: "quicd", Name: "VMess-QUIC-dynamic-port", ArgNames: []string{"port", "uuid", "type", "start_port", "end_port"}, Required: 3, ListenAddr: ListenAny},

	{Token: "ws", Name: "VMess-WS-TLS", IsTLS: true, ArgNames: []string{"host", "uuid", "path"}, Required: 3, ListenAddr: ListenLoopback},
	{Token: "h2", Name: "VMess-H2-TLS", IsTLS: true, ArgNames: []string{"host", "uuid", "path"}, Required: 3, ListenAddr: ListenLoopback},
	{Token: "grpc", Name: "VMess-gRPC-TLS", IsTLS: true, ArgNames: []string{"host", "uuid", "path"}, Required: 3, ListenAddr: ListenLoopback},
	{Token: "vws", Name: "VLESS-WS-TLS", IsTLS: true, ArgNames: []string{"host", "uuid", "path"}, Required: 3, ListenAddr: ListenLoopback,
		Note: "端口由脚本随机分配, 无法用参数指定"},
	{Token: "vh2", Name: "VLESS-H2-TLS", IsTLS: true, ArgNames: []string{"host", "uuid", "path"}, Required: 3, ListenAddr: ListenLoopback},
	{Token: "vgrpc", Name: "VLESS-gRPC-TLS", IsTLS: true, ArgNames: []string{"host", "uuid", "path"}, Required: 3, ListenAddr: ListenLoopback},
	{Token: "tws", Name: "Trojan-WS-TLS", IsTLS: true, ArgNames: []string{"host", "uuid", "path"}, Required: 3, ListenAddr: ListenLoopback},
	{Token: "th2", Name: "Trojan-H2-TLS", IsTLS: true, ArgNames: []string{"host", "uuid", "path"}, Required: 3, ListenAddr: ListenLoopback},
	{Token: "tgrpc", Name: "Trojan-gRPC-TLS", IsTLS: true, ArgNames: []string{"host", "uuid", "path"}, Required: 3, ListenAddr: ListenLoopback},

	{Token: "ss", Name: "Shadowsocks", ArgNames: []string{"port", "password", "method"}, Required: 3, ListenAddr: ListenAny,
		Note: "method 省略时脚本随机取一种加密方式"},
	{Token: "socks", Name: "Socks", ArgNames: []string{"port", "user", "pass"}, Required: 3, ListenAddr: ListenAny,
		Note: "省略凭据时脚本自动生成随机 user/pass, 面板将无法得知"},
	{Token: "http", Name: "local-http", ArgNames: []string{"port"}, Required: 1, ListenAddr: ListenLoopback,
		Note: "仅监听 127.0.0.1, 无法公网直连"},
	{Token: "door", Name: "Dokodemo-Door", ArgNames: []string{"port", "addr", "dest_port"}, Required: 3, ListenAddr: ListenAny,
		Note: "转发用途; 缺 addr 或 dest_port 都会自旋"},
}

// SSMethods 脚本 ss_method_list 中未注释的加密方式。
var SSMethods = []string{"aes-128-gcm", "aes-256-gcm", "chacha20-ietf-poly1305"}

// HeaderTypes 脚本 header_type_list 全集 (core.sh:33)。
// 注意 tcp 只接受 none|http (core.sh:223 覆盖), kcp/quic 用全表且随机时不含 none。
var HeaderTypes = []string{"none", "http", "srtp", "utp", "wechat-video", "dtls", "wireguard"}

// HeaderTypesFor 返回某协议可用的伪装类型集合。
func HeaderTypesFor(token string) []string {
	switch token {
	case "tcp", "tcpd":
		return []string{"none", "http"}
	default:
		return HeaderTypes
	}
}

// ProtocolDefs 返回协议表副本 (按 Token 排序)。
func ProtocolDefs() []ProtocolDef {
	out := make([]ProtocolDef, len(protocolDefs))
	copy(out, protocolDefs)
	sort.Slice(out, func(i, j int) bool { return out[i].Token < out[j].Token })
	return out
}

// LookupProtocol 按简写查协议定义。
func LookupProtocol(token string) (ProtocolDef, bool) {
	token = strings.ToLower(strings.TrimSpace(token))
	for _, d := range protocolDefs {
		if d.Token == token {
			return d, true
		}
	}
	return ProtocolDef{}, false
}

// RealitySupported 恒为 false: v4.27 的 add() 里 r|reality) 与 *reality*) 两个分支
// 都被注释掉了 (core.sh:946-951), `gen reality` 直接报 "无法识别 (r), 请使用: v2r"。
// UI 据此把 reality 置灰。绝不去 patch /etc/v2ray/sh/ —— 升级会被覆盖, 且属于注入。
func RealitySupported() bool { return false }

// ---------------------------------------------------------------------------
// 参数校验 (逐条对齐 core.sh 的 is_test(), 不自己发明规则)
// ---------------------------------------------------------------------------

var (
	// 对齐 is_test domain: ^\w(\w|\-|\.)?+\.\w+$ —— 允许 a.b.c.com 这类多级子域。
	reDomain = regexp.MustCompile(`^\w([\w\-]*\w)?(\.[\w\-]+)+$`)
	// 对齐 is_test path: ^\/\w(\w|\-|\/)?+\w$ —— 首尾都必须是非符号字符。
	rePath = regexp.MustCompile(`^/\w[\w\-/]*\w$`)
	// 对齐 is_test uuid: 只要求"能匹配到", 但脚本上游还有 uuidgen 兜底, 这里从严。
	reUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	// 对齐 is_test number: ^[1-9][0-9]?+$ —— 拒绝前导 0 与 0 本身 (实测 09999/0 被脚本拒绝)。
	rePortStr = regexp.MustCompile(`^[1-9][0-9]{0,4}$`)
)

// ValidatePort 校验端口字符串。
func ValidatePort(s string) error {
	if !rePortStr.MatchString(s) {
		return fmt.Errorf("端口必须是 1-5 位数字且不以 0 开头, 收到 %q", s)
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("端口超出范围 1-65535, 收到 %q", s)
	}
	return nil
}

// ValidateDomain 校验域名 (与脚本 is_test domain 对齐)。
func ValidateDomain(s string) error {
	if !reDomain.MatchString(s) {
		return fmt.Errorf("域名格式不合法, 收到 %q", s)
	}
	return nil
}

// ValidateUUID 校验 UUID。
func ValidateUUID(s string) error {
	if !reUUID.MatchString(s) {
		return fmt.Errorf("UUID 格式不合法, 收到 %q", s)
	}
	return nil
}

// ValidatePath 校验 ws/h2/grpc 路径 (脚本要求首尾都是 \w, 即至少 2 个字符)。
func ValidatePath(s string) error {
	if !rePath.MatchString(s) {
		return fmt.Errorf("路径格式不合法 (须形如 /abc, 至少 2 位且首尾为字母数字), 收到 %q", s)
	}
	return nil
}

// ValidateSSMethod 校验 Shadowsocks 加密方式。
func ValidateSSMethod(s string) error {
	for _, m := range SSMethods {
		if m == s {
			return nil
		}
	}
	return fmt.Errorf("不支持的加密方式 %q, 可用: %s", s, strings.Join(SSMethods, ", "))
}

// ValidateHeaderType 校验伪装类型 (VMess tcp/kcp/quic 系)。
func ValidateHeaderType(token, s string) error {
	allowed := HeaderTypesFor(token)
	for _, t := range allowed {
		if t == s {
			return nil
		}
	}
	return fmt.Errorf("协议 %s 不支持的伪装类型 %q, 可用: %s", token, s, strings.Join(allowed, ", "))
}

// ---------------------------------------------------------------------------
// AddSpec: 一次建节点的完整意图
// ---------------------------------------------------------------------------

// AddSpec 建节点参数。
//
// 字段带 json tag 是**有意的**: 本结构体会作为 node_op 消息的载荷经 WebSocket
// 在面板与 agent 之间传输 (见 protocol.go 的 NodeOpPayload.Spec)。
// 面板侧填表 → agent 侧 BuildArgs() 二次校验, 两道关都不信任"参数恰好齐全"。
type AddSpec struct {
	Token    string `json:"token"`               // 协议简写, 见 ProtocolDefs()
	Host     string `json:"host,omitempty"`      // *-tls: 对外域名
	UUID     string `json:"uuid,omitempty"`      // *-tls / vmess: 用户 UUID
	Path     string `json:"path,omitempty"`      // *-tls: ws/h2/grpc 路径
	Port     string `json:"port,omitempty"`      // 入站端口 (tls 系由脚本随机分配, 此字段忽略)
	Password string `json:"password,omitempty"`  // ss: 密码
	Method   string `json:"method,omitempty"`    // ss: 加密方式
	Header   string `json:"header,omitempty"`    // vmess 系: 伪装类型 (tcp: none|http; kcp/quic 等: 全表)
	Addr     string `json:"addr,omitempty"`      // door: 目标地址
	DestPort string `json:"dest_port,omitempty"` // door: 目标端口
	User     string `json:"user,omitempty"`      // socks: 用户名
	Pass     string `json:"pass,omitempty"`      // socks: 密码
	// NoAutoTLS 走 `no-auto-tls` 子命令: 让 host-test 直接 return 且不建 Caddy 站点。
	// 仅对 *-tls 协议有意义 —— 脚本里 [[ $is_no_auto_tls && ! $is_use_tls ]] && err,
	// 非 TLS 协议带这个开关会直接 exit 1。
	NoAutoTLS bool `json:"no_auto_tls,omitempty"`
}

// BuildArgs 把 AddSpec 翻译成脚本位置参数, 并做完整性校验。
// 这是防"静默随机配置"的唯一闸门, 必须严格。
func (s AddSpec) BuildArgs() ([]string, ProtocolDef, error) {
	def, ok := LookupProtocol(s.Token)
	if !ok {
		return nil, def, fmt.Errorf("不支持的协议简写 %q", s.Token)
	}
	if s.NoAutoTLS && !def.IsTLS {
		return nil, def, fmt.Errorf("no-auto-tls 只适用于 TLS 协议 (%s 不支持)", def.Name)
	}

	var vals []string
	switch {
	case def.IsTLS:
		vals = []string{s.Host, s.UUID, s.Path}
	case strings.HasPrefix(s.Token, "vmess") || s.Token == "tcp" || s.Token == "kcp" || s.Token == "quic" ||
		s.Token == "tcpd" || s.Token == "kcpd" || s.Token == "quicd":
		vals = []string{s.Port, s.UUID, s.Header}
	case s.Token == "ss":
		vals = []string{s.Port, s.Password, s.Method}
	case s.Token == "socks":
		vals = []string{s.Port, s.User, s.Pass}
	case s.Token == "door":
		vals = []string{s.Port, s.Addr, s.DestPort}
	case s.Token == "http":
		vals = []string{s.Port}
	default:
		return nil, def, fmt.Errorf("协议 %s (%s) 的参数映射未实现", def.Token, def.Name)
	}

	if len(vals) < def.Required {
		return nil, def, fmt.Errorf("协议 %s (%s) 至少需要 %d 个参数 (%s), 当前只有 %d 个; "+
			"参数不全会让 233boy 静默取随机值或陷入交互式等待",
			def.Token, def.Name, def.Required, strings.Join(def.ArgNames, " "), len(vals))
	}
	for i := 0; i < def.Required; i++ {
		if strings.TrimSpace(vals[i]) == "" {
			return nil, def, fmt.Errorf("协议 %s (%s) 缺少必填参数 %s", def.Token, def.Name, def.ArgNames[i])
		}
		// auto 会被脚本 unset 掉走随机值 (core.sh:1026), 面板拿不到确切配置, 因此显式禁止。
		if vals[i] == "auto" {
			return nil, def, fmt.Errorf("协议 %s (%s) 的 %s 不允许用 auto: "+
				"脚本会 unset 后随机取值, 面板将无法得知实际配置", def.Token, def.Name, def.ArgNames[i])
		}
	}

	// 逐项按脚本 is_test() 的规则预校验: 提前失败好过让脚本 err() 退出。
	switch {
	case def.IsTLS:
		if err := ValidateDomain(s.Host); err != nil {
			return nil, def, err
		}
		if err := ValidateUUID(s.UUID); err != nil {
			return nil, def, err
		}
		if err := ValidatePath(s.Path); err != nil {
			return nil, def, err
		}
	case s.Token == "ss":
		if err := ValidatePort(s.Port); err != nil {
			return nil, def, err
		}
		if err := ValidateSSMethod(s.Method); err != nil {
			return nil, def, err
		}
	case s.Token == "socks":
		if err := ValidatePort(s.Port); err != nil {
			return nil, def, err
		}
	case s.Token == "door":
		if err := ValidatePort(s.Port); err != nil {
			return nil, def, err
		}
		if err := ValidatePort(s.DestPort); err != nil {
			return nil, def, err
		}
	case s.Token == "http":
		if err := ValidatePort(s.Port); err != nil {
			return nil, def, err
		}
	default: // vmess 系
		if err := ValidatePort(s.Port); err != nil {
			return nil, def, err
		}
		if err := ValidateUUID(s.UUID); err != nil {
			return nil, def, err
		}
		if err := ValidateHeaderType(s.Token, s.Header); err != nil {
			return nil, def, err
		}
	}

	args := []string{def.Token}
	args = append(args, vals...)
	return args, def, nil
}

// ConfName 推导节点配置文件名。
// 依据 create(): 有 host 用 `<协议全名>-<host>.json`, 否则 `<协议全名>-<端口>.json`。
func (s AddSpec) ConfName() (string, error) {
	def, ok := LookupProtocol(s.Token)
	if !ok {
		return "", fmt.Errorf("不支持的协议简写 %q", s.Token)
	}
	if def.IsTLS {
		if s.Host == "" {
			return "", errors.New("TLS 协议缺少 host, 无法推导配置文件名")
		}
		return def.Name + "-" + s.Host + ".json", nil
	}
	if s.Port == "" {
		return "", errors.New("缺少端口, 无法推导配置文件名")
	}
	return def.Name + "-" + s.Port + ".json", nil
}

// ---------------------------------------------------------------------------
// 对外方法
// ---------------------------------------------------------------------------

// GenRaw 直接以给定参数做 dry-run (不做完整性校验), 用于 UI 探测/预览。
// 仍然开启提示检测: 参数不全时脚本会自旋, 本层会尽快中断并返回 InteractiveError。
func (r *Runner) GenRaw(args ...string) (*Result, error) {
	return r.exec(append([]string{"gen"}, args...), true)
}

// Gen 按 AddSpec 做 dry-run 并返回脚本将写入的配置 JSON。
// gen 不落盘 (create() 的 is_gen 分支在写文件之前 return, 已实测 conf 指纹不变),
// 因此可以安全地当作"建节点前的预校验"。
func (r *Runner) Gen(spec AddSpec) (json.RawMessage, *Result, error) {
	args, _, err := spec.BuildArgs()
	if err != nil {
		return nil, nil, err
	}
	res, err := r.GenRaw(args...)
	if err != nil {
		return nil, res, err
	}
	raw, jerr := ExtractJSONObject(res.Stdout)
	if jerr != nil {
		return nil, res, fmt.Errorf("gen 输出中未找到合法 JSON: %w", jerr)
	}
	return raw, res, nil
}

// Add 真正建节点。执行前强制完整性校验, 执行后对比 conf 目录得出新增文件。
// NoAutoTLS 为真时改用 no-auto-tls 子命令 (让 host-test 短路, 不建 Caddy 站点)。
func (r *Runner) Add(spec AddSpec) (*Result, error) {
	args, _, err := spec.BuildArgs()
	if err != nil {
		return nil, err
	}
	sub := "add"
	if spec.NoAutoTLS {
		sub = "no-auto-tls"
	}
	full := append([]string{sub}, args...)

	before, _ := r.ConfFiles()
	res, err := r.exec(full, true)
	res.NewConfFiles = diffConfFiles(before, r.mustConfFiles())
	return res, err
}

// Del 删除节点配置。传协议全名或完整文件名均可, 内部会锚定成正则,
// 避免脚本因匹配到多个文件而进入 ask get_config_file 自旋。
func (r *Runner) Del(confName string) (*Result, error) {
	name := strings.TrimSpace(confName)
	if name == "" {
		return nil, errors.New("删除节点需要给出配置文件名")
	}
	// get file 用 grep -E -i 匹配, 锚定后保证唯一命中。
	pattern := "^" + regexp.QuoteMeta(name) + "$"
	return r.exec([]string{"del", pattern}, true)
}

// URL 取单个节点的客户端链接。
//
// 必须带锚定文件名, 绝不能不带参数: `v2ray url` 无参时**不是批量导出**, 而是进
// 配置选择菜单。调用链 url_qr $@ -> info $2 -> get info $1 -> `get file $2`,
// 其中 $2 是 `ls $is_conf_dir | grep -E -i "$is_file_str"` 的**模式**:
// 命中恰好 1 个才自动选中 (core.sh:1205 `[[ ${#is_all_json[@]} -eq 1 ]]`),
// 命中 >=2 个就走 ask get_config_file -> `while :; do echo -ne $提示; read REPLY; done`
// 自旋; Stdin 是 /dev/null, EOF 下空转刷屏, 直到超时才被整组杀掉。
// 实测 (82, 双节点): 锚定调用 exit=0 / 314~340ms / 10 行 503B;
// 无参调用 exit=124 / 6004ms / 1742 行 83463B 全是 "请选择配置:"。
// 锚定后保证唯一命中 —— 与 Del 同一套路。
func (r *Runner) URL(confName string) (NodeURI, *Result, error) {
	name := strings.TrimSpace(confName)
	if name == "" {
		return NodeURI{}, nil, errors.New("生成链接需要给出配置文件名")
	}
	pattern := "^" + regexp.QuoteMeta(name) + "$"
	// detectPrompt=true: 锚定后命中唯一, 脚本不会走 ask 分支, 所以 "请输入/请选择"
	// 只可能来自自旋本身。命中即杀进程组, 把"挂满 Timeout"变成"毫秒级报错"。
	res, err := r.exec([]string{"url", pattern}, true)
	if err != nil {
		return NodeURI{}, res, err
	}
	uris := ParseURLOutput(res.Stdout)
	if len(uris) == 0 {
		return NodeURI{}, res, fmt.Errorf("`v2ray url %s` 未输出可解析的客户端链接", pattern)
	}
	u := uris[0]
	if u.ConfName == "" {
		u.ConfName = name
	}
	return u, res, nil
}

// URLs 逐个节点取客户端链接, 返回链接列表与逐节点的失败原因 (单个失败不影响其它)。
// 逐节点调用是硬要求, 理由见 URL: 无参 url 在多节点机器上必然自旋,
// 每次都耗满 Timeout 且一条数据也拿不到 —— 于是 Address/PublicPort/Host/Path
// 这些只由 url 层提供的权威值会永久缺失。
func (r *Runner) URLs() ([]NodeURI, []string, error) {
	names, err := r.ConfFiles()
	if err != nil {
		return nil, nil, err
	}
	if len(names) == 0 {
		return nil, nil, nil
	}
	var (
		out   []NodeURI
		warns []string
	)
	for _, name := range names {
		u, _, err := r.URL(name)
		if err != nil {
			warns = append(warns, fmt.Sprintf("%s 取客户端链接失败: %v", name, err))
			continue
		}
		out = append(out, u)
	}
	return out, warns, nil
}

// Stats 读取 V2Ray 流量计数器。
// 只取 inbound>>>TAG>>>traffic>>>uplink|downlink, 显式排除 inbound>>>api>>>;
// 绝不用 outbound/Total —— 那些把出站也算进去, 约为真实用量的两倍。
func (r *Runner) Stats() ([]StatsCounter, *Result, error) {
	// 用 execLenientJSON 而不是 exec: `xapi stats` 因 233boy 脚本自身的 bug
	// 恒返回退出码 1, 严格检查会让整条流量采集链路永久失效。详见该函数注释。
	res, err := r.execLenientJSON([]string{"xapi", "stats", "-json"})
	if err != nil {
		return nil, res, err
	}
	raw, jerr := ExtractJSONObject(res.Stdout)
	if jerr != nil {
		return nil, res, fmt.Errorf("stats 输出中未找到合法 JSON: %w", jerr)
	}

	// 实测形态: {"stat":[{"name":"...","value":"524016057"}]}
	// 顶层是对象不是数组; value 是字符串; 零流量条目整条没有 value 字段。
	var payload struct {
		Stat []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"stat"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, res, fmt.Errorf("解析 stats JSON 失败: %w", err)
	}

	agg := map[string]*StatsCounter{}
	for _, it := range payload.Stat {
		parts := strings.Split(it.Name, ">>>")
		if len(parts) != 4 || parts[0] != "inbound" || parts[2] != "traffic" {
			continue
		}
		tag := parts[1]
		if tag == "api" { // 面板自身的 API 流量, 不计入用户用量
			continue
		}
		var n int64
		if it.Value != "" {
			v, perr := strconv.ParseInt(it.Value, 10, 64)
			if perr != nil {
				continue
			}
			n = v
		}
		c, ok := agg[tag]
		if !ok {
			c = &StatsCounter{ConfName: tag}
			agg[tag] = c
		}
		switch parts[3] {
		case "uplink":
			c.Up = n
		case "downlink":
			c.Down = n
		}
	}

	out := make([]StatsCounter, 0, len(agg))
	for _, c := range agg {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConfName < out[j].ConfName })
	return out, res, nil
}

// UserStats 读**按用户**的累计流量计数器。
//
// 计数器形态: `user>>>EMAIL>>>traffic>>>uplink|downlink`。
//
// ⚠️ 这些计数器存在的两个前提 (缺一则返回空, 且**不会报错**):
//  1. /etc/v2ray/config.json 的 policy.levels.0.statsUserUplink/Downlink = true
//     (233boy 的默认模板里本来就是 true, 实测 82 上确认过);
//  2. 对应 inbound 的 clients[] 里那一项带 `email` 字段 —— 这正是
//     usersync.go 在写 conf 时补上的标记。
//
// 所以「读不到用户计数」通常不是解析错, 而是还没同步过用户名单。
// 排障时先确认 conf 里有没有 email。
func (r *Runner) UserStats() ([]UserStatsCounter, error) {
	// 同上: 必须走宽容路径, 否则 per-user 计量恒为空。
	res, err := r.execLenientJSON([]string{"xapi", "stats", "-json"})
	if err != nil {
		return nil, err
	}
	raw, jerr := ExtractJSONObject(res.Stdout)
	if jerr != nil {
		return nil, fmt.Errorf("stats 输出中未找到合法 JSON: %w", jerr)
	}

	var payload struct {
		Stat []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"stat"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("解析 stats JSON 失败: %w", err)
	}

	agg := map[string]*UserStatsCounter{}
	for _, it := range payload.Stat {
		parts := strings.Split(it.Name, ">>>")
		// user>>>EMAIL>>>traffic>>>uplink 恰好 4 段
		if len(parts) != 4 || parts[0] != "user" || parts[2] != "traffic" {
			continue
		}
		email := strings.TrimSpace(parts[1])
		if email == "" {
			continue
		}
		// 零流量条目整条没有 value 字段 —— 容忍缺失 (与 Stats 同一处理)。
		var n int64
		if it.Value != "" {
			v, perr := strconv.ParseInt(it.Value, 10, 64)
			if perr != nil {
				continue
			}
			n = v
		}
		c, ok := agg[email]
		if !ok {
			c = &UserStatsCounter{Email: email}
			agg[email] = c
		}
		switch parts[3] {
		case "uplink":
			c.Up = n
		case "downlink":
			c.Down = n
		}
	}

	out := make([]UserStatsCounter, 0, len(agg))
	for _, c := range agg {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out, nil
}

// ServiceStatus 内核与 Caddy 的运行状态。
type ServiceStatus struct {
	CoreVersion  string
	CoreRunning  bool
	CaddyVersion string
	CaddyRunning bool
	Raw          string
}

var reStatusLine = regexp.MustCompile(`^(.*?):\s*(running|stopped|error|not running)\s*$`)

// Status 解析 `v2ray status` 输出, 形如:
//
//	V2Ray 5.53.0: running
//	Caddy v2.11.4: running
func (r *Runner) Status() (*ServiceStatus, *Result, error) {
	res, err := r.exec([]string{"status"}, false)
	if err != nil {
		return nil, res, err
	}
	st := &ServiceStatus{Raw: res.Stdout}
	for _, line := range strings.Split(res.Stdout, "\n") {
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
	return st, res, nil
}

// ConfFiles 列出节点配置文件 (按名排序)。
// 跳过 dynamic-port-*-link 这类由脚本生成的辅助文件。
func (r *Runner) ConfFiles() ([]string, error) {
	dir := r.confDir()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("读取配置目录 %s 失败: %w", dir, err)
	}
	var names []string
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".json") {
			continue
		}
		if strings.HasPrefix(n, "dynamic-port-") {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

func (r *Runner) mustConfFiles() []string {
	names, _ := r.ConfFiles()
	return names
}

// ReadConf 读取单个节点配置原始内容。
func (r *Runner) ReadConf(name string) ([]byte, error) {
	if strings.ContainsAny(name, "/\\") || name == "" {
		return nil, fmt.Errorf("非法的配置文件名 %q", name)
	}
	return os.ReadFile(filepath.Join(r.confDir(), name))
}

// ConfDirDigest 返回配置目录内容的指纹 (文件名 + 大小 + mtime)。
// 用于"操作前后必须一致"的断言, 例如校验 gen 没落盘、失败的 add 没留下垃圾。
func (r *Runner) ConfDirDigest() (string, error) {
	dir := r.confDir()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, e := range ents {
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		fmt.Fprintf(&sb, "%s|%d|%d\n", e.Name(), info.Size(), info.ModTime().UnixNano())
	}
	return sb.String(), nil
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func diffConfFiles(before, after []string) []string {
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
	return out
}

// ExtractJSONObject 从混杂输出中截取配置 JSON。
//
// 不能简单取"第一个 { 到最后一个 }": 实测 gen 的输出开头就有一行 core 的 warn
//
//	{"level":"warn","ts":...,"msg":"unable to determine directory ..."}
//
// 它本身就是合法 JSON, 简单截取会把 warn 行和真正的配置对象连成一段非法内容。
// 这里改为按花括号深度扫描 (跳过字符串字面量与转义), 收集所有完整的 {...} 候选,
// 取最后一个合法对象 —— 真实配置总是排在提示行之后。
func ExtractJSONObject(s string) (json.RawMessage, error) {
	cands := scanJSONObjects(s)
	for i := len(cands) - 1; i >= 0; i-- {
		if json.Valid(cands[i]) {
			return json.RawMessage(cands[i]), nil
		}
	}
	return nil, errors.New("输出中不存在合法的 JSON 对象")
}

// scanJSONObjects 用花括号深度扫描, 返回所有顶层 {...} 片段。
// 字符串内的花括号与转义引号不参与计数 (warn 行的 msg 里可能带花括号)。
func scanJSONObjects(s string) [][]byte {
	var out [][]byte
	depth, start := 0, -1
	inStr := false
	esc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					out = append(out, []byte(s[start:i+1]))
					start = -1
				}
			}
		}
	}
	return out
}

// lastMeaningfulLine 取最后一行非空内容, 用于错误信息摘要。
func lastMeaningfulLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// regexpQuoteMeta 供测试与 Del 复用 (避免调用方各自 import regexp)。
func regexpQuoteMeta(s string) string { return regexp.QuoteMeta(s) }
