import { createApp } from 'vue'
import { createRouter, createWebHistory } from 'vue-router'
import './styles/theme.css'

import App from './App.vue'
import Dashboard from './views/Dashboard.vue'
import Nodes from './views/Nodes.vue'
import NodeDetail from './views/NodeDetail.vue'
import Servers from './views/Servers.vue'
import Subscriptions from './views/Subscriptions.vue'
import Plans from './views/Plans.vue'
import Operations from './views/Operations.vue'
import Traffic from './views/Traffic.vue'
import Settings from './views/Settings.vue'
import Login from './views/Login.vue'
import NotFound from './views/NotFound.vue'

// ⚠️ 不要 `import ElementPlus from 'element-plus'` + `app.use(ElementPlus)`。
// 那是全量引入 —— 会把所有组件都打进包里 (实测 1.1MB / 363KB gzip)。
// unplugin-vue-components 的 ElementPlusResolver 已经按需引入了用到的组件与样式,
// 全量引入反而把按需优化抵消掉。
// 中文 locale 由 App.vue 里的 <el-config-provider> 提供。

// history 模式 + 动态 base。
//
// ⚠️ 两个都不能省:
//
// 1. 不能用 hash 模式: 面板首页是安全入口路径就地渲染的 (如 /a1b2c3d4e5f60718),
//    用 hash 的话用户看到的地址会变成 /a1b2c3d4e5f60718#/nodes —— 安全入口混在
//    前端路由里, 刷新、分享、排错都会很乱。history 模式下路由是 /nodes 这种干净路径。
//
// 2. base 必须是后端注入的安全入口路径, 不能写死 '/'。
//    因为首页是「就地渲染」在 /a1b2c3d4e5f60718 这个地址上的, 浏览器地址栏里带着
//    这个前缀。若 router base 是 '/', Vue Router 会拿 /a1b2c3d4e5f60718 去匹配路由
//    → 命中 404 页。后端在 index.html 的 </head> 前注入了 window.__VP_BASE__。
//
// 拿不到注入值时回落到 '/' (例如直接跑 vite dev)。
const BASE = (typeof window !== 'undefined' && window.__VP_BASE__) || '/'

const router = createRouter({
  history: createWebHistory(BASE),
  routes: [
    { path: '/', name: 'dashboard', component: Dashboard, meta: { title: '概览' } },
    { path: '/nodes', name: 'nodes', component: Nodes, meta: { title: '节点' } },
    { path: '/nodes/:id', name: 'node-detail', component: NodeDetail, meta: { title: '节点详情' } },
    { path: '/servers', name: 'servers', component: Servers, meta: { title: '运维' } },
    { path: '/subscriptions', name: 'subscriptions', component: Subscriptions, meta: { title: '订阅' } },
    { path: '/plans', name: 'plans', component: Plans, meta: { title: '套餐' } },
    { path: '/operations', name: 'operations', component: Operations, meta: { title: '运营' } },
    { path: '/traffic', name: 'traffic', component: Traffic, meta: { title: '流量' } },
    { path: '/settings', name: 'settings', component: Settings, meta: { title: '设置' } },
    { path: '/login', name: 'login', component: Login, meta: { title: '登录', bare: true } },
    { path: '/:pathMatch(.*)*', name: 'notfound', component: NotFound, meta: { title: '未找到' } }
  ],
  scrollBehavior() {
    return { top: 0 }
  }
})

// 路由守卫: 只做「没登录就去登录页」这一件事。
// 安全入口的校验在后端 (SafeEntranceFilter), 前端不做也不能做 ——
// 前端拦不住任何人直接调 API。
let meCache = null
export function setMe(v) {
  meCache = v
}
export function getMe() {
  return meCache
}

router.beforeEach(async (to) => {
  if (to.meta.bare) return true
  if (meCache) return true
  // 还没确认登录态, 交给 App.vue 的初始化去拉；这里只放行, 避免重复请求
  return true
})

const app = createApp(App)
app.use(router)
app.mount('#app')
