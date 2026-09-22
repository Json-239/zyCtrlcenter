<script setup>
// 「任务」页：定时自动任务（新手链 / 抓鬼 两套独立策略）+ 手动启动固定任务 + 卡死待恢复 + 链数据（高级）。
//
// 设计口径（2026-09-21，对齐参考实现 robot-ctrl-web 的「任务编排」页）：
//   - 用户面对的是**固定任务**（新手链 / 抓鬼），不需要理解链数据文件；
//   - 自动任务：每轮在"基础间隔 + 随机抖动"后随机挑 N 个"该做但没在做"的号，上线并下发任务；
//     没号可拉且开了自动注册 → 注册新号；同时在线上限到了就等空槽；
//   - 「链数据（高级）」折叠里保留原来的链文件/模块视图（排障用，平时不用看）。
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { apiGet } from '../api'
import { post, state } from '../store'
import ChainView from './ChainView.vue'

const err = ref('')
const tasks = ref([])       // 两套策略的运行态
const cands = ref({})       // 候选数
const reghost = ref([])     // 待恢复（卡死自动重登）
const pools = ref({})       // 号池分区：新手池 / 抓鬼池（可用数 + 在跑数 + 目标/缺口）
const busy = ref('')
const editing = reactive({}) // kind -> 参数编辑区（只在展开时用）

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

async function load() {
  try {
    const res = await apiGet('/api/autotask')
    tasks.value = res.tasks || []
    cands.value = res.candidates || {}
    reghost.value = res.reghost || []
    pools.value = res.pools || {}
    err.value = ''
    // 回显已启动策略的参数（改完再点启动会覆盖）
    for (const t of tasks.value) {
      const f = form[t.kind]
      if (!f) continue
      const c = t.config || {}
      f.intervalMin = Math.max(1, Math.round((c.interval_sec || 300) / 60))
      f.jitterMin = Math.round((c.jitter_sec || 0) / 60)
      f.batchMin = c.batch_min || 1
      f.batchMax = c.batch_max || 3
      f.targetOnline = c.target_online || 0
      f.maxOnline = c.max_online || 0
      f.registerEnabled = !!c.register_enabled
      f.registerCount = c.register_count || 10
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

async function startKind(kind) {
  const f = form[kind]
  busy.value = kind
  try {
    await post('/api/autotask/start', {
      kind,
      interval_sec: Math.max(1, Math.round(f.intervalMin * 60)),
      jitter_sec: Math.max(0, Math.round(f.jitterMin * 60)),
      batch_min: f.batchMin, batch_max: f.batchMax, target_online: f.targetOnline, max_online: f.maxOnline,
      register_enabled: f.registerEnabled, register_count: f.registerCount,
    })
    await load()
  } finally {
    busy.value = ''
  }
}
async function stopKind(kind) {
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
  <div v-if="err" class="card"><span class="tag danger">{{ err }}</span></div>

  <!-- 定时自动任务：两套独立策略 -->
  <div class="card">
    <div class="row">
      <h3 style="margin: 0">定时自动任务</h3>
      <span class="muted">按<b>保持在线数</b>往对应号池里拉号：每轮在「间隔 + 随机」后只补差额（<b>新手池 → 新手链，抓鬼池 → 抓鬼，互相独立</b>）</span>
      <span class="spacer" />
      <span class="tag" :class="chainReady ? 'ok' : 'warn'" :title="'链目录里可用的链数据文件数（自动任务用到 newbie_full / zhongkui_nav）'">
        链数据 {{ chainReady }} 个
      </span>
      <button class="btn sm" @click="load">刷新</button>
    </div>

    <div v-for="kind in ['newbie', 'ghost']" :key="kind" class="ob-block">
      <div class="row">
        <b>{{ KIND_LABEL[kind] }}</b>
        <span class="tag" :class="running(kind) ? 'ok' : 'dim'">{{ running(kind) ? '运行中' : '已停止' }}</span>
        <span class="muted">{{ lastMsg(kind) }}</span>
        <span v-if="running(kind)" class="mono muted">下一个 {{ nextIn(kind) }}</span>
        <span class="spacer" />
        <button class="btn sm primary" :disabled="busy === kind" @click="startKind(kind)">{{ running(kind) ? '应用参数并重启' : '启动' }}</button>
        <button class="btn sm" :disabled="busy === kind || !running(kind)" @click="runKind(kind)">立即跑一轮</button>
        <button class="btn sm danger" :disabled="busy === kind || !running(kind)" @click="stopKind(kind)">停止</button>
      </div>
      <div class="row muted" style="font-size: 12.5px; margin-top: 4px">{{ KIND_HINT[kind] }}</div>
      <div class="row" style="margin-top: 6px">
        <span class="tag" :class="kind === 'newbie' ? 'info' : 'warn'">{{ kind === 'newbie' ? '新手号池' : '抓鬼号池' }}</span>
        <span class="muted">{{ poolText(kind) }}</span>
        <span class="muted">（每轮只补差额，不重复拉起；没有可拉的号才会走自动注册）</span>
      </div>

      <div class="row" style="margin-top: 8px; gap: 10px; flex-wrap: wrap">
        <label class="muted">间隔(分钟)<input v-model.number="form[kind].intervalMin" type="number" min="1" max="120" style="width: 74px" /></label>
        <label class="muted">随机(分钟)<input v-model.number="form[kind].jitterMin" type="number" min="0" max="120" style="width: 74px" /></label>
        <label class="muted">每轮
          <input v-model.number="form[kind].batchMin" type="number" min="1" max="50" style="width: 56px" /> ~
          <input v-model.number="form[kind].batchMax" type="number" min="1" max="50" style="width: 56px" /> 个
        </label>
        <label class="muted"><b>保持在线</b><input v-model.number="form[kind].targetOnline" type="number" min="0" max="2000" style="width: 74px" /> 个(0=不限)</label>
        <label class="muted">硬上限<input v-model.number="form[kind].maxOnline" type="number" min="0" max="2000" style="width: 74px" /> 个(0=不限)</label>
        <template v-if="kind === 'newbie'">
          <label class="muted">
            <input v-model="form[kind].registerEnabled" type="checkbox" /> 没号时自动注册
          </label>
          <label v-if="form[kind].registerEnabled" class="muted">
            每次 <input v-model.number="form[kind].registerCount" type="number" min="1" max="20" style="width: 56px" /> 个
          </label>
        </template>
      </div>

      <details v-if="stateOf(kind)?.rounds?.length" style="margin-top: 8px">
        <summary class="muted" style="cursor: pointer">最近 {{ Math.min(5, stateOf(kind).rounds.length) }} 轮</summary>
        <div v-for="(rd, i) in stateOf(kind).rounds.slice(0, 5)" :key="i" class="muted" style="font-size: 12.5px; margin-top: 4px">
          <span class="mono">{{ new Date(rd.at).toLocaleTimeString('zh-CN', { hour12: false }) }}</span> · {{ rd.msg }}
        </div>
      </details>
    </div>
  </div>

  <!-- 手动启动固定任务 + 卡死待恢复 -->
  <div class="card">
    <div class="row">
      <h3 style="margin: 0">手动启动（固定任务）</h3>
      <input v-model="manualAccounts" placeholder="账号（留空 = 全部：按每个号的意图自动分配）" style="min-width: 320px" />
      <button class="btn primary" :disabled="busy === 'fixed'" @click="startFixed('newbie')">按意图启动</button>
      <span class="muted">「按意图启动」= 新手链号发 start_chain、抓鬼号发 ghost_start（带导航数据）；低于抓鬼门槛的号会被跳过并给出原因</span>
    </div>

    <div v-if="reghost.length" style="margin-top: 10px">
      <div class="row"><b>待恢复（钟馗对话卡死 → 自动重登后重新下发）</b><span class="muted">{{ reghost.length }} 个</span></div>
      <table>
        <thead><tr><th>账号</th><th style="width:110px">阶段</th><th style="width:70px">次数</th><th>说明</th><th style="width:90px">动作</th></tr></thead>
        <tbody>
          <tr v-for="r in reghost" :key="r.account">
            <td class="mono">{{ r.account }}</td>
            <td><span class="tag" :class="r.phase === 'failed' ? 'danger' : r.phase === 'done' ? 'ok' : 'warn'">{{ r.phase }}</span></td>
            <td>{{ r.attempts }}</td>
            <td class="muted">{{ r.last_msg }}</td>
            <td><button class="btn sm" @click="cancelRegHost(r.account)">取消恢复</button></td>
          </tr>
        </tbody>
      </table>
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
.ob-block { background: var(--panel2); border: 1px solid var(--line); border-radius: 10px; padding: 12px 14px; margin-top: 10px; }
.ob-block:first-of-type { margin-top: 0; }
label.muted { font-size: 12.5px; }
input[type='number'] { margin-left: 4px; }
</style>
