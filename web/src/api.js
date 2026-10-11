// v2panel API 客户端。
//
// 与旧版 app.js 的 request() 保持同样的契约:
//   - 所有接口在 /api/v1 下
//   - 成功响应形如 { code: 0, data: ... }；**不解包**, 返回整个信封, 调用方自己取 .data
//   - 失败时抛出带 code / http / message 的 Error，由调用方决定怎么提示
//
// 这里额外做两件旧版没有的事:
//   1. 401 时广播一个事件，让 App 层统一跳登录页（而不是每个调用点各写一遍）
//   2. 403 时区分「没走安全入口」和其它权限问题 —— 前者要给出明确指引

const BASE = '/api/v1'

export class ApiError extends Error {
  constructor(message, { code, http } = {}) {
    super(message)
    this.name = 'ApiError'
    this.code = code
    this.http = http
  }
}

// 401 统一由 App.vue 监听后跳登录
const onUnauthorized = new Set()
export function onAuthLost(fn) {
  onUnauthorized.add(fn)
  return () => onUnauthorized.delete(fn)
}

async function request(method, path, body) {
  const opt = {
    method,
    credentials: 'same-origin',
    headers: {}
  }
  if (body !== undefined) {
    opt.headers['Content-Type'] = 'application/json'
    opt.body = JSON.stringify(body)
  }

  let res
  try {
    res = await fetch(BASE + path, opt)
  } catch (e) {
    throw new ApiError('网络错误，请检查面板是否还在运行', { http: 0 })
  }

  const text = await res.text()
  let json = null
  try {
    json = text ? JSON.parse(text) : null
  } catch (e) {
    // 反代返回了 HTML 错误页之类
    throw new ApiError('服务端返回了非 JSON 响应 (HTTP ' + res.status + ')', { http: res.status })
  }

  if (res.status === 401) {
    onUnauthorized.forEach((fn) => fn())
    throw new ApiError((json && json.message) || '登录已失效', { http: 401, code: 401 })
  }

  if (res.status === 403) {
    const msg = (json && json.message) || ''
    // 这个提示是后端 SafeEntranceFilter 的固定文案，值得单独引导
    if (msg.indexOf('安全入口') >= 0) {
      throw new ApiError(
        '当前访问没有通过安全入口。请改用带安全入口路径的地址进入面板。',
        { http: 403, code: 403 }
      )
    }
    throw new ApiError(msg || '没有权限', { http: 403, code: 403 })
  }

  if (!res.ok || (json && json.code !== 0)) {
    throw new ApiError(
      (json && (json.message || json.error)) || ('请求失败 (HTTP ' + res.status + ')'),
      { http: res.status, code: json && json.code }
    )
  }

  return json || {}
}

export const api = {
  get: (p) => request('GET', p),
  post: (p, b) => request('POST', p, b),
  put: (p, b) => request('PUT', p, b),
  del: (p) => request('DELETE', p)
}

// ── 具体接口 ────────────────────────────────────────────────
export const auth = {
  me: () => api.get('/auth/me'),
  login: (username, password) => api.post('/auth/login', { username, password }),
  logout: () => api.post('/auth/logout'),
  changePassword: (oldPassword, newPassword) =>
    api.post('/admin/change-password', { old_password: oldPassword, new_password: newPassword })
}

export const nodes = {
  list: () => api.get('/admin/nodes'),
  get: (id) => api.get('/admin/nodes/' + id),
  create: (body) => api.post('/admin/nodes', body),
  update: (id, body) => api.put('/admin/nodes/' + id, body),
  remove: (id) => api.del('/admin/nodes/' + id),
  sync: (id) => api.post('/admin/nodes/' + id + '/sync'),
  capability: (id) => api.get('/admin/nodes/' + id + '/capability'),
  restart: (id) => api.post('/admin/nodes/' + id + '/restart'),
  logs: (id, source, lines) =>
    api.get('/admin/nodes/' + id + '/logs?source=' + source + '&lines=' + lines),
  inbounds: (id) => api.get('/admin/nodes/' + id + '/inbounds'),
  createInbound: (id, body) => api.post('/admin/nodes/' + id + '/inbounds', body),
  updateInbounds: (id, body) => api.put('/admin/nodes/' + id + '/inbounds', body),
  probeInbound: (id, confName, timeoutSec) =>
    api.post('/admin/nodes/' + id + '/inbounds/probe', { conf_name: confName, timeout_sec: timeoutSec }),
  deleteInbound: (id, conf) =>
    api.del('/admin/nodes/' + id + '/inbounds/' + encodeURIComponent(conf)),
  // 重新签发 agent Token。后端目前**没有**这个路由 —— 调用会 404,
  // 由调用方捕获后提示用户 (该接口属于尚未实现的远程安装功能)。
  rotateToken: (id) => api.post('/admin/nodes/' + id + '/token')
}

export const users = {
  list: () => api.get('/admin/users'),
  create: (body) => api.post('/admin/users', body),
  batch: (body) => api.post('/admin/users/batch', body),
  update: (id, body) => api.put('/admin/users/' + id, body),
  remove: (id) => api.del('/admin/users/' + id),
  resetTraffic: (id) => api.post('/admin/users/' + id + '/reset-traffic'),
  regenerate: (id) => api.post('/admin/users/' + id + '/regenerate'),
  resetAll: () => api.post('/admin/users/reset-all')
}

export const traffic = {
  overview: (hours, nodeId) =>
    api.get('/admin/traffic/overview?hours=' + hours + (nodeId ? '&node_id=' + nodeId : '')),
  inbounds: () => api.get('/admin/traffic/inbounds'),
  purge: (days) => api.post('/admin/traffic/purge?days=' + days)
}

export const misc = {
  dashboard: () => api.get('/admin/dashboard'),
  series: (hours) => api.get('/admin/traffic/series?hours=' + hours),
  protocols: () => api.get('/admin/protocols'),
  settings: () => api.get('/admin/settings'),
  saveSettings: (body) => api.post('/admin/settings', body),
  safeEntrance: () => api.get('/admin/safe-entrance'),
  saveSafeEntrance: (body) => api.post('/admin/safe-entrance', body)
}

// ── 工具 ────────────────────────────────────────────────────
export function humanBytes(n) {
  n = Number(n) || 0
  if (n <= 0) return '0 B'
  const u = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']
  let i = 0
  while (n >= 1024 && i < u.length - 1) {
    n /= 1024
    i++
  }
  return (i === 0 ? n : n.toFixed(n >= 100 ? 0 : n >= 10 ? 1 : 2)) + ' ' + u[i]
}

export function fmtTime(t) {
  if (!t) return '—'
  const d = new Date(t)
  if (isNaN(d.getTime())) return '—'
  const p = (x) => String(x).padStart(2, '0')
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) +
    ' ' + p(d.getHours()) + ':' + p(d.getMinutes())
}

export function ago(t) {
  if (!t) return '从未'
  const d = new Date(t)
  if (isNaN(d.getTime())) return '从未'
  const s = Math.floor((Date.now() - d.getTime()) / 1000)
  if (s < 0) return '刚刚'
  if (s < 60) return s + ' 秒前'
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前'
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前'
  if (s < 2592000) return Math.floor(s / 86400) + ' 天前'
  return fmtTime(t)
}

// 订阅地址: 后端给 base_url, 拼上 /sub/<token>
export function subURL(u, baseUrl) {
  return (baseUrl || '') + '/sub/' + (u.sub_token || '')
}

// 二维码走后端生成, 避免引入前端库
export function qrURL(text) {
  return '/api/v1/qrcode?text=' + encodeURIComponent(text || '')
}
