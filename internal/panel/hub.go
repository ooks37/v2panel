package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"v2panel/internal/common"

	"github.com/gorilla/websocket"
)

// 哨兵错误: 让上层 API 能把 hub 的失败翻译成合适的 HTTP 状态码
// (离线 -> 503, 超时 -> 504), 而不是一律 500。
// 必须用 %w 包装后返回, errors.Is 才能穿透。
var (
	ErrNodeOffline = errors.New("节点离线")
	ErrOpTimeout   = errors.New("节点操作超时")
)

// DefaultOpTimeout 单次节点操作 (建/删/探测/取日志) 的默认等待上限。
// create 要串行跑 gen + add + 校验 conf 出现, 82 上实测单步 300~1500ms, 45s 给足余量。
const DefaultOpTimeout = 45 * time.Second

// heartbeatDBThrottle 心跳落库节流窗口。
// 心跳本身是秒级高频, 但 SQLite 只有一个写句柄; 不节流会让写锁被心跳占满,
// 导致面板 API 的写操作 (改配置/记流量) 全部排队等锁。
const heartbeatDBThrottle = 15 * time.Second

var upgrader = websocket.Upgrader{
	// Agent 是反向拨号 (常经 NAT / 反代), Origin 无意义且通常缺失, 故不做同源校验。
	// 真正的鉴权在首帧 auth_req 的 token 上, 未认证连接既不能读清单也不能下发操作。
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
}

// AgentConn 一台被纳管机器上的 agent 长连。
//
// ⚠️ 与 work-v 的差异: 这里**不 close(SendChan)**, 改用 done 通道广播关闭。
// work-v 的 unregister 里 close(SendChan) 之后 readPump 的 defer 又调 a.Close(),
// 而 Close 也会 close 同一个 channel —— 干净断开时必然 "close of closed channel" panic。
// done 通道没有这个语义陷阱: SafeSend 同时 select done, 关闭后立刻返回 false。
type AgentConn struct {
	NodeID    uint
	NodeToken string
	Conn      *websocket.Conn
	SendChan  chan *common.WSMessage
	LastSeen  time.Time

	lastDBUpdate time.Time
	done         chan struct{}
	closeOnce    sync.Once
}

// Close 幂等关闭: 广播 done 并断开底层连接。可被任意 goroutine 重复调用。
func (a *AgentConn) Close() {
	a.closeOnce.Do(func() {
		close(a.done)
		if a.Conn != nil {
			_ = a.Conn.Close()
		}
	})
}

// Closed 连接是否已关闭。
func (a *AgentConn) Closed() bool {
	select {
	case <-a.done:
		return true
	default:
		return false
	}
}

// SafeSend 线程安全下发, 绝不 panic (见 AgentConn 顶部注释)。
func (a *AgentConn) SafeSend(msg *common.WSMessage, timeout time.Duration) bool {
	if a.Closed() {
		return false
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case a.SendChan <- msg:
		return true
	case <-a.done:
		return false
	case <-timer.C:
		return false
	}
}

// ---------------------------------------------------------------------------
// 回调类型 (由 cmd/panel 注入, hub 自身不依赖 service 层, 避免反向依赖)
// ---------------------------------------------------------------------------

// TrafficCallback 处理 agent 上报的 per-inbound 增量流量。
type TrafficCallback func(nodeID uint, deltas []common.InboundDeltaTraffic) error

// UserTrafficCallback 处理 agent 上报的 **per-user** 增量流量 (真限流的数据来源)。
type UserTrafficCallback func(nodeID uint, rep *common.UserTrafficPayload) error

// InventoryCallback 处理整机节点清单 (落 Inbound 表)。
type InventoryCallback func(nodeID uint, rep *common.InventoryReportPayload) error

// CapabilityCallback 处理能力上报 (协议表 / 内核版本 / reality 可用性)。
type CapabilityCallback func(nodeID uint, rep *common.CapabilityReportPayload) error

// opWaiter 一次在途 node_op 的等待者。
type opWaiter struct {
	nodeID uint
	ch     chan *common.NodeOpResultPayload
}

// Hub 全局节点连接池。
type Hub struct {
	mu       sync.RWMutex
	agents   map[uint]*AgentConn // NodeID -> AgentConn
	tokenMap map[string]uint     // NodeToken -> NodeID

	onTraffic     TrafficCallback
	onUserTraffic UserTrafficCallback
	onInventory   InventoryCallback
	onCapability  CapabilityCallback

	waiterMu       sync.Mutex
	opWaiters      map[string]*opWaiter // OpID -> 等待者
	logWaiters     map[uint]chan *common.GetLogsRespPayload
	restartWaiters map[uint]chan *common.RestartRespPayload
}

// GlobalHub 进程级单例 (与 work-v 风格一致, 便于 handler 直接引用)。
var GlobalHub = NewHub()

// NewHub 构造空的连接池。
func NewHub() *Hub {
	return &Hub{
		agents:         make(map[uint]*AgentConn),
		tokenMap:       make(map[string]uint),
		opWaiters:      make(map[string]*opWaiter),
		logWaiters:     make(map[uint]chan *common.GetLogsRespPayload),
		restartWaiters: make(map[uint]chan *common.RestartRespPayload),
	}
}

// SetCallbacks 注入上报处理回调。
//
// userTrafficCb 允许为 nil —— 老 agent (不带 per-user 采集) 连上来时不会有
// 这类报文, 回调也就永远不会被调用。这样面板能兼容旧 agent 而无需版本判断。
func (h *Hub) SetCallbacks(
	trafficCb TrafficCallback,
	inventoryCb InventoryCallback,
	capabilityCb CapabilityCallback,
	userTrafficCb UserTrafficCallback,
) {
	h.onTraffic = trafficCb
	h.onInventory = inventoryCb
	h.onCapability = capabilityCb
	h.onUserTraffic = userTrafficCb
}

// ---------------------------------------------------------------------------
// 接入
// ---------------------------------------------------------------------------

// HandleWebSocket 处理 agent 发起的反向 WS 升级 (路由 /ws/agent)。
func (h *Hub) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[Hub] WebSocket 升级失败: %v", err)
		return
	}

	// 未认证连接同样要限帧 + 限时: 否则任何人都能推超大首帧打爆内存,
	// 或占着连接不发首帧把 fd 耗光。
	conn.SetReadLimit(64 * 1024)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var authMsg common.WSMessage
	if err := conn.ReadJSON(&authMsg); err != nil {
		log.Printf("[Hub] 读取节点握手认证超时或失败: %v", err)
		_ = conn.Close()
		return
	}
	if authMsg.Type != common.MsgTypeAuthReq || authMsg.NodeToken == "" {
		log.Printf("[Hub] 节点首包非有效认证: type=%s", authMsg.Type)
		_ = conn.Close()
		return
	}

	var node Node
	if err := DB.Where("token = ?", authMsg.NodeToken).First(&node).Error; err != nil {
		log.Printf("[Hub] 节点 Token 验证未通过: %s", maskToken(authMsg.NodeToken))
		writeAuthResp(conn, &common.AuthRespPayload{Success: false, Message: "token 无效或节点不存在"})
		_ = conn.Close()
		return
	}

	// 解析 agent 自报信息 (旧版本可能不带 payload, 容忍失败)。
	var req common.AuthReqPayload
	_ = json.Unmarshal(authMsg.Payload, &req)

	_ = conn.SetReadDeadline(time.Time{})
	if err := writeAuthResp(conn, &common.AuthRespPayload{
		Success:    true,
		NodeID:     node.ID,
		NodeName:   node.Name,
		Message:    "节点接入成功",
		ServerTime: time.Now().UnixMilli(),
	}); err != nil {
		_ = conn.Close()
		return
	}

	agent := &AgentConn{
		NodeID:    node.ID,
		NodeToken: node.Token,
		Conn:      conn,
		SendChan:  make(chan *common.WSMessage, 64),
		LastSeen:  time.Now(),
		done:      make(chan struct{}),
	}
	h.register(agent)

	now := time.Now()
	updates := map[string]any{
		"status":       "online",
		"last_seen_at": &now,
	}
	if req.Version != "" {
		updates["agent_ver"] = req.Version
	}

	// 回填真实对端 IP: 仅当连接来自本机反向代理时才信任转发头, 防止直连伪造。
	if ip := peerIP(r); ip != "" && !isLoopbackIP(ip) {
		if node.Address == "" || node.Address == "127.0.0.1" || strings.Contains(node.Address, "自动探测") {
			updates["address"] = ip
		}
	}
	DB.Model(&Node{}).Where("id = ?", node.ID).Updates(updates)

	log.Printf("[Hub] 节点已上线: ID=%d, Name=%s, Token=%s, AgentVer=%s", node.ID, node.Name, maskToken(node.Token), req.Version)

	go agent.writePump()

	// 接入即要一次清单, 让面板立刻知道这台机器上有什么 conf (而不是等下一个采集周期)。
	go func(id uint) {
		if err := h.RequestInventory(id); err != nil {
			log.Printf("[Hub] 节点 %d 首次清单请求失败: %v", id, err)
		}
	}(node.ID)

	// 接入即重新下发一次用户名单。
	//
	// 为什么必须在"每次接入"做, 而不是只在管理员改用户时做:
	//   - 机器重启 / agent 重启 / conf 被人手工还原 → clients[] 里的订阅用户没了,
	//     而面板这边毫无察觉 (用户不掉线才怪, 且没有任何地方报错);
	//   - 新纳管的机器第一次上线, 存量用户必须立刻铺上去, 否则要管理员手动点同步。
	//
	// 幂等且便宜: SyncUsers 只在真有改动时才写盘 + 重启内核, 名单一致时直接返回。
	// 用防抖通道走, 避免 agent 反复重连时把内核重启风暴打出来。
	go PushUserSyncAll()

	agent.readPump(h)
}

// writeAuthResp 回写认证结果。
func writeAuthResp(conn *websocket.Conn, resp *common.AuthRespPayload) error {
	msg, err := common.NewWSMessage(common.MsgTypeAuthResp, resp)
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return conn.WriteJSON(msg)
}

// peerIP 提取对端真实 IP。
func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !isLoopbackIP(host) {
		return host
	}
	// 来自本机 => 可能是宝塔 nginx 反代, 此时转发头可信。
	if xfwd := r.Header.Get("X-Forwarded-For"); xfwd != "" {
		if idx := strings.Index(xfwd, ","); idx != -1 {
			xfwd = xfwd[:idx]
		}
		if v := strings.TrimSpace(xfwd); v != "" {
			return v
		}
	}
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return v
	}
	return host
}

func isLoopbackIP(host string) bool {
	if host == "127.0.0.1" || host == "::1" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (h *Hub) register(agent *AgentConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// 同一节点重连: 优雅关掉旧连接 (done 广播会让旧 readPump/writePump 自行退出)
	if old, exists := h.agents[agent.NodeID]; exists {
		old.Close()
	}
	h.agents[agent.NodeID] = agent
	h.tokenMap[agent.NodeToken] = agent.NodeID
}

func (h *Hub) unregister(agent *AgentConn) {
	removed := false
	h.mu.Lock()
	if cur, exists := h.agents[agent.NodeID]; exists && cur == agent {
		delete(h.agents, agent.NodeID)
		delete(h.tokenMap, agent.NodeToken)
		removed = true
	}
	h.mu.Unlock()

	if !removed {
		// 已被 KickNode / 新连接顶掉: 状态由后者接管, 不在这里写 offline。
		agent.Close()
		return
	}
	agent.Close()

	// 让在途操作立刻失败, 而不是各自干等到超时。
	h.failPendingOps(agent.NodeID, fmt.Errorf("节点 %d 已断线", agent.NodeID))

	// DB 写必须在释放 h.mu 之后: SQLite 慢查询/忙等待时持锁写库会把
	// register / IsNodeOnline / SafeSend 全部挡死。
	DB.Model(&Node{}).Where("id = ?", agent.NodeID).Update("status", "offline")
	log.Printf("[Hub] 节点已下线: ID=%d", agent.NodeID)
}

// KickNode 主动断开指定节点 (删除节点 / 强制下线)。
func (h *Hub) KickNode(nodeID uint) {
	h.mu.Lock()
	agent, ok := h.agents[nodeID]
	if ok {
		delete(h.agents, nodeID)
		delete(h.tokenMap, agent.NodeToken)
	}
	h.mu.Unlock()

	if !ok {
		return
	}
	agent.Close()
	h.failPendingOps(nodeID, fmt.Errorf("节点 %d 已被面板主动断开", nodeID))
	log.Printf("[Hub] 已主动断开节点: ID=%d", nodeID)
}

// IsNodeOnline 节点是否在线。
func (h *Hub) IsNodeOnline(nodeID uint) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.agents[nodeID]
	return ok
}

// OnlineNodes 返回当前在线节点 ID 列表。
func (h *Hub) OnlineNodes() []uint {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]uint, 0, len(h.agents))
	for id := range h.agents {
		out = append(out, id)
	}
	return out
}

// SyncOfflineNodes 把库里标记为 online 但实际无长连的节点纠正为 offline。
// 面板重启后内存连接池是空的, 必须靠这个把残留的 "online" 清掉。
func (h *Hub) SyncOfflineNodes() {
	h.mu.RLock()
	onlineMap := make(map[uint]bool, len(h.agents))
	for id := range h.agents {
		onlineMap[id] = true
	}
	h.mu.RUnlock()

	var nodes []Node
	DB.Where("status = ?", "online").Find(&nodes)
	for _, n := range nodes {
		if !onlineMap[n.ID] {
			DB.Model(&Node{}).Where("id = ?", n.ID).Update("status", "offline")
			log.Printf("[Hub] 节点 %d 无活跃长连, 状态纠正为 offline", n.ID)
		}
	}
}

// ---------------------------------------------------------------------------
// 下发: 操作 (imperative)
// ---------------------------------------------------------------------------

// SendNodeOp 向节点下达一次操作并同步等待结果。
//
// OpID 是本层生成的关联键: agent 必须原样回填, 否则无法配对 (见 common.NodeOpPayload)。
func (h *Hub) SendNodeOp(nodeID uint, p *common.NodeOpPayload, timeout time.Duration) (*common.NodeOpResultPayload, error) {
	if p == nil {
		return nil, fmt.Errorf("操作载荷为空")
	}
	if p.Op == "" {
		return nil, fmt.Errorf("操作名为空")
	}
	if p.OpID == "" {
		p.OpID = NewToken()
	}
	if timeout <= 0 {
		timeout = DefaultOpTimeout
	}

	h.mu.RLock()
	agent, ok := h.agents[nodeID]
	h.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: 节点 %d 未在线", ErrNodeOffline, nodeID)
	}

	ch := make(chan *common.NodeOpResultPayload, 1)
	h.waiterMu.Lock()
	h.opWaiters[p.OpID] = &opWaiter{nodeID: nodeID, ch: ch}
	h.waiterMu.Unlock()
	defer func() {
		h.waiterMu.Lock()
		delete(h.opWaiters, p.OpID)
		h.waiterMu.Unlock()
	}()

	msg, err := common.NewWSMessage(common.MsgTypeNodeOp, p)
	if err != nil {
		return nil, err
	}
	if !agent.SafeSend(msg, 3*time.Second) {
		return nil, fmt.Errorf("下发操作失败 (agent 发送队列拥塞)")
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		return res, nil
	case <-timer.C:
		return nil, fmt.Errorf("%w: 等待节点响应超时 (%s)", ErrOpTimeout, timeout)
	case <-agent.done:
		return nil, fmt.Errorf("%w: 节点在操作执行期间断线", ErrOpTimeout)
	}
}

// failPendingOps 让某节点所有在途操作立刻失败。
func (h *Hub) failPendingOps(nodeID uint, err error) {
	h.waiterMu.Lock()
	defer h.waiterMu.Unlock()
	for opID, w := range h.opWaiters {
		if w.nodeID != nodeID {
			continue
		}
		select {
		case w.ch <- &common.NodeOpResultPayload{OpID: opID, OK: false, Error: err.Error()}:
		default:
		}
	}
}

// RequestInventory 请求节点立刻重采集清单。
func (h *Hub) RequestInventory(nodeID uint) error {
	h.mu.RLock()
	agent, ok := h.agents[nodeID]
	h.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: 节点 %d 未在线", ErrNodeOffline, nodeID)
	}
	msg, err := common.NewWSMessage(common.MsgTypeInventoryReq, struct{}{})
	if err != nil {
		return err
	}
	if !agent.SafeSend(msg, 2*time.Second) {
		return fmt.Errorf("下发清单采集请求失败")
	}
	return nil
}

// BroadcastInventoryReq 向所有在线节点请求清单 (面板启动后调用)。
func (h *Hub) BroadcastInventoryReq() {
	h.mu.RLock()
	targets := make([]*AgentConn, 0, len(h.agents))
	for _, a := range h.agents {
		targets = append(targets, a)
	}
	h.mu.RUnlock()

	msg, err := common.NewWSMessage(common.MsgTypeInventoryReq, struct{}{})
	if err != nil {
		return
	}
	for _, a := range targets {
		a.SafeSend(msg, time.Second)
	}
}

// RequestNodeLogs 取节点上的运行日志 (source: v2ray / caddy / agent)。
func (h *Hub) RequestNodeLogs(nodeID uint, source string, lines int) (*common.GetLogsRespPayload, error) {
	h.mu.RLock()
	agent, ok := h.agents[nodeID]
	h.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: 节点 %d 未在线", ErrNodeOffline, nodeID)
	}
	if lines <= 0 {
		lines = 200
	}

	ch := make(chan *common.GetLogsRespPayload, 1)
	h.waiterMu.Lock()
	h.logWaiters[nodeID] = ch
	h.waiterMu.Unlock()
	defer func() {
		h.waiterMu.Lock()
		delete(h.logWaiters, nodeID)
		h.waiterMu.Unlock()
	}()

	msg, err := common.NewWSMessage(common.MsgTypeGetLogsReq, &common.GetLogsReqPayload{Source: source, Lines: lines})
	if err != nil {
		return nil, err
	}
	if !agent.SafeSend(msg, 2*time.Second) {
		return nil, fmt.Errorf("向节点发送日志请求超时")
	}

	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case resp := <-ch:
		return resp, nil
	case <-timer.C:
		return nil, fmt.Errorf("%w: 等待节点日志响应超时", ErrOpTimeout)
	case <-agent.done:
		return nil, fmt.Errorf("节点在取日志期间断线")
	}
}

// RestartNode 重启节点上的 v2ray (TLS 节点连带 caddy)。
func (h *Hub) RestartNode(nodeID uint) (*common.RestartRespPayload, error) {
	h.mu.RLock()
	agent, ok := h.agents[nodeID]
	h.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: 节点 %d 未在线", ErrNodeOffline, nodeID)
	}

	ch := make(chan *common.RestartRespPayload, 1)
	h.waiterMu.Lock()
	h.restartWaiters[nodeID] = ch
	h.waiterMu.Unlock()
	defer func() {
		h.waiterMu.Lock()
		delete(h.restartWaiters, nodeID)
		h.waiterMu.Unlock()
	}()

	msg, err := common.NewWSMessage(common.MsgTypeRestartReq, struct{}{})
	if err != nil {
		return nil, err
	}
	if !agent.SafeSend(msg, 2*time.Second) {
		return nil, fmt.Errorf("向节点发送重启指令超时")
	}

	// systemctl restart v2ray + caddy 在低配机上要几秒, 给 30s。
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case resp := <-ch:
		return resp, nil
	case <-timer.C:
		return nil, fmt.Errorf("%w: 等待节点重启响应超时", ErrOpTimeout)
	case <-agent.done:
		return nil, fmt.Errorf("节点在重启期间断线")
	}
}

// maskToken 日志脱敏, 避免节点凭证进日志文件。
func maskToken(token string) string {
	if len(token) <= 6 {
		return "***"
	}
	return token[:6] + "***"
}

// ---------------------------------------------------------------------------
// 收报
// ---------------------------------------------------------------------------

// readPump 收 agent 上报。
func (a *AgentConn) readPump(h *Hub) {
	defer func() {
		h.unregister(a)
		a.Close()
	}()

	a.Conn.SetReadLimit(4 * 1024 * 1024)
	_ = a.Conn.SetReadDeadline(time.Now().Add(75 * time.Second))

	for {
		var msg common.WSMessage
		if err := a.Conn.ReadJSON(&msg); err != nil {
			break
		}
		_ = a.Conn.SetReadDeadline(time.Now().Add(75 * time.Second))
		a.LastSeen = time.Now()

		switch msg.Type {
		case common.MsgTypeHeartbeat:
			h.handleHeartbeat(a, msg)
		case common.MsgTypeTraffic:
			h.handleTraffic(a, msg)
		case common.MsgTypeUserTraffic:
			h.handleUserTraffic(a, msg)
		case common.MsgTypeInventoryReport:
			h.handleInventory(a, msg)
		case common.MsgTypeCapabilityReport:
			h.handleCapability(a, msg)
		case common.MsgTypeNodeOpResp:
			h.handleNodeOpResp(msg)
		case common.MsgTypeGetLogsResp:
			h.handleLogsResp(a, msg)
		case common.MsgTypeRestartResp:
			h.handleRestartResp(a, msg)
		default:
			log.Printf("[Hub] 节点 %d 发来未识别消息类型: %s", a.NodeID, msg.Type)
		}
	}
}

func (h *Hub) handleHeartbeat(a *AgentConn, msg common.WSMessage) {
	var stats common.NodeHardwareStats
	if err := json.Unmarshal(msg.Payload, &stats); err != nil {
		log.Printf("[Hub] 节点 %d 心跳解析失败: %v", a.NodeID, err)
		return
	}
	now := time.Now()
	if now.Sub(a.lastDBUpdate) < heartbeatDBThrottle {
		return
	}
	a.lastDBUpdate = now

	// 用 map 而非 struct: struct 更新会跳过零值, 而 "v2ray_pid=0 / caddy_status=''"
	// 恰恰是「进程挂了」的有效事实, 必须写进去。
	DB.Model(&Node{}).Where("id = ?", a.NodeID).Updates(map[string]any{
		"status":       "online",
		"last_seen_at": &now,
		"cpu_percent":  stats.CPUPercent,
		"mem_used":     stats.MemUsedBytes,
		"mem_total":    stats.MemTotalBytes,
		"net_rx_rate":  stats.NetRxRate,
		"net_tx_rate":  stats.NetTxRate,
		"uptime_sec":   stats.UptimeSec,
		"agent_ver":    stats.Version,
		"v2ray_status": stats.V2rayStatus,
		"v2ray_pid":    stats.V2rayPid,
		"v2ray_ver":    stats.V2rayVer,
		"caddy_status": stats.CaddyStatus,
		"caddy_ver":    stats.CaddyVer,
	})
}

func (h *Hub) handleTraffic(a *AgentConn, msg common.WSMessage) {
	var rep common.TrafficReportPayload
	if err := json.Unmarshal(msg.Payload, &rep); err != nil {
		log.Printf("[Hub] 节点 %d 流量报文解析失败: %v", a.NodeID, err)
		return
	}
	if len(rep.Inbounds) == 0 {
		return
	}
	if h.onTraffic == nil {
		return
	}
	if err := h.onTraffic(a.NodeID, rep.Inbounds); err != nil {
		log.Printf("[Hub] 节点 %d 流量入账失败: %v", a.NodeID, err)
	}
}

// handleUserTraffic 处理 per-user 增量。
//
// 与 handleTraffic 一样只做解包+转发, 业务逻辑在回调里 —— hub 不依赖 service 层。
func (h *Hub) handleUserTraffic(a *AgentConn, msg common.WSMessage) {
	var rep common.UserTrafficPayload
	if err := json.Unmarshal(msg.Payload, &rep); err != nil {
		log.Printf("[Hub] 节点 %d 用户流量报文解析失败: %v", a.NodeID, err)
		return
	}
	if len(rep.Entries) == 0 {
		return
	}
	if h.onUserTraffic == nil {
		return
	}
	if err := h.onUserTraffic(a.NodeID, &rep); err != nil {
		log.Printf("[Hub] 节点 %d 用户流量入账失败: %v", a.NodeID, err)
	}
}

func (h *Hub) handleInventory(a *AgentConn, msg common.WSMessage) {
	var rep common.InventoryReportPayload
	if err := json.Unmarshal(msg.Payload, &rep); err != nil {
		log.Printf("[Hub] 节点 %d 清单解析失败: %v", a.NodeID, err)
		return
	}
	log.Printf("[Hub] 节点 %d 清单上报: %d 个 conf, http=%d https=%d, complete=%v, 耗时=%dms",
		a.NodeID, len(rep.Nodes), rep.HTTPPort, rep.HTTPSPort, rep.Complete, rep.DurationMs)
	for _, w := range rep.Warnings {
		log.Printf("[Hub] 节点 %d 采集告警: %s", a.NodeID, w)
	}
	if h.onInventory == nil {
		return
	}
	if err := h.onInventory(a.NodeID, &rep); err != nil {
		log.Printf("[Hub] 节点 %d 清单入库失败: %v", a.NodeID, err)
	}
}

func (h *Hub) handleCapability(a *AgentConn, msg common.WSMessage) {
	var rep common.CapabilityReportPayload
	if err := json.Unmarshal(msg.Payload, &rep); err != nil {
		log.Printf("[Hub] 节点 %d 能力报文解析失败: %v", a.NodeID, err)
		return
	}
	if h.onCapability == nil {
		return
	}
	if err := h.onCapability(a.NodeID, &rep); err != nil {
		log.Printf("[Hub] 节点 %d 能力入库失败: %v", a.NodeID, err)
	}
}

func (h *Hub) handleNodeOpResp(msg common.WSMessage) {
	var res common.NodeOpResultPayload
	if err := json.Unmarshal(msg.Payload, &res); err != nil {
		log.Printf("[Hub] node_op_resp 解析失败: %v", err)
		return
	}
	if res.OpID == "" {
		log.Printf("[Hub] node_op_resp 缺少 op_id, 无法配对 (op=%s)", res.Op)
		return
	}

	h.waiterMu.Lock()
	w, ok := h.opWaiters[res.OpID]
	h.waiterMu.Unlock()
	if !ok {
		// 已超时/已断线, 结果作废 —— 只记日志。
		log.Printf("[Hub] 收到无人等待的操作结果 op_id=%s op=%s ok=%v (可能已超时)", res.OpID, res.Op, res.OK)
		return
	}
	select {
	case w.ch <- &res:
	default:
		log.Printf("[Hub] 操作结果通道已满, 丢弃 op_id=%s", res.OpID)
	}
}

func (h *Hub) handleLogsResp(a *AgentConn, msg common.WSMessage) {
	var resp common.GetLogsRespPayload
	if err := json.Unmarshal(msg.Payload, &resp); err != nil {
		return
	}
	h.waiterMu.Lock()
	ch, ok := h.logWaiters[a.NodeID]
	h.waiterMu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- &resp:
	default:
	}
}

func (h *Hub) handleRestartResp(a *AgentConn, msg common.WSMessage) {
	var resp common.RestartRespPayload
	if err := json.Unmarshal(msg.Payload, &resp); err != nil {
		return
	}
	h.waiterMu.Lock()
	ch, ok := h.restartWaiters[a.NodeID]
	h.waiterMu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- &resp:
	default:
	}
}

// writePump 串行下发 + 20s 心跳 ping。
// websocket 连接不允许并发写, 所有下发都必须经这一个 goroutine。
func (a *AgentConn) writePump() {
	ticker := time.NewTicker(20 * time.Second)
	defer func() {
		ticker.Stop()
		a.Close()
	}()

	for {
		select {
		case <-a.done:
			_ = a.Conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
			_ = a.Conn.WriteMessage(websocket.CloseMessage, []byte{})
			return
		case msg := <-a.SendChan:
			_ = a.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := a.Conn.WriteJSON(msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = a.Conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := a.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
