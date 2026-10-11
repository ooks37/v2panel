<template>
  <div class="vp-head">
    <h1>套餐</h1>
    <div class="desc">订阅计划模板：流量额度、有效期、设备数与重置方式</div>
    <div class="spacer"></div>
    <el-button size="small" :loading="busy" @click="load">刷新</el-button>
    <el-button type="primary" size="small" @click="openEdit(null)">新建套餐</el-button>
  </div>

  <div v-if="loading" style="text-align:center;padding:40px">
    <el-icon class="is-loading" :size="22"><Loading /></el-icon>
  </div>

  <template v-else>
    <div class="vp-note">
      套餐是<b>开号模板</b>：开号时把套餐的值<b>快照</b>进用户，之后两者各自独立。
      改套餐<b>不会</b>影响已开出去的用户 —— 想同步到存量用户，用每行的「同步到用户」按钮。
    </div>

    <div class="vp-card">
      <h2>套餐列表</h2>
      <el-table v-if="list.length" :data="list" class="vp-table" style="width:100%">
        <el-table-column label="套餐" min-width="180">
          <template #default="{ row }">
            <b>{{ row.name }}</b>
            <el-tag v-if="row.for_sale" size="small" type="success" style="margin-left:6px">在售</el-tag>
            <div v-if="row.desc" class="vp-sub">{{ row.desc }}</div>
          </template>
        </el-table-column>
        <el-table-column label="流量" width="110" align="right">
          <template #default="{ row }">
            <span class="vp-num">{{ row.traffic_text }}</span>
          </template>
        </el-table-column>
        <el-table-column label="有效期" width="100" align="right">
          <template #default="{ row }">
            <span class="vp-num">{{ row.duration_days > 0 ? row.duration_days + ' 天' : '永久' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="设备数" width="90" align="right">
          <template #default="{ row }">
            <span class="vp-num">{{ row.device_limit > 0 ? row.device_limit + ' 台' : '不限' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="重置方式" min-width="130">
          <template #default="{ row }">
            {{ resetLabel(row.reset_mode) }}
          </template>
        </el-table-column>
        <el-table-column label="节点" min-width="110">
          <template #default="{ row }">
            <span v-if="!row.node_ids || !row.node_ids.length" class="vp-sub">全部节点</span>
            <el-tag v-else size="small">{{ row.node_ids.length }} 个</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="用户" width="100" align="right">
          <template #default="{ row }">
            <span class="vp-num">{{ row.user_count }}</span>
            <span v-if="row.max_users > 0" class="vp-sub">/ {{ row.max_users }}</span>
          </template>
        </el-table-column>
        <el-table-column label="价格" width="90" align="right">
          <template #default="{ row }">
            <span class="vp-num">{{ row.price_cents > 0 ? '¥' + (row.price_cents / 100).toFixed(2) : '—' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="230" fixed="right">
          <template #default="{ row }">
            <el-button size="small" link type="primary" @click="openEdit(row)">编辑</el-button>
            <el-button size="small" link type="primary" :disabled="!row.user_count"
              @click="openApply(row)">同步到用户</el-button>
            <el-button size="small" link type="danger" @click="confirmRemove(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>
      <el-empty v-else description="还没有套餐">
        <div class="vp-note" style="text-align:left;margin-bottom:14px">
          套餐不是必须的 —— 你也可以在每个账号上单独填配额（账号编辑里选「自定义」）。
          但用户一多，用套餐能省掉每次重复填同样几个数字。
        </div>
        <el-button type="primary" @click="openEdit(null)">新建套餐</el-button>
      </el-empty>
    </div>
  </template>

  <!-- 新建 / 编辑 -->
  <el-dialog v-model="editOpen" :title="isNew ? '新建套餐' : '编辑套餐 · ' + form.name" width="600px">
    <el-form label-position="top">
      <el-form-item label="套餐名称">
        <el-input v-model="form.name" placeholder="例如 基础版 / 标准版" />
      </el-form-item>
      <el-form-item label="说明">
        <el-input v-model="form.desc" placeholder="一句话描述，展示给用户" />
      </el-form-item>

      <div class="form-row">
        <el-form-item label="流量额度 (GB)">
          <el-input-number v-model="form.quotaGB" :min="0" :step="10" :precision="1" style="width:100%" />
          <div class="vp-sub" style="margin-top:5px">0 = 不限量。</div>
        </el-form-item>
        <el-form-item label="有效期 (天)">
          <el-input-number v-model="form.durationDays" :min="0" :step="30" style="width:100%" />
          <div class="vp-sub" style="margin-top:5px">0 = 永久。</div>
        </el-form-item>
      </div>

      <el-form-item label="流量重置方式">
        <el-radio-group v-model="form.reset_mode">
          <el-radio v-for="m in RESET_MODES" :key="m.value" :value="m.value">{{ m.label }}</el-radio>
        </el-radio-group>
        <div class="vp-sub" style="margin-top:5px">
          {{ (RESET_MODES.find(m => m.value === form.reset_mode) || {}).hint }}
        </div>
      </el-form-item>

      <div class="form-row">
        <el-form-item label="设备数上限">
          <el-input-number v-model="form.device_limit" :min="0" style="width:100%" />
          <div class="vp-sub" style="margin-top:5px">0 = 不限。</div>
        </el-form-item>
        <el-form-item label="最多用户数">
          <el-input-number v-model="form.max_users" :min="0" style="width:100%" />
          <div class="vp-sub" style="margin-top:5px">0 = 不限，满员后拒绝分配。</div>
        </el-form-item>
      </div>

      <el-form-item label="可用节点">
        <el-select v-model="form.node_ids" multiple style="width:100%" placeholder="留空 = 全部节点">
          <el-option v-for="n in nodeList" :key="n.id" :value="n.id" :label="n.name" />
        </el-select>
        <div class="vp-sub" style="margin-top:5px">
          留空表示该套餐的用户能用所有节点。选了就是白名单 —— 不在名单上的节点不会给这些用户下发凭据。
        </div>
      </el-form-item>

      <div class="form-row">
        <el-form-item label="价格 (元)">
          <el-input-number v-model="form.priceYuan" :min="0" :step="10" :precision="2" style="width:100%" />
          <div class="vp-sub" style="margin-top:5px">仅展示用，本面板不收款。</div>
        </el-form-item>
        <el-form-item label="排序">
          <el-input-number v-model="form.sort" :min="0" style="width:100%" />
        </el-form-item>
      </div>

      <el-checkbox v-model="form.for_sale">在商店/门户展示这个套餐</el-checkbox>
    </el-form>
    <template #footer>
      <el-button @click="editOpen = false">取消</el-button>
      <el-button type="primary" :loading="saving" @click="save">{{ isNew ? '创建' : '保存' }}</el-button>
    </template>
  </el-dialog>

  <!-- 同步到存量用户 -->
  <el-dialog v-model="applyOpen" :title="'同步套餐到用户 · ' + applyPlan?.name" width="520px">
    <div class="vp-note warn">
      该套餐下有 <b>{{ applyPlan?.user_count }}</b> 个账号。选中的项会<b>覆盖</b>这些账号的现有值。
      没选的不动。
    </div>
    <el-checkbox v-model="applyForm.traffic">流量额度（{{ applyPlan?.traffic_text }}）</el-checkbox>
    <div style="margin:8px 0"><el-checkbox v-model="applyForm.reset">重置方式（{{ resetLabel(applyPlan?.reset_mode) }}）</el-checkbox></div>
    <div style="margin:8px 0"><el-checkbox v-model="applyForm.devices">设备数上限</el-checkbox></div>
    <div style="margin:8px 0">
      <el-checkbox v-model="applyForm.expire">到期时间</el-checkbox>
      <div class="vp-sub" style="margin-left:24px">
        勾了会从<b>今天</b>起按套餐的 {{ applyPlan?.duration_days }} 天重算到期日 —— 这会改变所有用户的到期时间。
      </div>
    </div>
    <template #footer>
      <el-button @click="applyOpen = false">取消</el-button>
      <el-button type="primary" :loading="applying" @click="doApply">确认同步</el-button>
    </template>
  </el-dialog>
</template>

<script setup>
import { ref, reactive, onMounted } from 'vue'
import { Loading } from '@element-plus/icons-vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { nodes as apiNodes } from '../api.js'
import { plans as apiPlans, RESET_MODES, gbToBytes, bytesToGB } from '../api-ops.js'

const list = ref([])
const nodeList = ref([])
const loading = ref(true)
const busy = ref(false)

const editOpen = ref(false)
const isNew = ref(true)
const saving = ref(false)
const form = reactive({
  id: null, name: '', desc: '', quotaGB: 0, durationDays: 30,
  reset_mode: 'monthly', device_limit: 0, max_users: 0,
  node_ids: [], priceYuan: 0, sort: 0, for_sale: false
})

const applyOpen = ref(false)
const applying = ref(false)
const applyPlan = ref(null)
const applyForm = reactive({ traffic: true, reset: false, devices: false, expire: false })

function resetLabel(m) {
  const x = RESET_MODES.find((r) => r.value === m)
  return x ? x.label : '每月 1 号重置'
}

async function load() {
  busy.value = true
  try {
    const [p, n] = await Promise.all([
      apiPlans.list(),
      apiNodes.list().catch(() => ({ data: [] }))
    ])
    list.value = p.data || []
    nodeList.value = (n.data || []).map((x) => x.node || {}).filter((x) => x.id)
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    loading.value = false
    busy.value = false
  }
}

function openEdit(row) {
  isNew.value = !row
  if (row) {
    Object.assign(form, {
      id: row.id,
      name: row.name || '',
      desc: row.desc || '',
      quotaGB: bytesToGB(row.traffic_limit),
      durationDays: row.duration_days || 0,
      reset_mode: row.reset_mode || 'monthly',
      device_limit: row.device_limit || 0,
      max_users: row.max_users || 0,
      node_ids: row.node_ids || [],
      priceYuan: (row.price_cents || 0) / 100,
      sort: row.sort || 0,
      for_sale: !!row.for_sale
    })
  } else {
    Object.assign(form, {
      id: null, name: '', desc: '', quotaGB: 0, durationDays: 30,
      reset_mode: 'monthly', device_limit: 0, max_users: 0,
      node_ids: [], priceYuan: 0, sort: 0, for_sale: false
    })
  }
  editOpen.value = true
}

async function save() {
  if (!form.name.trim()) return ElMessage.warning('请填写套餐名称')
  const body = {
    name: form.name.trim(),
    desc: form.desc.trim(),
    traffic_limit: gbToBytes(form.quotaGB),
    duration_days: form.durationDays || 0,
    reset_mode: form.reset_mode,
    device_limit: form.device_limit || 0,
    max_users: form.max_users || 0,
    node_ids: form.node_ids || [],
    price_cents: Math.round((form.priceYuan || 0) * 100),
    sort: form.sort || 0,
    for_sale: !!form.for_sale
  }
  saving.value = true
  try {
    const r = isNew.value
      ? await apiPlans.create(body)
      : await apiPlans.update(form.id, body)
    ElMessage.success(r.message || '已保存')
    editOpen.value = false
    load()
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    saving.value = false
  }
}

function openApply(row) {
  applyPlan.value = row
  applyForm.traffic = true
  applyForm.reset = false
  applyForm.devices = false
  applyForm.expire = false
  applyOpen.value = true
}

async function doApply() {
  applying.value = true
  try {
    const r = await apiPlans.apply(applyPlan.value.id, {
      apply_traffic: applyForm.traffic,
      apply_reset: applyForm.reset,
      apply_devices: applyForm.devices,
      apply_expire: applyForm.expire
    })
    ElMessage.success(r.message || '已同步')
    applyOpen.value = false
    load()
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    applying.value = false
  }
}

async function confirmRemove(row) {
  const tip = row.user_count > 0
    ? `该套餐下有 ${row.user_count} 个账号，删除套餐后这些账号保留原配额不变。`
    : '删除后不可恢复。'
  try {
    await ElMessageBox.confirm(tip, '删除套餐 ' + row.name,
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消', confirmButtonClass: 'el-button--danger' })
  } catch (e) {
    return
  }
  try {
    const r = await apiPlans.remove(row.id)
    ElMessage.success(r.message || '已删除')
    load()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

onMounted(load)
</script>
