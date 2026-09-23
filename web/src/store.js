// 前端共享状态（性能优先）：
//   - 权威状态：3 秒轮询 /api/status，但**原地合并**（行对象复用，只 patch 变化的字段）
//   - 实时反馈：WebSocket 事件先入缓冲，按帧批量 flush（默认 ~160ms 一次）
//   - 高频位置：robot_pos_batch 单独走低频通道（默认 400ms 刷一次表格，地图页自绘不受影响）
// 目的：机器人上百个时，前端不因为"整表替换 + 逐条写响应式"卡顿。
import { reactive, ref } from 'vue'
import { apiGet, apiPost, connectWS } from './api'

// ---------------- 可调参数（想更省/更灵敏改这里）----------------
const FLUSH_MS = 160        // 事件批量 flush 间隔
const POS_FLUSH_MS = 400    // 位置刷新间隔
const LOG_MAX = 600         // 运行日志保留条数（环形缓冲，非响应式）
const PENDING_MAX = 3000    // 缓冲上限（超出丢最旧，防极端情况下内存/延迟膨胀）

// ---------------- 响应式状态 ----------------

export const state = reactive({
  status: {
    robots: [],
    counts: { total: 0, online: 0, handshake: 0 },
    zones: [],
    current: {},
    task_failed: { count: 0, items: [] },
    waterline: {},       // 在线水位保持器摘要（/api/status 顺带带）
  },
  statusError: '',
  wsConnected: false,
  lastRefresh: 0,
  toasts: [],
  maps: {},            // mapid -> 地图名（60s 刷新）
  taskNames: {},       // 任务号 -> 任务名（来自游戏配置，60s 刷新；没有就回退显示编号）
  gridCell: 16,        // 客户端坐标口径：1 格 = 16 像素
})

// 运行日志：非响应式环形缓冲 + 版本号（组件依赖 logVersion，避免深响应式开销）
// 每条日志带 _i（自增序号）：供列表用稳定 key + v-memo，避免重排时整表重渲染
export const logEvents = []
export const logVersion = ref(0)
let logSeq = 0

let toastSeq = 0
export function toast(msg, kind = 'info', ms = 4000) {
  const id = ++toastSeq
  state.toasts.push({ id, msg, kind })
  setTimeout(() => {
    const i = state.toasts.findIndex((t) => t.id === id)
    if (i >= 0) state.toasts.splice(i, 1)
  }, ms)
}

// ---------------- 状态合并（关键：不重建行对象）----------------

function sameVal(a, b) {
  if (a === b) return true
  if (Array.isArray(a) && Array.isArray(b)) {
    if (a.length !== b.length) return false
    for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false
    return true
  }
  return false
}

// 把权威行字段写入常驻行对象：只赋真正变化的字段（减少无谓的组件更新）
function mergeRow(row, fresh) {
  let changed = 0
  for (const k in fresh) {
    if (k[0] === '_') continue
    const v = fresh[k]
    if (sameVal(row[k], v)) continue
    row[k] = v
    changed++
  }
  // 权威数据里没有的字段（例如机器人不再上报 booth）要清掉，避免残留
  for (const k in row) {
    if (k[0] === '_') continue
    if (!(k in fresh)) { delete row[k]; changed++ }
  }
  return changed
}

function mergeStatus(next) {
  const st = state.status
  const rows = Array.isArray(next?.robots) ? next.robots : []

  const byAcc = new Map()
  for (const r of st.robots) byAcc.set(r.account, r)

  const merged = new Array(rows.length)
  for (let i = 0; i < rows.length; i++) {
    const fresh = rows[i]
    const acc = fresh.account
    let row = acc ? byAcc.get(acc) : null
    if (row) {
      byAcc.delete(acc)
      mergeRow(row, fresh)
    } else {
      row = { ...fresh, _flash: false, _online0: !!fresh.online }
      if (acc) byAcc.set(acc, row) // 新账号：先登记（下一次轮询复用同一对象）
    }
    merged[i] = row
  }
  // 已下线的老行：标记后丢弃（给 UI 一个转身的余地）
  st.robots = merged

  // 其余字段整体赋值（都是小对象，不会引起大批量 patch）
  st.counts = next?.counts || st.counts
  st.task_failed = next?.task_failed || st.task_failed
  st.zones = next?.zones || st.zones
  st.current = next?.current || st.current
  st.current_keys = next?.current_keys
  st.zone_counts = next?.zone_counts
  // 在线水位保持器摘要（当前/目标/差值/来源/待下线）：/api/status 顺带带，面板少一次请求
  st.waterline = next?.waterline || st.waterline
  for (const k of ['version', 'robot_connected', 'ctrl_addr', 'ctrl_zone', 'robot_running',
    'robot_exe', 'robot_exe_exists', 'server', 'chains', 'maps_count', 'grid_cell',
    'ws_clients', 'logs_file', 'data_dir', 'chain_dir', 'zones_file', 'deploy_dir']) {
    if (k in (next || {})) st[k] = next[k]
  }
  st.removed = next?.removed || st.removed
}

export async function refreshStatus() {
  try {
    const next = await apiGet('/api/status')
    mergeStatus(next)
    state.statusError = ''
    state.lastRefresh = Date.now()
  } catch (e) {
    state.statusError = e.message
  }
}

export async function post(path, body = {}) {
  try {
    const res = await apiPost(path, body)
    if (res.ok === false) toast(res.msg || '操作失败', 'warn')
    else toast(res.msg || '操作成功', 'ok')
    refreshStatus()
    return res
  } catch (e) {
    toast(e.message, 'danger')
    return { ok: false, msg: e.message }
  }
}

function rowOf(account) {
  if (!account) return null
  const rows = state.status.robots
  for (let i = 0; i < rows.length; i++) if (rows[i].account === account) return rows[i]
  return null
}

// 进度上涨时给行打一个短暂的"上跳"标记（拟人化：能看出它在干活）
// 节流：同一行 1.2 秒内只闪一次（进度事件密集时避免反复触发渲染）
const flashTimers = new Map()
const flashLast = new Map()
function flash(row) {
  if (row._flash) return
  const last = flashLast.get(row.account) || 0
  const now = Date.now()
  if (now - last < 1200) return
  flashLast.set(row.account, now)
  row._flash = true
  const old = flashTimers.get(row.account)
  if (old) clearTimeout(old)
  flashTimers.set(row.account, setTimeout(() => {
    row._flash = false
    flashTimers.delete(row.account)
  }, 800))
}

// ---------------- 事件缓冲与批量 flush ----------------

const evtBuf = []
const posBuf = new Map() // account -> {x,y,mapid,zone}
let flushTimer = 0
let posTimer = 0
let hidden = false

function scheduleFlush() {
  if (flushTimer) return
  flushTimer = setTimeout(flushEvents, FLUSH_MS)
}

// 把缓冲里的事件应用到行 + 日志（一次 flush 只触发一轮渲染）
function flushEvents() {
  flushTimer = 0
  if (evtBuf.length) {
    const batch = evtBuf.splice(0, evtBuf.length)
    for (let i = 0; i < batch.length; i++) {
      const ev = batch[i]
      applyEventToRow(ev)
      // 已判完成的号被"启动/重置"时机器人不跑、直接回 already_done：必须说一声 + 指路，
      // 否则看着像没反应（忘了把下拉切到「自动分配」时会一直踩这个坑）。
      // 放在这里而不是 applyEventToRow：账号还没进状态表（刚 add 还没上报）时也要提示。
      if (ev.type === 'chain_done' && ev.already_done) {
        toast(`该号已判完成新手链（${ev.msg || '机器人直接标记 DONE'}），不会重跑；抓鬼请用「自动分配（按意图）」启动`, 'warn', 6000)
      }
      ev._i = ++logSeq
      logEvents.push(ev)
    }
    if (logEvents.length > LOG_MAX) logEvents.splice(0, logEvents.length - LOG_MAX)
    logVersion.value++
  }
}

function schedulePos() {
  if (posTimer) return
  posTimer = setTimeout(flushPos, POS_FLUSH_MS)
}

// 位置单独低频刷新（表格里只是坐标文本；地图页用 canvas 自绘，不受这里限制）
function flushPos() {
  posTimer = 0
  if (!posBuf.size) return
  for (const [account, p] of posBuf) {
    const row = rowOf(account)
    if (!row) continue
    const pos = [p.x, p.y]
    if (!sameVal(row.pos, pos)) row.pos = pos
    if (p.mapid && row.mapid !== p.mapid) row.mapid = p.mapid
    if (p.zone && !row.zone) row.zone = p.zone
  }
  posBuf.clear()
}

function applyEventToRow(ev) {
  const row = rowOf(ev.account)
  if (!row) return
  if (ev._zone && !row.zone) row.zone = ev._zone
  switch (ev.type) {
    case 'robot_online':
      row.online = true; row.state = 'ONLINE'
      if (ev.role_name) row.role_name = ev.role_name
      if (ev.level) row.level = ev.level
      break
    case 'robot_offline':
      row.online = false; row.state = 'OFFLINE'; row.hs = false
      break
    case 'robot_state':
      if (ev.state) row.state = ev.state
      if (ev.task_index !== undefined) row.task_index = ev.task_index
      if (ev.done !== undefined && ev.done !== row.done) { row.done = ev.done; flash(row) }
      if (ev.mapid) row.mapid = ev.mapid
      if (Array.isArray(ev.pos)) row.pos = ev.pos
      if (Array.isArray(ev.hp)) row.hp = ev.hp
      if (Array.isArray(ev.mp)) row.mp = ev.mp
      if (ev.bag) row.bag = ev.bag
      if (ev.summons) row.summons = ev.summons
      break
    case 'task_progress':
      if (ev.done !== undefined && ev.done !== row.done) { row.done = ev.done; flash(row) }
      if (ev.task_index !== undefined) row.task_index = ev.task_index
      break
    case 'chain_done':
      row.state = 'DONE'; row.chain_done = true; flash(row)
      break
    case 'error':
      row.err_code = ev.code; row.err_ts = Date.now() / 1000
      if (ev.msg) row.err_msg = ev.msg
      break
    default:
      break
  }
}

export function pushEvent(ev) {
  if (!ev || !ev.type) return
  if (ev.type === 'robot_pos_batch') {
    for (const item of ev.list || []) posBuf.set(item.account, item)
    schedulePos()
    return
  }
  evtBuf.push(ev)
  if (!ev.ts) ev._t = Date.now() / 1000 // 实时事件没有 ts：记下接收时间，日志页据此显示时间
  if (evtBuf.length > PENDING_MAX) evtBuf.splice(0, evtBuf.length - PENDING_MAX)
  scheduleFlush()
}

// ---------------- 启动 ----------------

async function loadMaps() {
  try {
    const d = await apiGet('/api/maps')
    state.maps = d.maps || {}
    if (d.grid_cell > 0) state.gridCell = d.grid_cell
  } catch (e) { /* 拿不到就回退显示 #mapid */ }
}

// 任务号 → 任务名（读游戏配置 task/*.xml；拿不到就回退：抓鬼段用通用标签、其它显示编号）
async function loadTaskNames() {
  try {
    const d = await apiGet('/api/tasknames')
    state.taskNames = d.names || {}
  } catch (e) { /* 保持上一次缓存 */ }
}

let pollTimer = 0
export function startRealtime() {
  refreshStatus()
  const loop = () => {
    // 页面不可见时降频（省电、避免后台无谓渲染）
    pollTimer = setTimeout(async () => { await refreshStatus(); loop() }, hidden ? 10000 : 3000)
  }
  loop()
  loadMaps()
  loadTaskNames()
  setInterval(loadMaps, 60000)
  setInterval(loadTaskNames, 60000)
  connectWS(pushEvent, (ok) => { state.wsConnected = ok })
  document.addEventListener('visibilitychange', () => { hidden = document.hidden })
}

// ---------------- 多区 ----------------

export function zones() { return state.status.zones || [] }
export function currentZoneKey() { return state.status.current?.key || '' }

export function zoneLabel(key) {
  if (!key) return '--'
  const z = zones().find((x) => x.key === key)
  return z ? z.name : key
}

export function zoneFullLabel(key) {
  if (!key) return '--'
  const z = zones().find((x) => x.key === key)
  if (!z) return key
  if (!z.server_name || z.server_name === z.name) return z.name
  return `${z.server_name}/${z.name}`
}

export async function switchZone(key) {
  return post('/api/config/switch', { key })
}

// ---------------- 展示辅助（拟人化）----------------

export const STATE_LABELS = {
  WAIT_TASK: '等任务', NAV: '导航', CLICK: '点击', DIALOG: '对话', FIGHT: '战斗',
  SHOP: '商店', ALLOC: '加点', WAIT_NEXT: '等下一任务', SUBMIT: '交任务中', WAIT_GHOST: '等刷鬼',
  DONE: '完成', ERROR: '出错', READY: '就绪', IDLE: '空闲', ONLINE: '在线', OFFLINE: '离线',
}

// 状态短语：像人说话（悬停气泡/详情用）
export const STATE_PHRASES = {
  WAIT_TASK: '在等任务派下来',
  NAV: '正在赶路',
  CLICK: '正在点交互',
  DIALOG: '正在跟 NPC 对话',
  FIGHT: '正在打怪',
  SHOP: '正在逛商店',
  ALLOC: '正在加点',
  WAIT_NEXT: '等下一个任务',
  SUBMIT: '正在交任务（抓鬼·交付）',
  WAIT_GHOST: '在钟馗处等鬼刷出来',
  DONE: '这一轮跑完了',
  ERROR: '卡住了，需要看看',
  READY: '已就绪，等派活',
  IDLE: '发着呆，等指令',
  ONLINE: '刚上线',
  OFFLINE: '掉线了',
}

export function stateLabel(s) { return STATE_LABELS[s] || s || '--' }
export function statePhrase(s) { return STATE_PHRASES[s] || STATE_LABELS[s] || '状态未知' }

export function stateTagClass(s) {
  if (!s) return 'dim'
  if (s === 'DONE') return 'ok'
  if (s === 'ERROR') return 'danger'
  if (s === 'OFFLINE' || s === 'IDLE') return 'dim'
  if (s === 'FIGHT' || s === 'ONLINE') return 'ok'
  return 'info'
}

// 头像：取角色名首字（没有角色名就取账号尾号），颜色按状态
export function avatarChar(r) {
  const name = (r.role_name || '').trim()
  if (name) return name[0]
  const acc = (r.account || '').replace(/@.*$/, '')
  const m = acc.match(/(\d{2,})$/)
  return m ? m[1].slice(-2) : (acc[0] || '?')
}

const AVATAR_COLORS = {
  FIGHT: ['#ff8a9b', '#7d2534'],
  ERROR: ['#ffd479', '#7a5410'],
  DONE: ['#7ff0bc', '#12503a'],
  IDLE: ['#b9c6e0', '#2b3a55'],
  OFFLINE: ['#8c98b4', '#242f45'],
}
export function avatarStyle(r) {
  const [fg, bg] = AVATAR_COLORS[r.state] || ['#9fc4ff', '#1d3560']
  return { color: fg, background: bg }
}

// 任务中（用于"呼吸"动效）
const ACTIVE_STATES = ['WAIT_TASK', 'NAV', 'CLICK', 'DIALOG', 'FIGHT', 'SHOP', 'ALLOC', 'WAIT_NEXT']
export function isActive(r) { return !!r.online && ACTIVE_STATES.includes(r.state) }

export function fmtTime(ts) {
  if (!ts) return '--'
  const d = new Date(ts * 1000)
  return d.toLocaleTimeString('zh-CN', { hour12: false })
}

// 相对时间（"3 秒前"），比绝对时间更有"活着"的感觉
export function fmtAgo(ts) {
  if (!ts) return '--'
  const sec = Math.max(0, Math.floor(Date.now() / 1000 - ts))
  if (sec < 60) return `${sec} 秒前`
  if (sec < 3600) return `${Math.floor(sec / 60)} 分钟前`
  return `${Math.floor(sec / 3600)} 小时前`
}

// 任务号 → 人话（中控不生产任务数据，这里只把机器人上报的编号翻成能看懂的标签）。
// 抓鬼任务段来自机器人端 daily_ghost.py：打鬼 2019501~2019510 / 交付 2019511 / 高阶 2019512~2019513。
const TASK_LABELS = { 2019511: '抓鬼·交付', 2019512: '抓鬼·高阶', 2019513: '抓鬼·高阶' }
export function taskLabel(ti) {
  const n = Number(ti) || 0
  if (!n) return '--'
  // 优先用游戏配置里的真名（例：2019508 → 捉鬼、2019511 → 交付捉鬼任务）
  const real = state.taskNames[String(n)]
  if (real) return real
  if (n >= 2019501 && n <= 2019510) return '抓鬼·打鬼'
  return TASK_LABELS[n] || String(n)
}
// 悬停提示：保留原始编号（排障时要看它）
export function taskHint(ti) {
  const n = Number(ti) || 0
  if (!n) return '暂无任务'
  const label = taskLabel(n)
  return label === String(n) ? `任务号 ${n}` : `任务号 ${n}（${label}）`
}

export function mapLabel(mapid) {
  if (mapid === null || mapid === undefined || mapid === '' || mapid === 0) return '--'
  const name = state.maps[String(mapid)]
  return name ? `${name}(${mapid})` : `#${mapid}`
}

export function posLabel(pos) {
  if (!pos || pos.length < 2) return '--'
  const [x, y] = pos
  if (typeof x !== 'number' || typeof y !== 'number') return '--'
  const g = state.gridCell > 0 ? state.gridCell : 16
  return `${Math.floor(x / g)}, ${Math.floor(y / g)}`
}

export function posPxLabel(pos) {
  if (!pos || pos.length < 2) return '--'
  return `${pos[0]}, ${pos[1]}`
}

// 血量/法力：机器人上报的是 [当前, 上限]
export function hpPair(v) {
  if (Array.isArray(v) && v.length >= 2) return [Number(v[0]) || 0, Number(v[1]) || 0]
  if (typeof v === 'number') return [v, 0]
  return [0, 0]
}
export function hpPct(v) {
  const [cur, max] = hpPair(v)
  if (!max) return 0
  return Math.max(0, Math.min(100, Math.round((cur / max) * 100)))
}
export function hpText(v) {
  const [cur, max] = hpPair(v)
  return max ? `${cur}/${max}` : (cur ? String(cur) : '--')
}
