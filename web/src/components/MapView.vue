<script setup>
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { apiGet } from '../api'
import { state, mapLabel, posLabel, taskLabel, taskHint, logEvents, logVersion, post, stateTagClass } from '../store'

const mapid = ref(0)
const cellPx = ref(3)
const grid = ref(null)
const mapName = ref('')
const err = ref('')
const loading = ref(false)
const picked = ref('')       // 选中的机器人账号
const hoverName = ref('')
const canvasRef = ref(null)

// 2026-09-22 游荡入口（选中号 → 当前图/选图/随机图/孵化图/停止 + 档位 + 限时）
const roamMode = ref('default')     // 档位（机器人端 ROAM_PROFILES）
const roamMinutes = ref(null)       // 限时（分钟；空 = 不限）
const roamMap = ref(0)              // "选图游荡"的目标图
const hatchMap = ref(6)             // "孵化图游荡"的目标图（孵化白名单 6/17/34/40）
const roamBusy = ref(false)         // 单次下发中
const batchBusy = ref(false)        // 多图分配循环中（防连点重复下发）

// 2026-09-22 批量游荡：本图多选（一个个点太慢）
const selAccounts = ref([])            // 已勾选的账号
// 2026-09-22 批量选图（多图 + 按数量分配）：勾选若干目标图, 把已选号分批派过去
//   batchMaps 是"草稿态"：只在本地编辑，点「分配到所选图」时才真正下发（不参与 3 秒轮询）
const batchMaps = ref([])              // [{id, count}]
const draftIds = ref([])               // 弹窗里 el-checkbox-group 的勾选态（图 id，保持勾选顺序）
const batchDlg = ref(false)            // 批量游荡弹窗
const candPage = ref(1)                // 选号列表页码
const candSize = ref(50)               // 每页条数（20/50/100）
const HATCH_MAPS = [6, 17, 34, 40]  // 服务端 CFG/mount.xml：只有这 4 张图能孵出坐骑
const ROAM_PROFILES = [
  { id: 'default', label: 'default（拟人挂机）' },
  { id: 'dense', label: 'dense（密集游荡）' },
  { id: 'gather', label: 'gather（采集·占位未实现）' },
]
const ROAM_KIND_LABEL = { current: '当前图', picked: '指定图', random: '随机图', hatch: '孵化图' }

const maps = computed(() => state.maps || {})
const robots = computed(() => (state.status?.robots || []).filter((r) => r.online && r.mapid))

// 每张图当前在线人数
const onlineByMap = computed(() => {
  const m = {}
  for (const r of robots.value) m[r.mapid] = (m[r.mapid] || 0) + 1
  return m
})
// 地图下拉：有人的图优先，其次按 id
const mapOptions = computed(() => {
  const list = Object.entries(maps.value).map(([id, name]) => ({ id: Number(id), name, online: onlineByMap.value[id] || 0 }))
  list.sort((a, b) => (b.online - a.online) || (a.id - b.id))
  return list
})
// 2026-09-22 游荡排除图（用户口径）：幽冥界(24) 是抓鬼专属（钟馗所在图），游荡不派
//   → 游荡的「选图」与「多图分配」列表里不出现（机器人端 random_walk 还有一道兜底拒绝，
//   见 config.robot_roam_exclude_maps —— 双保险）。顶部"选择地图"（浏览）不受影响。
const ROAM_EXCLUDE_MAPS = [24]
const roamMapOptions = computed(() => mapOptions.value.filter((m) => !ROAM_EXCLUDE_MAPS.includes(m.id)))
function optLabel(m) { return `${m.name || ('#' + m.id)}（${m.id}）${m.online ? ' · 在线 ' + m.online : ''}` }
const robotsOnMap = computed(() => robots.value.filter((r) => r.mapid === mapid.value))
// 只暴露"个数"给模板：robotsOnMap 每次轮询都是新数组，直接用它会让本组件每 ~3 秒整体重渲染
const onlineCnt = computed(() => robotsOnMap.value.length)
const detail = computed(() => state.status?.robots?.find((r) => r.account === picked.value) || null)
// 2026-09-22 选中号的游荡状态（机器人心跳 walk 字段：{enabled,mapid,state}）
const detailWalk = computed(() => detail.value?.walk || null)

// 状态 → el-tag 类型（沿用 store 的状态分类，保证与其他页面口径一致）
const TAG_TYPE = { ok: 'success', warn: 'warning', danger: 'danger', info: 'primary', dim: 'info' }
function stateType(s) { return TAG_TYPE[stateTagClass(s)] || 'info' }

// 2026-09-23 货币缩写（选号列表「银两 / 储备」列）：0/缺省 → '-'；
//   1234567 → '123.5万'；123456789 → '1.2亿'；<1万 原样。
function fmtMoney(v) {
  const n = Number(v)
  if (!Number.isFinite(n) || n <= 0) return '-'
  if (n >= 1e8) return (n / 1e8).toFixed(1) + '亿'
  if (n >= 1e4) return (n / 1e4).toFixed(1) + '万'
  return String(Math.round(n))
}

// 2026-09-23 「双」标记（选号列表）：今日已领双倍经验 —— 机器人心跳 double_claim_date
//   （YYYYMMDD，机器人端 pre_daily 落盘状态，重启不丢）。
//   机器人端只在"领取日期 == 当天"时上报该值，跨日心跳自然变空 → 这里**非空即今日**，
//   不再按浏览器本地日期二次判定（面板可能与机器人不同机/跨时区，二次判定会误标）。
function hasDouble(row) { return !!row?.double_claim_date }
function doubleTitle(row) {
  const d = String(row?.double_claim_date || '')
  const txt = d.length === 8 ? `${d.slice(0, 4)}-${d.slice(4, 6)}-${d.slice(6, 8)}` : d
  return `今日（${txt}）已领双倍经验 · 优先抓鬼（双倍有时长，别派去游荡）`
}

// 下发游荡：kind = current(当前图) / picked(选图) / random(随机图) / hatch(孵化图·dense)
async function roamStart(kind, accs, targetMap) {
  const d = detail.value
  const targets = (accs && accs.length) ? accs : (d ? [d.account] : [])
  if (!targets.length) return
  const body = { accounts: targets }
  if (kind === 'batchmap') {
    if (!targetMap) return
    body.mapid = targetMap
    kind = 'picked'          // 走"指定图"分支
  }
  if (kind === 'current') {
    // 选中号面板：目标=该号所在图；批量场景没有"选中号"，退回地图页当前图（接口 mapid 必填，0 会被服务端拒）
    body.mapid = (d && d.mapid) || mapid.value || 0
    // 原实现 d.mapid（d 为 null 时 TypeError）→ 批量·当前图 直接报错，现一并修掉
  } else if (kind === 'picked') {
    // 原实现这里调了未导入的 toast() → ReferenceError（失败分支直接抛错），改用 ElMessage
    if (!roamMap.value) { ElMessage.warning('请先选择目标图（「选图」下拉还是空的）'); return }
    body.mapid = roamMap.value
  } else if (kind === 'random') {
    body.mapid = 'random'        // 机器人端从链数据里有网格的图随机挑（每号不同）
  } else if (kind === 'hatch') {
    body.mapid = hatchMap.value  // 孵化白名单 6/17/34/40
    body.mode = 'dense'          // 孵化靠走动踩暗雷：固定密集档（与 mount_egg 内部一致）
  }
  if (body.mode === undefined) body.mode = roamMode.value
  const m = Number(roamMinutes.value)
  if (m > 0) body.minutes = Math.floor(m)
  roamBusy.value = true
  try {
    return await post('/api/random_walk', body)
  } finally {
    roamBusy.value = false
  }
}

async function roamStop(accs) {
  const d = detail.value
  const targets = (accs && accs.length) ? accs : (d ? [d.account] : [])
  if (!targets.length) return
  roamBusy.value = true
  try {
    return await post('/api/random_walk/stop', { accounts: targets })
  } finally {
    roamBusy.value = false
  }
}

// 破坏性操作统一确认（el-dialog 内点停止也一样）
async function confirmStop(text) {
  try {
    await ElMessageBox.confirm(text, '停止游荡', {
      type: 'warning', confirmButtonText: '停止', cancelButtonText: '取消',
      confirmButtonClass: 'el-button--danger',
    })
    return true
  } catch (e) {
    return false      // 取消 / 关闭：什么都不做
  }
}

// ---------------- 2026-09-22 批量游荡（本图多选，弹窗内操作） ----------------
const roamCands = computed(() => robotsOnMap.value)
const roamCandCnt = computed(() => roamCands.value.length)     // 同上：给模板用稳定值
function isSel(acc) { return selAccounts.value.includes(acc) }
function toggleSel(acc) {
  const i = selAccounts.value.indexOf(acc)
  if (i >= 0) selAccounts.value.splice(i, 1)
  else selAccounts.value.push(acc)
}
// 只把"新勾选的图"按剩余未分配数量补齐，不动用户已经填好的数字
function autoSplitIfNeeded() {
  if (!batchMaps.value.length) return
  if (batchMapSum.value > 0) return    // 已手动分配过：保留用户输入（旧实现会整体覆盖，是"输不进去"的观感来源之一）
  autoSplitBatchMaps()
}
function selAll() {
  selAccounts.value = roamCands.value.map((r) => r.account)
  autoSplitIfNeeded()
}
function selNone() { selAccounts.value = [] }
function selByFilter(kind) {
  // idle: 只怕任务链/抓鬼占着 → 只勾状态为 READY/DONE 的（空闲）
  const idle = ['READY', 'DONE', 'IDLE']
  const list = kind === 'idle'
    ? roamCands.value.filter((r) => idle.includes(String(r.state || '').toUpperCase()))
    : roamCands.value
  selAccounts.value = list.map((r) => r.account)
  autoSplitIfNeeded()
}

// 分页（渲染量收敛：上百个号也只渲染 20/50/100 行）
const maxPage = computed(() => Math.max(1, Math.ceil(roamCands.value.length / candSize.value)))
watch(maxPage, (m) => { if (candPage.value > m) candPage.value = m })
let candCache = []
const pagedCands = computed(() => {
  const s = (candPage.value - 1) * candSize.value
  const rows = roamCands.value.slice(s, s + candSize.value)
  // 行对象是 store 里"原地合并"的同一批引用：本页没换人也没增删时，把上一次的数组原样返回，
  // 这样进度位置(position)每 400ms 的更新不会让 el-table 整表重算列宽（单元格该变的字段照样会更新）
  if (candCache.length === rows.length && rows.every((r, i) => r === candCache[i])) return candCache
  candCache = rows
  return rows
})
const pageAccs = computed(() => pagedCands.value.map((r) => r.account))
const pageAllSel = computed(() => pageAccs.value.length > 0 && pageAccs.value.every((a) => selAccounts.value.includes(a)))
const pageSomeSel = computed(() => !pageAllSel.value && pageAccs.value.some((a) => selAccounts.value.includes(a)))
// 表头勾选框：只增删本页的号，不重新平均（避免覆盖用户手填的数量）
function togglePageAll(v) {
  const set = new Set(selAccounts.value)
  for (const a of pageAccs.value) { if (v) set.add(a); else set.delete(a) }
  selAccounts.value = Array.from(set)
}

async function roamBatch(kind) {
  if (!selAccounts.value.length) { ElMessage.warning('请先选号（左侧列表勾选）'); return }
  const n = selAccounts.value.length
  const res = await roamStart(kind, selAccounts.value.slice())
  if (res?.ok) ElMessage.success(`已下发${ROAM_KIND_LABEL[kind] || ''}游荡：${n} 个号`)
}
async function roamBatchStopConfirm() {
  const n = selAccounts.value.length
  if (!n) { ElMessage.warning('请先选号（左侧列表勾选）'); return }
  if (!(await confirmStop(`停止 ${n} 个号的游荡？它们会留在各自的当前图，之后可以再派。`))) return
  const res = await roamStop(selAccounts.value.slice())
  if (res?.ok) ElMessage.success(`已停止 ${n} 个号的游荡`)
}
async function roamStopPicked() {
  const d = detail.value
  if (!d) return
  if (!(await confirmStop(`停止 ${d.account} 的游荡？它会留在当前图，之后可以再派。`))) return
  const res = await roamStop()
  if (res?.ok) ElMessage.success(`已停止游荡：${d.account}`)
}

// ---------------- 批量选图（多图 + 数量分配，草稿态） ----------------
function inBatchMap(id) { return batchMaps.value.some((m) => m.id === id) }
function countOf(id) { const m = batchMaps.value.find((x) => x.id === id); return m ? Number(m.count) || 0 : 0 }
function setCount(id, v) {
  const m = batchMaps.value.find((x) => x.id === id)
  if (m) m.count = Math.max(0, Math.floor(Number(v) || 0))
}
// 勾选变化：保留已填数量；新勾的图只补"剩余未分配"（余数给靠前的）
function onDraftIds(ids) {
  const prev = batchMaps.value
  const kept = []
  const added = []
  let used = 0
  for (const id of ids) {
    const m = prev.find((x) => x.id === id)
    if (m) { const c = Number(m.count) || 0; kept.push({ id, count: c }); used += c }
    else added.push(id)
  }
  const rest = Math.max(0, selAccounts.value.length - used)
  const base = Math.floor(rest / (added.length || 1))
  const rem = added.length ? rest % added.length : 0
  const addMap = new Map(added.map((id, i) => [id, base + (i < rem ? 1 : 0)]))
  batchMaps.value = ids.map((id) => kept.find((k) => k.id === id) || { id, count: addMap.get(id) || 0 })
}
// 平均分配：把已选号尽量均摊到勾选的图上（余数给靠前的图）
function autoSplitBatchMaps() {
  const n = selAccounts.value.length
  const k = batchMaps.value.length
  if (!k) return
  const base = Math.floor(n / k)
  const rem = n % k
  batchMaps.value.forEach((m, i) => { m.count = base + (i < rem ? 1 : 0) })
}
function clearDraftMaps() { batchMaps.value = []; draftIds.value = [] }
const batchMapSum = computed(() => batchMaps.value.reduce((s, m) => s + (Number(m.count) || 0), 0))

function openBatchDlg() {
  draftIds.value = batchMaps.value.map((m) => m.id)   // 用当前草稿回填勾选态
  candPage.value = 1
  batchDlg.value = true
}

async function roamBatchToMaps() {
  const sel = selAccounts.value.slice()
  if (!sel.length) { ElMessage.warning('请先选号（左侧列表勾选）'); return }
  if (!batchMaps.value.length) { ElMessage.warning('请先勾选目标图（右侧列表）'); return }
  if (batchBusy.value) return
  batchBusy.value = true
  let idx = 0
  const done = []
  try {
    for (const m of batchMaps.value) {
      const cnt = Math.max(0, Math.min(Number(m.count) || 0, sel.length - idx))
      if (cnt <= 0) continue
      const accs = sel.slice(idx, idx + cnt)
      idx += cnt
      const res = await roamStart('batchmap', accs, m.id)
      if (res?.ok === false) continue           // 失败的那批不写进小结（post() 已 toast 原因）
      done.push(`${mapLabel(m.id)}×${accs.length}`)
      await new Promise((r) => setTimeout(r, 250))   // 轻微间隔, 别把控制通道打爆
    }
  } finally {
    batchBusy.value = false
  }
  // 原实现这里调了未导入的 toast()（ReferenceError，小结永远看不到），改用 ElMessage
  if (done.length) {
    ElMessage.success(`已分配游荡：${done.join(' / ')}`)
    batchDlg.value = false
  } else {
    ElMessage.warning('没有可分配的号/图（检查每图数量是否都填了 0）')
  }
}

// 2026-09-22 选中号的日志输出：历史种子(/api/logs 一次) + 实时(logEvents, 随 logVersion 刷新)。
//   只取该账号的事件, 最近 80 条, 倒序阅读更顺手(最新在底部, 自动滚到底)。
const seedLogs = ref([])
const logFilter = ref('all')        // all / error / warn
const rlogEl = ref(null)
function logLevel(ev) {
  const tp = ev.type || ''
  if (tp === 'error') return 'error'
  if (tp === 'ghost_offline') return 'warn'
  const lv = String(ev.level || '')
  if (lv === 'error' || lv === 'critical') return 'error'
  if (lv === 'warn' || lv === 'warning') return 'warn'
  return 'info'
}
// 只产出"要渲染的行"：带稳定 key（历史用负数序号，实时用 store 的 _i），配合 v-memo 让旧行不重渲染
const pickedLogRows = computed(() => {
  void logVersion.value	// 依赖版本号: logEvents 是非响应式数组, 靠它触发重算
  const acc = picked.value
  if (!acc) return []
  const live = logEvents.filter((ev) => ev.account === acc)
  const hist = seedLogs.value.filter((ev) => ev.account === acc)
  const all = hist.concat(live)
  const rows = []
  for (let i = Math.max(0, all.length - 200); i < all.length; i++) {
    const ev = all[i]
    const lv = logLevel(ev)
    if (logFilter.value !== 'all' && lv !== logFilter.value) continue
    rows.push({ key: ev._i, t: rlogTime(ev.ts || ev._t), text: rlogText(ev), lv })
  }
  return rows.slice(-80)
})
function rlogTime(ts) {
  const n = Number(ts) || 0
  if (!n) return '--:--:--'
  const d = new Date(n * 1000)	// 事件 ts 是"秒"(与中控 runs 一致)
  return isNaN(d.getTime()) ? '--:--:--' : d.toTimeString().slice(0, 8)
}
function rlogText(ev) {
  const tp = ev.type || ev.kind || ''
  const msg = ev.msg || ev.text || ev.action || ''
  return `[${tp}] ${String(msg)}`.slice(0, 180)
}
// 贴底才自动跟随（用户往上翻看历史时不打断）
let rlogBottom = true
function onRlogScroll() {
  const el = rlogEl.value
  if (!el) return
  rlogBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 40
}
watch(logVersion, () => {
  if (!rlogBottom) return
  nextTick(() => {
    const el = rlogEl.value
    if (el && rlogBottom) el.scrollTop = el.scrollHeight
  })
})

async function loadGrid() {
  if (!mapid.value) return
  loading.value = true
  try {
    const d = await apiGet(`/api/map/grid?mapid=${mapid.value}`)
    if (!d.ok) { err.value = d.msg || '网格不可用'; grid.value = null; return }
    grid.value = d.grid
    mapName.value = d.name || ''
    err.value = ''
  } catch (e) {
    err.value = e.message
  } finally {
    loading.value = false
  }
}
watch(mapid, () => { loadGrid(); picked.value = '' })   // 2026-09-22: 不再把"选图"自动设成当前图(用户要手动选)

onMounted(async () => {
  // 默认选"在线人数最多"的图，没有则选 1
  const first = mapOptions.value.find((m) => m.online > 0) || mapOptions.value.find((m) => m.id === 1) || mapOptions.value[0]
  if (first) mapid.value = first.id
  // 2026-09-22 用户口径: "选图游荡"必须**手动选图**, 不再默认跟随当前图(roamMap=0 表示未选)
  // 2026-09-22 选号日志: 先拉一次历史(带 account 的事件), 之后由 WS 实时日志续上
  try {
    const res = await apiGet('/api/logs?n=600')
    // 历史日志补一个负数序号：和实时日志的 _i 一起做稳定 key（避免用数组下标当 key 导致整块重渲染）
    seedLogs.value = (res.logs || []).filter((e) => e && e.account).slice(-200).map((e, i) => ({ ...e, _i: -(i + 1) }))
  } catch (e) { /* 不阻塞地图 */ }
})

// ---------------------------------------------------------------- 绘制

const COL_BLOCK = [41, 49, 60]
const COL_FREE = [122, 132, 148]
const STATE_COLORS = {
  FIGHT: '#ff5d6c', ERROR: '#f5b83d', DIALOG: '#4c8dff', NAV: '#46c6e6',
  SHOP: '#b18cff', ALLOC: '#b18cff', DONE: '#3ecf8e', IDLE: '#93a1c0',
}
let raf = 0
let lastDraw = 0
let sigPrev = ''
const FRAME_MIN_MS = 33      // 上限 ~30fps：地图是静态底图 + 少量点，没必要每帧重画
const anim = new Map()   // account -> {x, y, tx, ty}
let gridCache = { key: '', img: null }

function colorOf(r) {
  return STATE_COLORS[r.state] || '#4c8dff'
}

// 变化指纹：位置目标/选中/悬停/缩放/地图/游荡标记都没变就不重绘（空闲时不烧 CPU）
function frameSig() {
  let s = `${mapid.value}:${cellPx.value}:${picked.value}:${hoverName.value}`
  for (const r of robotsOnMap.value) s += `|${r.account},${r.pos?.[0] || 0},${r.pos?.[1] || 0},${r.state},${r.walk?.enabled ? 1 : 0}`
  return s
}

function settled() {
  for (const a of anim.values()) {
    if (Math.abs(a.tx - a.x) > 0.6 || Math.abs(a.ty - a.y) > 0.6) return false
  }
  return true
}

function draw(now) {
  raf = requestAnimationFrame(draw)
  if (!grid.value) return
  const sig = frameSig()
  if (sig === sigPrev && settled()) return           // 静止且无变化：跳过（关键优化）
  if (now - lastDraw < FRAME_MIN_MS) return           // 限帧
  lastDraw = now
  sigPrev = sig

  const cvs = canvasRef.value
  const g = grid.value
  const cp = cellPx.value
  const W = g.w * cp
  const H = g.h * cp
  if (cvs.width !== W || cvs.height !== H) { cvs.width = W; cvs.height = H }

  const ctx = cvs.getContext('2d')
  // 底图（按 mapid+cp 缓存）
  const key = `${mapid.value}:${cp}:${g.w}x${g.h}`
  if (gridCache.key !== key) {
    const off = document.createElement('canvas')
    off.width = W; off.height = H
    const octx = off.getContext('2d')
    const img = octx.createImageData(W, H)
    for (let gy = 0; gy < g.h; gy++) {
      const row = g.rows[gy] || ''
      for (let gx = 0; gx < g.w; gx++) {
        const blocked = gx >= row.length || row[gx] === '1'
        const c = blocked ? COL_BLOCK : COL_FREE
        for (let py = 0; py < cp; py++) {
          for (let px = 0; px < cp; px++) {
            const idx = (((gy * cp) + py) * W + ((gx * cp) + px)) * 4
            img.data[idx] = c[0]; img.data[idx + 1] = c[1]; img.data[idx + 2] = c[2]; img.data[idx + 3] = 255
          }
        }
      }
    }
    octx.putImageData(img, 0, 0)
    gridCache = { key, img: off }
  }
  ctx.clearRect(0, 0, W, H)
  ctx.drawImage(gridCache.img, 0, 0)
  // 网格线（放大时）
  if (cp >= 6) {
    ctx.strokeStyle = 'rgba(0,0,0,.12)'
    ctx.beginPath()
    for (let x = 0; x <= g.w; x++) { ctx.moveTo(x * cp, 0); ctx.lineTo(x * cp, H) }
    for (let y = 0; y <= g.h; y++) { ctx.moveTo(0, y * cp); ctx.lineTo(W, y * cp) }
    ctx.stroke()
  }

  // 机器人点位（简易插值：向目标位置靠拢）
  const seen = new Set()
  for (const r of robotsOnMap.value) {
    seen.add(r.account)
    const tx = (r.pos?.[0] || 0) / 16 * cp
    const ty = (r.pos?.[1] || 0) / 16 * cp
    let a = anim.get(r.account)
    if (!a) { a = { x: tx, y: ty, tx, ty }; anim.set(r.account, a) }
    a.tx = tx; a.ty = ty
    a.x += (a.tx - a.x) * 0.18
    a.y += (a.ty - a.y) * 0.18

    const gx = Math.floor((r.pos?.[0] || 0) / 16)
    const gy = Math.floor((r.pos?.[1] || 0) / 16)
    const wall = g.rows[gy] ? (g.rows[gy][gx] === '1') : true
    const isPicked = r.account === picked.value
    const rad = Math.max(3, cp * 0.6)
    ctx.beginPath()
    ctx.arc(a.x, a.y, isPicked ? rad + 2 : rad, 0, Math.PI * 2)
    ctx.fillStyle = wall ? '#ff2d55' : colorOf(r)
    ctx.fill()
    // 2026-09-22 游荡中的号：加一圈绿环（一眼看出谁在游荡/孵化）
    if (r.walk?.enabled) {
      ctx.beginPath()
      ctx.arc(a.x, a.y, rad + 3, 0, Math.PI * 2)
      ctx.strokeStyle = '#3ecf8e'
      ctx.lineWidth = 1.5
      ctx.stroke()
    }
    if (isPicked || hoverName.value === r.account) {
      ctx.lineWidth = 2
      ctx.strokeStyle = '#fff'
      ctx.stroke()
      ctx.fillStyle = '#fff'
      ctx.font = '12px Consolas, monospace'
      ctx.fillText(r.walk?.enabled ? `${r.account}·游荡` : r.account, a.x + rad + 2, a.y - rad)
    }
  }
  for (const k of Array.from(anim.keys())) if (!seen.has(k)) anim.delete(k)
}

onMounted(() => { raf = requestAnimationFrame(draw) })
onUnmounted(() => cancelAnimationFrame(raf))

// ---------------------------------------------------------------- 交互

function onClick(e) {
  if (!grid.value) return
  const rect = canvasRef.value.getBoundingClientRect()
  const mx = e.clientX - rect.left
  const my = e.clientY - rect.top
  const cp = cellPx.value
  let hit = ''
  for (const r of robotsOnMap.value) {
    const a = anim.get(r.account)
    if (!a) continue
    if (Math.hypot(a.x - mx, a.y - my) <= Math.max(6, cp)) { hit = r.account; break }
  }
  picked.value = hit || ''
}

function onMove(e) {
  if (!grid.value) return
  const rect = canvasRef.value.getBoundingClientRect()
  const mx = e.clientX - rect.left
  const my = e.clientY - rect.top
  const cp = cellPx.value
  let hit = ''
  for (const r of robotsOnMap.value) {
    const a = anim.get(r.account)
    if (!a) continue
    if (Math.hypot(a.x - mx, a.y - my) <= Math.max(6, cp)) { hit = r.account; break }
  }
  hoverName.value = hit
}

function hpPct(v) {
  if (!Array.isArray(v) || v.length < 2 || !v[1]) return 0
  return Math.max(0, Math.min(100, Math.round(v[0] / v[1] * 100)))
}
function bagList(r) { return Array.isArray(r.bag) ? r.bag : [] }
function summonsList(r) { return Array.isArray(r.summons) ? r.summons : [] }
</script>

<template>
  <el-card class="panel-card toolbar" shadow="never">
    <div class="row">
      <el-select v-model="mapid" filterable size="small" class="map-select" placeholder="选择地图">
        <el-option v-for="m in mapOptions" :key="m.id" :value="m.id" :label="optLabel(m)" />
      </el-select>
      <span class="lbl">缩放</span>
      <el-slider v-model="cellPx" :min="2" :max="8" :step="1" class="zoom" />
      <el-tag size="small" type="info" effect="plain">{{ cellPx }} px/格</el-tag>
      <span class="spacer" />
      <el-tag size="small" :type="onlineCnt ? 'success' : 'info'">本图在线 {{ onlineCnt }}</el-tag>
      <el-tag size="small" type="info" effect="plain">图上共 {{ grid ? `${grid.w}×${grid.h} 格` : '--' }}</el-tag>
      <el-tag v-if="loading" size="small" type="warning">加载中…</el-tag>
    </div>
  </el-card>

  <el-card v-if="err" class="panel-card" shadow="never">
    <el-alert :title="err" type="error" :closable="false" show-icon />
  </el-card>

  <div class="map-layout" :class="{ 'has-detail': !!detail }">
    <div class="col">
      <el-card class="panel-card" shadow="never">
        <div class="row" style="margin-bottom:8px">
          <b>{{ mapName || mapLabel(mapid) }}</b>
          <span class="muted small">mapid={{ mapid }} · 点击圆点查看机器人详情</span>
        </div>
        <div class="canvas-wrap">
          <canvas ref="canvasRef" @click="onClick" @mousemove="onMove" @mouseleave="hoverName = ''" />
        </div>
        <div class="row" style="margin-top:8px">
          <el-tag size="small" type="info" effect="plain">可走</el-tag><span class="swatch free" />
          <el-tag size="small" type="info" effect="plain">阻挡</el-tag><span class="swatch block" />
          <span class="muted small">点位颜色 = 状态；<span class="red-note">红点</span>=站在阻挡格（异常）</span>
        </div>
      </el-card>

      <!-- 2026-09-22 批量游荡：入口常驻，选号/选图收进弹窗（避免上百行列表每 3 秒被重渲染） -->
      <el-card class="panel-card" shadow="never">
        <template #header>
          <div class="row">
            <b>批量游荡</b>
            <el-tag size="small" type="info" effect="plain">本图在线 {{ roamCandCnt }}</el-tag>
            <el-tag size="small" :type="selAccounts.length ? 'success' : 'info'">已选 {{ selAccounts.length }}</el-tag>
            <el-tag v-if="batchMaps.length" size="small" type="warning" effect="plain">
              {{ batchMaps.length }} 图 · 合计 {{ batchMapSum }} 号
            </el-tag>
            <span class="spacer" />
            <el-button type="primary" size="small" @click="openBatchDlg">
              <el-icon><Operation /></el-icon>批量游荡…
            </el-button>
            <el-button size="small" :disabled="batchBusy || roamBusy || !selAccounts.length || !batchMaps.length"
                       @click="roamBatchToMaps()">批量·分配到所选图</el-button>
            <el-button size="small" type="danger" plain :disabled="roamBusy || !selAccounts.length"
                       @click="roamBatchStopConfirm">批量·停止游荡</el-button>
          </div>
        </template>
        <div class="muted small">
          候选号 = 地图「当前图」的在线号。点「批量游荡…」选号 → 勾目标图并填每图数量 → 下发。
          游荡与抓鬼/任务链互斥；档位/限时与下方「选中号」面板共用同一份设置（当前 {{ roamMode }}<span v-if="roamMinutes"> · {{ roamMinutes }} 分钟</span>）。
        </div>
      </el-card>
    </div>

    <!-- 详情面板 -->
    <el-card v-if="detail" class="panel-card detail-card" shadow="never">
      <div class="row" style="margin-bottom:8px">
        <b class="mono">{{ detail.account }}</b>
        <span class="spacer" />
        <el-button size="small" @click="picked = ''">关闭</el-button>
      </div>
      <table class="kv-table">
        <tbody>
          <tr><td class="muted">角色</td><td>{{ detail.role_name || '--' }} Lv{{ detail.level || '--' }}</td></tr>
          <tr><td class="muted">状态</td><td>
            <el-tag size="small" :type="stateType(detail.state)">{{ detail.state || '--' }}</el-tag>
            <el-tag size="small" :type="detail.online ? 'success' : 'info'" effect="plain">{{ detail.online ? '在线' : '离线' }}</el-tag>
            <el-tag size="small" :type="detail.hs ? 'success' : 'warning'" effect="plain">{{ detail.hs ? '已握手' : '未握手' }}</el-tag>
          </td></tr>
          <tr><td class="muted">区 / 地图</td><td>{{ detail.zone || '--' }} · {{ mapLabel(detail.mapid) }}</td></tr>
          <tr><td class="muted">坐标(格) / 像素</td>
            <td class="mono">{{ posLabel(detail.pos) }} / {{ (detail.pos || []).join(', ') || '--' }}</td></tr>
          <tr><td class="muted">任务</td><td class="mono" :title="taskHint(detail.task_index)">{{ taskLabel(detail.task_index) }} · 进度 {{ detail.done || 0 }}</td></tr>
          <tr v-if="detail.err_code"><td class="muted">最近错误</td>
            <td><el-tag size="small" type="danger">{{ detail.err_code }} ×{{ detail.err_repeat || 1 }}</el-tag>
              <span class="muted">{{ detail.err_msg }}</span></td></tr>
        </tbody>
      </table>

      <!-- 2026-09-22 游荡入口（选中号）：当前图 / 选图 / 随机图 / 孵化图 / 停止 + 档位 + 限时 -->
      <div class="sec">游荡
        <el-tag size="small" :type="detailWalk?.enabled ? 'success' : 'info'" effect="plain">
          {{ detailWalk?.enabled ? '游荡中' : '未游荡' }}
        </el-tag>
      </div>
      <div v-if="detailWalk?.enabled" class="roam-now">
        目标图 {{ mapLabel(detailWalk.mapid) }} · 状态 {{ detailWalk.state || '--' }}
      </div>
      <div class="row form-row">
        <span class="lbl">档位</span>
        <el-select v-model="roamMode" size="small" style="width:186px">
          <el-option v-for="p in ROAM_PROFILES" :key="p.id" :value="p.id" :label="p.label" />
        </el-select>
        <span class="lbl">限时</span>
        <el-input-number v-model="roamMinutes" size="small" :min="1" :max="1440" :step="5" :precision="0"
                         controls-position="right" placeholder="不限" style="width:126px" />
        <span class="muted small">分钟（留空=不限）</span>
      </div>
      <div class="row form-row">
        <span class="lbl">选图</span>
        <el-select v-model="roamMap" size="small" filterable style="width:210px" placeholder="（请选择目标图）">
          <el-option :value="0" label="（请选择目标图）" />
          <el-option v-for="m in roamMapOptions" :key="m.id" :value="m.id" :label="optLabel(m)" />
        </el-select>
        <span class="lbl">孵化图</span>
        <el-select v-model="hatchMap" size="small" style="width:150px">
          <el-option v-for="hid in HATCH_MAPS" :key="hid" :value="hid" :label="mapLabel(hid)" />
        </el-select>
      </div>
      <div class="row" style="margin:6px 0">
        <el-button size="small" :disabled="roamBusy" @click="roamStart('current')">当前图游荡</el-button>
        <el-button size="small" :disabled="roamBusy || !roamMap" :title="roamMap ? '' : '先在上方选择目标图'"
                   @click="roamStart('picked')">选图游荡</el-button>
        <el-button size="small" :disabled="roamBusy" @click="roamStart('random')">随机图游荡</el-button>
        <el-button size="small" :disabled="roamBusy" @click="roamStart('hatch')">孵化图游荡(dense)</el-button>
        <el-button size="small" type="danger" plain :disabled="roamBusy" @click="roamStopPicked">停止游荡</el-button>
      </div>
      <div class="muted small roam-hint">
        游荡与任务链/抓鬼互斥（机器人端会先停它们）；孵化图=白名单 6/17/34/40（号上没蛋则无灵气收益）；
        档位 gather 为采集占位（机器人端明确回退 default，别当采集用）。
      </div>

      <!-- 血量/法力 -->
      <div class="bars">
        <div class="bar-row">
          <span class="lab">HP</span>
          <div class="bar"><div class="fill hp" :style="{ width: hpPct(detail.hp) + '%' }" /></div>
          <span class="mono">{{ (detail.hp || []).join(' / ') || '--' }}</span>
        </div>
        <div class="bar-row">
          <span class="lab">MP</span>
          <div class="bar"><div class="fill mp" :style="{ width: hpPct(detail.mp) + '%' }" /></div>
          <span class="mono">{{ (detail.mp || []).join(' / ') || '--' }}</span>
        </div>
      </div>

      <!-- 守护 -->
      <div class="sec">守护（{{ summonsList(detail).length }}）</div>
      <table v-if="summonsList(detail).length" class="mini">
        <thead><tr><th>名称</th><th>等级</th><th>忠诚</th><th>亲密度</th><th>HP</th><th>出战</th></tr></thead>
        <tbody>
          <tr v-for="(s, i) in summonsList(detail)" :key="i">
            <td>{{ s.name || ('#' + s.id) }}</td>
            <td>{{ s.level ?? '--' }}</td>
            <td>{{ s.happy ?? '--' }}</td>
            <td>{{ s.close ?? '--' }}</td>
            <td class="mono">{{ (s.hp || []).join('/') || '--' }}</td>
            <td><el-tag size="small" :type="s.fighting ? 'success' : 'info'" effect="plain">{{ s.fighting ? '出战' : '休息' }}</el-tag></td>
          </tr>
        </tbody>
      </table>
      <div v-else class="muted" style="padding:4px 0">（无守护数据）</div>

      <!-- 背包 -->
      <div class="sec">背包（{{ bagList(detail).length }}）</div>
      <div v-if="bagList(detail).length" class="bag">
        <div v-for="(it, i) in bagList(detail)" :key="i" class="bag-item" :title="'itemid=' + it.id">
          <span class="bname">{{ it.name || ('#' + it.id) }}</span>
          <span class="bcount" v-if="it.count > 1">×{{ it.count }}</span>
        </div>
      </div>
      <div v-else class="muted" style="padding:4px 0">（无背包数据）</div>

      <!-- 2026-09-22 选中号的日志输出（历史 + 实时） -->
      <div class="sec">
        日志（{{ pickedLogRows.length }} 条）
        <span class="spacer" />
        <el-radio-group v-model="logFilter" size="small">
          <el-radio-button value="all">全部</el-radio-button>
          <el-radio-button value="error">仅错误</el-radio-button>
          <el-radio-button value="warn">仅警告</el-radio-button>
        </el-radio-group>
      </div>
      <div ref="rlogEl" class="rlog" @scroll.passive="onRlogScroll">
        <div v-for="row in pickedLogRows" :key="row.key" v-memo="[row.key]"
             class="rlog-line" :class="row.lv">
          <span class="rlog-t">{{ row.t }}</span>
          <span class="rlog-m">{{ row.text }}</span>
        </div>
        <div v-if="!pickedLogRows.length" class="muted" style="padding:4px 0">
          （暂无该号日志：等它产生事件，或换个筛选看看）
        </div>
      </div>

      <!-- 摆摊 -->
      <div v-if="detail.booth" class="sec">摆摊</div>
      <table v-if="detail.booth" class="mini">
        <tbody>
          <tr><td class="muted">状态</td><td>{{ detail.booth.state }}</td>
            <td class="muted">已售</td><td>{{ detail.booth.sold ?? 0 }}</td>
            <td class="muted">收入</td><td>{{ detail.booth.income ?? 0 }}</td></tr>
        </tbody>
      </table>
    </el-card>
  </div>

  <!-- 批量游荡弹窗：选号（表格+分页） + 选图（每图数量） + 下发。
       destroy-on-close：关掉后内容整体销毁，列表不再参与 3 秒轮询的重渲染（"一直在刷新"的主因之一） -->
  <el-dialog v-model="batchDlg" title="批量游荡" width="1000px" top="4vh" append-to-body
             :close-on-click-modal="false" destroy-on-close>
    <div class="bdg">
      <div class="row form-row bdg-params">
        <span class="lbl">档位</span>
        <el-select v-model="roamMode" size="small" style="width:180px">
          <el-option v-for="p in ROAM_PROFILES" :key="p.id" :value="p.id" :label="p.label" />
        </el-select>
        <span class="lbl">限时</span>
        <el-input-number v-model="roamMinutes" size="small" :min="1" :max="1440" :step="5" :precision="0"
                         controls-position="right" placeholder="不限" style="width:126px" />
        <span class="lbl">选图</span>
        <el-select v-model="roamMap" size="small" filterable style="width:200px" placeholder="（请选择目标图）">
          <el-option :value="0" label="（请选择目标图）" />
          <el-option v-for="m in roamMapOptions" :key="m.id" :value="m.id" :label="optLabel(m)" />
        </el-select>
        <span class="lbl">孵化图</span>
        <el-select v-model="hatchMap" size="small" style="width:140px">
          <el-option v-for="hid in HATCH_MAPS" :key="hid" :value="hid" :label="mapLabel(hid)" />
        </el-select>
      </div>

      <div class="bdg-cols">
        <!-- ① 选号 -->
        <div class="bdg-col">
          <div class="bdg-head">
            <b>① 选号</b>
            <span class="muted small">本图在线 {{ roamCandCnt }} · 已选 <b class="ok-text">{{ selAccounts.length }}</b></span>
            <span class="spacer" />
            <el-button size="small" @click="selByFilter('idle')" title="只勾空闲号（READY/DONE）——最适合批量派游荡">选空闲</el-button>
            <el-button size="small" @click="selAll()">全选本图</el-button>
            <el-button size="small" @click="selNone()">清空</el-button>
          </div>
          <el-table :data="pagedCands" size="small" height="320" class="cand-table">
            <el-table-column width="42">
              <template #header>
                <el-checkbox :model-value="pageAllSel" :indeterminate="pageSomeSel"
                             @change="(v) => togglePageAll(v)" title="本页全选" />
              </template>
              <template #default="{ row }">
                <el-checkbox :model-value="isSel(row.account)" @change="() => toggleSel(row.account)" />
              </template>
            </el-table-column>
            <el-table-column label="账号" prop="account" min-width="160" class-name="mono" show-overflow-tooltip />
            <el-table-column label="状态" width="96">
              <template #default="{ row }">
                <el-tag size="small" :type="stateType(row.state)" effect="plain">{{ row.state || '--' }}</el-tag>
              </template>
            </el-table-column>
            <el-table-column label="等级" width="66">
              <template #default="{ row }">Lv{{ row.level ?? '--' }}</template>
            </el-table-column>
            <!-- 2026-09-23 「双」：今日已领双倍经验（机器人心跳 double_claim_date）。
                 双倍有时长 → 该号应优先抓鬼（中控调度已把它置顶、不派游荡）。 -->
            <el-table-column label="双" width="46" align="center">
              <template #default="{ row }">
                <el-tag v-if="hasDouble(row)" size="small" type="warning" effect="dark"
                        :title="doubleTitle(row)">双</el-tag>
                <span v-else class="muted">—</span>
              </template>
            </el-table-column>
            <!-- 2026-09-23 货币详情：银两(money) / 储备金(reserve)，数据来自机器人心跳
                 （机器人端 90353 全量 + 90073 增量解析，见 msghandle.match_role_data_handle）。
                 0/缺省显示 '-'；完整数值见悬浮提示。 -->
            <el-table-column label="银两 / 储备" width="140">
              <template #default="{ row }">
                <span class="mono ok-text"
                      :title="`银两 ${row.money ?? 0} · 储备金 ${row.reserve ?? 0}`">{{ fmtMoney(row.money) }}</span>
                <span class="muted"> / </span>
                <span class="mono warn-text"
                      :title="`银两 ${row.money ?? 0} · 储备金 ${row.reserve ?? 0}`">{{ fmtMoney(row.reserve) }}</span>
              </template>
            </el-table-column>
            <el-table-column label="游荡" width="80">
              <template #default="{ row }">
                <el-tag v-if="row.walk?.enabled" size="small" type="success">游荡中</el-tag>
                <span v-else class="muted">—</span>
              </template>
            </el-table-column>
            <template #empty>
              <span class="muted">（本图当前没有在线号；先在地图上换到有人的图）</span>
            </template>
          </el-table>
          <div class="bdg-pager">
            <el-pagination v-model:current-page="candPage" v-model:page-size="candSize"
                           :total="roamCandCnt" :page-sizes="[20, 50, 100]" size="small" background
                           layout="total, sizes, prev, pager, next" />
          </div>
        </div>

        <!-- ② 选图 + 每图数量 -->
        <div class="bdg-col">
          <div class="bdg-head">
            <b>② 选图（每图数量）</b>
            <span class="muted small">合计 <b :class="batchMapSum === selAccounts.length ? 'ok-text' : 'warn-text'">{{ batchMapSum }}</b> / 已选 {{ selAccounts.length }}</span>
            <span class="spacer" />
            <el-button size="small" :disabled="!batchMaps.length" @click="autoSplitBatchMaps()">重新平均</el-button>
            <el-button size="small" :disabled="!batchMaps.length" @click="clearDraftMaps()">清空图</el-button>
          </div>
          <el-checkbox-group v-model="draftIds" class="map-pick" @change="onDraftIds">
            <div v-for="m in roamMapOptions" :key="m.id" class="map-pick-row">
              <el-checkbox :value="m.id" class="mp-check">
                <span class="mp-name">{{ m.name || ('#' + m.id) }}</span>
              </el-checkbox>
              <el-tag size="small" type="info" effect="plain">#{{ m.id }}</el-tag>
              <el-tag v-if="m.online" size="small" type="success" effect="plain">在线 {{ m.online }}</el-tag>
              <span v-else class="muted small">在线 0</span>
              <span class="spacer" />
              <el-input-number v-if="inBatchMap(m.id)" size="small" :min="0"
                               :max="Math.max(1, selAccounts.length)" :precision="0" :step="1"
                               controls-position="right" style="width:118px"
                               :model-value="countOf(m.id)"
                               @update:model-value="(v) => setCount(m.id, v)" />
            </div>
          </el-checkbox-group>
          <div class="bdg-diff">
            <template v-if="batchMaps.length && batchMapSum !== selAccounts.length">
              <el-tag size="small" type="warning" effect="plain">合计 {{ batchMapSum }} ≠ 已选 {{ selAccounts.length }}</el-tag>
              <span class="muted small">按顺序尽量分配：排在前面的图先拿满，多出来的号留在原地。</span>
            </template>
            <span v-else-if="batchMaps.length" class="muted small">合计与已选一致，会按上面的数量依次下发。</span>
            <span v-else class="muted small">勾选目标图；数量会按"剩余未分配"自动补，也可以手填或点「重新平均」。</span>
          </div>
        </div>
      </div>
    </div>

    <template #footer>
      <div class="row">
        <el-button size="small" :disabled="roamBusy || !selAccounts.length" @click="roamBatch('current')">批量·当前图</el-button>
        <el-button size="small" :disabled="roamBusy || !selAccounts.length || !roamMap" @click="roamBatch('picked')">
          批量·选图{{ roamMap ? '（' + mapLabel(roamMap) + '）' : '(未选)' }}
        </el-button>
        <el-button size="small" :disabled="roamBusy || !selAccounts.length" @click="roamBatch('random')">批量·随机图</el-button>
        <el-button size="small" :disabled="roamBusy || !selAccounts.length" @click="roamBatch('hatch')">批量·孵化图(dense)</el-button>
        <el-button type="primary" size="small" :disabled="batchBusy || roamBusy || !selAccounts.length || !batchMaps.length"
                   @click="roamBatchToMaps()">
          批量·分配到所选图（{{ batchMaps.length }} 图 / {{ batchMapSum }} 号）
        </el-button>
        <el-button type="danger" plain size="small" :disabled="roamBusy || !selAccounts.length" @click="roamBatchStopConfirm">批量·停止游荡</el-button>
        <span class="spacer" />
        <el-button size="small" @click="batchDlg = false">关闭</el-button>
      </div>
    </template>
  </el-dialog>
</template>

<style scoped>
/* 卡片统一 el-card，但底色/内边距贴项目色板（其余页面用的 .card 也是这套变量） */
.panel-card { --el-card-bg-color: var(--panel); --el-card-padding: 14px 16px; border-radius: var(--radius); }
.toolbar { margin-bottom: 14px; }
.map-select { width: 320px; }
.zoom { width: 140px; margin: 0 4px; }
.lbl { color: var(--text-dim); font-size: 12px; }
.small { font-size: 12px; }
.ok-text { color: var(--ok); }
.warn-text { color: var(--warn); }
.red-note { color: var(--danger); }
.form-row { margin: 6px 0; gap: 8px; }

.map-layout { display: grid; grid-template-columns: 1fr; gap: 14px; }
.map-layout.has-detail { grid-template-columns: minmax(0, 1fr) 400px; }
@media (max-width: 1300px) { .map-layout.has-detail { grid-template-columns: 1fr; } }
.col { display: flex; flex-direction: column; gap: 14px; min-width: 0; }
.col :deep(.el-card + .el-card) { margin-top: 0; }   /* styles.css 里有 .el-card+.el-card 的 16px 上边距，这里用 gap 代替 */

.canvas-wrap { overflow: auto; max-height: 68vh; border: 1px solid var(--border); border-radius: 8px; background: #0b0f18; }
canvas { display: block; cursor: crosshair; }
.swatch { display: inline-block; width: 14px; height: 10px; border-radius: 2px; margin-right: 6px; }
.swatch.free { background: rgb(122,132,148); }
.swatch.block { background: rgb(41,49,60); }

.detail-card :deep(.el-card__body) { max-height: 76vh; overflow: auto; }
.kv-table { width: 100%; }
.kv-table td { padding: 3px 6px; border: none; }
.kv-table td :deep(.el-tag) { margin-right: 4px; }
.bars { margin: 8px 0 4px; }
.bar-row { display: flex; align-items: center; gap: 8px; margin-bottom: 4px; }
.bar-row .lab { width: 26px; color: var(--muted); font-size: 12px; }
.bar { flex: 1; height: 8px; background: var(--bg); border: 1px solid var(--border); border-radius: 6px; overflow: hidden; }
.fill { height: 100%; }
.fill.hp { background: linear-gradient(90deg, #3ecf8e, #2f9f6d); }
.fill.mp { background: linear-gradient(90deg, #4c8dff, #2f6fd0); }
.sec { display: flex; align-items: center; gap: 8px; margin: 12px 0 6px; color: var(--text-dim); font-size: 13px; font-weight: 600; border-bottom: 1px solid var(--border); padding-bottom: 4px; }
table.mini th, table.mini td { padding: 3px 6px; font-size: 12px; }
.bag { display: flex; flex-wrap: wrap; gap: 6px; }
.bag-item { background: var(--panel-2); border: 1px solid var(--border); border-radius: 6px; padding: 3px 8px; font-size: 12px; }
.bag-item .bcount { color: var(--accent); margin-left: 4px; }

/* 2026-09-22 选中号日志面板：固定高度 + 细滚动条 + 稳定 key/v-memo（旧行不再逐批重渲染） */
.rlog { height: 240px; overflow-y: auto; border: 1px solid var(--border); border-radius: 6px;
        background: var(--panel-2); padding: 6px 8px; }
.rlog-line { display: flex; gap: 8px; font-size: 12px; line-height: 1.5; }
.rlog-t { color: var(--muted, #888); flex: 0 0 auto; }
.rlog-m { font-family: ui-monospace, Consolas, monospace; word-break: break-all; }
.rlog-line.warn .rlog-m { color: var(--warn); }
.rlog-line.error .rlog-m { color: var(--danger); }

/* 2026-09-22 游荡入口 */
.roam-now { font-size: 12px; color: var(--ok); margin: 2px 0 4px; }
.roam-hint { font-size: 12px; line-height: 1.5; margin-top: 4px; }

/* ---------------- 批量游荡弹窗 ---------------- */
.bdg-params { padding-bottom: 10px; border-bottom: 1px solid var(--border); margin-bottom: 12px; }
.bdg-cols { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 392px); gap: 14px; align-items: start; }
@media (max-width: 1000px) { .bdg-cols { grid-template-columns: 1fr; } }
.bdg-col { min-width: 0; }
.bdg-head { display: flex; align-items: center; gap: 8px; margin-bottom: 6px; flex-wrap: wrap; }
.cand-table :deep(.el-table__cell) { padding: 3px 0; }
.bdg-pager { display: flex; justify-content: flex-end; margin-top: 8px; }
.map-pick { max-height: 330px; overflow-y: auto; border: 1px solid var(--border); border-radius: 8px;
            background: var(--panel-2); padding: 4px 8px; }
.map-pick-row { display: flex; align-items: center; gap: 8px; padding: 2px 0; font-size: 12px; }
.map-pick-row :deep(.el-checkbox__label) { padding-left: 6px; }
.mp-name { display: inline-block; min-width: 100px; font-size: 12px; }
.bdg-diff { display: flex; align-items: center; gap: 8px; margin-top: 8px; flex-wrap: wrap; }
</style>
