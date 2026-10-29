<script setup>
// 「实例管理」页（2026-10-14 多实例形态：A 栈 28082 生产 / B 栈 28083 试点 / 将来越多）。
// 一个页面管所有实例：列表+状态、启动/停止/重启、打开面板（跳转）、创建向导、归档。
//
// 数据源（后端 2026-10-14 新契约，未部署时双兼容降级）：
//   GET  /api/instances                      → {ok:true,instances:[...]}
//   POST /api/instances                      → {ok:true,id,start_script,warnings[]}
//                                              | {ok:false,checks:[{name,pass,detail}]}
//   POST /api/instances/{id}/start|stop      → {ok,msg?}
//   POST /api/instances/{id}/restart {mode}  → {ok,msg?}（mode=controller|cold|robot）
//   DELETE /api/instances/{id} {confirm:id}  → {ok,archived?}
//
// 降级：GET 404 / 非 ok → 空态卡片「此后端版本不支持实例管理（需新版中控），升级后本页自动可用」，
// 并用慢轮询（20s）自动探测——后端部署后本页无需刷新即自动可用；页面其他部分不报错。
//
// 轮询：进入拉一次 + 每 5s 轮询；document.hidden 时暂停（与 store.js 的 visibilitychange 同思路）。
// 写操作统一走 ElMessage 提示 + 单按钮 loading；危险操作（停止/重启/归档）ElMessageBox 二次确认。
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { apiGet, apiPost } from '../api'
import { fmtAgo } from '../store'

const POLL_MS = 5000        // 正常轮询间隔
const DEGRADE_MS = 20000    // 降级（后端未部署）时的慢轮询：部署后自动恢复

const loading = ref(false)      // 首次/手动刷新转圈（静默轮询不动它）
const degrade = ref(false)      // 后端不支持（404 / 非 ok）→ 降级卡片
const degradeDetail = ref('')   // 降级时的原始错误（弱化显示，便于排查）
const err = ref('')             // 其他错误（网络/5xx）：保留旧数据 + 顶部提示
const instances = ref([])
const lastAt = ref(0)           // 上次检查完成时间（秒）
const busy = reactive({})       // `${id}:${act}` -> true（按钮 loading）

function busyKey(id, act) { return `${id}:${act}` }
function isBusy(id, act) { return !!busy[busyKey(id, act)] }

// ---------------- 列表加载 ----------------

async function load(opts = {}) {
  const silent = !!opts.silent // 轮询：不转圈、失败不弹窗（保留旧数据）
  if (!silent) loading.value = true
  try {
    const res = await apiGet('/api/instances')
    if (!res || res.ok !== true || !Array.isArray(res.instances)) {
      // 接口在但返回不合契约（ok!==true）→ 按降级处理（与 404 同一张空态卡片）
      degrade.value = true
      degradeDetail.value = (res && res.msg) ? String(res.msg) : `响应不合契约（ok=${res ? res.ok : '无响应'}）`
      lastAt.value = Date.now() / 1000
      return
    }
    instances.value = res.instances
    degrade.value = false
    degradeDetail.value = ''
    err.value = ''
    lastAt.value = Date.now() / 1000
  } catch (e) {
    const msg = (e && e.message) ? e.message : String(e)
    if (msg.includes('HTTP 404')) {
      // 旧中控还没加载这个路由：降级空态；静默轮询会持续探测，部署后自动恢复
      degrade.value = true
      degradeDetail.value = msg
      err.value = ''
      lastAt.value = Date.now() / 1000
    } else {
      err.value = msg
      if (!silent) ElMessage.error(`刷新失败：${msg}`)
    }
  } finally {
    if (!silent) loading.value = false
  }
}

// ---------------- 轮询（隐藏暂停，同 store.js 思路）----------------

let timer = 0
let hidden = document.hidden
let alive = true

function scheduleNext() {
  if (!alive || hidden) return
  clearTimeout(timer)
  timer = setTimeout(async () => {
    timer = 0
    await load({ silent: true })
    scheduleNext()
  }, degrade.value ? DEGRADE_MS : POLL_MS)
}

function onVisibility() {
  const wasHidden = hidden
  hidden = document.hidden
  if (hidden) {
    clearTimeout(timer)
    timer = 0
    return
  }
  if (wasHidden) load({ silent: true }) // 切回可见：立即补一轮
  scheduleNext()
}

onMounted(() => {
  load()
  scheduleNext()
  document.addEventListener('visibilitychange', onVisibility)
})
onUnmounted(() => {
  alive = false
  clearTimeout(timer)
  document.removeEventListener('visibilitychange', onVisibility)
})

// ---------------- 展示辅助 ----------------

function stOf(inst) { return inst.status || {} }
function dotClass(v) { return v === true ? 'ok' : (v === false ? 'bad' : '') }
function zoneText(inst) {
  if (!inst.zone_server && !inst.zone_key) return '--'
  return `${inst.zone_server || '--'}/${inst.zone_key || '--'}`
}
// 本页由哪个实例提供（web_port 与当前地址栏端口一致）→ 打「本页」标
function isThisPage(inst) { return String(inst.web_port) === String(location.port || '') }
function onlineText(inst) {
  const n = stOf(inst).online
  if (n === null || n === undefined || Number(n) < 0) return '--' // -1=无读数
  return String(n)
}
function pidText(v) { return v ? `PID ${v}` : '' }

// ---------------- 操作：启动/停止/重启/打开面板/归档 ----------------

function openPanel(inst) {
  window.open(`http://${location.hostname}:${inst.web_port}/`, '_blank')
}

// DELETE 没有现成封装（api.js 只有 get/post）：就地补一个，错误语义与 apiPost 对齐
async function apiDelete(path, body) {
  const resp = await fetch(path, {
    method: 'DELETE',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body || {}),
  })
  const data = await resp.json().catch(() => ({}))
  if (resp.status === 401) throw new Error(data.msg || '未授权（需要 API token）')
  if (!resp.ok) throw new Error(`DELETE ${path} -> HTTP ${resp.status}`)
  return data
}

// 写操作统一入口：单按钮 loading + ElMessage 结果 + 刷新列表
async function runOp(inst, act, fn, okText) {
  const k = busyKey(inst.id, act)
  busy[k] = true
  try {
    const res = await fn()
    if (res && res.ok === false) ElMessage.error((res && res.msg) || `${okText}失败`)
    else ElMessage.success((res && res.msg) || okText)
    if (res && Array.isArray(res.warnings) && res.warnings.length) {
      ElMessage.warning(res.warnings.join('；'))
    }
    await load({ silent: true })
  } catch (e) {
    ElMessage.error((e && e.message) ? e.message : String(e))
  } finally {
    busy[k] = false
  }
}

async function onStart(inst) {
  await runOp(inst, 'start', () => apiPost(`/api/instances/${inst.id}/start`), '启动指令已下发')
}

async function onStop(inst) {
  try {
    await ElMessageBox.confirm(
      `停止【${inst.name || inst.id}】？将停止该实例的中控与机器人进程，并标记为「人工停止」——看门狗不会自动拉起；需点「启动」才会恢复。`,
      '确认停止', { type: 'warning', confirmButtonText: '停止', cancelButtonText: '取消' },
    )
  } catch { return }
  await runOp(inst, 'stop', () => apiPost(`/api/instances/${inst.id}/stop`), '已停止（人工停止，看门狗不会拉起）')
}

// 重启三选（后端 mode 口径：controller/cold/robot）
const RESTART_MODES = {
  controller: { label: '仅中控', tip: '只重启中控进程，机器人进程不动；重启期间该实例面板短暂不可用。' },
  cold: { label: '冷启动', tip: '按启动脚本冷启整个实例（中控与机器人进程），机器人会断开并重新登录。' },
  robot: { label: '仅机器人', tip: '只重启机器人进程，中控不动；机器人会断开并重新登录，当前任务会中断。' },
}
async function onRestart(inst, mode) {
  const m = RESTART_MODES[mode]
  if (!m) return
  try {
    await ElMessageBox.confirm(
      `重启【${inst.name || inst.id}】——${m.label}：${m.tip}`,
      `确认重启（${m.label}）`, { type: 'warning', confirmButtonText: '重启', cancelButtonText: '取消' },
    )
  } catch { return }
  await runOp(inst, `restart-${mode}`, () => apiPost(`/api/instances/${inst.id}/restart`, { mode }),
    `重启指令已下发（${m.label}）`)
}

// 归档：危险操作，要求输入实例 id 确认（body.confirm=id）
async function onArchive(inst) {
  try {
    await ElMessageBox.prompt(
      `归档【${inst.name || inst.id}】：将停止该实例的进程，并把实例目录改名为 _archived_ 归档保留（可手工恢复）。进程不会自动恢复。\n\n请输入实例 id「${inst.id}」以确认：`,
      '归档实例（危险操作）',
      {
        type: 'warning', confirmButtonText: '归档', cancelButtonText: '取消',
        inputPlaceholder: inst.id,
        inputValidator: (v) => String(v || '').trim() === String(inst.id) || `请输入完整实例 id：${inst.id}`,
      },
    )
  } catch { return }
  await runOp(inst, 'archive', () => apiDelete(`/api/instances/${inst.id}`, { confirm: inst.id }), '已归档')
}

// ---------------- 创建向导 ----------------

const createDlg = ref(false)
const creating = ref(false)
const formRef = ref(null)
const form = reactive({ name: '', zone_server: 'prod-240', zone_key: '', web_port: 28082, ctrl_port: 27200, note: '' })
const createChecks = ref([])   // 预检失败清单 [{name,pass,detail}]
const createResult = ref(null) // 成功结果 {id,start_script,warnings[]}
const createErr = ref('')      // 提交异常（网络/404 等）

const formRules = {
  name: [{ required: true, message: '请填实例名称', trigger: 'blur' }],
  zone_server: [{ required: true, message: '请填游戏服（如 prod-240）', trigger: 'blur' }],
  zone_key: [
    { required: true, message: '请填区号（如 2500）', trigger: 'blur' },
    { pattern: /^[A-Za-z0-9._-]+$/, message: '只能用字母/数字/._-', trigger: 'blur' },
  ],
}

const failCount = computed(() => createChecks.value.filter((c) => !c.pass).length)
// 端口服冲突提示（不阻断提交，交给后端预检做权威判定）
const dupPort = computed(() => {
  const out = []
  const w = instances.value.find((i) => Number(i.web_port) === Number(form.web_port))
  const c = instances.value.find((i) => Number(i.ctrl_port) === Number(form.ctrl_port))
  if (w) out.push(`web_port ${form.web_port} 已被【${w.name || w.id}】占用`)
  if (c) out.push(`ctrl_port ${form.ctrl_port} 已被【${c.name || c.id}】占用`)
  return out
})

function openCreate() {
  // 端口默认：现有最大 + 1（拿不到就用约定默认值）
  const ws = instances.value.map((i) => Number(i.web_port)).filter((n) => n > 0)
  const cs = instances.value.map((i) => Number(i.ctrl_port)).filter((n) => n > 0)
  form.name = ''
  form.zone_server = 'prod-240'
  form.zone_key = ''
  form.web_port = ws.length ? Math.max(...ws) + 1 : 28082
  form.ctrl_port = cs.length ? Math.max(...cs) + 1 : 27200
  form.note = ''
  createChecks.value = []
  createResult.value = null
  createErr.value = ''
  createDlg.value = true
}

async function onCreate() {
  createErr.value = ''
  createChecks.value = []
  try { await formRef.value.validate() } catch { return }
  creating.value = true
  try {
    const body = {
      name: form.name.trim(),
      zone_server: form.zone_server.trim(),
      zone_key: form.zone_key.trim(),
      web_port: Number(form.web_port),
      ctrl_port: Number(form.ctrl_port),
    }
    const note = form.note.trim()
    if (note) body.note = note
    const res = await apiPost('/api/instances', body)
    if (res && res.ok) {
      // 创建成功（注意：只生成脚本，不自动启动）
      createResult.value = { id: res.id, start_script: res.start_script || '', warnings: res.warnings || [] }
      await load({ silent: true })
    } else {
      createChecks.value = Array.isArray(res && res.checks) ? res.checks : []
      if (!createChecks.value.length) createErr.value = (res && res.msg) || '创建失败（后端未给出原因）'
    }
  } catch (e) {
    const msg = (e && e.message) ? e.message : String(e)
    createErr.value = msg.includes('HTTP 404')
      ? `${msg} —— 该中控版本还没有实例管理接口，升级后可创建。`
      : msg
  } finally {
    creating.value = false
  }
}
</script>

<template>
  <div class="card">
    <div class="row">
      <b>实例管理</b>
      <span class="muted small">每个实例 = 一个中控 + 一个机器人进程，绑定一个游戏区；危险操作有确认。</span>
      <span class="spacer" />
      <span v-if="lastAt" class="muted small">检查于 {{ fmtAgo(lastAt) }}</span>
      <el-button size="small" :loading="loading" @click="load()">刷新</el-button>
      <el-button
        size="small" type="primary" :disabled="degrade" @click="openCreate"
        :title="degrade ? '需新版中控（当前后端不支持实例管理接口）' : '创建新实例（只生成启动脚本，不自动启动）'"
      >新建实例</el-button>
    </div>
  </div>

  <!-- 其他错误（网络/5xx）：保留旧数据 + 顶部提示 -->
  <el-alert v-if="err" class="mb" type="error" :closable="false" show-icon title="实例列表刷新失败（显示的是上次数据）">
    <div class="mono small">{{ err }}</div>
    <el-button size="small" class="mt6" :loading="loading" @click="load()">重试</el-button>
  </el-alert>

  <!-- 降级：旧中控没有 /api/instances（404 / 非 ok）→ 空态卡片，慢轮询自动探测升级 -->
  <div v-if="degrade" class="card">
    <el-empty :image-size="70" description="此后端版本不支持实例管理（需新版中控），升级后本页自动可用" />
    <div class="center muted small mono">{{ degradeDetail || 'GET /api/instances 不可用' }}</div>
  </div>

  <!-- 实例卡列表 -->
  <template v-else>
    <div v-if="instances.length" class="cards">
      <div v-for="inst in instances" :key="inst.id" class="inst">
        <div class="row">
          <b>{{ inst.name || inst.id }}</b>
          <span class="tag info mono" title="zone_server / zone_key">{{ zoneText(inst) }}</span>
          <span v-if="isThisPage(inst)" class="tag warn" title="本页面由该实例的中控提供">本页</span>
          <span v-if="inst.stopped" class="tag warn" title="面板人工停止：看门狗不会自动拉起；点「启动」恢复">已停用</span>
          <span class="spacer" />
          <span class="muted small mono">id: {{ inst.id }}</span>
        </div>

        <div class="row small">
          <span class="mono">web:{{ inst.web_port }} · ctrl:{{ inst.ctrl_port }}</span>
          <span class="spacer" />
          <span v-if="inst.start_script" class="mono muted" :title="inst.start_script">
            启动脚本 {{ inst.start_script }}
          </span>
        </div>

        <div class="kv-row" :title="inst.data_dir || ''">
          <span class="k">data</span>
          <span class="mono ellipsis">{{ inst.data_dir || '--' }}</span>
        </div>
        <div class="kv-row" :title="inst.deploy_dir || ''">
          <span class="k">deploy</span>
          <span class="mono ellipsis">{{ inst.deploy_dir || '--' }}</span>
        </div>
        <div v-if="inst.note" class="small muted">备注：{{ inst.note }}</div>

        <!-- 状态灯：中控 / HTTP / 机器人 / 在线数 -->
        <div class="row status">
          <span class="st" :title="`中控进程（${inst.ctrl_port}）`">
            <span class="dot" :class="dotClass(stOf(inst).ctrl_alive)" />
            中控{{ stOf(inst).ctrl_alive ? '运行中' : '未运行' }}
            <span v-if="stOf(inst).ctrl_alive && stOf(inst).ctrl_pid" class="mono muted">{{ pidText(stOf(inst).ctrl_pid) }}</span>
          </span>
          <span class="st" :title="'中控 HTTP 接口是否可达'">
            <span class="dot" :class="dotClass(stOf(inst).http_ok)" />
            HTTP{{ stOf(inst).http_ok ? '正常' : '不通' }}
          </span>
          <span class="st" :title="'机器人进程'">
            <span class="dot" :class="dotClass(stOf(inst).robot_alive)" />
            机器人{{ stOf(inst).robot_alive ? '运行中' : '未运行' }}
            <span v-if="stOf(inst).robot_alive && stOf(inst).robot_pid" class="mono muted">{{ pidText(stOf(inst).robot_pid) }}</span>
          </span>
          <span class="st" title="游戏在线数（-1=暂无读数）">
            在线 <b class="mono">{{ onlineText(inst) }}</b>
          </span>
          <span v-if="stOf(inst).checked_at" class="muted small" :title="'状态检查时间'">检查于 {{ fmtAgo(stOf(inst).checked_at) }}</span>
        </div>

        <!-- 操作组 -->
        <div class="row">
          <template v-if="!stOf(inst).ctrl_alive">
            <el-button size="small" type="primary" :loading="isBusy(inst.id, 'start')" @click="onStart(inst)">启动</el-button>
          </template>
          <template v-else>
            <el-button size="small" type="danger" :loading="isBusy(inst.id, 'stop')" @click="onStop(inst)">停止</el-button>
            <el-dropdown trigger="click" @command="(m) => onRestart(inst, m)">
              <el-button
                size="small"
                :loading="isBusy(inst.id, 'restart-controller') || isBusy(inst.id, 'restart-cold') || isBusy(inst.id, 'restart-robot')"
              >
                重启<el-icon class="el-icon--right"><arrow-down /></el-icon>
              </el-button>
              <template #dropdown>
                <el-dropdown-menu>
                  <el-dropdown-item command="controller">仅中控（controller）</el-dropdown-item>
                  <el-dropdown-item command="cold">冷启动（cold）</el-dropdown-item>
                  <el-dropdown-item command="robot">仅机器人（robot）</el-dropdown-item>
                </el-dropdown-menu>
              </template>
            </el-dropdown>
          </template>
          <el-button
            size="small" :disabled="!stOf(inst).ctrl_alive"
            :title="stOf(inst).ctrl_alive ? '新标签页打开该实例面板' : '中控未运行，面板打不开'"
            @click="openPanel(inst)"
          >打开面板</el-button>
          <span class="spacer" />
          <el-button size="small" type="danger" plain :loading="isBusy(inst.id, 'archive')" @click="onArchive(inst)">归档</el-button>
        </div>
      </div>
    </div>
    <div v-else-if="!loading" class="card">
      <el-empty :image-size="70" description="还没有实例记录（点右上「新建实例」创建）" />
    </div>
  </template>

  <!-- 创建向导 -->
  <el-dialog v-model="createDlg" title="新建实例" width="580px" :close-on-click-modal="false">
    <!-- 第一步：表单 + 预检结果（成功后切结果视图） -->
    <template v-if="!createResult">
      <el-form ref="formRef" :model="form" :rules="formRules" label-width="92px">
        <el-form-item label="名称" prop="name">
          <el-input v-model="form.name" placeholder="如：试点实例(2500)" maxlength="40" />
        </el-form-item>
        <el-form-item label="游戏服" prop="zone_server">
          <el-input v-model="form.zone_server" placeholder="prod-240" />
        </el-form-item>
        <el-form-item label="区号" prop="zone_key">
          <el-input v-model="form.zone_key" placeholder="如：2500" />
        </el-form-item>
        <el-form-item label="web 端口">
          <el-input-number v-model="form.web_port" :min="1" :max="65535" :controls="false" style="width: 140px" />
          <span class="muted small ml8">面板端口（默认取现有最大 + 1）</span>
        </el-form-item>
        <el-form-item label="ctrl 端口">
          <el-input-number v-model="form.ctrl_port" :min="1" :max="65535" :controls="false" style="width: 140px" />
          <span class="muted small ml8">中控监听端口（默认取现有最大 + 1）</span>
        </el-form-item>
        <el-form-item label="备注">
          <el-input v-model="form.note" type="textarea" :rows="2" placeholder="选填，如：试点区专用" />
        </el-form-item>
      </el-form>
      <div v-if="dupPort.length" class="small warn-text">
        <div v-for="(d, i) in dupPort" :key="i">⚠ {{ d }}（可直接提交，由后端预检做最终判定）</div>
      </div>
      <div v-if="creating" class="muted small">正在预检并创建……</div>
      <div v-if="createErr" class="small error-text mt6">{{ createErr }}</div>
      <!-- 预检失败逐项展示（pass=false 红色） -->
      <div v-if="createChecks.length" class="checks mt10">
        <div class="mb6" :class="failCount ? 'error-text' : 'muted'">预检结果（{{ failCount }} 项未通过）：</div>
        <div v-for="(c, i) in createChecks" :key="i" class="check" :class="{ fail: !c.pass }">
          <span class="mono">{{ c.pass ? '✓' : '✗' }}</span>
          <b>{{ c.name }}</b>
          <span class="detail">{{ c.detail }}</span>
        </div>
      </div>
    </template>

    <!-- 第二步：创建成功 → 展示启动脚本（不自动启动） -->
    <template v-else>
      <div class="row">
        <el-tag type="success" effect="dark">创建成功</el-tag>
        <span class="mono">id: {{ createResult.id }}</span>
      </div>
      <div class="mt10 muted small">请确认无误后手动运行脚本启动（创建不自动启动）：</div>
      <div class="script mono">{{ createResult.start_script || '（后端未返回 start_script）' }}</div>
      <div v-for="(w, i) in createResult.warnings" :key="i" class="small warn-text mt6">⚠ {{ w }}</div>
    </template>

    <template #footer>
      <template v-if="!createResult">
        <el-button @click="createDlg = false">取消</el-button>
        <el-button type="primary" :loading="creating" @click="onCreate">预检并创建</el-button>
      </template>
      <el-button v-else type="primary" @click="createDlg = false">完成</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.mb { margin-bottom: 12px; }
.mt6 { margin-top: 6px; }
.mt10 { margin-top: 10px; }
.ml8 { margin-left: 8px; }
.center { text-align: center; }
.warn-text { color: var(--warn); }
.error-text { color: var(--danger); }
.mb6 { margin-bottom: 6px; }

/* 实例卡：自适应多列 */
.cards { display: grid; grid-template-columns: repeat(auto-fill, minmax(min(430px, 100%), 1fr)); gap: 12px; }
.inst {
  background: var(--panel); border: 1px solid var(--border); border-radius: var(--radius);
  padding: 12px 14px; display: flex; flex-direction: column; gap: 8px;
}
.kv-row { display: flex; align-items: center; gap: 8px; font-size: 12px; min-width: 0; }
.kv-row .k { flex: 0 0 48px; color: var(--text-dim); }
.kv-row .mono { flex: 1; min-width: 0; }
.ellipsis { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

.status { gap: 14px; row-gap: 6px; }
.st { display: inline-flex; align-items: center; gap: 6px; font-size: 12px; }

.checks { border-top: 1px solid var(--border); padding-top: 8px; }
.check { display: flex; align-items: baseline; gap: 8px; padding: 2px 0; font-size: 13px; }
.check.fail { color: var(--danger); }
.check .detail { color: var(--text-dim); word-break: break-all; }
.check.fail .detail { color: inherit; opacity: .82; }

.script {
  background: var(--bg); border: 1px solid var(--border); border-radius: 8px;
  padding: 8px 10px; margin-top: 6px; word-break: break-all; user-select: all;
}
</style>
