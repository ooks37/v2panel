<template>
  <div class="vp-head">
    <h1>设置</h1>
    <div class="desc">对外域名、订阅页行为与快速上手</div>
  </div>

  <div v-if="loading" style="text-align:center;padding:40px">
    <el-icon class="is-loading" :size="22"><Loading /></el-icon>
  </div>

  <template v-else>
    <div class="vp-card">
      <h2>快速上手</h2>
      <div class="vp-steps">
        <div class="vp-step">
          <span class="n">1</span>
          <span>
            <b>建节点</b>
            <div class="vp-sub" style="margin-top:2px">
              到「节点」页新建一个，填客户端要连的域名。
              <b>如果那台机器上已经在跑线上节点</b>，一定要填「保护名单」。
            </div>
          </span>
        </div>
        <div class="vp-step">
          <span class="n">2</span>
          <span>
            <b>接机器</b>
            <div class="vp-sub" style="margin-top:2px">
              复制面板给出的<b>一键纳管命令</b>，在目标机上以 root 执行。
              agent 会主动连回来，不需要目标机开放任何端口。
            </div>
          </span>
        </div>
        <div class="vp-step">
          <span class="n">3</span>
          <span>
            <b>发订阅</b>
            <div class="vp-sub" style="margin-top:2px">
              到「订阅」页新建账号，把生成的订阅地址发给用户。
              用户打开就能看到图文导入教程。
            </div>
          </span>
        </div>
      </div>
      <div class="vp-note" style="margin-top:14px;margin-bottom:0">
        做完这三步，用户拿到一条地址就能自己配好客户端。
      </div>
    </div>

    <div class="vp-card">
      <h2>对外域名<span class="spacer"></span><span class="sub">影响所有订阅链接</span></h2>
      <el-form label-position="top">
        <el-form-item label="面板域名">
          <el-input v-model="form.panel_domain" placeholder="例如 https://vp.example.com" />
          <div class="vp-sub" style="margin-top:6px">
            订阅链接和二维码会用这个域名生成。<b>不填的话</b>订阅地址会退化成当前访问地址 ——
            用 IP 访问面板时，生成给用户的链接就是 IP，换机器就失效。
            填了域名后，用户拿到的链接始终指向域名。
          </div>
        </el-form-item>
      </el-form>
      <el-button type="primary" :loading="savingDomain" @click="saveDomain">保存域名</el-button>
    </div>

    <div class="vp-card">
      <h2>订阅页行为</h2>
      <div class="vp-note" :class="form.sub_require_password ? 'warn' : 'ok'">
        <template v-if="form.sub_require_password">
          当前<b>已开启</b>订阅密码保护。用户打开订阅页要输密码，
          把地址粘进客户端时必须在链接后加 <code>?pwd=密码</code>，否则客户端会拿到 401。
        </template>
        <template v-else>
          当前<b>不需要密码</b>。用户拿到订阅地址直接粘进客户端就能用 —— 这是最省事的默认值。
        </template>
      </div>
      <el-checkbox v-model="form.sub_require_password" style="margin-bottom:10px">
        要求订阅密码（防订阅链接被转发）
      </el-checkbox>
      <div class="vp-sub" style="margin-bottom:14px">
        订阅地址里的 Token 本身就是 128 位随机串，猜不到。开启密码后等于多一层，
        代价是<b>用户的客户端配置会复杂一步</b> —— 粘进客户端时得手动加 <code>?pwd=</code>。
        发给不熟的人建议保持关闭。
      </div>
      <el-button type="primary" :loading="savingSub" @click="saveSub">保存</el-button>
    </div>

    <div class="vp-card">
      <h2>关于</h2>
      <el-descriptions :column="1" border size="small">
        <el-descriptions-item label="节点内核">
          233boy/v2ray v4.27 · V2Ray core 5.53.0 · Caddy 2.11.4
        </el-descriptions-item>
        <el-descriptions-item label="登录账号">{{ me?.username || 'admin' }}</el-descriptions-item>
        <el-descriptions-item label="安全入口">
          <span class="vp-mono">{{ entry }}</span>
        </el-descriptions-item>
      </el-descriptions>
      <div class="vp-note" style="margin-top:14px;margin-bottom:0">
        改密码：顶部「改密」按钮。改安全入口路径需要命令行：
        <code>v2panel -set-entry /新路径 -db /opt/v2panel/data/v2panel.db</code>
      </div>
    </div>
  </template>
</template>

<script setup>
import { ref, reactive, onMounted } from 'vue'
import { Loading } from '@element-plus/icons-vue'
import { ElMessage } from 'element-plus'
import { misc } from '../api.js'
import { getMe } from '../main.js'

const loading = ref(true)
const savingDomain = ref(false)
const savingSub = ref(false)
const me = ref(getMe())
const entry = ref('')

const form = reactive({ panel_domain: '', sub_require_password: false })

async function load() {
  try {
    const [s, se] = await Promise.all([
      misc.settings(),
      misc.safeEntrance().catch(() => ({ data: {} }))
    ])
    const d = s.data || {}
    form.panel_domain = d.panel_domain || ''
    form.sub_require_password = d.sub_require_password === true
    entry.value = (se.data && se.data.path) || '—'
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    loading.value = false
  }
}

async function saveDomain() {
  savingDomain.value = true
  try {
    const r = await misc.saveSettings({ panel_domain: form.panel_domain.trim() })
    const d = r.data || {}
    form.panel_domain = d.panel_domain || ''
    ElMessage.success(form.panel_domain ? '域名已保存' : '已清除域名')
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    savingDomain.value = false
  }
}

async function saveSub() {
  savingSub.value = true
  try {
    const r = await misc.saveSettings({ sub_require_password: form.sub_require_password })
    const d = r.data || {}
    form.sub_require_password = d.sub_require_password === true
    ElMessage.success(form.sub_require_password ? '已开启订阅密码保护' : '已关闭订阅密码保护')
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    savingSub.value = false
  }
}

onMounted(load)
</script>
