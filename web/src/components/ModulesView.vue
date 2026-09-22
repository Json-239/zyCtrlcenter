<script setup>
// 模块地图：把"拆出来的功能模块 + 各自职责/数据来源/对应参考实现/关联用例"摆出来。
// 数据源 GET /api/modules（注册表在 Go 侧，动作层 9 个键与 chainplan.ModuleNames() 有单测钉住）。
import { computed, onMounted, ref } from 'vue'
import { apiGet } from '../api'

const err = ref('')
const layers = ref([])
const modules = ref([])
const report = ref(null)
const reportPath = ref('')
const keyword = ref('')

async function load() {
  try {
    const res = await apiGet('/api/modules')
    layers.value = res.layers || []
    modules.value = res.modules || []
    report.value = res.report || null
    reportPath.value = res.report_path || ''
    err.value = res.report_error || ''
  } catch (e) {
    err.value = e.message
  }
}
onMounted(load)

// 最近一次测试报告里"每个测试包"的结果（卡片的关联用例归到包里看）
const pkgIndex = computed(() => {
  const m = {}
  for (const p of report.value?.packages || []) m[p.short] = p
  return m
})

function pkgOf(t) { return String(t).split('::')[0] }

// 卡片右上角徽标：该模块关联用例所在包在最近一次报告里的结果；没跑过报告显示 —
function statusOf(mod) {
  if (!report.value) return { text: '—', cls: 'dim', title: '还没跑过报告：go run ./tools/test_report' }
  const pkgs = [...new Set((mod.tests || []).map(pkgOf))]
  let cases = 0, passed = 0, failed = 0
  for (const p of pkgs) {
    const st = pkgIndex.value[p]
    if (!st) continue
    cases += st.cases || 0
    passed += st.passed || 0
    failed += st.failed || 0
  }
  if (!cases) return { text: '—', cls: 'dim', title: '报告里没有这些包（可能还没登记）' }
  if (failed) return { text: `失败 ${failed}/${cases}`, cls: 'danger', title: '有失败用例，见报告' }
  return { text: `通过 ${passed}/${cases}`, cls: 'ok', title: `${pkgs.join('、')} 全绿` }
}

function match(m, kw) {
  if (!kw) return true
  const hay = [m.key, m.name, m.desc, m.data, m.py].concat(m.tests || []).join(' ').toLowerCase()
  return hay.includes(kw)
}

const grouped = computed(() => {
  const kw = keyword.value.trim().toLowerCase()
  return layers.value
    .map((l) => ({ ...l, items: modules.value.filter((m) => m.layer === l.key && match(m, kw)) }))
    .filter((g) => g.items.length)
})

function reportLine() {
  const r = report.value
  if (!r) return '还没有测试报告（跑 go run ./tools/test_report 生成；文件 data/test_report.json）'
  const t = r.totals || {}
  return `${r.date} · ${r.ok ? '全绿' : '有失败'} · 用例 ${t.cases ?? '—'}（通过 ${t.passed ?? '—'} / 失败 ${t.failed ?? '—'}）· vet=${r.vet} 竞态=${r.race}`
}
</script>

<template>
  <div v-if="err" class="card"><span class="tag danger">{{ err }}</span></div>

  <div class="card">
    <div class="row">
      <h3 style="margin: 0">模块地图</h3>
      <span class="tag" :class="report ? (report.ok ? 'ok' : 'warn') : 'dim'">{{ reportLine() }}</span>
      <span class="spacer" />
      <input v-model="keyword" class="grow-sm" style="max-width: 260px" placeholder="搜模块 / 职责 / 用例" />
      <button class="btn sm" @click="load">刷新</button>
    </div>
    <p class="muted" style="margin: 10px 0 0">
      中控不执行链：动作原语由机器人端执行，这里只描述"卡住时卡在哪一层"。
      动作层 9 个模块的键集与顺序与 <span class="mono">chainplan.ModuleNames()</span> 完全一致（有单测钉住，漏登记会红）。
      <span v-if="reportPath" class="mono">报告：{{ reportPath }}</span>
    </p>
  </div>

  <div v-for="g in grouped" :key="g.key" class="card">
    <div class="row" style="margin-bottom: 8px">
      <h3 style="margin: 0">{{ g.name }}</h3>
      <span class="muted">{{ g.items.length }} 个</span>
    </div>
    <div class="grid cols-4">
      <div v-for="m in g.items" :key="m.key" class="stat">
        <div class="row" style="gap: 6px">
          <span class="mono">{{ m.key }}</span>
          <span class="tag" :class="statusOf(m).cls" :title="statusOf(m).title">{{ statusOf(m).text }}</span>
        </div>
        <div class="value" style="font-size: 18px">{{ m.name }}</div>
        <div class="sub">{{ m.desc }}</div>
        <div class="muted" style="font-size: 12px; margin-top: 6px">数据：{{ m.data }}</div>
        <div class="muted" style="font-size: 12px">py：{{ m.py }}</div>
        <div class="muted" style="font-size: 12px; margin-top: 6px">
          测试：
          <span v-for="t in m.tests" :key="t" class="mono" style="display: block">{{ t }}</span>
        </div>
      </div>
    </div>
  </div>

  <div v-if="!grouped.length" class="card"><div class="empty">没有匹配的模块：换个关键词。</div></div>
</template>
