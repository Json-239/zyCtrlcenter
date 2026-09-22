import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 中控默认地址（与参考项目错开端口）：可用环境变量 VITE_CTRL_TARGET 覆盖
const TARGET = process.env.VITE_CTRL_TARGET || 'http://127.0.0.1:28082'

export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5273,
    proxy: {
      '/api': { target: TARGET, changeOrigin: true },
      '/ws': { target: TARGET, ws: true },
    },
  },
})
