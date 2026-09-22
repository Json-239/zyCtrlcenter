<script setup>
// 运行日志：实时滚动 + 等级筛选 + 暂停/清空。
// 2026-09-22 改 Element Plus 工具栏（筛选/跟随/暂停/清空）；日志列表**保持原生滚动容器**：
// 自动跟随逻辑依赖 scrollTop/scrollHeight/clientHeight，换成 el-scrollbar 会多一层包装，
// 收益（几乎相同的细滚动条，styles.css 已全局覆盖）不值这个风险。
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { ElMessageBox } from 'element-plus'
import { apiGet } from '../api'
import { post, mapLabel, posLabel, taskLabel, logEvents, logVersion } from '../store'

const MAX_DOM = 300      // 最多渲染多少行（渲染成本与滚动流畅度的平衡）
const NEAR_BOTTOM = 40   // 距底部多少像素算"贴底"

const seed = ref([])
const filter = ref('all')
const paused = ref(false)
const autoScroll = ref(true)
const listEl = ref(null)
const err = ref('')
const frozen = ref([])   // 暂停时的快照（先声明：rows 计算属性会读它）

onMounted(async () => {
  try {
    const res = await apiGet('/api/logs?n=400')
    // 给历史日志也分配稳定序号（负数，避免与实时日志的 _i 冲突）：供 key + v-memo 使用
    seed.value = (res.logs || []).slice(-MAX_DOM).map((ev, i) => ({ ...ev, _i: -(i + 1) }))
  } catch (e) {
    err.value = e.message
  }
})

const filters = [
  { key: 'all', label: '全部' },
  { key: 'log', label: '日志' },
  { key: 'error', label: '出错' },
  { key: 'done', label: '完成/下线' },
  { key: 'debug', label: 'debug' },
]

function match(ev) {
  const tp = ev.type || ''
  switch (filter.value) {
    case 'log': return tp === 'log' && ev.level !== 'debug'
    case 'debug': return tp === 'log' && ev.level === 'debug'
    case 'error': return tp === 'error'
    case 'done': return ['chain_done', 'ghost_done', 'ghost_offline'].includes(tp)
    default: return true
  }
}

// 依赖 logVersion（批量 flush 后自增一次）——避免逐条事件触发重算
const rows = computed(() => {
  logVersion.value // 建立依赖
  if (paused.value) return frozen.value
  const live = logEvents.slice(-MAX_DOM)
  const all = seed.value.length ? seed.value.concat(live) : live
  return all.filter(match).slice(-MAX_DOM)
})

// 暂停时拍一张当前视图的快照（切换筛选也要重拍，否则列表像"点不动"）
function snapshot() {
  frozen.value = logEvents.slice(-MAX_DOM).filter(match)
}
watch(filter, () => { if (paused.value) snapshot() })

function togglePause() {
  if (!paused.value) {
    snapshot()
    paused.value = true
  } else {
    paused.value = false
    scrollToBottomSoon()
  }
}

function levelClass(ev) {
  if (ev.type === 'error') return 'error'
  if (ev.type === 'ghost_offline') return 'warn'
  const lv = String(ev.level || '')
  if (lv === 'warn' || lv === 'warning') return 'warn'
  if (lv === 'error' || lv === 'critical') return 'error'
  if (lv === 'debug' || lv === 'trace') return 'debug'
  return ''
}

function textOf(ev) {
  if (ev.msg) return ev.msg
  switch (ev.type) {
    case 'error':
      return `${ev.code || ''} ${ev.msg || ''}`.trim()
    case 'task_progress':
      return `任务进度：${taskLabel(ev.task_index)} 已完成 ${ev.done ?? 0}${ev.status ? `（${ev.status}）` : ''}`
    case 'chain_done':
      return `链完成：${ev.chain_id || '--'}，共 ${ev.done ?? 0} 个任务${ev.elapsed_sec ? `，耗时 ${ev.elapsed_sec}s` : ''}`
    case 'ghost_offline':
      return `抓鬼下线换号：${ev.code || ''}（已打 ${ev.done ?? 0}/${ev.limit ?? 0}）${ev.reason ? ' — ' + ev.reason : ''}`
    case 'ghost_done':
      return `抓鬼满额：${ev.done ?? 0}/${ev.limit ?? 0}${ev.reason ? `（${ev.reason}）` : ''}`
    case 'robot_state':
      return `状态：${ev.state || '--'} 地图 ${mapLabel(ev.mapid)}${ev.pos ? ` 坐标(格) ${posLabel(ev.pos)}` : ''}`
    case 'robot_online':
      return `账号上线：${ev.role_name || ''} Lv${ev.level ?? '--'} 地图 #${ev.mapid ?? '--'}`
    case 'robot_offline':
      return '账号下线'
    case 'robot_manage_reply': {
      const added = ev.result?.added || []
      const removed = ev.result?.removed || []
      return `机器人管理回执：added=[${added.join(', ')}] removed=[${removed.join(', ')}]`
    }
    case 'api':
      return `[api] ${ev.action || ''}${ev.sub ? ' / ' + ev.sub : ''}${ev.accounts ? ` 账号=${[].concat(ev.accounts).join(',')}` : ''}`
    default:
      return JSON.stringify(ev)
  }
}

function timeOf(ev) {
  const t = ev.ts || ev._t // 实时事件用接收时间兜底（避免整列 --）
  if (!t) return '--'
  const d = new Date(t * 1000)
  return d.toLocaleTimeString('zh-CN', { hour12: false }) + '.' + String(d.getMilliseconds()).padStart(3, '0')
}

// 只在"贴底"时自动跟随；用户翻看历史时不打断
function onScroll() {
  const el = listEl.value
  if (!el) return
  const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < NEAR_BOTTOM
  if (atBottom !== autoScroll.value) autoScroll.value = atBottom
}

// 手动打开"跟随最新"时立刻贴底（开关从关到开要有即时反馈）
function onFollowChange(v) {
  if (v) scrollToBottomSoon()
}

let scrollScheduled = false
function scrollToBottomSoon() {
  if (scrollScheduled) return
  scrollScheduled = true
  requestAnimationFrame(async () => {
    scrollScheduled = false
    await nextTick()
    const el = listEl.value
    if (el && autoScroll.value && !paused.value) el.scrollTop = el.scrollHeight
  })
}

// 每批事件 flush（logVersion++）后跟随一次，避免每个事件都强制滚动
watch(logVersion, () => scrollToBottomSoon())

async function clearLogs() {
  try {
    await ElMessageBox.confirm(
      '将清空当天运行日志并删除历史日志文件，且不可恢复。',
      '清空日志',
      { type: 'warning', confirmButtonText: '清空', cancelButtonText: '取消', confirmButtonClass: 'el-button--danger' },
    )
  } catch (e) {
    return // 用户取消
  }
  const res = await post('/api/logs/clear', {})
  if (res.ok) { seed.value = []; logEvents.length = 0; logVersion.value++ }
}
</script>

<template>
  <div class="card">
    <div class="row">
      <el-radio-group v-model="filter" size="small">
        <el-radio-button v-for="f in filters" :key="f.key" :value="f.key">{{ f.label }}</el-radio-button>
      </el-radio-group>
      <span class="spacer" />
      <el-switch v-model="autoScroll" size="small" active-text="跟随最新" @change="onFollowChange" />
      <el-button size="small" :type="paused ? 'warning' : 'default'" @click="togglePause">
        <el-icon><VideoPause v-if="!paused" /><VideoPlay v-else /></el-icon>
        <span>{{ paused ? '继续' : '暂停' }}</span>
      </el-button>
      <el-button size="small" type="danger" plain @click="clearLogs">
        <el-icon><Delete /></el-icon>
        <span>清空日志</span>
      </el-button>
      <el-tag size="small" type="info" effect="plain">显示 {{ rows.length }} 条</el-tag>
      <span v-if="rows.length >= MAX_DOM" class="muted small">只显示最近 {{ MAX_DOM }} 条</span>
      <el-tag v-if="paused" size="small" type="warning" effect="plain">已暂停（列表冻结）</el-tag>
    </div>
  </div>

  <el-alert v-if="err" class="mb" type="error" :closable="false" show-icon :title="err" />

  <div ref="listEl" class="log-list" @scroll.passive="onScroll">
    <div
      v-for="ev in rows" :key="ev._i"
      v-memo="[ev._i]"
      class="log-line" :class="levelClass(ev)"
    >
      <span class="ts">{{ timeOf(ev) }}</span>
      <span class="tp"><span class="tag dim">{{ ev.type }}</span></span>
      <span class="acc">{{ ev.account || '' }}</span>
      <span class="msg">{{ textOf(ev) }}</span>
    </div>
    <el-empty v-if="!rows.length" :image-size="60">
      <template #description>
        <span class="muted">
          暂无日志。运行历史按天写入 <span class="mono">data/runs_YYYYMMDD.jsonl</span>，有事件时这里会实时滚动。
        </span>
      </template>
    </el-empty>
  </div>
</template>

<style scoped>
/* 两个卡片之间的间距（.card 自带 margin-bottom，这里只给 alert 补一点） */
.mb { margin-bottom: 12px; }
/* 日志列表固定高度（一屏铺满、内部滚动）：只靠 max-height 时行少会塌成一条 */
.log-list { height: calc(100vh - 236px); min-height: 320px; max-height: none; }
</style>
