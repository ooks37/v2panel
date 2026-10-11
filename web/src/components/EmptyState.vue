<template>
  <!-- 统一的空状态: 给一句说明 + 一个明确的下一步动作 -->
  <el-empty :description="description">
    <slot />
  </el-empty>
</template>

<script setup>
defineProps({
  description: { type: String, default: '暂无数据' }
})
</script>
