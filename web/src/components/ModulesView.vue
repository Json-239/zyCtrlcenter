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

// 2026-09-23 种族策略升级为「种族 × 性别」：键优先级 "种族:性别" → "种族"（通用）→ "default"，
// 与机器人端 skill_attack.pick_skill 的退化顺序一致（sex 取值 male/female，见 skill_meta.ROLE_META）。
// 值支持两种形态：字符串（旧格式/非系别模式）与对象 {mode, magics}（magic_random = 指定系别随机）。
// 2026-09-23 用户口径「一个性别只有 3 个系」：门派按性别限定，男/女行不再列全部 4 个系。
// 矩阵依据（三路一致，勿再凭猜）：
//   ① 服务端技能配置的硬条件（最权威）：config/skill/{human,immortal,demon}_skill.xml 里
//      五庄观(风)、法门寺(混乱)、狮驼岭(速)=限定"男"；普陀山(火)、女儿国(毒)、盘丝洞(防)=限定"女"，
//      天宫/龙宫/方寸山/折冲府/千魔谷/地府 = 无性别条件；
//   ② 服务端 config/rolemsg.csv「种族推荐技能」列：6 个造型各自只列该性别的 3 个系；
//   ③ 运行数据：298 个在跑号实学技能（diag.log REINC_SKILL）只出现 5 种三元组，无例外，
//      与 deploy 侧 role_data.py 的 5 个造型（10001/10006/10008/10014/10016）一一对应；
//      其中 48 个号包内"外形限定装备"的外形与技能推断性别 100% 一致、0 矛盾。
//   仙族男(白龙将)=风/雷/水  仙族女(飞天姬)=火/雷/水  → 女仙没有风系（五庄观限男）
//   人族男(玉公子)=混乱/昏睡/封印  人族女(红霞女)=毒/昏睡/封印
//   魔族男(万兽王)=攻/速/震慑      魔族女(青蛇妖)=攻/防/震慑
// magics = 「通用」行可选（两性并集，性别未知时用）；byGender = 男/女行各自的集合。
const SK_RACES = [
  { key: 'immortal', name: '仙族', magics: ['水系', '火系', '雷系', '风系'],
    byGender: { male: ['水系', '雷系', '风系'], female: ['水系', '火系', '雷系'] } },       // 龙宫/普陀山/天宫/五庄观
  { key: 'human', name: '人族', magics: ['毒系', '昏睡系', '封印系', '混乱系'],
    byGender: { male: ['昏睡系', '封印系', '混乱系'], female: ['毒系', '昏睡系', '封印系'] } }, // 女儿国/方寸山/折冲府/法门寺
  { key: 'demon', name: '魔族', magics: ['攻系', '防系', '震慑', '速系'],
    byGender: { male: ['攻系', '震慑', '速系'], female: ['攻系', '防系', '震慑'] } },        // 魔王寨/盘丝洞/地府/狮驼岭
]
const SK_GENDERS = [
  { key: 'male', name: '男' },
  { key: 'female', name: '女' },
  { key: '', name: '通用' },
]
const SK_MODES = [
  { value: 'best_damage', label: '最优伤害技' },
  { value: 'magic_random', label: '指定系别随机' },
  { value: 'special_random', label: '特殊技能随机' },
  { value: 'random', label: '全部随机' },
]
const SK_MODE_VALUES = SK_MODES.map((m) => m.value)
// 内置默认（与 Go DefaultSkillConfig / 机器人端兜底一致）：仙族最优伤害、人/魔特殊技能随机
const SK_MODE_DEFAULT = { immortal: 'best_damage', human: 'special_random', demon: 'special_random' }

function skKey(race, gender) { return gender ? `${race}:${gender}` : race }

// 该「种族 × 性别」可选的系别（通用行=两性并集）。矩阵外的一律不出现。
function skMagicsOf(race, gender) {
  return (gender && race.byGender && race.byGender[gender]) || race.magics
}

// 服务端一个策略值 → {mode, magics}：字符串=旧格式；对象=新格式；未识别模式按 random（与后端同口径）
function normStratVal(v) {
  const o = (v && typeof v === 'object') ? v : { mode: v, magics: [] }
  return {
    mode: SK_MODE_VALUES.includes(o.mode) ? o.mode : 'random',
    magics: Array.isArray(o.magics) ? o.magics.map(String) : [],
  }
}

// 一个键的生效值：key → 种族 → default → 内置默认（与机器人端退化顺序一致）
function skEffective(raw, race, gender) {
  for (const k of [skKey(race, gender), race, 'default']) {
    if (raw && raw[k] !== undefined && raw[k] !== null) return normStratVal(raw[k])
  }
  return { mode: SK_MODE_DEFAULT[race] || 'random', magics: [] }
}

// 旧配置里"不属于该性别"的系别（例：女仙配了风系）——不显示、保存时丢弃；
// 机器人侧本来也用不到（该系别无可用技能 → pick_skill 退化为"全部随机"），这里明确提示。
const skDropped = ref([])

// 整棵 strategy 状态：三族的 男/女/通用 + 兜底；系别按"该性别可选集合"裁剪
function buildStrategy(raw) {
  const out = {}
  const dropped = []
  for (const r of SK_RACES) {
    for (const g of SK_GENDERS) {
      const v = skEffective(raw, r.key, g.key)
      const allow = skMagicsOf(r, g.key)
      const lost = v.magics.filter((m) => !allow.includes(m))
      if (lost.length) dropped.push({ label: r.name + '·' + g.name, magics: lost })
      out[skKey(r.key, g.key)] = { mode: v.mode, magics: v.magics.filter((m) => allow.includes(m)) }
    }
  }
  out.default = skEffective(raw, 'default', '')
  skDropped.value = dropped
  return out
}

// 状态 → 提交值：magic_random 送对象 {mode[, magics]}，其它模式送字符串（后端两种都收）
function strategyPayload(strat) {
  const out = {}
  for (const [k, v] of Object.entries(strat)) {
    if (v.mode === 'magic_random') {
      const magics = (v.magics || []).filter(Boolean)
      out[k] = magics.length ? { mode: v.mode, magics } : { mode: v.mode }
    } else {
      out[k] = v.mode
    }
  }
  return out
}

// 键 → 中文名（保存校验提示用）
function skLabel(k) {
  if (k === 'default') return '兜底'
  const [race, gender] = k.split(':')
  const r = SK_RACES.find((x) => x.key === race)
  const g = { male: '男', female: '女' }[gender]
  return (r ? r.name : race) + (g ? '·' + g : '')
}

// 保存回读校验：对象值（指定系别）应能原样回读。若中控侧 Strategy 仍是 map[string]string，
// 对象值会被静默丢弃 —— 那就明确提示用户，而不是假装保存成功。
function lostStratKeys(sent, saved) {
  const out = []
  for (const [k, v] of Object.entries(sent)) {
    if (!v || typeof v !== 'object') continue
    const got = saved ? saved[k] : undefined
    const mode = (got && typeof got === 'object') ? got.mode : (typeof got === 'string' ? got : '')
    if (mode !== v.mode) out.push(k)
  }
  return out
}

const skCfg = ref({
  enabled: true,
  scenes: { ghost: true, wild: true, story: false },
  guardian: 'attack',
  strategy: buildStrategy({}),
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
        strategy: buildStrategy(c.strategy || {}), // 字符串/对象两种值都归一化成 {mode, magics}
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
    const strategy = strategyPayload(skCfg.value.strategy)
    const res = await apiPost('/api/skill_config', {
      enabled: skCfg.value.enabled, scenes: skCfg.value.scenes,
      guardian: skCfg.value.guardian, strategy, special,
    })
    if (res.ok) {
      const lost = lostStratKeys(strategy, res.config && res.config.strategy)
      if (lost.length) {
        const names = lost.slice(0, 4).map(skLabel).join('、')
        ElMessage({
          type: 'warning',
          message: `已保存，但 ${names}${lost.length > 4 ? ' 等 ' + lost.length + ' 项' : ''}的"指定系别"未生效（当前中控不接收对象值）`,
        })
      } else {
        ElMessage({ type: 'success', message: res.msg || '已保存' })
      }
      skOpen.value = false
    } else {
      ElMessage({ type: 'warning', message: res.msg || '保存失败' })
    }
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
        <div class="sub">选技能逻辑：种族×性别策略（可指定系别随机）、分场景开关、特殊技能池（901/902）</div>
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
      <el-divider content-position="left">种族策略（每族按 男 / 女 / 通用 配）</el-divider>
      <el-alert
        v-if="skDropped.length"
        class="mb"
        type="warning"
        :closable="false"
        show-icon
        title="旧配置里有该系统不存在的系别：已不显示，保存后会被忽略"
        :description="skDropped.map((d) => d.label + '：' + d.magics.join('/')).join('；')"
      />
      <template v-for="race in SK_RACES" :key="race.key">
        <el-divider content-position="left" class="sk-race">{{ race.name }}</el-divider>
        <el-form-item v-for="g in SK_GENDERS" :key="g.key" :label="g.name">
          <div class="sk-row">
            <el-select v-model="skCfg.strategy[skKey(race.key, g.key)].mode" size="small" style="width: 140px">
              <el-option v-for="m in SK_MODES" :key="m.value" :value="m.value" :label="m.label" />
            </el-select>
            <template v-if="skCfg.strategy[skKey(race.key, g.key)].mode === 'magic_random'">
              <span class="sk-magics-wrap">
                <span class="muted small">系别</span>
                <el-checkbox-group v-model="skCfg.strategy[skKey(race.key, g.key)].magics" size="small" class="sk-magics">
                  <el-checkbox v-for="mg in skMagicsOf(race, g.key)" :key="mg" :value="mg">{{ mg }}</el-checkbox>
                </el-checkbox-group>
              </span>
              <span v-if="!skCfg.strategy[skKey(race.key, g.key)].magics.length" class="muted small">未勾选 → 机器人按"全部随机"</span>
              <span v-else-if="!g.key" class="muted small">通用=两性并集，性别未知的号走这行</span>
            </template>
            <span v-else-if="!g.key" class="muted small">不分性别或性别未知时用这行</span>
          </div>
        </el-form-item>
      </template>
      <el-divider content-position="left" class="sk-race">兜底</el-divider>
      <el-form-item label="模式">
        <div class="sk-row">
          <el-select v-model="skCfg.strategy.default.mode" size="small" style="width: 140px">
            <el-option v-for="m in SK_MODES" :key="m.value" :value="m.value" :label="m.label" />
          </el-select>
          <span class="muted small">默认"全部随机"；这一档不区分系别（选了指定系别也按全部随机）</span>
        </div>
      </el-form-item>
      <el-divider content-position="left">特殊技能池</el-divider>
      <el-form-item label="技能号">
        <el-input v-model="skSpecialText" size="small" style="width: 240px" placeholder="逗号分隔，如 901,902" />
        <div class="muted small">901 初露锋芒（单体物理）/ 902 一石二鸟（打 2 个）—— 每号都有的新手伤害技；60 级以上不再使用（"特殊技能随机"会退化为"全部随机"）</div>
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
/* 种族策略行（种族×性别）：模式下拉 + 系别勾选，抽屉窄（540px）时系别整体换行成一排 */
.sk-row { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; width: 100%; }
.sk-magics-wrap { display: inline-flex; align-items: center; gap: 8px; }
.sk-magics :deep(.el-checkbox) { margin-right: 10px; }
.sk-race :deep(.el-divider__text) { font-size: 13px; font-weight: 600; }
</style>
