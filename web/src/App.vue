<template>
  <!-- el-config-provider 提供中文 locale。放在这里而不是 main.js 的 app.use(ElementPlus),
       是为了避免全量引入 Element Plus (那会把按需引入的优化抵消掉)。 -->
  <el-config-provider :locale="zhCn">
  <!-- 登录页是独立布局, 不套顶栏 -->
  <router-view v-if="$route.meta.bare" />

  <div v-else-if="booting" class="vp-login-wrap">
    <div style="text-align:center;color:#86868b">
      <el-icon class="is-loading" :size="22"><Loading /></el-icon>
      <div style="margin-top:10px;font-size:13.5px">正在校验登录状态…</div>
    </div>
  </div>

  <div v-else-if="fatal" class="vp-login-wrap">
    <div class="vp-login-card">
      <h1>无法访问面板</h1>
      <div class="sub">{{ fatal }}</div>
      <el-button type="primary" style="width:100%" @click="reload">重新加载</el-button>
    </div>
  </div>

  <div v-else class="vp-shell">
    <header class="vp-topbar">
      <div class="vp-brand">v2panel<span>233boy 订阅面板</span></div>
      <nav class="vp-nav">
        <router-link v-for="p in PAGES" :key="p.path" :to="p.path">{{ p.label }}</router-link>
      </nav>
      <div class="vp-right">
        <span>{{ me?.username || '' }}</span>
        <el-button size="small" @click="pwdOpen = true">改密</el-button>
        <el-button size="small" @click="logout">退出</el-button>
      </div>
    </header>

    <main class="vp-wrap">
      <router-view />
    </main>
  </div>

  <!-- 改密码 -->
  <el-dialog v-model="pwdOpen" title="修改密码" width="420px">
    <el-form label-position="top">
      <el-form-item label="当前密码">
        <el-input v-model="pwd.old" type="password" show-password autocomplete="current-password" />
      </el-form-item>
      <el-form-item label="新密码">
        <el-input v-model="pwd.next" type="password" show-password autocomplete="new-password" />
        <div class="vp-sub" style="margin-top:5px">至少 6 位。</div>
      </el-form-item>
      <el-form-item label="确认新密码">
        <el-input v-model="pwd.repeat" type="password" show-password autocomplete="new-password" />
      </el-form-item>
    </el-form>
    <template #footer>
      <el-button @click="pwdOpen = false">取消</el-button>
      <el-button type="primary" :loading="pwdBusy" @click="changePwd">保存</el-button>
    </template>
  </el-dialog>
  </el-config-provider>
</template>

<script setup>
import { ref, reactive, onMounted } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { ElMessage } from 'element-plus'
import zhCn from 'element-plus/es/locale/lang/zh-cn'
import { Loading } from '@element-plus/icons-vue'
import { auth, misc, onAuthLost } from './api.js'
import { setMe } from './main.js'

const router = useRouter()
const route = useRoute()

const PAGES = [
  { path: '/', label: '概览' },
  { path: '/nodes', label: '节点' },
  { path: '/servers', label: '运维' },
  { path: '/subscriptions', label: '订阅' },
  { path: '/plans', label: '套餐' },
  { path: '/operations', label: '运营' },
  { path: '/traffic', label: '流量' },
  { path: '/settings', label: '设置' }
]

const booting = ref(true)
const fatal = ref('')
const me = ref(null)

const pwdOpen = ref(false)
const pwdBusy = ref(false)
const pwd = reactive({ old: '', next: '', repeat: '' })

function reload() {
  location.reload()
}

// 登录态在启动时统一确认一次。
// 旧版是每个页面各自 guard(), 结果每切一次页都多两个请求; 这里只做一次。
onMounted(async () => {
  try {
    const r = await auth.me()
    me.value = r.data || {}
    setMe(me.value)
    booting.value = false
  } catch (e) {
    booting.value = false
    if (e.http === 401) {
      // 未登录: 跳登录页, 并记住原本要去哪
      router.replace('/login?next=' + encodeURIComponent(route.fullPath))
    } else if (e.http === 403) {
      // 安全入口没过 —— 这不能靠跳转解决, 要明确告诉用户
      fatal.value = e.message
    } else {
      fatal.value = e.message || '无法连接面板'
    }
  }
})

// 任何请求返回 401 都统一踢回登录页 (token 过期 / 被改密)
onAuthLost(() => {
  if (route.meta.bare) return
  me.value = null
  setMe(null)
  router.replace('/login?next=' + encodeURIComponent(route.fullPath))
})

async function logout() {
  try {
    await auth.logout()
  } catch (e) {
    /* 登出失败也要清本地状态 */
  }
  me.value = null
  setMe(null)
  router.replace('/login')
}

async function changePwd() {
  if (!pwd.old) return ElMessage.warning('请填写当前密码')
  if (!pwd.next || pwd.next.length < 6) return ElMessage.warning('新密码至少 6 位')
  if (pwd.next !== pwd.repeat) return ElMessage.warning('两次输入的新密码不一致')
  pwdBusy.value = true
  try {
    const r = await auth.changePassword(pwd.old, pwd.next)
    ElMessage.success(r.message || '密码已修改')
    pwdOpen.value = false
    pwd.old = pwd.next = pwd.repeat = ''
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    pwdBusy.value = false
  }
}

// 让子页面能刷新顶栏里的用户名
defineExpose({ me })
</script>
