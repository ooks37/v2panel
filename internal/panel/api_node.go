package panel

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"v2panel/internal/common"

	"github.com/gin-gonic/gin"
)

// 各类 NodeOp 的超时预算。
//
// create 最慢: 脚本要下载/校验内核、写 conf、起 Caddy 并等 ACME 签发。
// 首次签证书在慢机器上可能 30s+, 给到 120s 才不至于把「正在成功」误判成超时。
// 但也不能无限等: 参数不全时 233boy 会 while read 自旋, agent 侧虽有自旋检测兜底,
// 面板侧再压一道硬上限, 避免前端请求永久挂起。
const (
	opTimeoutCreate    = 120 * time.Second
	opTimeoutDelete    = 45 * time.Second
	opTimeoutPreview   = 30 * time.Second
	opTimeoutInventory = 30 * time.Second
	opTimeoutProbe     = 40 * time.Second
)

// parseUintParam 取路径参数里的 uint。
func parseUintParam(c *gin.Context, name string) (uint, bool) {
	raw := c.Param(name)
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || n == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "无效的 ID: " + raw})
		return 0, false
	}
	return uint(n), true
}

// loadNode 按 ID 取节点, 失败时已写响应。
func loadNode(c *gin.Context) (*Node, bool) {
	id, ok := parseUintParam(c, "id")
	if !ok {
		return nil, false
	}
	var n Node
	if err := DB.First(&n, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "error": "NotFound", "message": "节点不存在"})
		return nil, false
	}
	return &n, true
}

// HandleListProtocols 返回面板可用的协议表。
// 由后端下发而不是前端硬编码 —— reality 在 v4.27 是死的, 只有后端知道,
// 前端照表渲染下拉框即可, 两份表不会走偏。
func HandleListProtocols(c *gin.Context) {
	defs := common.ProtocolDefs()
	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"protocols":       defs,
			"ss_methods":      common.SSMethods,
			"header_types":    common.HeaderTypes,
			"reality":         common.RealitySupported(),
			"reality_why":     "233boy v4.27 的 add() 里 reality 分支已被注释, 脚本自身不识别该协议",
			"listen_loopback": common.ListenLoopback,
			"listen_any":      common.ListenAny,
		},
	})
}

// ---------------------------------------------------------------------------
// 节点 CRUD
// ---------------------------------------------------------------------------

type nodeCreateRequest struct {
	Name      string  `json:"name" binding:"required"`
	Address   string  `json:"address"`
	Rate      float64 `json:"rate"`
	Remark    string  `json:"remark"`
	Protected string  `json:"protected"` // 逗号分隔的配置名子串, 见 NormalizeProtected
}

func HandleListNodes(c *gin.Context) {
	var nodes []Node
	DB.Order("id asc").Find(&nodes)

	// 在线状态以 hub 的实时连接池为准, 而不是数据库里可能过期的 status 字段。
	out := make([]gin.H, 0, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		online := GlobalHub.IsNodeOnline(n.ID)
		var inboundCnt int64
		DB.Model(&Inbound{}).Where("node_id = ?", n.ID).Count(&inboundCnt)
		out = append(out, gin.H{
			"node":        n,
			"online":      online,
			"inbound_cnt": inboundCnt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": out})
}

func HandleGetNode(c *gin.Context) {
	n, ok := loadNode(c)
	if !ok {
		return
	}
	var inbounds []Inbound
	DB.Where("node_id = ?", n.ID).Order("sort_idx asc, id asc").Find(&inbounds)
	cap := GetCapability(n.ID)
	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"node":       n,
			"online":     GlobalHub.IsNodeOnline(n.ID),
			"inbounds":   inbounds,
			"capability": cap,
		},
	})
}

func HandleCreateNode(c *gin.Context) {
	var req nodeCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "请填写节点名称"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "节点名称不能为空"})
		return
	}
	if req.Rate <= 0 {
		req.Rate = 1.0
	}
	addr := strings.TrimSpace(req.Address)
	if addr == "" {
		addr = "127.0.0.1"
	}

	node := Node{
		Name:      req.Name,
		Token:     NewToken(),
		Address:   addr,
		Rate:      req.Rate,
		Status:    "offline",
		Protected: NormalizeProtected(req.Protected),
	}
	if err := DB.Create(&node).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "创建节点失败: " + err.Error()})
		return
	}

	// 面板不打印明文 Token 到日志 (日志会进 journald), 只在本次响应里回给管理员一次。
	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "节点创建成功, 请在目标机执行一键纳管命令",
		"data": gin.H{
			"node":        node,
			"install_cmd": BuildAgentInstallCommand(c, node.Token),
		},
	})
}

type nodeUpdateRequest struct {
	Name      *string  `json:"name"`
	Address   *string  `json:"address"`
	Rate      *float64 `json:"rate"`
	Hidden    *bool    `json:"hidden"`
	Protected *string  `json:"protected"`
}

func HandleUpdateNode(c *gin.Context) {
	n, ok := loadNode(c)
	if !ok {
		return
	}
	var req nodeUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "请求参数格式错误"})
		return
	}

	// 用 map 而不是 struct 更新: GORM 的 struct 更新会跳过零值,
	// 于是 rate=0 / hidden=false 这类「有意义的零值」会被静默丢弃。
	updates := map[string]any{}
	if req.Name != nil {
		v := strings.TrimSpace(*req.Name)
		if v == "" {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "节点名称不能为空"})
			return
		}
		updates["name"] = v
	}
	if req.Address != nil {
		updates["address"] = strings.TrimSpace(*req.Address)
	}
	if req.Rate != nil {
		if *req.Rate <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "流量倍率必须大于 0"})
			return
		}
		updates["rate"] = *req.Rate
	}
	if req.Hidden != nil {
		updates["hidden"] = *req.Hidden
	}
	// 保护名单要清洗后再落库 —— 它最终会进以 root 执行的安装脚本。
	// 空串是合法值 (= 不保护任何配置), 用指针区分「没传」和「传了空」。
	if req.Protected != nil {
		updates["protected"] = NormalizeProtected(*req.Protected)
	}
	if len(updates) == 0 {
		c.JSON(http.StatusOK, gin.H{"code": 0, "message": "没有需要修改的字段", "data": n})
		return
	}
	if err := DB.Model(&Node{}).Where("id = ?", n.ID).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "更新节点失败"})
		return
	}
	DB.First(n, n.ID)
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "节点已更新", "data": n})
}

// HandleDeleteNode 删除节点及其入站清单。
//
// 只删面板侧记录, **不动机器上的 233boy 配置** —— 面板对节点是"纳管"不是"拥有",
// 误删一条记录不该把线上还在服务的节点配置一起带走。要清机器上的 conf 请用入站删除。
func HandleDeleteNode(c *gin.Context) {
	n, ok := loadNode(c)
	if !ok {
		return
	}
	if err := DB.Where("node_id = ?", n.ID).Delete(&Inbound{}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "清理入站清单失败"})
		return
	}
	if err := DB.Delete(&Node{}, n.ID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "删除节点失败"})
		return
	}
	GlobalHub.KickNode(n.ID)
	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "节点已从面板移除 (目标机上的 233boy 配置未改动)",
	})
}

// HandleSyncNode 让 agent 立刻重采集三层清单。
func HandleSyncNode(c *gin.Context) {
	n, ok := loadNode(c)
	if !ok {
		return
	}
	if !GlobalHub.IsNodeOnline(n.ID) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": 503, "error": "NodeOffline", "message": "节点当前离线, 无法采集"})
		return
	}
	res, err := GlobalHub.SendNodeOp(n.ID, &common.NodeOpPayload{Op: common.NodeOpInventory}, opTimeoutInventory)
	if err != nil {
		writeNodeOpError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "采集完成", "data": res})
}

func HandleGetNodeLogs(c *gin.Context) {
	n, ok := loadNode(c)
	if !ok {
		return
	}
	source := strings.TrimSpace(c.DefaultQuery("source", "v2ray"))
	lines, _ := strconv.Atoi(c.DefaultQuery("lines", "200"))
	if lines <= 0 || lines > 2000 {
		lines = 200
	}
	resp, err := GlobalHub.RequestNodeLogs(n.ID, source, lines)
	if err != nil {
		writeNodeOpError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": resp})
}

func HandleRestartNode(c *gin.Context) {
	n, ok := loadNode(c)
	if !ok {
		return
	}
	resp, err := GlobalHub.RestartNode(n.ID)
	if err != nil {
		writeNodeOpError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": resp.Message, "data": resp})
}

func HandleGetNodeCapability(c *gin.Context) {
	n, ok := loadNode(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": GetCapability(n.ID)})
}

// ---------------------------------------------------------------------------
// 入站 (233boy 节点配置) 增删查
// ---------------------------------------------------------------------------

func HandleListNodeInbounds(c *gin.Context) {
	n, ok := loadNode(c)
	if !ok {
		return
	}
	var inbounds []Inbound
	DB.Where("node_id = ?", n.ID).Order("sort_idx asc, id asc").Find(&inbounds)

	out := make([]gin.H, 0, len(inbounds))
	for i := range inbounds {
		ib := &inbounds[i]
		out = append(out, gin.H{
			"inbound":  ib,
			"warnings": ib.Warnings(),
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"code":      0,
		"data":      out,
		"online":    GlobalHub.IsNodeOnline(n.ID),
		"last_sync": n.LastSyncAt,
	})
}

// inboundUpdateItem 面板侧只允许改「展示与订阅」属性。
//
// Enabled/Remark/SortIdx 是面板自己的元数据, 不写回 233boy —— 面板与脚本是解耦的,
// 面板不该去改机器上的 conf (改了也会被下次 233boy 操作覆盖, 造成"改完又变回去"的错觉)。
type inboundUpdateItem struct {
	ID      uint    `json:"id"`
	Enabled *bool   `json:"enabled"`
	Remark  *string `json:"remark"`
	SortIdx *int    `json:"sort_idx"`
}

func HandleUpdateNodeInbounds(c *gin.Context) {
	n, ok := loadNode(c)
	if !ok {
		return
	}
	var items []inboundUpdateItem
	if err := c.ShouldBindJSON(&items); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "请求体应为入站更新数组"})
		return
	}

	updated := 0
	for _, it := range items {
		if it.ID == 0 {
			continue
		}
		updates := map[string]any{}
		if it.Enabled != nil {
			updates["enabled"] = *it.Enabled
		}
		if it.Remark != nil {
			updates["remark"] = *it.Remark
		}
		if it.SortIdx != nil {
			updates["sort_idx"] = *it.SortIdx
		}
		if len(updates) == 0 {
			continue
		}
		// 带 node_id 条件: 防止越权改到别的节点的入站。
		res := DB.Model(&Inbound{}).Where("id = ? AND node_id = ?", it.ID, n.ID).Updates(updates)
		if res.Error != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "更新入站失败: " + res.Error.Error()})
			return
		}
		updated += int(res.RowsAffected)
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "已更新", "data": gin.H{"updated": updated}})
}

// createInboundRequest 新建入站的请求体。
// 直接复用 common.AddSpec 的字段布局, 但额外带一个 dry_run 开关。
type createInboundRequest struct {
	common.AddSpec
	DryRun bool `json:"dry_run"`
}

// prepareSpec 补齐可自动生成的字段, 然后做**面板侧**的完整性校验。
//
// 这一步不能省: 参数不全时 233boy 会掉进 while read 自旋, 把 agent 的一个 worker 卡住。
// AddSpec.BuildArgs() 是唯一能提前拦住它的闸门, 所以面板在派发前先跑一遍,
// agent 侧还会再跑一遍 (双保险: 面板可能被绕过, agent 也不能信任面板)。
func prepareSpec(spec *common.AddSpec) error {
	def, ok := common.LookupProtocol(spec.Token)
	if !ok {
		return errors.New("不支持的协议简写: " + spec.Token)
	}
	spec.Token = def.Token

	// UUID 缺失就生成一个 —— 但不能是 "auto" (脚本会 unset 后随机, 面板将拿不到真实值)。
	if def.IsTLS {
		if strings.TrimSpace(spec.UUID) == "" {
			spec.UUID = newUUID()
		}
		if strings.TrimSpace(spec.Path) == "" {
			// 与 233boy 默认习惯一致: 路径用 uuid, 天然唯一, 同机多节点不会撞 path。
			spec.Path = "/" + strings.ReplaceAll(spec.UUID, "-", "")
		}
		if strings.TrimSpace(spec.Host) == "" {
			return errors.New("TLS 协议必须填写域名 (host): 缺少时 233boy 会进入交互式等待")
		}
	}

	_, _, err := spec.BuildArgs()
	return err
}

func HandleCreateNodeInbound(c *gin.Context) {
	n, ok := loadNode(c)
	if !ok {
		return
	}
	var req createInboundRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "请求参数格式错误: " + err.Error()})
		return
	}
	if err := prepareSpec(&req.AddSpec); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": err.Error()})
		return
	}
	if !GlobalHub.IsNodeOnline(n.ID) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": 503, "error": "NodeOffline", "message": "节点当前离线, 无法下发配置"})
		return
	}

	op := common.NodeOpCreate
	timeout := opTimeoutCreate
	if req.DryRun {
		// dry_run 走 gen 干跑: 只回显将要写入的 JSON, 不落盘、不建 Caddy。
		// 233boy 在 is_gen 时于 core.sh:352 提前 return, 早于 :366 的落盘 —— 确实安全。
		op = common.NodeOpPreview
		timeout = opTimeoutPreview
	}

	res, err := GlobalHub.SendNodeOp(n.ID, &common.NodeOpPayload{Op: op, Spec: &req.AddSpec}, timeout)
	if err != nil {
		writeNodeOpError(c, err)
		return
	}
	if !res.OK {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"error":   "NodeOpFailed",
			"message": firstNonEmpty(res.Error, "节点操作失败"),
			"data":    res,
		})
		return
	}

	// 落盘成功后让 agent 重采集一次, 把新节点的三层信息 (尤其 Caddy 随机分配的内网口
	// 与公网 path) 立刻同步进面板 —— 这些值只能从机器上读回来, 不能靠推断。
	if !req.DryRun {
		GlobalHub.RequestInventory(n.ID)

		// 还要把**用户名单**也下发一次。
		//
		// 为什么必须在这里做: 新建的 conf 是 233boy 写的, 里面只有 clients[0]
		// (它自己那个 UUID), **没有订阅用户**。而用户同步此前只在"名单变化"时
		// 触发 —— 新建一个 inbound 并不改变名单, 于是这个新 conf 上永远铺不上用户,
		// 客户端拿订阅里的链接去连只会被拒 (UUID 不在该 conf 的 clients[] 里)。
		//
		// 症状很隐蔽: 面板上节点在线、订阅里线路也在、链接看着完全正常,
		// 就是连不上。实测踩到过一次 (给新机器建第一个 VLESS-WS-TLS 节点时)。
		//
		// 幂等: 名单一致时 agent 直接返回, 不写盘不重启内核。
		PushUserSyncAll()
	}

	msg := "节点配置已创建"
	if req.DryRun {
		msg = "干跑通过, 未写入配置"
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": msg, "data": res})
}

func HandleDeleteNodeInbound(c *gin.Context) {
	n, ok := loadNode(c)
	if !ok {
		return
	}
	confName := strings.TrimSpace(c.Param("conf"))
	if confName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "缺少配置名"})
		return
	}
	// 配置名会进 agent 侧的命令行, 必须挡掉路径穿越与 shell 元字符。
	if strings.ContainsAny(confName, "/\\ \t\"';$`|&<>*?") || strings.Contains(confName, "..") {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "配置名含非法字符"})
		return
	}
	if !GlobalHub.IsNodeOnline(n.ID) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": 503, "error": "NodeOffline", "message": "节点当前离线, 无法删除配置"})
		return
	}

	res, err := GlobalHub.SendNodeOp(n.ID, &common.NodeOpPayload{
		Op:       common.NodeOpDelete,
		ConfName: confName,
	}, opTimeoutDelete)
	if err != nil {
		writeNodeOpError(c, err)
		return
	}

	// 注意: 233boy 的 del() 即使成功也会以退出码 1 结束
	// (core.sh:767 的 `[[ $is_dont_auto_exit ]] && unset is_config_file` 在 CLI 路径下恒假),
	// 所以 **不能** 用 res.OK 判定成败 —— 以 conf 文件是否真的消失为准。
	// agent 侧已按这个规则算好 res.OK 并把 del 的退出码放进 steps 的 Detail 里备查。
	if !res.OK {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"error":   "NodeOpFailed",
			"message": firstNonEmpty(res.Error, "删除失败"),
			"data":    res,
		})
		return
	}

	GlobalHub.RequestInventory(n.ID)
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "配置已删除", "data": res})
}

// HandleProbeNodeInbound 公网可达性探测 (TLS 握手 + WS Upgrade 断言 101)。
// 由 agent 从目标机发起 —— 从面板侧探测只能验证「面板到节点」,
// 而用户关心的是「客户端到节点」, 在节点本机发起最接近真实链路。
func HandleProbeNodeInbound(c *gin.Context) {
	n, ok := loadNode(c)
	if !ok {
		return
	}
	var body struct {
		ConfName   string `json:"conf_name"`
		TimeoutSec int    `json:"timeout_sec"`
	}
	_ = c.ShouldBindJSON(&body)
	if strings.TrimSpace(body.ConfName) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "缺少配置名"})
		return
	}
	if !GlobalHub.IsNodeOnline(n.ID) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": 503, "error": "NodeOffline", "message": "节点当前离线, 无法探测"})
		return
	}
	if body.TimeoutSec <= 0 || body.TimeoutSec > 20 {
		body.TimeoutSec = 8
	}
	res, err := GlobalHub.SendNodeOp(n.ID, &common.NodeOpPayload{
		Op:              common.NodeOpProbe,
		ConfName:        body.ConfName,
		ProbeTimeoutSec: body.TimeoutSec,
	}, opTimeoutProbe)
	if err != nil {
		writeNodeOpError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": res})
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// writeNodeOpError 把 hub 的错误翻译成合适的 HTTP 状态码。
func writeNodeOpError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	code := "InternalError"
	msg := err.Error()
	if errors.Is(err, ErrNodeOffline) {
		status = http.StatusServiceUnavailable
		code = "NodeOffline"
	} else if errors.Is(err, ErrOpTimeout) {
		status = http.StatusGatewayTimeout
		code = "NodeOpTimeout"
	}
	c.JSON(status, gin.H{"code": status, "error": code, "message": msg})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
