<template>
  <div class="vp-head">
    <h1>运营</h1>
    <div class="desc">公告、开号记录与用户名单下发</div>
    <div class="spacer"></div>
    <el-button size="small" :loading="busy" @click="loadAll">刷新</el-button>
    <el-button type="primary" size="small" @click="openEdit(null)">新建公告</el-button>
  </div>

  <div v-if="loading" style="text-align:center;padding:40px">
    <el-icon class="is-loading" :size="22"><Loading /></el-icon>
  </div>

  <template v-else>
    <!-- ── 公告 ── -->
    <div class="vp-card">
      <h2>公告</h2>
      <div class="vp-note">
        公告会显示在<b>用户订阅页顶部</b>。适合发「今晚维护」「线路调整」这类通知 ——
        用户下次打开订阅页就能看到，不用挨个私聊。
      </div>
      <el-table v-if="anns.length" :data="anns" class="vp-table" style="width:100%">
        <el-table-column label="标题" min-width="200">
          <template #default="{ row }">
            <b>{{ row.title }}</b>
            <el-tag v-if="row.pinned" size="small" type="warning" style="margin-left:6px">置顶</el-tag>
            <el-tag v-if="!row.enabled" size="small" type="info" style="margin-left:6px">已停用</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="级别" width="90">
          <template #default="{ row }">
            <el-tag size="small" :type="levelTag(row.level)">{{ levelText(row.level) }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="内容" min-width="260">
          <template #default="{ row }">
            <span class="vp-sub">{{ row.body || '—' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="有效期" min-width="150">
          <template #default="{ row }">
            <span class="vp-sub">{{ periodText(row) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="200" fixed="right">
          <template #default="{ row }">
            <el-button size="small" link type="primary" @click="toggleEnabled(row)">
              {{ row.enabled ? '停用' : '启用' }}
            </el-button>
            <el-button size="small" link type="primary" @click="openEdit(row)">编辑</el-button>
            <el-button size="small" link type="danger" @click="removeAnn(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>
      <el-empty v-else description="还没有公告" :image-size="70" />
    </div>

    <!-- ── 用户名单下发 ── -->
    <div class="vp-card">
      <h2>用户名单下发</h2>
      <div class="vp-note">
        把「哪些账号现在能用」推到节点机器上。平时<b>不需要手动点</b> ——
        配额翻转、开号、删号、节点重新上线时会自动下发。<br>
        这里的手动入口用于排障：<b>机器重启后 conf 被还原</b>、
        或怀疑机器上的用户与面板不一致时，点一下强制对齐。
      </div>
      <el-table v-if="nodes.length" :data="nodes" class="vp-table" style="width:100%">
        <el-table-column label="节点" min-width="160">
          <template #default="{ row }">
            <b>{{ row.node.name }}</b>
            <div class="vp-sub">{{ row.node.address }}</div>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="90">
          <template #default="{ row }">
            <el-tag size="small" :type="row.online ? 'success' : 'info'">
              {{ row.online ? '在线' : '离线' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="内核" width="100">
          <template #default="{ row }">
            <el-tag size="small" :type="row.node.v2ray_status === 'running' ? 'success' : 'danger'">
              {{ row.node.v2ray_status === 'running' ? '运行中' : (row.node.v2ray_status || '未知') }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="150" fixed="right">
          <template #default="{ row }">
            <el-button
              size="small" link type="primary"
              :disabled="!row.online"
              :loading="syncing === row.node.id"
              @click="syncUsers(row)">
              下发用户名单
            </el-button>
          </template>
        </el-table-column>
      </el-table>
      <el-empty v-else description="还没有纳管节点" :image-size="70" />
    </div>

    <!-- ── 订单 / 开号记录 ── -->
    <div class="vp-card">
      <h2>开号记录</h2>
      <div class="vp-note">
        面板<b>不收款</b> —— 这里是审计日志，回答「这个号是谁什么时候给的、
        到期推到哪天」。手工开号时这个问题以前没人答得上来。
      </div>
      <el-table v-if="orders.length" :data="orders" class="vp-table" style="width:100%">
        <el-table-column label="时间" width="160">
          <template #default="{ row }">
            <span class="vp-sub">{{ fmtTime(row.paid_at) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="账号" min-width="130">
          <template #default="{ row }">
            <b>{{ row.username }}</b>
          </template>
        </el-table-column>
        <el-table-column label="套餐" min-width="120">
          <template #default="{ row }">
            <span v-if="row.plan_name">{{ row.plan_name }}</span>
            <span v-else class="vp-sub">自定义</span>
          </template>
        </el-table-column>
        <el-table-column label="类型" width="90">
          <template #default="{ row }">
            <el-tag size="small" type="info">{{ typeText(row.type) }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="周期" min-width="200">
          <template #default="{ row }">
            <span class="vp-sub">{{ periodRange(row) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="流量" width="110" align="right">
          <template #default="{ row }">
            <span class="vp-num">{{ row.traffic_add > 0 ? fmtBytes(row.traffic_add) : '不限' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="备注" min-width="160">
          <template #default="{ row }">
            <span class="vp-sub">{{ row.remark || '—' }}</span>
          </template>
        </el-table-column>
      </el-table>
      <el-empty v-else description="还没有开号记录" :image-size="70" />
    </div>
  </template>

  <!-- ── 公告编辑 ── -->
  <el-dialog v-model="editOpen" :title="isNew ? '新建公告' : '编辑公告'" width="560px">
    <el-form label-position="top">
      <el-form-item label="标题">
        <el-input v-model="form.title" placeholder="例如：今晚 23:00 线路维护" maxlength="80" show-word-limit />
      </el-form-item>
      <el-form-item label="内容">
        <el-input v-model="form.body" type="textarea" :rows="4"
          placeholder="支持换行。留空则只显示标题。" maxlength="500" show-word-limit />
      </el-form-item>
      <el-form-item label="级别">
        <el-radio-group v-model="form.level">
          <el-radio-button value="info">通知</el-radio-button>
          <el-radio-button value="warn">提醒</el-radio-button>
          <el-radio-button value="danger">重要</el-radio-button>
        </el-radio-group>
      </el-form-item>
      <el-form-item label="展示时间（留空表示立即生效 / 永久）">
        <el-date-picker v-model="form.range" type="datetimerange" style="width:100%"
          range-separator="至" start-placeholder="开始" end-placeholder="结束" />
      </el-form-item>
      <el-form-item>
        <el-checkbox v-model="form.pinned">置顶</el-checkbox>
        <el-checkbox v-model="form.enabled" style="margin-left:16px">启用</el-checkbox>
      </el-form-item>
    </el-form>
    <template #footer>
      <el-button @click="editOpen = false">取消</el-button>
      <el-button type="primary" :loading="saving" @click="doSave">保存</el-button>
    </template>
  </el-dialog>
</template>

<script setup>
import { ref, reactive, onMounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Loading } from '@element-plus/icons-vue'
import { announcements as annApi, orders as orderApi, syncNodeUsers } from '../api-ops.js'
import { nodes as nodeApi } from '../api.js'

const anns = ref([])
const orders = ref([])
const nodes = ref([])
const loading = ref(true)
const busy = ref(false)
const syncing = ref(0)

const editOpen = ref(false)
const isNew = ref(true)
const saving = ref(false)
const editingId = ref(0)
const form = reactive({
  title: '', body: '', level: 'info', pinned: false, enabled: true, range: null
})

function levelText(l) {
  return { info: '通知', warn: '提醒', danger: '重要' }[l] || l
}
function levelTag(l) {
  return { info: 'info', warn: 'warning', danger: 'danger' }[l] || 'info'
}
function typeText(t) {
  return { new: '新开', renew: '续费', reset: '重置' }[t] || t || '—'
}
function fmtBytes(n) {
  n = Number(n) || 0
  if (n < 1024) return n + ' B'
  const u = ['KB', 'MB', 'GB', 'TB']
  let i = -1
  do { n /= 1024; i++ } while (n >= 1024 && i < u.length - 1)
  return n.toFixed(2) + ' ' + u[i]
}
function fmtTime(ts) {
  if (!ts) return '—'
  const d = new Date(ts * 1000)
  const p = (x) => String(x).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}
function fmtDay(ts) {
  if (!ts) return '永久'
  const d = new Date(ts * 1000)
  const p = (x) => String(x).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
}
function periodText(row) {
  if (!row.starts_at && !row.ends_at) return '永久'
  return `${row.starts_at ? fmtTime(row.starts_at) : '立即'} → ${row.ends_at ? fmtTime(row.ends_at) : '永久'}`
}
function periodRange(row) {
  if (!row.period_start && !row.period_end) return '—'
  return `${fmtDay(row.period_start)} → ${fmtDay(row.period_end)}`
}

async function loadAll() {
  busy.value = true
  try {
    // ⚠️ api.get 返回的是**整个信封** { code, data, ... }, 不是 data 本身
    // (api.js 顶部的注释说"自动解包", 那是过时的说法 —— 实际调用方都取 .data)。
    const [a, o, n] = await Promise.all([annApi.list(), orderApi.list(), nodeApi.list()])
    anns.value = a.data || []
    orders.value = o.data || []
    // 节点接口的每一项是 { node: {...}, online, inbound_cnt }, 要摊平
    nodes.value = (n.data || []).map((it) => ({ node: it.node || {}, online: it.online === true }))
  } catch (e) {
    ElMessage.error(e.message || '加载失败')
  } finally {
    loading.value = false
    busy.value = false
  }
}

function openEdit(row) {
  isNew.value = !row
  editingId.value = row ? row.id : 0
  form.title = row ? row.title : ''
  form.body = row ? row.body : ''
  form.level = row ? row.level : 'info'
  form.pinned = row ? !!row.pinned : false
  form.enabled = row ? !!row.enabled : true
  form.range = row && (row.starts_at || row.ends_at)
    ? [row.starts_at ? new Date(row.starts_at * 1000) : null, row.ends_at ? new Date(row.ends_at * 1000) : null]
    : null
  editOpen.value = true
}

async function doSave() {
  if (!form.title.trim()) {
    ElMessage.warning('请填写标题')
    return
  }
  saving.value = true
  try {
    const body = {
      title: form.title.trim(),
      body: form.body,
      level: form.level,
      pinned: form.pinned,
      enabled: form.enabled,
      starts_at: form.range && form.range[0] ? Math.floor(form.range[0].getTime() / 1000) : 0,
      ends_at: form.range && form.range[1] ? Math.floor(form.range[1].getTime() / 1000) : 0
    }
    if (isNew.value) {
      await annApi.create(body)
      ElMessage.success('公告已发布')
    } else {
      await annApi.update(editingId.value, body)
      ElMessage.success('公告已更新')
    }
    editOpen.value = false
    await loadAll()
  } catch (e) {
    ElMessage.error(e.message || '保存失败')
  } finally {
    saving.value = false
  }
}

async function toggleEnabled(row) {
  try {
    await annApi.update(row.id, { enabled: !row.enabled })
    ElMessage.success(row.enabled ? '已停用' : '已启用')
    await loadAll()
  } catch (e) {
    ElMessage.error(e.message || '操作失败')
  }
}

async function removeAnn(row) {
  try {
    await ElMessageBox.confirm(`确定删除公告「${row.title}」？`, '删除确认', { type: 'warning' })
  } catch { return }
  try {
    await annApi.remove(row.id)
    ElMessage.success('已删除')
    await loadAll()
  } catch (e) {
    ElMessage.error(e.message || '删除失败')
  }
}

async function syncUsers(row) {
  syncing.value = row.node.id
  try {
    const r = await syncNodeUsers(row.node.id)
    ElMessage.success(r.message || '已下发')
  } catch (e) {
    ElMessage.error(e.message || '下发失败')
  } finally {
    syncing.value = 0
  }
}

onMounted(loadAll)
</script>
