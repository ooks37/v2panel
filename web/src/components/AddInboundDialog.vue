<template>
  <el-dialog
    :model-value="modelValue"
    title="新增协议实例"
    width="620px"
    @update:model-value="$emit('update:modelValue', $event)"
  >
    <el-form label-position="top">
      <el-form-item label="协议">
        <el-select v-model="token" style="width:100%" @change="onProtoChange">
          <el-option
            v-for="p in protocols"
            :key="p.token"
            :value="p.token"
            :label="p.name + ' (' + p.token + ')' + (p.is_tls ? ' · TLS' : '')"
          />
        </el-select>
        <div v-if="cur && cur.note" class="vp-sub" style="margin-top:5px">{{ cur.note }}</div>
      </el-form-item>

      <!-- 参数表单完全由后端下发的 arg_names 驱动, 前端不硬编码协议表 -->
      <el-form-item v-for="(nm, i) in argNames" :key="nm" :label="labelFor(nm) + (needFor(i) ? ' *' : '')">
        <el-select v-if="nm === 'method'" v-model="args.method" style="width:100%">
          <el-option v-for="s in meta.ss_methods || []" :key="s" :value="s" :label="s" />
        </el-select>
        <el-select v-else-if="nm === 'type'" v-model="args.header" style="width:100%">
          <el-option v-for="s in meta.header_types || []" :key="s" :value="s" :label="s" />
        </el-select>
        <el-input
          v-else
          v-model="args[argKey(nm)]"
          :type="nm === 'port' || nm === 'dest_port' ? 'number' : 'text'"
          :placeholder="hintFor(nm)"
        />
        <div v-if="hintText(nm)" class="vp-sub" style="margin-top:5px">{{ hintText(nm) }}</div>
      </el-form-item>

      <el-form-item v-if="!argNames.length">
        <div class="vp-note" style="margin-bottom:0">该协议不需要额外参数。</div>
      </el-form-item>

      <el-checkbox v-model="dry">先干跑（只回显将要写入的配置，不落盘、不动 Caddy）</el-checkbox>
    </el-form>

    <pre v-if="out" class="vp-log" style="margin-top:14px">{{ out }}</pre>

    <template #footer>
      <el-button @click="close">取消</el-button>
      <el-button type="primary" :loading="busy" @click="submit">提交</el-button>
    </template>
  </el-dialog>
</template>

<script setup>
import { ref, reactive, computed, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { nodes as apiNodes } from '../api.js'

const props = defineProps({
  modelValue: Boolean,
  nodeId: [String, Number],
  protocols: { type: Array, default: () => [] },
  meta: { type: Object, default: () => ({}) }
})
const emit = defineEmits(['update:modelValue', 'done'])

const token = ref('')
const args = reactive({})
const dry = ref(false)
const busy = ref(false)
const out = ref('')

const cur = computed(() => props.protocols.find((p) => p.token === token.value) || null)
const argNames = computed(() => (cur.value && cur.value.arg_names) || [])

// 参数名 → 提交字段名。脚本的位置参数名与后端 AddSpec 的字段名不完全同名,
// 这里做一次映射 (type → header, pass → pass, 其余同名)。
function argKey(nm) {
  if (nm === 'type') return 'header'
  return nm
}
function needFor(i) {
  return cur.value && cur.value.required > i
}
function labelFor(nm) {
  return {
    port: '端口', uuid: 'UUID', path: '路径', host: '域名', type: '伪装类型',
    password: '密码', method: '加密方式', addr: '目标地址', dest_port: '目标端口',
    user: '用户名', pass: '密码'
  }[nm] || nm
}
function hintFor(nm) {
  if (nm === 'host') return '已解析到本机的域名'
  if (nm === 'port') return cur.value && cur.value.is_tls ? 'TLS 协议忽略此值' : '监听端口'
  return ''
}
function hintText(nm) {
  if (nm === 'host') return '必须是已解析到本机的域名；留空 233boy 会卡在交互式等待。'
  if (nm === 'path') return '留空则由后端按 UUID 自动生成（同机多节点不会撞）。'
  if (nm === 'uuid') return '留空自动生成；不要填 auto（脚本会随机后不回显，面板拿不到真值）。'
  if (nm === 'port') return cur.value && cur.value.is_tls ? 'TLS 协议的公网口由 Caddy 统一分配，此字段会被忽略。' : '入站监听端口。'
  if (nm === 'type') return 'VMess 伪装类型，默认 none。'
  if (nm === 'method') return 'Shadowsocks 加密方式。'
  if (nm === 'dest_port') return 'Dokodemo-Door 的目标端口，必填。'
  if (nm === 'addr') return 'Dokodemo-Door 的目标地址，必填。'
  if (nm === 'password') return '留空自动生成。'
  if (nm === 'user' || nm === 'pass') return 'SOCKS5 认证凭据。'
  return ''
}

function reset() {
  out.value = ''
  dry.value = false
  Object.keys(args).forEach((k) => delete args[k])
  if (props.protocols.length) {
    token.value = props.protocols[0].token
  }
  applyDefaults()
}

// 切协议时给几个有默认值的字段填上, 省得用户面对空表单
function applyDefaults() {
  const p = cur.value
  if (!p) return
  ;(p.arg_names || []).forEach((nm) => {
    const k = argKey(nm)
    if (args[k] === undefined) args[k] = ''
  })
  if (p.is_tls && !args.path) {
    // path 留空后端会自动生成, 这里只提示不预填
  }
  if (args.header === '' && (p.arg_names || []).includes('type')) {
    args.header = 'none'
  }
  if (args.method === '' && (p.arg_names || []).includes('method')) {
    const m = (props.meta.ss_methods || [])[0]
    if (m) args.method = m
  }
}

function onProtoChange() {
  reset()
}

watch(() => props.modelValue, (v) => {
  if (v) reset()
})

function close() {
  emit('update:modelValue', false)
}

async function submit() {
  const p = cur.value
  if (!p) return ElMessage.warning('请选择协议')

  const spec = { token: token.value, no_auto_tls: !p.is_tls }
  for (const [i, nm] of (p.arg_names || []).entries()) {
    const k = argKey(nm)
    const v = (args[k] === undefined || args[k] === null) ? '' : String(args[k]).trim()
    if (needFor(i) && !v) return ElMessage.warning('请填写: ' + labelFor(nm))
    if (v) spec[k] = v
  }

  busy.value = true
  out.value = ''
  try {
    const r = await apiNodes.createInbound(props.nodeId, { ...spec, dry_run: dry.value })
    const o = r.data || {}
    out.value = (o.output || o.error || JSON.stringify(o, null, 2) || '') +
      ((o.steps || []).length
        ? '\n\n' + o.steps.map((s) => (s.ok ? '✓ ' : '✕ ') + s.name + (s.detail ? ' — ' + s.detail : '')).join('\n')
        : '')
    if (!dry.value) {
      ElMessage.success(r.message || '已创建')
      emit('done')
      close()
    } else {
      ElMessage.success('干跑通过，未写入配置')
    }
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    busy.value = false
  }
}
</script>
