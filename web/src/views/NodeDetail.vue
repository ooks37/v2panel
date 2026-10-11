<template>
  <div class="vp-head">
    <h1>{{ node.name || '节点' }}</h1>
    <div class="desc">{{ node.address || '未设地址' }}</div>
    <div class="spacer"></div>
    <router-link to="/nodes"><el-button size="small">← 节点列表</el-button></router-link>
    <el-button size="small" :loading="busy" @click="load">重新采集</el-button>
    <el-button size="small" @click="editOpen = true">节点设置</el-button>
    <router-link :to="'/servers?id=' + id"><el-button size="small">运维 →</el-button></router-link>
    <el-button type="primary" size="small" @click="openAdd">新增协议</el-button>
  </div>

  <div v-if="loading" style="text-align:center;padding:40px">
    <el-icon class="is-loading" :size="22"><Loading /></el-icon>
  </div>

  <template v-else>
    <div class="vp-stats">
      <div class="vp-stat">
        <div class="k">状态</div>
        <div class="v">{{ online ? '在线' : '离线' }}</div>
        <div class="m">
          <el-tag :type="online ? 'success' : 'danger'" size="small">
            {{ online ? '已连接' : '未连接' }}
          </el-tag>
        </div>
      </div>
      <div class="vp-stat">
        <div class="k">协议实例</div>
        <div class="v">{{ inbounds.length }}</div>
        <div class="m">{{ node.address || '未设地址' }}</div>
      </div>
      <div class="vp-stat">
        <div class="k">机器公网入口</div>
        <div class="v">{{ node.https_port ? ':' + node.https_port : '—' }}</div>
        <div class="m">http_port {{ node.http_port || '—' }}</div>
      </div>
      <div class="vp-stat">
        <div class="k">倍率</div>
        <div class="v">{{ node.rate || 1 }}×</div>
        <div class="m">最后同步 {{ ago(lastSync) }}</div>
      </div>
    </div>

    <div class="vp-card">
      <h2>本机能力</h2>
      <div v-if="!cap" class="vp-note" style="margin-bottom:0">
        该节点还没有上报过能力报文（agent 连上来后会自动上报）。
      </div>
      <el-descriptions v-else :column="1" border size="small">
        <el-descriptions-item label="233boy 内核">{{ cap.core_version || '—' }}</el-descriptions-item>
        <el-descriptions-item label="Caddy">
          {{ cap.has_caddy ? (cap.caddy_version || '已安装') : '未安装' }}
        </el-descriptions-item>
        <el-descriptions-item label="配置目录">
          <span class="vp-mono">{{ cap.conf_dir || '—' }}</span>
        </el-descriptions-item>
        <el-descriptions-item label="Reality">
          <el-tag :type="cap.reality ? 'success' : 'danger'" size="small">
            {{ cap.reality ? '可用' : '不可用' }}
          </el-tag>
          <span v-if="!cap.reality" class="vp-sub" style="margin-left:8px">{{ cap.reality_why }}</span>
        </el-descriptions-item>
        <el-descriptions-item label="保护名单">
          <template v-if="(cap.protected || []).length">
            <el-tag v-for="p in cap.protected" :key="p" size="small" class="vp-mono" style="margin-right:5px">{{ p }}</el-tag>
            <span class="vp-sub">这些配置面板拒绝删除</span>
          </template>
          <span v-else class="vp-sub">无 —— 面板可删该机任意配置</span>
        </el-descriptions-item>
      </el-descriptions>
    </div>

    <div class="vp-card">
      <h2>
        协议实例
        <span class="spacer"></span>
        <el-button size="small" :loading="busy" @click="load">重新采集</el-button>
      </h2>

      <el-table v-if="inbounds.length" :data="inbounds" class="vp-table" style="width:100%">
        <el-table-column label="配置" min-width="200">
          <template #default="{ row }">
            <b class="vp-mono">{{ row.conf_name }}</b>
            <div v-if="row.remark" class="vp-sub">{{ row.remark }}</div>
            <div v-if="(row.warnings || []).length" class="vp-note bad" style="margin:8px 0 0;padding:6px 9px">
              <div v-for="(w, i) in row.warnings" :key="i">{{ w }}</div>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="协议" min-width="130">
          <template #default="{ row }">
            {{ protoName(row.protocol) }}
            <span v-if="row.network" class="vp-sub">{{ row.network }}</span>
            <div class="vp-sub">
              {{ row.inbound_security || '' }}
              <template v-if="row.client_security && row.client_security !== row.inbound_security">
                → {{ row.client_security }}
              </template>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="对外地址" min-width="170">
          <template #default="{ row }">
            <span class="vp-mono">{{ displayAddr(row) }}</span>
            <div class="vp-sub vp-mono">{{ row.path || '—' }}</div>
          </template>
        </el-table-column>
        <el-table-column label="内网口" width="110">
          <template #default="{ row }">
            <span class="vp-mono">{{ row.internal_port || '—' }}</span>
            <div class="vp-sub">{{ row.listen_addr === '0.0.0.0' ? '直连' : '经 Caddy' }}</div>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="90">
          <template #default="{ row }">
            <el-tag :type="row.enabled === false ? 'warning' : 'success'" size="small">
              {{ row.enabled === false ? '已停用' : '启用中' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="250" fixed="right">
          <template #default="{ row }">
            <el-button v-if="row.uri" size="small" link type="primary" @click="showURI(row)">链接</el-button>
            <el-button size="small" link type="primary" :loading="probing === row.conf_name" @click="probe(row)">探测</el-button>
            <el-button size="small" link type="primary" @click="toggle(row)">
              {{ row.enabled === false ? '启用' : '停用' }}
            </el-button>
            <el-button size="small" link type="danger" @click="confirmDelete(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>
      <el-empty v-else description="该节点还没有任何 233boy 配置">
        <el-button type="primary" @click="openAdd">新增协议</el-button>
      </el-empty>
    </div>
  </template>

  <!-- 客户端链接 -->
  <el-dialog v-model="uriOpen" :title="uriRow?.conf_name || '客户端链接'" width="680px">
    <pre class="vp-log">{{ uriRow?.uri }}</pre>
    <div style="text-align:center;margin-top:16px">
      <img :src="qrURL(uriRow?.uri)" alt="二维码" style="width:200px;height:200px">
    </div>
    <template #footer>
      <CopyBtn :text="uriRow?.uri || ''" label="复制链接" msg="链接已复制" size="default" />
      <el-button @click="uriOpen = false">关闭</el-button>
    </template>
  </el-dialog>

  <!-- 探测结果 -->
  <el-dialog v-model="probeOpen" :title="'探测 ' + probeConf" width="560px">
    <div class="vp-note" :class="probeRes.ok ? 'ok' : 'bad'">
      {{ probeRes.ok ? '链路可用' : (probeRes.error || '探测失败') }}
    </div>
    <div v-if="(probeRes.steps || []).length" class="vp-steps">
      <div v-for="(s, i) in probeRes.steps" :key="i" class="vp-step">
        <span :style="{ color: s.ok ? 'var(--vp-ok)' : 'var(--vp-danger)' }">{{ s.ok ? '✓' : '✕' }}</span>
        <span>{{ s.name }}<span v-if="s.detail" class="vp-sub"> — {{ s.detail }}</span></span>
      </div>
    </div>
    <template #footer><el-button type="primary" @click="probeOpen = false">知道了</el-button></template>
  </el-dialog>

  <!-- 节点设置 -->
  <el-dialog v-model="editOpen" title="节点设置" width="560px">
    <el-form label-position="top">
      <el-form-item label="节点名称"><el-input v-model="edit.name" /></el-form-item>
      <el-form-item label="访问地址"><el-input v-model="edit.address" /></el-form-item>
      <el-form-item label="流量倍率">
        <el-input-number v-model="edit.rate" :min="0.1" :step="0.1" :precision="1" />
      </el-form-item>
      <el-form-item label="保护名单">
        <el-input v-model="edit.protected" placeholder="留空 = 不保护任何配置" />
      </el-form-item>
    </el-form>
    <div class="vp-note warn" style="margin-bottom:0">
      保护名单是在<b>安装 agent 时</b>写进机器上配置文件的。改完这里要<b>重跑一次纳管命令</b>
      （幂等，只覆盖配置与二进制）才会在该机器上生效。
    </div>
    <template #footer>
      <el-button @click="editOpen = false">取消</el-button>
      <el-button type="primary" :loading="saving" @click="saveEdit">保存</el-button>
    </template>
  </el-dialog>

  <!-- 新增协议 -->
  <AddInboundDialog v-model="addOpen" :node-id="id" :protocols="protocols" :meta="protoMeta" @done="load" />
</template>

<script setup>
import { ref, reactive, onMounted, watch } from 'vue'
import { useRoute } from 'vue-router'
import { Loading } from '@element-plus/icons-vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { nodes as apiNodes, misc, ago, qrURL } from '../api.js'
import CopyBtn from '../components/CopyBtn.vue'
import AddInboundDialog from '../components/AddInboundDialog.vue'

const route = useRoute()
const id = route.params.id

const node = ref({})
const inbounds = ref([])
const cap = ref(null)
const protocols = ref([])
const protoMeta = ref({})
const online = ref(false)
const lastSync = ref(null)
const loading = ref(true)
const busy = ref(false)
const probing = ref(null)
const saving = ref(false)

const uriOpen = ref(false)
const uriRow = ref(null)
const probeOpen = ref(false)
const probeConf = ref('')
const probeRes = ref({})
const editOpen = ref(false)
const addOpen = ref(false)
const edit = reactive({ name: '', address: '', rate: 1, protected: '' })

function protoName(token) {
  const p = (protocols.value || []).find((x) => x.token === token)
  return p ? p.name : (token || '—')
}

function displayAddr(row) {
  if (row.address) return row.address + (row.public_port ? ':' + row.public_port : '')
  if (node.value.address) return node.value.address + (node.value.https_port ? ':' + node.value.https_port : '')
  return '—'
}

async function load() {
  busy.value = true
  try {
    const [g, inb, pro] = await Promise.all([
      apiNodes.get(id),
      apiNodes.inbounds(id).catch(() => ({ data: [] })),
      misc.protocols().catch(() => ({ data: {} }))
    ])
    const d = g.data || {}
    node.value = d.node || {}
    cap.value = d.capability || null
    online.value = d.online === true
    inbounds.value = (inb.data || []).map((it) => ({ ...(it.inbound || {}), warnings: it.warnings || [] }))
    lastSync.value = inb.last_sync || node.value.last_sync_at
    const p = pro.data || {}
    protocols.value = p.protocols || []
    protoMeta.value = p

    edit.name = node.value.name || ''
    edit.address = node.value.address || ''
    edit.rate = node.value.rate || 1
    edit.protected = node.value.protected || ''
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    loading.value = false
    busy.value = false
  }
}

function showURI(row) {
  uriRow.value = row
  uriOpen.value = true
}

async function probe(row) {
  probing.value = row.conf_name
  try {
    const r = await apiNodes.probeInbound(id, row.conf_name, 8)
    probeConf.value = row.conf_name
    probeRes.value = r.data || {}
    probeOpen.value = true
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    probing.value = null
  }
}

async function toggle(row) {
  const on = row.enabled === false
  try {
    await apiNodes.updateInbounds(id, [{ id: row.id, enabled: on }])
    ElMessage.success(on ? '已启用' : '已停用')
    load()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function confirmDelete(row) {
  try {
    await ElMessageBox.confirm(
      `将从机器上删除配置 ${row.conf_name}，对应的 Caddy 路由会一并移除，` +
      `该节点上正在用这条链接的客户端会立刻断线。`,
      '删除协议实例',
      { type: 'warning', confirmButtonText: '确认删除', cancelButtonText: '取消', confirmButtonClass: 'el-button--danger' }
    )
  } catch (e) {
    return
  }
  try {
    const r = await apiNodes.deleteInbound(id, row.conf_name)
    ElMessage.success(r.message || '已删除')
    load()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function saveEdit() {
  if (!edit.name.trim()) return ElMessage.warning('请填写节点名称')
  saving.value = true
  try {
    const r = await apiNodes.update(id, {
      name: edit.name.trim(),
      address: edit.address.trim(),
      rate: edit.rate || 1,
      protected: edit.protected.trim()
    })
    ElMessage.success(r.message || '已保存')
    editOpen.value = false
    load()
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    saving.value = false
  }
}

function openAdd() {
  addOpen.value = true
}

onMounted(load)
</script>
