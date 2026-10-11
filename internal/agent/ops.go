package agent

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"v2panel/internal/common"
)

// ===========================================================================
// NodeOp 执行器 —— 把面板下发的操作翻译成 233boy 驱动调用, 并回报**事实**。
//
// 回报原则 (与 driver.go 的建节点事实链同一套):
//   不报"配置已应用", 只报"conf 目录现在长什么样"。OK 一律由独立事实判定,
//   绝不用 233boy 脚本的退出码 (它全篇没有 set -e, 失败也常常 exit 0;
//   del() 反过来是成功也 exit 1)。
// ===========================================================================

// Ops 节点操作执行器。
type Ops struct {
	Driver  *Driver
	Traffic *TrafficTracker
	// UserTraffic 按账号的流量跟踪 (真限流用, 见 usersync.go)
	UserTraffic *UserTrafficTracker

	mu sync.Mutex // 233boy 脚本改动共享状态, 操作必须串行
}

// NewOps 构造执行器。
func NewOps(d *Driver) *Ops {
	r := d.runner()
	o := &Ops{
		Driver:      d,
		Traffic:     NewTrafficTracker(r),
		UserTraffic: NewUserTrafficTracker(r),
	}
	// 启动时先从机器上的 conf 里认出「哪些 email 是面板管理的用户」。
	//
	// 这一步只在 agent 启动时做一次, 但很关键: agent 重启后基线是空的,
	// 而面板要等到下一次 sync_users 才会把名单推过来。中间这段时间若用户
	// 产生流量, 没有名单就会把 0→N 的整段增量当成"新 client"丢掉。
	// 从 conf 反推名单能把这个窗口补上 (conf 里 clients[] 带 @v2panel 后缀的
	// 就是面板用户, 见 usersync.go 的 UserEmailFor)。
	o.UserTraffic.SetKnownUsers(d.managedEmailsFromConf())
	return o
}

// Execute 执行一个 NodeOpPayload 并返回结果。
//
// ⚠️ OpID 必须原样回填: 面板 hub 的 handleNodeOpResp 看到空 op_id 会直接丢弃整条
// 响应 (无法配对), 调用方就只能等到超时。所以任何提前返回路径都带上它。
func (o *Ops) Execute(p *common.NodeOpPayload) *common.NodeOpResultPayload {
	started := time.Now()
	res := &common.NodeOpResultPayload{OpID: p.OpID, Op: p.Op}
	defer func() { res.DurationMs = time.Since(started).Milliseconds() }()

	// 只读操作 (清单/探测/日志) 不加串行锁: 它们不写脚本状态, 且面板会在
	// 建节点后立刻 RequestInventory, 锁住会让这一步白等。
	switch p.Op {
	case common.NodeOpInventory:
		o.opInventory(res)
		return res
	case common.NodeOpPreview:
		o.opPreview(res, p)
		return res
	case common.NodeOpProbe:
		o.opProbe(res, p)
		return res
	case common.NodeOpLogs:
		o.opLogs(res, p)
		return res
	case common.NodeOpCreate, common.NodeOpDelete, common.NodeOpRestart, common.NodeOpSyncUsers:
		o.mu.Lock()
		defer o.mu.Unlock()
	default:
		res.OK = false
		res.Error = "未知操作: " + p.Op
		return res
	}

	switch p.Op {
	case common.NodeOpCreate:
		o.opCreate(res, p)
	case common.NodeOpDelete:
		o.opDelete(res, p)
	case common.NodeOpRestart:
		o.opRestart(res)
	case common.NodeOpSyncUsers:
		o.opSyncUsers(res, p)
	}
	return res
}

// ---------------------------------------------------------------------------
// 各操作
// ---------------------------------------------------------------------------

func (o *Ops) opInventory(res *common.NodeOpResultPayload) {
	rep, err := o.Driver.BuildInventory()
	if err != nil {
		res.OK = false
		res.Error = "采集清单失败: " + err.Error()
		return
	}
	res.OK = true
	res.Warnings = rep.Warnings
	res.Steps = []common.StepInfo{{
		Name: "采集三层清单", OK: true,
		Detail: fmt.Sprintf("%d 个 conf, http=%d https=%d, complete=%v",
			len(rep.Nodes), rep.HTTPPort, rep.HTTPSPort, rep.Complete),
	}}
}

func (o *Ops) opPreview(res *common.NodeOpResultPayload, p *common.NodeOpPayload) {
	if p.Spec == nil {
		res.OK = false
		res.Error = "preview 缺少 spec"
		return
	}
	confName, raw, err := o.Driver.Preview(*p.Spec)
	if err != nil {
		res.OK = false
		res.Error = err.Error()
		res.ConfName = confName
		res.Steps = []common.StepInfo{{Name: "gen 干跑", OK: false, Detail: err.Error()}}
		return
	}
	res.OK = true
	res.ConfName = confName
	res.Output = raw
	res.Steps = []common.StepInfo{{
		Name: "gen 干跑", OK: true,
		Detail: "参数已被 233boy 接受, 未落盘",
	}}
	// ⚠️ 干跑里的端口是脚本随机分配的, 与真正 add 时的端口不是同一个 ——
	// 只用于判断"参数能否被接受", 绝不能当节点端口用。
	res.Warnings = append(res.Warnings,
		"干跑结果中的内网端口是脚本本次随机分配的, 与正式创建时不同; 请以创建后读回的值为准")
}

func (o *Ops) opCreate(res *common.NodeOpResultPayload, p *common.NodeOpPayload) {
	if p.Spec == nil {
		res.OK = false
		res.Error = "create 缺少 spec"
		return
	}
	r, err := o.Driver.Create(*p.Spec)
	if err != nil {
		res.OK = false
		res.Error = err.Error()
	}
	if r != nil {
		applyCreateResult(res, r)
	}
	// 建完立刻刷新一次流量基线, 免得新 conf 的累计值被算成"从 0 到 N"的巨额增量。
	if o.Traffic != nil {
		o.Traffic.Resync()
	}
}

func (o *Ops) opDelete(res *common.NodeOpResultPayload, p *common.NodeOpPayload) {
	name := strings.TrimSpace(p.ConfName)
	if name == "" {
		res.OK = false
		res.Error = "delete 缺少 conf_name"
		return
	}
	r, err := o.Driver.Delete(name)
	if err != nil {
		res.OK = false
		res.Error = err.Error()
	}
	if r != nil {
		applyCreateResult(res, r)
	}
	if o.Traffic != nil {
		o.Traffic.Resync()
	}
}

// opSyncUsers 把面板的用户名单写进各 conf 的 clients[]。
//
// 这是「真限流」的落地点: 名单里没有的用户会被从 clients[] 摘掉, 客户端立刻断线。
//
// 与其它 op 的关键区别: **不调用 233boy 脚本**。因为 add() 只会写一个 client,
// 没有多用户接口; 而 clients[] 本身就是数组, 直接增删是唯一可行路径。
// 所以这个操作走 driver 自己实现的 SyncUsers (见 usersync.go), 绕开脚本。
//
// 改写后必须重启内核 —— v2ray 不会热加载 conf。因此这个 op 比较重:
// 面板应只在用户名单真的变化时下发, 不要定时轮询式调用。
func (o *Ops) opSyncUsers(res *common.NodeOpResultPayload, p *common.NodeOpPayload) {
	users := p.Users
	if users == nil {
		users = []common.UserCredential{}
	}

	r, err := o.Driver.SyncUsers(users)
	if err != nil {
		res.OK = false
		res.Error = "同步用户失败: " + err.Error()
		return
	}
	if r == nil {
		r = &SyncUsersResult{}
	}

	res.Steps = append(res.Steps, common.StepInfo{
		Name:   "写入 clients[]",
		OK:     len(r.Errors) == 0,
		Detail: fmt.Sprintf("改动 %d 个配置, 未变 %d, 新增 %d, 移除 %d", len(r.Changed), len(r.Unchanged), r.Added, r.Removed),
	})
	for _, e := range r.Errors {
		res.Steps = append(res.Steps, common.StepInfo{Name: "配置错误", OK: false, Detail: e})
		res.Warnings = append(res.Warnings, e)
	}

	// 名单无论如何都要记下来 —— 它是"基线里没有这个 email 时算不算增量"的判据
	// (见 UserTrafficTracker.Collect)。放在"是否重启"判断**之前**, 因为
	// 名单没变时同样需要刷新它 (比如面板新增了用户但该用户的 conf 条目
	// 早就在, 或者 agent 刚重启过)。
	if o.UserTraffic != nil {
		emails := make([]string, 0, len(users))
		for _, u := range users {
			if e := strings.TrimSpace(u.Email); e != "" {
				emails = append(emails, e)
			}
		}
		o.UserTraffic.SetKnownUsers(emails)
	}

	if len(r.Changed) == 0 {
		// 没有实际改动就不重启 —— 重启会掐断所有在线连接, 不能白做。
		res.OK = len(r.Errors) == 0
		res.Output = "名单已是最新, 未改动任何配置"
		return
	}

	// 有改动: 重启内核让 v2ray 重新读取 conf。
	steps, pid, rerr := o.Driver.RestartServices()
	res.Steps = append(res.Steps, steps...)
	if rerr != nil {
		res.OK = false
		res.Error = "配置已写入但内核重启失败: " + rerr.Error()
		return
	}

	res.OK = len(r.Errors) == 0
	res.Output = fmt.Sprintf("已同步 %d 个用户, 改动 %d 个配置, v2ray pid=%d",
		len(users), len(r.Changed), pid)

	// 重启清空了计数器 —— 两个跟踪器都要重建基线。
	// 名单已在上面记过, 这里只重建基线。
	if o.UserTraffic != nil {
		o.UserTraffic.Resync()
	}
	if o.Traffic != nil {
		o.Traffic.Resync()
	}
}

func (o *Ops) opRestart(res *common.NodeOpResultPayload) {
	steps, pid, err := o.Driver.RestartServices()
	res.Steps = steps
	if err != nil {
		res.OK = false
		res.Error = err.Error()
		return
	}
	res.OK = true
	res.Output = fmt.Sprintf("v2ray pid=%d", pid)
	// 重启会清空 StatsService 计数器 —— 必须重置基线, 否则下一次上报
	// 会把"归零"当成负数, 或把重新累积的值当成新增量。
	if o.Traffic != nil {
		o.Traffic.Resync()
	}
}

func (o *Ops) opProbe(res *common.NodeOpResultPayload, p *common.NodeOpPayload) {
	name := strings.TrimSpace(p.ConfName)
	if name == "" {
		res.OK = false
		res.Error = "probe 缺少 conf_name"
		return
	}
	snap, err := o.Driver.Snapshot()
	if err != nil {
		res.OK = false
		res.Error = "采集失败, 无法定位节点: " + err.Error()
		return
	}
	n := snap.FindNode(name)
	if n == nil {
		res.OK = false
		res.Error = "未找到配置: " + name
		return
	}
	timeout := time.Duration(p.ProbeTimeoutSec) * time.Second
	pr := o.Driver.ProbeNode(n, timeout)
	res.ConfName = n.ConfName
	res.OK = pr.Reachable
	res.Output = formatProbe(n, pr)
	res.Steps = []common.StepInfo{{Name: "公网 TLS+WS 探测", OK: pr.Reachable, Detail: res.Output}}
	if !pr.Reachable {
		res.Error = "节点公网不可达"
		if pr.Err != "" {
			res.Error += ": " + pr.Err
		}
	}
}

func (o *Ops) opLogs(res *common.NodeOpResultPayload, p *common.NodeOpPayload) {
	lines := p.Lines
	if lines <= 0 {
		lines = 200
	}
	content, err := o.Driver.ReadLogs(p.ConfName, lines)
	if err != nil {
		res.OK = false
		res.Error = err.Error()
		res.Output = content
		return
	}
	res.OK = true
	res.Output = content
	res.Steps = []common.StepInfo{{
		Name: "读取日志", OK: true,
		Detail: fmt.Sprintf("来源 %s, %d 行", p.ConfName, lines),
	}}
}

// ---------------------------------------------------------------------------
// 结果翻译
// ---------------------------------------------------------------------------

// applyCreateResult 把 driver 的 CreateResult 摊平进线格式。
func applyCreateResult(res *common.NodeOpResultPayload, r *CreateResult) {
	res.ConfName = r.ConfName
	res.InternalPort = r.InternalPort
	res.PublicPort = r.PublicPort
	res.Path = r.Path
	res.URI = r.URI
	res.RolledBack = r.RolledBack
	res.Warnings = append(res.Warnings, r.Warnings...)
	res.Steps = make([]common.StepInfo, 0, len(r.Steps))
	for _, s := range r.Steps {
		res.Steps = append(res.Steps, common.StepInfo{
			Name:       s.Name,
			OK:         s.OK,
			Detail:     s.Detail,
			DurationMs: s.Duration.Milliseconds(),
		})
	}
	// 成败只认 driver 判定的事实链 (Create 的 OK 由 conf 是否真的落盘决定;
	// Delete 的 OK 由 conf 是否真的消失决定 —— 都不看退出码)。
	res.OK = !r.RolledBack && allStepsOK(r.Steps)
	if !res.OK && res.Error == "" {
		for _, s := range r.Steps {
			if !s.OK {
				res.Error = s.Name + " 失败"
				if s.Detail != "" {
					res.Error += ": " + s.Detail
				}
				break
			}
		}
		if res.RolledBack {
			res.Error = "操作失败, 已回滚半成品配置"
		}
	}
}

func allStepsOK(steps []Step) bool {
	for _, s := range steps {
		if !s.OK {
			return false
		}
	}
	return len(steps) > 0
}

func formatProbe(n *NodeView, pr ProbeResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "目标: %s:%d%s\n", firstNonEmptyStr(n.Address, n.CaddyHost), n.PublicPort, firstNonEmptyStr(n.Path, n.CaddyPath))
	fmt.Fprintf(&b, "可达: %v", pr.Reachable)
	if pr.StatusCode > 0 {
		fmt.Fprintf(&b, "  HTTP 状态: %d", pr.StatusCode)
	}
	if pr.Protocol != "" {
		fmt.Fprintf(&b, "  协议: %s", pr.Protocol)
	}
	b.WriteString("\n")
	if pr.ServerName != "" {
		fmt.Fprintf(&b, "SNI: %s\n", pr.ServerName)
	}
	if pr.CertCN != "" {
		fmt.Fprintf(&b, "证书 CN: %s (签发: %s)\n", pr.CertCN, pr.CertIssuer)
	}
	if !pr.CertNotAfter.IsZero() {
		fmt.Fprintf(&b, "证书到期: %s\n", pr.CertNotAfter.Format("2006-01-02 15:04:05"))
	}
	if pr.Err != "" {
		fmt.Fprintf(&b, "错误: %s\n", pr.Err)
	}
	return strings.TrimRight(b.String(), "\n")
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 流量增量跟踪
// ---------------------------------------------------------------------------

// TrafficTracker 把 v2ray 的**累计**计数器转成面板要的**增量**。
//
// 233boy 没有 per-user 计量 —— 计数器是 per-inbound-TAG 的。
// 所以"某账号用了多少"只能在面板侧按订阅账号累加节点增量, 这里只负责产出增量。
type TrafficTracker struct {
	runner *common.Runner

	mu   sync.Mutex
	prev map[string]common.StatsCounter // key: conf 名
	// 上一轮是否读失败。失败后本轮即便成功也只用来重建基线, 不上报 ——
	// 否则会把"停了 3 分钟的流量"当成一次 3 分钟间隔的巨额增量。
	stale bool
}

// NewTrafficTracker 构造跟踪器。
func NewTrafficTracker(r *common.Runner) *TrafficTracker {
	return &TrafficTracker{runner: r, prev: map[string]common.StatsCounter{}, stale: true}
}

// Resync 丢弃基线。建/删节点、重启内核之后必须调用:
// 重启会把计数器清零, 不重置基线就会算出负数或被钳成 0, 白白丢掉一段真实流量。
func (t *TrafficTracker) Resync() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prev = map[string]common.StatsCounter{}
	t.stale = true
}

// Collect 读一次计数器并返回相对上一次的增量。
//
// 返回 nil 表示"本轮不上报": 首次采样 (没有基线) 或上一轮读失败后的重建。
// 增量恒 >= 0: 计数器归零 (内核重启) 时钳到 0, 绝不产生负数。
func (t *TrafficTracker) Collect() []common.InboundDeltaTraffic {
	cur, _, err := t.runner.Stats()

	t.mu.Lock()
	defer t.mu.Unlock()

	if err != nil {
		// 读失败: 保留旧基线 (v2ray 可能只是短暂不可用), 但要标记 stale,
		// 让恢复后的第一轮只重建基线而不上报。
		t.stale = true
		return nil
	}

	next := make(map[string]common.StatsCounter, len(cur))
	for _, c := range cur {
		next[c.ConfName] = c
	}

	// 首次 / 刚重建基线 / 刚重启过: 只记基线, 不上报。
	if t.stale || t.prev == nil {
		t.prev = next
		t.stale = false
		return nil
	}

	out := make([]common.InboundDeltaTraffic, 0, len(cur))
	for name, c := range next {
		p, ok := t.prev[name]
		if !ok {
			// 新出现的 conf (刚建的节点): 它的累计值不是本轮增量, 只记基线。
			continue
		}
		up := delta(c.Up, p.Up)
		down := delta(c.Down, p.Down)
		if up == 0 && down == 0 {
			continue
		}
		out = append(out, common.InboundDeltaTraffic{ConfName: name, Up: up, Down: down})
	}

	t.prev = next
	sort.Slice(out, func(i, j int) bool { return out[i].ConfName < out[j].ConfName })
	return out
}

// delta 累计值差分。计数器归零 (重启/回绕) 时返回 0。
func delta(cur, prev int64) int64 {
	if cur <= prev {
		return 0
	}
	return cur - prev
}

// ---------------------------------------------------------------------------
// 按用户流量跟踪
// ---------------------------------------------------------------------------

// UserTrafficTracker 把 `user>>>EMAIL>>>...` 的累计值转成增量。
//
// 与 TrafficTracker 完全独立: 那个按 conf 聚合 (机器视角), 这个按 email
// (账号视角)。两者共用同一次 `v2ray xapi stats -json` 的输出, 但基线必须分开记 ——
// 合成一个结构会让「某个 conf 被删」和「某个用户被摘掉」互相干扰。
type UserTrafficTracker struct {
	runner *common.Runner

	mu    sync.Mutex
	prev  map[string]common.UserStatsCounter // key: email
	known map[string]bool                    // 面板下发的用户名单 (email)
	stale bool
}

func NewUserTrafficTracker(r *common.Runner) *UserTrafficTracker {
	return &UserTrafficTracker{
		runner: r,
		prev:   map[string]common.UserStatsCounter{},
		known:  map[string]bool{},
		stale:  true,
	}
}

// SetKnownUsers 记录面板下发的用户名单。
//
// 这个名单是「基线里没有这个 email 时该怎么算」的判据 —— 详见 Collect 里的注释:
// 名单里有 = 用户刚开始产生流量, 那一段 0→N 是真实增量, 必须记账;
// 名单里没有 = 真的新 client, 累计值不是增量, 丢弃。
//
// 必须在 Resync **之前**调用: Resync 会清空基线, 紧接着的第一次 Collect
// 就要用这个名单来判断。
func (t *UserTrafficTracker) SetKnownUsers(emails []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	m := make(map[string]bool, len(emails))
	for _, e := range emails {
		if e = strings.TrimSpace(e); e != "" {
			m[e] = true
		}
	}
	t.known = m
}

// Resync 丢弃基线。同步用户名单/重启内核之后必须调用。
func (t *UserTrafficTracker) Resync() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prev = map[string]common.UserStatsCounter{}
	t.stale = true
}

// Collect 读一次并返回相对上次的增量。nil 表示本轮不上报 (首次/重建基线/读失败)。
func (t *UserTrafficTracker) Collect() []common.UserStatsCounter {
	cur, err := t.runner.UserStats()

	t.mu.Lock()
	defer t.mu.Unlock()

	if err != nil {
		t.stale = true
		return nil
	}

	next := make(map[string]common.UserStatsCounter, len(cur))
	for _, c := range cur {
		next[c.Email] = c
	}
	return t.applyLocked(next)
}

// apply 是增量算法本体, 与 Runner 解耦以便单测 (见 usertraffic_test.go)。
// 入参是"本轮 stats 里出现的 email → 累计值"; V2Ray 对零值计数器整条不输出,
// 所以**没流量过的用户不会出现在里面** —— 这正是那段 0→N 容易丢的根源。
func (t *UserTrafficTracker) apply(next map[string]common.UserStatsCounter) []common.UserStatsCounter {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.applyLocked(next)
}

func (t *UserTrafficTracker) applyLocked(next map[string]common.UserStatsCounter) []common.UserStatsCounter {
	if t.stale || t.prev == nil {
		t.prev = next
		t.stale = false
		return nil
	}

	out := make([]common.UserStatsCounter, 0, len(next))
	for email, c := range next {
		p, ok := t.prev[email]
		if !ok {
			// 基线里没有这个 email —— 有两种可能, 后果完全不同:
			//
			//   (a) 刚同步进去的新用户, 它的累计值不是本轮增量 → 应当丢弃;
			//   (b) **零流量用户开始产生流量**。V2Ray 对 value 为 0 的计数器
			//       整条不输出 (实测 `xapi stats -json`: 只有流过量的 user>>> 条目
			//       才存在), 所以基线里根本没这个 email, 而 cur 里的是
			//       0 → N 的**真实增量**。
			//
			// 早期实现一律按 (a) 处理 (直接 continue), 结果 (b) 里那一段流量
			// 被静默丢掉 —— 表现为"用户跑了几百 MB, 面板显示 0"。因为用户
			// 往往是在一段时间内连续使用, 而丢失只发生在"从无到有"的那一轮,
			// 所以看起来像是"流量偶尔少记一点", 极难定位。
			//
			// 正确的判据不是"在不在基线里", 而是"上一轮它在不在**名单**里"。
			// 名单 = 面板下发的用户集合, tracker 在 Resync 时拿得到。
			if !t.known[email] {
				// 名单里也没有 → 真的是新用户 (或手工加的第三方 client),
				// 它的累计值不是增量, 丢弃。
				continue
			}
			// 在名单里但基线没值 → 从 0 开始算。
			p = common.UserStatsCounter{}
		}
		up := delta(c.Up, p.Up)
		down := delta(c.Down, p.Down)
		if up == 0 && down == 0 {
			continue
		}
		out = append(out, common.UserStatsCounter{Email: email, Up: up, Down: down})
	}

	t.prev = next
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out
}
