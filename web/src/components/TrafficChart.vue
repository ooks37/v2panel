<template>
  <!--
    手写 SVG 折线图。
    不引 echarts / chart.js: 只需要一条上下行曲线, 为此多背 200KB+ 不划算,
    而且面板 CSP 是 'self', 引 CDN 也不通。
  -->
  <div v-if="!points.length" class="vp-sub" style="padding:20px 0;text-align:center">
    该时间段没有采样数据。
  </div>
  <div v-else>
    <svg :viewBox="'0 0 ' + W + ' ' + H" preserveAspectRatio="none" class="vp-chart">
      <!-- 网格 -->
      <line v-for="g in 4" :key="'g' + g"
        :x1="PAD_L" :x2="W - PAD_R"
        :y1="gridY(g)" :y2="gridY(g)"
        stroke="#ececf0" stroke-width="1" vector-effect="non-scaling-stroke" />
      <!-- 上行 -->
      <path :d="areaPath('up')" fill="rgba(0,113,227,.10)" />
      <path :d="linePath('up')" fill="none" stroke="#0071e3" stroke-width="2"
        stroke-linejoin="round" stroke-linecap="round" vector-effect="non-scaling-stroke" />
      <!-- 下行 -->
      <path :d="areaPath('down')" fill="rgba(26,156,80,.10)" />
      <path :d="linePath('down')" fill="none" stroke="#1a9c50" stroke-width="2"
        stroke-linejoin="round" stroke-linecap="round" vector-effect="non-scaling-stroke" />
    </svg>

    <div class="vp-legend">
      <span><i style="background:#0071e3"></i>上行</span>
      <span><i style="background:#1a9c50"></i>下行</span>
      <span class="spacer"></span>
      <span>峰值 {{ humanBytes(maxVal) }} / 点</span>
    </div>
  </div>
</template>

<script setup>
import { computed } from 'vue'
import { humanBytes } from '../api.js'

const props = defineProps({
  series: { type: Array, default: () => [] },
  bucketSec: { type: Number, default: 0 }
})

const W = 1000, H = 240, PAD_L = 6, PAD_R = 6, PAD_T = 12, PAD_B = 12

const points = computed(() => {
  const s = props.series || []
  return s.map((p) => ({ up: p.up || 0, down: p.down || 0 }))
})

const maxVal = computed(() => {
  let m = 0
  points.value.forEach((p) => {
    if (p.up > m) m = p.up
    if (p.down > m) m = p.down
  })
  return m || 1
})

function gridY(i) {
  const inner = H - PAD_T - PAD_B
  return PAD_T + (inner * (i - 1)) / 3
}

// x 坐标: 均匀铺满, 单点时放中间
function xAt(i) {
  const n = points.value.length
  if (n === 1) return (W - PAD_L - PAD_R) / 2 + PAD_L
  return PAD_L + ((W - PAD_L - PAD_R) * i) / (n - 1)
}
function yAt(v) {
  const inner = H - PAD_T - PAD_B
  const r = Math.min(1, v / maxVal.value)
  return H - PAD_B - inner * r
}

function linePath(key) {
  return points.value.map((p, i) => (i === 0 ? 'M' : 'L') + xAt(i) + ' ' + yAt(p[key])).join(' ')
}
function areaPath(key) {
  const n = points.value.length
  if (!n) return ''
  const base = H - PAD_B
  let d = 'M' + xAt(0) + ' ' + base
  points.value.forEach((p, i) => {
    d += ' L' + xAt(i) + ' ' + yAt(p[key])
  })
  d += ' L' + xAt(n - 1) + ' ' + base + ' Z'
  return d
}
</script>

<style scoped>
.vp-chart { display: block; width: 100%; height: 240px; overflow: visible; }
.vp-legend { display: flex; gap: 16px; align-items: center; font-size: 12.5px; color: #86868b; margin-top: 10px; }
.vp-legend .spacer { flex: 1; }
.vp-legend i { display: inline-block; width: 10px; height: 10px; border-radius: 3px; margin-right: 6px; vertical-align: -1px; }
</style>
