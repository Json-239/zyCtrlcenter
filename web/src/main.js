import { createApp } from 'vue'
import ElementPlus from 'element-plus'
import zhCn from 'element-plus/es/locale/lang/zh-cn'
import * as ElementPlusIconsVue from '@element-plus/icons-vue'
import 'element-plus/dist/index.css'
import 'element-plus/theme-chalk/dark/css-vars.css'
import './styles.css'
import App from './App.vue'

// 2026-09-22: 面板切 Element Plus（全量注册 + 中文 + 深色主题）。
// 深色由 html.dark + element-plus dark css-vars 提供；颜色在 styles.css 里按项目色板覆盖。
document.documentElement.classList.add('dark')

const app = createApp(App)
for (const [key, comp] of Object.entries(ElementPlusIconsVue)) {
  app.component(key, comp)
}
app.use(ElementPlus, { locale: zhCn })
app.mount('#app')
