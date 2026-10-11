<template>
  <div class="vp-head">
    <h1>节点</h1>
    <div class="desc">纳管机器与 233boy 协议实例</div>
    <div class="spacer"></div>
    <el-button size="small" :loading="busy" @click="load">刷新</el-button>
    <el-button type="primary" size="small" @click="openCreate">新建节点</el-button>
  </div>

  <div v-if="loading" style="text-align:center;padding:40px">
    <el-icon class="is-loading" :size="22"><Loading /></el-icon>
  </div>

  <div v-else class="vp-card">
    <h2>节点清单</h2>
    <el-table v-if="rows.length" :data="rows" class="vp-table" style="width:100%">
      <el-table-column label="名称" min-width="150">
        <template #default="{ row }">
          <router-link :to="'/nodes/' + row.id">{{ row.name }}</router-link>
          <el-tag v-if="row.hidden" size="small" style="margin-left:6px">隐藏</el-tag>
          <div v-if="row.protected" class="vp-sub">
            保护：<span class="vp-mono">{{ row.protected }}</span>
          </div>
        </template>
      </el-table-column>
      <el-table-column label="地址" min-width="170">
        <template #default="{ row }">
          <span class="vp-mono">{{ row.address || '—' }}</span>
        </template>
      </el-table-column>
      <el-table-column label="状态" width="100">
        <template #default="{ row }">
          <el-tag :type="row.online ? 'success' : 'danger'" size="small">
            {{ row.online ? '在线' : '离线' }}
          </el-tag>
        </template>
      </el-table-column>
      <el-table-column label="入站" width="80" align="right">
        <template #default="{ row }"><span class="vp-num">{{ row.inbound_cnt || 0 }}</span></template>
      </el-table-column>
      <el-table-column label="倍率" width="80" align="right">
        <template #default="{ row }"><span class="vp-num">{{ row.rate || 1 }}×</span></template>
      </el-table-column>
      <el-table-column label="v2ray" min-width="110">
        <template #default="{ row }">
          <span class="vp-mono vp-sub">{{ row.v2ray_ver || '—' }}</span>
        </template>
      </el-table-column>
      <el-table-column label="最后同步" min-width="110">
        <template #default="{ row }">
          <span class="vp-sub">{{ ago(row.last_sync_at) }}</span>
        </template>
      </el-table-column>
      <el-table-column label="操作" width="210" fixed="right">
        <template #default="{ row }">
          <router-link :to="'/nodes/' + row.id">
            <el-button size="small" link type="primary">配置</el-button>
          </router-link>
          <router-link :to="'/servers?id=' + row.id">
            <el-button size="small" link type="primary">运维</el-button>
          </router-link>
          <el-button size="small" link type="primary" :loading="syncing === row.id" @click="sync(row)">
            采集
          </el-button>
          <el-button size="small" link type="danger" @click="confirmRemove(row)">删除</el-button>
        </template>
      </el-table-column>
    </el-table>
    <el-empty v-else description="还没有纳管任何节点">
      <el-button type="primary" @click="openCreate">新建节点</el-button>
    </el-empty>
  </div>

  <!-- 新建节点 -->
  <el-dialog v-model="createOpen" title="新建节点" width="560px">
    <el-form label-position="top">
      <el-form-item label="节点名称">
        <el-input v-model="form.name" placeholder="例如 hk-01" />
        <div class="vp-sub" style="margin-top:5px">面板内标识，建议用机器简称。</div>
      </el-form-item>
      <el-form-item label="访问地址">
        <el-input v-model="form.address" placeholder="例如 hk.example.com" />
        <div class="vp-sub" style="margin-top:5px">
          客户端要连的域名。TLS 协议必填，且必须已解析到那台机器。
        </div>
      </el-form-item>
      <el-form-item label="流量倍率">
        <el-input-number v-model="form.rate" :min="0.1" :step="0.1" :precision="1" />
        <div class="vp-sub" style="margin-top:5px">仅影响订阅展示，不改变实际流量。</div>
      </el-form-item>
      <el-form-item label="保护名单">
        <el-input v-model="form.protected" placeholder="留空即可；如 tzq1,hs" />
        <div class="vp-note warn" style="margin-top:8px;margin-bottom:0">
          逗号分隔的<b>配置名子串</b>，命中的 233boy 配置面板<b>拒绝删除与改写</b>。<br>
          纳管一台<b>已经在跑线上节点</b>的机器时必须填 —— 否则面板就拿到了
          「删光该机全部配置」的能力。全新机器留空。
        </div>
      </el-form-item>
    </el-form>
    <template #footer>
      <el-button @click="createOpen = false">取消</el-button>
      <el-button type="primary" :loading="creating" @click="doCreate">创建</el-button>
    </template>
  </el-dialog>

  <!-- 纳管命令 -->
  <el-dialog v-model="cmdOpen" title="在目标机上执行" width="680px">
    <div class="vp-note">
      节点 <b>{{ created?.name }}</b> 已建好，但机器还没接进来。
      把下面这条命令在目标机上以 <b>root</b> 执行，agent 会自动拨回本面板。
    </div>
    <pre class="vp-log">{{ installCmd }}</pre>
    <div class="vp-note warn" style="margin-top:12px">
      这条命令含该节点的接入 Token，等同凭证 —— 不要贴到公开渠道。
      面板日志里不会记录它。
    </div>
    <template #footer>
      <CopyBtn :text="installCmd" label="复制命令" msg="命令已复制" size="default" />
      <el-button type="primary" @click="cmdOpen = false; load()">我已执行</el-button>
    </template>
  </el-dialog>
</template>

<script setup>
import { ref, reactive, onMounted } from 'vue'
import { Loading } from '@element-plus/icons-vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { nodes as apiNodes, ago } from '../api.js'
import CopyBtn from '../components/CopyBtn.vue'

const rows = ref([])
const loading = ref(true)
const busy = ref(false)
const syncing = ref(null)

const createOpen = ref(false)
const creating = ref(false)
const form = reactive({ name: '', address: '', rate: 1, protected: '' })

const cmdOpen = ref(false)
const installCmd = ref('')
const created = ref(null)

async function load() {
  busy.value = true
  try {
    const r = await apiNodes.list()
    rows.value = (r.data || []).map((it) => ({ ...(it.node || {}), online: it.online === true, inbound_cnt: it.inbound_cnt || 0 }))
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    loading.value = false
    busy.value = false
  }
}

function openCreate() {
  form.name = ''
  form.address = ''
  form.rate = 1
  form.protected = ''
  createOpen.value = true
}

async function doCreate() {
  if (!form.name.trim()) return ElMessage.warning('请填写节点名称')
  creating.value = true
  try {
    const r = await apiNodes.create({
      name: form.name.trim(),
      address: form.address.trim(),
      rate: form.rate || 1,
      protected: form.protected.trim()
    })
    createOpen.value = false
    const d = r.data || {}
    created.value = d.node || null
    installCmd.value = d.install_cmd || ''
    cmdOpen.value = true
    load()
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    creating.value = false
  }
}

// 「采集」= 让 agent 重新读一遍机器上的 conf / Caddy / v2ray url, 把清单拉回面板。
//
// ⚠️ 这**不是**下发用户名单 (那是 /operations 页的「下发用户名单」)。
// 早前这个按钮叫「同步」, 与用户同步混在一起, 排查"用户超额了还能连"时
// 会点错地方 —— 改了名字明确区分。
async function sync(row) {
  syncing.value = row.id
  try {
    const r = await apiNodes.sync(row.id)
    ElMessage.success(r.message || '采集完成')
    load()
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    syncing.value = null
  }
}

async function confirmRemove(row) {
  try {
    await ElMessageBox.confirm(
      `删除后该节点在本面板的「全部入站清单」也会一并清除（机器上的 233boy 配置不受影响）。`,
      '删除节点 ' + row.name,
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消', confirmButtonClass: 'el-button--danger' }
    )
  } catch (e) {
    return
  }
  try {
    await apiNodes.remove(row.id)
    ElMessage.success('节点已删除')
    load()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

onMounted(load)
</script>
