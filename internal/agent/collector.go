package agent

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"v2panel/internal/common"
)

// ===========================================================================
// 硬件与 233boy 运行时状态采集 —— 心跳载荷 (NodeHardwareStats) 的唯一来源。
//
// 为什么不引 github.com/shirou/gopsutil/v3:
//   82 可用内存只有 ~800MB, 加依赖要重跑 go mod tidy 解析整个模块图, 有 OOM 风险;
//   而且本项目要的指标比 gopsutil 给的更贴题 (Caddy 状态 / conf 数量),
//   手写 /proc 解析反而更短、更可控。
//
// 为什么 Collector 有状态:
//   CPU 使用率与网卡速率都是**增量**指标, 必须保留上一次采样做差分,
//   单次 /proc 快照里读不出来。
// ===========================================================================

// 默认探测路径 (82 上实测确认)。可被 config 覆盖。
const (
	DefaultV2rayUnit = "v2ray.service"
	DefaultCaddyUnit = "caddy.service"
	DefaultV2rayBin  = "/etc/v2ray/bin/v2ray"
	DefaultCaddyBin  = "/usr/local/bin/caddy"
)

// cpuSample 一次 /proc/stat 首行的汇总快照 (jiffies)。
type cpuSample struct {
	idle  uint64
	total uint64
}

// netSample 一次 /proc/net/dev 的汇总快照 (bytes, 已排除 lo)。
type netSample struct {
	rx uint64
	tx uint64
}

// Collector 采集本机硬件与 233boy 运行时状态。
type Collector struct {
	V2rayUnit string
	CaddyUnit string
	V2rayBin  string
	CaddyBin  string

	mu       sync.Mutex
	cpuPrev  cpuSample
	netPrev  netSample
	prevTime time.Time

	// 内核版本运行期不变, 首次读成功后缓存, 避免每 5s 白拉一个进程只为读版本号。
	v2rayVer string
	caddyVer string
}

// NewCollector 构造采集器。首次 Collect 时增量指标为 0 (还没有历史样本)。
func NewCollector() *Collector {
	return &Collector{
		V2rayUnit: DefaultV2rayUnit,
		CaddyUnit: DefaultCaddyUnit,
		V2rayBin:  DefaultV2rayBin,
		CaddyBin:  DefaultCaddyBin,
	}
}

// Collect 采一帧心跳载荷。任何单项失败都只让该字段保持零值, 不影响整帧上报
// (面板把 0/"" 当作有效事实 —— 例如 v2ray_pid=0 就是"进程挂了")。
func (c *Collector) Collect() common.NodeHardwareStats {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	st := common.NodeHardwareStats{Version: common.AgentVersion}

	// —— CPU: /proc/stat 差分 ——
	if cur, err := readCPUStat(); err == nil {
		if c.cpuPrev.total > 0 && cur.total > c.cpuPrev.total {
			dTotal := float64(cur.total - c.cpuPrev.total)
			dIdle := float64(cur.idle) - float64(c.cpuPrev.idle)
			pct := (1 - dIdle/dTotal) * 100
			if pct < 0 {
				pct = 0
			}
			if pct > 100 {
				pct = 100
			}
			st.CPUPercent = math.Round(pct*10) / 10
		}
		c.cpuPrev = cur
	}

	// —— 内存: MemTotal - MemAvailable (与 free -m 的 available 口径一致) ——
	if total, avail, err := readMemInfo(); err == nil {
		st.MemTotalBytes = total
		if avail <= total {
			st.MemUsedBytes = total - avail
		}
	}

	// —— 网卡速率: /proc/net/dev 差分 ÷ 真实间隔 (排除 lo, 只算物理出口) ——
	if cur, err := readNetDev(); err == nil {
		if !c.prevTime.IsZero() {
			if secs := now.Sub(c.prevTime).Seconds(); secs > 0 {
				st.NetRxRate = ratePerSec(cur.rx, c.netPrev.rx, secs)
				st.NetTxRate = ratePerSec(cur.tx, c.netPrev.tx, secs)
			}
		}
		c.netPrev = cur
	}
	c.prevTime = now

	// —— 系统运行时间 ——
	if up, err := readUptime(); err == nil {
		st.UptimeSec = up
	}

	// —— 内核状态: 233boy 的 v2ray/caddy 都是 systemd 单元, agent 只读不接管 ——
	st.V2rayStatus, st.V2rayPid = systemdUnitState(c.V2rayUnit)
	st.CaddyStatus, st.CaddyPid = systemdUnitState(c.CaddyUnit)
	st.V2rayVer = c.cachedVersion(&c.v2rayVer, c.V2rayBin, []string{"version"}, parseV2rayVersion)
	st.CaddyVer = c.cachedVersion(&c.caddyVer, c.CaddyBin, []string{"version"}, firstField)

	// —— conf 数量 (与 common.Runner.ConfFiles 同一口径) ——
	st.ConfCount = countConfFiles(common.DefaultConfDir)

	return st
}

// countConfFiles 数 conf 目录下真实的节点配置 (跳过 dynamic-port- 前缀的辅助文件)。
func countConfFiles(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, "dynamic-port-") {
			continue
		}
		n++
	}
	return n
}

// cachedVersion 读一次内核版本并缓存 (运行期不变)。读失败则下次心跳重试。
func (c *Collector) cachedVersion(cache *string, bin string, args []string, pick func(string) string) string {
	if *cache != "" {
		return *cache
	}
	out, err := runOut(8*time.Second, bin, args...)
	if err != nil || out == "" {
		return ""
	}
	v := pick(out)
	if v != "" {
		*cache = v
	}
	return v
}

// systemdUnitState 返回 ("running"|"stopped"|"error", mainPID)。
// 面板侧 handleHeartbeat 用 map[string]any 写库, 0/"" 会被如实落盘 —— 所以这里
// 单元不活跃时返回 0 是**有效事实**而不是"缺数据"。
func systemdUnitState(unit string) (string, int) {
	status := "stopped"
	switch strings.TrimSpace(runText(6*time.Second, "systemctl", "is-active", unit)) {
	case "active":
		status = "running"
	case "failed", "error":
		status = "error"
	}
	pid := 0
	if status == "running" {
		if s, err := runOut(6*time.Second, "systemctl", "show", "-p", "MainPID", "--value", unit); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
				pid = n
			}
		}
	}
	return status, pid
}

// ---------------------------------------------------------------------------
// /proc 解析
// ---------------------------------------------------------------------------

// readCPUStat 读 /proc/stat 首行 (整机 CPU 汇总)。
func readCPUStat() (cpuSample, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuSample{}, err
	}
	line := string(data)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuSample{}, fmt.Errorf("意外的 /proc/stat 首行: %q", line)
	}
	var s cpuSample
	for i, f := range fields[1:] {
		// 第 9、10 列 (guest, guest_nice) 已经计在 user/nice 里, 再加会重复计数。
		if i == 8 || i == 9 {
			continue
		}
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			continue
		}
		// 第 4、5 列是 idle 与 iowait, 都算空闲。
		if i == 3 || i == 4 {
			s.idle += v
		}
		s.total += v
	}
	return s, nil
}

// readMemInfo 读 /proc/meminfo, 返回 (MemTotal, MemAvailable) 字节数。
func readMemInfo() (total, avail uint64, err error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, e := strconv.ParseUint(f[1], 10, 64)
		if e != nil {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			avail = v * 1024
		}
	}
	if total == 0 {
		return 0, 0, fmt.Errorf("/proc/meminfo 未读到 MemTotal")
	}
	return total, avail, nil
}

// readNetDev 汇总所有非 lo 网卡的收发字节数。
func readNetDev() (netSample, error) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return netSample{}, err
	}
	var s netSample
	for _, line := range strings.Split(string(data), "\n") {
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		name := strings.TrimSpace(line[:i])
		if name == "" || name == "lo" {
			continue
		}
		f := strings.Fields(line[i+1:])
		if len(f) < 9 {
			continue
		}
		rx, e1 := strconv.ParseUint(f[0], 10, 64)
		tx, e2 := strconv.ParseUint(f[8], 10, 64)
		if e1 != nil || e2 != nil {
			continue
		}
		s.rx += rx
		s.tx += tx
	}
	return s, nil
}

// readUptime 读 /proc/uptime 首字段 (秒)。
func readUptime() (uint64, error) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(data))
	if len(f) == 0 {
		return 0, fmt.Errorf("/proc/uptime 为空")
	}
	sec, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, err
	}
	if sec < 0 {
		return 0, nil
	}
	return uint64(sec), nil
}

// ratePerSec 由两次累计值算速率。计数器归零 (重启/回绕) 时返回 0 而不是负数。
func ratePerSec(cur, prev uint64, secs float64) uint64 {
	if secs <= 0 || cur <= prev {
		return 0
	}
	return uint64(float64(cur-prev) / secs)
}

// ---------------------------------------------------------------------------
// 命令执行 (本包统一出口: systemctl / journalctl / 内核二进制)
// ---------------------------------------------------------------------------

// runOut 执行命令, 返回 stdout (已 TrimSpace)。失败时把 stderr 摘要放进 error。
func runOut(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C.UTF-8", "LANG=C.UTF-8", "TERM=dumb")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return strings.TrimSpace(stdout.String()), fmt.Errorf("%s 执行超时 (%s)", name, timeout)
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return strings.TrimSpace(stdout.String()), fmt.Errorf("%s: %s", name, msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// runText 执行命令并只取 stdout, 忽略退出码。
// systemctl is-active 对 inactive 单元会返回非 0, 但它的 stdout 才是我们要的答案。
func runText(timeout time.Duration, name string, args ...string) string {
	out, _ := runOut(timeout, name, args...)
	return out
}

// firstLine 取首行 (日志/版本号解析用)。
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// firstField 取首行首个空白分隔字段 ("v2.11.4 h1:..." -> "v2.11.4")。
func firstField(s string) string {
	f := strings.Fields(firstLine(s))
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// parseV2rayVersion 从 `v2ray version` 首行里抽出 "5.53.0"。
// 实测首行: "V2Ray 5.53.0 (V2Fly, a community-driven edition of V2Ray.) Custom (go1.26.1 linux/amd64)"
func parseV2rayVersion(s string) string {
	f := strings.Fields(firstLine(s))
	for i, tok := range f {
		if (tok == "V2Ray" || tok == "v2ray") && i+1 < len(f) {
			return f[i+1]
		}
	}
	return firstField(s)
}
