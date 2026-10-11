package agent

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"v2panel/internal/common"
)

// ===========================================================================
// 反向 WebSocket 客户端 —— agent 主动拨面板, 而不是面板连 agent。
//
// 为什么反向: 被纳管的机器大多在 NAT 后面, 面板拨不进去; 反向长连让"面板
// 能不能管到"只取决于目标机能否出网, 与它有没有公网入口无关。
//
// ⚠️ 写并发: websocket 连接不允许并发写。所有下行/上报都必须经过 writeMu
// (或者只在单 goroutine 里写)。本文件把发送统一收口到 send()。
// ===========================================================================

const (
	// 心跳/流量上报周期。面板侧 handleHeartbeat 有节流, 不会每 5s 写一次库。
	heartbeatInterval = 5 * time.Second
	trafficInterval   = 5 * time.Second
	// 重连退避: 面板重启/网络抖动时不要打满 CPU。
	reconnectDelay = 5 * time.Second
	// 读超时。面板的 writePump 每 20s 发一个 ping, 这里给 3 倍余量;
	// 超时即认为链路已死, 断开重连 (否则会挂在一个半开的 TCP 上永远收不到指令)。
	readTimeout  = 75 * time.Second
	writeTimeout = 10 * time.Second
	// 单帧上限。清单上报在节点多时可能上百 KB。
	maxFrameBytes = 4 * 1024 * 1024
)

// Client 反向长连客户端。
type Client struct {
	Server   string // 面板地址, http(s)://host[:port]
	Token    string
	Insecure bool // 跳过 TLS 校验 (面板用自签证书时)

	Driver    *Driver
	Collector *Collector
	Ops       *Ops

	mu   sync.Mutex
	conn *websocket.Conn
	// writeMu 串行化所有对 conn 的写。
	writeMu sync.Mutex

	done     chan struct{}
	doneOnce sync.Once
}

// NewClient 构造客户端。
func NewClient(server, token string, insecure bool) *Client {
	d := NewDriver()
	return &Client{
		Server:    strings.TrimRight(strings.TrimSpace(server), "/"),
		Token:     token,
		Insecure:  insecure,
		Driver:    d,
		Collector: NewCollector(),
		Ops:       NewOps(d),
		done:      make(chan struct{}),
	}
}

// wsURL 把 http(s):// 面板地址换成 ws(s)://.../ws/agent。
func (c *Client) wsURL() (string, error) {
	u, err := url.Parse(c.Server)
	if err != nil {
		return "", fmt.Errorf("面板地址无法解析: %w", err)
	}
	switch u.Scheme {
	case "http", "ws":
		u.Scheme = "ws"
	case "https", "wss":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("不支持的地址协议: %q (应为 http/https)", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/ws/agent"
	u.RawQuery = ""
	return u.String(), nil
}

// Start 进入"连接 → 工作 → 断开 → 重连"的常驻循环, 阻塞直到 Close。
func (c *Client) Start() {
	for {
		select {
		case <-c.done:
			return
		default:
		}
		if err := c.runOnce(); err != nil {
			log.Printf("[Agent] 连接断开: %v", err)
		}
		select {
		case <-c.done:
			return
		case <-time.After(reconnectDelay):
		}
	}
}

// Close 关闭客户端并断开当前连接。
func (c *Client) Close() {
	c.doneOnce.Do(func() { close(c.done) })
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// ---------------------------------------------------------------------------
// 单次连接的生命周期
// ---------------------------------------------------------------------------

func (c *Client) runOnce() error {
	wsURL, err := c.wsURL()
	if err != nil {
		return err
	}

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 15 * time.Second
	if c.Insecure {
		// #nosec G402 - 面板可能用自签证书, 由运维显式开 -insecure 才走到这里。
		dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	conn, resp, err := dialer.Dial(wsURL, http.Header{})
	if err != nil {
		if resp != nil {
			return fmt.Errorf("拨号 %s 失败 (HTTP %d): %w", wsURL, resp.StatusCode, err)
		}
		return fmt.Errorf("拨号 %s 失败: %w", wsURL, err)
	}
	defer conn.Close()

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
	}()

	conn.SetReadLimit(maxFrameBytes)

	// —— 首帧必须是 auth_req, 否则面板会直接关掉连接 ——
	if err := c.sendAuth(conn); err != nil {
		return err
	}
	conn.SetReadDeadline(time.Now().Add(readTimeout))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("等待认证响应失败: %w", err)
	}
	var authResp common.WSMessage
	if err := json.Unmarshal(raw, &authResp); err != nil {
		return fmt.Errorf("认证响应无法解析: %w", err)
	}
	if authResp.Type != common.MsgTypeAuthResp {
		return fmt.Errorf("认证响应类型异常: %q", authResp.Type)
	}
	var ar common.AuthRespPayload
	if err := json.Unmarshal(authResp.Payload, &ar); err != nil {
		return fmt.Errorf("认证响应载荷无法解析: %w", err)
	}
	if !ar.Success {
		// 令牌错是配置问题, 重连也不会好, 但保持循环 (运维改完配置重启即恢复)。
		return fmt.Errorf("面板拒绝接入: %s", ar.Message)
	}
	log.Printf("[Agent] 已接入面板 (节点 #%d %s), 版本 %s", ar.NodeID, ar.NodeName, common.AgentVersion)

	// 面板的 writePump 会周期性 ping; 收到 pong 就续命读超时。
	//
	// ⚠️ 光有 PongHandler **不够**。实测 (tcpdump 抓 127.0.0.1:9000 明文帧):
	//
	//	SRV→AGT len=2   (ping)
	//	AGT→SRV len=6   (pong)     ← 确实回了, 但下面这个 handler 不会因此被调用
	//
	// 因为 websocket 库只在**读循环里读到** pong 才回调 PongHandler, 而
	// readLoop 每次 ReadMessage 返回数据消息时会自己 SetReadDeadline(75s)。
	// 面板除了 3 分钟一次的 inventory_req 几乎不发数据 → 75 秒必然超时断线重连,
	// 线上表现为「每 80 秒掉一次线」, 每次掉线都让节点在面板上闪一下 offline。
	//
	// 真正的修复是让 agent **自己也发 ping** (见 keepaliveLoop), 这样读循环会
	// 收到 pong 并续命。PongHandler 保留作为第二道保险。
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(readTimeout))
	})

	// —— 接入后立刻上报一次能力 (含协议表与 reality 可用性) ——
	if err := c.sendCapability(conn); err != nil {
		return err
	}

	ctxDone := make(chan struct{})
	var wg sync.WaitGroup

	// 心跳
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.heartbeatLoop(conn, ctxDone)
	}()
	// 链路保活 (自己发 ping, 让读循环收到 pong 续命) —— 见 PongHandler 上的注释
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.keepaliveLoop(conn, ctxDone)
	}()
	// 流量增量
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.trafficLoop(conn, ctxDone)
	}()

	readErr := c.readLoop(conn)
	close(ctxDone)
	_ = conn.Close() // 打断仍在写的心跳
	wg.Wait()
	return readErr
}

func (c *Client) sendAuth(conn *websocket.Conn) error {
	host, _ := os.Hostname()
	msg, err := common.NewWSMessage(common.MsgTypeAuthReq, common.AuthReqPayload{
		Token:    c.Token,
		Version:  common.AgentVersion,
		Hostname: host,
	})
	if err != nil {
		return err
	}
	msg.NodeToken = c.Token // 面板首帧只读这个字段做 token → node 映射
	return c.writeJSON(conn, msg)
}

// readLoop 处理面板下发的一切指令, 直到连接断开。
func (c *Client) readLoop(conn *websocket.Conn) error {
	for {
		conn.SetReadDeadline(time.Now().Add(readTimeout))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			select {
			case <-c.done:
				return nil // 主动关闭, 不是错误
			default:
			}
			return err
		}
		var msg common.WSMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			log.Printf("[Agent] 收到无法解析的消息: %v", err)
			continue
		}
		switch msg.Type {
		case common.MsgTypeInventoryReq:
			c.handleInventory(conn)

		case common.MsgTypeNodeOp:
			c.handleNodeOp(conn, &msg)

		case common.MsgTypeGetLogsReq:
			c.handleGetLogs(conn, &msg)

		case common.MsgTypeRestartReq:
			c.handleRestart(conn)

		case common.MsgTypeHeartbeat:
			// 面板不该主动发心跳, 兼容处理即可。

		case common.MsgTypeCommand:
			// 233boy 世界没有"下发整份配置"的概念 (见 common/protocol.go 的说明),
			// 契约上保留 command 类型, 但目前没有语义 —— 记录以便排查。
			log.Printf("[Agent] 收到 command 消息 (当前无对应语义, 已忽略)")

		default:
			log.Printf("[Agent] 未识别的消息类型: %q", msg.Type)
		}
	}
}

// ---------------------------------------------------------------------------
// 指令处理
// ---------------------------------------------------------------------------

func (c *Client) handleInventory(conn *websocket.Conn) {
	rep, err := c.Driver.BuildInventory()
	if err != nil {
		// 采集彻底失败也必须回一帧, 否则面板会一直等 (它没有超时重试)。
		// Complete=false 是关键: 面板据此**不会**清理"消失的 conf"。
		rep = &common.InventoryReportPayload{
			Complete:    false,
			Warnings:    []string{fmt.Sprintf("agent 采集清单失败: %v", err)},
			CollectedAt: time.Now().UnixMilli(),
		}
		log.Printf("[Agent] 清单采集失败: %v", err)
	}
	msg, err := common.NewWSMessage(common.MsgTypeInventoryReport, rep)
	if err != nil {
		log.Printf("[Agent] 构造清单消息失败: %v", err)
		return
	}
	if err := c.writeJSON(conn, msg); err != nil {
		log.Printf("[Agent] 上报清单失败: %v", err)
		return
	}
	log.Printf("[Agent] 已上报清单: %d 个 conf, complete=%v, 耗时 %dms",
		len(rep.Nodes), rep.Complete, rep.DurationMs)
}

func (c *Client) handleNodeOp(conn *websocket.Conn, msg *common.WSMessage) {
	var p common.NodeOpPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		log.Printf("[Agent] node_op 载荷无法解析: %v", err)
		return
	}
	if p.OpID == "" {
		// 面板 hub 会用 op_id 配对; 没有它面板会丢弃响应。这里只能记日志。
		log.Printf("[Agent] node_op 缺少 op_id (op=%s), 面板将无法配对", p.Op)
		return
	}

	log.Printf("[Agent] 执行节点操作 op=%s op_id=%s", p.Op, p.OpID)
	res := c.Ops.Execute(&p)

	out, err := common.NewWSMessage(common.MsgTypeNodeOpResp, res)
	if err != nil {
		log.Printf("[Agent] 构造操作响应失败: %v", err)
		return
	}
	// ⚠️ op_id 原样回填 (Execute 里已从 p 拷进 res), 面板靠它配对。
	if err := c.writeJSON(conn, out); err != nil {
		log.Printf("[Agent] 上报操作结果失败: %v", err)
		return
	}
	log.Printf("[Agent] 操作完成 op=%s ok=%v 耗时=%dms", res.Op, res.OK, res.DurationMs)
}

func (c *Client) handleGetLogs(conn *websocket.Conn, msg *common.WSMessage) {
	var req common.GetLogsReqPayload
	_ = json.Unmarshal(msg.Payload, &req) // 解析失败就用零值 (=v2ray/200 行)

	resp := common.GetLogsRespPayload{Source: req.Source}
	if resp.Source == "" {
		resp.Source = LogSourceV2ray
	}
	content, err := c.Driver.ReadLogs(req.Source, req.Lines)
	resp.Content = content
	if err != nil {
		resp.Error = err.Error()
	}
	out, err := common.NewWSMessage(common.MsgTypeGetLogsResp, resp)
	if err != nil {
		return
	}
	if err := c.writeJSON(conn, out); err != nil {
		log.Printf("[Agent] 上报日志失败: %v", err)
	}
}

func (c *Client) handleRestart(conn *websocket.Conn) {
	steps, pid, err := c.Driver.RestartServices()
	resp := common.RestartRespPayload{Success: err == nil, V2rayPid: pid}
	if err != nil {
		resp.Message = err.Error()
	} else {
		resp.Message = fmt.Sprintf("重启完成 (v2ray pid=%d, %d 步)", pid, len(steps))
	}
	for _, s := range steps {
		if !s.OK {
			resp.Message += " | " + s.Name + ": " + s.Detail
		}
	}
	// 重启会清空 StatsService 计数器, 必须重置基线。
	if err == nil && c.Ops != nil && c.Ops.Traffic != nil {
		c.Ops.Traffic.Resync()
	}
	out, e := common.NewWSMessage(common.MsgTypeRestartResp, resp)
	if e != nil {
		return
	}
	if e := c.writeJSON(conn, out); e != nil {
		log.Printf("[Agent] 上报重启结果失败: %v", e)
	}
}

// ---------------------------------------------------------------------------
// 周期上报
// ---------------------------------------------------------------------------

func (c *Client) heartbeatLoop(conn *websocket.Conn, done <-chan struct{}) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-c.done:
			return
		case <-ticker.C:
			stats := c.Collector.Collect()
			msg, err := common.NewWSMessage(common.MsgTypeHeartbeat, stats)
			if err != nil {
				continue
			}
			if err := c.writeJSON(conn, msg); err != nil {
				log.Printf("[Agent] 上报心跳失败: %v", err)
				return
			}
		}
	}
}

func (c *Client) trafficLoop(conn *websocket.Conn, done <-chan struct{}) {
	ticker := time.NewTicker(trafficInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-c.done:
			return
		case <-ticker.C:
			// 两个维度各收一次。共用同一条 WS 连接, 但消息类型分开 ——
			// 面板侧的落库逻辑完全不同 (inbound 表 vs 用户累加), 合一条消息
			// 会让 hub 的分派里塞满类型判断。
			c.reportInboundTraffic(conn)
			c.reportUserTraffic(conn)
		}
	}
}

// reportInboundTraffic 上报 per-inbound 增量 (机器视角)。
func (c *Client) reportInboundTraffic(conn *websocket.Conn) {
	deltas := c.Ops.Traffic.Collect()
	if len(deltas) == 0 {
		return // 没有变化就不发, 省得面板白写库
	}
	msg, err := common.NewWSMessage(common.MsgTypeTraffic,
		common.TrafficReportPayload{Inbounds: deltas})
	if err != nil {
		return
	}
	if err := c.writeJSON(conn, msg); err != nil {
		log.Printf("[Agent] 上报流量失败: %v", err)
	}
}

// reportUserTraffic 上报 per-user 增量 (账号视角, 真限流的数据来源)。
//
// 计数器来自 V2Ray 的 `user>>>EMAIL>>>traffic>>>...`, 只有在
// (a) policy.levels.0.statsUserUplink/Downlink 为 true 且
// (b) conf 的 clients[] 那一项带 email
// 时才存在。条件不满足时这里拿到的是空切片 —— **不是错误**, 只是还没同步过用户名单。
// 因此不上报、也不打日志 (否则日志会被这条刷满)。
func (c *Client) reportUserTraffic(conn *websocket.Conn) {
	if c.Ops.UserTraffic == nil {
		return
	}
	deltas := c.Ops.UserTraffic.Collect()
	if len(deltas) == 0 {
		return
	}

	entries := make([]common.UserTrafficEntry, 0, len(deltas))
	for _, d := range deltas {
		uid := parseUserIDFromEmail(d.Email)
		if uid == 0 {
			continue // 不是面板管理的 email (手工加的 client), 跳过
		}
		entries = append(entries, common.UserTrafficEntry{
			UserID: uid, Email: d.Email, Up: d.Up, Down: d.Down,
		})
	}
	if len(entries) == 0 {
		return
	}

	msg, err := common.NewWSMessage(common.MsgTypeUserTraffic, common.UserTrafficPayload{
		CollectedAt: time.Now().UnixMilli(),
		Entries:     entries,
	})
	if err != nil {
		return
	}
	if err := c.writeJSON(conn, msg); err != nil {
		log.Printf("[Agent] 上报用户流量失败: %v", err)
	}
}

func (c *Client) sendCapability(conn *websocket.Conn) error {
	rep := c.Driver.BuildCapability(c.Collector)
	msg, err := common.NewWSMessage(common.MsgTypeCapabilityReport, rep)
	if err != nil {
		return err
	}
	return c.writeJSON(conn, msg)
}

// writeJSON 串行化写一帧。所有发送路径都必须走这里 (websocket 不支持并发写)。
func (c *Client) writeJSON(conn *websocket.Conn, v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return conn.WriteJSON(v)
}

// writePing 发一个 WebSocket ping 控制帧。
//
// 必须走 writeMu —— websocket 连接不允许并发写, 而 keepaliveLoop 与
// heartbeatLoop / trafficLoop 是三个独立 goroutine。
func (c *Client) writePing(conn *websocket.Conn) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return conn.WriteMessage(websocket.PingMessage, nil)
}

// keepaliveLoop 定期发 ping, 维持读超时。
//
// 为什么必须由 **agent** 主动发: 读超时只能被"读循环里读到的东西"续命 ——
// 数据消息由 readLoop 自己续, pong 由 PongHandler 续。面板几乎不发数据,
// 所以只有 agent 自己发 ping 才能换来 pong。详见 PongHandler 上的实测注释。
//
// 周期取 readTimeout/3: 给丢包和调度抖动留两次重试余量, 不会误判断线。
func (c *Client) keepaliveLoop(conn *websocket.Conn, done <-chan struct{}) {
	ticker := time.NewTicker(readTimeout / 3)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-c.done:
			return
		case <-ticker.C:
			if err := c.writePing(conn); err != nil {
				// 写失败说明链路已死, 让 readLoop 去收尾 (它会触发重连)。
				return
			}
		}
	}
}
