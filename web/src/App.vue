<script setup>
import { computed, onMounted, ref, watch } from 'vue'
import { state, startRealtime, post, switchZone, zones, currentZoneKey } from './store'
import Dashboard from './components/Dashboard.vue'
import AccountsView from './components/AccountsView.vue'
import MapView from './components/MapView.vue'
import ChainView from './components/ChainView.vue'
import TasksView from './components/TasksView.vue'
import ModulesView from './components/ModulesView.vue'
import LogView from './components/LogView.vue'
import SystemView from './components/SystemView.vue'

const tab = ref('dashboard')
const tabs = [
  { key: 'dashboard', label: '监控大屏' },
  { key: 'accounts', label: '账号池' },
  { key: 'map', label: '地图' },
  { key: 'tasks', label: '任务' },
  { key: 'chains', label: '链数据' },
  { key: 'modules', label: '模块地图' },
  { key: 'logs', label: '运行日志' },
  { key: 'system', label: '系统信息' },
]
const view = computed(() => ({
  dashboard: Dashboard, accounts: AccountsView, map: MapView,
  tasks: TasksView, chains: ChainView, modules: ModulesView, logs: LogView, system: SystemView,
}[tab.value]))

const ctrlOnline = computed(() => !!state.status?.robot_connected)
const counts = computed(() => state.status?.counts || { total: 0, online: 0, handshake: 0 })
const zoneList = computed(() => zones())
const curZone = computed(() => state.status?.current || {})

// 当前区选择器（本地双向绑定，随轮询结果同步）
const zoneSel = ref('')
watch(currentZoneKey, (k) => { zoneSel.value = k || '' }, { immediate: true })

async function onSwitchZone() {
  if (!zoneSel.value || zoneSel.value === currentZoneKey.value) return
  await switchZone(zoneSel.value)
}

onMounted(startRealtime)

async function stopAll() {
  await post('/api/stop', {})
}
async function restartRobot() {
  const z = curZone.value.key ? `${curZone.value.name}（${curZone.value.addr}）` : '当前区'
  if (!confirm(`重启机器人进程？它会断开并重新登录【${z}】。`)) return
  await post('/api/robot/restart', {})
}
</script>

<template>
  <div class="app-shell">
    <header class="topbar">
      <div class="brand">zyCtrlcenter <small>机器人中控（单进程 · 多区可切换）</small></div>
      <nav class="nav">
        <button
          v-for="t in tabs" :key="t.key"
          :class="{ active: tab === t.key }"
          @click="tab = t.key"
        >{{ t.label }}</button>
      </nav>
      <div class="topbar-right">
        <span class="tag" :class="ctrlOnline ? 'ok' : 'danger'">
          <span class="dot" :class="ctrlOnline ? 'ok' : 'bad'" /> 通道{{ ctrlOnline ? '已连接' : '未连接' }}
        </span>
        <span class="tag" :class="state.wsConnected ? 'ok' : 'warn'">
          WS{{ state.wsConnected ? '实时' : '重连中' }}
        </span>
        <select v-if="zoneList.length" v-model="zoneSel" @change="onSwitchZone"
                title="切换当前区（切换后需「应用到机器人」并重启机器人才真正生效）">
          <option v-for="z in zoneList" :key="z.key" :value="z.key">
            {{ z.name }}（{{ z.addr }}）
          </option>
        </select>
        <span class="tag dim">在线 {{ counts.online }}/{{ counts.total }}</span>
        <button class="btn sm" @click="stopAll">全部停止</button>
        <button class="btn sm danger" @click="restartRobot">重启机器人</button>
      </div>
    </header>

    <main>
      <component :is="view" />
    </main>

    <div class="toasts">
      <div v-for="t in state.toasts" :key="t.id" class="toast" :class="t.kind">{{ t.msg }}</div>
    </div>
  </div>
</template>
