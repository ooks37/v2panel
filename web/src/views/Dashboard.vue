<template>
  <div class="vp-head">
    <h1>概览</h1>
    <div class="desc">节点状态、流量与账号概况</div>
    <div class="spacer"></div>
    <el-button size="small" :loading="busy" @click="load">刷新</el-button>
  </div>

  <div v-if="loading" style="text-align:center;padding:40px">
    <el-icon class="is-loading" :size="22"><Loading /></el-icon>
  </div>

  <template v-else>
    <div class="vp-stats">
      <div class="vp-stat">
        <div class="k">节点</div>
        <div class="v">{{ d.node_online }}<small>/ {{ d.node_total }} 在线</small></div>
        <div class="m">
          <el-tag v-if="d.node_online > 0" type="success" size="small">已连接</el-tag>
          <el-tag v-else type="danger" size="small">全部离线</el-tag>
        </div>
      </div>
      <div class="vp-stat">
        <div class="k">协议实例</div>
        <div class="v">{{ d.inbound_total }}</div>
        <div class="m">233boy 配置总数</div>
      </div>
      <div class="vp-stat">
        <div class="k">订阅账号</div>
        <div class="v">{{ d.user_total }}</div>
        <div class="m">
          <span v-if="d.user_abnormal > 0" style="color:var(--vp-danger)">
            {{ d.user_abnormal }} 个已熔断/到期
          </span>
          <span v-else>全部正常</span>
        </div>
      </div>
      <div class="vp-stat">
        <div class="k">今日流量</div>
        <div class="v">{{ d.today_total_h || humanBytes(d.today_total) }}</div>
        <div class="m">↑ {{ humanBytes(d.today_up) }} · ↓ {{ humanBytes(d.today_down) }}</div>
      </div>
    </div>

    <div class="vp-card">
      <h2>
        近 24 小时流量
        <span class="spacer"></span>
        <span class="sub">每点 {{ Math.round((bucketSec || 0) / 60) }} 分钟</span>
      </h2>
      <TrafficChart :series="series" :bucket-sec="bucketSec" />
      <div class="vp-sub" style="margin-top:10px">
        上行 {{ humanBytes(sumUp) }} · 下行 {{ humanBytes(sumDown) }}
      </div>
    </div>

    <div class="vp-card">
      <h2>
        节点状态
        <span class="spacer"></span>
        <router-link to="/nodes"><el-button size="small">管理节点</el-button></router-link>
      </h2>
      <el-table
        v-if="d.nodes && d.nodes.length"
        :data="d.nodes"
        class="vp-table"
        style="width:100%"
      >
        <el-table-column label="名称" min-width="150">
          <template #default="{ row }">
            <router-link :to="'/nodes/' + row.id">{{ row.name }}</router-link>
            <el-tag v-if="row.hidden" size="small" style="margin-left:6px">隐藏</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="地址" min-width="180">
          <template #default="{ row }">
            <span class="vp-mono">{{ row.address || '—' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="110">
          <template #default="{ row }">
            <el-tag :type="row.online ? 'success' : 'danger'" size="small">
              {{ row.online ? '在线' : '离线' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="协议实例" width="100" align="right">
          <template #default="{ row }">
            <span class="vp-num">{{ row.inbound_cnt || 0 }}</span>
          </template>
        </el-table-column>
        <el-table-column label="内核" min-width="120">
          <template #default="{ row }">
            <span class="vp-mono vp-sub">{{ row.v2ray_ver || '—' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="最后同步" min-width="110">
          <template #default="{ row }">
            <span class="vp-sub">{{ ago(row.last_sync_at) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="130">
          <template #default="{ row }">
            <router-link :to="'/nodes/' + row.id">
              <el-button size="small" link type="primary">配置</el-button>
            </router-link>
            <router-link :to="'/servers?id=' + row.id">
              <el-button size="small" link type="primary">运维</el-button>
            </router-link>
          </template>
        </el-table-column>
      </el-table>
      <el-empty v-else description="还没有纳管任何节点">
        <router-link to="/nodes">
          <el-button type="primary">去新建节点</el-button>
        </router-link>
      </el-empty>
    </div>
  </template>
</template>

<script setup>
import { ref, computed, onMounted } from 'vue'
import { Loading } from '@element-plus/icons-vue'
import { ElMessage } from 'element-plus'
import { misc, humanBytes, ago } from '../api.js'
import TrafficChart from '../components/TrafficChart.vue'

const d = ref({ nodes: [] })
const series = ref([])
const bucketSec = ref(0)
const loading = ref(true)
const busy = ref(false)

const sumUp = computed(() => series.value.reduce((a, p) => a + (p.up || 0), 0))
const sumDown = computed(() => series.value.reduce((a, p) => a + (p.down || 0), 0))

async function load() {
  busy.value = true
  try {
    const [dash, ser] = await Promise.all([
      misc.dashboard(),
      misc.series(24).catch(() => ({ data: {} }))
    ])
    d.value = dash.data || {}
    const s = ser.data || {}
    series.value = s.series || []
    bucketSec.value = s.bucket_sec || 0
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    loading.value = false
    busy.value = false
  }
}

onMounted(load)
</script>
