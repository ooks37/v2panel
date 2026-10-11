import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import AutoImport from 'unplugin-auto-import/vite'
import Components from 'unplugin-vue-components/vite'
import { ElementPlusResolver } from 'unplugin-vue-components/resolvers'

// 产物直接落到 web/static/ —— 那就是 Go 侧 //go:embed static 的目标目录。
//
// ⚠️ base 必须是绝对路径 '/' 而不是 './'。
// 面板首页是通过安全入口路径就地渲染的 (形如 /a1b2c3d4e5f60718),
// 用相对 base 时, 如果用户带了尾斜杠访问 (/a1b2c3d4e5f60718/),
// 资源会被解析成 /a1b2c3d4e5f60718/assets/xxx.js 而 404。
// 绝对 base 恒为 /assets/xxx.js, 配合后端把 /assets/ 加进静态白名单即可。
export default defineConfig({
  plugins: [
    vue(),
    AutoImport({ resolvers: [ElementPlusResolver()] }),
    Components({ resolvers: [ElementPlusResolver()] })
  ],
  build: {
    outDir: 'static',
    emptyOutDir: true,
    // 单文件面板, 不做代码分割 —— 少几个请求, 也少几个要白名单的路径
    chunkSizeWarningLimit: 2000,
    rollupOptions: {
      output: {
        entryFileNames: 'assets/[name]-[hash].js',
        chunkFileNames: 'assets/[name]-[hash].js',
        assetFileNames: 'assets/[name]-[hash][extname]'
      }
    }
  }
})
