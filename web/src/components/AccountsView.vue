<script setup>
import { computed, h, onMounted, reactive, ref } from 'vue'
// 脚本式 API（ElMessage / ElMessageBox）必须显式 import；模板里的 <el-xxx> 是全局注册的
import { ElMessage, ElMessageBox } from 'element-plus'
import { apiGet, apiPost } from '../api'
import { state, post, zones, currentZoneKey } from '../store'

// MessageBox 的 message 用 VNode 才能保留 \n 换行（纯文本里的换行会被折叠）
function pre(text) { return h('div', { style: 'white-space: pre-line' }, text) }
// 破坏性/长说明类操作统一走 ElMessageBox（替换原来的 window.confirm）；点"取消"不算错误
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

// 弹窗开关（原来内嵌在页面里的表单搬进 el-dialog，纯 UI 状态）
const ui = reactive({ createOpen: false, addOpen: false })

// 项目色板的状态类（ok/warn/danger/info/dim）→ el-tag 的 type。纯 UI 映射，判据不变。
const TAG_TYPE = { ok: 'success', warn: 'warning', danger: 'danger', info: 'info', dim: 'info' }
function tagType(cls) { return TAG_TYPE[cls] || 'info' }

// el-table 的行 class 回调：在线行淡绿底（原来裸 table 上是 .row-on）
function poolRowClass({ row }) { return row.online ? 'row-on' : '' }

const PAGE_SIZES = [20, 50, 100]

const rows = ref([])
const meta = ref({})
const stats = ref({})
const pageInfo = ref({ page: 1, pageSize: 50, hasMore: false }) // 默认 50/页（可切 20/50/100）
const loading = ref(false)
const err = ref('')
const selected = ref(new Set())

const filter = reactive({ zone: '', keyword: '', usable: false, onlyOnline: false, includeLive: false, pool: '' })
const batch = reactive({ limit: 1, chunk: 1, interval_ms: 1500, only_usable: true })
const addForm = reactive({ text: '', password: '', zone: '' })

// 验证：默认「从本地库按当前区批量验证」；手输账号只是次要入口（同步验证单个/几个）
const verifyForm = reactive({
  text: '', password: '', zone: '', concurrency: 2, timeout_sec: 8, query_role: true, persist: true,
  // 默认「本区全部 + 不限个数」：点一下把本区账号全验一遍（0=不限；任务带进度、可随时停）
  scope: 'zone', limit: 0, skip_online: true,
})
const verify = reactive({
  running: false, summary: null, results: [], msg: '', game_addr: '', coding: '',
  job: null, jobId: '', polling: false,
})
const pending = ref({})

const SCOPES = [
  { value: 'zone', label: '本区全部（该区有记录的全部账号）' },
  { value: 'unverified', label: '待验证（该区有记录、还没验过）' },
  { value: 'unusable', label: '已知不可用（重验看是否恢复）' },
  { value: 'unknown', label: '该区无记录（探明有没有这个号）' },
  { value: 'all', label: '池内全部' },
]

const jobProgress = computed(() => {
  const j = verify.job
  if (!j || !j.total) return 0
  return Math.min(100, Math.round((j.done / j.total) * 100))
})
const jobRunning = computed(() => verify.job?.status === 'running')

const zoneList = computed(() => zones())

function zoneOptions() {
  return zoneList.value.map((z) => ({ value: z.addr, label: `${z.name}（${z.addr}）`, key: z.key }))
}

// 当前区（默认跟随中控当前区）
function currentAddr() {
  const cur = zoneList.value.find((z) => z.key === currentZoneKey())
  return cur ? cur.addr : ''
}
function targetZone() {
  return filter.zone || currentAddr()
}

async function load(resetPage = false) {
  if (resetPage) pageInfo.value.page = 1
  loading.value = true
  try {
    const q = new URLSearchParams()
    if (filter.zone) q.set('zone', filter.zone)
    if (filter.usable) q.set('usable', '1')
    if (filter.onlyOnline) q.set('online', '1')
    if (!filter.includeLive) q.set('include_live', '0') // 默认只看池内（池外在线号要显式打开）
    if (filter.keyword) q.set('keyword', filter.keyword)
    if (filter.pool) q.set('pool', filter.pool) // 号池分区：newbie 新手池 / ghost 抓鬼池
    q.set('limit', String(pageInfo.value.pageSize))
    q.set('offset', String((pageInfo.value.page - 1) * pageInfo.value.pageSize))
    const d = await apiGet('/api/accounts?' + q.toString())
    rows.value = d.accounts || []
    stats.value = d.stats || {}
    meta.value = d.meta || {}
    pageInfo.value.hasMore = rows.value.length >= pageInfo.value.pageSize
    err.value = ''
  } catch (e) {
    err.value = e.message
  } finally {
    loading.value = false
  }
}
onMounted(() => {
  verifyForm.zone = currentAddr()
  addForm.zone = currentAddr()
  load()
  loadPending()
})

function gotoPage(delta) {
  const next = pageInfo.value.page + delta
  if (next < 1) return
  if (delta > 0 && !pageInfo.value.hasMore) return
  pageInfo.value.page = next
  load()
}

function toggle(name) {
  const s = new Set(selected.value)
  if (s.has(name)) s.delete(name)
  else s.add(name)
  selected.value = s
}
// 本页勾选状态（跨页选择保留，所以判断"全选"必须只看本页行）
const allPageSelected = computed(() => rows.value.length > 0 && rows.value.every((r) => selected.value.has(r.name)))
const somePageSelected = computed(() => !allPageSelected.value && rows.value.some((r) => selected.value.has(r.name)))
// 表头全选：只增删"本页这一批"，别把别的页已勾的号一起清掉
function toggleAll() {
  const s = new Set(selected.value)
  if (allPageSelected.value) rows.value.forEach((r) => s.delete(r.name))
  else rows.value.forEach((r) => s.add(r.name))
  selected.value = s
}
const selectedNames = computed(() => Array.from(selected.value))

// ---------------- 可用性验证（直连游戏服跑登录协议）----------------

// 待验证数量预览（面板显示"本次将验 N 个"）
// 必须跟**验证区**（不是检索区）：否则会拿别的区的数字误导用户
async function loadPending() {
  const zone = verifyForm.zone || currentAddr()
  try {
    const d = await apiGet('/api/accounts/stats?zone=' + encodeURIComponent(zone || ''))
    pending.value = d.pending || {}
  } catch (e) { /* 预览拿不到不影响主流程 */ }
}

// 默认动作：从本地库按当前区选号 → 后台批量验证 → 结果逐条同步回账号池
async function startBatchVerify() {
  const zone = verifyForm.zone || targetZone()
  if (!zone) { ElMessage.warning('还没有可用的区：先在「系统信息 → 区管理」配置一个区'); return }
  if (jobRunning.value) return
  // 预计个数与耗时（并发越高越快；每个号大约 0.5~1 秒）
  const planned = verifyForm.limit > 0
    ? Math.min(verifyForm.limit, pending.value.zone ?? verifyForm.limit)
    : (pending.value.zone || 0)
  const mins = planned ? Math.max(1, Math.round((planned * 0.7) / Math.max(1, verifyForm.concurrency) / 60)) : 0
  const tip = `开始批量验证【${zone}】\n范围：${SCOPES.find((s) => s.value === verifyForm.scope)?.label}\n`
    + (verifyForm.limit > 0 ? `本次最多 ${verifyForm.limit} 个` : `本次全部${planned ? `（约 ${planned} 个）` : ''}`)
    + `，并发 ${verifyForm.concurrency}`
    + (mins ? `，约 ${mins} 分钟` : '')
    + (verifyForm.skip_online ? '，跳过正在线的号' : '') + '\n\n'
    + '每个账号会真连一次游戏服（不进入游戏），结果实时同步回账号池；跑着的时候可以点「停止」。'
  if (!await confirmBox(tip, '开始批量验证', '开始验证')) return
  try {
    const res = await apiPost('/api/accounts/verify', {
      zone,
      scope: verifyForm.scope,
      limit: verifyForm.limit,
      concurrency: verifyForm.concurrency,
      timeout_sec: verifyForm.timeout_sec,
      query_role: verifyForm.query_role,
      persist: verifyForm.persist,
      skip_online: verifyForm.skip_online,
    })
    if (!res.ok) {
      verify.msg = res.msg || '批量验证没跑起来'
      ElMessage.warning(verify.msg)
      loadPending()
      return
    }
    verify.jobId = res.job?.id || ''
    verify.job = res.job
    verify.msg = res.msg || ''
    verify.game_addr = res.game_addr || ''
    verify.coding = res.coding || ''
    verify.results = []
    verify.summary = null
    ElMessage.success(res.msg || '批量验证已开始')
    pollJob()
  } catch (e) {
    ElMessage.error(e.message)
    verify.msg = e.message
  }
}

// 轮询任务进度（700ms 一次；结束后刷新列表与预览）
function pollJob() {
  if (verify.polling) return
  verify.polling = true
  const tick = async () => {
    if (!verify.jobId) { verify.polling = false; return }
    try {
      const d = await apiGet('/api/accounts/verify/job?id=' + encodeURIComponent(verify.jobId))
      if (d.ok && d.job) {
        verify.job = d.job
        if (d.job.status === 'running') { setTimeout(tick, 700); return }
      }
    } catch (e) { /* 网络抖动：继续轮询 */ }
    verify.polling = false
    const j = verify.job
    if (j) {
      const head = j.status === 'canceled' ? '批量验证已停止' : '批量验证完成'
      const text = `${head}：可用 ${j.usable} · 不可用 ${j.unusable} · 不存在 ${j.not_exists}`
        + (j.error ? ` · 失败 ${j.error}` : '') + `（${j.done}/${j.total}）`
      if (j.status === 'canceled') ElMessage.warning(text)
      else ElMessage.success(text)
    }
    load(true)
    loadPending()
  }
  tick()
}

async function stopBatchVerify() {
  if (!verify.jobId) return
  try {
    const res = await apiPost('/api/accounts/verify/cancel', { id: verify.jobId })
    if (res.job) verify.job = res.job
    if (res.ok) ElMessage.warning(res.msg || '已请求停止')
    else ElMessage.error(res.msg || '停止失败')
  } catch (e) {
    ElMessage.error(e.message)
  }
}

// 次要入口：手输账号，同步验证这几个（结果同样写回池）
async function runVerify() {
  const tokens = verifyForm.text.split(/[,，;\s\n]+/).filter(Boolean)
  if (!tokens.length) { ElMessage.warning('请输入要验证的账号（每行一个，可写 账号:密码）'); return }
  if (tokens.length > 20) {
    if (!await confirmBox(`要一次验证 ${tokens.length} 个账号吗？\n建议一次 1 个：每个账号都会真连一次游戏服。`, '确认批量验证', '验证')) return
  }
  const accounts = tokens.map((t) => {
    const i = t.indexOf(':')
    return i > 0 ? [t.slice(0, i), t.slice(i + 1)] : t
  })
  verify.running = true
  try {
    const res = await apiPost('/api/accounts/verify', {
      accounts,
      zone: verifyForm.zone || undefined,
      password: verifyForm.password || undefined,
      concurrency: verifyForm.concurrency,
      timeout_sec: verifyForm.timeout_sec,
      query_role: verifyForm.query_role,
      persist: verifyForm.persist,
    })
    if (!res.ok) {
      verify.msg = res.msg || '验证失败'
      ElMessage.warning(verify.msg)
      return
    }
    verify.summary = res.summary || {}
    verify.results = res.results || []
    verify.msg = res.msg || ''
    verify.game_addr = res.game_addr || ''
    verify.coding = res.coding || ''
    ElMessage.success(`${res.msg}（${res.game_addr}）`)
    load(true)
    loadPending()
  } catch (e) {
    ElMessage.error(e.message)
    verify.msg = e.message
  } finally {
    verify.running = false
  }
}

function verifyTagClass(r) {
  if (r.err) return 'warn'
  if (!r.exists) return 'danger'
  return r.usable ? 'ok' : 'warn'
}
function verifyText(r) {
  if (r.err) return '验证失败'
  if (!r.exists) return '不存在'
  return r.usable ? '可用' : '不可用'
}

// ---------------- 建号（注册协议 106→104→700；密码随机生成后写回库）----------------

const createForm = reactive({
  mode: 'number', // number=按编号（默认，像 py 中控那样）| list=手动指定
  text: '', zone: '',
  prefix: 'robot000', start: 3004, count: 5, suffix: '@xy3.com', pad: 0, auto_start: false,
  password_len: 16, batch_size: 10, concurrency: 2, timeout_sec: 12, agent_key: '',
})
const createState = reactive({ running: false, results: [], msg: '', created: 0 })

// 账号名预览（与后端 BuildNames 同一套规则：前缀 + 序号补零 + 后缀）
function buildName(i) {
  const start = Number(createForm.start) || 1
  const count = Math.max(1, Number(createForm.count) || 1)
  const w = Number(createForm.pad) > 0 ? Number(createForm.pad) : String(start + count - 1).length
  return `${createForm.prefix}${String(start + i).padStart(w, '0')}${createForm.suffix || ''}`
}
const createPreview = computed(() => {
  if (createForm.mode !== 'number') {
    const n = createForm.text.split(/[,，;\s\n]+/).filter(Boolean).length
    return { count: n, first: '', last: '' }
  }
  const count = Number(createForm.count) || 0
  if (count <= 0 || !createForm.prefix) return { count: 0, first: '', last: '' }
  return { count, first: buildName(0), last: buildName(count - 1) }
})

// 点一下复制（建号成功后要拿密码给机器人用）
async function copyText(text) {
  if (!text) return
  try {
    await navigator.clipboard.writeText(text)
    ElMessage.success('已复制：' + text)
  } catch (e) {
    // 剪贴板 API 要安全上下文（https/localhost）；失败就把内容摆出来让用户手抄
    ElMessage.error('复制失败，请手动选中：' + text)
  }
}

async function runCreate() {
  const zone = createForm.zone || targetZone()
  if (!zone) { ElMessage.warning('先选一个区'); return }
  const count = createForm.mode === 'number' ? Number(createForm.count) || 0 : createPreview.value.count
  if (count <= 0) { ElMessage.warning('数量要大于 0'); return }
  if (count > 200) { ElMessage.warning('一次最多建 200 个号（注册同 IP 过频会触发风控）'); return }
  const what = createForm.mode === 'number'
    ? `${createPreview.value.first} … ${createPreview.value.last}（共 ${count} 个）`
    : `指定的 ${count} 个账号`
  const tip = `建号：${what}\n区：${zone}\n密码：随机 ${createForm.password_len} 位，建号成功后写入账号库\n`
    + `分批：每批 ${createForm.batch_size} 个 · 并发 ${createForm.concurrency}\n`
    + '本项目没有统一密码：之后登录/验证都用库里的密码。'
  if (!await confirmBox(tip, '确认建号', '开始建号')) return
  createState.running = true
  try {
    const body = {
      zone,
      password_len: createForm.password_len,
      batch_size: createForm.batch_size,
      concurrency: createForm.concurrency,
      timeout_sec: createForm.timeout_sec,
      agent_key: createForm.agent_key || undefined,
      interval_ms: 0,
      batch_interval_ms: 400,
    }
    if (createForm.mode === 'number') {
      Object.assign(body, {
        prefix: createForm.prefix,
        start: Number(createForm.start) || 0,
        count,
        suffix: createForm.suffix || '',
        pad: Number(createForm.pad) || 0,
        auto_start: !!createForm.auto_start,
      })
    } else {
      body.accounts = createForm.text.split(/[,，;\s\n]+/).filter(Boolean)
    }
    const res = await apiPost('/api/accounts/create', body)
    createState.results = res.results || []
    createState.created = res.created || 0
    createState.msg = res.msg || ''
    if (res.ok) {
      ElMessage.success(res.msg || '建号完成')
      ui.createOpen = false // 关了弹窗才能在下面的卡片里看逐条结果
    } else {
      ElMessage.warning(res.msg || '建号失败（结果见卡片）')
    }
    load(true)
    loadPending()
  } catch (e) {
    ElMessage.error(e.message)
    createState.msg = e.message
  } finally {
    createState.running = false
  }
}

// 弹窗里的"建号"入口：先过表单校验（必填/数量），再走原来的 runCreate
const createFormRef = ref(null)
async function submitCreate() {
  try { await createFormRef.value?.validate() } catch (e) { return }
  runCreate()
}
const createRules = computed(() => ({
  prefix: createForm.mode === 'number' ? [{ required: true, message: '前缀不能为空', trigger: 'blur' }] : [],
  count: createForm.mode === 'number'
    ? [{ required: true, message: '数量要大于 0', trigger: 'blur' }] : [],
  text: createForm.mode === 'list' ? [{ required: true, message: '请输入要建的账号', trigger: 'blur' }] : [],
}))

// 建号结果状态（先验证再注册：新建 / 已存在可用 / 密码不符 / 失败）
const CREATE_STATUS = {
  created: { label: '新建成功', cls: 'ok' },
  existing: { label: '已存在可用', cls: 'ok' },
  password_mismatch: { label: '密码不符', cls: 'warn' },
  failed: { label: '失败', cls: 'danger' },
}
function createStatusLabel(s) { return (CREATE_STATUS[s] || {}).label || s || '--' }
function createStatusClass(s) { return (CREATE_STATUS[s] || {}).cls || 'dim' }

// ---------------- 上线 / 下线：**点选即可，不用输账号** ----------------
// 单个：行内「上线/下线」点一下就走；批量：勾选（或快捷选择）后点批量按钮。

const rowBusy = ref('')     // 正在上下线的账号：行内显示"上线中…"
const batchBusy = ref(false)

async function toggleOnline(r) {
  if (rowBusy.value || batchBusy.value) return
  const up = !r.online
  rowBusy.value = r.name
  try {
    const res = await post('/api/robots/batch', {
      action: up ? 'online' : 'offline',
      accounts: [r.name],
      zone: targetZone(),
      chunk: 1, interval_ms: 0,
    })
    // 成功提示由 post() 统一弹（res.msg），这里只兜失败
    if (res && res.ok === false) ElMessage.error(res.msg || (up ? '上线失败' : '下线失败'))
    load()
  } finally {
    rowBusy.value = ''
  }
}

// 快捷选择（都是"替换选择"，结果可预期；手动逐行勾选仍跨页保留）
function selectPage(kind) {
  if (kind === 'none') { selected.value = new Set(); return }
  const pick = rows.value.filter((r) => {
    if (kind === 'usable') return !!r.usable
    if (kind === 'offline') return !r.online
    if (kind === 'online') return !!r.online
    return true
  })
  selected.value = new Set(pick.map((r) => r.name))
}

// 批量上线：auto=true 时不看勾选，让服务端从池里挑可用号（限 batch.limit 个）
async function batchOnline(auto = false) {
  const names = auto ? [] : selectedNames.value
  if (!auto && !names.length) { ElMessage.warning('先勾选账号，或用「自动挑号」上线'); return }
  const tip = auto
    ? `从池里自动挑最多 ${batch.limit} 个【${targetZone() || '当前区'}】可用号上线？`
    : `上线勾选的 ${names.length} 个账号？\n${names.slice(0, 5).join('\n')}${names.length > 5 ? `\n… 等 ${names.length} 个` : ''}`
  if (!await confirmBox(`${tip}\n分批：每批 ${batch.chunk} 个，间隔 ${batch.interval_ms}ms`, '确认上线', '上线')) return
  batchBusy.value = true
  try {
    const body = {
      action: 'online', zone: targetZone(),
      limit: batch.limit, chunk: batch.chunk, interval_ms: batch.interval_ms,
      only_usable: batch.only_usable,
    }
    if (names.length) body.accounts = names
    const res = await post('/api/robots/batch', body)
    if (res && res.ok) {
      // 跳过数是服务端算的（post() 的通用提示里没有），这里补一条更有信息量的反馈
      const skipped = (res.skipped_online || []).length + (res.skipped_no_password || []).length
      if (skipped) ElMessage.info(`跳过 ${skipped} 个（已在线 / 没密码）`)
      if (!auto) selected.value = new Set()
      load()
    } else if (res) {
      ElMessage.error(res.msg || '批量上线失败')
    }
  } finally {
    batchBusy.value = false
  }
}

async function batchOffline() {
  const names = selectedNames.value
  if (!names.length) { ElMessage.warning('请先勾选要下线的账号'); return }
  if (!await confirmBox(`下线的 ${names.length} 个账号？`, '确认下线', '下线')) return
  const res = await post('/api/robots/batch', {
    action: 'offline', accounts: names, chunk: batch.chunk, interval_ms: batch.interval_ms,
  })
  if (res.ok) load()
}

async function addAccounts() {
  const names = addForm.text.split(/[,，;\s\n]+/).filter(Boolean)
  if (!names.length) { ElMessage.warning('请输入账号（逗号/换行分隔，可写 账号:密码）'); return }
  const res = await post('/api/accounts/add', { password: addForm.password, zone: addForm.zone, accounts: names })
  if (res.ok) { addForm.text = ''; ui.addOpen = false; load(true) }
}

// 弹窗里的"加入池"入口：先过表单校验，再走原来的 addAccounts
const addFormRef = ref(null)
async function submitAdd() {
  try { await addFormRef.value?.validate() } catch (e) { return }
  addAccounts()
}

async function removeAccounts() {
  const names = selectedNames.value
  if (!names.length) { ElMessage.warning('请先勾选账号'); return }
  if (!await confirmBox(`从池里删除 ${names.length} 个账号？（游戏服上的账号不受影响）`, '确认删除', '从池删除')) return
  const res = await post('/api/accounts/remove', { accounts: names })
  if (res.ok) { selected.value = new Set(); load() }
}

function fmtTime(ts) {
  if (!ts) return '--'
  return new Date(ts * 1000).toLocaleString('zh-CN', { hour12: false })
}
const headline = computed(() => {
  const inPool = meta.value.count ?? 0
  const usable = stats.value.usable ?? 0
  if (!inPool) return '账号池是空的：先在下面把账号加进来，再做可用性验证'
  return `池内 ${inPool} 个账号，该区可用 ${usable} 个`
})
</script>

<template>
  <!-- 第一行统计卡：信息与原来一致，改成 el-card + 图标 + 大字号数字 -->
  <div class="grid cols-4 stat-grid">
    <el-card class="stat-card" shadow="never">
      <div class="label"><el-icon><Collection /></el-icon>池内账号</div>
      <div class="value">{{ meta.count ?? 0 }}</div>
      <div class="sub">导入时间 {{ fmtTime(meta.generated_at) }}</div>
    </el-card>
    <el-card class="stat-card" shadow="never">
      <div class="label"><el-icon><CircleCheck /></el-icon>该区可用</div>
      <div class="value">{{ stats.usable ?? 0 }}<span class="unit">/{{ stats.total ?? 0 }}</span></div>
      <div class="sub">{{ filter.zone || '未选区（任意区可用）' }}</div>
    </el-card>
    <el-card class="stat-card" shadow="never">
      <div class="label"><el-icon><Monitor /></el-icon>当前在线</div>
      <div class="value">{{ state.status.counts.online }}<span class="unit">/{{ state.status.counts.total }}</span></div>
      <div class="sub">当前区 {{ state.status.current.key || '--' }}</div>
    </el-card>
    <el-card class="stat-card" shadow="never">
      <div class="label"><el-icon><Select /></el-icon>已勾选</div>
      <div class="value">{{ selectedNames.length }}</div>
      <div class="sub">批量上线/下线/删号用</div>
    </el-card>
  </div>

  <el-card class="panel-card" shadow="never">
    <div class="toolbar">
      <span class="headline">{{ headline }}</span>
      <span class="spacer" />
      <el-select v-model="filter.zone" size="small" style="width: 240px" placeholder="区：任意（不过滤）"
                 @change="load(true); loadPending()">
        <el-option value="" label="区：任意（不过滤）" />
        <el-option v-for="z in zoneOptions()" :key="z.value" :value="z.value" :label="`区：${z.label}`" />
      </el-select>
      <el-input v-model="filter.keyword" size="small" style="width: 200px" placeholder="检索：账号 / 角色名"
                clearable @keyup.enter="load(true)">
        <template #prefix><el-icon><Search /></el-icon></template>
      </el-input>
      <el-button type="primary" size="small" @click="load(true)">检索</el-button>
      <el-checkbox v-model="filter.usable" size="small" @change="load(true)">只看可用</el-checkbox>
      <el-checkbox v-model="filter.onlyOnline" size="small" @change="load(true)">只看在线</el-checkbox>
      <el-checkbox v-model="filter.includeLive" size="small" title="把池里没有但当前在线的号也列出来"
                   @change="load(true)">含池外在线</el-checkbox>
      <el-select v-model="filter.pool" size="small" style="width: 176px" @change="load(true)"
                 placeholder="池分区：全部"
                 title="号池分区：新手池 = 未毕业；抓鬼池 = 已毕业（≥31 级或链完成）">
        <el-option value="" label="池分区：全部" />
        <el-option value="newbie" label="新手池（未毕业）" />
        <el-option value="ghost" label="抓鬼池（已毕业）" />
        <el-option value="unknown" label="未知（等级未上报）" />
      </el-select>
      <span class="spacer" />
      <el-button size="small" type="primary" plain @click="ui.addOpen = true">
        <el-icon><Plus /></el-icon>加号入池
      </el-button>
      <el-button size="small" type="primary" @click="ui.createOpen = true">
        <el-icon><Plus /></el-icon>建号 / 校验
      </el-button>
    </div>
  </el-card>

  <el-card class="panel-card" shadow="never">
    <template #header>
      <div class="card-head">
        <span class="card-title">可用性验证（直连游戏服跑登录协议 102，不进入游戏、无副作用）</span>
      </div>
    </template>

    <!-- 默认动作：从本地库按当前区批量验证，结果逐条同步回账号池 -->
    <div class="toolbar" style="margin-bottom: 8px">
      <el-select v-model="verifyForm.zone" size="small" style="width: 220px" placeholder="验证区：跟随当前区"
                 @change="loadPending()">
        <el-option value="" label="验证区：跟随当前区" />
        <el-option v-for="z in zoneOptions()" :key="z.value" :value="z.value" :label="z.label" />
      </el-select>
      <el-select v-model="verifyForm.scope" size="small" style="width: 260px">
        <el-option v-for="s in SCOPES" :key="s.value" :value="s.value" :label="`范围：${s.label}`" />
      </el-select>
      <span class="k">本次</span>
      <el-input-number v-model="verifyForm.limit" size="small" :min="0" :max="100000" :controls="false"
                       style="width: 96px" title="0 = 不限（验完范围内全部）" />
      <span v-if="!verifyForm.limit" class="muted">全部 ≈ {{ pending.zone ?? 0 }} 个</span>
      <span class="k">并发</span>
      <el-input-number v-model="verifyForm.concurrency" size="small" :min="1" :max="16" :controls="false"
                       style="width: 78px" title="同时探测几个账号（默认 2）" />
      <el-checkbox v-model="verifyForm.skip_online" size="small">跳过在线号</el-checkbox>
      <el-button type="primary" size="small" :disabled="jobRunning" :loading="jobRunning" @click="startBatchVerify">
        {{ jobRunning ? '批量验证中…' : '批量验证当前区' }}
      </el-button>
      <el-button type="danger" size="small" plain :disabled="!jobRunning" @click="stopBatchVerify">停止</el-button>
    </div>

    <div class="toolbar">
      <el-tag size="small" effect="plain" disable-transitions>本区全部 <b>{{ pending.zone ?? 0 }}</b></el-tag>
      <el-tag size="small" effect="plain" disable-transitions>待验证 <b>{{ pending.unverified ?? 0 }}</b></el-tag>
      <el-tag size="small" effect="plain" disable-transitions>已知不可用 <b>{{ pending.unusable ?? 0 }}</b></el-tag>
      <el-tag size="small" effect="plain" disable-transitions>该区无记录 <b>{{ pending.unknown ?? 0 }}</b></el-tag>
      <el-tag size="small" effect="plain" disable-transitions>池内共 <b>{{ pending.all ?? 0 }}</b></el-tag>
      <span class="spacer" />
      <el-checkbox v-model="verifyForm.query_role" size="small">查角色名/等级</el-checkbox>
      <el-checkbox v-model="verifyForm.persist" size="small">结果写回池（同步）</el-checkbox>
      <span class="k">超时(秒)</span>
      <el-input-number v-model="verifyForm.timeout_sec" size="small" :min="1" :max="120" :controls="false"
                       style="width: 78px" />
    </div>

    <!-- 批量进度：el-progress 的 status 跟着任务状态走（进行中/已停止/已完成） -->
    <div v-if="verify.job" class="verify-job">
      <div class="toolbar" style="margin-bottom: 6px">
        <el-tag size="small" disable-transitions
                :type="jobRunning ? 'info' : (verify.job.status === 'canceled' ? 'warning' : 'success')">
          {{ jobRunning ? '进行中' : (verify.job.status === 'canceled' ? '已停止' : '已完成') }}
        </el-tag>
        <span class="muted">
          {{ verify.job.done }}/{{ verify.job.total }} · 可用 {{ verify.job.usable }} · 不可用
          {{ verify.job.unusable }} · 不存在 {{ verify.job.not_exists }}
          <template v-if="verify.job.error"> · 失败 {{ verify.job.error }}</template>
          · {{ ((verify.job.elapsed_ms || 0) / 1000).toFixed(1) }}s
        </span>
        <span class="spacer" />
        <span class="muted mono">{{ verify.job.zone }}</span>
      </div>
      <el-progress :percentage="jobProgress" :stroke-width="12" :show-text="false"
                   :status="jobRunning ? undefined : (verify.job.status === 'canceled' ? 'warning' : 'success')" />
    </div>

    <div v-if="verify.msg" class="toolbar" style="margin-top: 10px">
      <el-tag size="small" disable-transitions
              :type="(verify.summary?.error || verify.job?.error) ? 'warning' : 'success'">{{ verify.msg }}</el-tag>
      <span class="muted mono">{{ verify.game_addr }} · {{ verify.coding }}</span>
    </div>

    <!-- 次要入口：手输账号（同步验证这几个） -->
    <div class="toolbar" style="margin-top: 12px">
      <span class="muted">手输账号单独验（密码从库里取）：</span>
      <el-input v-model="verifyForm.text" size="small" style="flex:1; min-width: 260px"
                placeholder="账号（每行/逗号分隔，可写 账号:密码）；默认 1 个就够" @keyup.enter="runVerify" />
      <el-button size="small" :disabled="verify.running" :loading="verify.running" @click="runVerify">
        {{ verify.running ? '验证中…' : '立即验证' }}
      </el-button>
    </div>

    <el-table v-if="verify.results.length" :data="verify.results" size="small" max-height="320"
              row-key="account" style="margin-top: 8px">
      <el-table-column label="账号" min-width="170" class-name="mono" show-overflow-tooltip>
        <template #default="{ row: r }">{{ r.account }}</template>
      </el-table-column>
      <el-table-column label="结论" width="94">
        <template #default="{ row: r }">
          <el-tag size="small" disable-transitions :type="tagType(verifyTagClass(r))">{{ verifyText(r) }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="服务端码" width="96" class-name="mono">
        <template #default="{ row: r }">{{ r.ret_code }}</template>
      </el-table-column>
      <el-table-column label="角色" min-width="120" show-overflow-tooltip>
        <template #default="{ row: r }">{{ r.role_name || '--' }}</template>
      </el-table-column>
      <el-table-column label="等级" width="70">
        <template #default="{ row: r }">{{ r.level || '--' }}</template>
      </el-table-column>
      <el-table-column label="耗时" width="90">
        <template #default="{ row: r }"><span class="muted">{{ r.elapsed_ms }}ms</span></template>
      </el-table-column>
      <el-table-column label="说明" min-width="200" show-overflow-tooltip>
        <template #default="{ row: r }"><span class="muted">{{ r.err || r.msg }}</span></template>
      </el-table-column>
    </el-table>
  </el-card>

  <el-card class="panel-card" shadow="never">
    <template #header>
      <div class="card-head">
        <span class="card-title">建号 / 校验账号（注册协议 106 → 104 → 700）</span>
        <span class="spacer" />
        <el-button type="primary" size="small" @click="ui.createOpen = true">
          <el-icon><Plus /></el-icon>打开建号表单
        </el-button>
      </div>
    </template>
    <div class="toolbar">
      <span class="muted">先验证再注册：已存在且密码对 → 跳过（标可用）；不存在 → 建号；密码不符 → 提示改密码</span>
      <span class="spacer" />
      <span class="muted">密码每号随机、按服通用、存账号库（登录只用库里的密码）</span>
    </div>
    <div v-if="createState.msg" class="toolbar" style="margin-top: 8px">
      <el-tag size="small" disable-transitions :type="createState.created ? 'success' : 'warning'">{{ createState.msg }}</el-tag>
    </div>
    <el-table v-if="createState.results.length" :data="createState.results" size="small" max-height="320"
              row-key="account" style="margin-top: 8px">
      <el-table-column label="账号" min-width="170" class-name="mono" show-overflow-tooltip>
        <template #default="{ row: r }">{{ r.account }}</template>
      </el-table-column>
      <el-table-column label="结果" width="112">
        <template #default="{ row: r }">
          <el-tag size="small" disable-transitions :type="tagType(createStatusClass(r.status))">
            {{ createStatusLabel(r.status) }}
          </el-tag>
        </template>
      </el-table-column>
      <el-table-column label="探测码" width="92" class-name="mono">
        <template #default="{ row: r }">{{ r.ret_code }}</template>
      </el-table-column>
      <el-table-column label="errid" width="90" class-name="mono">
        <template #default="{ row: r }">{{ r.err_id || '--' }}</template>
      </el-table-column>
      <el-table-column label="密码（已入库）" min-width="180">
        <template #default="{ row: r }">
          <el-tooltip :disabled="!r.password" content="点击复制" placement="top">
            <span class="mono copyable" @click="copyText(r.password)">{{ r.password ? r.password + ' ⧉' : '--' }}</span>
          </el-tooltip>
        </template>
      </el-table-column>
      <el-table-column label="角色" min-width="130" show-overflow-tooltip>
        <template #default="{ row: r }">{{ r.role_name ? `${r.role_name} Lv${r.level || 0}` : '--' }}</template>
      </el-table-column>
      <el-table-column label="说明" min-width="200" show-overflow-tooltip>
        <template #default="{ row: r }"><span class="muted">{{ r.err || r.msg }}</span></template>
      </el-table-column>
    </el-table>
  </el-card>

  <el-card v-if="err" class="panel-card" shadow="never">
    <el-tag size="small" type="danger" disable-transitions>{{ err }}</el-tag>
  </el-card>

  <!-- 上线/下线放在表格**上面**：勾完就能点，不用往下滚 -->
  <el-card class="panel-card" shadow="never">
    <template #header>
      <div class="card-head">
        <span class="card-title">上线 / 下线</span>
        <span class="muted">点行内「上线」立刻拉起；批量就勾选（可跨页），不用输账号</span>
        <span class="spacer" />
        <span class="muted">已选</span><b>{{ selectedNames.length }}</b>
        <el-button type="primary" size="small" :disabled="!selectedNames.length" :loading="batchBusy"
                   @click="batchOnline(false)">
          {{ batchBusy ? '下发中…' : `上线勾选的 ${selectedNames.length} 个` }}
        </el-button>
        <el-button size="small" :disabled="!selectedNames.length || batchBusy" @click="batchOffline">下线勾选的</el-button>
        <el-button type="danger" size="small" plain :disabled="!selectedNames.length" @click="removeAccounts">从池删除</el-button>
      </div>
    </template>
    <div class="toolbar">
      <span class="muted">快捷选择</span>
      <el-button size="small" @click="selectPage('all')">全选本页</el-button>
      <el-button size="small" @click="selectPage('usable')">选本页可用</el-button>
      <el-button size="small" @click="selectPage('offline')">选本页未上线</el-button>
      <el-button size="small" @click="selectPage('online')">选本页在线</el-button>
      <el-button size="small" @click="selectPage('none')">清空</el-button>
      <span class="spacer" />
      <span class="k">自动挑号上限</span>
      <el-input-number v-model="batch.limit" size="small" :min="1" :max="500" :controls="false"
                       style="width: 78px" title="不勾选时自动挑号的个数（默认 1：一次一个）" />
      <el-button size="small" :disabled="batchBusy" @click="batchOnline(true)">自动挑 {{ batch.limit }} 个可用号上线</el-button>
    </div>
    <div class="toolbar muted" style="margin-top: 8px">
      <span class="k">每批</span>
      <el-input-number v-model="batch.chunk" size="small" :min="1" :max="200" :controls="false" style="width: 78px" />
      <span class="k">批间隔(ms)</span>
      <el-input-number v-model="batch.interval_ms" size="small" :min="0" :max="60000" :controls="false" style="width: 100px" />
      <el-checkbox v-model="batch.only_usable" size="small">只选可用号</el-checkbox>
      <span class="spacer" />
      <span>默认「一次一个」（上限/每批都是 1）——想放量再往上调；已在线的会被跳过</span>
    </div>
  </el-card>

  <el-card class="panel-card" shadow="never">
    <template #header>
      <div class="card-head">
        <span class="card-title">账号池</span>
        <span class="muted">第 {{ pageInfo.page }} 页 · 本页 {{ rows.length }} 行{{ loading ? ' · 加载中…' : '' }}</span>
        <span class="spacer" />
        <span class="muted">密码不下发到前端；批量上线由服务端从池里取密码</span>
      </div>
    </template>
    <el-table v-loading="loading" :data="rows" class="pool-table" size="small" stripe height="560"
              row-key="name" :row-class-name="poolRowClass">
      <el-table-column width="46">
        <template #header>
          <el-checkbox :model-value="allPageSelected" :indeterminate="somePageSelected"
                       :disabled="!rows.length" @change="toggleAll" />
        </template>
        <template #default="{ row: r }">
          <el-checkbox :model-value="selected.has(r.name)" @change="toggle(r.name)" />
        </template>
      </el-table-column>
      <el-table-column label="账号" min-width="170" class-name="mono" show-overflow-tooltip>
        <template #default="{ row: r }">{{ r.name }}</template>
      </el-table-column>
      <el-table-column label="池" width="66">
        <template #default="{ row: r }">
          <el-tag v-if="r.pool === 'newbie'" size="small" type="info" effect="light" disable-transitions
                  title="新手池：未毕业（等级<31 且未链完成）">新手</el-tag>
          <el-tag v-else-if="r.pool === 'ghost'" size="small" type="warning" effect="light" disable-transitions
                  title="抓鬼池：已毕业（≥31 级 或 链完成）">抓鬼</el-tag>
          <el-tag v-else size="small" type="info" effect="plain" disable-transitions title="等级未上报/无记录">未知</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="等级" width="70">
        <template #default="{ row: r }">{{ r.level || '--' }}</template>
      </el-table-column>
      <el-table-column label="角色" min-width="120" show-overflow-tooltip>
        <template #default="{ row: r }">{{ r.role_name || '--' }}</template>
      </el-table-column>
      <el-table-column label="该区可用" width="100">
        <template #default="{ row: r }">
          <el-tag size="small" disable-transitions
                  :type="r.usable ? 'success' : (r.verified ? 'danger' : 'info')"
                  :effect="r.usable || r.verified ? 'light' : 'plain'">
            {{ r.usable ? '可用' : (r.verified ? '不可用' : '未验证') }}
          </el-tag>
        </template>
      </el-table-column>
      <el-table-column label="验证信息" min-width="200" show-overflow-tooltip>
        <template #default="{ row: r }"><span class="muted">{{ r.verify_msg || '--' }}</span></template>
      </el-table-column>
      <el-table-column label="验证时间" width="160">
        <template #default="{ row: r }">
          <span class="muted" :title="r.verified_at ? fmtTime(r.verified_at) : '从未验证'">
            {{ r.verified_at ? fmtTime(r.verified_at) : '--' }}
          </span>
        </template>
      </el-table-column>
      <el-table-column label="在线" width="80">
        <template #default="{ row: r }">
          <el-tag size="small" disable-transitions :type="r.online ? 'success' : 'info'"
                  :effect="r.online ? 'light' : 'plain'">{{ r.online ? '在线' : '离线' }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="状态" width="104">
        <template #default="{ row: r }">
          <el-tag size="small" type="info" effect="plain" disable-transitions>{{ r.state || '--' }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="运行时区" width="150" class-name="mono" show-overflow-tooltip>
        <template #default="{ row: r }"><span class="muted">{{ r.runtime_zone || r.zone || '--' }}</span></template>
      </el-table-column>
      <el-table-column label="密码" width="80">
        <template #default="{ row: r }">
          <el-tag size="small" disable-transitions :type="r.has_password ? 'info' : 'warning'"
                  :effect="r.has_password ? 'plain' : 'light'">{{ r.has_password ? '已设' : '未设' }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="备注" min-width="140" show-overflow-tooltip>
        <template #default="{ row: r }"><span class="muted">{{ r.note || '--' }}</span></template>
      </el-table-column>
      <!-- 固定在最右：14 列在窄屏要横向滚动，固定列保证「上线/下线」随时点得到 -->
      <el-table-column label="操作" width="92" fixed="right">
        <template #default="{ row: r }">
          <el-button link size="small" :type="r.online ? undefined : 'primary'" :disabled="!!rowBusy"
                     @click="toggleOnline(r)">
            {{ rowBusy === r.name ? (r.online ? '下线中…' : '上线中…') : (r.online ? '下线' : '上线') }}
          </el-button>
        </template>
      </el-table-column>
      <template #empty>
        <el-empty :image-size="60" description="这一页没有数据">
          <div class="muted small">
            <template v-if="!meta.count">先点「加号入池」把账号加进来，或「建号」批量注册。</template>
            <template v-else>换个关键词/筛选，或翻回上一页。</template>
          </div>
        </el-empty>
      </template>
    </el-table>

    <!-- 分页：接口是 offset+limit+hasMore（没有过滤后的总数），所以用"上一页/下一页"+每页条数 -->
    <div class="pager">
      <span class="muted">
        第 {{ pageInfo.page }} 页 · 本页 {{ rows.length }} 行
        <template v-if="stats.total">· 池内共 {{ stats.total }} 个（该区可用 {{ stats.usable ?? 0 }}）</template>
      </span>
      <span class="spacer" />
      <span class="k">每页</span>
      <el-select v-model="pageInfo.pageSize" size="small" style="width: 96px" @change="load(true)">
        <el-option v-for="n in PAGE_SIZES" :key="n" :value="n" :label="`${n} 行`" />
      </el-select>
      <el-button-group>
        <el-button size="small" :disabled="pageInfo.page <= 1" @click="gotoPage(-1)">
          <el-icon><ArrowLeft /></el-icon>上一页
        </el-button>
        <el-button size="small" :disabled="!pageInfo.hasMore" @click="gotoPage(1)">
          下一页<el-icon><ArrowRight /></el-icon>
        </el-button>
      </el-button-group>
    </div>
  </el-card>

  <!-- 加号入池：原来是页面底部的内嵌表单，改成弹窗（逻辑不变） -->
  <el-dialog v-model="ui.addOpen" title="加号入池" width="620px">
    <el-form ref="addFormRef" :model="addForm" label-width="88px" size="small">
      <el-form-item label="账号" prop="text" :rules="[{ required: true, message: '请输入账号（逗号/换行分隔）', trigger: 'blur' }]">
        <el-input v-model="addForm.text" type="textarea" :rows="4"
                  placeholder="账号，逗号/换行分隔（可写 账号:密码）" />
      </el-form-item>
      <el-form-item label="统一密码">
        <el-input v-model="addForm.password" placeholder="可空 = 默认" />
      </el-form-item>
      <el-form-item label="分配区">
        <el-select v-model="addForm.zone" placeholder="不指定" style="width: 100%">
          <el-option value="" label="不指定" />
          <el-option v-for="z in zoneOptions()" :key="z.value" :value="z.value" :label="z.label" />
        </el-select>
      </el-form-item>
    </el-form>
    <template #footer>
      <el-button size="small" @click="ui.addOpen = false">取消</el-button>
      <el-button type="primary" size="small" @click="submitAdd">加入池</el-button>
    </template>
  </el-dialog>

  <!-- 建号：原来是页面中部的内嵌表单，改成弹窗（分段用 el-radio-group） -->
  <el-dialog v-model="ui.createOpen" title="建号 / 校验账号（注册协议 106 → 104 → 700）" width="720px">
    <el-form ref="createFormRef" :model="createForm" :rules="createRules" label-width="88px" size="small">
      <el-form-item label="建号方式">
        <el-radio-group v-model="createForm.mode">
          <el-radio-button value="number">按编号</el-radio-button>
          <el-radio-button value="list">指定账号</el-radio-button>
        </el-radio-group>
      </el-form-item>
      <el-form-item label="建号区">
        <el-select v-model="createForm.zone" placeholder="跟随当前区" style="width: 100%">
          <el-option value="" label="跟随当前区" />
          <el-option v-for="z in zoneOptions()" :key="z.value" :value="z.value" :label="z.label" />
        </el-select>
      </el-form-item>

      <template v-if="createForm.mode === 'number'">
        <el-form-item label="前缀" prop="prefix">
          <el-input v-model="createForm.prefix" placeholder="robot000" />
        </el-form-item>
        <div class="form-row">
          <el-form-item label="起始序号">
            <el-input-number v-model="createForm.start" :min="0" :max="999999" :disabled="createForm.auto_start"
                             :controls="false" style="width: 110px" />
          </el-form-item>
          <el-form-item label="数量" prop="count" label-width="56px">
            <el-input-number v-model="createForm.count" :min="1" :max="200" :controls="false" style="width: 100px" />
          </el-form-item>
        </div>
        <div class="form-row">
          <el-form-item label="后缀">
            <el-input v-model="createForm.suffix" placeholder="@xy3.com" />
          </el-form-item>
          <el-form-item label="补零位数" label-width="76px">
            <el-input-number v-model="createForm.pad" :min="0" :max="12" :controls="false" style="width: 110px"
                             title="0=自动（按最大序号位数）" />
          </el-form-item>
        </div>
        <el-form-item label=" ">
          <el-checkbox v-model="createForm.auto_start" title="取池内同前缀最大序号+1，避免撞已有账号">自动接续</el-checkbox>
        </el-form-item>
      </template>
      <el-form-item v-else label="账号" prop="text">
        <el-input v-model="createForm.text" type="textarea" :rows="3" placeholder="要建的账号名（每行/逗号分隔）" />
      </el-form-item>

      <el-form-item label="将建">
        <span v-if="createPreview.count" class="mono">
          {{ createPreview.first }}<template v-if="createPreview.count > 1"> … {{ createPreview.last }}</template>
          （共 {{ createPreview.count }} 个）
        </span>
        <span v-else class="warnText">还没有可建的账号（检查前缀/数量）</span>
      </el-form-item>
      <div class="form-row">
        <el-form-item label="每批">
          <el-input-number v-model="createForm.batch_size" :min="1" :max="100" :controls="false" style="width: 96px" />
        </el-form-item>
        <el-form-item label="并发" label-width="56px">
          <el-input-number v-model="createForm.concurrency" :min="1" :max="16" :controls="false" style="width: 90px"
                           title="过高易触发同 IP 频控(112)" />
        </el-form-item>
        <el-form-item label="密码长度" label-width="76px">
          <el-input-number v-model="createForm.password_len" :min="8" :max="64" :controls="false" style="width: 96px"
                           title="默认 16 位（最少 8）" />
        </el-form-item>
      </div>
      <div class="form-row">
        <el-form-item label="超时(秒)">
          <el-input-number v-model="createForm.timeout_sec" :min="1" :max="120" :controls="false" style="width: 96px" />
        </el-form-item>
        <el-form-item label="密匙(可选)" label-width="86px">
          <el-input v-model="createForm.agent_key" placeholder="写入姓名位" />
        </el-form-item>
      </div>
      <el-form-item label="说明">
        <span class="muted">
          先验证再注册：已存在且密码对 → 跳过（标可用）；不存在 → 建号；密码不符 → 提示改密码。<br />
          密码每号随机、按服通用、存账号库（登录只用库里的密码）；建号结果在下方的卡片里逐条列出。
        </span>
      </el-form-item>
    </el-form>
    <template #footer>
      <el-button size="small" @click="ui.createOpen = false">取消</el-button>
      <el-button type="primary" size="small" :loading="createState.running" @click="submitCreate">
        {{ createState.running ? '建号中…' : '建号' }}
      </el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
/* ============================================================
   AccountsView 的 Element Plus 皮肤补丁（全局深色变量在 styles.css 里，这里不动）
   ============================================================ */

/* 统计卡：网格内并排，必须抵消全局的 `.el-card + .el-card { margin-top:16px }` */
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

.toolbar { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
/* el-button 相邻时自带 12px 左间距，和 flex gap 叠加会变宽，这里抹平 */
.toolbar :deep(.el-button + .el-button) { margin-left: 0; }
.k { color: var(--text-dim); font-size: 12px; }
.pager { display: flex; align-items: center; gap: 8px; margin-top: 10px; flex-wrap: wrap; }

/* 弹窗表单里的多列排布（比如"每批 / 并发 / 密码长度"一行三个） */
.form-row { display: flex; flex-wrap: wrap; gap: 0 12px; }

/* 在线行淡绿底：el-table 的底色画在 td 上，所以要落到 td */
:deep(.row-on td.el-table__cell) { background: rgba(62, 207, 142, 0.06); }

/* 固定列（操作）用 position:sticky + background:inherit，而全局把 --el-table-tr-bg-color
   设成了 transparent → 横向滚动时右侧列会从固定列底下透出来。
   把表格行底色设成与卡片一致的实色即可（观感不变，斑马纹/悬停仍由 Element 自己的规则覆盖）。
   注意：必须写成 `.pool-table.el-table`——全局那条 `html.dark .el-table` 的特异性更高，
   只写 `.pool-table` 会被压过去，变量改了不生效。 */
.pool-table.el-table {
  --el-table-tr-bg-color: var(--panel);
  /* 悬停底色从 rgba(76,141,255,.08) 换成等效实色 #1c2a46：观感一样，
     但固定列是 sticky，半透明底会让下面的列从它底下透出来 */
  --el-table-row-hover-bg-color: #1c2a46;
}
/* 表头行本身没有底色，固定表头要单独给（选择器要比 Element 的固定列规则更具体） */
:deep(.el-table__header-wrapper tr th.el-table-fixed-column--right) { background-color: var(--el-table-header-bg-color); }

/* 可点复制（建号拿到的随机密码） */
.copyable { cursor: pointer; }
.copyable:hover { color: var(--info); }
</style>
