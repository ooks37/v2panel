package agent

import (
	"sort"
	"strings"
	"time"

	"v2panel/internal/common"
)

// ===========================================================================
// 本地解析结果 (NodeView) -> 线格式 (common.InboundView) 的转换。
//
// 为什么不把 NodeView 直接当线格式: internal/agent 依赖 internal/common,
// 反向引用成环。契约与实现解耦还有第二个好处 —— 解析器重构不会破坏面板。
//
// ⚠️ 三层字段来源不可混 (面板按层做交叉校验与告警):
//   conf 层  → InternalPort/ListenAddr/InboundNet/InboundSec/UUID/Password/Method
//   url 层   → Address/PublicPort/Host/Path/ClientSecurity/ClientNetwork/URI
//   Caddy 层 → Caddy* (仅供校验, 不作权威)
// ===========================================================================

// ToInboundView 把一条本地节点视图转成线格式。
func ToInboundView(n NodeView) common.InboundView {
	return common.InboundView{
		// —— conf 层 (权威) ——
		ConfName:      n.ConfName,
		ConfFile:      n.ConfFile,
		Protocol:      n.Protocol,
		UUID:          n.UUID,
		Password:      n.Password,
		Method:        n.Method,
		User:          n.User,
		InternalPort:  n.InternalPort,
		ListenAddr:    n.ListenAddr,
		InboundNet:    n.InboundNet,
		InboundSec:    n.InboundSec,
		IsDynamicPort: n.IsDynamicPort,

		// —— url 层 (权威) ——
		Address:        n.Address,
		PublicPort:     n.PublicPort,
		Host:           n.Host,
		Path:           n.Path,
		ClientSecurity: n.ClientSecurity,
		ClientNetwork:  n.ClientNetwork,
		URI:            n.URI,

		// —— Caddy 层 (校验用) ——
		CaddyFile:     n.CaddyFile,
		CaddySite:     n.CaddySite,
		CaddyHost:     n.CaddyHost,
		CaddyPort:     n.CaddyPort,
		CaddyUpstream: n.CaddyUpstream,
		CaddyPath:     n.CaddyPath,

		HasConf:  n.HasConf,
		HasCaddy: n.HasCaddy,
		HasURL:   n.HasURL,
		Warnings: append([]string(nil), n.Warnings...),
	}
}

// BuildInventory 跑一次完整采集, 产出可以直接上报的 InventoryReportPayload。
//
// Complete 的语义 (面板侧 UpsertInventory 据此决定是否清理"消失的 conf"):
//
//	true  → 三个数据源全部读成功, Nodes 是完整事实, 面板可以安全地删掉库里多余的 conf
//	false → 任一源读失败 (例如 url 层整批超时), Nodes 可能是残缺的,
//	        此时面板**绝不能**清理 —— 否则会把还在的节点误删, 订阅当场断掉。
func (d *Driver) BuildInventory() (*common.InventoryReportPayload, error) {
	started := time.Now()
	snap, err := d.Snapshot()
	if err != nil {
		return nil, err
	}

	rep := &common.InventoryReportPayload{
		HTTPPort:    snap.HTTPPort,
		HTTPSPort:   snap.HTTPSPort,
		Warnings:    append([]string(nil), snap.Warnings...),
		CollectedAt: started.UnixMilli(),
		Complete:    true,
	}

	// 判定"完整"的硬条件: conf 层读成功 且 url 层没有整批失败。
	// (Caddy 层允许缺失 —— 纯动态端口机器本来就没有 Caddy。)
	urlFailed := false
	for _, w := range snap.Warnings {
		if strings.Contains(w, "采集客户端链接失败") {
			urlFailed = true
			break
		}
	}
	if urlFailed {
		rep.Complete = false
	}

	rep.Nodes = make([]common.InboundView, 0, len(snap.Nodes))
	for _, n := range snap.Nodes {
		rep.Nodes = append(rep.Nodes, ToInboundView(n))
	}
	// 稳定排序: 面板按 conf 名做主键, 顺序固定后 diff 才不会被抖动干扰。
	sort.Slice(rep.Nodes, func(i, j int) bool { return rep.Nodes[i].ConfName < rep.Nodes[j].ConfName })

	rep.DurationMs = time.Since(started).Milliseconds()
	return rep, nil
}

// BuildCapability 产出能力上报 (协议表 + reality 可用性 + 内核版本 + 本机保护名单)。
func (d *Driver) BuildCapability(c *Collector) *common.CapabilityReportPayload {
	caps := d.Capabilities()
	rep := &common.CapabilityReportPayload{
		Protocols:  caps.Protocols,
		Reality:    caps.Reality,
		RealityWhy: caps.RealityWhy,
		HasCaddy:   caps.HasCaddy,
		ConfDir:    d.Paths.ConfDir,
		Protected:  append([]string(nil), d.Protected...),
	}
	if c != nil {
		rep.CoreVersion = c.cachedVersion(&c.v2rayVer, c.V2rayBin, []string{"version"}, parseV2rayVersion)
		rep.CaddyVersion = c.cachedVersion(&c.caddyVer, c.CaddyBin, []string{"version"}, firstField)
	}
	return rep
}
