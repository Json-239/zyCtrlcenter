<script setup>
// 「任务」页：定时自动任务（新手链 / 抓鬼 两套独立策略）+ 手动启动固定任务 + 卡死待恢复 + 链数据（高级）。
//
// 设计口径（2026-09-21，对齐参考实现 robot-ctrl-web 的「任务编排」页）：
//   - 用户面对的是**固定任务**（新手链 / 抓鬼），不需要理解链数据文件；
//   - 自动任务：每轮在"基础间隔 + 随机抖动"后随机挑 N 个"该做但没在做"的号，上线并下发任务；
//     没号可拉且开了自动注册 → 注册新号；同时在线上限到了就等空槽；
//   - 「链数据（高级）」折叠里保留原来的链文件/模块视图（排障用，平时不用看）。
//
// 2026-09-22 UI 改 Element Plus：两块参数改成 el-form + el-input-number/el-switch；
// 同时补一个"编辑态"闸门——轮询（4s）回显参数时跳过用户正在改的那一块，
// 否则每 4 秒会把刚填的数字顶回服务端旧值（原来的 <input> 也有这个毛病）。
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { apiGet } from '../api'
import { post, state } from '../store'
import ChainView from './ChainView.vue'

const err = ref('')
const tasks = ref([])       // 两套策略的运行态
const cands = ref({})       // 候选数
const reghost = ref([])     // 待恢复（卡死自动重登）
const pools = ref({})       // 号池分区：新手池 / 抓鬼池（可用数 + 在跑数 + 目标/缺口）
const busy = ref('')
const dirty = reactive({})  // kind -> 用户改过参数还没"启动/应用"（轮询不回显它）
const srv = reactive({})    // kind -> 服务端最近一次参数快照（供"还原"）

// 每套策略的面板参数（分钟制，和参考实现一致）；启动时换算成秒
// 默认按需求给一份"保持数"：新手保持有号在跑（默认 3）、抓鬼保持 50 个
const form = reactive({
  newbie: { intervalMin: 5, jitterMin: 5, batchMin: 1, batchMax: 3, targetOnline: 3, maxOnline: 0, registerEnabled: false, registerCount: 10 },
  ghost: { intervalMin: 5, jitterMin: 5, batchMin: 1, batchMax: 5, targetOnline: 50, maxOnline: 0, registerEnabled: false, registerCount: 10 },
})
const KIND_LABEL = { newbie: '🆕 新手链', ghost: '👻 抓鬼' }
const KIND_HINT = {
  newbie: '把没毕业的号拉去跑新手链；没号可拉且开了自动注册 → 注册新号',
  ghost: '把已毕业（≥31 级或新手链完成）的号拉去钟馗抓鬼；低于门槛的号会被自动跳过',
}

const chainReady = computed(() => {
  const r = state.status?.chains
  return r || 0
})

// 服务端参数 → 表单，并留一份快照（"还原"按钮用）
function syncForm(t) {
  const f = form[t.kind]
  if (!f) return
  const c = t.config || {}
  f.intervalMin = Math.max(1, Math.round((c.interval_sec || 300) / 60))
  f.jitterMin = Math.round((c.jitter_sec || 0) / 60)
  f.batchMin = c.batch_min || 1
  f.batchMax = c.batch_max || 3
  f.targetOnline = c.target_online || 0
  f.maxOnline = c.max_online || 0
  f.registerEnabled = !!c.register_enabled
  f.registerCount = c.register_count || 10
  srv[t.kind] = { ...f }
}

async function load() {
  try {
    const res = await apiGet('/api/autotask')
    tasks.value = res.tasks || []
    cands.value = res.candidates || {}
    reghost.value = res.reghost || []
    pools.value = res.pools || {}
    err.value = ''
    // 回显已启动策略的参数（改完再点启动会覆盖）；用户正在编辑的那块跳过，避免被轮询顶掉
    for (const t of tasks.value) {
      if (dirty[t.kind]) continue
      syncForm(t)
    }
  } catch (e) {
    err.value = e.message
  }
}
onMounted(() => {
  load()
  timer = setInterval(load, 4000) // 倒计时/候选数/待恢复都要新鲜
})
let timer = 0
onUnmounted(() => clearInterval(timer))

function markDirty(kind) { dirty[kind] = true }
// 还原成服务端最近一次参数（放弃未应用的改动）
function resetKind(kind) {
  if (srv[kind]) Object.assign(form[kind], srv[kind])
  dirty[kind] = false
}

function stateOf(kind) {
  return tasks.value.find((t) => t.kind === kind) || null
}
function running(kind) {
  return !!stateOf(kind)?.enabled
}
function nextIn(kind) {
  const t = stateOf(kind)
  if (!t || !t.enabled || !t.next_at) return '--'
  const s = Math.max(0, Math.round(new Date(t.next_at).getTime() / 1000 - Date.now() / 1000))
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`
}
function lastMsg(kind) {
  const t = stateOf(kind)
  if (!t) return '--'
  const n = cands.value[kind] ?? 0
  const tg = t.target > 0 ? `在跑 ${t.online || 0} / 目标 ${t.target}${t.deficit > 0 ? `（差 ${t.deficit}）` : '（已达标）'}` : `在跑 ${t.online || 0}`
  return `${t.enabled ? '⏳ 运行中' : '已停止'} · ${tg} · 可拉 ${n} 个 · 累计拉起 ${t.picked || 0}${t.registered ? ` · 注册 ${t.registered}` : ''}`
}
// 号池分区文案：可用数（在跑 / 目标）
function poolText(kind) {
  const p = pools.value[kind]
  if (!p) return '--'
  const tg = p.target > 0 ? ` / 目标 ${p.target}` : ''
  return `可用 ${p.usable} · 在跑 ${p.running}${tg}`
}
function fmtClock(ts) {
  return ts ? new Date(ts).toLocaleTimeString('zh-CN', { hour12: false }) : '--'
}
function phaseTag(p) {
  if (p === 'failed') return 'danger'
  if (p === 'done') return 'success'
  return 'warning'
}

async function startKind(kind) {
  const f = form[kind]
  if (f.batchMin > f.batchMax) {
    ElMessage.warning('「每轮个数」的下限不能大于上限')
    return
  }
  busy.value = kind
  try {
    const res = await post('/api/autotask/start', {
      kind,
      interval_sec: Math.max(1, Math.round(f.intervalMin * 60)),
      jitter_sec: Math.max(0, Math.round(f.jitterMin * 60)),
      batch_min: f.batchMin, batch_max: f.batchMax, target_online: f.targetOnline, max_online: f.maxOnline,
      register_enabled: f.registerEnabled, register_count: f.registerCount,
    })
    if (res.ok !== false) dirty[kind] = false // 下发成功：表单与服务端一致了
    await load()
  } finally {
    busy.value = ''
  }
}
async function stopKind(kind) {
  try {
    await ElMessageBox.confirm(
      `停止「${KIND_LABEL[kind]}」的定时任务？停止后不再自动拉号补位；已经在跑的号不受影响。`,
      '停止定时任务',
      { type: 'warning', confirmButtonText: '停止', cancelButtonText: '取消', confirmButtonClass: 'el-button--danger' },
    )
  } catch (e) {
    return // 用户取消
  }
  busy.value = kind
  try {
    await post('/api/autotask/stop', { kind })
    await load()
  } finally {
    busy.value = ''
  }
}
async function runKind(kind) {
  busy.value = kind
  try {
    await post('/api/autotask/run', { kind })
    await load()
  } finally {
    busy.value = ''
  }
}

// 手动启动固定任务（走「自动分配」：按每个号的意图决定发什么命令）
const manualAccounts = ref('')
async function startFixed(kind) {
  const body = { auto: true, accounts: manualAccounts.value.split(/[,，;\s]+/).filter(Boolean) }
  if (!body.accounts.length) delete body.accounts
  if (kind === 'newbie') body.chain_id = 'newbie_full' // 自动分配会用链数据文件；无意图号回落它
  busy.value = 'fixed'
  try {
    await post('/api/start', body)
    await load()
  } finally {
    busy.value = ''
  }
}
async function cancelRegHost(acc) {
  await post('/api/reghost/cancel', { account: acc })
  await load()
}
</script>

<template>
  <el-alert v-if="err" class="mb" type="error" :closable="false" show-icon :title="err" />

  <!-- 定时自动任务：两套独立策略 -->
  <div class="card">
    <div class="row">
      <h3 style="margin: 0">定时自动任务</h3>
      <span class="muted">按<b>保持在线数</b>往对应号池里拉号：每轮在「间隔 + 随机」后只补差额（<b>新手池 → 新手链，抓鬼池 → 抓鬼，互相独立</b>）</span>
      <span class="spacer" />
      <el-tooltip content="链目录里可用的链数据文件数（自动任务用到 newbie_full / zhongkui_nav）" placement="top">
        <el-tag size="small" :type="chainReady ? 'success' : 'warning'" effect="plain">链数据 {{ chainReady }} 个</el-tag>
      </el-tooltip>
      <el-button size="small" @click="load">
        <el-icon><Refresh /></el-icon>
        <span>刷新</span>
      </el-button>
    </div>

    <div v-for="kind in ['newbie', 'ghost']" :key="kind" class="ob-block">
      <div class="row">
        <b>{{ KIND_LABEL[kind] }}</b>
        <el-tag size="small" :type="running(kind) ? 'success' : 'info'" :effect="running(kind) ? 'dark' : 'plain'">
          {{ running(kind) ? '运行中' : '已停止' }}
        </el-tag>
        <span class="muted">{{ lastMsg(kind) }}</span>
        <span v-if="running(kind)" class="mono muted">下一个 {{ nextIn(kind) }}</span>
        <span class="spacer" />
        <el-tag v-if="dirty[kind]" size="small" type="warning" effect="plain">参数已改，未应用</el-tag>
        <el-button v-if="dirty[kind]" size="small" type="primary" link @click="resetKind(kind)">还原</el-button>
        <el-button size="small" type="primary" :disabled="busy === kind" @click="startKind(kind)">
          {{ running(kind) ? '应用参数并重启' : '启动' }}
        </el-button>
        <el-button size="small" :disabled="busy === kind || !running(kind)" @click="runKind(kind)">立即跑一轮</el-button>
        <el-button size="small" type="danger" plain :disabled="busy === kind || !running(kind)" @click="stopKind(kind)">停止</el-button>
      </div>
      <div class="row muted" style="font-size: 12.5px; margin-top: 4px">{{ KIND_HINT[kind] }}</div>
      <div class="row" style="margin-top: 6px">
        <el-tag size="small" :type="kind === 'newbie' ? 'primary' : 'warning'" effect="plain">
          {{ kind === 'newbie' ? '新手号池' : '抓鬼号池' }}
        </el-tag>
        <span class="muted">{{ poolText(kind) }}</span>
        <span class="muted">（每轮只补差额，不重复拉起；没有可拉的号才会走自动注册）</span>
      </div>

      <el-form class="cfg-grid" :model="form[kind]" size="small" label-width="86px" @submit.prevent>
        <el-form-item label="间隔(分钟)">
          <el-input-number v-model="form[kind].intervalMin" :min="1" :max="120" :step="1"
                           style="width: 132px" @change="markDirty(kind)" />
        </el-form-item>
        <el-form-item label="随机(分钟)">
          <el-input-number v-model="form[kind].jitterMin" :min="0" :max="120" :step="1"
                           style="width: 132px" @change="markDirty(kind)" />
        </el-form-item>
        <el-form-item label="每轮个数">
          <div class="row" style="gap: 6px; flex-wrap: nowrap">
            <el-input-number v-model="form[kind].batchMin" :min="1" :max="50" style="width: 100px" @change="markDirty(kind)" />
            <span class="muted">~</span>
            <el-input-number v-model="form[kind].batchMax" :min="1" :max="50" style="width: 100px" @change="markDirty(kind)" />
            <span class="muted">个</span>
          </div>
        </el-form-item>
        <el-form-item label="保持在线">
          <div class="row" style="gap: 6px; flex-wrap: nowrap">
            <el-input-number v-model="form[kind].targetOnline" :min="0" :max="2000" style="width: 132px" @change="markDirty(kind)" />
            <span class="muted">个(0=不限)</span>
          </div>
        </el-form-item>
        <el-form-item label="硬上限">
          <div class="row" style="gap: 6px; flex-wrap: nowrap">
            <el-input-number v-model="form[kind].maxOnline" :min="0" :max="2000" style="width: 132px" @change="markDirty(kind)" />
            <span class="muted">个(0=不限)</span>
          </div>
        </el-form-item>
        <el-form-item v-if="kind === 'newbie'" label="自动注册">
          <div class="row" style="gap: 8px; flex-wrap: nowrap">
            <el-switch v-model="form[kind].registerEnabled" @change="markDirty(kind)" />
            <template v-if="form[kind].registerEnabled">
              <el-input-number v-model="form[kind].registerCount" :min="1" :max="20" style="width: 96px" @change="markDirty(kind)" />
              <span class="muted">个/次</span>
            </template>
            <span v-else class="muted">没号可拉时注册新号</span>
          </div>
        </el-form-item>
      </el-form>

      <el-collapse v-if="stateOf(kind)?.rounds?.length" class="rounds">
        <el-collapse-item :title="`最近 ${Math.min(5, stateOf(kind).rounds.length)} 轮`" :name="kind">
          <el-timeline>
            <el-timeline-item
              v-for="(rd, i) in stateOf(kind).rounds.slice(0, 5)" :key="i"
              :timestamp="fmtClock(rd.at)" placement="top"
            >
              <span class="muted">{{ rd.msg }}</span>
            </el-timeline-item>
          </el-timeline>
        </el-collapse-item>
      </el-collapse>
    </div>
  </div>

  <!-- 手动启动固定任务 + 卡死待恢复 -->
  <div class="card">
    <div class="row">
      <h3 style="margin: 0">手动启动（固定任务）</h3>
      <el-input v-model="manualAccounts" size="small" clearable style="min-width: 320px; max-width: 520px"
                placeholder="账号（留空 = 全部：按每个号的意图自动分配）" />
      <el-button size="small" type="primary" :disabled="busy === 'fixed'" @click="startFixed('newbie')">按意图启动</el-button>
      <span class="muted">「按意图启动」= 新手链号发 start_chain、抓鬼号发 ghost_start（带导航数据）；低于抓鬼门槛的号会被跳过并给出原因</span>
    </div>

    <div v-if="reghost.length" style="margin-top: 10px">
      <div class="row" style="margin-bottom: 6px">
        <b>待恢复（钟馗对话卡死 → 自动重登后重新下发）</b>
        <el-tag size="small" type="warning" effect="plain">{{ reghost.length }} 个</el-tag>
      </div>
      <el-table :data="reghost" size="small" style="width: 100%">
        <el-table-column label="账号" width="190">
          <template #default="{ row }"><span class="mono">{{ row.account }}</span></template>
        </el-table-column>
        <el-table-column label="阶段" width="110">
          <template #default="{ row }"><el-tag size="small" :type="phaseTag(row.phase)" effect="plain">{{ row.phase }}</el-tag></template>
        </el-table-column>
        <el-table-column prop="attempts" label="次数" width="70" />
        <el-table-column label="说明" min-width="220" show-overflow-tooltip>
          <template #default="{ row }"><span class="muted">{{ row.last_msg }}</span></template>
        </el-table-column>
        <el-table-column label="动作" width="110" align="right">
          <template #default="{ row }">
            <el-button link type="danger" size="small" @click="cancelRegHost(row.account)">取消恢复</el-button>
          </template>
        </el-table-column>
      </el-table>
    </div>
  </div>

  <!-- 链数据（高级）：排障用，平时不用看 -->
  <div class="card">
    <details>
      <summary style="cursor: pointer"><b>链数据 / 链路工具（高级，一般不用动）</b>
        <span class="muted">文件驱动：链数据是"提示表"，中控只搬运；抓鬼的导航数据由中控组装（基座链 + 抓鬼专属）</span>
      </summary>
      <div style="margin-top: 10px"><ChainView /></div>
    </details>
  </div>
</template>

<style scoped>
/* 原有类名保留；变量名对齐 styles.css 的真实定义（--panel2/--line 是不存在的旧名，等于没生效） */
.ob-block { background: var(--panel-2); border: 1px solid var(--border); border-radius: 10px; padding: 12px 14px; margin-top: 10px; }
.ob-block:first-of-type { margin-top: 0; }
/* 参数区：固定 label 宽度的两列以上网格（窄屏自动折行），比一行 flex 更好对齐 */
.cfg-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(262px, 1fr)); gap: 0 12px; margin-top: 8px; }
.cfg-grid :deep(.el-form-item) { margin-bottom: 6px; }
.cfg-grid :deep(.el-form-item__label) { padding-right: 8px; }
.mb { margin-bottom: 12px; }
</style>
