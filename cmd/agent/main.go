package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"v2panel/internal/agent"
	"v2panel/internal/common"
)

// ===========================================================================
// v2panel Node Agent
//
// 反向拨号到面板的 /ws/agent, 驱动本机的 233boy 脚本。
//
// ⚠️ 与 work-v 的 agent 有一个本质区别: 这里的 v2ray / caddy 是 **233boy 装的
// systemd 单元**, 归系统所有, 不是 agent 的子进程。所以进程退出时 **绝不能**
// 去 Stop 内核 —— 那会把整台机器上别人正在用的节点一起打掉。agent 只读状态 +
// 按面板指令 `systemctl restart`。
//
// 启动方式由 panel 的 install.sh 固定为:
//   /usr/local/bin/v2panel-agent -config /etc/v2panel-agent/config.json
// 所以 -config 是必须支持的形态; -server/-token 作为调试覆盖保留。
// ===========================================================================

// fileConfig 与 install.sh 写入的 /etc/v2panel-agent/config.json 对应。
// 键名不能改 —— 安装脚本是按这三个键生成的。
type fileConfig struct {
	Server   string `json:"server"`
	Token    string `json:"token"`
	Insecure bool   `json:"insecure"`
	// Protected 可选: 本机保护名单 (子串匹配), 任何写操作前都过这道闸。
	Protected []string `json:"protected,omitempty"`
}

func main() {
	configPath := flag.String("config", "/etc/v2panel-agent/config.json", "配置文件路径 (JSON: server/token/insecure/protected)")
	server := flag.String("server", "", "面板地址 (覆盖配置文件, 例如 https://vp.example.com)")
	// 令牌优先取环境变量 V2PANEL_TOKEN: 这样令牌不会出现在 /proc/<pid>/cmdline 里
	// 被同机其他用户读到 (install.sh 用的是 0600 的配置文件, 同样出于这个考虑)。
	token := flag.String("token", os.Getenv("V2PANEL_TOKEN"), "节点接入令牌 (覆盖配置文件)")
	insecure := flag.Bool("insecure", false, "跳过面板 TLS 证书校验 (面板用自签证书时才需要)")
	protected := flag.String("protected", "", "受保护节点名, 逗号分隔 (子串匹配); 命中者拒绝任何写操作")
	showVer := flag.Bool("version", false, "打印版本并退出")
	flag.Parse()

	if *showVer {
		fmt.Printf("v2panel-agent %s\n", common.AgentVersion)
		os.Exit(0)
	}

	cfg := loadConfig(*configPath)

	// 优先级: 命令行 > 环境变量 > 配置文件
	if *server != "" {
		cfg.Server = *server
	}
	if *token != "" {
		cfg.Token = *token
	}
	if isFlagPassed("insecure") {
		cfg.Insecure = *insecure
	}
	if strings.TrimSpace(*protected) != "" {
		cfg.Protected = splitList(*protected)
	}

	cfg.Server = strings.TrimSpace(cfg.Server)
	cfg.Token = strings.TrimSpace(cfg.Token)
	if cfg.Server == "" || cfg.Token == "" {
		log.Fatal("❌ 必须提供面板地址与接入令牌 (--server / --token, 或 -config 指向的配置文件)")
	}

	log.Println("==================================================")
	log.Println("          v2panel Node Agent (233boy)             ")
	log.Println("==================================================")
	log.Printf("[Agent] 面板地址: %s", cfg.Server)
	log.Printf("[Agent] 节点令牌: %s", maskToken(cfg.Token))
	log.Printf("[Agent] 版本:     %s", common.AgentVersion)
	if len(cfg.Protected) > 0 {
		log.Printf("[Agent] 保护名单: %s", strings.Join(cfg.Protected, ", "))
	} else {
		log.Printf("[Agent] 保护名单: (空 —— 面板可改动本机任何节点)")
	}
	log.Printf("[Agent] 配置来源: %s", *configPath)

	client := agent.NewClient(cfg.Server, cfg.Token, cfg.Insecure)
	if len(cfg.Protected) > 0 {
		client.Driver.Protected = cfg.Protected
	}

	go client.Start()

	// 优雅退出: 只断开长连, **不碰内核** (v2ray/caddy 由 systemd 托管)。
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	sig := <-sigChan
	log.Printf("[Agent] 收到信号 %v, 正在断开与面板的长连...", sig)
	client.Close()
	log.Println("[Agent] 已退出 (内核 v2ray/caddy 由 systemd 继续托管, 未受影响)")
}

// loadConfig 读配置文件。文件不存在不算错误 —— 允许纯命令行启动 (调试用)。
func loadConfig(path string) fileConfig {
	var cfg fileConfig
	path = strings.TrimSpace(path)
	if path == "" {
		return cfg
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[Agent] 读取配置 %s 失败: %v", path, err)
		}
		return cfg
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		log.Printf("[Agent] 配置 %s 不是合法 JSON: %v", path, err)
	}
	return cfg
}

// isFlagPassed 判断某个 flag 是否被显式传入。
// 不能只看 *insecure 的值: 默认 false 与"显式传 -insecure=false"无法区分,
// 后者应当能覆盖配置文件里的 true。
func isFlagPassed(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func splitList(s string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == ';' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// maskToken 日志里不回显完整令牌 (日志可能被转发/留存)。
func maskToken(t string) string {
	if len(t) <= 6 {
		return "***"
	}
	return t[:6] + "***"
}
