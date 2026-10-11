<template>
  <div class="vp-head">
    <h1>页面不存在</h1>
    <div class="desc">{{ $route.fullPath }}</div>
  </div>
  <div class="vp-card">
    <el-empty description="没有这个页面">
      <router-link to="/"><el-button type="primary">回到概览</el-button></router-link>
    </el-empty>
  </div>
</template>
