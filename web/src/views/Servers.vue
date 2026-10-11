<template>
  <div class="vp-head">
    <h1>运维</h1>
    <div class="desc">机器状态、日志与内核重启</div>
    <div class="spacer"></div>
    <el-select v-if="list.length" v-model="curId" style="width:220px" @change="onPick">
      <el-option
        v-for="n in list"
        :key="n.id"
        :value="n.id"
        :label="n.name + (n._online ? '' : '（离线）')"
      />
    </el-select>
    <el-button size="small" :loading="busy" @click="load">重新采集</el-button>
    <el-button size="small" type="danger" plain @click="restart">重启内核</el-button>
    <el-button size="small" @click="cmdOpen = true">纳管命令</el-button>
  </div>

  <div v-if="loading" style="text-align:center;padding:40px">
    <el-icon class="is-loading" :size="22"><Loading /></el-icon>
  </div>

  <div v-else-if="!list.length" class="vp-card">
    <el-empty description="还没有纳管任何节点">
      <div class="vp-note" style="text-align:left;margin-bottom:14px">
        运维页需要先有节点。到「节点」页新建一个，再把一键纳管命令在目标机上执行。
      </div>
      <router-link to="/nodes"><el-button type="primary">去新建节点</el-button></router-link>
    </el-empty>
  </div>

  <template v-else-if="cur">
    <div class="vp-stats">
      <div class="vp-stat">
        <div class="k">CPU</div>
        <div class="v">{{ (cur.cpu_percent || 0).toFixed(1) }}<small>%</small></div>
        <div class="vp-bar"><i :style="{ width: Math.min(100, cur.cpu_percent || 0) + '%' }"></i></div>
      </div>
      <div class="vp-stat">
        <div class="k">内存</div>
        <div class="v">{{ humanBytes(cur.mem_used) }}<small> / {{ humanBytes(cur.mem_total) }}</small></div>
        <div class="vp-bar"><i :style="{ width: memPct + '%' }"></i></div>
      </div>
      <div class="vp-stat">
        <div class="k">实时速率</div>
        <div class="v" style="font-size:19px">↓ {{ humanBytes(cur.net_rx_rate) }}/s</div>
        <div class="m">↑ {{ humanBytes(cur.net_tx_rate) }}/s</div>
      </div>
      <div class="vp-stat">
        <div class="k">运行时长</div>
        <div class="v" style="font-size:19px">{{ cur.uptime_sec ? humanUptime(cur.uptime_sec) : '—' }}</div>
        <div class="m">agent {{ cur.agent_ver || '未知' }}</div>
      </div>
    </div>

    <div class="vp-card">
      <h2>内核进程</h2>
      <el-descriptions :column="1" border size="small">
        <el-descriptions-item label="v2ray">
          <el-tag :type="runTag(cur.v2ray_status)" size="small">{{ runText(cur.v2ray_status) }}</el-tag>
          <span class="vp-mono vp-sub" style="margin-left:8px">
            {{ cur.v2ray_ver || '—' }}{{ cur.v2ray_pid ? ' · pid ' + cur.v2ray_pid : '' }}
          </span>
        </el-descriptions-item>
        <el-descriptions-item label="Caddy">
          <el-tag :type="runTag(cur.caddy_status)" size="small">{{ runText(cur.caddy_status) }}</el-tag>
          <span class="vp-mono vp-sub" style="margin-left:8px">{{ cur.caddy_ver || '—' }}</span>
        </el-descriptions-item>
        <el-descriptions-item label="公网入口">
          <span class="vp-mono">{{ cur.address || '—' }}</span>
          <el-tag v-if="cur.https_port" size="small" style="margin-left:8px">https {{ cur.https_port }}</el-tag>
          <el-tag v-if="cur.http_port" size="small" style="margin-left:5px">http {{ cur.http_port }}</el-tag>
        </el-descriptions-item>
        <el-descriptions-item label="最后心跳">
          {{ ago(cur.last_seen_at) }} <span class="vp-sub">{{ fmtTime(cur.last_seen_at) }}</span>
        </el-descriptions-item>
        <el-descriptions-item label="最后采集">
          {{ ago(cur.last_sync_at) }} <span class="vp-sub">{{ fmtTime(cur.last_sync_at) }}</span>
        </el-descriptions-item>
        <el-descriptions-item label="部署状态">
          <el-tag :type="deployTag(cur.deploy_status).type" size="small">{{ deployTag(cur.deploy_status).text }}</el-tag>
          <span v-if="cur.deploy_host" class="vp-mono vp-sub" style="margin-left:8px">{{ cur.deploy_host }}</span>
        </el-descriptions-item>
      </el-descriptions>
    </div>

    <div class="vp-card">
      <h2>
        日志
        <span class="spacer"></span>
      </h2>
      <div style="display:flex;gap:9px;flex-wrap:wrap;margin-bottom:12px">
        <el-select v-model="logSource" style="width:130px" size="small">
          <el-option value="v2ray" label="v2ray" />
          <el-option value="caddy" label="caddy" />
          <el-option value="agent" label="agent" />
        </el-select>
        <el-select v-model="logLines" style="width:120px" size="small">
          <el-option :value="100" label="100 行" />
          <el-option :value="200" label="200 行" />
          <el-option :value="500" label="500 行" />
          <el-option :value="1000" label="1000 行" />
        </el-select>
        <el-button size="small" :loading="logBusy" @click="fetchLogs">拉取</el-button>
        <CopyBtn :text="logText" label="复制" msg="日志已复制" />
      </div>
      <pre class="vp-log">{{ logText }}</pre>
    </div>
  </template>

  <!-- 纳管命令: token 只在创建时回显一次, 这里只能走重新签发 -->
  <el-dialog v-model="cmdOpen" title="纳管命令" width="680px">
    <div class="vp-note">
      agent 的接入 Token 只在<b>创建节点时</b>回显一次，之后面板不再保存明文副本。
      需要重新获取时，点下面的按钮为当前节点<b>重新签发</b> Token —— 旧命令会立即失效。
    </div>
    <pre v-if="newCmd" class="vp-log">{{ newCmd }}</pre>
    <template #footer>
      <CopyBtn v-if="newCmd" :text="newCmd" label="复制命令" msg="命令已复制" size="default" />
      <el-button :loading="rotating" @click="rotateToken">重新签发并显示</el-button>
      <el-button type="primary" @click="cmdOpen = false">关闭</el-button>
    </template>
  </el-dialog>
</template>

<script setup>
import { ref, computed, onMounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { Loading } from '@element-plus/icons-vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { nodes as apiNodes, humanBytes, ago, fmtTime } from '../api.js'
import CopyBtn from '../components/CopyBtn.vue'

const route = useRoute()
const router = useRouter()

const list = ref([])
const curId = ref(null)
const loading = ref(true)
const busy = ref(false)

const logSource = ref('v2ray')
const logLines = ref(200)
const logText = ref('点「拉取」读取远端日志。')
const logBusy = ref(false)

const cmdOpen = ref(false)
const newCmd = ref('')
const rotating = ref(false)

const cur = computed(() => list.value.find((n) => n.id === curId.value) || null)
const memPct = computed(() => {
  const n = cur.value
  if (!n || !n.mem_total) return 0
  return Math.round((n.mem_used / n.mem_total) * 100)
})

async function load() {
  busy.value = true
  try {
    const r = await apiNodes.list()
    list.value = (r.data || []).map((it) => {
      const n = it.node || {}
      n._online = it.online === true
      return n
    })
    if (list.value.length) {
      const want = route.query.id
      const found = want && list.value.find((n) => String(n.id) === String(want))
      if (found) curId.value = found.id
      else if (!curId.value || !list.value.find((n) => n.id === curId.value)) {
        const on = list.value.find((n) => n._online)
        curId.value = (on || list.value[0]).id
      }
    }
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    loading.value = false
    busy.value = false
  }
}

function onPick(v) {
  router.replace('/servers?id=' + v)
  logText.value = '点「拉取」读取远端日志。'
}

async function restart() {
  try {
    await ElMessageBox.confirm(
      '重启 233boy 内核会短暂中断该机器上所有节点的连接（通常 1–3 秒），Caddy 也会一并重启。',
      '重启内核',
      { type: 'warning', confirmButtonText: '重启', cancelButtonText: '取消' }
    )
  } catch (e) {
    return
  }
  try {
    const r = await apiNodes.restart(curId.value)
    const d = r.data || {}
    ElMessage.success((d.message || '已重启') + (d.v2ray_pid ? ' · v2ray pid ' + d.v2ray_pid : ''))
    load()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function fetchLogs() {
  logBusy.value = true
  logText.value = '拉取中…'
  try {
    const r = await apiNodes.logs(curId.value, logSource.value, logLines.value)
    const d = r.data || {}
    logText.value = d.error ? '[' + d.error + ']\n' + (d.content || '') : (d.content || '（无输出）')
  } catch (e) {
    logText.value = '拉取失败：' + e.message
  } finally {
    logBusy.value = false
  }
}

async function rotateToken() {
  rotating.value = true
  try {
    const r = await apiNodes.rotateToken(curId.value)
    const d = r.data || {}
    newCmd.value = d.install_cmd || JSON.stringify(d, null, 2)
    ElMessage.success('已重新签发')
  } catch (e) {
    if (e.http === 404 || e.code === 404) {
      ElMessage.error('当前后端还不支持重新签发 Token（该接口属于 M3 一键安装的一部分）')
    } else {
      ElMessage.error(e.message)
    }
  } finally {
    rotating.value = false
  }
}

function runTag(s) {
  if (s === 'running') return 'success'
  if (s === 'error') return 'danger'
  return 'warning'
}
function runText(s) {
  if (s === 'running') return '运行中'
  if (s === 'error') return '异常'
  return '已停止'
}
function deployTag(s) {
  if (s === 'running') return { type: 'warning', text: '安装中' }
  if (s === 'success') return { type: 'success', text: '成功' }
  if (s === 'failed') return { type: 'danger', text: '失败' }
  return { type: 'info', text: '未执行' }
}
function humanUptime(s) {
  const dd = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  return (dd ? dd + ' 天 ' : '') + (h || dd ? h + ' 小时 ' : '') + m + ' 分'
}

onMounted(load)
</script>
