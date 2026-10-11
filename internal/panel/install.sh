#!/usr/bin/env bash
# v2panel agent 一键安装脚本
#
# 由面板的 /install.sh 动态下发, 面板会把 DEFAULT_SERVER / DEFAULT_TOKEN /
# DEFAULT_PROTECTED 三个变量用字面量替换 (不是正则展开) 注入进来, 所以不要依赖任何外部参数。
#
# 用法:
#   curl -fsSL https://<面板>/install.sh | bash
#   curl -fsSL https://<面板>/install.sh | bash -s -- --token <TOKEN> --server https://<面板>
#
# 本脚本做四件事:
#   1. 装 233boy v2ray (若机器上还没有) —— 带镜像回退, 因为脚本默认直连 GitHub
#   2. 下载 v2panel-agent 二进制
#   3. 写 /etc/v2panel-agent/config.json
#   4. 装 systemd 服务并启动
#
# 幂等: 重复执行只会覆盖二进制与配置, 不会重复安装 233boy。

set -uo pipefail

DEFAULT_SERVER="__V2PANEL_DEFAULT_SERVER__"
DEFAULT_TOKEN="__V2PANEL_DEFAULT_TOKEN__"
DEFAULT_PROTECTED="__V2PANEL_DEFAULT_PROTECTED__"

SERVER="$DEFAULT_SERVER"
TOKEN="$DEFAULT_TOKEN"
PROTECTED="$DEFAULT_PROTECTED"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --server) SERVER="${2:-}"; shift 2 ;;
    --token)  TOKEN="${2:-}";  shift 2 ;;
    --protected) PROTECTED="${2:-}"; shift 2 ;;
    --server=*) SERVER="${1#*=}"; shift ;;
    --token=*)  TOKEN="${1#*=}";  shift ;;
    --protected=*) PROTECTED="${1#*=}"; shift ;;
    *) shift ;;
  esac
done

red()  { printf '\033[31m%s\033[0m\n' "$*"; }
grn()  { printf '\033[32m%s\033[0m\n' "$*"; }
ylw()  { printf '\033[33m%s\033[0m\n' "$*"; }
info() { printf '\033[36m[v2panel]\033[0m %s\n' "$*"; }

if [[ "$(id -u)" != "0" ]]; then
  red "错误: 请以 root 身份运行 (sudo -i 后重试)"
  exit 1
fi

if [[ -z "$SERVER" || "$SERVER" == __V2PANEL_* ]]; then
  red "错误: 未指定面板地址。请用 --server https://你的面板地址 指定"
  exit 1
fi
if [[ -z "$TOKEN" || "$TOKEN" == __V2PANEL_* ]]; then
  red "错误: 未指定节点 Token。请在面板「节点」页面复制完整的一键纳管命令"
  exit 1
fi

# Token 只含十六进制字符, 面板地址只允许 http(s)://host[:port] —— 在这里再挡一次,
# 因为这两个值会被写进 root 拥有的 systemd 配置。本脚本由管道直接执行,
# 任何一次注入都等于目标机失守。
if [[ ! "$TOKEN" =~ ^[A-Za-z0-9]+$ ]]; then
  red "错误: Token 含非法字符"
  exit 1
fi
if [[ ! "$SERVER" =~ ^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?$ ]]; then
  red "错误: 面板地址格式不合法: $SERVER"
  exit 1
fi

# 保护名单: 逗号分隔的配置名子串 (如 tzq1,hs)。面板没注入时保留占位符 -> 视为空。
# 字符集只允许 [A-Za-z0-9._-] 加分隔符 , : / —— 它会被拼进 config.json 的 JSON 数组,
# 引号/反斜杠/$/反引号一个都不能放进来 (否则等于把注入点交给面板)。
if [[ "$PROTECTED" == __V2PANEL_* ]]; then
  PROTECTED=""
fi
if [[ -n "$PROTECTED" && ! "$PROTECTED" =~ ^[A-Za-z0-9._,:/-]+$ ]]; then
  red "错误: 保护名单含非法字符: $PROTECTED"
  exit 1
fi

# 逗号分隔 -> JSON 数组字面量。空名单写成 [] 而不是 [""]。
PROTECTED_JSON="[]"
if [[ -n "$PROTECTED" ]]; then
  PROTECTED_JSON="["
  first=1
  IFS=',' read -ra _plist <<< "$PROTECTED"
  for _p in "${_plist[@]}"; do
    _p="${_p#"${_p%%[![:space:]]*}"}"   # ltrim
    _p="${_p%"${_p##*[![:space:]]}"}"   # rtrim
    [[ -z "$_p" ]] && continue
    [[ $first == 1 ]] || PROTECTED_JSON+=", "
    PROTECTED_JSON+="\"$_p\""
    first=0
  done
  PROTECTED_JSON+="]"
fi

SERVER="${SERVER%/}"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) red "错误: 不支持的架构 $ARCH"; exit 1 ;;
esac

info "面板地址: $SERVER"
info "节点架构: linux/$ARCH"

# ---------------------------------------------------------------------------
# 1. 233boy v2ray
# ---------------------------------------------------------------------------
if [[ -x /usr/local/bin/v2ray && -d /etc/v2ray/sh ]]; then
  grn "✓ 已检测到 233boy v2ray, 跳过安装"
else
  info "正在安装 233boy v2ray ..."
  # 官方脚本内部直连 GitHub raw, 在国内/受限网络上会挂。
  # 官方脚本本身支持 -m 指定镜像前缀, 依次尝试直到成功。
  # 注意: 镜像只影响下载源, 脚本逻辑仍是上游 v4.27 原版。
  installed=0
  for mirror in "" "https://ghproxy.net/" "https://gh-proxy.com/" "https://ghfast.top/"; do
    if [[ -n "$mirror" ]]; then
      info "尝试镜像: $mirror"
    fi
    if curl -fsSL --connect-timeout 10 --max-time 180 \
        "${mirror}https://github.com/233boy/v2ray/raw/master/install.sh" -o /tmp/233boy-install.sh 2>/dev/null \
       && [[ -s /tmp/233boy-install.sh ]]; then
      # 上游脚本没有 set -e, 必须自己判退出码 + 复查产物
      if bash /tmp/233boy-install.sh -m "$mirror" >/tmp/233boy-install.log 2>&1; then
        if [[ -x /usr/local/bin/v2ray && -d /etc/v2ray/sh ]]; then
          installed=1
          break
        fi
      fi
      ylw "该源安装未成功, 继续尝试下一个"
    fi
  done
  rm -f /tmp/233boy-install.sh

  if [[ "$installed" != "1" ]]; then
    red "✗ 233boy v2ray 安装失败。日志尾部:"
    tail -n 20 /tmp/233boy-install.log 2>/dev/null
    red "  可手动安装后重跑本脚本: bash <(curl -fsSL https://github.com/233boy/v2ray/raw/master/install.sh)"
    exit 1
  fi
  grn "✓ 233boy v2ray 安装完成"
fi

# ---------------------------------------------------------------------------
# 2. agent 二进制
# ---------------------------------------------------------------------------
info "正在下载 v2panel-agent ..."
mkdir -p /usr/local/bin /etc/v2panel-agent
TMP_BIN="/tmp/v2panel-agent.$$"
if ! curl -fsSL --connect-timeout 10 --max-time 300 \
      "$SERVER/download/v2panel-agent-linux-$ARCH" -o "$TMP_BIN"; then
  red "✗ 下载 agent 失败: $SERVER/download/v2panel-agent-linux-$ARCH"
  exit 1
fi
# 至少要有 ELF 头, 防止反代返回的 HTML 错误页被当成二进制装上
if ! head -c 4 "$TMP_BIN" | grep -q $'\x7fELF'; then
  red "✗ 下载到的不是可执行文件 (可能被反代/防火墙拦截)"
  rm -f "$TMP_BIN"
  exit 1
fi
install -m 0755 "$TMP_BIN" /usr/local/bin/v2panel-agent
rm -f "$TMP_BIN"
grn "✓ agent 已安装: /usr/local/bin/v2panel-agent"

# ---------------------------------------------------------------------------
# 3. 配置
# ---------------------------------------------------------------------------
cat > /etc/v2panel-agent/config.json <<CFG_EOF
{
  "server": "$SERVER",
  "token": "$TOKEN",
  "insecure": false,
  "protected": $PROTECTED_JSON
}
CFG_EOF
chmod 0600 /etc/v2panel-agent/config.json
grn "✓ 配置已写入: /etc/v2panel-agent/config.json"

# ---------------------------------------------------------------------------
# 4. systemd
# ---------------------------------------------------------------------------
cat > /etc/systemd/system/v2panel-agent.service <<'UNIT_EOF'
[Unit]
Description=v2panel Agent (233boy v2ray node manager)
Documentation=https://github.com/233boy/v2ray
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/v2panel-agent -config /etc/v2panel-agent/config.json
Restart=always
RestartSec=5
# agent 需要调用 v2ray / systemctl, 因此以 root 运行。
# 不用 ProtectSystem 之类沙箱: 它会挡住 agent 写 /etc/v2ray/conf 与操作 caddy。
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
UNIT_EOF

systemctl daemon-reload
systemctl enable v2panel-agent >/dev/null 2>&1
systemctl restart v2panel-agent

sleep 2
if systemctl is-active --quiet v2panel-agent; then
  grn "✓ v2panel-agent 已启动并设为开机自启"
else
  red "✗ v2panel-agent 启动失败, 最近日志:"
  journalctl -u v2panel-agent -n 20 --no-pager 2>/dev/null
  exit 1
fi

echo
grn "=============================================="
grn "  纳管完成! 请回到面板查看该节点是否在线"
grn "=============================================="
echo
info "常用命令:"
echo "  systemctl status v2panel-agent     # 查看 agent 状态"
echo "  journalctl -u v2panel-agent -f     # 跟踪 agent 日志"
echo "  v2ray                              # 233boy 交互菜单"
