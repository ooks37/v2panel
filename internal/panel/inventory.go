package panel

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"v2panel/internal/common"
)

// UpsertInventory 把 agent 上报的整机清单落库。
//
// 合并规则 (与 internal/agent/parser.go 的 Merge() 一致, 权威层不可混):
//
//	conf 层 -> Protocol/Network/InternalPort/ListenAddr/InboundSec/UUID/Password/Method/User
//	url 层  -> Address/PublicPort/Host/Path/ClientSecurity/ClientNetwork/URI
//	Caddy 层-> CaddyHost/CaddyPort/CaddyUpstream/CaddyPath (交叉校验)
//
// ⚠️ 只覆盖「机器事实」字段。Enabled / Remark / SortIdx 是**面板侧人工设置**,
// 每次采集都覆盖它们等于「用户改一次就被回滚一次」。
func UpsertInventory(nodeID uint, rep *common.InventoryReportPayload) error {
	if rep == nil {
		return fmt.Errorf("空清单")
	}
	if DB == nil {
		return fmt.Errorf("数据库未初始化")
	}

	now := time.Now()
	updates := map[string]any{"last_sync_at": &now}
	// 机器级公网端口 (Caddy http_port/https_port): 0 表示这次没采到, 不要用 0 覆盖已有值。
	if rep.HTTPPort > 0 {
		updates["http_port"] = rep.HTTPPort
	}
	if rep.HTTPSPort > 0 {
		updates["https_port"] = rep.HTTPSPort
	}
	if err := DB.Model(&Node{}).Where("id = ?", nodeID).Updates(updates).Error; err != nil {
		return fmt.Errorf("更新节点机器级信息失败: %w", err)
	}

	var existing []Inbound
	if err := DB.Where("node_id = ?", nodeID).Find(&existing).Error; err != nil {
		return fmt.Errorf("读取既有节点实例失败: %w", err)
	}
	byName := make(map[string]*Inbound, len(existing))
	for i := range existing {
		byName[existing[i].ConfName] = &existing[i]
	}

	seen := make(map[string]bool, len(rep.Nodes))
	for _, v := range rep.Nodes {
		if v.ConfName == "" {
			continue
		}
		seen[v.ConfName] = true

		ib, ok := byName[v.ConfName]
		if !ok {
			// 新出现的 conf: 可能是面板建的, 也可能是用户手工 233boy add 的 ——
			// 一律纳管并置 Enabled=true (面板的职责是「如实呈现机器现状」)。
			ib = &Inbound{NodeID: nodeID, ConfName: v.ConfName, Enabled: true}
		}

		ib.Protocol = v.Protocol
		ib.Network = v.InboundNet
		ib.InternalPort = v.InternalPort
		ib.ListenAddr = v.ListenAddr
		ib.InboundSec = v.InboundSec
		ib.UUID = v.UUID
		ib.Password = v.Password
		ib.Method = v.Method
		ib.User = v.User

		ib.Address = v.Address
		ib.PublicPort = v.PublicPort
		ib.Host = v.Host
		ib.Path = v.Path
		ib.ClientSecurity = v.ClientSecurity
		ib.ClientNetwork = v.ClientNetwork
		ib.URI = v.URI

		ib.CaddyHost = v.CaddyHost
		ib.CaddyPort = v.CaddyPort
		ib.CaddyUpstream = v.CaddyUpstream
		ib.CaddyPath = v.CaddyPath

		ib.HasConf = v.HasConf
		ib.HasCaddy = v.HasCaddy
		ib.HasURL = v.HasURL
		ib.SetWarnings(v.Warnings)

		if err := DB.Save(ib).Error; err != nil {
			log.Printf("[Panel] 节点 %d 实例 %s 入库失败: %v", nodeID, v.ConfName, err)
		}
	}

	// 清理已被删除的 conf —— 仅在 Complete=true 时执行。
	// 采集异常时 Nodes 可能是残缺的, 此时清理会把还在的节点误删 (订阅直接断)。
	if !rep.Complete {
		log.Printf("[Panel] 节点 %d 清单不完整 (complete=false), 跳过清理, 保留 %d 条既有记录", nodeID, len(existing))
		return nil
	}
	for _, old := range existing {
		if seen[old.ConfName] {
			continue
		}
		if err := DB.Delete(&Inbound{}, old.ID).Error; err != nil {
			log.Printf("[Panel] 清理节点 %d 已消失实例 %s 失败: %v", nodeID, old.ConfName, err)
			continue
		}
		log.Printf("[Panel] 节点 %d 实例 %s 已从机器上消失, 已从面板清理", nodeID, old.ConfName)
	}
	return nil
}

// capabilitySettingKey 能力报文的存储键。
func capabilitySettingKey(nodeID uint) string {
	return fmt.Sprintf("capability:%d", nodeID)
}

// SaveCapability 持久化节点的协议能力 (协议表 / 内核版本 / reality 可用性)。
//
// 存 SystemSetting 而不是给 Node 加列: 这是一份**由 agent 如实上报**的 JSON,
// 面板只做透传与灰显, 不需要按字段查询。
func SaveCapability(nodeID uint, rep *common.CapabilityReportPayload) error {
	if rep == nil {
		return fmt.Errorf("空能力报文")
	}
	b, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	return SetSetting(capabilitySettingKey(nodeID), string(b))
}

// GetCapability 读取节点能力报文; 未上报过时返回 nil。
func GetCapability(nodeID uint) *common.CapabilityReportPayload {
	raw := GetSetting(capabilitySettingKey(nodeID), "")
	if raw == "" {
		return nil
	}
	var rep common.CapabilityReportPayload
	if err := json.Unmarshal([]byte(raw), &rep); err != nil {
		return nil
	}
	return &rep
}
