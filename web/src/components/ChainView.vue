<script setup>
// 「链数据」页：链文件列表 / 任务节点表 / 模块视图（组装出的链路）。
// 2026-09-22 UI 改 Element Plus：下拉与输入换组件、两个大表换 el-table（长文本溢出用 tooltip）、
// 模块用量/步骤标签换 el-tag；数据来源与交互（含"自动分配"哨兵值）保持不变。
import { computed, onMounted, ref, watch } from 'vue'
import { ElMessageBox } from 'element-plus'
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
const loading = ref(false)

// 「自动分配」是下拉里的哨兵值（不是链 id）：交给后端按意图分组下发
// （等级 <31 → 新手链；≥40 且新日常未满 → 大唐神捕/烽火大唐；其余 → 抓鬼）
const AUTO = '__auto__'
const current = computed(() => list.value.find((c) => c.id === chainId.value) || null)
// 导航数据（没有任务节点的文件，如 zhongkui_nav）：**可以在这里查看**，但不能当任务链启动
const navOnly = computed(() => !!current.value?.nav_only)

// 节点表空态文案（三种情况分开说，别让"自动分配"看起来像掉数据了）
const emptyText = computed(() => {
  if (chainId.value === AUTO) return '「自动分配」由后端按每个号的意图决定跑哪条链（<31 新手链 / ≥40 且新日常未满 → 大唐神捕·烽火大唐 / 其余抓鬼），这里没有单条链的节点表；选一条具体的链可以看节点。'
  if (found.value) return '该链没有任务节点数据'
  return '还没有链数据文件：把 <chain_id>.json 放进上面的链目录即可'
})

async function loadList() {
  loading.value = true
  try {
    const res = await apiGet('/api/chains')
    list.value = res.chains || []
    chainDir.value = res.chain_dir || ''
    // 默认「自动分配」：按账号意图决定跑哪条（<31 新手链；≥40 且新日常未满 → 大唐神捕/烽火大唐；其余抓鬼走 ghost_start）
    if (!chainId.value && list.value.length) chainId.value = AUTO
  } catch (e) {
    err.value = e.message
  } finally {
    loading.value = false
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
// 原有语义色类名 → el-tag 的 type（配色含义不变）
function tagType(cls) {
  return { ok: 'success', danger: 'danger', warn: 'warning', info: 'info', dim: 'info' }[cls] || 'info'
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
    // 自动分配：不手选链，后端按每个号的意图分组
    // （<31 新手链 → start_chain；≥40 且新日常未满 → 大唐神捕/烽火大唐；其余 → ghost_start 带导航数据）
    body.auto = true
  } else if (chainId.value) {
    body.chain_id = chainId.value
  }
  const res = await post('/api/start', body)
  // 后端会回带"该号其实应该走抓鬼"这类提示（msg 已由 post 弹 toast，这里再列出来便于细看）
  warnings.value = Array.isArray(res?.warnings) ? res.warnings : []
}
async function stop() { await post('/api/stop', cmdBody()) }
// 重置重跑：给账号下发 reset（机器人端会从头跑），属破坏性动作 → 二次确认
async function reset() {
  try {
    await ElMessageBox.confirm(
      '给所选账号下发「重置重跑」？该号当前的链路进度会被清掉、从头开始跑。',
      '重置重跑',
      { type: 'warning', confirmButtonText: '重置', cancelButtonText: '取消', confirmButtonClass: 'el-button--danger' },
    )
  } catch (e) {
    return // 用户取消
  }
  await post('/api/reset', cmdBody())
}
</script>

<template>
  <el-alert v-if="err" class="mb" type="error" :closable="false" show-icon :title="err" />

  <div class="card">
    <h3>启动任务链</h3>
    <div class="row">
      <el-select v-model="chainId" size="small" style="width: 340px" filterable>
        <el-option :value="AUTO" label="自动分配（按意图：等级 <31 新手链 / ≥40 且新日常未满 → 大唐神捕·烽火大唐 / 其余抓鬼）" />
        <el-option value="" label="（不指定链，仅下发 chain_id）" />
        <el-option
          v-for="c in list" :key="c.id" :value="c.id"
          :label="`${c.name || c.chain_id || c.id}（${c.nav_only ? '导航数据，不可直接启动' : c.task_count + ' 节点'}）`"
        />
      </el-select>
      <el-input v-model="accountsText" size="small" style="width: 380px" clearable
                placeholder="账号（空=全部；多个用逗号分隔）" />
      <el-button
        size="small" type="primary"
        :disabled="navOnly"
        :title="navOnly ? '这是导航数据（没有任务节点），不能作为任务链启动；抓鬼请选「自动分配」' : ''"
        @click="start"
      >启动链</el-button>
      <el-button size="small" @click="stop">停链</el-button>
      <el-button size="small" type="danger" plain @click="reset">重置重跑</el-button>
      <el-button size="small" @click="loadList(); loadDetail()">
        <el-icon><Refresh /></el-icon>
        <span>刷新</span>
      </el-button>
    </div>
    <el-alert v-if="navOnly" class="mt" type="warning" :closable="false" show-icon title="导航数据">
      <span class="mono">{{ chainId }}</span> 是抓鬼导航数据（没有任务节点）：可以在这里查看内容，但
      <b>不能作为任务链启动</b> —— 抓鬼请选「自动分配」（后端会下发
      <span class="mono">ghost_start</span> 并带上这份导航数据）。
    </el-alert>
    <el-alert v-for="w in warnings" :key="w.account" class="mt" type="warning" :closable="false" show-icon :title="w.msg" />
    <p class="muted" style="margin: 10px 0 0">
      链数据文件驱动：把 <span class="mono">&lt;chain_id&gt;.json</span> 放进链目录即会出现在列表；
      文件内容由中控<b>原样透传</b>给机器人（未声明字段与字段形状都不改）。
      未找到文件时只下发 chain_id，由机器人端决定怎么跑。
    </p>
    <p class="muted mono" style="margin: 6px 0 0">链目录：{{ chainDir || '(未知)' }}</p>
  </div>

  <div class="card">
    <div class="row" style="margin-bottom: 8px">
      <el-tag v-if="chainId === AUTO" size="small" type="info" effect="plain">自动分配（按意图）</el-tag>
      <el-tag v-else size="small" :type="found ? 'success' : 'warning'" effect="plain">{{ found ? '已找到链数据' : '无链数据文件' }}</el-tag>
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

    <el-table :data="nodes" v-loading="loading" size="small" max-height="46vh" style="width: 100%; margin-top: 12px">
      <el-table-column label="序号" width="70">
        <template #default="{ $index }"><span class="muted">{{ $index + 1 }}</span></template>
      </el-table-column>
      <el-table-column label="任务号" width="140">
        <template #default="{ row }"><span class="mono">{{ row.index }}</span></template>
      </el-table-column>
      <el-table-column label="名称" min-width="200" show-overflow-tooltip>
        <!-- 原始 JSON 仍挂在名称上（悬停可看），排障习惯不变 -->
        <template #default="{ row }"><span :title="row.raw">{{ row.name || '--' }}</span></template>
      </el-table-column>
      <el-table-column label="接取 NPC" width="150">
        <template #default="{ row }"><span class="mono">{{ row.npc || '--' }}</span></template>
      </el-table-column>
      <el-table-column label="后续任务" min-width="180" show-overflow-tooltip>
        <template #default="{ row }"><span class="mono muted">{{ row.next || '--' }}</span></template>
      </el-table-column>
      <template #empty>
        <el-empty :image-size="56" :description="emptyText">
          <template v-if="!found && chainId !== AUTO" #default>
            <el-button size="small" @click="chainId = '_example'">看示例模板（_example）</el-button>
            <div class="muted small" style="margin-top: 6px">
              模板里有一条任务节点 + 购买提示，能直接看到「模块视图」长什么样
            </div>
          </template>
        </el-empty>
      </template>
    </el-table>
    <div class="row" style="margin-top: 8px" v-if="detail && (detail.task_order || []).length > 100">
      <el-input v-model="keyword" size="small" style="width: 240px" clearable placeholder="搜索任务号或名称">
        <template #prefix><el-icon><Search /></el-icon></template>
      </el-input>
      <span class="muted">节点较多，默认只显示前 100 个</span>
    </div>
  </div>

  <!-- 模块视图：中控不执行链，这里把提示表展开成"由哪些模块组成"，并提前暴露结构问题 -->
  <div class="card" v-if="plan">
    <div class="row" style="margin-bottom:8px">
      <h3 style="margin:0">模块视图（组装出的链路）</h3>
      <span class="muted">寻路 → 移动 →（跨图）→ 战斗 → 状态补充 → 对话 → 购买/加点 → 等下一环</span>
      <span class="spacer" />
      <el-tag v-if="plan.active_accept" size="small" type="warning" effect="plain">允许主动接取</el-tag>
      <span class="muted">任务 {{ planTasks.length }} 个</span>
    </div>

    <div class="row" style="margin-bottom:6px" v-if="planModules.length">
      <span class="muted">模块用量</span>
      <el-tag v-for="[m, n] in planModules" :key="m" size="small" :type="tagType(modLevel(m))" effect="plain">
        {{ modLabel(m) }} ×{{ n }}
      </el-tag>
    </div>

    <el-alert v-if="planErr" class="mb" type="error" :closable="false" show-icon :title="`结构问题：${planErr}`" />
    <el-alert v-if="planWarnings.length" class="mb" type="warning" :closable="false" show-icon title="组装提示">
      <div v-for="(w, i) in planWarnings" :key="i" class="muted small">· {{ w }}</div>
    </el-alert>

    <el-table :data="planTasks" size="small" max-height="42vh" style="width: 100%">
      <el-table-column label="任务号" width="140">
        <template #default="{ row }"><span class="mono">{{ row.task_index }}</span></template>
      </el-table-column>
      <el-table-column label="名称" width="170" show-overflow-tooltip>
        <template #default="{ row }">{{ row.name || '--' }}</template>
      </el-table-column>
      <el-table-column label="模块序列" min-width="320">
        <template #default="{ row }">
          <el-tag
            v-for="s in row.steps" :key="s.order"
            size="small" :type="tagType(modLevel(s.module))" effect="plain"
            :title="stepTitle(s)" style="margin: 1px 4px 1px 0"
          >{{ modLabel(s.module) }}</el-tag>
        </template>
      </el-table-column>
      <template #empty>
        <el-empty :image-size="56"
                  :description="(plan.tasks || []).length ? '没有匹配的任务' : '这条链没有任务节点（task_order 为空）：只下发 chain_id，由机器人端自行决定怎么跑'" />
      </template>
    </el-table>
    <p class="muted" style="margin:8px 0 0">
      <b>这是骨架 / 排障视图，不是固定脚本</b>：实际推进由服务端推送驱动，抓鬼/捉鬼要自己寻路去找
      任务给予者 NPC 接取，刷鬼点与任务怪目标都是服务端动态给的（每次跑不一样）。
      模块拆分只为"卡住时能一眼看出卡在哪个模块"——
      <span class="mono">状态补充 / 等下一环</span> 标 <b>policy</b>（机器人端策略）、<span class="mono">寻路/移动/对话</span> 标 <b>derived</b>（按 npcs/dijkstra 推导），都不代表真实时序。
    </p>
  </div>
</template>

<style scoped>
.mb { margin-bottom: 8px; }
.mt { margin-top: 10px; }
/* 排障记录（2026-09-22）：全局规则 input[type="text"]{background:var(--bg)} 会命中 el-select 内部的
   搜索输入框，而 Element 的选中文本用的是 z-index:-1 的 .el-select__placeholder，
   结果 filterable 下拉的选中项被这个实心输入框盖住（看起来像"空的下拉"）。
   这里只把下拉内嵌输入框还原成透明（不写死颜色）；根治应在 styles.css 把那条全局规则排除 .el-*。 */
:deep(.el-select__input) { background: transparent; border: 0; padding: 0; }
</style>
