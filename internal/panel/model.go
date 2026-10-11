package panel

import (
	"encoding/json"
	"strings"
	"time"

	"gorm.io/gorm"
)

const (
	DefaultAdminUser = "admin"
	DefaultAdminPass = "admin123456"
	DefaultSafePath  = "/safe_entry"
)

// Admin 面板管理员账号。
type Admin struct {
	ID           uint           `gorm:"primaryKey" json:"id"`
	Username     string         `gorm:"uniqueIndex;size:64;not null" json:"username"`
	PasswordHash string         `gorm:"size:255;not null" json:"-"`
	LastLoginAt  *time.Time     `json:"last_login_at"`
	LastLoginIP  string         `gorm:"size:64" json:"last_login_ip"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	DeletedAt    gorm.DeletedAt `gorm:"index" json:"-"`
}

// Node 一台被纳管的服务器 (对应一个 agent 长连)。
//
// ⚠️ 为什么 Node 与 Inbound 必须分两张表:
//
//	233boy 一台机器上可以有**多个**节点实例, 每个 /etc/v2ray/conf/<name>.json 一个。
//	它们共享**机器级**的 Caddy https_port (82 上是 55509), 靠 path 区分
//	(实测结论, 见 internal/agent/parser.go 顶部注释)。
//	所以:
//	  Node    —— 纳管单位: 持有 agent token、SSH 部署信息、硬件指标、v2ray/caddy 进程状态
//	  Inbound —— 机器上的一个 233boy conf 实例: 订阅链接的载体, 有自己的内网口与 path
//	把 conf 名/公网口直接挂在 Node 上会导致「同机多节点」无法表达。
type Node struct {
	ID      uint    `gorm:"primaryKey" json:"id"`
	Name    string  `gorm:"size:100;not null" json:"name"`
	Token   string  `gorm:"uniqueIndex;size:64;not null" json:"token"` // agent 连接凭证
	Address string  `gorm:"size:255;not null" json:"address"`          // 面板/客户端访问地址 (域名或公网 IP)
	Rate    float64 `gorm:"default:1.0" json:"rate"`                   // 流量计算倍率
	Status  string  `gorm:"size:20;default:'offline'" json:"status"`   // "online" | "offline"
	Hidden  bool    `gorm:"default:false" json:"hidden"`               // 整机隐藏于订阅 (仅管理员可见)

	// Protected 是这台机器上的**保护名单**: 逗号分隔的配置名子串, 命中的 conf
	// 面板拒绝删除/改写 (agent 侧 Driver.guard() 拦)。
	//
	// 为什么是节点级而不是面板级: 保护名单回答的是「这台机器上哪些 conf 不许碰」,
	// 是机器属性。82 上有线上节点 tzq1, 而一台全新机器一个都不需要保护。
	// 该值只在**安装 agent 时**写进 /etc/v2panel-agent/config.json, 事后改这里
	// 不会影响已在运行的 agent —— 要生效需重跑一次纳管命令 (幂等, 只覆盖配置)。
	Protected string `gorm:"size:512" json:"protected"`

	LastSeenAt *time.Time `json:"last_seen_at"`
	LastSyncAt *time.Time `json:"last_sync_at"` // 最近一次三层采集 (节点清单) 时间

	// —— 心跳上报的硬件指标 ——
	CPUPercent float64 `json:"cpu_percent"`
	MemUsed    uint64  `json:"mem_used"`
	MemTotal   uint64  `json:"mem_total"`
	NetRxRate  uint64  `json:"net_rx_rate"` // 实时下行速率 bytes/s
	NetTxRate  uint64  `json:"net_tx_rate"` // 实时上行速率 bytes/s
	UptimeSec  uint64  `json:"uptime_sec"`
	AgentVer   string  `gorm:"size:32" json:"agent_ver"`

	// —— 机器级内核状态 (v2ray 与 caddy 各一个进程, 与节点数无关) ——
	V2rayStatus string `gorm:"size:20" json:"v2ray_status"` // "running" | "stopped" | "error"
	V2rayPid    int    `json:"v2ray_pid"`
	V2rayVer    string `gorm:"size:32" json:"v2ray_ver"`
	CaddyStatus string `gorm:"size:20" json:"caddy_status"`
	CaddyVer    string `gorm:"size:32" json:"caddy_ver"`

	// —— 机器级公网入口 (Caddy 的 http_port/https_port), 订阅与校验都要用 ——
	// ⚠️ HTTPSPort 必须显式声明 column: GORM 的默认 NamingStrategy 会把
	// HTTPSPort 拆成 http_s_port (把 "HTTPS" 当成 S 开头的一个词), 而 inventory.go
	// 的 Updates(map) 写的是 https_port —— 两边不一致导致每次清单入库都报
	// "no such column: https_port", 整个 Inbound 写入跟着一起失败。
	HTTPPort  int `gorm:"column:http_port" json:"http_port"`
	HTTPSPort int `gorm:"column:https_port" json:"https_port"`

	// —— SSH 一键部署 (deploy.go) 状态, 供面板轮询进度 ——
	DeployStatus string `gorm:"size:20;default:''" json:"deploy_status"` // "" | running | success | failed
	DeployLog    string `gorm:"type:text" json:"-"`                      // 安装脚本输出 (末尾截断)
	DeployHost   string `gorm:"size:255" json:"deploy_host"`             // 自动部署使用的 SSH 地址

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

// Inbound 一台机器上的一个 233boy 节点实例 (/etc/v2ray/conf/<ConfName>.json)。
//
// 字段按**权威层**分组 —— 合并规则见 internal/agent/parser.go 的 Merge():
//
//	conf 层 : Protocol/Network/InternalPort/ListenAddr/UUID/Password/Method/User
//	url 层  : Address/PublicPort/Host/Path/ClientSecurity (233boy 的 `v2ray url` 是这几项的权威来源)
//	Caddy 层: CaddyHost/CaddyPort/CaddyUpstream/CaddyPath (公网↔内网映射, 校验用)
//
// 不要把它们合并成单字段: 入站 security 恒为 none、客户端 security 为 tls 是 233boy 的
// **设计如此** (TLS 在 Caddy 终结), 合并会掩盖真实链路。
type Inbound struct {
	ID     uint `gorm:"primaryKey" json:"id"`
	NodeID uint `gorm:"uniqueIndex:idx_node_conf;not null" json:"node_id"`

	// —— conf 层 (权威) ——
	ConfName     string `gorm:"uniqueIndex:idx_node_conf;size:191;not null" json:"conf_name"`
	Protocol     string `gorm:"size:32" json:"protocol"` // vless / vmess / trojan / shadowsocks / socks / http / dokodemo-door
	Network      string `gorm:"size:16" json:"network"`
	InternalPort int    `json:"internal_port"`
	ListenAddr   string `gorm:"size:32" json:"listen_addr"` // 127.0.0.1 => 必须经 Caddy; 0.0.0.0 => 真实直连入口
	InboundSec   string `gorm:"size:16" json:"inbound_security"`
	UUID         string `gorm:"size:64" json:"uuid"`
	Password     string `gorm:"size:128" json:"password"`
	Method       string `gorm:"size:32" json:"method"`
	User         string `gorm:"size:64" json:"user"`

	// —— url 层 (权威) ——
	Address        string `gorm:"size:255" json:"address"`
	PublicPort     int    `json:"public_port"`
	Host           string `gorm:"size:255" json:"host"`
	Path           string `gorm:"size:255" json:"path"`
	ClientSecurity string `gorm:"size:16" json:"client_security"`
	ClientNetwork  string `gorm:"size:16" json:"client_network"`
	URI            string `gorm:"type:text" json:"uri"` // 客户端直连链接

	// —— Caddy 层 ——
	CaddyHost     string `gorm:"size:255" json:"caddy_host"`
	CaddyPort     int    `json:"caddy_port"`
	CaddyUpstream int    `json:"caddy_upstream"`
	CaddyPath     string `gorm:"size:255" json:"caddy_path"`

	HasConf  bool `json:"has_conf"`
	HasCaddy bool `json:"has_caddy"`
	HasURL   bool `json:"has_url"`

	// Enabled=false 表示面板侧不纳入订阅 (但**不动机器上的 conf**, 与 233boy 解耦)
	Enabled bool   `gorm:"default:true" json:"enabled"`
	Remark  string `gorm:"size:255" json:"remark"`
	SortIdx int    `json:"sort_idx"`

	// 三层不一致告警 (JSON 数组文本)。不阻断: 233boy 允许手工改过 conf。
	WarningsJSON string `gorm:"type:text" json:"-"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Warnings 读取三层合并告警。
func (i *Inbound) Warnings() []string {
	if i == nil || i.WarningsJSON == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(i.WarningsJSON), &out); err != nil {
		return nil
	}
	return out
}

// SetWarnings 写入三层合并告警。
func (i *Inbound) SetWarnings(w []string) {
	if len(w) == 0 {
		i.WarningsJSON = ""
		return
	}
	b, err := json.Marshal(w)
	if err != nil {
		return
	}
	i.WarningsJSON = string(b)
}

// Plan 订阅套餐。
//
// 为什么需要套餐而不是直接在用户上填配额:
// 个人用一两个号时直接填字段确实够了, 但一旦有十几个人, 每次开号都要重复填
// 「多少流量 / 多久到期 / 几台设备」, 而且改一次政策要挨个改用户。
// 套餐把这三件事变成模板: 用户从套餐继承配额, 改套餐就能影响一批人。
//
// ⚠️ 用户是**继承**而不是引用配额: 开号时把套餐的配额**快照**进 User 字段。
// 这样后续改套餐不会悄悄改掉已开用户的额度 (用户以为买的是 100G, 结果被降到 50G,
// 那是最招骂的一类变更)。想同步已有用户, 面板提供显式的「同步到该套餐用户」动作。
type Plan struct {
	ID   uint   `gorm:"primaryKey" json:"id"`
	Name string `gorm:"size:64;not null" json:"name"` // 如「基础版」「标准版」
	Sort int    `gorm:"default:0" json:"sort"`        // 展示排序, 小的在前
	Desc string `gorm:"size:255" json:"desc"`         // 一句话说明, 展示在商店/门户

	// —— 配额 ——
	TrafficLimit int64 `gorm:"default:0" json:"traffic_limit"`  // 每周期流量 (Bytes, 0=不限)
	DurationDays int   `gorm:"default:30" json:"duration_days"` // 有效期天数 (0=永久)
	DeviceLimit  int   `gorm:"default:0" json:"device_limit"`   // 同时在线设备数 (0=不限)

	// —— 周期重置 ——
	// reset_mode 决定「流量什么时候归零」, 这是这类面板最容易做错的地方:
	//   none    不重置 —— 一次性流量包, 用完为止 (按量付费的形态)
	//   monthly 每月 1 号 0 点重置
	//   cycle   按到期日重置 (4 号到期 → 每月 4 号重置)
	// 空值按 monthly 处理 (最常见的默认)。
	ResetMode string `gorm:"size:16;default:'monthly'" json:"reset_mode"`

	// —— 节点权限 ——
	// 允许访问的节点 ID 列表 (JSON 数组文本)。空 = 全部节点。
	// 用文本存是因为 SQLite + GORM 下 []uint 的迁移与查询都很别扭,
	// 而这张表最多几十行, 读出来反序列化毫无压力。
	NodeIDs string `gorm:"type:text" json:"-"`

	// —— 售卖相关 (为将来的订单/支付留位, 当前面板不收款) ——
	PriceCents int  `gorm:"default:0" json:"price_cents"`  // 价格 (分), 0=免费/不展示
	ForSale    bool `gorm:"default:false" json:"for_sale"` // 是否在商店展示
	// MaxUsers 套餐最大容纳用户数, 0=不限。人满后不再允许把新用户分到这个套餐。
	MaxUsers int `gorm:"default:0" json:"max_users"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

// PlanNodeIDs 解析 NodeIDs 为切片。空/损坏一律返回 nil (表示「全部节点」)。
func (p *Plan) PlanNodeIDs() []uint {
	s := strings.TrimSpace(p.NodeIDs)
	if s == "" || s == "[]" {
		return nil
	}
	var ids []uint
	if err := json.Unmarshal([]byte(s), &ids); err != nil {
		return nil
	}
	return ids
}

// SetPlanNodeIDs 写入节点白名单。空切片写成 "" (表示全部节点)。
func (p *Plan) SetPlanNodeIDs(ids []uint) error {
	if len(ids) == 0 {
		p.NodeIDs = ""
		return nil
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	p.NodeIDs = string(b)
	return nil
}

// User 订阅账号。
//
// ⚠️ 流量计量粒度 (2026-10 重做):
// 早期实现把「已用流量」记在账号上, 由面板把**整机**增量摊给账号 —— 那在单用户
// 场景碰巧正确, 多用户时完全失真 (一个人跑满, 所有人都被记满)。
//
// 现在改为**真·按用户计量**: agent 把该用户的 UUID 以 `email=<username>` 的
// 形式写进每个 233boy conf 的 clients[] 数组, V2Ray 的
// policy.levels.0.statsUserUplink/Downlink 会为每个 email 单独出计数器
// (实测 82 上该开关本就是开的), agent 用 `user>>>EMAIL>>>traffic>>>...` 读回增量。
// 配额耗尽时 agent 把该 UUID 从 clients[] 摘掉 —— 客户端立刻断线, 这才是真限流。
//
// 约束: clients[0] 必须**永远是 233boy 自己写的那个 UUID**。
// 因为 `v2ray url` 取的是 clients[0].id 生成客户端链接, 动它会毁掉该节点的
// 地址/path 推导 (见 internal/agent/parser.go)。订阅用户一律从 clients[1] 开始追加。
type User struct {
	ID       uint   `gorm:"primaryKey" json:"id"`
	Username string `gorm:"size:64;not null" json:"username"`
	Password string `gorm:"size:64;default:''" json:"password"`            // 订阅门户/订阅链接鉴权口令
	UUID     string `gorm:"uniqueIndex;size:64;not null" json:"uuid"`      // 客户端鉴权 UUID (写进 conf 的就是它)
	SubToken string `gorm:"uniqueIndex;size:64;not null" json:"sub_token"` // 订阅链接 Token

	// —— 套餐归属 ——
	// PlanID=0 表示「自定义」: 配额直接看下面的字段, 不从套餐继承。
	PlanID   uint   `gorm:"index;default:0" json:"plan_id"`
	PlanName string `gorm:"size:64;default:''" json:"plan_name"` // 开号时快照的套餐名 (套餐被删也能显示)

	TrafficLimit int64 `gorm:"default:0" json:"traffic_limit"` // 本周期流量配额 (Bytes, 0=不限)
	UsedTraffic  int64 `gorm:"default:0" json:"used_traffic"`  // 本周期已用 (Bytes)
	DeviceLimit  int   `gorm:"default:0" json:"device_limit"`  // 同时在线设备数 (0=不限)

	ExpireAt int64  `gorm:"default:0" json:"expire_at"`             // 到期时间戳 (秒, 0=永久)
	Status   string `gorm:"size:20;default:'active'" json:"status"` // active | suspended | expired
	Remark   string `gorm:"size:255" json:"remark"`

	// —— 周期重置 ——
	// ResetMode 在开号时从套餐快照过来, 之后独立演进 (用户可单独调)。
	ResetMode string `gorm:"size:16;default:'monthly'" json:"reset_mode"`
	// LastResetAt 上次重置时间。周期重置任务靠它判断「这一轮该不该重置」,
	// 只存时间戳不存「下次重置时间」—— 后者会因改套餐/改到期日而失效。
	LastResetAt int64 `gorm:"default:0" json:"last_reset_at"`

	// —— 真限流状态 ——
	// Locked 为真表示 agent 已把该用户的 UUID 从各节点 conf 摘除 (配额耗尽/到期/封停)。
	// 它和 Status 是两回事: Status 是管理员意图, Locked 是机器上的实际状态。
	// 两者不一致时以 Locked 为准 —— 因为那才是客户端能不能连的事实。
	Locked   bool  `gorm:"default:false" json:"locked"`
	LockedAt int64 `gorm:"default:0" json:"locked_at"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

// TrafficSample 流量采样点。
//
// 233boy 只给**累计值** (计数器), 重启会归零; 曲线图需要历史, 因此面板定时把
// 各节点当前累计值落一行, 由 api_traffic.go 做差分与归零钳制。
type TrafficSample struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	NodeID    uint      `gorm:"index:idx_sample_node_time" json:"node_id"`
	ConfName  string    `gorm:"size:191;index" json:"conf_name"`
	Up        int64     `json:"up"`
	Down      int64     `json:"down"`
	SampledAt time.Time `gorm:"index:idx_sample_node_time" json:"sampled_at"`
}

// UserTrafficSample 按用户的流量采样点。
//
// 与 TrafficSample 并存而不是合并: 前者是**机器/实例**视角 (节点排行、容量规划用),
// 后者是**账号**视角 (用量曲线、超额预警用)。合并的话两个维度的索引都会退化,
// 而这张表按天清理, 体积可控。
type UserTrafficSample struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	UserID    uint      `gorm:"index:idx_usample_user_time" json:"user_id"`
	NodeID    uint      `gorm:"index" json:"node_id"`
	ConfName  string    `gorm:"size:191;index" json:"conf_name"`
	Up        int64     `json:"up"`
	Down      int64     `json:"down"`
	SampledAt time.Time `gorm:"index:idx_usample_user_time" json:"sampled_at"`
}

// Announcement 公告。展示在用户订阅门户顶部。
type Announcement struct {
	ID        uint           `gorm:"primaryKey" json:"id"`
	Title     string         `gorm:"size:191;not null" json:"title"`
	Body      string         `gorm:"type:text" json:"body"`
	Level     string         `gorm:"size:16;default:'info'" json:"level"` // info | warn | danger
	Pinned    bool           `gorm:"default:false" json:"pinned"`         // 置顶 (只展示置顶时用)
	Enabled   bool           `gorm:"default:true" json:"enabled"`
	StartsAt  int64          `gorm:"default:0" json:"starts_at"` // 0=立即
	EndsAt    int64          `gorm:"default:0" json:"ends_at"`   // 0=永久
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

// Order 订单。
//
// 本面板**不接支付网关** (个人自用没有收款需求), 订单只用来记录
// 「谁在什么时候开了/续了什么套餐、到期日推到哪」。
// 这样即使不收款, 也能回答「这个号是谁什么时候给的、还剩多久」——
// 纯手工开号时这个问题没人答得上来。
//
// 字段留了 AmountCents / PaidAt, 将来接支付不用改表。
type Order struct {
	ID       uint   `gorm:"primaryKey" json:"id"`
	UserID   uint   `gorm:"index" json:"user_id"`
	Username string `gorm:"size:64" json:"username"` // 冗余快照: 用户被删后订单仍可读
	PlanID   uint   `json:"plan_id"`
	PlanName string `gorm:"size:64" json:"plan_name"`

	// Type  new | renew | reset | plan_change
	Type string `gorm:"size:20;default:'new'" json:"type"`
	// Status pending | paid | cancelled  (手工开号默认直接 paid)
	Status string `gorm:"size:20;default:'paid'" json:"status"`

	AmountCents int   `gorm:"default:0" json:"amount_cents"`
	PaidAt      int64 `gorm:"default:0" json:"paid_at"`

	// 本次订单造成的变更, 便于回溯
	PeriodStart int64 `gorm:"default:0" json:"period_start"` // 本周期起点
	PeriodEnd   int64 `gorm:"default:0" json:"period_end"`   // 到期时间
	TrafficAdd  int64 `gorm:"default:0" json:"traffic_add"`  // 本次加的流量额度

	Remark    string         `gorm:"size:255" json:"remark"`
	CreatedAt time.Time      `json:"created_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

// SystemSetting 系统全局设置。
type SystemSetting struct {
	Key       string    `gorm:"primaryKey;size:64" json:"key"`
	Value     string    `gorm:"type:text" json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
}
