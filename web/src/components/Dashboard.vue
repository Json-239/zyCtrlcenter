<script setup>
import { computed, h, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
// 脚本式 API（ElMessage / ElMessageBox）必须显式 import；模板里的 <el-xxx> 才是全局注册的
import { ElMessage, ElMessageBox } from 'element-plus'
import { apiGet } from '../api'
import {
  state, post, stateLabel, statePhrase, stateTagClass, fmtTime, fmtAgo,
  mapLabel, posLabel, posPxLabel, zoneFullLabel, isActive,
  avatarChar, avatarStyle, hpPct, hpText, taskLabel, taskHint,
} from '../store'

const DENSE_ROWS = 80 // 单页超过这个行数就不跑"呼吸"动画（几百个常驻动画会明显吃帧）
const PAGE_SIZES = [20, 50, 100]

const filter = ref('all')
const onlyFailed = ref(false)
const page = ref(1)
const pageSize = ref(50) // 默认 50/页（可切 20/50/100）：几百个号也只渲染当前页，不再整表重排

// 项目色板的状态类（store.stateTagClass 的 ok/warn/danger/info/dim）→ el-tag 的 type。
// 纯 UI 映射，判据仍由 store 提供，不改任何业务含义。
const TAG_TYPE = { ok: 'success', warn: 'warning', danger: 'danger', info: 'info', dim: 'info' }
function tagType(cls) { return TAG_TYPE[cls] || 'info' }
function tagEffect(cls) { return cls === 'dim' ? 'plain' : 'light' }

// 弹窗消息要保留换行：纯文本里的 \n 会被折叠，包一层 white-space: pre-line
function pre(text) { return h('div', { style: 'white-space: pre-line' }, text) }
// 破坏性操作统一走 ElMessageBox（替换原来的 window.confirm）；用户点"取消"不算错误
async function confirmBox(msg, title, okText) {
  try {
    await ElMessageBox.confirm(pre(msg), title, {
      type: 'warning', confirmButtonText: okText || '确定', cancelButtonText: '取消',
    })
    return true
  } catch (e) {
    return false
  }
}

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
  // 2026-09-22 游荡：机器人上报的 walk.enabled === true（MapView 的绿环同一口径）
  { key: 'walk', label: '🚶游荡', title: '游荡中的号（机器人上报 walk.enabled === true）' },
  { key: 'idle', label: '发呆' },
  { key: 'error', label: '卡住', title: '只算机器人停在 ERROR 态的号（需要处理）。仅带历史 err_code 但仍在运行的号不计入，行内以弱化"曾出错"标注' },
  { key: 'offline', label: '掉线' },
]

const TASK_STATES = ['WAIT_TASK', 'NAV', 'CLICK', 'DIALOG', 'FIGHT', 'SHOP', 'ALLOC', 'WAIT_NEXT']
function isTasking(r) { return r.online && TASK_STATES.includes(r.state) }
// "真在抓鬼" = 机器人上报的抓鬼会话 enabled（ghost 字段非空只代表该号有 GhostState，
// 停止/跑完的号 enabled=false 也会上报，故不能只看字段存在）
function isGhosting(r) { return !!(r.ghost && r.ghost.enabled === true) }
// 2026-09-22 游荡中(与地图页绿环同一判据): 机器人上报 walk.enabled === true
function isWalking(r) { return !!r.online && !!(r.walk && r.walk.enabled) }

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
  const c = { all: 0, online: 0, task: 0, ghost: 0, newbie: 0, walk: 0, idle: 0, error: 0, offline: 0 }
  const kinds = intentKind.value
  for (const r of robots.value) {
    c.all++
    if (r.online) c.online++
    else c.offline++
    if (isTasking(r)) c.task++
    if (isGhosting(r)) c.ghost++
    if (kinds[r.account] === 'newbie') c.newbie++
    if (isWalking(r)) c.walk++
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
    case 'walk': return isWalking(r)
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

// 2026-09-22 全服在线（含真实玩家）：中控直连游戏服 /gm/online（source=svr_provider，参考 hqm）
// 优先；其次机器人 @online 回执（source=svr）；两路都没有时 source=local（面板另有本地兜底口径）。
const svrOnline = computed(() => {
  const s = state.status?.svr_online || {}
  const age = Number(s.age_sec || 0)
  const ageText = age <= 0 ? '刚刚' : (age < 90 ? `${age} 秒前` : `${Math.floor(age / 60)} 分钟前`)
  const source = String(s.source || '')
  const sourceText = source === 'svr_provider' ? '直连游戏服 /gm/online'
    : source === 'svr' ? '机器人 @online 回执'
    : source === 'local' ? '无服务端读数' : ''
  return { count: Number(s.count || 0), ageText, source, sourceText }
})

// 拟人化的一句话现状
const headline = computed(() => {
  const c = counts.value
  if (state.statusError) return '跟中控失联了，正在重试…'
  if (!state.status.robot_connected) return '还没接到机器人，等它连上来'
  if (c.online === 0) return `${c.total} 个账号都在休息`
  const working = countsByFilter.value.task
  return `${c.online}/${c.total} 个账号在线，其中 ${working} 个正在干活`
})

// ---------------- 在线水位保持器（2026-09-22：把当前区在线人数维持在目标附近）----------------
// 口径：人数 = 服务端全服在线（含真人，机器人 @online 回执）；读数缺失/过期（>180s）时
// 中控用"本地握手数"兜底 —— 面板必须把**来源**显示出来，别把兜底当成全服真值。
const wl = computed(() => state.status.waterline || {})
const wlReady = computed(() => Object.keys(wl.value).length > 0)
const wlForm = reactive({ enabled: false, target: 100, dead_zone: 3, max_step: 5, interval_sec: 60 })
const wlDirty = ref(false) // 用户正在改数字：别被轮询回来的状态覆盖掉输入框

watch(wl, (v) => {
  if (!v || !Object.keys(v).length || wlDirty.value) return
  wlForm.enabled = !!v.enabled
  wlForm.target = Number(v.target ?? 100)
  wlForm.dead_zone = Number(v.dead_zone ?? 3)
  wlForm.max_step = Number(v.max_step ?? 5)
  wlForm.interval_sec = Number(v.interval_sec ?? 60)
}, { immediate: true })

const wlSource = computed(() => {
  if (!wlReady.value) return '保持器未装配'
  if (wl.value.source === 'svr' && wl.value.fresh) return '服务端全服在线（含真人）'
  const age = Number(wl.value.svr_age_sec ?? -1)
  return age >= 0 ? `本地兜底（服务端读数 ${age} 秒前已过期）` : '本地兜底（服务端还没读数）'
})
const wlPending = computed(() => wl.value.pending || [])
const wlDiff = computed(() => Number(wl.value.diff ?? 0))
const wlDiffText = computed(() => (wlDiff.value > 0 ? `+${wlDiff.value}` : String(wlDiff.value)))

async function saveWaterline(patch) {
  const res = await post('/api/waterline', patch)
  if (res && res.ok) wlDirty.value = false
  return res
}
// 开关：勾选即生效（不等"保存"按钮，避免误以为点了开关没反应）
// el-switch 的 change 直接给布尔值（原来是原生 checkbox 的 e.target.checked）
function toggleWaterline(v) {
  wlForm.enabled = !!v
  return saveWaterline({ enabled: wlForm.enabled })
}
function saveWaterlineNums() {
  return saveWaterline({
    target: Number(wlForm.target) || 0,
    dead_zone: Number(wlForm.dead_zone) || 0,
    max_step: Number(wlForm.max_step) || 1,
    interval_sec: Number(wlForm.interval_sec) || 60,
  })
}

// ---------------- 游荡池（2026-09-22：任务池缺人 → 立刻回收游荡号；余量 → 按图均匀派游荡）----------------
// 口径（用户拍板）：在线总数 200 = 抓鬼池 100 + 新手池 0 + 游荡池（余量，目标 100 左右）；
// 任务池缺人 → 立刻回收游荡号（优先挑所在图人最多的，顺带纠偏分布）；否则把空闲号按"各图在线数从少到多"
// 轮流派出去（别让某张图机器人过多）。数据走 GET /api/roampool（keeper 默认关，参数落盘 data/roampool.json）。
const rp = ref({})
const rpReady = computed(() => rp.value && rp.value.ok === true)
const rpForm = reactive({
  enabled: false, target: 100, interval_sec: 60, max_step: 5, minutes: 0,
  balance: true, reclaim_on_deficit: true,
})
const rpDirty = ref(false) // 用户正在改数字：别被轮询回来的状态覆盖掉输入框
watch(rp, (v) => {
  if (!v || v.ok !== true || rpDirty.value) return
  rpForm.enabled = !!v.enabled
  rpForm.target = Number(v.target ?? 100)
  rpForm.interval_sec = Number(v.interval_sec ?? 60)
  rpForm.max_step = Number(v.max_step ?? 5)
  rpForm.minutes = Number(v.minutes ?? 0)
  rpForm.balance = v.balance !== false
  rpForm.reclaim_on_deficit = v.reclaim_on_deficit !== false
}, { immediate: true })
const rpTag = computed(() => {
  if (!rpReady.value) return { text: '未装配', cls: 'dim' }
  if (!rp.value.enabled) return { text: '已停用', cls: 'warn' }
  return { text: `在游荡 ${rp.value.running ?? 0}/${rp.value.target ?? 0}`, cls: 'ok' }
})
const rpDeficit = computed(() => Number(rp.value.deficit ?? 0))
async function loadRoampool() {
  try { rp.value = await apiGet('/api/roampool') } catch (e) { rp.value = {} }
}
async function saveRoampool(patch) {
  const res = await post('/api/roampool', patch)
  if (res && res.ok) { rpDirty.value = false; await loadRoampool() }
  return res
}
function toggleRoampool(v) {
  rpForm.enabled = !!v
  return saveRoampool({ enabled: rpForm.enabled })
}
function saveRoampoolNums() {
  return saveRoampool({
    target: Number(rpForm.target) || 0,
    interval_sec: Number(rpForm.interval_sec) || 60,
    max_step: Number(rpForm.max_step) || 1,
    minutes: Number(rpForm.minutes) || 0,
    balance: !!rpForm.balance,
    reclaim_on_deficit: !!rpForm.reclaim_on_deficit,
  })
}
let rpTimer = 0
onMounted(() => {
  loadRoampool() // 打开页面先拉一次（最近动作/在游荡数开屏即有）
  rpTimer = setInterval(loadRoampool, 15000)
})
onUnmounted(() => clearInterval(rpTimer))

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
    ElMessage.warning('没走意图自动分配（后端未返回 auto），已按指定链下发')
  }
}
async function stopRow(acc) { await post('/api/stop', { accounts: [acc] }) }
async function resetRow(acc) { await post('/api/reset', { accounts: [acc] }) }

// ---------------- 选号上线（点选即可，不用输账号）----------------
// 单个上线用行内「启动」；批量上线在这里勾选（跨页保留）或让服务端自动挑号。

const picker = reactive({
  open: false, keyword: '', usable: true, rows: [], page: 1, pageSize: 20,
  hasMore: false, sel: new Set(), busy: false, autoLimit: 5, err: '',
  mode: 'pick', // pick=手动勾选（默认）/ auto=自动挑号：同一个弹窗里的两段（UI 状态）
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
  if (!await confirmBox(`上线选中的 ${names.length} 个账号？\n分批：每批 10 个、间隔 300ms；已在线的由服务端跳过。`, '确认上线', '上线')) return
  picker.busy = true
  try {
    const res = await post('/api/robots/batch', {
      action: 'online', accounts: names, zone: pickZone(),
      chunk: 10, interval_ms: 300,
    })
    // 成功提示由 post() 统一弹（res.msg），这里只兜失败
    if (res && res.ok) pickerClear()
    else if (res) ElMessage.error(res.msg || '上线失败')
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
  if (!await confirmBox(`按意图补发一次（${who}）？\n只会补"在线 + 闲着"的号；冷却/熔断仍然生效。`, '确认补发', '补发')) return
  intents.busy = account || 'all'
  try {
    const res = await post('/api/intents/restore', account ? { account } : {})
    if (res.ok) {
      // 补发条数是服务端算的（post() 的通用提示里没有），这里补一条更有信息量的反馈
      if (res.sent) ElMessage.success(`已补发 ${res.sent} 条（跳过 ${res.skipped || 0}）`)
      else ElMessage.warning('没有需要补发的号（在跑/跑完/离线/冷却中）')
    } else {
      ElMessage.error(res.msg || '补发失败')
    }
    loadIntents()
  } finally {
    intents.busy = ''
  }
}

async function autoOnline() {
  const n = Number(picker.autoLimit) || 1
  if (!await confirmBox(`从账号池自动挑最多 ${n} 个【${pickZone() || '当前区'}】可用号上线？`, '确认上线', '挑号上线')) return
  picker.busy = true
  try {
    const res = await post('/api/robots/batch', {
      action: 'online', zone: pickZone(), limit: n, chunk: 10, interval_ms: 300, only_usable: true,
    })
    if (res && res.ok) ElMessage.success(`已通知上线：${res.sent ?? 0} 个（已在线的会跳过）`)
    else if (res) ElMessage.error(res.msg || '上线失败')
  } finally { picker.busy = false }
}

async function removeRow(acc) {
  // 2026-09-22 用户口径："掉线的直接移除" —— 离线号不再弹确认（一键清掉僵尸行）；
  //   在线号仍然确认一次（避免误删正在干活的号）。这里只把 window.confirm 换成 ElMessageBox。
  const row = (state.status?.robots || []).find((x) => x.account === acc)
  const offline = row && !row.online
  if (!offline && !await confirmBox(`让 ${acc} 下线？（会通知机器人端移除该账号）`, '确认移除', '移除')) return
  const res = await post('/api/robots/manage', { action: 'remove', accounts: [acc] })
  if (res && !res.ok) ElMessage.warning(res.msg || '移除失败')
}

// 2026-09-22 批量移除所有"掉线"的号（僵尸行一键清）
const offlineAccounts = computed(() => (state.status?.robots || []).filter((r) => !r.online).map((r) => r.account))
async function removeOffline() {
  const accs = offlineAccounts.value.slice()
  if (!accs.length) { ElMessage.info('当前没有掉线的号'); return }
  if (!await confirmBox(`移除全部掉线的号？（共 ${accs.length} 个；会通知机器人端移除）`, '确认移除', '移除全部')) return
  let ok = 0
  for (let i = 0; i < accs.length; i += 20) {
    const chunk = accs.slice(i, i + 20)
    const res = await post('/api/robots/manage', { action: 'remove', accounts: chunk })
    if (res && res.ok) ok += chunk.length
    await new Promise((r) => setTimeout(r, 200))
  }
  // 每批都被 post() 弹过一次提示，这里再汇总一句总数（批量操作要看最后结果）
  ElMessage({ type: ok === accs.length ? 'success' : 'warning', message: `已移除掉线号 ${ok}/${accs.length} 个` })
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

// 按账号预计算一次（模板里直接取，一行不重复算 4~5 次）；
// 用账号做 key 而不是"当前页下标"：el-table 内部行序不由我们控制，下标会错位
const taskCells = computed(() => {
  const m = {}
  for (const r of shownRows.value) m[r.account] = { ...taskCell(r), hint: taskCellHint(r) }
  return m
})

// el-table 的行 class 回调：掉线行压暗 + 进度推进时闪一下（样式仍是原来的 .row-off / .row-flash）
function dashRowClass({ row }) {
  return `${row.online ? '' : 'row-off'}${row._flash ? ' row-flash' : ''}`.trim()
}
// 选号弹窗里的候选行：离线压暗（与主表同一口径）
function pickerRowClass({ row }) { return row.online ? '' : 'row-off' }

// 说明：原来这里有一组给 v-memo 用的"外部依赖签名"（zonesSig/mapsSig/ghostSig/chainSig/
// intentSig/taskNamesSig）。表格换成 el-table + 分页后不再用 v-memo（单元格插槽里的 v-memo
// 缓存语义对 v-for 之外的行不可靠），改由"只渲染当前页 50 行"来收敛开销；
// 全局值（地图名/任务名/意图）变了由渲染副作用自身重新订阅，不会显示陈旧数据。
</script>

<template>
  <!-- 第一行统计卡：信息与原来一致，改成 el-card + 图标 + 大字号数字（数字跳动动画保留） -->
  <div class="grid cols-4 stat-grid">
    <el-card class="stat-card" shadow="never">
      <div class="label"><el-icon><UserFilled /></el-icon>在线 / 机器人（握手口径）</div>
      <div class="value">
        {{ counts.handshake }}<span class="unit">在线</span>
        <span class="unit">·</span>
        {{ counts.total }}<span class="unit">机器人</span>
      </div>
      <div class="sub">在线 = 握手完成（已连上游戏服）· 机器人 = 表内全部 · 已登录角色 {{ counts.online }} 个</div>
      <div class="sub" v-if="svrOnline.count"
           :title="`服务端全服在线角色数（含真实玩家，不只是我们的号）${svrOnline.sourceText ? ' · ' + svrOnline.sourceText : ''}`">
        🌐 全服在线（含真人）：<b>{{ svrOnline.count }}</b>
        <span class="muted">（{{ svrOnline.ageText }}）</span>
        <span class="muted" v-if="svrOnline.sourceText"> · {{ svrOnline.sourceText }}</span>
      </div>
    </el-card>
    <el-card class="stat-card" shadow="never">
      <div class="label"><el-icon><Connection /></el-icon>控制通道</div>
      <div class="value">
        <span class="dot" :class="state.status.robot_connected ? 'ok' : 'bad'"
              :style="state.status.robot_connected ? 'animation: breath 2.4s ease-in-out infinite' : ''" />
        {{ state.status.robot_connected ? '已连接' : '未连接' }}
      </div>
      <div class="sub mono">{{ state.status.ctrl_addr || '--' }}</div>
    </el-card>
    <el-card class="stat-card" shadow="never">
      <div class="label"><el-icon><Monitor /></el-icon>机器人进程</div>
      <div class="value">{{ robotRunning ? '运行中' : '未运行' }}</div>
      <div class="sub">程序{{ exeExists ? '存在' : '缺失' }} · WS 客户端 {{ state.status.ws_clients ?? 0 }}</div>
    </el-card>
    <el-card class="stat-card" shadow="never">
      <div class="label"><el-icon><WarningFilled /></el-icon>需要处理</div>
      <div class="value" :class="{ warnText: failed.count }">{{ failed.count }}</div>
      <div class="sub">{{ failed.count ? '重复出错 ≥ 2 次的账号' : '一切正常' }}</div>
    </el-card>
  </div>

  <!-- 在线水位保持器（2026-09-22）：把当前区在线人数维持在目标附近（不足补号 / 超出压空闲号） -->
  <el-card class="panel-card" shadow="never">
    <template #header>
      <div class="card-head">
        <span class="card-title">在线水位</span>
        <el-tag :type="!wlReady ? 'warning' : (wl.enabled ? 'success' : 'info')" effect="plain" size="small" disable-transitions>
          {{ !wlReady ? '未装配' : (wl.enabled ? '已启用' : '已停用') }}
        </el-tag>
        <span class="spacer" />
        <el-tooltip placement="top" content="开启后按间隔检查：人数不足从号池补号，超出优先断空闲号（正在抓鬼/任务的号只标“待下线”，等它收工再断）">
          <el-switch v-model="wlForm.enabled" :disabled="!wlReady" size="small" active-text="启用" @change="toggleWaterline" />
        </el-tooltip>
      </div>
    </template>
    <div class="metrics">
      <div class="metric"><span class="k">当前</span><b class="v big">{{ wl.cur ?? '--' }}</b></div>
      <div class="metric"><span class="k">目标</span><b class="v">{{ wl.target ?? '--' }}</b></div>
      <div class="metric">
        <span class="k">差值</span><b class="v" :class="{ warnText: wlDiff !== 0 }">{{ wlDiffText }}</b>
        <span class="k">（正=要补号 / 负=要压号）</span>
      </div>
      <div class="metric">
        <span class="k">待下线</span>
        <b class="v" :title="wlPending.join(', ') || '没有待下线的号'">{{ wlPending.length }}</b>
        <span class="k">个（在忙，等收工）</span>
      </div>
      <div class="metric"><span class="k">数据来源</span><b class="v" :class="{ warnText: !wl.fresh }">{{ wlSource }}</b></div>
    </div>
    <div class="param-row">
      <span class="k">目标</span>
      <el-input-number v-model="wlForm.target" size="small" :min="0" :max="10000" :controls="false"
                       class="num" @change="wlDirty = true" />
      <span class="k">死区 ±</span>
      <el-input-number v-model="wlForm.dead_zone" size="small" :min="0" :controls="false"
                       class="num-sm" @change="wlDirty = true" />
      <span class="k">单轮上限</span>
      <el-input-number v-model="wlForm.max_step" size="small" :min="1" :max="50" :controls="false"
                       class="num-sm" @change="wlDirty = true" />
      <span class="k">间隔(秒)</span>
      <el-input-number v-model="wlForm.interval_sec" size="small" :min="10" :max="3600" :controls="false"
                       class="num-sm" @change="wlDirty = true" />
      <el-button type="primary" size="small" :disabled="!wlReady" @click="saveWaterlineNums">保存水位参数</el-button>
      <span v-if="!wlReady" class="muted">中控还没装配水位保持器（需重启中控）</span>
    </div>
    <div class="sub">
      最近动作：{{ wl.last_action || '还没跑过' }}
      <template v-if="wl.last_run_ts"> · 上一轮 {{ fmtAgo(wl.last_run_ts) }}</template>
      <template v-if="wl.last_err"> · <span class="warnText">{{ wl.last_err }}</span></template>
    </div>
    <div class="sub">
      人数口径 = **本地（我们自己的握手数）**（2026-09-22 用户口径：暂不做"全服含真人"）；
      将来要切"全服含真人"，用 POST /api/livecount 打开直连数据源并把 allow_local 关掉即可。
      历史上服务端口径的说明：
      用本地握手数兜底（“数据来源”会标出来）。调整规则：每 {{ wl.interval_sec || 60 }} 秒一轮、
      |差值| 小于死区不动、每轮最多 ±{{ wl.max_step || 5 }} 个；压号优先断空闲号，在忙的号等任务收尾。
    </div>
  </el-card>

  <!-- 游荡池（2026-09-22）：任务池缺人 → 立刻回收游荡号；余量 → 按图均匀派游荡 -->
  <el-card class="panel-card" shadow="never">
    <template #header>
      <div class="card-head">
        <span class="card-title">游荡池</span>
        <el-tag :type="rpTag.cls === 'ok' ? 'success' : (rpTag.cls === 'warn' ? 'warning' : 'info')"
                effect="plain" size="small" disable-transitions>{{ rpTag.text }}</el-tag>
        <span class="spacer" />
        <el-tooltip placement="top" content="开启后每轮检查：任务池（抓鬼/新手）缺人 → 回收游荡号；否则把空闲号按各图人数从少到多轮流派出去游荡。参数落盘 data/roampool.json（默认关）">
          <el-switch v-model="rpForm.enabled" :disabled="!rpReady" size="small" active-text="启用" @change="toggleRoampool" />
        </el-tooltip>
      </div>
    </template>
    <div class="metrics">
      <div class="metric"><span class="k">在游荡</span><b class="v big">{{ rp.running ?? '--' }}</b></div>
      <div class="metric"><span class="k">目标</span><b class="v">{{ rp.target ?? '--' }}</b></div>
      <div class="metric"><span class="k">空闲</span><b class="v">{{ rp.idle ?? '--' }}</b><span class="k">个</span></div>
      <div class="metric">
        <span class="k">任务池缺口</span><b class="v" :class="{ warnText: rpDeficit > 0 }">{{ rpDeficit }}</b>
        <span class="k">（正=要从游荡池回收）</span>
      </div>
      <div class="metric">
        <span class="k">可用图</span><b class="v">{{ rp.map_count ?? 0 }}</b>
        <span class="k">张 · 单轮 ≤{{ rp.max_step ?? 5 }} 个</span>
      </div>
      <div v-if="(rp.maps || []).length" class="metric">
        <span class="k">白名单</span><b class="v mono">[{{ (rp.maps || []).join(',') }}]</b>
      </div>
    </div>
    <div class="param-row">
      <span class="k">目标</span>
      <el-input-number v-model="rpForm.target" size="small" :min="0" :max="10000" :controls="false"
                       class="num" @change="rpDirty = true" />
      <span class="k">间隔(秒)</span>
      <el-input-number v-model="rpForm.interval_sec" size="small" :min="10" :max="3600" :controls="false"
                       class="num-sm" @change="rpDirty = true" />
      <span class="k">单轮上限</span>
      <el-input-number v-model="rpForm.max_step" size="small" :min="1" :max="50" :controls="false"
                       class="num-sm" @change="rpDirty = true" />
      <span class="k" title="0 = 不限时（机器人端不自动收工）">限时(分)</span>
      <el-input-number v-model="rpForm.minutes" size="small" :min="0" :max="1440" :controls="false"
                       class="num-sm" @change="rpDirty = true" />
      <el-checkbox v-model="rpForm.balance" size="small" title="勾选=按各图在线数从少到多轮流派（均匀）；不勾=每号自抽随机图"
                   @change="saveRoampoolNums">按图均匀</el-checkbox>
      <el-checkbox v-model="rpForm.reclaim_on_deficit" size="small" title="勾选=任务池缺人时立刻回收游荡号（会打断正在走的游荡，路程损耗可接受）"
                   @change="saveRoampoolNums">缺人立刻回收</el-checkbox>
      <el-button type="primary" size="small" :disabled="!rpReady" @click="saveRoampoolNums">保存游荡池参数</el-button>
      <span v-if="!rpReady" class="muted">中控还没装配游荡池 keeper（需重启中控）</span>
    </div>
    <div class="sub">
      最近动作：{{ rp.last_action || '还没跑过' }}
      <template v-if="rp.last_run_ts"> · 上一轮 {{ fmtAgo(rp.last_run_ts) }}</template>
      <template v-if="rp.last_err"> · <span class="warnText">{{ rp.last_err }}</span></template>
    </div>
    <div class="sub">
      口径：在线总数 = 抓鬼池 + 新手池 + 游荡池（余量）。调整规则：每 {{ rp.interval_sec || 60 }} 秒一轮，
      任务池缺人时**立刻回收**游荡号（优先挑所在图人最多的，顺带纠偏），否则把空闲号**按图均匀**派出去；
      单轮最多调 {{ rp.max_step || 5 }} 个。机器人端「空闲 90s 自动游荡」仍是兜底（本池只管名额与分布）。
    </div>
  </el-card>

  <el-card class="panel-card" shadow="never">
    <div class="toolbar">
      <span class="headline">{{ headline }}</span>
      <span class="spacer" />
      <el-button type="primary" size="small" @click="togglePicker">
        <el-icon><Plus /></el-icon>选号上线
      </el-button>
      <span class="k">启动链路</span>
      <el-tooltip placement="top" content="只作用于「全部启动」（整批走这一条链）；行内「启动」按该号意图自动分配，不看这里">
        <el-select v-model="chains.id" size="small" style="width: 296px" placeholder="（只下发默认链 id）">
          <el-option value="auto" label="自动分配（按意图：等级&lt;31 新手链 / 其余抓鬼）" />
          <el-option value="" label="（只下发默认链 id）" />
          <el-option v-for="c in chains.list.filter((x) => !x.nav_only)" :key="c.id" :value="c.id"
                     :label="`${c.name || c.chain_id || c.id}（${c.task_count} 节点）`" />
          <el-option value="__nav_head__" disabled label="—— 以下不是链，是导航数据 ——"
                     v-if="chains.list.some((x) => x.nav_only)" />
          <el-option v-for="c in chains.list.filter((x) => x.nav_only)" :key="c.id" :value="c.id" disabled
                     :label="`${c.name || c.chain_id || c.id}（导航数据，不可直接启动）`" />
        </el-select>
      </el-tooltip>
      <el-tag size="small" effect="plain" disable-transitions
              :type="autoMode ? 'success' : (curChain ? (curChain.error ? 'warning' : 'success') : 'warning')"
              :title="autoMode ? '按每个账号的意图分配：新手链 → start_chain(newbie_full)；抓鬼 → ghost_start；没意图的用默认链' : ''">
        {{ autoMode ? '自动分配：按意图' : (curChain ? (curChain.error ? '链文件有问题' : '链文件已就绪') : '无链文件') }}
      </el-tag>
      <el-button size="small" @click="toggleIntents">
        {{ intents.open ? '收起意图' : '意图 / 恢复' }}
      </el-button>
      <el-checkbox v-model="onlyFailed" size="small">只看卡住的</el-checkbox>
      <el-button type="primary" size="small" @click="startAll">全部启动</el-button>
    </div>

    <div v-if="intents.open" class="sub-panel">
      <div class="toolbar" style="margin-bottom:6px">
        <span class="card-title">意图 / 恢复</span>
        <span class="muted">
          该跑哪条链：等级 &lt; {{ intents.data?.newbie_max_level ?? 31 }} → 新手链优先，≥ 或链已完成 → 抓鬼
        </span>
        <span class="spacer" />
        <el-tag size="small" effect="plain" disable-transitions :type="intents.data?.auto_restore ? 'success' : 'info'">
          自动补发 {{ intents.data?.auto_restore ? '开' : '关' }}
        </el-tag>
        <span class="muted">（CTRL_AUTO_RESTORE=1 打开）</span>
        <el-button size="small" :loading="intents.loading" @click="loadIntents">刷新</el-button>
        <el-button type="primary" size="small" :disabled="!!intents.busy" @click="restoreNow('')">
          {{ intents.busy === 'all' ? '补发中…' : '立即补发全部' }}
        </el-button>
      </div>
      <div class="toolbar" style="margin-bottom:6px" v-if="intents.data">
        <span class="muted">共 {{ intents.data.count || 0 }} 条</span>
        <span v-for="(n, k) in (intents.data.counts || {})" :key="k" class="tag" :class="kindClass(k)">
          {{ kindLabel(k) }} ×{{ n }}
        </span>
        <span v-if="!(intents.data.count)" class="muted">还没有意图：机器人上线/心跳报等级后会自动登记</span>
      </div>
      <el-table :data="intents.rows" size="small" height="300" row-key="account">
        <el-table-column label="账号" min-width="170" class-name="mono" show-overflow-tooltip>
          <template #default="{ row: it }">{{ it.account }}</template>
        </el-table-column>
        <el-table-column label="意图" width="96">
          <template #default="{ row: it }">
            <span class="tag" :class="kindClass(it.kind)">{{ kindLabel(it.kind) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="来源" width="86">
          <template #default="{ row: it }"><span class="muted">{{ it.source || '--' }}</span></template>
        </el-table-column>
        <el-table-column label="判据" min-width="220" show-overflow-tooltip>
          <template #default="{ row: it }"><span class="muted">{{ it.reason || '--' }}</span></template>
        </el-table-column>
        <el-table-column label="恢复" width="200">
          <template #default="{ row: it }">
            <span class="tag" :class="recoverClass(it.rc)" :title="(it.rc && it.rc.last_msg) || ''">
              {{ recoverText(it.rc) }}
            </span>
          </template>
        </el-table-column>
        <el-table-column label="" width="84">
          <template #default="{ row: it }">
            <el-button link type="primary" size="small" :disabled="!!intents.busy" @click="restoreNow(it.account)">
              {{ intents.busy === it.account ? '…' : '补发' }}
            </el-button>
          </template>
        </el-table-column>
        <template #empty>
          <el-empty :image-size="56" :description="intents.err || '暂无意图（机器人上线并报一次等级后自动登记）'" />
        </template>
      </el-table>
      <div class="row muted" style="margin-top:6px">
        <span>「补发」= 按意图发 start_chain / ghost_start；只补"在线 + 闲着"的号，在跑/跑完/离线/冷却中的会跳过</span>
      </div>
    </div>
  </el-card>

  <!-- 选号上线：原来是内嵌在页面里的面板，改成弹窗（勾选状态跨页保留，逻辑不变） -->
  <el-dialog v-model="picker.open" title="选号上线" width="780px" class="picker-dialog">
    <el-radio-group v-model="picker.mode" size="small" class="seg-row">
      <el-radio-button value="pick">手动勾选账号</el-radio-button>
      <el-radio-button value="auto">自动挑号</el-radio-button>
    </el-radio-group>
    <template v-if="picker.mode === 'pick'">
      <div class="toolbar" style="margin-bottom:8px">
        <el-input v-model="picker.keyword" size="small" placeholder="搜账号 / 角色名" clearable
                  class="grow-sm" @keyup.enter="loadPicker(true)">
          <template #prefix><el-icon><Search /></el-icon></template>
        </el-input>
        <el-checkbox v-model="picker.usable" size="small" @change="loadPicker(true)">只看可用</el-checkbox>
        <el-button size="small" @click="loadPicker(true)">查询</el-button>
        <span class="spacer" />
        <el-button size="small" @click="pickerAll">全选本页</el-button>
        <el-button size="small" @click="pickerClear">清空</el-button>
      </div>
      <el-table :data="picker.rows" size="small" height="330" row-key="name" :row-class-name="pickerRowClass">
        <el-table-column width="46">
          <template #default="{ row: a }">
            <el-checkbox :model-value="picker.sel.has(a.name)" @change="pickerToggle(a.name)" />
          </template>
        </el-table-column>
        <el-table-column label="账号" min-width="170" class-name="mono" show-overflow-tooltip>
          <template #default="{ row: a }">{{ a.name }}</template>
        </el-table-column>
        <el-table-column label="等级" width="70">
          <template #default="{ row: a }">{{ a.level || '--' }}</template>
        </el-table-column>
        <el-table-column label="角色" min-width="120" show-overflow-tooltip>
          <template #default="{ row: a }">{{ a.role_name || '--' }}</template>
        </el-table-column>
        <el-table-column label="该区可用" width="100">
          <template #default="{ row: a }">
            <el-tag size="small" disable-transitions
                    :type="a.usable ? 'success' : (a.verified ? 'danger' : 'info')"
                    :effect="a.usable || a.verified ? 'light' : 'plain'">
              {{ a.usable ? '可用' : (a.verified ? '不可用' : '未验证') }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="在线" width="90">
          <template #default="{ row: a }">
            <el-tag size="small" disable-transitions :type="a.online ? 'success' : 'info'"
                    :effect="a.online ? 'light' : 'plain'">{{ a.online ? '在线' : '离线' }}</el-tag>
          </template>
        </el-table-column>
        <template #empty>
          <el-empty :image-size="56" :description="picker.err || '没有可选的账号：换筛选，或先去「账号池」建号。'" />
        </template>
      </el-table>
      <div class="toolbar" style="margin-top:8px">
        <el-button size="small" :disabled="picker.page <= 1" @click="pickerGoto(-1)">上一页</el-button>
        <span class="muted">第 {{ picker.page }} 页</span>
        <el-button size="small" :disabled="!picker.hasMore" @click="pickerGoto(1)">下一页</el-button>
        <span class="spacer" />
        <span class="muted">跨页勾选会保留；已在线的由服务端跳过，密码由服务端从池里取</span>
      </div>
    </template>
    <div v-else class="toolbar">
      <span class="k">从账号池自动挑号上线</span>
      <el-input-number v-model="picker.autoLimit" size="small" :min="1" :max="200" style="width:130px" />
      <span class="k">个（只挑可用号，已在线的会跳过）</span>
    </div>
    <template #footer>
      <div class="toolbar">
        <span class="spacer" />
        <el-button size="small" @click="picker.open = false">关闭</el-button>
        <el-button v-if="picker.mode === 'pick'" type="primary" size="small"
                   :disabled="!picker.sel.size" :loading="picker.busy" @click="onlinePicked">
          上线选中的 {{ picker.sel.size }} 个
        </el-button>
        <el-button v-else type="primary" size="small" :loading="picker.busy" @click="autoOnline">
          自动挑 {{ picker.autoLimit }} 个上线
        </el-button>
      </div>
    </template>
  </el-dialog>

  <el-card class="panel-card" shadow="never">
    <div class="toolbar" style="margin-bottom:10px">
      <el-radio-group v-model="filter" size="small" class="filter-group">
        <el-tooltip v-for="f in filters" :key="f.key" placement="top" :content="f.title || f.label"
                    :disabled="!f.title">
          <el-radio-button :value="f.key">{{ f.label }} {{ countsByFilter[f.key] }}</el-radio-button>
        </el-tooltip>
      </el-radio-group>
      <span class="spacer" />
      <el-button size="small" type="danger" plain :disabled="!offlineAccounts.length" @click="removeOffline"
                 :title="offlineAccounts.length ? '一键移除所有掉线的号（离线号免确认）' : '当前没有掉线的号'">
        <el-icon><Delete /></el-icon>移除掉线({{ offlineAccounts.length }})
      </el-button>
      <span class="muted">当前区 {{ curZone.key || '--' }} · 每 3 秒对齐一次</span>
    </div>

    <!-- 核心表：el-table + 固定高度内部滚动 + 分页（几百个号也只渲染当前页，页面不再整表重排） -->
    <el-table :data="shownRows" class="dash-table" size="small" stripe :height="560"
              row-key="account" :row-class-name="dashRowClass">
      <el-table-column label="账号" width="200">
        <template #default="{ row: r }">
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
        </template>
      </el-table-column>
      <el-table-column label="区" width="132">
        <template #default="{ row: r }">
          <el-tag size="small" type="info" effect="plain" disable-transitions :title="r.zone">{{ zoneFullLabel(r.zone) }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="在忙什么" width="180">
        <template #default="{ row: r }">
          <div class="state-cell" :title="statePhrase(r.state)">
            <el-tag size="small" disable-transitions :type="tagType(stateTagClass(r.state))"
                    :effect="tagEffect(stateTagClass(r.state))">{{ stateLabel(r.state) }}</el-tag>
            <el-tag v-if="isStuck(r)" size="small" type="danger" effect="dark" disable-transitions :title="r.err_msg || ''">
              {{ r.err_code }}<template v-if="r.err_repeat > 1">×{{ r.err_repeat }}</template>
            </el-tag>
            <!-- 非 ERROR 态的历史错误：弱化展示，不冒充"卡住"（见 isStuck/recentErrTitle） -->
            <el-tag v-else-if="r.err_code" size="small" type="info" effect="plain" disable-transitions
                    :title="recentErrTitle(r)">曾出错</el-tag>
          </div>
          <div class="phrase muted small">{{ statePhrase(r.state) }}</div>
        </template>
      </el-table-column>
      <el-table-column label="任务 / 进度" width="200">
        <template #default="{ row: r }">
          <div class="task-cell" :title="taskCells[r.account]?.hint || ''">
            <!-- 主行：链路 + 进度（👻 抓鬼 x/50、🆕 新手链 x/52、🆕✅ 新手链完成 …） -->
            <div class="task-row">
              <span class="tag" :class="[taskCells[r.account]?.tag, { 'num-up': r._flash }]">{{ taskCells[r.account]?.text }}</span>
              <!-- 新手链完成的号同时在抓鬼时，两个身份都要看得见 -->
              <span v-if="r.chain_done && taskCells[r.account]?.tag !== 'ok'" class="tag ok dim"
                    title="新手链已完成（chain_done=true），这个号现在是抓鬼号">🆕✅</span>
            </div>
            <!-- 进度条：离"当天抓满 / 整条链跑完"还有多远 -->
            <div v-if="taskCells[r.account]?.pct !== null && taskCells[r.account]?.pct !== undefined"
                 class="task-bar" :class="taskCells[r.account]?.bar">
              <i :style="{ width: (taskCells[r.account]?.pct || 0) + '%' }" />
            </div>
            <!-- 次行：此刻在做什么（任务真名），或终态说明 -->
            <div v-if="taskCells[r.account]?.sub" class="muted small">{{ taskCells[r.account].sub }}</div>
          </div>
        </template>
      </el-table-column>
      <el-table-column label="血量" width="120">
        <template #default="{ row: r }">
          <div class="hpbar" :class="hpClass(r)" :title="'HP ' + hpText(r.hp) + (r.mp ? ' / MP ' + hpText(r.mp) : '')">
            <i :style="{ width: hpPct(r.hp) + '%' }" />
          </div>
          <div class="muted small mono">{{ hpText(r.hp) }}</div>
        </template>
      </el-table-column>
      <el-table-column label="地图" min-width="140" show-overflow-tooltip>
        <template #default="{ row: r }">
          <span :title="r.mapid ? `mapid=${r.mapid}` : ''">{{ mapLabel(r.mapid) }}</span>
        </template>
      </el-table-column>
      <el-table-column label="坐标(格)" width="104">
        <template #default="{ row: r }">
          <span class="mono" :title="'像素: ' + posPxLabel(r.pos)">{{ posLabel(r.pos) }}</span>
        </template>
      </el-table-column>
      <el-table-column label="等级" width="68">
        <template #default="{ row: r }">{{ r.level || '--' }}</template>
      </el-table-column>
      <el-table-column label="握手" width="92">
        <template #default="{ row: r }">
          <el-tag size="small" disable-transitions :type="r.hs ? 'success' : 'info'"
                  :effect="r.hs ? 'light' : 'plain'">{{ r.hs ? '已握手' : '未握手' }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="心跳" width="96">
        <template #default="{ row: r }">
          <span class="muted" :title="fmtTime(r.last_seen)">{{ fmtAgo(r.last_seen) }}</span>
        </template>
      </el-table-column>
      <!-- 固定在最右：11 列在窄屏要横向滚动，固定列保证「启动/停止/重置/移除」随时点得到 -->
      <el-table-column label="操作" width="188" fixed="right">
        <template #default="{ row: r }">
          <div class="row-actions">
            <el-button link type="primary" size="small"
                       title="按该号意图自动分配（&lt;31 级 → 新手链 / 其余 → 抓鬼），不跟随顶部「启动链路」选择器"
                       @click="startRow(r.account)">启动</el-button>
            <el-button link size="small" @click="stopRow(r.account)">停止</el-button>
            <el-button link size="small" @click="resetRow(r.account)">重置</el-button>
            <el-button link type="danger" size="small" @click="removeRow(r.account)">移除</el-button>
          </div>
        </template>
      </el-table-column>
      <template #empty>
        <el-empty :image-size="60" description="还没有机器人在线">
          <div class="muted small">
            先在「账号池」把账号加进来，或等机器人连上控制通道
            <span class="mono">{{ state.status.ctrl_addr || '--' }}</span>。
          </div>
        </el-empty>
      </template>
    </el-table>

    <div class="pager">
      <!-- 分页：默认 50/页，可切 20/50/100（页码越界由原来的 watch 收敛） -->
      <el-pagination v-model:current-page="page" v-model:page-size="pageSize" :page-sizes="PAGE_SIZES"
                     :total="filtered.length" size="small" background
                     layout="total, sizes, prev, pager, next, jumper" />
    </div>
  </el-card>

  <el-card v-if="failed.items?.length" class="panel-card" shadow="never">
    <template #header>
      <div class="card-head"><span class="card-title">需要看看的账号（重复出错）</span></div>
    </template>
    <el-table :data="failed.items" size="small" max-height="280" row-key="account">
      <el-table-column label="账号" min-width="180" class-name="mono" show-overflow-tooltip>
        <template #default="{ row: it }">{{ it.account }}</template>
      </el-table-column>
      <el-table-column label="错误码" width="120">
        <template #default="{ row: it }">
          <el-tag size="small" type="danger" effect="light" disable-transitions>{{ it.code }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column prop="repeat" label="重复" width="90" />
      <el-table-column prop="age" label="距今(秒)" width="110" />
      <el-table-column prop="msg" label="说明" min-width="240" show-overflow-tooltip />
    </el-table>
  </el-card>
</template>

<style scoped>
/* ============================================================
   Dashboard 的 Element Plus 皮肤补丁（全局深色变量在 styles.css 里，这里不动）
   只做两件事：让 el-card 保持原来 .card 的信息密度；给新控件补极少量布局类
   ============================================================ */

/* 统计卡：网格内并排，必须抵消全局的 `.el-card + .el-card { margin-top:16px }`（那是给上下叠放用的） */
.stat-grid { margin-bottom: 16px; }
.stat-grid > .el-card + .el-card { margin-top: 0; }
.stat-card { height: 100%; }
.stat-card :deep(.el-card__body) { padding: 12px 14px; }
.stat-card .label { display: flex; align-items: center; gap: 5px; color: var(--text-dim); font-size: 12px; }
.stat-card .value {
  display: flex; align-items: baseline; gap: 6px;
  font-size: 26px; font-weight: 700; margin-top: 4px; font-variant-numeric: tabular-nums;
}
.stat-card .value .unit { font-size: 13px; font-weight: 400; color: var(--text-dim); }
.stat-card .sub { color: var(--text-dim); font-size: 12px; margin-top: 2px; }

/* 面板卡 */
.panel-card { margin-bottom: 16px; }
.panel-card :deep(.el-card__header) { padding: 10px 14px; }
.panel-card :deep(.el-card__body) { padding: 12px 14px; }
.card-head { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.card-title { font-size: 14px; font-weight: 600; color: var(--text-dim); }

/* 工具条 / 指标块：保持原来那种"一行塞满、窄屏自动换行"的密度 */
.toolbar { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
/* el-button 相邻时自带 12px 左间距，和 flex gap 叠加会变宽，这里抹平 */
.toolbar :deep(.el-button + .el-button) { margin-left: 0; }
.metrics { display: flex; flex-wrap: wrap; gap: 6px 22px; align-items: baseline; }
.metric { display: flex; align-items: baseline; gap: 5px; }
.k { color: var(--text-dim); font-size: 12px; }
.metric .v { font-variant-numeric: tabular-nums; }
.metric .v.big { font-size: 20px; }
.param-row { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; margin-top: 10px; }
.num { width: 92px; }
.num-sm { width: 78px; }
.sub { color: var(--text-dim); font-size: 12px; margin-top: 6px; }

/* 意图子面板（原来是内嵌的 .picker 块） */
.sub-panel {
  margin-top: 10px; padding: 10px 12px;
  border: 1px solid var(--border); border-radius: 10px;
  background: rgba(255, 255, 255, .02);
}

/* 9 个筛选项要能换行（el-radio-group 默认一行排开） */
.filter-group { flex-wrap: wrap; }
.seg-row { margin-bottom: 10px; }
.pager { display: flex; align-items: center; justify-content: flex-end; margin-top: 10px; }

/* el-table 的斑马纹/悬停底色画在 td 上，所以"进度推进闪一下"的动画也要落到 td，
   否则会被斑马纹盖住（裸 table 时代动画挂在 tr 上就够） */
:deep(.row-flash td.el-table__cell) { animation: rowFlash .8s ease-out; }

/* 固定列（操作）用的是 position:sticky + background:inherit，而全局把 --el-table-tr-bg-color
   设成了 transparent → 横向滚动时右侧的列会从固定列底下透出来。
   把表格行底色设成与卡片一致的实色即可（观感不变，斑马纹/悬停仍由 Element 自己的规则覆盖）。 */
.dash-table { --el-table-tr-bg-color: var(--panel); }
/* 表头行本身没有底色，固定表头要单独给（选择器要比 Element 的固定列规则更具体） */
:deep(.el-table__header-wrapper tr th.el-table-fixed-column--right) { background-color: var(--el-table-header-bg-color); }
</style>
