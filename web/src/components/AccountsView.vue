<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import { apiGet, apiPost } from '../api'
import { state, post, toast, zones, currentZoneKey } from '../store'

const PAGE_SIZES = [20, 50, 100]

const rows = ref([])
const meta = ref({})
const stats = ref({})
const pageInfo = ref({ page: 1, pageSize: 20, hasMore: false })
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
function toggleAll() {
  if (selected.value.size === rows.value.length) selected.value = new Set()
  else selected.value = new Set(rows.value.map((r) => r.name))
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
  if (!zone) { alert('还没有可用的区：先在「系统信息 → 区管理」配置一个区'); return }
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
  if (!confirm(tip)) return
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
      toast(verify.msg, 'warn')
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
    toast(res.msg, 'ok')
    pollJob()
  } catch (e) {
    toast(e.message, 'danger')
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
      toast(`${head}：可用 ${j.usable} · 不可用 ${j.unusable} · 不存在 ${j.not_exists}`
        + (j.error ? ` · 失败 ${j.error}` : '') + `（${j.done}/${j.total}）`,
        j.status === 'canceled' ? 'warn' : 'ok')
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
    toast(res.msg || '已请求停止', res.ok ? 'warn' : 'danger')
  } catch (e) {
    toast(e.message, 'danger')
  }
}

// 次要入口：手输账号，同步验证这几个（结果同样写回池）
async function runVerify() {
  const tokens = verifyForm.text.split(/[,，;\s\n]+/).filter(Boolean)
  if (!tokens.length) { alert('请输入要验证的账号（每行一个，可写 账号:密码）'); return }
  if (tokens.length > 20) {
    const ok = confirm(`要一次验证 ${tokens.length} 个账号吗？\n建议一次 1 个：每个账号都会真连一次游戏服。`)
    if (!ok) return
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
      toast(verify.msg, 'warn')
      return
    }
    verify.summary = res.summary || {}
    verify.results = res.results || []
    verify.msg = res.msg || ''
    verify.game_addr = res.game_addr || ''
    verify.coding = res.coding || ''
    toast(`${res.msg}（${res.game_addr}）`, 'ok')
    load(true)
    loadPending()
  } catch (e) {
    toast(e.message, 'danger')
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
    toast('已复制：' + text, 'ok')
  } catch (e) {
    alert('复制失败，请手动选中：' + text)
  }
}

async function runCreate() {
  const zone = createForm.zone || targetZone()
  if (!zone) { alert('先选一个区'); return }
  const count = createForm.mode === 'number' ? Number(createForm.count) || 0 : createPreview.value.count
  if (count <= 0) { alert('数量要大于 0'); return }
  if (count > 200) { alert('一次最多建 200 个号（注册同 IP 过频会触发风控）'); return }
  const what = createForm.mode === 'number'
    ? `${createPreview.value.first} … ${createPreview.value.last}（共 ${count} 个）`
    : `指定的 ${count} 个账号`
  const tip = `建号：${what}\n区：${zone}\n密码：随机 ${createForm.password_len} 位，建号成功后写入账号库\n`
    + `分批：每批 ${createForm.batch_size} 个 · 并发 ${createForm.concurrency}\n`
    + '本项目没有统一密码：之后登录/验证都用库里的密码。'
  if (!confirm(tip)) return
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
    toast(res.msg || '建号完成', res.ok ? 'ok' : 'warn')
    load(true)
    loadPending()
  } catch (e) {
    toast(e.message, 'danger')
    createState.msg = e.message
  } finally {
    createState.running = false
  }
}

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
    if (res && res.ok === false) alert(res.msg || (up ? '上线失败' : '下线失败'))
    else toast(up ? `${r.name} 已通知上线` : `${r.name} 已下线`, 'ok')
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
  if (!auto && !names.length) { toast('先勾选账号，或用「自动挑号」上线', 'warn'); return }
  const tip = auto
    ? `从池里自动挑最多 ${batch.limit} 个【${targetZone() || '当前区'}】可用号上线？`
    : `上线勾选的 ${names.length} 个账号？\n${names.slice(0, 5).join('\n')}${names.length > 5 ? `\n… 等 ${names.length} 个` : ''}`
  if (!confirm(`${tip}\n分批：每批 ${batch.chunk} 个，间隔 ${batch.interval_ms}ms`)) return
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
      toast(`已通知上线：${res.sent ?? 0} 个（跳过 ${((res.skipped_online || []).length + (res.skipped_no_password || []).length)} 个）`, 'ok')
      if (!auto) selected.value = new Set()
      load()
    } else if (res) {
      alert(res.msg || '批量上线失败')
    }
  } finally {
    batchBusy.value = false
  }
}

async function batchOffline() {
  const names = selectedNames.value
  if (!names.length) { alert('请先勾选要下线的账号'); return }
  if (!confirm(`下线的 ${names.length} 个账号？`)) return
  const res = await post('/api/robots/batch', {
    action: 'offline', accounts: names, chunk: batch.chunk, interval_ms: batch.interval_ms,
  })
  if (res.ok) load()
}

async function addAccounts() {
  const names = addForm.text.split(/[,，;\s\n]+/).filter(Boolean)
  if (!names.length) { alert('请输入账号（逗号/换行分隔，可写 账号:密码）'); return }
  const res = await post('/api/accounts/add', { password: addForm.password, zone: addForm.zone, accounts: names })
  if (res.ok) { addForm.text = ''; load(true) }
}

async function removeAccounts() {
  const names = selectedNames.value
  if (!names.length) { alert('请先勾选账号'); return }
  if (!confirm(`从池里删除 ${names.length} 个账号？（游戏服上的账号不受影响）`)) return
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
  <div class="grid cols-4">
    <div class="stat">
      <div class="label">池内账号</div>
      <div class="value">{{ meta.count ?? 0 }}</div>
      <div class="sub">导入时间 {{ fmtTime(meta.generated_at) }}</div>
    </div>
    <div class="stat">
      <div class="label">该区可用</div>
      <div class="value">{{ stats.usable ?? 0 }}<span class="muted" style="font-size:14px">/{{ stats.total ?? 0 }}</span></div>
      <div class="sub">{{ filter.zone || '未选区（任意区可用）' }}</div>
    </div>
    <div class="stat">
      <div class="label">当前在线</div>
      <div class="value">{{ state.status.counts.online }}<span class="muted" style="font-size:14px">/{{ state.status.counts.total }}</span></div>
      <div class="sub">当前区 {{ state.status.current.key || '--' }}</div>
    </div>
    <div class="stat">
      <div class="label">已勾选</div>
      <div class="value">{{ selectedNames.length }}</div>
      <div class="sub">批量上线/下线/删号用</div>
    </div>
  </div>

  <div class="card">
    <div class="row">
      <span class="headline">{{ headline }}</span>
      <span class="spacer" />
      <select v-model="filter.zone" style="min-width: 230px" @change="load(true); loadPending()">
        <option value="">区：任意（不过滤）</option>
        <option v-for="z in zoneOptions()" :key="z.value" :value="z.value">区：{{ z.label }}</option>
      </select>
      <input v-model="filter.keyword" type="text" placeholder="检索：账号 / 角色名" style="width: 200px"
             @keyup.enter="load(true)" />
      <button class="btn primary" @click="load(true)">检索</button>
      <label class="row muted" style="gap:4px">
        <input v-model="filter.usable" type="checkbox" @change="load(true)" /> 只看可用
      </label>
      <label class="row muted" style="gap:4px">
        <input v-model="filter.onlyOnline" type="checkbox" @change="load(true)" /> 只看在线
      </label>
      <label class="row muted" style="gap:4px" title="把池里没有但当前在线的号也列出来">
        <input v-model="filter.includeLive" type="checkbox" @change="load(true)" /> 含池外在线
      </label>
      <select v-model="filter.pool" @change="load(true)" style="min-width: 130px"
              title="号池分区：新手池 = 未毕业；抓鬼池 = 已毕业（≥31 级或链完成）">
        <option value="">池分区：全部</option>
        <option value="newbie">新手池（未毕业）</option>
        <option value="ghost">抓鬼池（已毕业）</option>
        <option value="unknown">未知（等级未上报）</option>
      </select>
    </div>
  </div>

  <div class="card">
    <h3>可用性验证（直连游戏服跑登录协议 102，不进入游戏、无副作用）</h3>

    <!-- 默认动作：从本地库按当前区批量验证，结果逐条同步回账号池 -->
    <div class="row" style="margin-bottom: 8px">
      <select v-model="verifyForm.zone" style="min-width: 210px" @change="loadPending()">
        <option value="">验证区：跟随当前区</option>
        <option v-for="z in zoneOptions()" :key="z.value" :value="z.value">{{ z.label }}</option>
      </select>
      <select v-model="verifyForm.scope" style="min-width: 250px">
        <option v-for="s in SCOPES" :key="s.value" :value="s.value">范围：{{ s.label }}</option>
      </select>
      <label class="muted">本次</label>
      <input v-model.number="verifyForm.limit" type="number" style="width: 88px"
             title="0 = 不限（验完范围内全部）" />
      <span v-if="!verifyForm.limit" class="muted">全部 ≈ {{ pending.zone ?? 0 }} 个</span>
      <label class="muted">并发</label>
      <input v-model.number="verifyForm.concurrency" type="number" style="width: 70px" title="同时探测几个账号（默认 2）" />
      <label class="row muted" style="gap:4px"><input v-model="verifyForm.skip_online" type="checkbox" /> 跳过在线号</label>
      <button class="btn primary" :disabled="jobRunning" @click="startBatchVerify">
        {{ jobRunning ? '批量验证中…' : '批量验证当前区' }}
      </button>
      <button class="btn danger" :disabled="!jobRunning" @click="stopBatchVerify">停止</button>
    </div>

    <div class="row muted" style="gap:14px">
      <span>本区全部 <b>{{ pending.zone ?? 0 }}</b></span>
      <span>待验证 <b>{{ pending.unverified ?? 0 }}</b></span>
      <span>已知不可用 <b>{{ pending.unusable ?? 0 }}</b></span>
      <span>该区无记录 <b>{{ pending.unknown ?? 0 }}</b></span>
      <span>池内共 <b>{{ pending.all ?? 0 }}</b></span>
      <span class="spacer" />
      <label class="row" style="gap:4px"><input v-model="verifyForm.query_role" type="checkbox" /> 查角色名/等级</label>
      <label class="row" style="gap:4px"><input v-model="verifyForm.persist" type="checkbox" /> 结果写回池（同步）</label>
      <label class="muted">超时(秒)</label>
      <input v-model.number="verifyForm.timeout_sec" type="number" style="width: 66px" />
    </div>

    <!-- 批量进度 -->
    <div v-if="verify.job" class="verify-job">
      <div class="row" style="margin-bottom: 6px">
        <span class="tag" :class="jobRunning ? 'info' : (verify.job.status === 'canceled' ? 'warn' : 'ok')">
          {{ jobRunning ? '进行中' : (verify.job.status === 'canceled' ? '已停止' : '已完成') }}
        </span>
        <span class="muted">
          {{ verify.job.done }}/{{ verify.job.total }} · 可用 {{ verify.job.usable }} · 不可用
          {{ verify.job.unusable }} · 不存在 {{ verify.job.not_exists }}
          <template v-if="verify.job.error"> · 失败 {{ verify.job.error }}</template>
          · {{ ((verify.job.elapsed_ms || 0) / 1000).toFixed(1) }}s
        </span>
        <span class="spacer" />
        <span class="muted mono">{{ verify.job.zone }}</span>
      </div>
      <div class="bar"><i :style="{ width: jobProgress + '%' }" :class="{ live: jobRunning }" /></div>
    </div>

    <div v-if="verify.msg" class="row" style="margin-top: 10px">
      <span class="tag" :class="(verify.summary?.error || verify.job?.error) ? 'warn' : 'ok'">{{ verify.msg }}</span>
      <span class="muted mono">{{ verify.game_addr }} · {{ verify.coding }}</span>
    </div>

    <!-- 次要入口：手输账号（同步验证这几个） -->
    <div class="row" style="margin-top: 12px">
      <span class="muted">手输账号单独验（密码从库里取）：</span>
      <input v-model="verifyForm.text" type="text" style="flex:1; min-width: 260px"
             placeholder="账号（每行/逗号分隔，可写 账号:密码）；默认 1 个就够"
             @keyup.enter="runVerify" />
      <button class="btn" :disabled="verify.running" @click="runVerify">
        {{ verify.running ? '验证中…' : '立即验证' }}
      </button>
    </div>

    <table v-if="verify.results.length" style="margin-top: 8px">
      <thead>
        <tr><th>账号</th><th>结论</th><th>服务端码</th><th>角色</th><th>等级</th><th>耗时</th><th>说明</th></tr>
      </thead>
      <tbody>
        <tr v-for="r in verify.results" :key="r.account">
          <td class="mono">{{ r.account }}</td>
          <td><span class="tag" :class="verifyTagClass(r)">{{ verifyText(r) }}</span></td>
          <td class="mono">{{ r.ret_code }}</td>
          <td>{{ r.role_name || '--' }}</td>
          <td>{{ r.level || '--' }}</td>
          <td class="muted">{{ r.elapsed_ms }}ms</td>
          <td class="muted">{{ r.err || r.msg }}</td>
        </tr>
      </tbody>
    </table>
  </div>

  <div class="card">
    <div class="row" style="margin-bottom: 8px">
      <h3 style="margin:0">建号 / 校验账号（注册协议 106 → 104 → 700）</h3>
      <span class="spacer" />
      <div class="seg">
        <button :class="{ on: createForm.mode === 'number' }" @click="createForm.mode = 'number'">按编号</button>
        <button :class="{ on: createForm.mode === 'list' }" @click="createForm.mode = 'list'">指定账号</button>
      </div>
      <select v-model="createForm.zone" style="min-width: 200px">
        <option value="">建号区：跟随当前区</option>
        <option v-for="z in zoneOptions()" :key="z.value" :value="z.value">{{ z.label }}</option>
      </select>
    </div>

    <div v-if="createForm.mode === 'number'" class="row muted">
      <label>前缀</label>
      <input v-model="createForm.prefix" type="text" style="width: 110px" placeholder="robot000" />
      <label>起始序号</label>
      <input v-model.number="createForm.start" type="number" style="width: 84px" :disabled="createForm.auto_start" />
      <label class="row" style="gap:4px" title="取池内同前缀最大序号+1，避免撞已有账号">
        <input v-model="createForm.auto_start" type="checkbox" /> 自动接续
      </label>
      <label>数量</label>
      <input v-model.number="createForm.count" type="number" style="width: 70px" />
      <label>后缀</label>
      <input v-model="createForm.suffix" type="text" style="width: 110px" placeholder="@xy3.com" />
      <label>补零位数</label>
      <input v-model.number="createForm.pad" type="number" style="width: 70px" title="0=自动（按最大序号位数）" />
      <button class="btn primary" :disabled="createState.running" @click="runCreate">
        {{ createState.running ? '建号中…' : '建号' }}
      </button>
    </div>
    <div v-else class="row" style="margin-bottom: 8px">
      <textarea v-model="createForm.text" rows="2" class="textarea"
                placeholder="要建的账号名（每行/逗号分隔）"></textarea>
      <button class="btn primary" :disabled="createState.running" @click="runCreate">
        {{ createState.running ? '建号中…' : '建号' }}
      </button>
    </div>

    <div class="row muted" style="gap:14px">
      <span v-if="createPreview.count" class="mono">
        将建：{{ createPreview.first }}<template v-if="createPreview.count > 1"> … {{ createPreview.last }}</template>
        （共 {{ createPreview.count }} 个）
      </span>
      <span v-else class="warnText">还没有可建的账号（检查前缀/数量）</span>
      <span class="spacer" />
      <label>每批</label>
      <input v-model.number="createForm.batch_size" type="number" style="width: 66px" />
      <label>并发</label>
      <input v-model.number="createForm.concurrency" type="number" style="width: 60px" title="过高易触发同 IP 频控(112)" />
      <label>密码长度</label>
      <input v-model.number="createForm.password_len" type="number" style="width: 66px" title="默认 16 位（最少 8）" />
      <label>超时(秒)</label>
      <input v-model.number="createForm.timeout_sec" type="number" style="width: 66px" />
      <label>密匙(可选)</label>
      <input v-model="createForm.agent_key" type="text" style="width: 130px" placeholder="写入姓名位" />
    </div>
    <div class="row muted">
      <span>先验证再注册：已存在且密码对 → 跳过（标可用）；不存在 → 建号；密码不符 → 提示改密码</span>
      <span class="spacer" />
      <span>密码每号随机、按服通用、存账号库（登录只用库里的密码）</span>
    </div>
    <div v-if="createState.msg" class="row" style="margin-top: 8px">
      <span class="tag" :class="createState.created ? 'ok' : 'warn'">{{ createState.msg }}</span>
    </div>
    <table v-if="createState.results.length" style="margin-top: 8px">
      <thead>
        <tr><th>账号</th><th>结果</th><th>探测码</th><th>errid</th><th>密码（已入库）</th><th>角色</th><th>说明</th></tr>
      </thead>
      <tbody>
        <tr v-for="r in createState.results" :key="r.account">
          <td class="mono">{{ r.account }}</td>
          <td><span class="tag" :class="createStatusClass(r.status)">{{ createStatusLabel(r.status) }}</span></td>
          <td class="mono">{{ r.ret_code }}</td>
          <td class="mono">{{ r.err_id || '--' }}</td>
          <td class="mono copyable" :title="r.password ? '点一下复制' : ''" @click="copyText(r.password)">
            {{ r.password ? r.password + ' ⧉' : '--' }}
          </td>
          <td>{{ r.role_name ? `${r.role_name} Lv${r.level || 0}` : '--' }}</td>
          <td class="muted">{{ r.err || r.msg }}</td>
        </tr>
      </tbody>
    </table>
  </div>

  <div v-if="err" class="card"><span class="tag danger">{{ err }}</span></div>

  <!-- 上线/下线放在表格**上面**：勾完就能点，不用往下滚 -->
  <div class="card">
    <div class="row" style="margin-bottom:8px">
      <h3 style="margin:0">上线 / 下线</h3>
      <span class="muted">点行内「上线」立刻拉起；批量就勾选（可跨页），不用输账号</span>
      <span class="spacer" />
      <span class="muted">已选</span><b>{{ selectedNames.length }}</b>
      <button class="btn primary" :disabled="!selectedNames.length || batchBusy" @click="batchOnline(false)">
        {{ batchBusy ? '下发中…' : `上线勾选的 ${selectedNames.length} 个` }}
      </button>
      <button class="btn" :disabled="!selectedNames.length || batchBusy" @click="batchOffline">下线勾选的</button>
      <button class="btn danger" :disabled="!selectedNames.length" @click="removeAccounts">从池删除</button>
    </div>
    <div class="row">
      <span class="muted">快捷选择</span>
      <button class="btn sm" @click="selectPage('all')">全选本页</button>
      <button class="btn sm" @click="selectPage('usable')">选本页可用</button>
      <button class="btn sm" @click="selectPage('offline')">选本页未上线</button>
      <button class="btn sm" @click="selectPage('online')">选本页在线</button>
      <button class="btn sm" @click="selectPage('none')">清空</button>
      <span class="spacer" />
      <label class="muted">自动挑号上限</label>
      <input v-model.number="batch.limit" type="number" style="width: 70px" title="不勾选时自动挑号的个数（默认 1：一次一个）" />
      <button class="btn" :disabled="batchBusy" @click="batchOnline(true)">自动挑 {{ batch.limit }} 个可用号上线</button>
    </div>
    <div class="row muted">
      <label>每批</label>
      <input v-model.number="batch.chunk" type="number" style="width: 66px" />
      <label>批间隔(ms)</label>
      <input v-model.number="batch.interval_ms" type="number" style="width: 92px" />
      <label class="row" style="gap:4px"><input v-model="batch.only_usable" type="checkbox" /> 只选可用号</label>
      <span class="spacer" />
      <span>默认「一次一个」（上限/每批都是 1）——想放量再往上调；已在线的会被跳过</span>
    </div>
  </div>

  <div class="card">
    <div class="row" style="margin-bottom:8px">
      <h3 style="margin:0">账号池（第 {{ pageInfo.page }} 页 · 本页 {{ rows.length }} 行{{ loading ? ' · 加载中…' : '' }}）</h3>
      <span class="spacer" />
      <span class="muted">密码不下发到前端；批量上线由服务端从池里取密码</span>
    </div>
    <div class="table-wrap" style="max-height: 56vh">
      <table>
        <thead>
          <tr>
            <th style="width:36px"><input type="checkbox" :checked="selected.size === rows.length && rows.length > 0" @change="toggleAll" /></th>
            <th>账号</th><th style="width:64px">池</th><th>等级</th><th>角色</th><th>该区可用</th><th>验证信息</th>
            <th>验证时间</th><th>在线</th><th>状态</th><th>运行时区</th><th>密码</th><th>备注</th>
            <th style="width:96px">操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="r in rows" :key="r.name" :class="{ 'row-on': r.online }">
            <td><input type="checkbox" :checked="selected.has(r.name)" @change="toggle(r.name)" /></td>
            <td class="mono">{{ r.name }}</td>
            <td>
              <span v-if="r.pool === 'newbie'" class="tag info" title="新手池：未毕业（等级<31 且未链完成）">新手</span>
              <span v-else-if="r.pool === 'ghost'" class="tag warn" title="抓鬼池：已毕业（≥31 级 或 链完成）">抓鬼</span>
              <span v-else class="tag dim" title="等级未上报/无记录">未知</span>
            </td>
            <td>{{ r.level || '--' }}</td>
            <td>{{ r.role_name || '--' }}</td>
            <td>
              <span class="tag" :class="r.usable ? 'ok' : (r.verified ? 'danger' : 'dim')">
                {{ r.usable ? '可用' : (r.verified ? '不可用' : '未验证') }}
              </span>
            </td>
            <td class="muted ellipsis" :title="r.verify_msg || ''">{{ r.verify_msg || '--' }}</td>
            <td class="muted" :title="r.verified_at ? fmtTime(r.verified_at) : '从未验证'">
              {{ r.verified_at ? fmtTime(r.verified_at) : '--' }}
            </td>
            <td><span class="tag" :class="r.online ? 'ok' : 'dim'">{{ r.online ? '在线' : '离线' }}</span></td>
            <td><span class="tag dim">{{ r.state || '--' }}</span></td>
            <td class="mono muted">{{ r.runtime_zone || r.zone || '--' }}</td>
            <td><span class="tag" :class="r.has_password ? 'dim' : 'warn'">{{ r.has_password ? '已设' : '未设' }}</span></td>
            <td class="muted">{{ r.note || '--' }}</td>
            <td>
              <button v-if="r.online" class="btn sm" :disabled="!!rowBusy"
                      @click="toggleOnline(r)">
                {{ rowBusy === r.name ? '下线中…' : '下线' }}
              </button>
              <button v-else class="btn sm primary" :disabled="!!rowBusy"
                      @click="toggleOnline(r)">
                {{ rowBusy === r.name ? '上线中…' : '上线' }}
              </button>
            </td>
          </tr>
          <tr v-if="!rows.length">
            <td colspan="13">
              <div class="empty">
                这一页没有数据。
                <template v-if="!meta.count">先在下面「加号入池」把账号加进来。</template>
                <template v-else>换个关键词/筛选，或翻回上一页。</template>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <div class="pager">
      <span class="muted">
        第 {{ pageInfo.page }} 页 · 本页 {{ rows.length }} 行
        <template v-if="stats.total">· 池内共 {{ stats.total }} 个（该区可用 {{ stats.usable ?? 0 }}）</template>
      </span>
      <span class="spacer" />
      <label class="row muted" style="gap:4px">
        每页
        <select v-model.number="pageInfo.pageSize" @change="load(true)">
          <option v-for="n in PAGE_SIZES" :key="n" :value="n">{{ n }}</option>
        </select>
      </label>
      <button class="btn sm" :disabled="pageInfo.page <= 1" @click="gotoPage(-1)">上一页</button>
      <button class="btn sm" :disabled="!pageInfo.hasMore" @click="gotoPage(1)">下一页</button>
    </div>
  </div>


  <div class="card">
    <h3>加号入池</h3>
    <div class="row">
      <textarea v-model="addForm.text" rows="2" placeholder="账号，逗号/换行分隔（可写 账号:密码）"
                class="textarea"></textarea>
      <input v-model="addForm.password" type="text" placeholder="统一密码（可空=默认）" style="width: 170px" />
      <select v-model="addForm.zone" style="min-width: 200px">
        <option value="">分配区：不指定</option>
        <option v-for="z in zoneOptions()" :key="z.value" :value="z.value">{{ z.label }}</option>
      </select>
      <button class="btn primary" @click="addAccounts">加入池</button>
    </div>
  </div>
</template>

<style scoped>
.row-on { background: rgba(62, 207, 142, 0.06); }
.ellipsis { max-width: 260px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.textarea {
  flex: 1; min-width: 320px; background: var(--bg); color: var(--text);
  border: 1px solid var(--border); border-radius: 8px; padding: 6px 10px; font-size: 13px;
}
</style>
