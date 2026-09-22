<script setup>
import { computed, onMounted, ref, watch } from 'vue'
import { apiGet } from '../api'
import { post } from '../store'

const list = ref([])
const chainDir = ref('')
const detail = ref(null)
const plan = ref(null)      // 模块化视图（组装出的链路）
const planErr = ref('')     // 结构问题（未知执行器 / 残缺后继…）
const found = ref(false)
const note = ref('')
const err = ref('')
const chainId = ref('')
const accountsText = ref('')
const keyword = ref('')
const warnings = ref([]) // /api/start 回带的"条件不匹配"提示（如该走抓鬼却选了剧情链）

// 「自动分配」是下拉里的哨兵值（不是链 id）：交给后端按意图分组下发
const AUTO = '__auto__'
const current = computed(() => list.value.find((c) => c.id === chainId.value) || null)
// 导航数据（没有任务节点的文件，如 zhongkui_nav）：**可以在这里查看**，但不能当任务链启动
const navOnly = computed(() => !!current.value?.nav_only)

async function loadList() {
  try {
    const res = await apiGet('/api/chains')
    list.value = res.chains || []
    chainDir.value = res.chain_dir || ''
    if (!chainId.value && list.value.length) chainId.value = AUTO // 默认「自动分配」：按账号意图决定跑哪条（抓鬼走 ghost_start）
  } catch (e) {
    err.value = e.message
  }
}

async function loadDetail() {
  if (!chainId.value || chainId.value === AUTO) {
    detail.value = null; plan.value = null; found.value = false; note.value = ''
    return
  }
  try {
    const res = await apiGet(`/api/chains?id=${encodeURIComponent(chainId.value)}`)
    detail.value = res.chain || null
    plan.value = res.plan || null
    planErr.value = res.plan_error || ''
    found.value = !!res.chain_found
    note.value = res.msg || ''
  } catch (e) {
    err.value = e.message
  }
}

onMounted(async () => { await loadList(); await loadDetail() })
watch(chainId, loadDetail)

// 模块视图：只显示有步骤的任务（keyword 与节点表共用）
const planTasks = computed(() => {
  const items = plan.value?.tasks || []
  const kw = keyword.value
  const filtered = kw
    ? items.filter((t) => String(t.task_index).includes(kw) || (t.name || '').includes(kw))
    : items
  return filtered.slice(0, 60)
})
const planWarnings = computed(() => plan.value?.warnings || [])
const planModules = computed(() => {
  const m = plan.value?.modules || {}
  return Object.entries(m).sort((a, b) => b[1] - a[1])
})
// 模块 → 中文名/颜色（与 internal/chainplan 的模块全集一致）
const MODULE_LABEL = {
  pathfind: '寻路', move: '移动', cross_map: '跨图', talk: '对话',
  buy: '购买', fight: '战斗', alloc: '加点', restore: '状态补充', wait: '等下一环',
}
function modLabel(m) { return MODULE_LABEL[m] || m }
function modLevel(m) {
  return { talk: 'ok', fight: 'danger', buy: 'warn', restore: 'warn', wait: 'dim' }[m] || 'dim'
}
function stepTitle(s) {
  const args = s.args ? JSON.stringify(s.args) : ''
  return `${modLabel(s.module)} · ${s.source}${s.note ? ' · ' + s.note : ''}${args ? '\n' + args : ''}`
}

const nodes = computed(() => {
  const raw = detail.value?.task_order || []
  const list = raw.map((n, i) => normalizeNode(n, i))
  if (!keyword.value) return list.slice(0, 100)
  const kw = keyword.value
  return list.filter((n) => String(n.index).includes(kw) || n.name.includes(kw)).slice(0, 200)
})

// 节点形状容错：对象按常见字段渲染，非对象直接原样展示
function normalizeNode(n, i) {
  if (n && typeof n === 'object' && !Array.isArray(n)) {
    return {
      index: n.task_index ?? n.index ?? i + 1,
      name: n.name ?? n.task_name ?? '',
      npc: n.catcher_npc ?? n.npc ?? '',
      next: Array.isArray(n.next) ? n.next.join(', ') : (n.next ?? ''),
      raw: JSON.stringify(n),
    }
  }
  return { index: i + 1, name: '', npc: '', next: '', raw: JSON.stringify(n) }
}

const specialCount = computed(() => Object.keys(detail.value?.task_hints || {}).length)
const mapCount = computed(() => Object.keys(detail.value?.maps || {}).length)
const npcCount = computed(() => Object.keys(detail.value?.npcs || {}).length)

function accountList() {
  return accountsText.value.split(/[,，;\s]+/).filter(Boolean)
}

function cmdBody() {
  const body = {}
  const accounts = accountList()
  if (accounts.length) body.accounts = accounts
  return body
}

async function start() {
  const body = cmdBody()
  warnings.value = []
  if (chainId.value === AUTO) {
    // 自动分配：不手选链，后端按每个号的意图分组（新手链 → start_chain；抓鬼 → ghost_start 带导航数据）
    body.auto = true
  } else if (chainId.value) {
    body.chain_id = chainId.value
  }
  const res = await post('/api/start', body)
  // 后端会回带"该号其实应该走抓鬼"这类提示（msg 已由 post 弹 toast，这里再列出来便于细看）
  warnings.value = Array.isArray(res?.warnings) ? res.warnings : []
}
async function stop() { await post('/api/stop', cmdBody()) }
async function reset() { await post('/api/reset', cmdBody()) }
</script>

<template>
  <div v-if="err" class="card"><span class="tag danger">{{ err }}</span></div>

  <div class="card">
    <h3>启动任务链</h3>
    <div class="row">
      <select v-model="chainId" style="min-width: 300px">
        <option :value="AUTO">自动分配（按意图：等级 &lt;31 新手链 / 其余抓鬼）</option>
        <option value="">（不指定链，仅下发 chain_id）</option>
        <option v-for="c in list" :key="c.id" :value="c.id">
          {{ c.name || c.chain_id || c.id }}（{{ c.nav_only ? '导航数据，不可直接启动' : c.task_count + ' 节点' }}）
        </option>
      </select>
      <input v-model="accountsText" type="text" placeholder="账号（空=全部；多个用逗号分隔）" style="width: 380px" />
      <button
        class="btn primary"
        :disabled="navOnly"
        :title="navOnly ? '这是导航数据（没有任务节点），不能作为任务链启动；抓鬼请选「自动分配」' : ''"
        @click="start"
      >启动链</button>
      <button class="btn" @click="stop">停链</button>
      <button class="btn danger" @click="reset">重置重跑</button>
      <button class="btn ghost" @click="loadList(); loadDetail()">刷新</button>
    </div>
    <p v-if="navOnly" class="muted" style="margin: 10px 0 0">
      <span class="tag warn">导航数据</span>
      <span class="mono">{{ chainId }}</span> 是抓鬼导航数据（没有任务节点）：可以在这里查看内容，但
      <b>不能作为任务链启动</b> —— 抓鬼请选「自动分配」（后端会下发
      <span class="mono">ghost_start</span> 并带上这份导航数据）。
    </p>
    <p v-for="w in warnings" :key="w.account" class="muted" style="margin: 10px 0 0">
      <span class="tag warn">提示</span> {{ w.msg }}
    </p>
    <p class="muted" style="margin: 10px 0 0">
      链数据文件驱动：把 <span class="mono">&lt;chain_id&gt;.json</span> 放进链目录即会出现在列表；
      文件内容由中控<b>原样透传</b>给机器人（未声明字段与字段形状都不改）。
      未找到文件时只下发 chain_id，由机器人端决定怎么跑。
    </p>
    <p class="muted mono" style="margin: 6px 0 0">链目录：{{ chainDir || '(未知)' }}</p>
  </div>

  <div class="card">
    <div class="row" style="margin-bottom: 8px">
      <span class="tag" :class="found ? 'ok' : 'warn'">{{ found ? '已找到链数据' : '无链数据文件' }}</span>
      <span class="mono muted">{{ chainId || '（未选择）' }}</span>
      <span v-if="note" class="muted">{{ note }}</span>
    </div>

    <div class="grid cols-3" v-if="detail">
      <div class="stat">
        <div class="label">链 ID</div>
        <div class="value" style="font-size:18px">{{ detail.chain_id }}</div>
        <div class="sub">{{ detail.start_task ?? '--' }} → {{ detail.end_task ?? '--' }}</div>
      </div>
      <div class="stat">
        <div class="label">任务节点</div>
        <div class="value">{{ (detail.task_order || []).length }}</div>
        <div class="sub">特殊任务 {{ specialCount }} 个</div>
      </div>
      <div class="stat">
        <div class="label">地图 / NPC</div>
        <div class="value">{{ mapCount }} / {{ npcCount }}</div>
        <div class="sub">grid_cell {{ detail.grid_cell ?? '--' }}</div>
      </div>
    </div>

    <div class="table-wrap" style="max-height: 46vh; margin-top: 12px">
      <table>
        <thead><tr><th style="width:90px">序号</th><th style="width:140px">任务号</th><th>名称</th><th style="width:140px">接取 NPC</th><th style="width:200px">后续任务</th></tr></thead>
        <tbody>
          <tr v-for="(n, i) in nodes" :key="i" :title="n.raw">
            <td class="muted">{{ i + 1 }}</td>
            <td class="mono">{{ n.index }}</td>
            <td>{{ n.name || '--' }}</td>
            <td class="mono">{{ n.npc || '--' }}</td>
            <td class="mono muted">{{ n.next || '--' }}</td>
          </tr>
          <tr v-if="!nodes.length">
            <td colspan="5">
              <div class="empty">
                {{ found ? '该链没有任务节点数据' : '还没有链数据文件：把 <chain_id>.json 放进上面的链目录即可' }}
                <div v-if="!found" style="margin-top:10px">
                  <button class="btn sm" @click="chainId = '_example'">看示例模板（_example）</button>
                  <span class="muted"> —— 模板里有一条任务节点 + 购买提示，能直接看到「模块视图」长什么样</span>
                </div>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <div class="row" style="margin-top: 8px" v-if="detail && (detail.task_order || []).length > 100">
      <input v-model="keyword" type="text" placeholder="搜索任务号或名称" style="width: 240px" />
      <span class="muted">节点较多，默认只显示前 100 个</span>
    </div>
  </div>

  <!-- 模块视图：中控不执行链，这里把提示表展开成"由哪些模块组成"，并提前暴露结构问题 -->
  <div class="card" v-if="plan">
    <div class="row" style="margin-bottom:8px">
      <h3 style="margin:0">模块视图（组装出的链路）</h3>
      <span class="muted">寻路 → 移动 →（跨图）→ 战斗 → 状态补充 → 对话 → 购买/加点 → 等下一环</span>
      <span class="spacer" />
      <span v-if="plan.active_accept" class="tag warn">允许主动接取</span>
      <span class="muted">任务 {{ planTasks.length }} 个</span>
    </div>

    <div class="row" style="margin-bottom:6px" v-if="planModules.length">
      <span class="muted">模块用量</span>
      <span v-for="[m, n] in planModules" :key="m" class="tag" :class="modLevel(m)">{{ modLabel(m) }} ×{{ n }}</span>
    </div>

    <div class="row" v-if="planErr" style="margin-bottom:6px">
      <span class="tag danger">结构问题</span>
      <span class="warnText">{{ planErr }}</span>
    </div>
    <div class="row muted" v-if="planWarnings.length" style="margin-bottom:6px; display:block">
      <div v-for="(w, i) in planWarnings" :key="i">· {{ w }}</div>
    </div>

    <div class="table-wrap" style="max-height: 42vh">
      <table>
        <thead><tr><th style="width:140px">任务号</th><th style="width:160px">名称</th><th>模块序列</th></tr></thead>
        <tbody>
          <tr v-for="t in planTasks" :key="t.task_index">
            <td class="mono">{{ t.task_index }}</td>
            <td>{{ t.name || '--' }}</td>
            <td>
              <span v-for="s in t.steps" :key="s.order" class="tag" :class="modLevel(s.module)"
                    :title="stepTitle(s)" style="margin-right:4px">{{ modLabel(s.module) }}</span>
            </td>
          </tr>
          <tr v-if="!planTasks.length">
            <td colspan="3">
              <div class="empty">
                {{ (plan.tasks || []).length ? '没有匹配的任务' : '这条链没有任务节点（task_order 为空）：只下发 chain_id，由机器人端自行决定怎么跑' }}
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <p class="muted" style="margin:8px 0 0">
      <b>这是骨架 / 排障视图，不是固定脚本</b>：实际推进由服务端推送驱动，抓鬼/捉鬼要自己寻路去找
      任务给予者 NPC 接取，刷鬼点与任务怪目标都是服务端动态给的（每次跑不一样）。
      模块拆分只为"卡住时能一眼看出卡在哪个模块"——
      <span class="mono">状态补充 / 等下一环</span> 标 <b>policy</b>（机器人端策略）、<span class="mono">寻路/移动/对话</span> 标 <b>derived</b>（按 npcs/dijkstra 推导），都不代表真实时序。
    </p>
  </div>
</template>
