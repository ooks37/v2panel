<template>
  <div class="vp-head">
    <h1>订阅</h1>
    <div class="desc">账号、配额与订阅链接</div>
    <div class="spacer"></div>
    <el-button size="small" @click="openBatch">批量生成</el-button>
    <el-button size="small" type="danger" plain @click="resetAll">全部重置流量</el-button>
    <el-button type="primary" size="small" @click="openEdit(null)">新建账号</el-button>
  </div>

  <div v-if="loading" style="text-align:center;padding:40px">
    <el-icon class="is-loading" :size="22"><Loading /></el-icon>
  </div>

  <template v-else>
    <div class="vp-stats">
      <div class="vp-stat">
        <div class="k">账号</div>
        <div class="v">{{ all.length }}</div>
        <div class="m">{{ totalLinks }} 条可用订阅</div>
      </div>
      <div class="vp-stat">
        <div class="k">熔断 / 到期</div>
        <div class="v">{{ lockedCount }}</div>
        <div class="m">
          <el-tag :type="lockedCount ? 'danger' : 'success'" size="small">
            {{ lockedCount ? '已停止下发' : '全部正常' }}
          </el-tag>
        </div>
      </div>
      <div class="vp-stat">
        <div class="k">已暂停</div>
        <div class="v">{{ suspendedCount }}</div>
        <div class="m">{{ suspendedCount ? '手动暂停' : '无' }}</div>
      </div>
      <div class="vp-stat">
        <div class="k">总用量</div>
        <div class="v">{{ humanBytes(sumUsed) }}</div>
        <div class="m">{{ sumLimit > 0 ? '配额合计 ' + humanBytes(sumLimit) : '配额不限量' }}</div>
      </div>
    </div>

    <div v-if="warnings.length" class="vp-note warn">
      <div v-for="(w, i) in warnings" :key="i">{{ w }}</div>
    </div>

    <div class="vp-card">
      <h2>
        账号清单
        <span class="spacer"></span>
        <el-input v-model="q" placeholder="搜索账号 / 备注 / Token" size="small" style="width:250px" clearable />
      </h2>

      <el-table v-if="filtered.length" :data="filtered" class="vp-table" style="width:100%">
        <el-table-column label="账号" min-width="150">
          <template #default="{ row }">
            <b>{{ row.username }}</b>
            <div v-if="row.remark" class="vp-sub">{{ row.remark }}</div>
            <div v-if="row.locked && row.lock_why" class="vp-note bad" style="margin:6px 0 0;padding:4px 8px">
              {{ row.lock_why }}
            </div>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="100">
          <template #default="{ row }">
            <el-tag :type="statusOf(row).type" size="small">{{ statusOf(row).text }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="用量" min-width="180">
          <template #default="{ row }">
            <template v-if="row.traffic_limit > 0">
              <div class="vp-bar">
                <i :class="{ full: pctOf(row) >= 100, warn: pctOf(row) >= 80 && pctOf(row) < 100 }"
                   :style="{ width: pctOf(row) + '%' }"></i>
              </div>
              <div class="vp-sub">{{ humanBytes(row.used_traffic) }} / {{ humanBytes(row.traffic_limit) }}</div>
            </template>
            <div v-else class="vp-sub">{{ humanBytes(row.used_traffic) }} / 不限量</div>
          </template>
        </el-table-column>
        <el-table-column label="到期" width="110">
          <template #default="{ row }">
            <span class="vp-sub">{{ row.expire_text || '永久' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="订阅" width="80" align="right">
          <template #default="{ row }">
            <span class="vp-num">{{ row.link_count || 0 }}</span>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="290" fixed="right">
          <template #default="{ row }">
            <el-button size="small" link type="primary" @click="showSub(row)">订阅</el-button>
            <el-button size="small" link type="primary" @click="openEdit(row)">编辑</el-button>
            <el-button size="small" link type="primary" @click="resetOne(row)">重置流量</el-button>
            <el-button size="small" link type="danger" @click="confirmRemove(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>
      <el-empty v-else :description="q ? '没有匹配的账号。' : '还没有订阅账号'">
        <el-button v-if="!q" type="primary" @click="openEdit(null)">新建账号</el-button>
      </el-empty>
    </div>
  </template>

  <!-- 订阅详情 -->
  <el-dialog v-model="subOpen" :title="(subUser?.username || '') + ' 的订阅'" width="700px">
    <div v-if="subUser?.locked" class="vp-note bad">
      该账号当前<b>{{ subUser.lock_why || '已停止下发' }}</b>，客户端拉取会拿到空配置。
      先重置流量或改到期时间。
    </div>
    <div style="margin-bottom:6px;font-size:12.5px;color:var(--vp-sub)">订阅地址</div>
    <div class="vp-uri">{{ subUrl }}</div>
    <div class="vp-sub" style="margin-top:6px">
      把这个地址填进客户端的「订阅」入口。面板会按 UA 自动返回 Clash / sing-box / Base64。
    </div>

    <div style="text-align:center;margin-top:16px">
      <img :src="qrURL(subUrl)" alt="订阅二维码" style="width:200px;height:200px">
    </div>

    <el-descriptions :column="1" border size="small" style="margin-top:16px">
      <el-descriptions-item label="包含节点">{{ subUser?.link_count || 0 }} 条</el-descriptions-item>
      <el-descriptions-item label="已用 / 配额">
        {{ humanBytes(subUser?.used_traffic) }} /
        {{ subUser?.traffic_limit > 0 ? humanBytes(subUser.traffic_limit) : '不限量' }}
      </el-descriptions-item>
      <el-descriptions-item label="到期">{{ subUser?.expire_text || '永久' }}</el-descriptions-item>
      <el-descriptions-item label="UUID">
        <span class="vp-mono">{{ subUser?.uuid || '—' }}</span>
      </el-descriptions-item>
      <el-descriptions-item label="订阅 Token">
        <span class="vp-mono">{{ subUser?.sub_token || '—' }}</span>
      </el-descriptions-item>
    </el-descriptions>

    <div class="vp-note" style="margin-top:14px">
      UUID 是所有节点共用的客户端鉴权凭据 —— <b>换掉它等于把这个人踢下线</b>，所有客户端都要重新导入订阅。
    </div>

    <template #footer>
      <CopyBtn :text="subUrl" label="复制订阅地址" msg="订阅地址已复制" size="default" />
      <el-button type="danger" plain @click="regenerate">重新生成凭据</el-button>
      <el-button @click="subOpen = false">关闭</el-button>
    </template>
  </el-dialog>

  <!-- 新建 / 编辑 -->
  <el-dialog v-model="editOpen" :title="isNew ? '新建账号' : '编辑 ' + editForm.username" width="560px">
    <el-form label-position="top">
      <el-form-item label="账号名"><el-input v-model="editForm.username" /></el-form-item>
      <el-form-item label="备注">
        <el-input v-model="editForm.remark" placeholder="例如「老王 - 微信」" />
      </el-form-item>
      <el-form-item label="流量配额 (GB)">
        <el-input-number v-model="editForm.quotaGB" :min="0" :step="1" :precision="1" />
        <div class="vp-sub" style="margin-top:5px">留空或 0 表示不限量。</div>
      </el-form-item>
      <el-form-item label="到期时间">
        <el-date-picker v-model="editForm.expireDate" type="date" value-format="YYYY-MM-DD"
          placeholder="留空为永久" style="width:100%" />
        <div class="vp-sub" style="margin-top:5px">按当天 23:59 计。留空 = 永久。</div>
      </el-form-item>

      <template v-if="!isNew">
        <el-form-item label="状态">
          <el-select v-model="editForm.status" style="width:100%">
            <el-option value="active" label="正常" />
            <el-option value="suspended" label="暂停（立即停止下发订阅）" />
            <el-option value="expired" label="已到期" />
          </el-select>
        </el-form-item>
        <el-form-item label="订阅 Token">
          <el-input v-model="editForm.sub_token" class="vp-mono" />
          <div class="vp-sub" style="margin-top:5px">改这个等于换订阅地址。</div>
        </el-form-item>
        <el-form-item label="UUID">
          <el-input v-model="editForm.uuid" class="vp-mono" />
          <div class="vp-sub" style="margin-top:5px">改这个会立刻把该用户踢下线。</div>
        </el-form-item>
      </template>
    </el-form>
    <template #footer>
      <el-button @click="editOpen = false">取消</el-button>
      <el-button type="primary" :loading="saving" @click="saveEdit">{{ isNew ? '创建' : '保存' }}</el-button>
    </template>
  </el-dialog>

  <!-- 批量生成 -->
  <el-dialog v-model="batchOpen" title="批量生成账号" width="560px">
    <div class="vp-note">
      一次最多 200 个。多个账号会按 <code>前缀_1</code>、<code>前缀_2</code> … 命名；单个账号直接用前缀本身。
    </div>
    <el-form label-position="top">
      <el-form-item label="数量">
        <el-input-number v-model="batchForm.count" :min="1" :max="200" />
      </el-form-item>
      <el-form-item label="前缀"><el-input v-model="batchForm.prefix" /></el-form-item>
      <el-form-item label="每人配额 (GB)">
        <el-input-number v-model="batchForm.quotaGB" :min="0" :step="1" :precision="1" />
        <div class="vp-sub" style="margin-top:5px">留空不限量。</div>
      </el-form-item>
      <el-form-item label="到期时间">
        <el-date-picker v-model="batchForm.expireDate" type="date" value-format="YYYY-MM-DD"
          placeholder="留空为永久" style="width:100%" />
      </el-form-item>
    </el-form>
    <pre v-if="batchOut" class="vp-log" style="margin-top:12px">{{ batchOut }}</pre>
    <template #footer>
      <CopyBtn v-if="batchOut" :text="batchOut" label="复制全部订阅地址" msg="已复制" size="default" />
      <el-button v-else @click="batchOpen = false">取消</el-button>
      <el-button type="primary" :loading="batching" @click="doBatch">生成</el-button>
    </template>
  </el-dialog>
</template>

<script setup>
import { ref, reactive, computed, onMounted } from 'vue'
import { Loading } from '@element-plus/icons-vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { users as apiUsers, humanBytes, qrURL } from '../api.js'
import CopyBtn from '../components/CopyBtn.vue'

const DATA = ref({ users: [], total_links: 0, warnings: [], base_url: '' })
const loading = ref(true)
const q = ref('')

const subOpen = ref(false)
const subUser = ref(null)
const subUrl = ref('')

const editOpen = ref(false)
const isNew = ref(true)
const saving = ref(false)
const editForm = reactive({
  id: null, username: '', remark: '', quotaGB: 0, expireDate: '',
  status: 'active', sub_token: '', uuid: ''
})

const batchOpen = ref(false)
const batching = ref(false)
const batchOut = ref('')
const batchForm = reactive({ count: 10, prefix: 'user', quotaGB: 0, expireDate: '' })

const all = computed(() => DATA.value.users || [])
const warnings = computed(() => DATA.value.warnings || [])
const totalLinks = computed(() => DATA.value.total_links || 0)
const lockedCount = computed(() => all.value.filter((u) => u.locked).length)
const suspendedCount = computed(() => all.value.filter((u) => u.status === 'suspended').length)
const sumUsed = computed(() => all.value.reduce((a, u) => a + (u.used_traffic || 0), 0))
const sumLimit = computed(() => all.value.reduce((a, u) => a + (u.traffic_limit || 0), 0))

const filtered = computed(() => {
  const kw = q.value.trim().toLowerCase()
  if (!kw) return all.value
  return all.value.filter((u) =>
    (u.username || '').toLowerCase().includes(kw) ||
    (u.remark || '').toLowerCase().includes(kw) ||
    (u.sub_token || '').toLowerCase().includes(kw)
  )
})

function pctOf(u) {
  if (!u.traffic_limit || u.traffic_limit <= 0) return 0
  return Math.min(100, Math.round(((u.used_traffic || 0) / u.traffic_limit) * 100))
}

function statusOf(u) {
  if (u.status === 'suspended') return { type: 'warning', text: '已暂停' }
  if (u.locked) return { type: 'danger', text: u.status === 'expired' ? '已到期' : '已熔断' }
  if (u.status === 'expired') return { type: 'danger', text: '已到期' }
  return { type: 'success', text: '正常' }
}

function urlOf(u) {
  return (DATA.value.base_url || '') + '/sub/' + (u.sub_token || '')
}

async function load() {
  try {
    const r = await apiUsers.list()
    DATA.value = r.data || DATA.value
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    loading.value = false
  }
}

function showSub(u) {
  subUser.value = u
  subUrl.value = urlOf(u)
  subOpen.value = true
}

function openEdit(u) {
  isNew.value = !u
  if (u) {
    editForm.id = u.id
    editForm.username = u.username || ''
    editForm.remark = u.remark || ''
    editForm.quotaGB = u.traffic_limit > 0 ? +(u.traffic_limit / 1073741824).toFixed(1) : 0
    editForm.expireDate = u.expire_at > 0 ? new Date(u.expire_at * 1000).toISOString().slice(0, 10) : ''
    editForm.status = u.status || 'active'
    editForm.sub_token = u.sub_token || ''
    editForm.uuid = u.uuid || ''
  } else {
    Object.assign(editForm, {
      id: null, username: '', remark: '', quotaGB: 0, expireDate: '',
      status: 'active', sub_token: '', uuid: ''
    })
  }
  editOpen.value = true
}

async function saveEdit() {
  if (!editForm.username.trim()) return ElMessage.warning('请填写账号名')
  const body = {
    username: editForm.username.trim(),
    remark: editForm.remark.trim(),
    traffic_limit: editForm.quotaGB > 0 ? Math.round(editForm.quotaGB * 1073741824) : 0,
    expire_date: editForm.expireDate || ''
  }
  if (!isNew.value) {
    body.status = editForm.status
    body.sub_token = editForm.sub_token.trim()
    body.uuid = editForm.uuid.trim()
  }
  saving.value = true
  try {
    const r = isNew.value ? await apiUsers.create(body) : await apiUsers.update(editForm.id, body)
    ElMessage.success(r.message || '已保存')
    editOpen.value = false
    if (isNew.value && r.data && r.data.id) {
      // 新建后直接把订阅信息摊开给管理员, 省一次点击
      subUser.value = r.data
      subUrl.value = urlOf(r.data)
      subOpen.value = true
    }
    load()
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    saving.value = false
  }
}

async function resetOne(u) {
  try {
    const r = await apiUsers.resetTraffic(u.id)
    ElMessage.success(r.message || '已重置流量')
    load()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function resetAll() {
  try {
    await ElMessageBox.confirm(
      '将把所有账号的已用流量清零，并把处于熔断（暂停）状态的账号恢复为正常。' +
      '因到期而失效的账号不会恢复 —— 那需要改到期时间。',
      '全部重置流量',
      { type: 'warning', confirmButtonText: '全部重置', cancelButtonText: '取消' }
    )
  } catch (e) {
    return
  }
  try {
    const r = await apiUsers.resetAll()
    ElMessage.success(r.message || '已重置')
    load()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function confirmRemove(u) {
  try {
    await ElMessageBox.confirm(
      `删除账号 ${u.username} 后其订阅链接立即失效。机器上的 233boy 配置不受影响，其它账号照常使用。`,
      '删除账号',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消', confirmButtonClass: 'el-button--danger' }
    )
  } catch (e) {
    return
  }
  try {
    const r = await apiUsers.remove(u.id)
    ElMessage.success(r.message || '已删除')
    load()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function regenerate() {
  try {
    await ElMessageBox.confirm(
      '将重新生成该账号的 UUID 与订阅 Token。旧订阅地址和所有已导入的客户端会立即失效，必须重新导入。',
      '重新生成凭据',
      { type: 'warning', confirmButtonText: '生成新凭据', cancelButtonText: '取消' }
    )
  } catch (e) {
    return
  }
  try {
    const r = await apiUsers.regenerate(subUser.value.id)
    ElMessage.success(r.message || '已重新生成')
    subOpen.value = false
    load()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

function openBatch() {
  batchForm.count = 10
  batchForm.prefix = 'user'
  batchForm.quotaGB = 0
  batchForm.expireDate = ''
  batchOut.value = ''
  batchOpen.value = true
}

async function doBatch() {
  batching.value = true
  try {
    const r = await apiUsers.batch({
      count: batchForm.count,
      prefix: batchForm.prefix.trim() || 'user',
      traffic_limit: batchForm.quotaGB > 0 ? Math.round(batchForm.quotaGB * 1073741824) : 0,
      expire_date: batchForm.expireDate || ''
    })
    const list = r.data || []
    batchOut.value = '已生成 ' + list.length + ' 个账号:\n' +
      list.map((u) => '  ' + u.username + '\t' + urlOf(u)).join('\n')
    ElMessage.success('已生成 ' + list.length + ' 个账号')
    load()
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    batching.value = false
  }
}

onMounted(load)
</script>
