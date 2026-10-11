<template>
  <!-- 复制按钮: 复制文本 + 成功提示, 失败时回落到 execCommand -->
  <el-button :size="size" :link="link" :type="type" @click="doCopy">
    <el-icon v-if="!link" style="margin-right:4px"><CopyDocument /></el-icon>
    {{ done ? '已复制' : label }}
  </el-button>
</template>

<script setup>
import { ref } from 'vue'
import { ElMessage } from 'element-plus'
import { CopyDocument } from '@element-plus/icons-vue'

const props = defineProps({
  text: { type: String, default: '' },
  label: { type: String, default: '复制' },
  msg: { type: String, default: '已复制到剪贴板' },
  size: { type: String, default: 'small' },
  link: { type: Boolean, default: false },
  type: { type: String, default: 'primary' }
})

const done = ref(false)

function fallback(text) {
  const ta = document.createElement('textarea')
  ta.value = text
  ta.style.position = 'fixed'
  ta.style.top = '-1000px'
  ta.style.opacity = '0'
  document.body.appendChild(ta)
  ta.select()
  let ok = false
  try {
    ok = document.execCommand('copy')
  } catch (e) {
    ok = false
  }
  document.body.removeChild(ta)
  return ok
}

async function doCopy() {
  const t = props.text || ''
  if (!t) return ElMessage.warning('没有可复制的内容')
  let ok = false
  // 非 HTTPS 或非 localhost 时 navigator.clipboard 不可用 —— 面板常以 http 内网访问
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(t)
      ok = true
    } catch (e) {
      ok = fallback(t)
    }
  } else {
    ok = fallback(t)
  }
  if (ok) {
    ElMessage.success(props.msg)
    done.value = true
    setTimeout(() => (done.value = false), 1200)
  } else {
    ElMessage.error('复制失败，请手动选中复制')
  }
}
</script>
