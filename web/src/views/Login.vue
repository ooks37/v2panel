<template>
  <div class="vp-login-wrap">
    <div class="vp-login-card">
      <h1>登录 v2panel</h1>
      <div class="sub">233boy 多节点订阅管理面板</div>

      <el-form @submit.prevent="submit" label-position="top">
        <el-form-item label="账号">
          <el-input v-model="form.username" size="large" autocomplete="username" placeholder="admin" />
        </el-form-item>
        <el-form-item label="密码">
          <el-input
            v-model="form.password"
            type="password"
            size="large"
            show-password
            autocomplete="current-password"
            placeholder="密码"
            @keyup.enter="submit"
          />
        </el-form-item>
        <el-button
          type="primary"
          size="large"
          style="width:100%"
          :loading="busy"
          @click="submit"
        >登录</el-button>
      </el-form>

      <div v-if="err" class="vp-note bad" style="margin-top:16px;margin-bottom:0">{{ err }}</div>

      <div class="vp-note" style="margin-top:18px;margin-bottom:0">
        首次安装的默认账号是 <code>admin</code> / <code>admin123456</code>，
        <b>登录后请立刻改密码</b>。
      </div>
    </div>
  </div>
</template>

<script setup>
import { ref, reactive, onMounted } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { ElMessage } from 'element-plus'
import { auth, misc } from '../api.js'

const router = useRouter()
const route = useRoute()

const form = reactive({ username: 'admin', password: '' })
const busy = ref(false)
const err = ref('')

// 已经登录过就直接进面板, 不用再看一次表单
onMounted(async () => {
  try {
    await auth.me()
    go()
  } catch (e) {
    /* 没登录, 留在本页 */
  }
})

function go() {
  const next = route.query.next
  router.replace(typeof next === 'string' && next ? next : '/')
}

async function submit() {
  if (!form.username || !form.password) {
    err.value = '请填写账号与密码'
    return
  }
  busy.value = true
  err.value = ''
  try {
    const r = await auth.login(form.username, form.password)

    // 后端在响应顶层返回 token, 同时也会 Set-Cookie。
    // Cookie 是主路径 (HttpOnly), 这里只在拿不到 Cookie 的极端情况下兜底。
    if (r.token && !document.cookie.includes('v2panel_token')) {
      document.cookie = 'v2panel_token=' + r.token + '; path=/; max-age=86400; samesite=lax'
    }

    // 安全入口地址由后端决定, 登录成功后用它跳转 —— 这样即使用户是从
    // /login.html 直连进来的, 也会被带到正确的入口地址下。
    try {
      const se = await misc.safeEntrance()
      const entry = se.data?.entry_url
      if (entry && !location.pathname.startsWith(new URL(entry, location.origin).pathname)) {
        location.replace(entry)
        return
      }
    } catch (e) {
      /* 拿不到入口就按当前路径继续 */
    }

    ElMessage.success('登录成功')
    go()
  } catch (e) {
    err.value = e.message || '登录失败'
  } finally {
    busy.value = false
  }
}
</script>
