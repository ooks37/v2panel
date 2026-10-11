import { api } from './api.js'

// 套餐 / 公告 / 订单 / 用户同步 —— 运营相关接口

export const plans = {
  list: () => api.get('/admin/plans'),
  get: (id) => api.get('/admin/plans/' + id),
  create: (body) => api.post('/admin/plans', body),
  update: (id, body) => api.put('/admin/plans/' + id, body),
  remove: (id) => api.del('/admin/plans/' + id),
  // 把套餐当前值同步到该套餐下的存量用户 (显式动作)
  apply: (id, body) => api.post('/admin/plans/' + id + '/apply', body || {})
}

export const announcements = {
  list: () => api.get('/admin/announcements'),
  create: (body) => api.post('/admin/announcements', body),
  update: (id, body) => api.put('/admin/announcements/' + id, body),
  remove: (id) => api.del('/admin/announcements/' + id)
}

export const orders = {
  list: (userId) => api.get('/admin/orders' + (userId ? '?user_id=' + userId : ''))
}

// 把当前用户名单推到指定节点 (摘掉超额用户 / 铺上新用户)
export function syncNodeUsers(nodeId) {
  return api.post('/admin/nodes/' + nodeId + '/sync-users')
}

// —— 常量 (与后端 usersync_panel.go 的 ResetMode* 保持一致) ——
export const RESET_MODES = [
  { value: 'monthly', label: '每月 1 号重置', hint: '最常见的默认。每月 1 日 0 点流量归零。' },
  { value: 'cycle', label: '按到期日重置', hint: '如 4 号到期，则每月 4 号归零。' },
  { value: 'none', label: '不重置', hint: '一次性流量包，用完为止。' }
]

export const GB = 1073741824

export function gbToBytes(gb) {
  const n = Number(gb)
  if (!n || n <= 0) return 0
  return Math.round(n * GB)
}

export function bytesToGB(b) {
  const n = Number(b)
  if (!n || n <= 0) return 0
  return +(n / GB).toFixed(2)
}
