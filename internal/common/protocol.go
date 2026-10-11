package common

import (
	"encoding/json"
	"time"
)

// ===========================================================================
// 反向 WebSocket 消息契约 (Agent 主动拨 Panel 的 /ws/agent)
//
// ⚠️ 为什么这里和 work-v 的契约长得不一样 —— 这是本项目最关键的架构差异:
//
//	work-v 的节点内核是 sing-box, 面板可以下发一份「期望配置」(inbounds + users),
//	agent 做「声明式收敛」。所以它有 config_push / config_ack / cutoff_user。
//
//	233boy 不是这样: 它没有「一份配置文件」的概念, 而是**一堆独立的 conf 实例**
//	(/etc/v2ray/conf/<name>.json, 每个单 inbound 单用户), 靠交互式 CLI 增删。
//	面板能做的只有「下达操作」(建一个 conf / 删一个 conf / 重启 / 取日志),
//	agent 执行后回报**事实**(conf 目录变成了什么样), 而不是回报「配置已应用」。
//
//	因此本契约是 **操作型 (imperative) + 清单型 (inventory)** 的,
//	而不是 work-v 的声明型 (declarative)。硬套 config_push 会导致:
//	  - 无法表达「这个 conf 是面板建的还是手工建的」(233boy 允许手工加);
//	  - 无法表达「删除」以外的一切运维动作;
//	  - 流量计数是 per-inbound-TAG 而非 per-user, 声明式模型无处安放。
//
// 另外: 233boy 的**公网端口是机器级的** (Caddy https_port 整机共享, 82 上是 55509),
// 同机多节点靠 path 区分。所以 inventory 里必须带 HTTPPort/HTTPSPort 两个机器级字段,
// 绝不能假设「一个节点一个公网口」。
// ===========================================================================

const (
	// —— 握手与心跳 ——
	MsgTypeAuthReq   = "auth_req"  // Agent -> Panel: 携带节点 Token 认证
	MsgTypeAuthResp  = "auth_resp" // Panel -> Agent: 认证结果
	MsgTypeHeartbeat = "heartbeat" // Agent -> Panel: 硬件指标 + 内核状态
	MsgTypeCommand   = "command"   // Panel -> Agent: 通用指令 (如强制重采集)

	// —— 清单 (inventory): agent 回报「机器现在长什么样」——
	MsgTypeInventoryReport  = "inventory_report"  // Agent -> Panel: 三层合并后的节点清单
	MsgTypeInventoryReq     = "inventory_req"     // Panel -> Agent: 请求立刻重采集
	MsgTypeCapabilityReport = "capability_report" // Agent -> Panel: 协议能力/内核版本

	// —— 操作 (imperative): 面板下达动作, agent 回报事实 ——
	MsgTypeNodeOp     = "node_op"      // Panel -> Agent: 建/删/重启/取日志/预检/探测
	MsgTypeNodeOpResp = "node_op_resp" // Agent -> Panel: 操作结果 (含分步耗时)

	// —— 流量 ——
	MsgTypeTraffic = "traffic_report" // Agent -> Panel: per-inbound 增量流量
	// Agent -> Panel: per-user 增量流量 (真·按账号计量, 靠 V2Ray 的
	// policy.statsUserUplink/Downlink + clients[].email 实现)
	MsgTypeUserTraffic = "user_traffic_report"
	// Panel -> Agent: 把「哪些用户该有权限」同步到机器上
	// (往 conf 的 clients[] 增删 UUID, 并给每个 client 打上 email 标记)
	MsgTypeUserSync = "user_sync"

	// —— 兼容 work-v 命名保留的独立日志/重启消息 ——
	// (node_op 里也能表达, 但独立消息更利于 hub 侧做请求-响应配对与超时)
	MsgTypeGetLogsReq  = "get_logs_req"  // Panel -> Agent: 请求内核运行日志
	MsgTypeGetLogsResp = "get_logs_resp" // Agent -> Panel: 响应内核日志
	MsgTypeRestartReq  = "restart_req"   // Panel -> Agent: 请求重启 v2ray
	MsgTypeRestartResp = "restart_resp"  // Agent -> Agent: 响应重启结果
)

// WSMessage 统一长连通信帧结构
type WSMessage struct {
	Type      string          `json:"type"`                 // 消息类型
	NodeToken string          `json:"node_token,omitempty"` // 节点 Token
	Timestamp int64           `json:"timestamp"`            // 毫秒时间戳
	Payload   json.RawMessage `json:"payload,omitempty"`    // 负载数据
}

// NewWSMessage 辅助构造消息
func NewWSMessage(msgType string, payload any) (*WSMessage, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &WSMessage{
		Type:      msgType,
		Timestamp: time.Now().UnixMilli(),
		Payload:   data,
	}, nil
}

// Version Agent 版本号 (随节点心跳上报)
const Version = "1.0.0"

// AgentVersion 兼容别名
const AgentVersion = Version

// ---------------------------------------------------------------------------
// 握手
// ---------------------------------------------------------------------------

// AuthReqPayload Agent 首帧认证请求
type AuthReqPayload struct {
	Token    string `json:"token"`              // 节点专属 Token
	Version  string `json:"version,omitempty"`  // Agent 版本
	Hostname string `json:"hostname,omitempty"` // 目标机主机名 (便于面板展示)
}

// AuthRespPayload 面板认证结果
type AuthRespPayload struct {
	Success    bool   `json:"success"`
	Message    string `json:"message,omitempty"`
	NodeID     uint   `json:"node_id,omitempty"`
	NodeName   string `json:"node_name,omitempty"`
	ServerTime int64  `json:"server_time,omitempty"` // 毫秒, 供 agent 校正时钟
}

// ---------------------------------------------------------------------------
// 心跳: 硬件 + 内核状态
// ---------------------------------------------------------------------------

// NodeHardwareStats 节点物理状态上报载荷
type NodeHardwareStats struct {
	CPUPercent    float64 `json:"cpu_percent"`         // CPU 使用率 0~100
	MemUsedBytes  uint64  `json:"mem_used_bytes"`      // 内存已用 (Bytes)
	MemTotalBytes uint64  `json:"mem_total_bytes"`     // 内存总量 (Bytes)
	NetRxRate     uint64  `json:"net_rx_rate"`         // 实时下行速率 (Bytes/s)
	NetTxRate     uint64  `json:"net_tx_rate"`         // 实时上行速率 (Bytes/s)
	V2rayStatus   string  `json:"v2ray_status"`        // "running" / "stopped" / "error"
	V2rayPid      int     `json:"v2ray_pid"`           // v2ray 主进程 PID
	V2rayVer      string  `json:"v2ray_ver,omitempty"` // 内核版本 (实测 v5.53.0)
	CaddyStatus   string  `json:"caddy_status"`        // Caddy 状态 (TLS 节点依赖它终结 TLS)
	CaddyPid      int     `json:"caddy_pid,omitempty"`
	CaddyVer      string  `json:"caddy_ver,omitempty"`
	ConfCount     int     `json:"conf_count,omitempty"` // /etc/v2ray/conf 下的节点数
	UptimeSec     uint64  `json:"uptime_sec"`           // 系统运行时间
	Version       string  `json:"version"`              // Agent 版本
}

// ---------------------------------------------------------------------------
// 清单 (inventory)
// ---------------------------------------------------------------------------

// InboundView 一个 233boy conf 实例的三层合并视图 —— 契约层的线格式。
//
// ⚠️ 为什么不直接用 agent.NodeView: internal/agent 依赖 internal/common,
// 反向引用会形成 import 环。这里是**线格式 (wire DTO)**, 由 agent 侧做转换,
// 好处是「协议契约」与「本地解析实现」解耦 —— 解析器重构不会破坏面板。
//
// 字段分三层存放, 来源不可混:
//   - conf 层   → InternalPort / ListenAddr / InboundNet / InboundSec / UUID / Password / Method
//   - url 层    → Address / PublicPort / Host / Path / ClientSecurity / ClientNetwork / URI
//   - Caddy 层  → Caddy* (仅用于交叉校验, 不作为权威)
//
// ⚠️ InboundSec 恒为 "none" 而 ClientSecurity 为 "tls" 是 233boy 的**设计如此**:
// TLS 在 Caddy 终结, 回环到内网口是明文。两者不可合并 (合并会掩盖真实链路)。
type InboundView struct {
	ConfName string `json:"conf_name"`           // VLESS-WS-TLS-node1.example.com.json
	ConfFile string `json:"conf_file,omitempty"` // 绝对路径

	// —— conf 层 (权威: 内网口 / 协议 / 凭据) ——
	Protocol      string `json:"protocol"` // vless / vmess / trojan / shadowsocks / ...
	UUID          string `json:"uuid,omitempty"`
	Password      string `json:"password,omitempty"`
	Method        string `json:"method,omitempty"`
	User          string `json:"user,omitempty"`
	InternalPort  int    `json:"internal_port"`
	ListenAddr    string `json:"listen_addr"`           // 127.0.0.1 或 0.0.0.0
	InboundNet    string `json:"inbound_net,omitempty"` // streamSettings.network
	InboundSec    string `json:"inbound_sec,omitempty"` // streamSettings.security (恒 none)
	IsDynamicPort bool   `json:"is_dynamic_port,omitempty"`

	// —— url 层 (权威: 公网地址 / 端口 / path) ——
	Address        string `json:"address,omitempty"`
	PublicPort     int    `json:"public_port,omitempty"`
	Host           string `json:"host,omitempty"`
	Path           string `json:"path,omitempty"`
	ClientSecurity string `json:"client_security,omitempty"`
	ClientNetwork  string `json:"client_network,omitempty"`
	URI            string `json:"uri,omitempty"`

	// —— Caddy 层 (交叉校验用) ——
	CaddyFile     string `json:"caddy_file,omitempty"`
	CaddySite     string `json:"caddy_site,omitempty"`
	CaddyHost     string `json:"caddy_host,omitempty"`
	CaddyPort     int    `json:"caddy_port,omitempty"`
	CaddyUpstream int    `json:"caddy_upstream,omitempty"`
	CaddyPath     string `json:"caddy_path,omitempty"`

	HasConf  bool `json:"has_conf"`
	HasCaddy bool `json:"has_caddy"`
	HasURL   bool `json:"has_url"`

	// 三层不一致时记录 (不阻断: 233boy 允许手工改过 conf, 只提示)
	Warnings []string `json:"warnings,omitempty"`
}

// InventoryReportPayload Agent 回报整机节点清单。
//
// HTTPPort / HTTPSPort 是**机器级**的 (Caddy 的 http_port / https_port),
// 82 上实测为 51350 / 55509。同机所有节点共用同一个公网端口, 靠 path 区分 ——
// 所以这两个字段挂在机器级载荷上, 而不是挂在每个 InboundView 上。
type InventoryReportPayload struct {
	HTTPPort  int           `json:"http_port"`  // Caddy http_port (ACME 用)
	HTTPSPort int           `json:"https_port"` // Caddy https_port (真实数据面入口)
	Nodes     []InboundView `json:"nodes"`
	// Complete 表示本次采集是否可信 (conf 目录读到, 且各层解析没有致命失败)。
	// ⚠️ 面板**只在 Complete=true 时**才把「清单里没有的 conf」判定为已被删除并从库里清理;
	// 否则一次采集异常会被误读成「用户把所有节点都删了」, 直接把订阅清空。
	Complete    bool     `json:"complete"`
	Warnings    []string `json:"warnings,omitempty"`
	CollectedAt int64    `json:"collected_at"`          // 毫秒
	DurationMs  int64    `json:"duration_ms,omitempty"` // 采集耗时
}

// CapabilityReportPayload Agent 回报本机 233boy 能力。
//
// Reality 恒为 false: v4.27 的 add() 里 reality 分支已被注释
// (`v2ray gen vless-reality auto` → "错误! 无法识别")。如实上报由 UI 灰掉,
// **绝不 patch /etc/v2ray/sh/** —— 升级会覆盖, 且违反「不注入」红线。
type CapabilityReportPayload struct {
	Protocols    []ProtocolDef `json:"protocols"`
	Reality      bool          `json:"reality"`
	RealityWhy   string        `json:"reality_why,omitempty"`
	HasCaddy     bool          `json:"has_caddy"`
	CoreVersion  string        `json:"core_version,omitempty"`  // v2ray v5.53.0
	CaddyVersion string        `json:"caddy_version,omitempty"` // caddy v2.11.4
	ConfDir      string        `json:"conf_dir,omitempty"`
	BinPath      string        `json:"bin_path,omitempty"`
	Protected    []string      `json:"protected,omitempty"` // 本机保护名单 (子串匹配)
}

// ---------------------------------------------------------------------------
// 操作 (imperative)
// ---------------------------------------------------------------------------

// 面板可下达的节点操作名。
const (
	NodeOpInventory = "inventory" // 立刻重采集清单
	NodeOpPreview   = "preview"   // gen 干跑, 不落盘 (参数不全由 AddSpec.BuildArgs 兜底, 不靠 gen 自己)
	NodeOpCreate    = "create"    // gen -> add -> 校验 conf 出现 (失败自动回滚)
	NodeOpDelete    = "delete"    // 按 ConfName 删除
	NodeOpRestart   = "restart"   // systemctl restart v2ray (+ caddy)
	NodeOpLogs      = "logs"      // 取内核日志
	NodeOpProbe     = "probe"     // 公网可达性探测 (TLS 握手 + WS Upgrade 断言 101)
	// NodeOpSyncUsers 把面板的用户名单同步进各 conf 的 clients[]。
	// 与其他 op 不同, 它**不经过 233boy 脚本** —— 直接改 conf 里的 clients 数组
	// 再重启内核。原因: 233boy 的 add() 只会写一个 client, 没有多用户接口,
	// 而 clients[] 本身就是数组, 直接增删是唯一可行的真限流路径。
	NodeOpSyncUsers = "sync_users"
)

// NodeOpPayload 面板 -> Agent 的操作请求。
//
// OpID 由面板生成 (uuid), agent 必须在 NodeOpResultPayload 里原样回填,
// 否则 hub 侧无法做请求-响应配对。
type NodeOpPayload struct {
	OpID string `json:"op_id"`
	Op   string `json:"op"`

	// Create/Preview 用。AddSpec 的完整性由 BuildArgs() 在 agent 侧二次校验 ——
	// 不信任面板传来的参数, 因为参数不全时 233boy 会掉进 while read 自旋。
	Spec *AddSpec `json:"spec,omitempty"`

	ConfName string `json:"conf_name,omitempty"` // Delete/Probe 用

	Lines int `json:"lines,omitempty"` // Logs 用, 默认 200

	ProbeTimeoutSec int `json:"probe_timeout_sec,omitempty"` // Probe 用, 默认 8

	// SyncUsers 用。面板给出「这台机器上应该有哪些用户」的**全量**名单,
	// agent 负责让 conf 与之对齐 (增、删、以及给已有 client 补 email 标记)。
	//
	// 为什么是全量而不是增量: 增量在下发丢失/agent 重启/面板回滚后会漂移,
	// 而全量是幂等的 —— 重发多少次结果都一样, 排障时也只要看一份名单。
	// 名单最多几十条, 传输成本可以忽略。
	Users []UserCredential `json:"users,omitempty"`
}

// UserCredential 一个订阅用户在节点上的凭据。
//
// Email 是关键: V2Ray 的按用户计数器形如 `user>>>EMAIL>>>traffic>>>uplink`,
// 面板靠它把流量归到具体账号。因此 email 必须**稳定且唯一** ——
// 用 `u<用户ID>@v2panel` 而不是用户名, 因为用户名可改, 改名会让历史计数错位。
type UserCredential struct {
	UserID   uint   `json:"user_id"`
	Email    string `json:"email"`              // V2Ray 计数器里的标识, 形如 u12@v2panel
	UUID     string `json:"uuid"`               // 客户端鉴权 UUID
	Password string `json:"password,omitempty"` // ss/trojan 类协议用
	Enabled  bool   `json:"enabled"`            // false = 该用户已被熔断, 不应出现在 clients[]
}

// UserTrafficEntry 单个用户的流量增量 (agent -> 面板)。
type UserTrafficEntry struct {
	UserID uint   `json:"user_id"`
	Email  string `json:"email"`
	Up     int64  `json:"up"`
	Down   int64  `json:"down"`
}

// UserTrafficPayload per-user 流量上报载荷。
//
// Entries 是**增量** (与 TrafficReportPayload 同一套路): 面板累加到 User.UsedTraffic。
// 计数器重启归零由 agent 侧处理 (负数钳到 0), 面板只负责加。
type UserTrafficPayload struct {
	CollectedAt int64              `json:"collected_at"` // 毫秒
	NodeID      uint               `json:"node_id,omitempty"`
	Entries     []UserTrafficEntry `json:"entries"`
	// Reset 为真表示 agent 检测到计数器重置 (v2ray 重启), 面板应把本次增量当基线
	// 而非真实用量 —— 避免重启后第一轮把「归零」误记成负数/异常值。
	Reset bool `json:"reset,omitempty"`
}

// StepInfo 一次操作中的单个步骤 —— 契约层线格式 (对应 agent.Step)。
type StepInfo struct {
	Name       string `json:"name"`
	OK         bool   `json:"ok"`
	Detail     string `json:"detail,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

// NodeOpResultPayload Agent -> Panel 的操作结果。
//
// ⚠️ OK 由**事实**决定, 不信 233boy 的退出码:
// 脚本全篇没有 set -e, 且 del() 最后一句 `[[ $is_dont_auto_exit ]] && unset ...`
// 在 CLI 路径上恒为假 → shell 函数返回 1, **即使删除成功也是 exit 1**。
// 所以 Delete 的成败判据是「conf 目录 before/after 对比」, delNote 只作附注。
type NodeOpResultPayload struct {
	OpID  string `json:"op_id"`
	Op    string `json:"op"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`

	Steps      []StepInfo `json:"steps,omitempty"`
	Warnings   []string   `json:"warnings,omitempty"`
	RolledBack bool       `json:"rolled_back,omitempty"` // 失败后是否已删除半成品 conf

	// Create/Delete 的结果事实
	ConfName     string `json:"conf_name,omitempty"`
	URI          string `json:"uri,omitempty"`
	InternalPort int    `json:"internal_port,omitempty"`
	PublicPort   int    `json:"public_port,omitempty"`
	Path         string `json:"path,omitempty"`

	// Preview 的 gen JSON / Logs 的日志正文 / Probe 的探测摘要
	Output string `json:"output,omitempty"`

	DurationMs int64 `json:"duration_ms,omitempty"`
}

// ---------------------------------------------------------------------------
// 流量
// ---------------------------------------------------------------------------

// InboundDeltaTraffic 单个 inbound 在采样周期内的**增量**流量。
//
// ⚠️ 为什么按 ConfName 而不是按 UUID 上报: 233boy 的 StatsService 计数器是
// **per-inbound-TAG** 的 (`inbound>>>TAG>>>traffic>>>uplink`), 根本没有 per-user 维度。
// agent 只做「读计数器 + 与上次快照相减 + 重启归零钳到 0」, 归属关系由面板按
// Inbound.ConfName -> Inbound.UserID 自己映射。
type InboundDeltaTraffic struct {
	ConfName string `json:"conf_name"`
	Up       int64  `json:"up"`
	Down     int64  `json:"down"`
}

// TrafficReportPayload Agent 上报的一组 inbound 增量流量。
type TrafficReportPayload struct {
	Inbounds []InboundDeltaTraffic `json:"inbounds"`
}

// StatsCounter 一条 v2ray StatsService 计数器的**累计值** (非增量)。
// 仅供面板 API 调试展示与「与 v2ray xapi stats -json 手算对齐」的验证用。
//
// ⚠️ 只取 `inbound>>>` 维度: `Total` 含 outbound, 约等于真实流量的 2 倍, 永远不用。
// ⚠️ 零流量条目整条没有 `value` 字段 → 解析必须容忍缺失 (当作 0)。
type StatsCounter struct {
	ConfName string `json:"conf_name"` // 计数器里的 inbound tag
	Up       int64  `json:"up"`
	Down     int64  `json:"down"`
}

// UserStatsCounter 单个用户的累计流量计数器。
//
// 来源是 V2Ray 的 `user>>>EMAIL>>>traffic>>>uplink|downlink`。
// 这些计数器**只有在 policy.levels.0.statsUserUplink/Downlink 为 true、
// 且 client 带 email 字段时**才会出现 —— 两者缺一, 读出来就是空的。
type UserStatsCounter struct {
	Email string `json:"email"`
	Up    int64  `json:"up"`
	Down  int64  `json:"down"`
}

// ---------------------------------------------------------------------------
// 日志 / 重启 (独立消息, 便于 hub 侧配对与超时)
// ---------------------------------------------------------------------------

// GetLogsReqPayload 面板请求日志
type GetLogsReqPayload struct {
	Source string `json:"source,omitempty"` // "v2ray" / "caddy" / "agent", 空=v2ray
	Lines  int    `json:"lines,omitempty"`  // 默认 200
}

// GetLogsRespPayload Agent 返回日志
type GetLogsRespPayload struct {
	Source  string `json:"source,omitempty"`
	Content string `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
}

// RestartRespPayload 重启结果
type RestartRespPayload struct {
	Success  bool   `json:"success"`
	Message  string `json:"message,omitempty"`
	V2rayPid int    `json:"v2ray_pid,omitempty"`
	CaddyPid int    `json:"caddy_pid,omitempty"`
}
