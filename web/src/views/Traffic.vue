<template>
  <div class="vp-head">
    <h1>流量</h1>
    <div class="desc">采样曲线与节点排行</div>
    <div class="spacer"></div>
    <el-button size="small" type="danger" plain @click="purgeOpen = true">清理旧数据</el-button>
  </div>

  <div v-if="loading" style="text-align:center;padding:40px">
    <el-icon class="is-loading" :size="22"><Loading /></el-icon>
  </div>

  <template v-else>
    <div class="vp-stats">
      <div class="vp-stat">
        <div class="k">窗口内合计</div>
        <div class="v">{{ humanBytes(d.total) }}</div>
        <div class="m">↑ {{ humanBytes(d.up) }} · ↓ {{ humanBytes(d.down) }}</div>
      </div>
      <div class="vp-stat">
        <div class="k">今日</div>
        <div class="v">{{ humanBytes((d.today_up || 0) + (d.today_down || 0)) }}</div>
        <div class="m">↑ {{ humanBytes(d.today_up) }} · ↓ {{ humanBytes(d.today_down) }}</div>
      </div>
      <div class="vp-stat">
        <div class="k">本月</div>
        <div class="v">{{ d.month_text || '—' }}</div>
        <div class="m">按自然月累计</div>
      </div>
      <div class="vp-stat">
        <div class="k">采样点</div>
        <div class="v">{{ d.sample_count || series.length || 0 }}</div>
        <div class="m">
          每点 {{ Math.round((d.bucket_sec || 0) / 60) }} 分钟
          <template v-if="d.last_sample"> · 最新 {{ ago(d.last_sample) }}</template>
        </div>
      </div>
    </div>

    <div class="vp-card">
      <h2>
        流量曲线
        <span class="spacer"></span>
      </h2>
      <div style="display:flex;gap:9px;flex-wrap:wrap;margin-bottom:14px">
        <el-radio-group v-model="hours" size="small" @change="load">
          <el-radio-button v-for="r in RANGES" :key="r[0]" :value="r[0]">{{ r[1] }}</el-radio-button>
        </el-radio-group>
        <el-select v-model="nodeId" size="small" style="width:180px" @change="load" placeholder="全部节点">
          <el-option value="" label="全部节点" />
          <el-option v-for="n in nodeList" :key="n.id" :value="n.id" :label="n.name" />
        </el-select>
      </div>
      <TrafficChart :series="series" :bucket-sec="d.bucket_sec" />
    </div>

    <div class="vp-card">
      <h2>按节点</h2>
      <el-table v-if="byNode.length" :data="byNode" class="vp-table" style="width:100%">
        <el-table-column label="节点" min-width="150">
          <template #default="{ row }">
            <router-link :to="'/nodes/' + row.node_id">{{ row.node_name || '#' + row.node_id }}</router-link>
          </template>
        </el-table-column>
        <el-table-column label="上行" width="110" align="right">
          <template #default="{ row }"><span class="vp-num">{{ humanBytes(row.up) }}</span></template>
        </el-table-column>
        <el-table-column label="下行" width="110" align="right">
          <template #default="{ row }"><span class="vp-num">{{ humanBytes(row.down) }}</span></template>
        </el-table-column>
        <el-table-column label="合计" min-width="180">
          <template #default="{ row }">
            <b class="vp-num">{{ humanBytes((row.up || 0) + (row.down || 0)) }}</b>
            <div class="vp-bar"><i :style="{ width: barPct(row) + '%' }"></i></div>
          </template>
        </el-table-column>
      </el-table>
      <el-empty v-else description="该窗口内没有节点采样。" />
    </div>

    <div class="vp-card">
      <h2>按协议实例</h2>
      <el-table v-if="byInbound.length" :data="byInbound" class="vp-table" style="width:100%">
        <el-table-column label="节点" min-width="130">
          <template #default="{ row }">
            <router-link :to="'/nodes/' + row.node_id">{{ row.node_name || '#' + row.node_id }}</router-link>
          </template>
        </el-table-column>
        <el-table-column label="配置" min-width="200">
          <template #default="{ row }"><span class="vp-mono">{{ row.conf_name }}</span></template>
        </el-table-column>
        <el-table-column label="协议" width="90">
          <template #default="{ row }">{{ row.protocol || '—' }}</template>
        </el-table-column>
        <el-table-column label="上行" width="100" align="right">
          <template #default="{ row }"><span class="vp-num">{{ humanBytes(row.up) }}</span></template>
        </el-table-column>
        <el-table-column label="下行" width="100" align="right">
          <template #default="{ row }"><span class="vp-num">{{ humanBytes(row.down) }}</span></template>
        </el-table-column>
        <el-table-column label="合计" width="110" align="right">
          <template #default="{ row }">
            <b class="vp-num">{{ row.total_text || humanBytes(row.total) }}</b>
          </template>
        </el-table-column>
        <el-table-column label="最后采样" width="110">
          <template #default="{ row }">
            <span class="vp-sub">{{ row.last_at ? ago(row.last_at) : '无采样' }}</span>
          </template>
        </el-table-column>
      </el-table>
      <el-empty v-else description="还没有任何入站流量采样。" />
    </div>
  </template>

  <el-dialog v-model="purgeOpen" title="清理历史采样" width="480px">
    <div class="vp-note warn">
      只删除<b>流量采样记录</b>，不影响账号的<b>已用流量累计</b>，也不影响节点配置。
      清掉之后该时间段的历史曲线无法恢复。
    </div>
    <el-form label-position="top">
      <el-form-item label="保留最近多少天">
        <el-input-number v-model="purgeDays" :min="1" :max="3650" />
      </el-form-item>
    </el-form>
    <template #footer>
      <el-button @click="purgeOpen = false">取消</el-button>
      <el-button type="danger" :loading="purging" @click="doPurge">清理</el-button>
    </template>
  </el-dialog>
</template>

<script setup>
import { ref, onMounted } from 'vue'
import { Loading } from '@element-plus/icons-vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { traffic as apiTraffic, nodes as apiNodes, humanBytes, ago } from '../api.js'
import TrafficChart from '../components/TrafficChart.vue'

const RANGES = [
  [6, '近 6 小时'], [24, '近 24 小时'], [72, '近 3 天'],
  [168, '近 7 天'], [720, '近 30 天']
]

const d = ref({})
const series = ref([])
const byNode = ref([])
const byInbound = ref([])
const nodeList = ref([])
const hours = ref(24)
const nodeId = ref('')
const loading = ref(true)

const purgeOpen = ref(false)
const purgeDays = ref(90)
const purging = ref(false)

function barPct(row) {
  const max = Math.max(...byNode.value.map((r) => (r.up || 0) + (r.down || 0)), 1)
  return Math.round(((row.up || 0) + (row.down || 0)) / max * 100)
}

async function load() {
  loading.value = true
  try {
    const [ov, inb, nds] = await Promise.all([
      apiTraffic.overview(hours.value, nodeId.value),
      apiTraffic.inbounds().catch(() => ({ data: [] })),
      apiNodes.list().catch(() => ({ data: [] }))
    ])
    d.value = ov.data || {}
    series.value = d.value.series || []
    byNode.value = d.value.by_node || []
    byInbound.value = (inb.data || []).slice().sort((a, b) => (b.total || 0) - (a.total || 0))
    nodeList.value = (nds.data || []).map((x) => x.node || {}).filter((n) => n.id)
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    loading.value = false
  }
}

async function doPurge() {
  try {
    await ElMessageBox.confirm(
      '确认清理 ' + purgeDays.value + ' 天之前的全部采样记录？',
      '确认清理',
      { type: 'warning', confirmButtonText: '清理', cancelButtonText: '取消' }
    )
  } catch (e) {
    return
  }
  purging.value = true
  try {
    const r = await apiTraffic.purge(purgeDays.value)
    ElMessage.success(r.message || '已清理')
    purgeOpen.value = false
    load()
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    purging.value = false
  }
}

onMounted(load)
</script>
