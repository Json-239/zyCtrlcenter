<script setup>
// 模块地图：把"拆出来的功能模块 + 各自职责/数据来源/对应参考实现/关联用例"摆出来。
// 数据源 GET /api/modules（注册表在 Go 侧，动作层 9 个键与 chainplan.ModuleNames() 有单测钉住）。
// 2026-09-22 UI 改 Element Plus：搜索换 el-input（带图标），模块卡片换 el-card，状态换 el-tag。
import { computed, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { apiGet, apiPost } from '../api'

const err = ref('')
const layers = ref([])
const modules = ref([])
const report = ref(null)
const reportPath = ref('')
const keyword = ref('')
const loading = ref(false)

async function load() {
  loading.value = true
  try {
    const res = await apiGet('/api/modules')
    layers.value = res.layers || []
    modules.value = res.modules || []
    report.value = res.report || null
    reportPath.value = res.report_path || ''
    err.value = res.report_error || ''
  } catch (e) {
    err.value = e.message
  } finally {
    loading.value = false
  }
}
onMounted(() => { load(); loadSkill() })

// ---------------------------------------------------------------------------
// 2026-09-23 「战斗」模块（可配置）：技能策略 —— GET/POST /api/skill_config
//   保存即下发机器人（在线号下一场战斗生效；机器人连上时中控自动补发，重启不丢）。
// ---------------------------------------------------------------------------
const skOpen = ref(false)
const skSaving = ref(false)
const skSpecialText = ref('901,902')
const skCfg = ref({
  enabled: true,
  scenes: { ghost: true, wild: true, story: false },
  guardian: 'attack',
  strategy: { immortal: 'best_damage', human: 'special_random', demon: 'special_random', default: 'random' },
  special: [901, 902],
})

async function loadSkill() {
  try {
    const res = await apiGet('/api/skill_config')
    if (res.ok && res.config) {
      const c = res.config
      skCfg.value = {
        enabled: c.enabled !== false,
        scenes: { ghost: true, wild: true, story: false, ...(c.scenes || {}) },
        guardian: c.guardian || 'attack',
        strategy: {
          immortal: 'best_damage', human: 'special_random', demon: 'special_random', default: 'random',
          ...(c.strategy || {}),
        },
        special: (c.special && c.special.length) ? c.special : [901, 902],
      }
      skSpecialText.value = (skCfg.value.special || []).join(',')
    }
  } catch (e) {
    // 老中控没有该接口：用默认展示，不打扰
  }
}

function openSkill() { loadSkill(); skOpen.value = true }

async function saveSkill() {
  skSaving.value = true
  try {
    const special = skSpecialText.value.split(',').map((x) => parseInt(x.trim(), 10)).filter((n) => n > 0)
    const res = await apiPost('/api/skill_config', {
      enabled: skCfg.value.enabled, scenes: skCfg.value.scenes,
      guardian: skCfg.value.guardian, strategy: skCfg.value.strategy, special,
    })
    ElMessage({ type: res.ok ? 'success' : 'warning', message: res.msg || (res.ok ? '已保存' : '保存失败') })
    if (res.ok) skOpen.value = false
  } catch (e) {
    ElMessage({ type: 'error', message: e.message })
  } finally {
    skSaving.value = false
  }
}

// 最近一次测试报告里"每个测试包"的结果（卡片的关联用例归到包里看）
const pkgIndex = computed(() => {
  const m = {}
  for (const p of report.value?.packages || []) m[p.short] = p
  return m
})

function pkgOf(t) { return String(t).split('::')[0] }

// 卡片右上角徽标：该模块关联用例所在包在最近一次报告里的结果；没跑过报告显示 —
// type 直接给 el-tag 用（原先是 .tag 的语义色类名）
function statusOf(mod) {
  if (!report.value) return { text: '—', type: 'info', title: '还没跑过报告：go run ./tools/test_report' }
  const pkgs = [...new Set((mod.tests || []).map(pkgOf))]
  let cases = 0, passed = 0, failed = 0
  for (const p of pkgs) {
    const st = pkgIndex.value[p]
    if (!st) continue
    cases += st.cases || 0
    passed += st.passed || 0
    failed += st.failed || 0
  }
  if (!cases) return { text: '—', type: 'info', title: '报告里没有这些包（可能还没登记）' }
  if (failed) return { text: `失败 ${failed}/${cases}`, type: 'danger', title: '有失败用例，见报告' }
  return { text: `通过 ${passed}/${cases}`, type: 'success', title: `${pkgs.join('、')} 全绿` }
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
  <el-alert v-if="err" class="mb" type="error" :closable="false" show-icon :title="err" />

  <div class="card">
    <div class="row">
      <h3 style="margin: 0">模块地图</h3>
      <el-tag size="small" :type="report ? (report.ok ? 'success' : 'warning') : 'info'" effect="plain">{{ reportLine() }}</el-tag>
      <span class="spacer" />
      <el-input v-model="keyword" size="small" style="width: 260px" clearable placeholder="搜模块 / 职责 / 用例">
        <template #prefix><el-icon><Search /></el-icon></template>
      </el-input>
      <el-button size="small" @click="load">
        <el-icon><Refresh /></el-icon>
        <span>刷新</span>
      </el-button>
    </div>
    <p class="muted" style="margin: 10px 0 0">
      中控不执行链：动作原语由机器人端执行，这里只描述"卡住时卡在哪一层"。
      动作层 9 个模块的键集与顺序与 <span class="mono">chainplan.ModuleNames()</span> 完全一致（有单测钉住，漏登记会红）。
      <span v-if="reportPath" class="mono">报告：{{ reportPath }}</span>
    </p>
  </div>

  <!-- 2026-09-23 「战斗」模块（可配置）：点卡片打开技能策略抽屉 -->
  <div class="card">
    <div class="row" style="margin-bottom: 8px">
      <h3 style="margin: 0">战斗</h3>
      <span class="muted">技能策略（可配置 · 保存即下发）</span>
      <span class="spacer" />
      <el-tag size="small" :type="skCfg.enabled ? 'success' : 'info'" effect="plain">
        {{ skCfg.enabled ? '技能攻击已开' : '全部普攻' }}
      </el-tag>
    </div>
    <div class="grid cols-4">
      <el-card shadow="hover" class="mod-card clickable" @click="openSkill">
        <div class="row" style="gap: 6px">
          <span class="mono">fight</span>
          <el-tag size="small" type="success" effect="plain">可配置</el-tag>
        </div>
        <div class="value" style="font-size: 18px">⚔ 战斗 / 技能策略</div>
        <div class="sub">选技能逻辑：种族策略（仙族最优伤害 / 人魔特殊技能随机）、分场景开关、特殊技能池（901/902）</div>
        <div class="muted small" style="margin-top: 6px">点击配置 →</div>
      </el-card>
    </div>
  </div>

  <el-drawer v-model="skOpen" title="⚔ 战斗 · 技能策略配置" size="540px">
    <el-form label-width="92px" label-position="left">
      <el-form-item label="技能攻击">
        <el-switch v-model="skCfg.enabled" active-text="开启" inactive-text="全部普攻" />
      </el-form-item>
      <el-form-item label="生效场景">
        <el-checkbox v-model="skCfg.scenes.ghost">抓鬼</el-checkbox>
        <el-checkbox v-model="skCfg.scenes.wild">野外</el-checkbox>
        <el-checkbox v-model="skCfg.scenes.story">剧情</el-checkbox>
        <div class="muted small">未勾选的场景用普通攻击（剧情默认不勾 —— 历史上有打不赢的教训）</div>
      </el-form-item>
      <el-divider content-position="left">种族策略（选技能模式）</el-divider>
      <el-form-item label="仙族">
        <el-select v-model="skCfg.strategy.immortal" size="small" style="width: 240px">
          <el-option value="best_damage" label="最优伤害技（推荐）" />
          <el-option value="special_random" label="特殊技能随机" />
          <el-option value="random" label="全部随机" />
        </el-select>
      </el-form-item>
      <el-form-item label="人族">
        <el-select v-model="skCfg.strategy.human" size="small" style="width: 240px">
          <el-option value="special_random" label="特殊技能随机（推荐）" />
          <el-option value="best_damage" label="最优伤害技" />
          <el-option value="random" label="全部随机" />
        </el-select>
      </el-form-item>
      <el-form-item label="魔族">
        <el-select v-model="skCfg.strategy.demon" size="small" style="width: 240px">
          <el-option value="special_random" label="特殊技能随机（推荐）" />
          <el-option value="best_damage" label="最优伤害技" />
          <el-option value="random" label="全部随机" />
        </el-select>
      </el-form-item>
      <el-form-item label="兜底">
        <el-select v-model="skCfg.strategy.default" size="small" style="width: 240px">
          <el-option value="random" label="全部随机（推荐）" />
          <el-option value="best_damage" label="最优伤害技" />
          <el-option value="special_random" label="特殊技能随机" />
        </el-select>
      </el-form-item>
      <el-divider content-position="left">特殊技能池</el-divider>
      <el-form-item label="技能号">
        <el-input v-model="skSpecialText" size="small" style="width: 240px" placeholder="逗号分隔，如 901,902" />
        <div class="muted small">901 初露锋芒（单体物理）/ 902 一石二鸟（打 2 个）—— 每号都有的新手伤害技</div>
      </el-form-item>
      <el-form-item>
        <el-button type="primary" :loading="skSaving" @click="saveSkill">保存并下发</el-button>
        <el-button @click="skOpen = false">取消</el-button>
      </el-form-item>
    </el-form>
  </el-drawer>

  <div v-for="g in grouped" :key="g.key" class="card" v-loading="loading">
    <div class="row" style="margin-bottom: 8px">
      <h3 style="margin: 0">{{ g.name }}</h3>
      <span class="muted">{{ g.items.length }} 个</span>
    </div>
    <div class="grid cols-4">
      <el-card v-for="m in g.items" :key="m.key" shadow="hover" class="mod-card">
        <div class="row" style="gap: 6px">
          <span class="mono">{{ m.key }}</span>
          <el-tag size="small" :type="statusOf(m).type" :title="statusOf(m).title" effect="plain">{{ statusOf(m).text }}</el-tag>
        </div>
        <div class="value" style="font-size: 18px">{{ m.name }}</div>
        <div class="sub">{{ m.desc }}</div>
        <div class="muted small" style="margin-top: 6px">数据：{{ m.data }}</div>
        <div class="muted small">py：{{ m.py }}</div>
        <div class="muted small" style="margin-top: 6px">
          测试：
          <span v-for="t in m.tests" :key="t" class="mono" style="display: block">{{ t }}</span>
        </div>
      </el-card>
    </div>
  </div>

  <div v-if="!grouped.length" class="card">
    <el-empty :image-size="56" description="没有匹配的模块：换个关键词。" />
  </div>
</template>

<style scoped>
.mb { margin-bottom: 12px; }
.mod-card :deep(.el-card__body) { padding: 12px 14px; }
/* styles.css 里有一条"卡片竖排叠放"的全局规则 .el-card + .el-card{margin-top:16px}，
   在网格里会顶歪第二列起的每一张卡：这里按本页布局覆盖掉 */
.mod-card + .mod-card { margin-top: 0; }
/* 可点击的模块卡（战斗）：悬停提示可配置 */
.clickable { cursor: pointer; transition: box-shadow .15s ease; }
.clickable:hover { box-shadow: 0 0 0 1px var(--brand, #4c8dff) inset; }
</style>
