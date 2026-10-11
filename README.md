# v2panel

基于 [233boy/v2ray](https://github.com/233boy/v2ray) 的**个人自建 VPN 订阅管理面板**。

一个 Go 单二进制 + 反向 WebSocket agent，把散在多台机器上的 233boy 节点收拢到一个面板里：
加节点、改协议、看流量、发订阅链接，不用再逐台 SSH 敲交互式菜单。

发给用户的订阅页自带图文导入教程，用户不需要懂技术。

---

## 它解决什么问题

233boy 的一键脚本很好用，但它只有 CLI：

- 加个节点要 SSH 上去敲交互式菜单，机器一多就受不了
- 看流量要 `v2ray xapi stats`，还得自己算
- 发订阅要手工拼链接，用户拿到一串 URL 不知道往哪贴
- 同机多节点共享 Caddy 端口、靠 path 区分，手工管理极易搞错

v2panel 把这些变成点几下：

| 原来的做法 | 现在 |
|---|---|
| SSH 进每台机器跑 `v2ray` 菜单 | 面板上点「新建协议」，agent 在目标机上执行 |
| 手工 `v2ray xapi stats` 算流量 | 面板自动采集，出曲线和排行 |
| 手工拼 `vless://` 链接 | 自动生成，UA 自适应返回 Clash / sing-box / Base64 |
| 把链接发给用户，用户问怎么用 | 发订阅页地址，页面上有四个平台的图文教程 |

---

## 套餐与真限流

### 套餐（Plan）

套餐是**开号模板**：流量额度 / 有效期 / 设备数 / 节点白名单 / 重置方式。

用户**继承**套餐的值 —— 但继承的是**快照**，不是引用。开号时把套餐的值抄进用户，
之后两者各自独立演进。这样改套餐不会悄悄改掉已开用户的额度
（用户以为买的是 100G，结果被降到 50G，那是最招骂的一类变更）。

想同步到存量用户，用套餐页的「同步到用户」按钮，显式选择要覆盖哪几项。

### 流量重置

三种模式，存在套餐上、开号时快照进用户：

| 模式 | 行为 |
|---|---|
| `monthly` | 每月 1 号 0 点流量归零（默认） |
| `cycle` | 按到期日重置 —— 4 号到期则每月 4 号归零 |
| `none` | 不重置，一次性流量包，用完为止 |

判定依据是 **`LastResetAt`（上次重置时间）** 而不是「下次重置时间」：
后者在改到期日/改套餐时会失效，需要额外同步逻辑；而「上次什么时候重置的」
是既成事实，判据永远自洽。面板重启会补跑一次（幂等）。

### 真限流（这是本项目的核心）

**早期版本只能按订阅账号数分摊整机流量，那不叫限制。** 现在是真的：

```
面板下发全量用户名单
  → agent 把每个用户的 UUID 写进 233boy conf 的 clients[]，并打上 email 标记
  → V2Ray 按 email 单独出计数器（user>>>EMAIL>>>traffic>>>uplink）
  → agent 读增量 → 面板累加到该账号
  → 超额 → 标记 Locked → 下次下发时 agent 把该 UUID 从 clients[] 摘掉
  → 客户端立刻连不上
```

实测证据（82 上跑的真实 conf）：

```
VLESS-WS-TLS-node1.example.com.json: 3 个 client
   [0] email=(无)           id=11111111-...   ← 233boy 原始 client，永不改动
   [1] email=u1@v2panel     id=aaaaaaaa-...   ← 面板用户
   [2] email=u7@v2panel     id=92aea27c-...   ← 面板用户
```

用户超额后，`u7` 那一项会从数组里消失 —— 这才是真的断线。

#### 三条不能违反的约束

1. **`clients[0]` 永远不许动。** `v2ray url` 取的是 `clients[0].id` 生成客户端链接，
   而面板的地址/path 推导完全依赖那份输出。动它会让该节点的订阅链接整体错乱。
   订阅用户一律从 `clients[1]` 起追加。

2. **只碰带 `@v2panel` 后缀的 email。** 手工加进 conf 的第三方 client 不会被误删。

3. **改写前必须备份。** 每个被改的 conf 会留一份 `<name>.json.v2panel-bak`，
   出问题可以手工恢复。

#### 为什么走「直接改 conf」而不是 233boy 命令

233boy 的 `add()` 只会往 conf 写**一个** client，没有多用户接口。
但 V2Ray 的 `settings.clients` 本身就是数组，`policy.levels.0.statsUserUplink/Downlink`
一开就按 email 出计数器（实测 82 上这两个开关本来就是开的，只是从来没人放过第二个用户）。
所以直接增删 `clients[]` 是唯一可行的真限流路径 —— **这不修改 `/etc/v2ray/sh/`**，
只改它生成出来的 conf，233boy 升级不会覆盖。

#### 判定与执行分离

```
EnforceUserQuotas   只看数据库，决定谁该被锁   (高频，便宜)
PushUserSyncAll     把「谁现在能用」推到节点   (低频，要重启内核)
```

分开的理由：入账是每 30 秒一次的高频路径，而下发要重启内核（会掐断所有在线连接）。
只有真的有人状态翻转时才重启。

**触发时机**：流量入账后、管理员改配额后、删号后、重置流量后、重新生成凭据后、
**节点每次接入时**、每 60 秒巡检一次。下发有 1.5 秒防抖 —— 批量导入 100 个用户只会下发一次。

节点接入时重下发是必须的：机器重启 / conf 被人手工还原之后，`clients[]` 里的订阅用户
就没了，而面板这边毫无察觉。同步是幂等的（名单一致时 agent 直接返回，不写盘不重启内核）。

#### 下发给用户的链接必须重绑凭据（最容易漏的一环）

`v2ray url` 输出的链接里，凭据是 **`clients[0].id`** —— 233boy 自己那个 UUID。
直接把它下发给所有用户的话：

```
用户拿到的 UUID = clients[0]        (无 email)
conf 里 u1@v2panel 的 UUID = 另一个

客户端用 clients[0] 鉴权 → V2Ray 归到"无 email"的匿名桶
→ user>>>u1@v2panel>>>traffic>>>uplink 永远是 0
→ UsedTraffic 永不增长 → 没人超额 → 没人被熔断
```

**流量能连，但记不到任何人头上。** 所以 `agent.RebindShareURI` 会在下发前按用户
重绑凭据（vless/trojan 换 userinfo、vmess 换 base64 JSON 里的 `id`、ss 换 password），
其余字段（地址/端口/path/host/sni/tag/加密方式）一律原样保留 —— 那些是 233boy 与
Caddy 层的事实。

> 注意：233boy 把 UUID 也用作了 WS 的 `path`（`path=%2F<uuid>`）。那个 path 是
> **节点级**的、所有用户共用，必须保留。判断"凭据换没换"要看解析后的 `UUID` 字段，
> 不能对整个 URL 做子串匹配 —— 旧 UUID 会合法地出现在 path 里。

`socks` / `http` 在 conf 里用 `accounts[]` 而不是 `clients[]`，agent 的用户同步会跳过它们
（机器上没有"每用户一条凭据"的概念）。这类线路**直接排除在订阅之外** ——
发出去只会让用户以为自己有条能计量的线路，实际记不到账。

#### `xapi stats` 恒返回退出码 1（233boy v4.27 自身的 bug）

`/etc/v2ray/sh/src/core.sh:846` 的 `api()` 结尾是：

```bash
$is_core_bin api $is_api_do --server=127.0.0.1:$is_api_port ${@:2}
[[ $? != 0 ]] && {
    is_api_fail=1
}
```

核心命令 exit 0、stdout 输出完全正确的 JSON，但那个 `[[ ]]` 测试为假 →
**整个函数返回 1**，于是 `v2ray xapi stats -json` 恒返回退出码 1。

如果调用侧严格检查退出码（`Runner.exec` 原本就是），`Stats()` / `UserStats()` 会一律
返回 error → **agent 什么都不上报** → 面板流量永远是 0、曲线永远空白、限流永远不触发。
不是"偶尔少记"，是整条计量链路从未工作过。

红线是不许 patch `/etc/v2ray/sh/`（升级会覆盖，且属于注入），所以修在调用侧：
`Runner.execLenientJSON` —— **只要 stdout 里能解析出合法 JSON 就认为成功**，
判据是产物而不是退出码。只给只读命令用；写操作继续严格检查退出码，
否则会把"改配置失败"当成成功（那才是真正危险的静默失败）。

#### 零流量用户的第一段流量（另一个静默丢数据的地方）

V2Ray 对 value 为 0 的计数器**整条不输出**。所以一个还没跑过流量的用户在
`xapi stats -json` 里根本不存在，跑出第一段流量后才第一次出现。

早期实现把所有"基线里没有的 email"都当成新 client 丢弃，于是那一段 `0 → N`
被静默吞掉。因为用户通常连续使用、丢失只发生在从无到有的那一轮，
现象像是"偶尔少记一点"，极难定位。

正确的判据不是"在不在基线里"，而是"上一轮它在不在**名单**里"：
`UserTrafficTracker.known` 由面板下发的用户名单维护，agent 启动时还会从 conf 里
反推一次（`managedEmailsFromConf`），补上面板还没推名单的那个窗口。

#### 链路保活（否则每 80 秒掉一次线）

agent 原本只设了 `SetPongHandler` 来续命读超时，但 **agent 自己从不发 ping** ——
是面板在 ping、agent 在回 pong，所以那个 handler 永远不触发。

实测（tcpdump 抓 127.0.0.1:9000 的明文帧）：

```
SRV→AGT len=2   (ping)
AGT→SRV len=6   (pong)      ← 确实回了, 但 handler 不会被调用
... 75 秒后 ...
AGT→SRV FIN                 ← agent 自己断开, 重连, 循环
```

`websocket` 库只在**读循环里读到** pong 才回调 PongHandler，而 `readLoop` 只在收到
**数据消息**时重置读超时，面板除了 3 分钟一次的 `inventory_req` 几乎不发数据。
于是 75 秒必然超时 → 节点在面板上每 80 秒闪一次 offline。

修法：agent 自己也发 ping（`keepaliveLoop`，周期 `readTimeout/3`），
这样读循环会收到 pong 并续命。

### 设备数限制

套餐上的 `device_limit` 会快照进用户并在门户展示，但**当前版本不做实际拦截**。
真要做需要在 agent 侧按 IP 计数并动态封禁，那超出了「基于 233boy」的范围。
面板如实展示这个数字，不假装它能生效。

---

## 运营

管理端的「运营」页有三块：

| 板块 | 作用 |
|---|---|
| **公告** | 发「今晚维护」这类通知，显示在**用户订阅页顶部**。支持置顶、级别、展示时间窗 |
| **用户名单下发** | 手动把「谁现在能用」推到某个节点。平时不用点，排障用（机器重启后 conf 被还原） |
| **开号记录** | 审计日志：谁什么时候开的号、到期推到哪天。**面板不收款**，这里不是订单 |

节点页的「采集」按钮和这里的「下发用户名单」是**两件不同的事**：
前者是让 agent 重新读机器上的 conf/Caddy/url 把清单拉回面板，
后者是把面板的用户名单推到机器。早前那个按钮叫「同步」，与用户同步混在一起，
排查"用户超额了还能连"时很容易点错地方，所以改了名字。


---

## 快速开始

### 1. 装面板

在**要跑面板的机器**上（建议选一台线路好的 VPS）以 root 执行：

```bash
# 先看一眼脚本再跑 —— 它是以 root 执行的
curl -fsSL -o install.sh https://raw.githubusercontent.com/<你的仓库>/main/install.sh
less install.sh
bash install.sh --domain panel.example.com
```

不带 `--domain` 就只装面板，用 `http://<IP>:9000/<安全入口>` 访问，之后再补域名。

装完会打印：

```
• 安全入口路径: /a1b2c3d4e5f60718
• 域名外网入口: https://panel.example.com/a1b2c3d4e5f60718
• 默认管理账号: admin
```

> **⚠️ 第一次登录后立刻改密码。** 默认是 `admin123456`，安全入口只能防扫描器，
> 防不住知道你域名的人。
>
> ```bash
> /opt/v2panel/v2panel -set-password '<新密码>' -db /opt/v2panel/data/v2panel.db
> ```

### 2. 加节点

面板里 **节点 → 新建节点**：

- **名称**：自己认得出的标识，如 `hk-01`
- **地址**：客户端要连的域名（TLS 协议必填，且要已解析到那台机器）
- **保护名单**：**这台机器上已经在跑的线上节点，把它的配置名关键片段填进去**

> **关于保护名单** —— 这是最容易出事的地方。
>
> 面板对节点是「纳管」不是「拥有」，但它确实能删机器上的 233boy 配置。
> 如果你把一台**正在服务用户**的机器纳管进来却不填保护名单，面板就有了删光它全部配置的能力。
>
> 填 `tzq1` 就能保护 `VLESS-WS-TLS-node1.example.com.json`（子串匹配）。
> 多个用逗号分隔：`tzq1,hs`。
>
> 全新的机器留空即可。

创建后面板给出**一键纳管命令**：

```bash
curl -fsSL https://panel.example.com/install.sh?token=xxxx | bash
```

### 3. 在节点机上执行

把上面那条命令在**目标机**上以 root 执行。它会：

1. 装 233boy/v2ray（已有则跳过）
2. 下载 agent 二进制到 `/usr/local/bin/v2panel-agent`
3. 写 `/etc/v2panel-agent/config.json`（含 Token 和保护名单）
4. 装 systemd 服务并启动

agent 会主动连回面板（反向 WebSocket），所以**目标机不需要公网端口、不需要能被连**。
节点状态变成「在线」就成功了。

### 4. 发号给用户

**订阅 → 新建账号**，填账号名、流量配额、到期时间，创建后会显示订阅地址：

```
https://panel.example.com/sub/8524a2a6a0fefb20845cd7fc6c2c8caa
```

**把这条地址直接发给用户就行。** 用户打开会看到：

- 一个大按钮「复制你的订阅地址」
- 四个客户端（Clash Verge / v2rayN / v2rayNG / Shadowrocket）的下载地址和分步教程
- 他的流量用量、剩余、到期时间
- 每条线路的独立链接和二维码

用户把地址粘进客户端就完事了。

---

## 订阅页给用户看什么

订阅页 (`/sub/<token>`) 是给**不懂技术的人**看的，它自己回答完这四个问题：

1. **我该下哪个 App** → 按平台列出四个主流客户端 + 官网下载地址
2. **下好之后点什么** → 每个客户端 3–4 步图文说明
3. **我的链接在哪** → 顶部一键复制按钮
4. **我还能用多久** → 流量条 / 到期时间 / 状态

**默认不需要密码** —— 订阅 Token 本身就是 128 位随机凭据，客户端里裸粘 `/sub/<token>` 直接成功。
这是刻意的：早期版本强制密码，用户粘进 Clash Verge 只会拿到 401，而界面上没有任何地方
告诉他要加 `?pwd=`，这是最劝退的一处。

如果你担心订阅链接被转发，可以在面板里开启「订阅密码保护」，
开启后门户要求输密码、链接要带 `?pwd=xxx`。

订阅格式按 User-Agent 自动分流：

| 客户端 | 返回格式 |
|---|---|
| Clash / Mihomo / Verge / Stash … | Clash YAML |
| sing-box | sing-box JSON |
| 其它（v2rayN / v2rayNG / Shadowrocket …） | Base64 链接列表 |

也可以强制指定：`?type=clash` / `?type=singbox` / `?type=base64`。

---

## 架构

```
        ┌──────────────────────────┐
        │  面板 (Go 单二进制)       │
        │  ┌────────┐ ┌──────────┐ │
        │  │ HTTP   │ │ WS hub   │ │
        │  └────────┘ └──────────┘ │
        └───────┬──────────┬───────┘
                │          │
        订阅 /sub/<token>  │  反向 WebSocket /ws/agent
                │          │  (agent 主动拨上来)
                ▼          ▼
           ┌────────┐  ┌─────────────────────────┐
           │  用户  │  │ 节点机 A                 │
           └────────┘  │  v2panel-agent          │
                       │    └── 233boy v2ray     │
                       │    └── Caddy            │
                       └─────────────────────────┘
```

**为什么用反向 WebSocket**：节点机常常在 NAT 后面，或者你没有它的公网入口。
让 agent 主动拨面板，就不需要每台机器都开放端口、也不需要逐节点反代。

### 面板 / agent 分工

| | 面板 | agent |
|---|---|---|
| 存数据 | 节点/用户/流量/配置清单 (SQLite) | 无状态 |
| 面向 | 管理员浏览器 + 用户订阅页 | 面板 |
| 碰 233boy | 不碰 | 唯一碰的一方 |
| 关键约束 | 不打印明文 Token | 保护名单内的配置拒绝改动 |

agent 是唯一接触 233boy 的组件。所有破坏性操作（增删协议）都在 agent 侧过 `guard()`，
命中保护名单直接拒绝。

---

## 常用命令

```bash
# 面板状态与日志
systemctl status v2panel
journalctl -u v2panel -f

# ⚠️ 所有 CLI 都要带 -db 绝对路径
# 不带的话它会按当前目录找 ./data/v2panel.db, 找不到就新建一个空库,
# 于是你看到的入口/账号全是假的。程序现在会直接报错拦住这种情况。
DB=/opt/v2panel/data/v2panel.db

/opt/v2panel/v2panel -entry          -db $DB   # 查看安全入口
/opt/v2panel/v2panel -nodes          -db $DB   # 列出所有节点与协议实例
/opt/v2panel/v2panel -url            -db $DB   # 列出所有订阅链接
/opt/v2panel/v2panel -set-password X -db $DB   # 改密码
/opt/v2panel/v2panel -set-entry /新路径 -db $DB # 改安全入口
/opt/v2panel/v2panel -reset-admin    -db $DB   # 重置密码为默认值

# 节点侧
systemctl status v2panel-agent
journalctl -u v2panel-agent -f
v2ray                                          # 233boy 交互菜单
```

---

## 目录结构

```
v2panel/
  cmd/panel/main.go          面板入口 (CLI 子命令 + HTTP + hub)
  cmd/agent/main.go          agent 入口 (反向拨号 + 驱动 233boy)
  internal/common/           WS 消息契约 + 233boy CLI 安全封装
  internal/panel/            模型 / 路由 / hub / 鉴权 / 订阅渲染 / 节点 API
    sub_portal.html          ★ 用户订阅页 (服务端渲染, 给最终用户看的)
  internal/agent/            233boy 驱动器 / 三层解析 / 流量采集 / 硬件指标
  web/                       ★ 管理端前端 (Vue 3 + Element Plus + Vite)
    index.html               SPA 入口
    vite.config.js           构建配置 (产物直接落到 web/static/)
    src/
      main.js                应用入口 + 路由 + 动态 base
      api.js                 接口封装与格式化工具
      App.vue                顶栏布局 + 改密 + 登录态
      styles/theme.css       Element Plus 主题覆写 (浅色简洁风)
      views/                 6 个页面 + 登录页
      components/            折线图 / 复制按钮 / 新增协议弹窗
    static/                  ← 构建产物 (Go 侧 //go:embed 的目标)
  install.sh                 一键安装
  deploy/v2panel.service     systemd 单元
```

### 前端

**管理端是 Vue 3 SPA**（Vue Router history 模式 + Element Plus），用户订阅页
（`internal/panel/sub_portal.html`）保持**服务端渲染** —— 它必须打开即用，
不依赖 JS，用户拿到的是一条链接而不是一个前端应用。

改前端：

```bash
cd web
npm install
npm run dev      # 本地开发 (需要面板后端在 127.0.0.1:9000 并配好代理)
npm run build    # 产物落到 web/static/
```

改完 `web/static/` 后**必须重新编译 Go 二进制** —— 前端是 `//go:embed` 进二进制的：

```bash
go build -o v2panel ./cmd/panel
```

#### 三个不能改错的地方

1. **`vite.config.js` 的 `base` 必须是 `/`**，不能是 `./`。
   面板首页是在安全入口路径下就地渲染的，相对 base 会把资源解析到
   `/<入口>/assets/xxx.js` 而 404。

2. **`src/main.js` 的 router base 来自 `window.__VP_BASE__`**，
   由后端在 index.html 的 `</head>` 前注入（值是安全入口路径）。
   写死 `'/'` 的话，浏览器地址栏里的 `/a1b2c3d4e5f60718` 会被当成路由去匹配 → 404 页。

3. **`scanStaticAssets()` 必须登记 `assets/` 子目录**。
   它只登记根级文件的话，`/assets/*.js` 会被安全入口拦成 HTML 拦截页 ——
   表现是「首页能打开但白屏」，且控制台只报 MIME 类型错误，很难定位。

---

## 实测过的 233boy 行为（改代码前必读）

这些都是踩过的坑，写在注释里也写在这里：

### `gen` 只在参数完整时才是安全的 dry-run

参数不全时会掉进 `ask()` 的 `while :; do read REPLY` 自旋，EOF 下空转刷屏。
三个已知自旋点：`*-tls` 缺 host、`door` 缺 addr、`door` 缺 dest_port。

**所以参数完整性由 `AddSpec.BuildArgs()` 在调用前把关，不靠 `gen` 自己兜底。**
另外执行时必须：`SysProcAttr{Setpgid:true}` + 超时后 `syscall.Kill(-pid, SIGKILL)` 杀整个进程组。

### `v2ray url` 无参不是「批量导出」

它进的是配置选择菜单。命中 ≥2 个配置就走 `ask()` 自旋。
**必须每节点一次、带锚定文件名**：`v2ray url '^<name>\.json$'`。

单节点机器上无参会「碰巧」成功 —— 这正是这个缺陷早期被漏掉的原因。

### 输出必须先去 ANSI

`v2ray url` 把链接包成 `\x1b[45mvless://…\x1b[0m`，
导致 `grep -c "^[a-z0-9]*://"` 返回 **0**。

### 公网端口是「机器级」的

Caddy 的 `https_port` / `http_port` 整机共享，**不是每节点一个公网口**。
同机多节点靠 **path** 区分。所以解析器与订阅引擎按 path 作主键，
绝不能假设「一个节点一个公网端口」。

### conf 的 `security:none` 与 URL 的 `security=tls` 是设计如此

233boy 的 `create()` 服务端分支无条件写 `security:none`，
客户端 URL 里硬编码 `security=tls`。

**这不是 bug，不要「修正」它。** 真实链路是
「客户端 TLS → Caddy(有真证书) → 明文回环到内网口」，TLS 在 Caddy 终结。

### 流量只用 `inbound>>>`

`v2ray xapi stats -json` 里：
- 只用 `inbound>>>TAG>>>traffic>>>uplink|downlink`
- **永远不用 `Total`** —— 它含 outbound，约等于真实流量的 2 倍
- 零流量条目**整条没有 `value` 字段**，解析器要容忍缺失
- 需要处理计数器重启归零（负数增量钳到 0）

### reality 在 v4.27 是死的

`add()` 的 `reality` 分支已被注释。UI 如实灰掉并说明原因，
**绝不 patch `/etc/v2ray/sh/`**（升级会覆盖，且属于注入）。

---

## 运维

### 备份

只需要备份一个文件：

```bash
cp /opt/v2panel/data/v2panel.db /somewhere/backup-$(date +%F).db
```

里面有节点 Token、用户凭据、流量历史。**它等同于全部凭证，别放公开位置。**

### 证书续签

如果用宝塔建站，续签由宝塔管。手动装的可以加 cron：

```bash
0 3 * * * certbot renew --quiet --deploy-hook "cp -f /etc/letsencrypt/live/panel.example.com/*.pem /www/server/panel/vhost/cert/panel.example.com/ && nginx -s reload"
```

### 面板为什么只监听 127.0.0.1

面板自身不做 TLS，由 nginx 终结。所以 `-bind 127.0.0.1` 是对的，
外面套一层反代就行。反代**必须**：

```nginx
proxy_http_version 1.1;
proxy_set_header Upgrade $http_upgrade;      # agent 的 WebSocket 靠这个
proxy_set_header Connection "upgrade";
proxy_set_header X-Forwarded-Proto $scheme;  # 面板靠它判断对外是 https
proxy_buffering off;                         # 否则实时日志会被攒着不发
proxy_read_timeout 3600s;                    # WS 是长连接
```

漏掉 Upgrade 头 → agent 连不上；漏掉 `proxy_buffering off` → 日志和心跳会卡。

---

## 排障

| 现象 | 原因 / 处理 |
|---|---|
| 打开根路径是 403 页 | 正常。必须带安全入口路径访问 |
| 登录后所有接口 403 | 没先过安全入口。先访问 `/<安全入口>` 拿 cookie |
| 节点一直「离线」 | 目标机 `journalctl -u v2panel-agent -f` 看；常见是 Token 错或面板地址不通 |
| 订阅粘进客户端 401 | 检查是否开了「订阅密码保护」；开了就要带 `?pwd=` |
| `-entry` 显示的入口不对 | 没带 `-db` 绝对路径，它读错库了。见上文 |
| 面板删不掉某条配置 | 它在保护名单里 —— 这是设计行为，去节点设置里改名单 |
| 加了协议但订阅里没有 | 该协议没有客户端链接（如 `door` / `local-http`），或地址/端口没采集到 |
| 用户跑了很多流量但面板显示 0 | 先看 `v2ray xapi stats -json` 里有没有 `user>>>uN@v2panel>>>` 条目。<br>没有 → 该用户的 UUID 不在 conf 里（点运营页「下发用户名单」）；<br>有但面板还是 0 → 看 agent 日志有没有 `上报用户流量失败` |
| 用户超额了却还能连 | 熔断要等一次下发（防抖 1.5s + 同步 + 重启内核）。超过 1 分钟还没断就去运营页点「下发用户名单」 |
| 节点每 ~80 秒闪一次「离线」 | agent 的链路保活没生效（自己不发 ping → 读超时）。见上文「链路保活」 |
| 订阅链接里的 UUID 与面板里的不一致 | 说明没走按用户重绑（`agent.RebindShareURI`）。`clients[0]` 的 UUID 会合法地出现在 WS `path` 里，别拿它当凭据比对 |

---

## 红线

这个项目**不做**这些事，改代码时也别加：

- 不 patch `/etc/v2ray/sh/`（233boy 升级会覆盖，且属于注入）
- 不留后门、不装驱动、不加自启动（除了平台自己的 systemd 服务）
- 不做全盘扫描、不做爆破、不压测
- 保护名单命中的配置，任何路径都不许改写

---

## 许可

个人自用项目。基于 [233boy/v2ray](https://github.com/233boy/v2ray)，遵循其原许可。
