#!/usr/bin/env bash
# =============================================================================
# v2panel 一键安装脚本
#
#   curl -fsSL https://raw.githubusercontent.com/<你>/v2panel/main/install.sh | bash
#
# 或者下载下来先看一眼再跑 (推荐, 因为你正在用 root 执行它):
#
#   curl -fsSL -o install.sh <上面的地址>
#   less install.sh
#   bash install.sh
#
# 做完的事:
#   1. 装 233boy/v2ray (带镜像回退)
#   2. 装 v2panel 面板 (单二进制 + systemd)
#   3. 起面板, 打印安全入口 + 账号密码
#   4. 可选: 用 Cloudflare API 加解析 + 签 Let's Encrypt + 建反代站点
#
# 幂等: 重复执行只覆盖二进制, 不动数据库, 不动 233boy 配置。
# =============================================================================
set -uo pipefail

# ---------------------------------------------------------------- 可调参数
V2PANEL_VERSION="${V2PANEL_VERSION:-latest}"
V2PANEL_HOME="${V2PANEL_HOME:-/opt/v2panel}"
V2PANEL_PORT="${V2PANEL_PORT:-9000}"
V2PANEL_BIND="${V2PANEL_BIND:-127.0.0.1}"

# 二进制下载源。默认从本脚本同级目录找, 找不到就报错并给出办法。
# (做成变量是为了让「面板自己下发安装脚本」和「从 GitHub 装」共用同一份逻辑)
V2PANEL_PANEL_URL="${V2PANEL_PANEL_URL:-}"
V2PANEL_AGENT_URL="${V2PANEL_AGENT_URL:-}"

DOMAIN=""            # --domain vp.example.com
CF_MAIL=""           # --cf-mail
CF_KEY=""            # --cf-key
CF_ADD_DNS=0         # --cf-dns
SKIP_233BOY=0
NO_SSL=0

# ---------------------------------------------------------------- 输出
if [[ -t 1 ]]; then
  R=$'\033[31m'; G=$'\033[32m'; Y=$'\033[33m'; C=$'\033[36m'; B=$'\033[1m'; N=$'\033[0m'
else
  R=""; G=""; Y=""; C=""; B=""; N=""
fi
ok()   { printf '%s✓%s %s\n' "$G" "$N" "$*"; }
warn() { printf '%s!%s %s\n' "$Y" "$N" "$*"; }
err()  { printf '%s✗%s %s\n' "$R" "$N" "$*" >&2; }
info() { printf '%s•%s %s\n' "$C" "$N" "$*"; }
hr()   { printf '%s\n' "────────────────────────────────────────────────────────"; }

die() { err "$*"; exit 1; }

usage() {
  cat <<'USAGE'
v2panel 一键安装

用法:
  bash install.sh [选项]

选项:
  --domain <域名>      面板对外域名, 如 vp.example.com。给了就自动建站 + 签证书。
  --cf-mail <邮箱>     Cloudflare 账号邮箱 (自动加解析用)
  --cf-key <API Key>   Cloudflare Global API Key (自动加解析用)
  --cf-dns             用上面的凭据自动添加 A 记录
  --port <端口>        面板监听端口 (默认 9000)
  --home <目录>        安装目录 (默认 /opt/v2panel)
  --skip-233boy        跳过 233boy 安装 (机器上已经有了)
  --no-ssl             不申请证书, 只用 HTTP
  -h, --help           显示本帮助

不带 --domain 时: 只装面板, 用 http://<IP>:<端口>/<安全入口> 访问。
装了之后随时可以再跑一次带 --domain 的, 把域名和证书补上。
USAGE
}

# --help 必须早于 root 检查, 否则普通用户想看帮助会被拦下
for a in "$@"; do
  case "$a" in
    -h|--help) usage; exit 0 ;;
  esac
done

while [[ $# -gt 0 ]]; do
  case "$1" in
    --domain)     DOMAIN="${2:-}"; shift 2 ;;
    --cf-mail)    CF_MAIL="${2:-}"; shift 2 ;;
    --cf-key)     CF_KEY="${2:-}"; shift 2 ;;
    --cf-dns)     CF_ADD_DNS=1; shift ;;
    --port)       V2PANEL_PORT="${2:-}"; shift 2 ;;
    --home)       V2PANEL_HOME="${2:-}"; shift 2 ;;
    --skip-233boy) SKIP_233BOY=1; shift ;;
    --no-ssl)     NO_SSL=1; shift ;;
    -h|--help)    usage; exit 0 ;;
    *)            shift ;;
  esac
done

# ---------------------------------------------------------------- 前置检查
hr
printf '%s  v2panel 一键安装%s\n' "$B" "$N"
hr

[[ "$(id -u)" == "0" ]] || die "请以 root 运行 (sudo -i 后重试)"

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) die "不支持的架构: $ARCH" ;;
esac
info "系统: $(. /etc/os-release 2>/dev/null && echo "$PRETTY_NAME" || uname -s) / $ARCH"

command -v curl >/dev/null 2>&1 || die "缺少 curl, 请先安装: apt install -y curl"

# 内存检查 —— 面板本身很轻, 但 233boy 安装会解压, 512M 以下给个提示
MEM_MB=$(awk '/MemTotal/{printf "%d", $2/1024}' /proc/meminfo 2>/dev/null || echo 0)
if [[ "$MEM_MB" -gt 0 && "$MEM_MB" -lt 400 ]]; then
  warn "可用内存仅 ${MEM_MB}MB, 建议先加 swap 再装"
fi

# 找一个能用的镜像前缀 (233boy 官方脚本直连 GitHub, 国内会挂)
pick_mirror() {
  for m in "" "https://ghproxy.net/" "https://gh-proxy.com/" "https://ghfast.top/"; do
    if curl -fsSL --connect-timeout 6 --max-time 20 "${m}https://github.com/233boy/v2ray/raw/master/install.sh" -o /dev/null 2>/dev/null; then
      printf '%s' "$m"; return 0
    fi
  done
  return 1
}

# ---------------------------------------------------------------- 1. 233boy
if [[ "$SKIP_233BOY" == "1" ]]; then
  info "按参数跳过 233boy 安装"
elif [[ -x /usr/local/bin/v2ray && -d /etc/v2ray/sh ]]; then
  ok "已检测到 233boy v2ray, 跳过"
  info "  版本: $(/usr/local/bin/v2ray version 2>/dev/null | head -1)"
else
  info "正在安装 233boy v2ray (会尝试镜像回退) ..."
  MIRROR="$(pick_mirror)" || die "所有下载源都不可达, 请检查网络后重试"
  [[ -n "$MIRROR" ]] && info "  使用镜像: $MIRROR"
  if ! curl -fsSL --connect-timeout 10 --max-time 300 \
       "${MIRROR}https://github.com/233boy/v2ray/raw/master/install.sh" -o /tmp/233boy-install.sh; then
    die "下载 233boy 安装脚本失败"
  fi
  # 上游脚本没有 set -e, 必须自己复查产物
  bash /tmp/233boy-install.sh -m "$MIRROR" >/tmp/233boy-install.log 2>&1 || true
  if [[ ! -x /usr/local/bin/v2ray || ! -d /etc/v2ray/sh ]]; then
    err "233boy 安装失败, 日志尾部:"
    tail -n 15 /tmp/233boy-install.log >&2
    die "请手动装好 233boy 后加 --skip-233boy 重跑"
  fi
  rm -f /tmp/233boy-install.sh
  ok "233boy v2ray 安装完成"
fi

# ---------------------------------------------------------------- 2. 面板二进制
info "安装 v2panel 面板 ..."
mkdir -p "$V2PANEL_HOME/data" "$V2PANEL_HOME/dist"

# 面板二进制来源优先级:
#   1) V2PANEL_PANEL_URL / 本地已有文件
#   2) 当前目录下的 v2panel-panel-linux-<arch>
#   3) 从面板自己的 /download/ 拉 (被安装脚本下发时走这条)
TMP_BIN="/tmp/v2panel-panel.$$"
got=0
for cand in \
  "${V2PANEL_PANEL_URL:-}" \
  "./v2panel-panel-linux-$ARCH" \
  "$(dirname "$0")/v2panel-panel-linux-$ARCH" \
  "$V2PANEL_HOME/dist/v2panel-panel-linux-$ARCH" ; do
  [[ -z "$cand" ]] && continue
  if [[ -f "$cand" ]]; then cp -f "$cand" "$TMP_BIN"; got=1; break; fi
  if [[ "$cand" == http* ]]; then
    curl -fsSL --connect-timeout 10 --max-time 300 "$cand" -o "$TMP_BIN" 2>/dev/null && got=1 && break
  fi
done

if [[ "$got" != "1" ]]; then
  err "找不到面板二进制。三种解决办法:"
  echo "  a) 把 v2panel-panel-linux-$ARCH 放到本脚本同目录, 然后重跑"
  echo "  b) 加参数: --panel-url <下载地址>"
  echo "  c) 从源码编译: cd $V2PANEL_HOME && go build -o v2panel ./cmd/panel"
  exit 1
fi

# 至少要有 ELF 头 —— 防止反代返回的 HTML 错误页被当成二进制装上
if ! head -c 4 "$TMP_BIN" | grep -q $'\x7fELF'; then
  rm -f "$TMP_BIN"
  die "下载到的不是可执行文件 (可能被防火墙/反代拦截)"
fi
install -m 0755 "$TMP_BIN" "$V2PANEL_HOME/v2panel"
rm -f "$TMP_BIN"
ok "面板二进制: $V2PANEL_HOME/v2panel"

# ---------------------------------------------------------------- 3. systemd
cat > /etc/systemd/system/v2panel.service <<UNIT
[Unit]
Description=v2panel 面板 (233boy 多节点订阅管理)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$V2PANEL_HOME
ExecStart=$V2PANEL_HOME/v2panel -bind $V2PANEL_BIND -port $V2PANEL_PORT -db $V2PANEL_HOME/data/v2panel.db
Restart=always
RestartSec=5
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable v2panel >/dev/null 2>&1
systemctl restart v2panel
sleep 3
systemctl is-active --quiet v2panel || {
  err "面板启动失败, 日志:"
  journalctl -u v2panel -n 20 --no-pager >&2
  exit 1
}
ok "面板已启动并设为开机自启"

# ---------------------------------------------------------------- 4. 域名 / 证书
if [[ -n "$DOMAIN" ]]; then
  hr
  info "配置域名: $DOMAIN"

  # 4a. 自动加解析
  if [[ "$CF_ADD_DNS" == "1" && -n "$CF_MAIL" && -n "$CF_KEY" ]]; then
    PUBLIC_IP="$(curl -fsSL --max-time 10 https://api.ipify.org 2>/dev/null || echo "")"
    if [[ -z "$PUBLIC_IP" ]]; then
      warn "取不到本机公网 IP, 跳过自动解析"
    else
      ROOT_DOMAIN="${DOMAIN#*.}"
      info "在 Cloudflare 上加 $DOMAIN -> $PUBLIC_IP ..."
      python3 - "$CF_MAIL" "$CF_KEY" "$DOMAIN" "$PUBLIC_IP" "$ROOT_DOMAIN" <<'PYEOF'
import json, sys, urllib.request, urllib.error

mail, key, name, ip, root = sys.argv[1:6]
API = "https://api.cloudflare.com/client/v4"

def cf(path, method="GET", body=None):
    data = json.dumps(body).encode() if body else None
    req = urllib.request.Request(API + path, data=data, method=method)
    req.add_header("X-Auth-Email", mail)
    req.add_header("X-Auth-Key", key)
    req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            return json.loads(r.read())
    except urllib.error.HTTPError as e:
        return json.loads(e.read())

z = cf("/zones?name=" + root)
if not z.get("success") or not z.get("result"):
    print("  ! 找不到 zone %s (检查邮箱/Key/域名是否在同一个 CF 账号)" % root)
    sys.exit(0)
zid = z["result"][0]["id"]

rec = cf("/zones/%s/dns_records?name=%s" % (zid, name))
payload = {"type": "A", "name": name, "content": ip, "ttl": 1, "proxied": False}
if rec.get("success") and rec.get("result"):
    rid = rec["result"][0]["id"]
    out = cf("/zones/%s/dns_records/%s" % (zid, rid), "PUT", payload)
    print("  ✓ 已更新解析 %s -> %s" % (name, ip) if out.get("success") else "  ! 更新失败: %s" % out.get("errors"))
else:
    out = cf("/zones/%s/dns_records" % zid, "POST", payload)
    print("  ✓ 已添加解析 %s -> %s (DNS only)" % (name, ip) if out.get("success") else "  ! 添加失败: %s" % out.get("errors"))
PYEOF
      info "等待解析生效 ..."
      for _ in $(seq 1 12); do
        [[ "$(getent hosts "$DOMAIN" 2>/dev/null | awk '{print $1}' | head -1)" == "$PUBLIC_IP" ]] && break
        sleep 5
      done
    fi
  fi

  # 4b. 把域名写进面板设置 —— 订阅链接和二维码都用它
  python3 - "$V2PANEL_HOME/data/v2panel.db" "$DOMAIN" <<'PYEOF'
import sqlite3, sys, datetime
db, domain = sys.argv[1], sys.argv[2]
c = sqlite3.connect(db)
now = datetime.datetime.now().isoformat()
c.execute("""INSERT INTO system_settings(key, value, updated_at) VALUES('panel_domain', ?, ?)
             ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at""",
          ("https://" + domain, now))
c.commit()
print("  ✓ 面板域名已写入: https://%s" % domain)
PYEOF
  systemctl restart v2panel && sleep 3

  # 4c. 证书 + 反代
  if [[ "$NO_SSL" == "1" ]]; then
    warn "按参数跳过证书申请, 面板仍是 HTTP"
  elif ! command -v nginx >/dev/null 2>&1 && [[ ! -x /www/server/nginx/sbin/nginx ]]; then
    warn "没检测到 nginx, 跳过建站。面板仍监听 $V2PANEL_BIND:$V2PANEL_PORT"
    warn "  你可以在自己的反代里指向它, 记得透传 Upgrade 头 (agent 用 WebSocket)"
  else
    info "申请 Let's Encrypt 证书并建反代站点 ..."
    NGINX_CONF_DIR="/www/server/panel/vhost/nginx"
    if [[ -d "$NGINX_CONF_DIR" ]]; then
      # 宝塔环境: 走它自己的目录结构
      mkdir -p "/www/server/panel/vhost/cert/$DOMAIN" "/www/wwwroot/$DOMAIN"
      CERT_OK=0
      if [[ -x /www/server/panel/BT-Tool ]] || [[ -d /www/server/panel ]]; then
        info "  用宝塔签发证书 (certbot 可能没装)"
      fi
      # 优先用系统 certbot, 没有就用宝塔的
      if command -v certbot >/dev/null 2>&1; then
        certbot certonly --webroot -w "/www/wwwroot/$DOMAIN" -d "$DOMAIN" \
          --non-interactive --agree-tos --register-unsafely-without-email >/tmp/certbot.log 2>&1 \
          && CERT_OK=1 \
          && cp -f "/etc/letsencrypt/live/$DOMAIN/fullchain.pem" "/www/server/panel/vhost/cert/$DOMAIN/fullchain.pem" \
          && cp -f "/etc/letsencrypt/live/$DOMAIN/privkey.pem"   "/www/server/panel/vhost/cert/$DOMAIN/privkey.pem"
      fi
      if [[ "$CERT_OK" != "1" ]]; then
        warn "证书没签成 (日志 /tmp/certbot.log)。面板仍可用, 但只有 HTTP。"
        warn "  可在宝塔面板里对站点 $DOMAIN 点「SSL → Let's Encrypt」申请。"
      fi
    else
      warn "不是宝塔环境, 请自行配置 nginx 反代到 $V2PANEL_BIND:$V2PANEL_PORT"
    fi
  fi
fi

# ---------------------------------------------------------------- 5. 收尾信息
hr
printf '%s  安装完成%s\n' "$B" "$N"
hr
# ⚠️ 必须显式带 -db 绝对路径。
# v2panel 的 -db 默认值是相对当前目录的 ./data/v2panel.db, 从别的目录跑 -entry
# 会**静默新建一个空数据库**并生成一个全新的安全入口 —— 打出来的入口和真正在跑
# 的面板对不上, 排查起来极其误导。踩过一次, 所有 CLI 调用都带上 -db。
DB_FILE="$V2PANEL_HOME/data/v2panel.db"
"$V2PANEL_HOME/v2panel" -entry -db "$DB_FILE" 2>/dev/null | sed 's/^/  /' || true
hr

ENTRY="$(python3 - "$V2PANEL_HOME/data/v2panel.db" <<'PYEOF' 2>/dev/null || echo ""
import sqlite3, sys
try:
    c = sqlite3.connect(sys.argv[1])
    row = c.execute("select value from system_settings where key='safe_path'").fetchone()
    print(row[0] if row else "")
except Exception:
    print("")
PYEOF
)"

IP="$(curl -fsSL --max-time 8 https://api.ipify.org 2>/dev/null || echo '<本机IP>')"
if [[ -n "$DOMAIN" && "$NO_SSL" != "1" ]]; then
  BASE="https://$DOMAIN"
else
  BASE="http://$IP:$V2PANEL_PORT"
fi

cat <<EOF

  ${B}下一步${N}

  1. 打开面板
       $BASE$ENTRY
     账号 admin    密码 admin123456
     ${Y}第一次登录后请立刻改密码${N}

  2. 面板里点「节点 → 新建节点」
     - 地址填客户端要连的域名 (TLS 协议必填, 且要已解析到节点机)
     - ${B}如果这台机器上已经在跑线上节点${N}, 「保护名单」填那个配置名的
       关键片段 (如 tzq1), 面板就不会去删它

  3. 复制面板给出的「一键纳管命令」, 到目标机上以 root 执行
     agent 会自动连回来, 节点变成「在线」

  4. 「订阅 → 新建账号」, 把生成的订阅地址发给用户
     用户打开那个地址就能看到图文导入教程

  常用命令
     systemctl status v2panel        面板状态
     journalctl -u v2panel -f        面板日志
     $V2PANEL_HOME/v2panel -entry    再看一次安全入口
     $V2PANEL_HOME/v2panel -set-password '<新密码>'

EOF
