package agent

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"v2panel/internal/common"
)

// ===========================================================================
// 运维动作: 读日志 / 重启内核。
//
// ⚠️ 本包**只读** 233boy 的 systemd 单元状态, 重启也只用 systemctl ——
// 绝不去 kill 进程或改单元文件。233boy 的 v2ray/caddy 是它自己管理的单元,
// agent 不是它们的所有者, 越权操作会和脚本的 `v2ray restart` 互相打架。
//
// ⚠️ 没有日志文件: 82 上实测 `/etc/v2ray/log` 不存在、`/var/log/caddy` 不存在,
// 内核日志全在 journald 里。所以日志唯一来源是 journalctl。
// ===========================================================================

// 日志来源标识 (与面板 GetLogsReqPayload.Source 对应)。
const (
	LogSourceV2ray = "v2ray"
	LogSourceCaddy = "caddy"
	LogSourceAgent = "agent"
)

// ReadLogs 读内核/agent 日志。
//
// source 为空默认 v2ray; lines <= 0 默认 200, 上限 2000 (一次拉太多会撑爆 WS 帧)。
func (d *Driver) ReadLogs(source string, lines int) (string, error) {
	if lines <= 0 {
		lines = 200
	}
	if lines > 2000 {
		lines = 2000
	}

	unit := DefaultV2rayUnit
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "", LogSourceV2ray:
		unit = DefaultV2rayUnit
	case LogSourceCaddy:
		unit = DefaultCaddyUnit
	case LogSourceAgent:
		// agent 自己的日志也走 systemd (install.sh 装的是 v2panel-agent.service)。
		unit = "v2panel-agent.service"
	default:
		return "", fmt.Errorf("未知日志来源: %q (可选 v2ray/caddy/agent)", source)
	}

	// --no-pager 必须加: 否则 journalctl 会调 less, 在无 tty 的子进程里挂住直到超时。
	// -o short-iso 让每行自带时间戳, 面板直接展示, 不需要再拼时间。
	out, err := runOut(12*time.Second, "journalctl", "-u", unit, "-n", strconv.Itoa(lines), "--no-pager", "-o", "short-iso")
	if err != nil {
		// 单元不存在时 journalctl 会报 "No journal files were found" 之类, 此时
		// 退回读 agent 自身的 stderr 没意义 —— 直接把原始错误交给面板, 让它如实显示。
		if out == "" {
			return "", fmt.Errorf("读取 %s 日志失败: %v", unit, err)
		}
		// 有输出但退出码非 0 (例如 --since 越界): 仍然把内容给出去。
		return out, nil
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Sprintf("(%s 暂无日志输出)", unit), nil
	}
	return out, nil
}

// RestartServices 重启 v2ray 内核 (以及 Caddy, 如果它在跑)。
//
// 只重启**正在运行**的 Caddy: 纯动态端口机器上 Caddy 本来就没装/没起,
// 无条件去 restart 会平白报一个失败, 让面板显示成"重启失败"。
//
// 返回 (步骤明细, v2ray 新 PID, 错误)。
func (d *Driver) RestartServices() ([]common.StepInfo, int, error) {
	steps := make([]common.StepInfo, 0, 2)

	// —— v2ray ——
	started := time.Now()
	if _, err := runOut(30*time.Second, "systemctl", "restart", DefaultV2rayUnit); err != nil {
		steps = append(steps, common.StepInfo{
			Name: "重启 v2ray", OK: false, Detail: err.Error(),
			DurationMs: time.Since(started).Milliseconds(),
		})
		return steps, 0, fmt.Errorf("重启 v2ray 失败: %w", err)
	}
	// 重启后立刻读 PID: 单元起没起来, 只看 systemctl 的退出码不可靠 (它只保证
	// "已发起"), 必须回读 MainPID 才算数 —— 与建节点只信 conf 落盘同一原则。
	pid := 0
	for i := 0; i < 10; i++ {
		if s, err := runOut(6*time.Second, "systemctl", "show", "-p", "MainPID", "--value", DefaultV2rayUnit); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
				pid = n
				break
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	v2OK := pid > 0
	v2Detail := fmt.Sprintf("新 PID %d", pid)
	if !v2OK {
		v2Detail = "重启命令已执行, 但 3 秒内未读到新的 MainPID (内核可能启动失败, 请查看日志)"
	}
	steps = append(steps, common.StepInfo{
		Name: "重启 v2ray", OK: v2OK, Detail: v2Detail,
		DurationMs: time.Since(started).Milliseconds(),
	})
	if !v2OK {
		return steps, 0, fmt.Errorf("v2ray 重启后未读到 MainPID, 请查看 v2ray 日志")
	}

	// —— Caddy (仅在运行时) ——
	if st, _ := systemdUnitState(DefaultCaddyUnit); st == "running" {
		started = time.Now()
		if _, err := runOut(30*time.Second, "systemctl", "restart", DefaultCaddyUnit); err != nil {
			steps = append(steps, common.StepInfo{
				Name: "重启 Caddy", OK: false, Detail: err.Error(),
				DurationMs: time.Since(started).Milliseconds(),
			})
			// Caddy 挂了不影响 v2ray 本身, 但公网入口会断 —— 仍算整体失败。
			return steps, pid, fmt.Errorf("重启 Caddy 失败: %w", err)
		}
		cpid := 0
		for i := 0; i < 10; i++ {
			if s, err := runOut(6*time.Second, "systemctl", "show", "-p", "MainPID", "--value", DefaultCaddyUnit); err == nil {
				if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
					cpid = n
					break
				}
			}
			time.Sleep(300 * time.Millisecond)
		}
		ok := cpid > 0
		detail := fmt.Sprintf("新 PID %d", cpid)
		if !ok {
			detail = "重启命令已执行, 但 3 秒内未读到新的 MainPID"
		}
		steps = append(steps, common.StepInfo{
			Name: "重启 Caddy", OK: ok, Detail: detail,
			DurationMs: time.Since(started).Milliseconds(),
		})
		if !ok {
			return steps, pid, fmt.Errorf("Caddy 重启后未读到 MainPID, 公网入口可能不可用")
		}
	}

	return steps, pid, nil
}
