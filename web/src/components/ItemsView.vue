<script setup>
// 「物品配置」页（2026-09-29 用户需求：面板可以点击物品，看见当前这个物品是怎么实现使用的）。
//
// 数据源：GET /api/item_usage（只读；中控读 data/item_usage_catalog.json 原文透传）。
//   - 接口不存在（中控未重启加载新路由）→ 404 → 显示"需中控重启后生效"，不空白不崩页；
//   - 接口在但数据文件缺失/损坏（available=false）→ 显示中控给的 msg；
//   - web/dist 每次请求读盘 → 前端构建后刷新即见，无需重启中控。
//
// 页面结构：统计条 + 筛选（类别/状态/搜索）+ 物品表（点击行看详情）+ 右侧详情抽屉。
// 详情回答"这个物品是怎么实现使用的"：用途/服务端语义/实现点（文件:函数:行）/触发条件/
// 阈值与配置化/扣减口径 + 关联的 F1-FN 不一致项（按证据行号自动关联，同文件 ±3 行内）。
import { computed, onMounted, ref } from 'vue'
import { apiGet } from '../api'

const loading = ref(false)
const err = ref('')          // 接口不可用（404 / 网络错）
const needRestart = ref(false) // 404：新路由未加载 → 提示重启中控
const notReady = ref('')     // available=false 的说明（数据文件缺失/损坏）
const catalog = ref(null)    // catalog 原文（items/categories/findings/unimplemented/protocols…）

const NO_CAT = '__none__' // 「未分类」哨兵值（空串会被 el-select 当成"未选择"）
const search = ref('')
const catFilter = ref('')
const statusFilter = ref('')
const drawerOpen = ref(false)
const detail = ref(null)

const STATUS = {
  yes: { label: '已实现', type: 'success' },
  partial: { label: '半实现', type: 'warning' },
  no: { label: '未实现', type: 'danger' },
}
const SEV_TYPE = { 高: 'danger', 中: 'warning', 低: 'info' }

// 未实现项的状态写法是 "no" / "partial(无效使用)" → 归一成三态
function normStatus(s) {
  const t = String(s || '').toLowerCase()
  if (t.startsWith('partial')) return 'partial'
  if (t.startsWith('no')) return 'no'
  if (t.startsWith('yes')) return 'yes'
  return 'no'
}

const catLabels = computed(() => {
  const out = {}
  for (const c of catalog.value?.categories || []) out[c.key] = c.label
  return out
})

// 把"已实现条目 + 未实现清单"归一成同一张表的行（未实现项没有 sites，只有 expected/evidence）。
// 2026-09-29 catalog v2（item-usage-audit e7a94e1/10da2f6）：items 里也出现 implemented=no/partial 且
// sites 为空的条目；新增纯加法字段 expected/drop_policy/obtain/contents/note —— 一律可选展示（有才渲染）。
const rows = computed(() => {
  const items = (catalog.value?.items || []).map((it, i) => ({
    key: `it-${i}-${it.name}`,
    item_id: it.item_id,
    name: it.name,
    aliases: it.aliases || [],
    category: it.category || '',
    implemented: normStatus(it.implemented),
    sites: it.sites || [],
    server: it.server || null,
    expected: it.expected || '',       // 期望实现（未实现/半实现条目的说明）
    dropPolicy: it.drop_policy || '',  // 丢弃策略（如"禁丢"）
    obtain: it.obtain || [],           // 获得来源（数组）
    contents: it.contents || null,     // 内容物（{编号: 说明}）
    note: it.note || '',
    unimpl: null,
  }))
  const un = (catalog.value?.unimplemented || []).map((u, i) => ({
    key: `un-${i}-${u.name}`,
    item_id: u.item_id,
    name: u.name,
    aliases: [],
    category: '',
    implemented: normStatus(u.status),
    sites: [],
    server: null,
    expected: u.expected || '',
    dropPolicy: '',
    obtain: [],
    contents: null,
    note: '',
    unimpl: u,
  }))
  return items.concat(un)
})

const findings = computed(() => catalog.value?.findings || [])

// ---- 关联不一致项：物品实现位置（file:line，含 note 里引用的行号） vs finding.evidence ----
// 规则：同文件且行号相差 ≤3（证据常引用函数内附近行，如 1221 实现 vs 1223 证据）。自动关联，供排查参考。
function locatorsOf(row) {
  const out = []
  const push = (file, line) => {
    if (file && line) out.push({ file: String(file), line: Number(line), text: `${file}:${line}` })
  }
  for (const s of row.sites) push(s.file, s.line)
  if (row.unimpl) {
    for (const m of String(row.unimpl.evidence || '').matchAll(/([\w./-]+\.(?:py|go|xml)):(\d+)/g)) {
      push(m[1], m[2])
    }
  }
  for (const s of row.sites) {
    for (const m of String(s.note || '').matchAll(/([\w./-]+\.(?:py|go|xml)):(\d+)/g)) {
      push(m[1], m[2])
    }
  }
  return out
}

function evidenceHits(ev, locs) {
  for (const m of String(ev).matchAll(/([\w./-]+\.(?:py|go|xml)):(\d+)/g)) {
    const file = m[1]
    const line = Number(m[2])
    for (const loc of locs) {
      if (loc.file !== file) continue
      if (Math.abs(loc.line - line) <= 3) return true
    }
  }
  return false
}

function relatedFindings(row) {
  const locs = locatorsOf(row)
  if (!locs.length) return []
  return findings.value.filter((f) => (f.evidence || []).some((ev) => evidenceHits(ev, locs)))
}

const shown = computed(() => {
  const kw = search.value.trim().toLowerCase()
  return rows.value.filter((r) => {
    if (catFilter.value === NO_CAT) {
      if (r.category !== '') return false
    } else if (catFilter.value && r.category !== catFilter.value) return false
    if (statusFilter.value && r.implemented !== statusFilter.value) return false
    if (kw) {
      const hay = [r.name, String(r.item_id ?? ''), ...r.aliases.map(String)].join(' ').toLowerCase()
      if (!hay.includes(kw)) return false
    }
    return true
  })
})

const stats = computed(() => {
  const c = { yes: 0, partial: 0, no: 0 }
  for (const r of rows.value) c[r.implemented]++
  return c
})

const catOptions = computed(() => {
  const used = new Set(rows.value.map((r) => r.category))
  const opts = []
  const declared = new Set()
  for (const c of catalog.value?.categories || []) {
    declared.add(c.key)
    if (used.has(c.key)) opts.push({ key: c.key, label: c.label })
  }
  // 数据侧可能先用新键（如 exp/misc/skill_book）而 categories[] 还没来得及声明中文名——
  // 这类键也要能筛选（label 回退用键名），否则条目"看得见、筛不到"。
  for (const k of used) if (k && !declared.has(k)) opts.push({ key: k, label: k + '（未声明）' })
  if (used.has('')) opts.push({ key: NO_CAT, label: '未分类（未实现清单）' })
  return opts
})

async function load() {
  loading.value = true
  err.value = ''
  needRestart.value = false
  notReady.value = ''
  try {
    const res = await apiGet('/api/item_usage')
    if (res.available === false) {
      catalog.value = null
      notReady.value = res.msg || '物品配置数据未就绪'
    } else {
      catalog.value = res.catalog || null
      if (!catalog.value) notReady.value = '接口未返回 catalog 数据'
    }
  } catch (e) {
    catalog.value = null
    err.value = e.message || String(e)
    needRestart.value = String(err.value).includes('404')
  } finally {
    loading.value = false
  }
}

function openDetail(row) {
  detail.value = row
  drawerOpen.value = true
}

function idText(row) {
  if (row.item_id) return String(row.item_id)
  return row.implemented === 'no' ? '—' : '泛类'
}

function fileLine(s) { return `${s.file}:${s.line}` }
function findById(id) { return findings.value.find((f) => f.id === id) }

onMounted(load)
</script>

<template>
  <div class="card">
    <div class="row">
      <el-input v-model="search" size="small" placeholder="搜索物品名 / 编号 / 别名"
                clearable style="width: 260px" />
      <el-select v-model="catFilter" size="small" placeholder="全部类别" clearable style="width: 220px">
        <el-option v-for="c in catOptions" :key="c.key" :value="c.key" :label="c.label" />
      </el-select>
      <el-select v-model="statusFilter" size="small" placeholder="全部状态" clearable style="width: 140px">
        <el-option value="yes" label="已实现" />
        <el-option value="partial" label="半实现" />
        <el-option value="no" label="未实现" />
      </el-select>
      <span class="spacer" />
      <el-tag v-if="catalog" size="small" type="info" effect="plain">数据版本 {{ catalog.version || '--' }}</el-tag>
      <el-tag v-if="catalog" size="small" type="success" effect="plain">已实现 {{ stats.yes }}</el-tag>
      <el-tag v-if="catalog" size="small" type="warning" effect="plain">半实现 {{ stats.partial }}</el-tag>
      <el-tag v-if="catalog" size="small" type="danger" effect="plain">未实现 {{ stats.no }}</el-tag>
      <el-tag v-if="catalog" size="small" effect="plain">不一致项 F{{ findings.length }}</el-tag>
      <el-button size="small" :loading="loading" @click="load">刷新</el-button>
    </div>
    <div class="muted small mt6">
      点任意一行看"这个物品是怎么实现使用的"：用途 / 实现位置（文件:函数:行）/ 触发条件 / 阈值 / 是否配置化。
      数据源 <span class="mono">data/item_usage_catalog.json</span>（只读）。
    </div>
  </div>

  <!-- 接口不可用（最常见：中控还没重启，新路由不存在）→ 优雅提示，不空白不报错崩页 -->
  <el-alert v-if="err" class="mb" type="warning" :closable="false" show-icon
            :title="needRestart ? '接口尚未就绪：需中控重启后生效' : '接口请求失败'">
    <div class="mono small">{{ err }}</div>
    <div v-if="needRestart" class="small">
      <span class="mono">GET /api/item_usage</span> 是本次新增的只读接口，运行中的中控进程还没加载它；
      页面（web/dist）刷新即生效，接口要等中控下次重启。
    </div>
    <el-button size="small" class="mt6" :loading="loading" @click="load">重试</el-button>
  </el-alert>
  <el-alert v-else-if="notReady" class="mb" type="info" :closable="false" show-icon
            title="物品配置数据未就绪" :description="notReady + '（文件放到 data/ 后刷新即可，无需重启）'" />

  <div v-else class="card">
    <el-table :data="shown" v-loading="loading" size="small" max-height="62vh"
              style="width: 100%" row-class-name="clickable" @row-click="openDetail">
      <el-table-column label="物品" min-width="200" show-overflow-tooltip>
        <template #default="{ row }">
          <span>{{ row.name }}</span>
          <el-tag v-for="a in row.aliases.slice(0, 4)" :key="a" size="small" type="info" effect="plain"
                  class="alias">{{ a }}</el-tag>
          <el-tag v-if="row.aliases.length > 4" size="small" type="info" effect="plain" class="alias">
            +{{ row.aliases.length - 4 }}
          </el-tag>
        </template>
      </el-table-column>
      <el-table-column label="编号" width="110">
        <template #default="{ row }"><span class="mono">{{ idText(row) }}</span></template>
      </el-table-column>
      <el-table-column label="类别" width="170" show-overflow-tooltip>
        <template #default="{ row }">{{ catLabels[row.category] || (row.category || '—') }}</template>
      </el-table-column>
      <el-table-column label="实现状态" width="100">
        <template #default="{ row }">
          <el-tag size="small" :type="STATUS[row.implemented].type">{{ STATUS[row.implemented].label }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="使用实现点" width="110">
        <template #default="{ row }">
          <span v-if="row.sites.length">{{ row.sites.length }} 处</span>
          <span v-else class="muted">—</span>
        </template>
      </el-table-column>
      <el-table-column label="关联不一致" width="120">
        <template #default="{ row }">
          <template v-if="relatedFindings(row).length">
            <el-tag v-for="f in relatedFindings(row)" :key="f.id" size="small" class="alias"
                    :type="SEV_TYPE[f.severity] || 'info'" effect="plain">{{ f.id }}</el-tag>
          </template>
          <span v-else class="muted">—</span>
        </template>
      </el-table-column>
      <el-table-column label="" width="90">
        <template #default="{ row }">
          <el-button size="small" link type="primary" @click.stop="openDetail(row)">看实现</el-button>
        </template>
      </el-table-column>
    </el-table>
    <el-empty v-if="!loading && !shown.length" :image-size="60"
              description="没有匹配的物品（换个关键词/清除筛选试试）" />
  </div>

  <!-- 详情：这个物品是怎么实现使用的 -->
  <el-drawer v-model="drawerOpen" size="640px" :title="detail ? detail.name : ''">
    <template v-if="detail">
      <div class="row mb">
        <el-tag size="small" :type="STATUS[detail.implemented].type">
          {{ STATUS[detail.implemented].label }}
        </el-tag>
        <el-tag size="small" type="info" effect="plain" class="mono">编号 {{ idText(detail) }}</el-tag>
        <el-tag size="small" effect="plain">{{ catLabels[detail.category] || (detail.category || '未分类') }}</el-tag>
        <el-tag v-for="a in detail.aliases" :key="a" size="small" type="info" effect="plain" class="mono">别名 {{ a }}</el-tag>
      </div>

      <!-- 服务端语义（item.xml 权威口径） -->
      <div v-if="detail.server" class="sect">
        <h4>服务端语义（item.xml）</h4>
        <div class="kv"><span class="k">handler</span><span>{{ detail.server.handler || '—' }}</span></div>
        <div class="kv"><span class="k">action</span><span class="mono">{{ detail.server.action || '—' }}</span></div>
        <div v-if="detail.server.param" class="kv"><span class="k">param</span><span class="mono">{{ detail.server.param }}</span></div>
        <div class="kv"><span class="k">可在守护上使用</span>
          <span>{{ detail.server.use_on_summon ? '是（use_on_summon）' : '否' }}</span></div>
      </div>

      <!-- 补充信息（catalog v2 加法字段：有才渲染） -->
      <div v-if="detail.obtain.length || detail.contents || detail.dropPolicy || detail.note" class="sect">
        <h4>补充信息</h4>
        <div v-if="detail.obtain.length" class="kv"><span class="k">获得来源</span>
          <span><div v-for="(o, i) in detail.obtain" :key="i" class="pre">{{ o }}</div></span></div>
        <div v-if="detail.contents" class="kv"><span class="k">内容物</span>
          <span><div v-for="(v, k) in detail.contents" :key="k" class="pre"><span class="mono">{{ k }}</span> {{ v }}</div></span></div>
        <div v-if="detail.dropPolicy" class="kv"><span class="k">丢弃策略</span><span class="pre">{{ detail.dropPolicy }}</span></div>
        <div v-if="detail.note" class="kv"><span class="k">备注</span><span class="pre">{{ detail.note }}</span></div>
      </div>

      <!-- 未实现/半实现说明（清单 unimplemented 段，或 items 内的 expected 字段） -->
      <div v-if="detail.unimpl || detail.expected" class="sect">
        <h4>未实现 / 半实现说明</h4>
        <div v-if="detail.expected" class="kv"><span class="k">期望实现</span><span>{{ detail.expected }}</span></div>
        <template v-if="detail.unimpl">
          <div class="kv"><span class="k">现状标注</span><span>{{ detail.unimpl.status || '—' }}</span></div>
          <div class="kv"><span class="k">证据</span><span>{{ detail.unimpl.evidence || '—' }}</span></div>
        </template>
      </div>

      <!-- 实现点：每处 = 一个"在哪、何时、用了什么阈值" -->
      <div class="sect" v-if="detail.sites.length">
        <h4>实现点（{{ detail.sites.length }} 处）</h4>
        <div v-for="(s, i) in detail.sites" :key="i" class="site">
          <div class="row">
            <span class="tag info">{{ i + 1 }}</span>
            <b>{{ s.module }}</b>
            <span class="mono">{{ s.function }}</span>
            <span class="spacer" />
            <span class="tag dim mono">{{ fileLine(s) }}</span>
          </div>
          <div class="kv"><span class="k">协议</span><span class="mono">{{ s.protocol || '—' }}</span></div>
          <div class="kv"><span class="k">场景</span><span>{{ s.scene || '—' }}</span></div>
          <div class="kv"><span class="k">触发条件</span><span>{{ s.trigger || '—' }}</span></div>
          <div class="kv"><span class="k">冷却/节奏</span><span>{{ s.cooldown || '—' }}</span></div>
          <div class="kv"><span class="k">配置项</span>
            <span>
              <template v-if="s.config && s.config.length">
                <el-tag v-for="c in s.config" :key="c" size="small" effect="plain" class="mono alias">{{ c }}</el-tag>
              </template>
              <span v-else class="muted">硬编码（不可配）</span>
            </span>
          </div>
          <div class="kv" v-if="s.deduction"><span class="k">本地扣减</span><span>{{ s.deduction }}</span></div>
          <div class="kv" v-if="s.note"><span class="k">备注</span><span>{{ s.note }}</span></div>
        </div>
      </div>
      <div v-else class="sect muted">
        <template v-if="detail.implemented === 'no'">未实现：暂无任何使用实现（期望见上「未实现 / 半实现说明」）。</template>
        <template v-else-if="detail.unimpl">无机器人端实现点（见上「未实现 / 半实现说明」）。</template>
        <template v-else>无实现点记录。</template>
      </div>

      <!-- 关联的不一致项（F1-FN，随清单增长；自动关联） -->
      <div class="sect">
        <h4>关联不一致项</h4>
        <template v-if="relatedFindings(detail).length">
          <div v-for="f in relatedFindings(detail)" :key="f.id" class="site">
            <div class="row">
              <el-tag size="small" :type="SEV_TYPE[f.severity] || 'info'">{{ f.id }} · {{ f.severity }}</el-tag>
              <b>{{ f.title }}</b>
            </div>
            <div class="small pre">{{ f.detail }}</div>
            <div class="small muted mono">证据：{{ (f.evidence || []).join('，') }}</div>
          </div>
        </template>
        <div v-else class="muted small">
          未关联到不一致项（关联规则：证据行号与该物品实现位置同文件且相差 ≤3 行）。
        </div>
      </div>
    </template>
  </el-drawer>
</template>

<style scoped>
.small { font-size: 12px; }
.mt6 { margin-top: 6px; }
.mb { margin-bottom: 12px; }
.alias { margin-left: 4px; }
.sect { border-top: 1px solid var(--border); padding-top: 10px; margin-top: 10px; }
.sect h4 { margin: 0 0 8px; font-size: 13px; color: var(--text-dim); }
.kv { display: flex; gap: 10px; font-size: 13px; padding: 2px 0; line-height: 1.5; }
.kv .k { flex: 0 0 92px; color: var(--text-dim); }
.kv span:last-child { word-break: break-all; }
.site {
  border: 1px solid var(--border); border-radius: 6px;
  padding: 8px 10px; margin-bottom: 8px; background: rgba(42, 55, 87, .12);
}
.site .row { margin-bottom: 4px; }
.pre { white-space: pre-wrap; word-break: break-all; color: var(--text-dim); }
:deep(.clickable) { cursor: pointer; }
</style>
