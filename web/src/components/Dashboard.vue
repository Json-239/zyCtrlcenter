<script setup>
import { computed, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { apiGet } from '../api'
import {
  state, post, stateLabel, statePhrase, stateTagClass, fmtTime, fmtAgo,
  mapLabel, posLabel, posPxLabel, zoneFullLabel, zones, isActive,
  avatarChar, avatarStyle, hpPct, hpText, taskLabel, taskHint, toast,
} from '../store'

const DENSE_ROWS = 80 // 单页超过这个行数就不跑"呼吸"动画（几百个常驻动画会明显吃帧）
const PAGE_SIZES = [20, 50, 100]

const filter = ref('all')
const onlyFailed = ref(false)
const page = ref(1)
const pageSize = ref(20)

const zoneList = computed(() => zones())
const curZone = computed(() => state.status.current || {})
const robots = computed(() => state.status.robots)
const counts = computed(() => state.status.counts)
const failed = computed(() => state.status.task_failed || { count: 0, items: [] })
const exeExists = computed(() => !!state.status.robot_exe_exists)
const robotRunning = computed(() => !!state.status.robot_running)

const filters = [
  { key: 'all', label: '全部' },
  { key: 'online', label: '在线' },
  { key: 'task', label: '干活中' },
  { key: 'ghost', label: '👻抓鬼', title: '有活跃抓鬼会话（机器人上报 ghost.enabled === true；enabled=false 的历史会话不算）' },
  { key: 'newbie', label: '🆕新手' },  // 意图表 kind === 'newbie'
  { key: 'idle', label: '发呆' },
  { key: 'error', label: '卡住', title: '只算机器人停在 ERROR 态的号（需要处理）。仅带历史 err_code 但仍在运行的号不计入，行内以弱化"曾出错"标注' },
  { key: 'offline', label: '掉线' },
]

const TASK_STATES = ['WAIT_TASK', 'NAV', 'CLICK', 'DIALOG', 'FIGHT', 'SHOP', 'ALLOC', 'WAIT_NEXT']
function isTasking(r) { return r.online && TASK_STATES.includes(r.state) }
// "真在抓鬼" = 机器人上报的抓鬼会话 enabled（ghost 字段非空只代表该号有 GhostState，
// 停止/跑完的号 enabled=false 也会上报，故不能只看字段存在）
function isGhosting(r) { return !!(r.ghost && r.ghost.enabled === true) }

// "卡住" = 停在 ERROR 态（机器人端等 reset/stop 的真卡住），而不是"err_code 非空"：
// err_code 是"最近一次错误"，机器人端有 3 分钟时效（quest_state.ERROR_TTL_MS）且
// 任务恢复时立即清除；时效内的历史错误仍会随心跳捎带一段，不能据此把正常运行的号
// 标成卡住（生产 20:17：5 个误报号全部是 NAV/WAIT_GHOST 活跃态、ghost.enabled=true）。
function isStuck(r) { return !!(r.err_code && r.state === 'ERROR') }
// 非 ERROR 态仍带 err_code：弱化提示"曾出错"，title 里保留错误码/信息，信息不丢。
function recentErrTitle(r) {
  return `最近一次错误 ${r.err_code}${r.err_repeat > 1 ? ` ×${r.err_repeat}` : ''}` +
    `${r.err_msg ? '：' + r.err_msg : ''}（当前非卡住态：已恢复或正在自动重试）`
}

// 一次遍历算完所有筛选计数（原来每个筛选各扫一遍全表）
const countsByFilter = computed(() => {
  const c = { all: 0, online: 0, task: 0, ghost: 0, newbie: 0, idle: 0, error: 0, offline: 0 }
  const kinds = intentKind.value
  for (const r of robots.value) {
    c.all++
    if (r.online) c.online++
    else c.offline++
    if (isTasking(r)) c.task++
    if (isGhosting(r)) c.ghost++
    if (kinds[r.account] === 'newbie') c.newbie++
    if (r.online && !isTasking(r) && r.state !== 'DONE') c.idle++
    if (isStuck(r)) c.error++ // 卡住 = ERROR 态（不是 err_code 非空，见 isStuck 注释）
  }
  return c
})

const filtered = computed(() => robots.value.filter((r) => {
  if (onlyFailed.value && !isStuck(r)) return false
  switch (filter.value) {
    case 'online': return r.online
    case 'offline': return !r.online
    case 'task': return isTasking(r)
    case 'ghost': return isGhosting(r)
    case 'newbie': return intentKind.value[r.account] === 'newbie'
    case 'idle': return r.online && !isTasking(r) && r.state !== 'DONE'
    case 'error': return isStuck(r)
    default: return true
  }
}))
// 分页：只渲染当前页（几百个机器人也不会卡；这是"表格分页"的落点）
const pageCount = computed(() => Math.max(1, Math.ceil(filtered.value.length / pageSize.value)))
const shownRows = computed(() => {
  const start = (page.value - 1) * pageSize.value
  return filtered.value.slice(start, start + pageSize.value)
})
const dense = computed(() => shownRows.value.length > DENSE_ROWS)

// 筛选/页大小变化后页码越界自动收敛（否则会停在空白页）
watch([filtered, pageSize], () => {
  if (page.value > pageCount.value) page.value = pageCount.value
  if (page.value < 1) page.value = 1
})

// 供 v-memo 使用的"外部依赖签名"：这些全局值变了，行也必须重渲染
const zonesSig = computed(() => `${zoneList.value.length}:${curZone.value.key || ''}`)
const mapsSig = computed(() => Object.keys(state.maps).length)

// 拟人化的一句话现状
const headline = computed(() => {
  const c = counts.value
  if (state.statusError) return '跟中控失联了，正在重试…'
  if (!state.status.robot_connected) return '还没接到机器人，等它连上来'
  if (c.online === 0) return `${c.total} 个账号都在休息`
  const working = countsByFilter.value.task
  return `${c.online}/${c.total} 个账号在线，其中 ${working} 个正在干活`
})

// ---------------- 启动哪条链（下拉选，不再"点了也不知道启什么"）----------------
// id === 'auto' = 不手选链，按账号意图自动分配（新手链/抓鬼），默认就是它
const chains = reactive({ list: [], dir: '', err: '', id: localStorage.getItem('zy_chain_id') || 'auto' })
const curChain = computed(() => chains.list.find((c) => c.id === chains.id) || null)
const autoMode = computed(() => chains.id === 'auto' || !chains.id)
async function loadChains() {
  try {
    const d = await apiGet('/api/chains')
    chains.list = d.chains || []
    chains.dir = d.chain_dir || ''
    chains.err = ''
  } catch (e) { chains.err = e.message }
}
watch(() => chains.id, (v) => { try { localStorage.setItem('zy_chain_id', v || '') } catch (e) { /* 隐私模式忽略 */ } })
onMounted(loadChains)

// 批量「全部启动」跟随顶部「启动链路」选择器：手动选链就是"整批走这一条"，这是设计如此。
function startBody(accounts) {
  const body = {}
  if (autoMode.value) body.auto = true // 服务端按意图分组下发（start_chain / ghost_start）
  else body.chain_id = chains.id
  if (accounts) body.accounts = accounts
  return body
}
async function startAll() {
  // 提示由 post() 统一弹（避免与行内启动重复弹同一条 msg）；下发行为不变。
  await post('/api/start', startBody(null))
}
// 行内「启动」= **强制按该号意图自动分配**（新手链 / 抓鬼），不受顶部「启动链路」选择器影响：
// 单选一个号时要的是"给它派一条合适的链"，而不是页面顶部替它指定的那条。
// 正常路径的提示由 post() 弹出（后端的「按意图自动分配：…」msg）；这里只补"没走 auto"的边界说明。
async function startRow(acc) {
  const res = await post('/api/start', { accounts: [acc], auto: true })
  if (res && res.ok && res.mode !== 'auto') {
    toast('没走意图自动分配（后端未返回 auto），已按指定链下发', 'warn')
  }
}
async function stopRow(acc) { await post('/api/stop', { accounts: [acc] }) }
async function resetRow(acc) { await post('/api/reset', { accounts: [acc] }) }

// ---------------- 选号上线（点选即可，不用输账号）----------------
// 单个上线用行内「启动」；批量上线在这里勾选（跨页保留）或让服务端自动挑号。

const picker = reactive({
  open: false, keyword: '', usable: true, rows: [], page: 1, pageSize: 20,
  hasMore: false, sel: new Set(), busy: false, autoLimit: 5, err: '',
})

async function loadPicker(reset = false) {
  if (reset) picker.page = 1
  try {
    const q = new URLSearchParams()
    if (picker.keyword) q.set('keyword', picker.keyword)
    if (picker.usable) q.set('usable', '1')
    q.set('include_live', '0')
    q.set('limit', String(picker.pageSize))
    q.set('offset', String((picker.page - 1) * picker.pageSize))
    const zone = curZone.value.addr || ''
    if (zone) q.set('zone', zone)
    const d = await apiGet('/api/accounts?' + q.toString())
    picker.rows = d.accounts || []
    picker.hasMore = picker.rows.length >= picker.pageSize
    picker.err = ''
  } catch (e) {
    picker.err = e.message
  }
}
function pickerGoto(delta) {
  const next = picker.page + delta
  if (next < 1 || (delta > 0 && !picker.hasMore)) return
  picker.page = next
  loadPicker()
}
function pickerToggle(name) {
  const s = new Set(picker.sel)
  if (s.has(name)) s.delete(name)
  else s.add(name)
  picker.sel = s
}
function pickerAll() {
  picker.sel = picker.sel.size >= picker.rows.length && picker.rows.length > 0
    ? new Set()
    : new Set([...picker.sel, ...picker.rows.map((a) => a.name)])
}
function pickerClear() { picker.sel = new Set() }
function togglePicker() {
  picker.open = !picker.open
  if (picker.open && !picker.rows.length) loadPicker(true)
}

function pickZone() { return curZone.value.addr || '' }

async function onlinePicked() {
  const names = Array.from(picker.sel)
  if (!names.length) return
  if (!confirm(`上线选中的 ${names.length} 个账号？`)) return
  picker.busy = true
  try {
    const res = await post('/api/robots/batch', {
      action: 'online', accounts: names, zone: pickZone(),
      chunk: 10, interval_ms: 300,
    })
    if (res && res.ok) { toast(`已通知上线：${res.sent ?? 0} 个`, 'ok'); pickerClear() }
    else if (res) alert(res.msg || '上线失败')
  } finally { picker.busy = false }
}

// ---------------- 意图 / 恢复（P1：面板看"该跑哪条链"与补发状态）----------------

const KIND_LABEL = { newbie: '新手链', zhuaogui: '捉鬼链', ghost: '抓鬼', idle: '空闲' }
const intents = reactive({
  open: false, loading: false, err: '', data: null, rows: [], timer: 0, busy: '',
})

async function loadIntents() {
  intents.loading = true
  try {
    const d = await apiGet('/api/intents')
    intents.data = d
    const rc = d.recover || {}
    intents.rows = (d.intents || []).map((it) => ({ ...it, rc: rc[it.account] || null }))
    intents.err = ''
  } catch (e) {
    intents.err = e.message
  } finally {
    intents.loading = false
  }
}

// 账号 → 意图 kind（筛选「🆕新手」用；与意图面板共用同一份 intents.rows，不新增请求）
const intentKind = computed(() => {
  const m = {}
  for (const it of intents.rows) m[it.account] = it.kind
  return m
})

// 意图数据何时拉：面板打开（看表）或筛选中了「🆕新手」（列表要按 kind 过滤）。
// 复用同一个 timer，不新增独立轮询。
function syncIntentsPoll() {
  const want = intents.open || filter.value === 'newbie'
  if (want && !intents.timer) {
    loadIntents()
    intents.timer = setInterval(loadIntents, 5000)
  } else if (!want && intents.timer) {
    clearInterval(intents.timer)
    intents.timer = 0
  }
}
function toggleIntents() {
  intents.open = !intents.open
  syncIntentsPoll()
}
watch(filter, syncIntentsPoll)
onMounted(loadIntents) // 打开页面先拉一次：🆕新手 chip 的计数开屏即有（只一次，非轮询）
onUnmounted(() => clearInterval(intents.timer))

function kindLabel(k) { return KIND_LABEL[k] || k || '--' }
function kindClass(k) {
  return { newbie: 'ok', zhuaogui: 'warn', ghost: 'danger', idle: 'dim' }[k] || 'dim'
}
function recoverText(rc) {
  if (!rc) return '未尝试'
  if (rc.blocked) return '熔断中：' + (rc.last_msg || '等人工')
  if (rc.attempts > 0) return `已补发 ${rc.attempts} 次`
  return '正常'
}
function recoverClass(rc) {
  if (!rc) return 'dim'
  if (rc.blocked) return 'danger'
  return rc.attempts > 0 ? 'warn' : 'ok'
}
// 立即补发（不带 account = 全表；冷却/熔断仍生效，正在跑的号不会被打扰）
async function restoreNow(account) {
  const who = account || '全部'
  if (!confirm(`按意图补发一次（${who}）？\n只会补"在线 + 闲着"的号；冷却/熔断仍然生效。`)) return
  intents.busy = account || 'all'
  try {
    const res = await post('/api/intents/restore', account ? { account } : {})
    if (res.ok) {
      toast(res.sent ? `已补发 ${res.sent} 条（跳过 ${res.skipped || 0}）` : '没有需要补发的号（在跑/跑完/离线/冷却中）',
        res.sent ? 'ok' : 'warn')
    } else {
      alert(res.msg || '补发失败')
    }
    loadIntents()
  } finally {
    intents.busy = ''
  }
}

async function autoOnline() {
  const n = Number(picker.autoLimit) || 1
  if (!confirm(`从账号池自动挑最多 ${n} 个【${pickZone() || '当前区'}】可用号上线？`)) return
  picker.busy = true
  try {
    const res = await post('/api/robots/batch', {
      action: 'online', zone: pickZone(), limit: n, chunk: 10, interval_ms: 300, only_usable: true,
    })
    if (res && res.ok) toast(`已通知上线：${res.sent ?? 0} 个（已在线的会跳过）`, 'ok')
    else if (res) alert(res.msg || '上线失败')
  } finally { picker.busy = false }
}

async function removeRow(acc) {
  if (!confirm(`让 ${acc} 下线？（会通知机器人端移除该账号）`)) return
  await post('/api/robots/manage', { action: 'remove', accounts: [acc] })
}

function hpClass(r) {
  const p = hpPct(r.hp)
  if (p <= 0) return 'dim'
  if (p < 30) return 'low'
  if (p < 60) return 'mid'
  return 'ok'
}

// ---------------- 任务列：链路 + 进度（"这个号在做什么、两条链跑到哪了"）----------------
// 机器人上报的 done **口径随会话切换**（client.py）：
//   · 抓鬼会话活跃（ghost.enabled=true）→ 今日抓鬼次数（与 ghost.done 同源）
//   · 其余（新手链 / 捉鬼链）         → 该链已完成的任务节点数（quest.done_base + done_tasks）
// 所以"哪条链、跑到哪一步"必须 ghost.enabled / chain_done / level 一起看：
// 只显示裸数字（原来的 `-- · 52`）看不出是新手链跑完 52 个节点，还是抓鬼抓了 52 次。
const NEWBIE_MAX_LEVEL_DEFAULT = 31 // 兜底；有意图表时用服务端给的 newbie_max_level

const newbieMaxLevel = computed(() => intents.data?.newbie_max_level ?? NEWBIE_MAX_LEVEL_DEFAULT)

// 链节点总数（进度的分母）：按意图表给的链 id 去 /api/chains 列表里取 task_count
// （链文件缺失/导航数据 task_count=0 → 分母未知，此时不硬编一个数字）
const chainTotals = computed(() => {
  const byId = {}
  for (const c of chains.list) byId[c.id] = c
  const total = (id) => {
    const c = byId[id]
    return c && !c.error && c.task_count > 0 ? c.task_count : 0
  }
  return {
    newbie: total(intents.data?.newbie_chain_id || ''),
    zhuaogui: total(intents.data?.zhuaogui_chain_id || ''),
  }
})

// 这个号跑的是哪条链：优先意图表（与「🆕新手」筛选用同一份数据），
// 没意图记录时按机器人上报的等级兜底（<门槛=还没毕业 → 新手链）。
function chainKindOf(r) {
  const k = intentKind.value[r.account]
  if (k === 'newbie' || k === 'zhuaogui' || k === 'ghost') return k
  if (r.chain_done) return 'newbie'
  const lv = Number(r.level) || 0
  return lv > 0 && lv < newbieMaxLevel.value ? 'newbie' : ''
}

// 有分母就 "12/52"，没有就 "12 个任务"（不硬编总数）
function frac(done, total) { return total > 0 ? `${done}/${total}` : `${done} 个任务` }

// 任务列内容，判定优先级：
//   抓鬼中 > 抓鬼已满 > 新手链完成 > 抓鬼已停 > 捉鬼链 > 新手链进行中 > 只剩任务号
// 返回：{ tag 徽标配色, bar 进度条配色, pct 进度%, text 主行, sub 次行 }
function taskCell(r) {
  const g = r.ghost || null
  const gDone = g ? (Number(g.done) || 0) : 0
  const gLimit = g && Number(g.limit) > 0 ? Number(g.limit) : 50
  const ghostOn = !!(g && g.enabled === true)          // 真在抓鬼（enabled=false 的历史会话不算）
  const ghostFull = !!g && gDone >= gLimit             // 今日抓鬼已满额
  const done = Number(r.done) || 0
  const kind = chainKindOf(r)
  const subTask = r.task_index ? taskLabel(r.task_index) : ''

  // ① 抓鬼进行中：今日 x/50（离"当天收工"还有多远）
  if (ghostOn && !ghostFull) {
    return {
      tag: 'info', bar: 'live', pct: Math.round((gDone / gLimit) * 100),
      text: `👻 抓鬼 ${gDone}/${gLimit}`, sub: subTask || '在钟馗抓鬼',
    }
  }
  // ② 抓鬼已满：满额即下线换号（enabled 会变 false，但 done/limit 留着）→ 明确写"已满"
  if (g && (ghostFull || (ghostOn && r.state === 'DONE'))) {
    return {
      tag: 'ok', bar: 'full', pct: 100,
      text: `👻 抓鬼 已满 ${gDone}/${gLimit}`,
      sub: r.online ? '今日抓满，本号收工' : '今日抓满（机器人已下线）',
    }
  }
  // ③ 新手链完成（旧界面那个含糊的"跑完"，其实是这一条）
  if (r.chain_done) {
    const total = chainTotals.value.newbie
    return {
      tag: 'ok', bar: 'full', pct: total > 0 ? Math.min(100, Math.round((done / total) * 100)) : 100,
      text: `🆕✅ 新手链完成 ${frac(done, total)}`,
      sub: total > 0 && done >= total ? `整条链 ${total} 个任务跑完` : '已判完成，不再重跑',
    }
  }
  // ④ 该抓鬼但会话没在跑（被停 / 卡死换号 / 等下发）
  if (kind === 'ghost') {
    return {
      tag: 'dim', bar: '', pct: g ? Math.round((gDone / gLimit) * 100) : null,
      text: g ? `👻 抓鬼 ${gDone}/${gLimit}` : '👻 抓鬼（无会话）',
      sub: subTask || '没在抓鬼（会话已停）',
    }
  }
  // ⑤ 捉鬼链（zhuaogui）进行中
  if (kind === 'zhuaogui') {
    const total = chainTotals.value.zhuaogui
    return {
      tag: 'info', bar: 'live', pct: total > 0 ? Math.round((done / total) * 100) : null,
      text: `⛓ 捉鬼链 ${frac(done, total)}`, sub: subTask || '链任务进行中',
    }
  }
  // ⑥ 新手链进行中：done = 链内已完成任务数
  if (kind === 'newbie') {
    const total = chainTotals.value.newbie
    return {
      tag: 'info', bar: 'live', pct: total > 0 ? Math.round((done / total) * 100) : null,
      text: `🆕 新手链 ${frac(done, total)}`, sub: subTask || (r.online ? '还没接任务' : '离线'),
    }
  }
  // ⑦ 兜底：链身份不明，但任务真名/进度还有（不猜链）
  if (r.task_index || done > 0) {
    return {
      tag: 'dim', bar: '', pct: null,
      text: subTask || `${done} 个任务`, sub: subTask ? `${done} 个任务` : '',
    }
  }
  return { tag: 'dim', bar: '', pct: null, text: '--', sub: '' }
}

// 悬停提示：保留全部原始信息（任务号、done、ghost 明细），排障时不用再翻日志
function taskCellHint(r) {
  const g = r.ghost || null
  const k = intentKind.value[r.account]
  const lines = [taskHint(r.task_index)]
  if (k) lines.push(`意图：${KIND_LABEL[k] || k}`)
  lines.push(`新手链：${r.chain_done ? '已完成' : '未完成'}，链内已完成 ${Number(r.done) || 0} 个任务`)
  lines.push(g
    ? `抓鬼会话：${g.enabled === true ? '进行中' : '已停'}${g.state ? `（${g.state}）` : ''}` +
      `，今日 ${Number(g.done) || 0}/${Number(g.limit) || 0}`
    : '抓鬼会话：无')
  return lines.join('\n')
}

// 按当前页行序预计算一次（模板里直接取，一行不重复算 4~5 次）
const taskCells = computed(() => shownRows.value.map((r) => ({ ...taskCell(r), hint: taskCellHint(r) })))

// v-memo 的外部依赖签名：这些全局值变了，行里的任务列也必须重渲染
// （ghost 是每轮新建的对象，不能直接进 v-memo，否则每行每 3 秒都要重渲染）
function ghostSig(r) {
  const g = r.ghost
  return g ? `${g.enabled ? 1 : 0}:${g.done}:${g.limit}` : ''
}
const chainSig = computed(() => `${chainTotals.value.newbie}/${chainTotals.value.zhuaogui}/${newbieMaxLevel.value}`)
const intentSig = computed(() => JSON.stringify(intents.data?.counts || {}))
const taskNamesSig = computed(() => Object.keys(state.taskNames).length)
</script>

<template>
  <div class="grid cols-4">
    <div class="stat">
      <div class="label">在线账号</div>
      <div class="value">
        {{ counts.online }}<span class="muted" style="font-size:14px">/{{ counts.total }}</span>
      </div>
      <div class="sub">握手完成 {{ counts.handshake }} 个</div>
    </div>
    <div class="stat">
      <div class="label">控制通道</div>
      <div class="value">
        <span class="dot" :class="state.status.robot_connected ? 'ok' : 'bad'"
              :style="state.status.robot_connected ? 'animation: breath 2.4s ease-in-out infinite' : ''" />
        {{ state.status.robot_connected ? '已连接' : '未连接' }}
      </div>
      <div class="sub mono">{{ state.status.ctrl_addr || '--' }}</div>
    </div>
    <div class="stat">
      <div class="label">机器人进程</div>
      <div class="value">{{ robotRunning ? '运行中' : '未运行' }}</div>
      <div class="sub">程序{{ exeExists ? '存在' : '缺失' }} · WS 客户端 {{ state.status.ws_clients ?? 0 }}</div>
    </div>
    <div class="stat">
      <div class="label">需要处理</div>
      <div class="value" :class="{ warnText: failed.count }">{{ failed.count }}</div>
      <div class="sub">{{ failed.count ? '重复出错 ≥ 2 次的账号' : '一切正常' }}</div>
    </div>
  </div>

  <div class="card">
    <div class="row">
      <span class="headline">{{ headline }}</span>
      <span class="spacer" />
      <button class="btn primary" @click="togglePicker">
        {{ picker.open ? '收起选号' : '选号上线' }}
      </button>
      <label class="row muted" style="gap:4px"
             title="只作用于「全部启动」（整批走这一条链）；行内「启动」按该号意图自动分配，不看这里">
        <span>启动链路</span>
        <select v-model="chains.id" style="max-width:230px">
          <option value="auto">自动分配（按意图：等级&lt;31 新手链 / 其余抓鬼）</option>
          <option value="">（只下发默认链 id）</option>
          <option v-for="c in chains.list.filter((x) => !x.nav_only)" :key="c.id" :value="c.id">
            {{ c.name || c.chain_id || c.id }}（{{ c.task_count }} 节点）
          </option>
          <option v-if="chains.list.some((x) => x.nav_only)" disabled>
            —— 以下不是链，是导航数据 ——
          </option>
          <option v-for="c in chains.list.filter((x) => x.nav_only)" :key="c.id" :value="c.id" disabled>
            {{ c.name || c.chain_id || c.id }}（导航数据，不可直接启动）
          </option>
        </select>
      </label>
      <span class="tag" :class="autoMode ? 'ok' : (curChain ? (curChain.error ? 'warn' : 'ok') : 'warn')"
            :title="autoMode ? '按每个账号的意图分配：新手链 → start_chain(newbie_full)；抓鬼 → ghost_start；没意图的用默认链' : ''">
        {{ autoMode ? '自动分配：按意图' : (curChain ? (curChain.error ? '链文件有问题' : '链文件已就绪') : '无链文件') }}
      </span>
      <button class="btn" @click="toggleIntents">
        {{ intents.open ? '收起意图' : '意图 / 恢复' }}
      </button>
      <label class="row muted" style="gap:4px">
        <input v-model="onlyFailed" type="checkbox" /> 只看卡住的
      </label>
      <button class="btn primary" @click="startAll">全部启动</button>
    </div>

    <div v-if="picker.open" class="picker">
      <div class="row" style="margin-bottom:6px">
        <input v-model="picker.keyword" class="grow-sm" placeholder="搜账号 / 角色名"
               @keyup.enter="loadPicker(true)" />
        <label class="row muted" style="gap:4px">
          <input v-model="picker.usable" type="checkbox" @change="loadPicker(true)" /> 只看可用
        </label>
        <button class="btn sm" @click="loadPicker(true)">查询</button>
        <span class="spacer" />
        <button class="btn sm" @click="pickerAll">全选本页</button>
        <button class="btn sm" @click="pickerClear">清空</button>
        <button class="btn primary" :disabled="!picker.sel.size || picker.busy" @click="onlinePicked">
          {{ picker.busy ? '下发中…' : `上线选中的 ${picker.sel.size} 个` }}
        </button>
        <input v-model.number="picker.autoLimit" type="number" style="width:60px" title="自动挑号个数" />
        <button class="btn" :disabled="picker.busy" @click="autoOnline">自动挑号上线</button>
      </div>
      <div class="table-wrap" style="max-height: 30vh">
        <table>
          <thead>
            <tr>
              <th style="width:36px"></th>
              <th>账号</th><th>等级</th><th>角色</th><th>该区可用</th><th>在线</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="a in picker.rows" :key="a.name" :class="{ 'row-off': !a.online }">
              <td><input type="checkbox" :checked="picker.sel.has(a.name)" @change="pickerToggle(a.name)" /></td>
              <td class="mono">{{ a.name }}</td>
              <td>{{ a.level || '--' }}</td>
              <td>{{ a.role_name || '--' }}</td>
              <td>
                <span class="tag" :class="a.usable ? 'ok' : (a.verified ? 'danger' : 'dim')">
                  {{ a.usable ? '可用' : (a.verified ? '不可用' : '未验证') }}
                </span>
              </td>
              <td><span class="tag" :class="a.online ? 'ok' : 'dim'">{{ a.online ? '在线' : '离线' }}</span></td>
            </tr>
            <tr v-if="!picker.rows.length">
              <td colspan="6">
                <div class="empty">
                  {{ picker.err || '没有可选的账号：换筛选，或先去「账号池」建号。' }}
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <div class="row muted" style="margin-top:6px">
        <button class="btn sm" :disabled="picker.page <= 1" @click="pickerGoto(-1)">上一页</button>
        <span>第 {{ picker.page }} 页</span>
        <button class="btn sm" :disabled="!picker.hasMore" @click="pickerGoto(1)">下一页</button>
        <span class="spacer" />
        <span>跨页勾选会保留；已在线的由服务端跳过，密码由服务端从池里取</span>
      </div>
    </div>

    <div v-if="intents.open" class="picker">
      <div class="row" style="margin-bottom:6px">
        <h3 style="margin:0">意图 / 恢复</h3>
        <span class="muted">
          该跑哪条链：等级 &lt; {{ intents.data?.newbie_max_level ?? 31 }} → 新手链优先，≥ 或链已完成 → 抓鬼
        </span>
        <span class="spacer" />
        <span class="tag" :class="intents.data?.auto_restore ? 'ok' : 'dim'">
          自动补发 {{ intents.data?.auto_restore ? '开' : '关' }}
        </span>
        <span class="muted">（CTRL_AUTO_RESTORE=1 打开）</span>
        <button class="btn sm" @click="loadIntents">{{ intents.loading ? '加载中…' : '刷新' }}</button>
        <button class="btn primary" :disabled="intents.busy" @click="restoreNow('')">
          {{ intents.busy === 'all' ? '补发中…' : '立即补发全部' }}
        </button>
      </div>
      <div class="row" style="margin-bottom:6px" v-if="intents.data">
        <span class="muted">共 {{ intents.data.count || 0 }} 条</span>
        <span v-for="(n, k) in (intents.data.counts || {})" :key="k" class="tag" :class="kindClass(k)">
          {{ kindLabel(k) }} ×{{ n }}
        </span>
        <span v-if="!(intents.data.count)" class="muted">还没有意图：机器人上线/心跳报等级后会自动登记</span>
      </div>
      <div class="table-wrap" style="max-height: 32vh">
        <table>
          <thead>
            <tr>
              <th>账号</th><th style="width:90px">意图</th><th style="width:72px">来源</th>
              <th>判据</th><th style="width:200px">恢复</th><th style="width:84px"></th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="it in intents.rows" :key="it.account">
              <td class="mono">{{ it.account }}</td>
              <td><span class="tag" :class="kindClass(it.kind)">{{ kindLabel(it.kind) }}</span></td>
              <td class="muted">{{ it.source || '--' }}</td>
              <td class="muted ellipsis" :title="it.reason || ''">{{ it.reason || '--' }}</td>
              <td>
                <span class="tag" :class="recoverClass(it.rc)" :title="(it.rc && it.rc.last_msg) || ''">
                  {{ recoverText(it.rc) }}
                </span>
              </td>
              <td>
                <button class="btn sm" :disabled="intents.busy" @click="restoreNow(it.account)">
                  {{ intents.busy === it.account ? '…' : '补发' }}
                </button>
              </td>
            </tr>
            <tr v-if="!intents.rows.length">
              <td colspan="6">
                <div class="empty">
                  {{ intents.err || '暂无意图（机器人上线并报一次等级后自动登记）' }}
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <div class="row muted" style="margin-top:6px">
        <span>「补发」= 按意图发 start_chain / ghost_start；只补"在线 + 闲着"的号，在跑/跑完/离线/冷却中的会跳过</span>
      </div>
    </div>
  </div>

  <div class="card">
    <div class="row" style="margin-bottom:10px">
      <div class="chips">
        <span
          v-for="f in filters" :key="f.key"
          class="chip" :class="{ active: filter === f.key }"
          :title="f.title"
          @click="filter = f.key"
        >{{ f.label }} {{ countsByFilter[f.key] }}</span>
      </div>
      <span class="spacer" />
      <span class="muted">当前区 {{ curZone.key || '--' }} · 每 3 秒对齐一次</span>
    </div>

    <div class="table-wrap dash-table">
      <table>
        <thead>
          <tr>
            <th style="width:210px">账号</th>
            <th>区</th>
            <th style="width:190px">在忙什么</th>
            <th style="width:186px">任务 / 进度</th>
            <th style="width:120px">血量</th>
            <th>地图</th>
            <th>坐标(格)</th>
            <th>等级</th>
            <th>握手</th>
            <th>心跳</th>
            <th style="width:196px">操作</th>
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="(r, i) in shownRows" :key="r.account"
            v-memo="[r.online, r.zone, r.state, r.err_code, r.err_repeat, r.err_msg, r.task_index, r.done,
                     r.chain_done, r.hp, r.mp, r.mapid, r.pos, r.level, r.hs, r.last_seen, r.role_name,
                     r._flash, dense, zonesSig, mapsSig, state.gridCell,
                     ghostSig(r), chainSig, intentSig, taskNamesSig]"
            :class="{ 'row-flash': r._flash, 'row-off': !r.online }"
          >
            <td>
              <div class="who">
                <span class="avatar" :class="{ pulse: isActive(r) && !dense, ring: isActive(r) && dense }"
                      :style="avatarStyle(r)">
                  {{ avatarChar(r) }}
                </span>
                <div class="who-text">
                  <div class="mono acc" :title="r.account">{{ r.account }}</div>
                  <div class="muted small">{{ r.role_name || '未建角色' }}</div>
                </div>
              </div>
            </td>
            <td><span class="tag dim" :title="r.zone">{{ zoneFullLabel(r.zone) }}</span></td>
            <td>
              <div class="state-cell" :title="statePhrase(r.state)">
                <span class="tag" :class="stateTagClass(r.state)">{{ stateLabel(r.state) }}</span>
                <span v-if="isStuck(r)" class="tag danger" :title="r.err_msg || ''">
                  {{ r.err_code }}<template v-if="r.err_repeat > 1">×{{ r.err_repeat }}</template>
                </span>
                <!-- 非 ERROR 态的历史错误：弱化展示，不冒充"卡住"（见 isStuck/recentErrTitle） -->
                <span v-else-if="r.err_code" class="tag dim" :title="recentErrTitle(r)">曾出错</span>
              </div>
              <div class="phrase muted small">{{ statePhrase(r.state) }}</div>
            </td>
            <td class="task-cell" :title="taskCells[i]?.hint || ''">
              <!-- 主行：链路 + 进度（👻 抓鬼 x/50、🆕 新手链 x/52、🆕✅ 新手链完成 …） -->
              <div class="task-row">
                <span class="tag" :class="[taskCells[i]?.tag, { 'num-up': r._flash }]">{{ taskCells[i]?.text }}</span>
                <!-- 新手链完成的号同时在抓鬼时，两个身份都要看得见 -->
                <span v-if="r.chain_done && taskCells[i]?.tag !== 'ok'" class="tag ok dim"
                      title="新手链已完成（chain_done=true），这个号现在是抓鬼号">🆕✅</span>
              </div>
              <!-- 进度条：离"当天抓满 / 整条链跑完"还有多远 -->
              <div v-if="taskCells[i]?.pct !== null && taskCells[i]?.pct !== undefined"
                   class="task-bar" :class="taskCells[i]?.bar">
                <i :style="{ width: (taskCells[i]?.pct || 0) + '%' }" />
              </div>
              <!-- 次行：此刻在做什么（任务真名），或终态说明 -->
              <div v-if="taskCells[i]?.sub" class="muted small">{{ taskCells[i].sub }}</div>
            </td>
            <td>
              <div class="hpbar" :class="hpClass(r)" :title="'HP ' + hpText(r.hp) + (r.mp ? ' / MP ' + hpText(r.mp) : '')">
                <i :style="{ width: hpPct(r.hp) + '%' }" />
              </div>
              <div class="muted small mono">{{ hpText(r.hp) }}</div>
            </td>
            <td :title="r.mapid ? `mapid=${r.mapid}` : ''">{{ mapLabel(r.mapid) }}</td>
            <td class="mono" :title="'像素: ' + posPxLabel(r.pos)">{{ posLabel(r.pos) }}</td>
            <td>{{ r.level || '--' }}</td>
            <td><span class="tag" :class="r.hs ? 'ok' : 'dim'">{{ r.hs ? '已握手' : '未握手' }}</span></td>
            <td class="muted" :title="fmtTime(r.last_seen)">{{ fmtAgo(r.last_seen) }}</td>
            <td>
              <div class="row-actions">
                <button class="btn sm"
                        title="按该号意图自动分配（&lt;31 级 → 新手链 / 其余 → 抓鬼），不跟随顶部「启动链路」选择器"
                        @click="startRow(r.account)">启动</button>
                <button class="btn sm" @click="stopRow(r.account)">停止</button>
                <button class="btn sm" @click="resetRow(r.account)">重置</button>
                <button class="btn sm danger" @click="removeRow(r.account)">移除</button>
              </div>
            </td>
          </tr>
          <tr v-if="!shownRows.length">
            <td colspan="11">
              <div class="empty">
                <svg class="empty-art" viewBox="0 0 48 48" width="46" height="46" aria-hidden="true">
                  <rect x="10" y="14" width="28" height="20" rx="6" fill="none" stroke="currentColor" stroke-width="2" />
                  <circle cx="19" cy="24" r="2.4" fill="currentColor" />
                  <circle cx="29" cy="24" r="2.4" fill="currentColor" />
                  <path d="M18 30h12" stroke="currentColor" stroke-width="2" stroke-linecap="round" />
                </svg>
                还没有机器人在线。先在右上角把账号加进来，或等机器人连上控制通道
                <span class="mono">{{ state.status.ctrl_addr || '--' }}</span>。
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <div class="pager">
      <span class="muted">共 {{ filtered.length }} 行 · 第 {{ page }} / {{ pageCount }} 页</span>
      <span class="spacer" />
      <label class="row muted" style="gap:4px">
        每页
        <select v-model.number="pageSize">
          <option v-for="n in PAGE_SIZES" :key="n" :value="n">{{ n }}</option>
        </select>
      </label>
      <button class="btn sm" :disabled="page <= 1" @click="page = 1">首页</button>
      <button class="btn sm" :disabled="page <= 1" @click="page--">上一页</button>
      <button class="btn sm" :disabled="page >= pageCount" @click="page++">下一页</button>
      <button class="btn sm" :disabled="page >= pageCount" @click="page = pageCount">末页</button>
    </div>
  </div>

  <div v-if="failed.items?.length" class="card">
    <h3>需要看看的账号（重复出错）</h3>
    <table>
      <thead><tr><th>账号</th><th>错误码</th><th>重复</th><th>距今(秒)</th><th>说明</th></tr></thead>
      <tbody>
        <tr v-for="it in failed.items" :key="it.account">
          <td class="mono">{{ it.account }}</td>
          <td><span class="tag danger">{{ it.code }}</span></td>
          <td>{{ it.repeat }}</td>
          <td>{{ it.age }}</td>
          <td class="muted">{{ it.msg }}</td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
