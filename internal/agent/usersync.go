package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"v2panel/internal/common"
)

// 按用户下发凭据 (真限流的实现层)。
//
// ─────────────────────────────────────────────────────────────────────────
// 为什么必须这么做
// ─────────────────────────────────────────────────────────────────────────
// 233boy 的 add() 只会往 conf 写**一个** client (clients[0]), 全机所有客户端
// 共用那一个 UUID —— 于是 V2Ray 的 `inbound>>>TAG>>>traffic>>>...` 计数器只能
// 给出**整机**流量, 分不清是谁用的。这就是「流量限制」在早期版本里只能按
// 订阅账号数平均分摊的原因, 也是它根本不叫限制的原因。
//
// 但 V2Ray 本身完全支持多用户:
//   - `settings.clients` 是**数组**;
//   - `policy.levels.0.statsUserUplink/Downlink` 一开, 它就会按
//     `clients[].email` 单独出计数器 (`user>>>EMAIL>>>traffic>>>uplink`)。
//   实测 82 上这两个开关**本来就是开的** (233boy 的默认模板里带), 只是从来
//   没人往 clients[] 里放过第二个用户, 所以那套计数器一直是空的。
//
// 所以「真限流」的完整链路是:
//   面板下发全量用户名单 → agent 把 UUID 写进每个 conf 的 clients[] 并打上 email
//   → V2Ray 按 email 计数 → agent 读 `user>>>EMAIL>>>...` 增量 → 面板累加到账号
//   → 超额后面板把该用户标为 disabled → 下次 sync 时 agent 把它从 clients[] 摘掉
//   → 客户端立刻连不上。这才是真的停。
//
// ─────────────────────────────────────────────────────────────────────────
// 红线: clients[0] 永远不许动
// ─────────────────────────────────────────────────────────────────────────
// `v2ray url` 取的是 `clients[0].id` 来生成客户端链接, 而面板的地址/path 推导
// 完全依赖那份输出 (见 parser.go 顶部注释)。把 233boy 写的那个 client 挪走或改掉,
// 会让该节点的订阅链接整体错乱。因此:
//
//   - clients[0] 原样保留 (如果它带 email, 一并保留);
//   - 订阅用户一律从 clients[1] 起追加, 按 UUID 去重;
//   - 同步时只删「带 v2panel 标记的 email 且不在名单里」的条目 ——
//     手工加进去的第三方 client 不会被误删。

const (
	// userEmailSuffix 是所有由面板管理的 client 的 email 后缀。
	// 同步时只碰带这个后缀的条目, 从而与手工维护的 client 隔离。
	userEmailSuffix = "@v2panel"

	// confBackupSuffix 改写前的备份后缀。出问题可以手工恢复。
	confBackupSuffix = ".v2panel-bak"
)

// UserEmailFor 生成稳定的 per-user 计数器标识。
//
// 用 UserID 而不是用户名: 用户名可改, 而 V2Ray 的计数器是按 email 字符串聚合的,
// 改名会让历史计数与新计数裂成两条, 面板那边就会看到用量突然归零。
func UserEmailFor(userID uint) string {
	return fmt.Sprintf("u%d%s", userID, userEmailSuffix)
}

// parseUserIDFromEmail 从 email 反解用户 ID。非面板管理的 email 返回 0。
//
// 与 UserEmailFor 严格互逆 —— 改其中一个必须同步改另一个, 否则流量会记到错的人头上。
func parseUserIDFromEmail(email string) uint {
	email = strings.TrimSpace(email)
	if !isManagedEmail(email) {
		return 0
	}
	body := strings.TrimSuffix(email, userEmailSuffix)
	if !strings.HasPrefix(body, "u") {
		return 0
	}
	n, err := strconv.ParseUint(body[1:], 10, 32)
	if err != nil || n == 0 {
		return 0
	}
	return uint(n)
}

// isManagedEmail 判断某个 client 的 email 是否由面板管理。
func isManagedEmail(email string) bool {
	return strings.HasSuffix(strings.TrimSpace(email), userEmailSuffix)
}

// confClient 是 conf 里 clients[] 的一项。
//
// 用 map 而不是 struct: 不同协议字段名不一样 (vless/vmess 用 id, trojan 用 password,
// ss 用 method+password), 而**未识别的字段必须原样保留** —— 用 struct 反序列化
// 会静默丢掉未知字段, 那等于每次同步都在破坏别人的配置。
type confClient = map[string]any

// SyncUsersResult 一次用户同步的结果。
type SyncUsersResult struct {
	Changed   []string // 被改写的 conf 文件名
	Unchanged []string
	Errors    []string
	// Added/Removed 累计增删的 client 数, 用于面板显示
	Added   int
	Removed int
}

// SyncUsers 让机器上所有 conf 的 clients[] 与给定名单对齐。
//
// 幂等: 重复调用结果一致。只改真正需要改的文件, 并先写 .v2panel-bak 备份。
func (d *Driver) SyncUsers(users []common.UserCredential) (*SyncUsersResult, error) {
	res := &SyncUsersResult{}

	confs, err := d.Runner.ConfFiles()
	if err != nil {
		return nil, fmt.Errorf("列举配置失败: %w", err)
	}
	if len(confs) == 0 {
		return res, nil
	}

	// 期望的 UUID 集合 (只含启用中的用户)
	want := make(map[string]string, len(users)) // uuid -> email
	for _, u := range users {
		if !u.Enabled || strings.TrimSpace(u.UUID) == "" {
			continue
		}
		want[strings.ToLower(strings.TrimSpace(u.UUID))] = u.Email
	}

	for _, name := range confs {
		path := filepath.Join(d.confDir(), name)
		changed, added, removed, err := d.syncOneConf(path, want)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if changed {
			res.Changed = append(res.Changed, name)
			res.Added += added
			res.Removed += removed
		} else {
			res.Unchanged = append(res.Unchanged, name)
		}
	}
	return res, nil
}

// syncOneConf 对齐单个 conf 的 clients[], 返回 (是否有改动, 新增数, 删除数)。
func (d *Driver) syncOneConf(path string, want map[string]string) (bool, int, int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, 0, 0, err
	}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		// conf 不是合法 JSON —— 绝不尝试"修"它, 交给上层报错。
		return false, 0, 0, fmt.Errorf("配置不是合法 JSON: %w", err)
	}

	inbounds, ok := doc["inbounds"].([]any)
	if !ok || len(inbounds) == 0 {
		return false, 0, 0, nil
	}

	added, removed := 0, 0
	anyChange := false

	for _, ibAny := range inbounds {
		ib, ok := ibAny.(map[string]any)
		if !ok {
			continue
		}
		settings, ok := ib["settings"].(map[string]any)
		if !ok {
			continue
		}
		clientsAny, ok := settings["clients"].([]any)
		if !ok {
			// socks/http 这类协议用 accounts[] 或根本不鉴权 —— 跳过,
			// 不要给它硬塞一个 clients 数组 (会让 v2ray 起不来)。
			continue
		}

		newClients, a, r, ch := alignClients(clientsAny, want, d.clientIDField(ib))
		if ch {
			settings["clients"] = newClients
			ib["settings"] = settings
			added += a
			removed += r
			anyChange = true
		}
	}

	if !anyChange {
		return false, 0, 0, nil
	}

	// 备份 + 原子写
	if err := os.WriteFile(path+confBackupSuffix, raw, 0o600); err != nil {
		return false, 0, 0, fmt.Errorf("写备份失败: %w", err)
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return false, 0, 0, err
	}
	out = append(out, '\n')

	tmp := path + ".v2panel-tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return false, 0, 0, fmt.Errorf("写临时文件失败: %w", err)
	}
	// 原子替换: 直接覆盖原文件的话, v2ray 可能读到写了一半的内容。
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return false, 0, 0, fmt.Errorf("替换配置失败: %w", err)
	}
	return true, added, removed, nil
}

// clientIDField 判断这个 inbound 的 client 用哪个字段存 UUID。
//
// vless/vmess 用 `id`; trojan/ss 用 `password` (trojan 的 password 就是 UUID 形态,
// ss 是自定义密码)。用错字段会写出一个 v2ray 读不懂的配置。
func (d *Driver) clientIDField(ib map[string]any) string {
	switch strings.ToLower(fmt.Sprint(ib["protocol"])) {
	case "trojan", "shadowsocks":
		return "password"
	default:
		return "id"
	}
}

// alignClients 对齐一个 clients 数组。返回 (新数组, 新增数, 删除数, 是否改动)。
//
// 规则:
//  1. clients[0] 原样保留 —— 它是 233boy 自己写的, 动它会毁掉订阅链接推导。
//  2. 保留所有**非面板管理**的 client (email 不带 @v2panel 后缀), 不动它们。
//  3. 面板管理的 client: 在名单里的保留 (并按需补/正 email), 不在名单里的删除。
//  4. 名单里缺的 UUID 追加到末尾。
func alignClients(clients []any, want map[string]string, idField string) ([]any, int, int, bool) {
	out := make([]any, 0, len(clients)+len(want))
	seen := make(map[string]bool)
	added, removed := 0, 0
	changed := false

	for i, cAny := range clients {
		c, ok := cAny.(confClient)
		if !ok {
			out = append(out, cAny)
			continue
		}

		// (1) clients[0] 永远原样保留
		if i == 0 {
			out = append(out, c)
			if id := clientUUID(c, idField); id != "" {
				seen[strings.ToLower(id)] = true
			}
			continue
		}

		email := strings.TrimSpace(fmt.Sprint(c["email"]))
		id := clientUUID(c, idField)

		// (2) 非面板管理的 client: 原样保留, 但记下 UUID 免得重复追加
		if !isManagedEmail(email) {
			out = append(out, c)
			if id != "" {
				seen[strings.ToLower(id)] = true
			}
			continue
		}

		// (3) 面板管理的: 看它还在不在名单里
		lower := strings.ToLower(id)
		wantEmail, keep := want[lower]
		if !keep {
			removed++
			changed = true
			continue // 摘掉 —— 这就是"熔断"在机器上的实际动作
		}

		// 在名单里: 顺手把 email 修正成期望值 (历史数据可能用了别的格式)
		if email != wantEmail {
			c["email"] = wantEmail
			changed = true
		}
		out = append(out, c)
		seen[lower] = true
	}

	// (4) 补齐名单里缺的
	// 排序保证输出稳定 —— 否则每次同步的 conf 内容顺序都可能不同,
	// 会造成"明明没变却每次都写盘 + 重启内核"。
	missing := make([]string, 0)
	for uuid := range want {
		if !seen[uuid] {
			missing = append(missing, uuid)
		}
	}
	sort.Strings(missing)

	for _, uuid := range missing {
		// 新 client 只带 id/password + email。其他字段 (level/flow/alterId)
		// 一律不写: V2Ray 的默认值就是我们要的, 写了反而可能因协议差异出错。
		nc := confClient{idField: uuid, "email": want[uuid]}
		out = append(out, nc)
		seen[uuid] = true
		added++
		changed = true
	}

	return out, added, removed, changed
}

// clientUUID 从 client 里取 UUID, 兼容 id / password 两种字段名。
func clientUUID(c confClient, idField string) string {
	if v, ok := c[idField].(string); ok {
		return strings.TrimSpace(v)
	}
	// 兜底: 有些 conf 的 trojan 同时写了 id 与 password
	for _, k := range []string{"id", "password"} {
		if v, ok := c[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// CountManagedClients 统计某个 conf 里由面板管理的 client 数量 (只读, 供清单展示)。
func CountManagedClients(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0
	}
	n := 0
	inbounds, _ := doc["inbounds"].([]any)
	for _, ibAny := range inbounds {
		ib, _ := ibAny.(map[string]any)
		settings, _ := ib["settings"].(map[string]any)
		clients, _ := settings["clients"].([]any)
		for i, cAny := range clients {
			if i == 0 {
				continue
			}
			c, _ := cAny.(confClient)
			if isManagedEmail(fmt.Sprint(c["email"])) {
				n++
			}
		}
	}
	return n
}

// confDir 取 233boy 配置目录。Runner 内部知道这个路径, 但没暴露 getter,
// 而 usersync 需要拼完整路径 —— 这里按同一套默认值推导, 保持与 Runner 一致。
func (d *Driver) confDir() string {
	if v := strings.TrimSpace(d.Paths.ConfDir); v != "" {
		return v
	}
	return "/etc/v2ray/conf"
}

// managedEmailsFromConf 扫机器上所有 conf, 收集由面板管理的 client email。
//
// 用途: agent 启动时给 UserTrafficTracker 一个初始名单 —— 见 NewOps 的注释。
// 只读, 不改任何文件; 任何 conf 读失败都跳过 (启动路径不能因为一个坏文件挂掉)。
func (d *Driver) managedEmailsFromConf() []string {
	confs, err := d.Runner.ConfFiles()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, name := range confs {
		raw, err := os.ReadFile(filepath.Join(d.confDir(), name))
		if err != nil {
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			continue
		}
		inbounds, _ := doc["inbounds"].([]any)
		for _, ibAny := range inbounds {
			ib, _ := ibAny.(map[string]any)
			settings, _ := ib["settings"].(map[string]any)
			clients, _ := settings["clients"].([]any)
			for _, cAny := range clients {
				c, _ := cAny.(confClient)
				if e := strings.TrimSpace(fmt.Sprint(c["email"])); isManagedEmail(e) {
					seen[e] = true
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for e := range seen {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}
