<script setup>
// 「系统信息」页：运行信息 + 多区配置（服/区）+ 端口约定 + 协议映射（只读）。
// 2026-09-22 UI 改 Element Plus：表格 → el-table；新增服/区 → el-dialog + el-form 校验；
// 切区/应用/重启/删除 → ElMessageBox.confirm 二次确认。
//
// 关于反馈通道：面板底部那套 toast 由 store.post() 自动弹（App.vue 渲染），
// 本页若再用 post() 就会「一次点击两条提示」。所以本页的写操作走 call()
// （apiPost + refreshStatus，接口与字段与原实现完全一致），只弹 ElMessage。
import { computed, nextTick, onMounted, reactive, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { apiGet, apiPost } from '../api'
import { state, zones, currentZoneKey, refreshStatus } from '../store'

const config = ref(null)
const protocols = ref(null)
const err = ref('')
const zoneSel = ref('')
const restartOnApply = ref(true)
const zoneDlg = ref(false)
const serverDlg = ref(false)
const zoneEditing = ref(false)
const zoneFormRef = ref(null)
const serverFormRef = ref(null)

// 新增/编辑区表单（单进程：区只描述"游戏服地址 + 编码"）
const zoneForm = reactive({ server: '', key: '', name: '', port: 2300, coding: '', note: '' })
// 新增服表单
const serverForm = reactive({ key: '', name: '', host: '', coding: 'UTF-8', note: '' })

const zoneRules = {
  server: [{ required: true, message: '请选择所属服', trigger: 'change' }],
  port: [{ required: true, message: '请填游戏服端口', trigger: 'blur' }],
  key: [{ pattern: /^[A-Za-z0-9._-]*$/, message: '只能用字母/数字/._-（留空=用端口）', trigger: 'blur' }],
}
const serverRules = {
  key: [
    { required: true, message: '请填服 key（如 prod-240）', trigger: 'blur' },
    { pattern: /^[A-Za-z0-9._-]+$/, message: '只能用字母/数字/._-', trigger: 'blur' },
  ],
  name: [{ required: true, message: '请填展示名（如 生产网关）', trigger: 'blur' }],
  host: [
    { required: true, message: '请填游戏服 IP', trigger: 'blur' },
    { pattern: /^(\d{1,3}\.){3}\d{1,3}$/, message: '只能填 IPv4 地址（不能填域名）', trigger: 'blur' },
  ],
}

const zoneList = computed(() => zones())
const currentKey = computed(() => currentZoneKey())
const curZone = computed(() => state.status?.current || {})
// 当前区选择器：随轮询结果同步（否则初次进入会显示"未选择"）
watch(currentKey, (k) => { if (k) zoneSel.value = k }, { immediate: true })

async function loadConfig() {
  try {
    config.value = await apiGet('/api/config')
    if (!zoneForm.server && config.value?.servers?.length) zoneForm.server = config.value.servers[0].key
  } catch (e) {
    err.value = e.message
  }
}
async function loadProtocols() {
  try { protocols.value = await apiGet('/api/protocols') } catch (e) { /* 不阻塞 */ }
}
onMounted(async () => { await loadConfig(); await loadProtocols() })

const servers = computed(() => config.value?.servers || [])
const defaults = computed(() => config.value?.defaults || {})

// 写操作统一入口：只弹 ElMessage（成功/失败各一条），并顺带刷新总览状态
async function call(path, body, okText) {
  try {
    const res = await apiPost(path, body)
    if (res.ok === false) ElMessage.error(res.msg || '操作失败')
    else ElMessage.success(res.msg || okText || '操作成功')
    refreshStatus()
    return res
  } catch (e) {
    ElMessage.error(e.message)
    return { ok: false, msg: e.message }
  }
}

// 切换当前区：只改"面板当前选择"（低风险），但切错了会误导后面的"重启机器人"，所以也确认一次
async function switchTo(key) {
  if (!key || key === currentKey.value) { zoneSel.value = currentKey.value; return }
  const z = zoneList.value.find((x) => x.key === key) || {}
  try {
    await ElMessageBox.confirm(
      `把「当前区」切到 ${z.name || key}（${z.addr || '--'} / ${z.coding || '--'}）？只改面板的当前选择与事件标记，不会碰机器人；真正切服还要再点「重启机器人」。`,
      '切换当前区',
      { type: 'warning', confirmButtonText: '切换', cancelButtonText: '取消' },
    )
  } catch (e) {
    zoneSel.value = currentKey.value // 取消：选择器回到真实当前区
    return
  }
  const res = await call('/api/config/switch', { key }, '已切换当前区')
  zoneSel.value = res.ok !== false ? key : currentKey.value
}

async function applyZone(key, restart) {
  const z = zoneList.value.find((x) => x.key === key) || {}
  const tip = `把【${z.addr || '--'} / ${z.coding || '--'}】写进机器人 config.py（${defaults.value.config_path || '部署目录/script/config.py'}）？` +
    '只改 ip / port / PROTOCOL_CODING 三个键，其余原样、写前自动备份；本部署的 ip/port 是环境变量表达式，会被跳过。'
  try {
    await ElMessageBox.confirm(tip, '应用到机器人', { type: 'warning', confirmButtonText: '写入', cancelButtonText: '取消' })
  } catch (e) {
    return
  }
  const res = await call('/api/config/apply', { zone: key, restart_robot: !!restart }, '已应用到机器人')
  if (res.ok) loadConfig()
}

// 2026-09-22 新增：重启机器人进程 —— "切区生效"的关键一步。
// 切区只改中控的当前选择；机器人进程还是连着旧服，必须重启才会按【当前区】
// 重新登录（中控启动机器人时会注入 ROBOT_ZONE_IP / ROBOT_ZONE_PORT）。
async function restartRobot() {
  const stt = state.status || {}
  const zone = curZone.value?.key || '（未选择）'
  try {
    await ElMessageBox.confirm(
      `重启机器人进程？将按【当前区 ${zone} → ${curZone.value?.addr || '--'}】重新登录，` +
      `当前在线 ${stt.counts?.online || 0} 个号会断开并自动重登（约 20~40 秒）。`,
      '重启机器人',
      { type: 'warning', confirmButtonText: '重启', cancelButtonText: '取消', confirmButtonClass: 'el-button--danger' },
    )
  } catch (e) {
    return
  }
  const res = await call('/api/robot/restart', {}, '机器人已重启')
  // 重启后号会自动重连；等几秒刷新一次配置里的进程状态
  if (res.ok) setTimeout(() => { loadConfig() }, 4000)
}

function openZoneDlg() {
  zoneEditing.value = false
  zoneForm.key = ''; zoneForm.name = ''; zoneForm.port = 2300; zoneForm.coding = ''; zoneForm.note = ''
  if (!zoneForm.server && servers.value.length) zoneForm.server = servers.value[0].key
  zoneDlg.value = true
  nextTick(() => zoneFormRef.value?.clearValidate())
}

function openServerDlg() {
  serverForm.key = ''; serverForm.name = ''; serverForm.host = ''; serverForm.note = ''
  serverDlg.value = true
  nextTick(() => serverFormRef.value?.clearValidate())
}

async function saveZone() {
  const ok = await zoneFormRef.value?.validate().catch(() => false)
  if (!ok) return
  const body = { server: zoneForm.server, zone: { ...zoneForm } }
  delete body.zone.server
  const res = await call('/api/config/zones', body, zoneEditing.value ? '已更新区' : '已新增区')
  if (res.ok) {
    await loadConfig()
    zoneForm.key = ''; zoneForm.name = ''; zoneForm.note = ''
    zoneDlg.value = false
  }
}

async function saveServer() {
  const ok = await serverFormRef.value?.validate().catch(() => false)
  if (!ok) return
  const res = await call('/api/config/servers', { server: { ...serverForm, zones: [] } }, '已保存服')
  if (res.ok) {
    serverForm.key = ''; serverForm.name = ''; serverForm.host = ''
    await loadConfig()
    serverDlg.value = false
  }
}

async function delZone(z) {
  try {
    await ElMessageBox.confirm(
      `删除区 ${z.key}？只删配置，不影响正在运行的机器人。`,
      '删除区',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消', confirmButtonClass: 'el-button--danger' },
    )
  } catch (e) {
    return
  }
  const res = await call('/api/config/zones/delete', { server: z.server_key, zone: z.zone_key }, '已删除区')
  if (res.ok) loadConfig()
}

async function delServer(s) {
  try {
    await ElMessageBox.confirm(
      `删除服 ${s.key}？其下 ${(s.zones || []).length} 个区会一并删除（只删配置，不影响正在运行的机器人）。`,
      '删除服',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消', confirmButtonClass: 'el-button--danger' },
    )
  } catch (e) {
    return
  }
  const res = await call('/api/config/servers/delete', { server: s.key }, '已删除服')
  if (res.ok) loadConfig()
}

function editZone(z) {
  zoneEditing.value = true
  zoneForm.server = z.server_key
  zoneForm.key = z.zone_key
  zoneForm.name = z.name
  zoneForm.port = z.port
  zoneForm.coding = z.coding
  zoneForm.note = z.note || ''
  zoneDlg.value = true
  nextTick(() => zoneFormRef.value?.clearValidate())
}

const st = computed(() => state.status || {})
const infoRows = computed(() => [
  ['版本', st.value.version || '--'],
  ['控制通道', `${st.value.ctrl_addr || '--'}（${st.value.robot_connected ? '已连接' : '未连接'}）`],
  ['机器人进程', `${st.value.robot_running ? '运行中' : '未运行'}（程序${st.value.robot_exe_exists ? '存在' : '缺失'}）`],
  ['当前区', curZone.value.key ? `${curZone.value.key} → ${curZone.value.addr}（${curZone.value.coding}）` : '--'],
  ['机器人部署目录', st.value.deploy_dir || '--'],
  ['机器人 config.py', defaults.value.config_path || '--'],
  ['游戏服地址（机器人上报）', st.value.server || '--'],
  ['数据目录', st.value.data_dir || '--'],
  ['多区配置文件', st.value.zones_file || '--'],
  ['链数据目录', st.value.chain_dir || '--'],
  ['当天运行历史', st.value.logs_file || '--'],
  ['WS 客户端', String(st.value.ws_clients ?? 0)],
])

// 协议表是二维数组：[列名...] + 行数组。el-table 的行最好是有稳定 key 的对象，
// 这里包一层 { i, cells }（协议数据是静态的，开销可忽略）。
const protoSections = computed(() => {
  const secs = protocols.value?.sections || []
  return secs.map((s) => ({ ...s, data: (s.rows || []).map((cells, i) => ({ i, cells })) }))
})
</script>

<template>
  <el-alert v-if="err" class="mb" type="error" :closable="false" show-icon :title="err" />

  <div class="card">
    <h3>运行信息</h3>
    <el-descriptions :column="2" border size="small">
      <el-descriptions-item v-for="[k, v] in infoRows" :key="k" :label="k">
        <span class="mono">{{ v }}</span>
      </el-descriptions-item>
    </el-descriptions>
  </div>

  <!-- ---------------- 多区配置（单进程：切换式） ---------------- -->
  <div class="card">
    <h3>多区配置（服 → 区）— 单机器人进程，切换后需「应用到机器人」</h3>
    <div class="row" style="margin-bottom: 10px">
      <span class="muted">当前区：</span>
      <el-select v-model="zoneSel" size="small" style="width: 300px" @change="switchTo(zoneSel)">
        <el-option value="" label="（未选择）" />
        <el-option v-for="z in zoneList" :key="z.key" :value="z.key" :label="`${z.name}（${z.addr} / ${z.coding}）`" />
      </el-select>
      <el-tag size="small" :type="currentKey ? 'success' : 'warning'" effect="plain">{{ currentKey || '未配置区' }}</el-tag>
      <span class="spacer" />
      <el-checkbox v-model="restartOnApply" size="small">应用后自动重启机器人</el-checkbox>
      <el-button size="small" @click="openServerDlg">
        <el-icon><Plus /></el-icon>
        <span>新增服</span>
      </el-button>
      <el-button size="small" type="primary" @click="openZoneDlg">
        <el-icon><Plus /></el-icon>
        <span>新增区</span>
      </el-button>
    </div>

    <el-table :data="zoneList" size="small" max-height="46vh" style="width: 100%">
      <el-table-column label="区" min-width="150">
        <template #default="{ row }">
          <div class="mono">{{ row.key }}</div>
          <div class="muted small">{{ row.name }}</div>
        </template>
      </el-table-column>
      <el-table-column label="游戏服地址" min-width="165">
        <template #default="{ row }"><span class="mono">{{ row.addr }}</span></template>
      </el-table-column>
      <el-table-column label="编码" width="96">
        <template #default="{ row }"><el-tag size="small" type="info" effect="plain">{{ row.coding }}</el-tag></template>
      </el-table-column>
      <el-table-column label="说明" min-width="180" show-overflow-tooltip>
        <template #default="{ row }"><span class="muted">{{ row.note || '--' }}</span></template>
      </el-table-column>
      <el-table-column label="状态" width="76">
        <template #default="{ row }">
          <el-tag v-if="row.current" size="small" type="success" effect="plain">当前</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="操作" width="286" align="right">
        <template #default="{ row }">
          <el-button link type="primary" size="small" :disabled="!!row.current" @click="switchTo(row.key)">切到此区</el-button>
          <el-button link type="primary" size="small" @click="applyZone(row.key, restartOnApply)">应用到机器人</el-button>
          <el-button link size="small" @click="editZone(row)">编辑</el-button>
          <el-button link type="danger" size="small" @click="delZone(row)">删除</el-button>
        </template>
      </el-table-column>
      <template #empty>
        <el-empty :image-size="56" description="还没有配置区：点右上「新增区」（先有服才有区）" />
      </template>
    </el-table>

    <!-- 2026-09-22 新增：切区生效的关键动作（重启机器人会按当前区重新登录） -->
    <div class="row" style="gap: 8px; margin-top: 10px; align-items: center">
      <el-button type="primary" @click="restartRobot">
        <el-icon><SwitchButton /></el-icon>
        <span>重启机器人（按当前区重新登录）</span>
      </el-button>
      <span class="muted">
        当前区：<span class="mono">{{ curZone.addr || '--' }}</span>
        · 切换区之后点这里才会真正切服（约 20~40 秒，号自动重登）
      </span>
    </div>

    <p class="muted" style="margin: 8px 0 0">
      「切换到某区」= 只改"当前选择"（面板上下文 + 事件标记），<b>不碰机器人</b>；
      「应用到机器人」= 把该区 <span class="mono">ip / port / PROTOCOL_CODING</span> 写进
      <span class="mono">{{ defaults.config_path || '&lt;部署目录&gt;/script/config.py' }}</span>
      （只改这三个键、其余原样、写前自动备份），重启机器人后生效。
      <br />
      <b>本部署注意</b>：config.py 的 ip/port 是<b>环境变量表达式</b>
      （<span class="mono">ROBOT_ZONE_IP / ROBOT_ZONE_PORT</span>），「应用到机器人」不会改写它们；
      正确流程是 <b>切到此区 → 重启机器人进程</b>（中控会按当前区自动注入这两个变量）。
    </p>
  </div>

  <div class="card">
    <h3>服务列表</h3>
    <div v-if="servers.length" class="row" style="gap: 6px">
      <el-tag
        v-for="s in servers" :key="s.key"
        class="srv-tag" closable type="info" effect="plain"
        @close="delServer(s)"
      >
        {{ s.name }}（{{ s.host }}，{{ (s.zones || []).length }} 区）
      </el-tag>
    </div>
    <el-empty v-else :image-size="56" description="还没有服：点上面「新增服」" />
    <p class="muted" style="margin: 10px 0 0">
      服只描述"游戏服 IP + 缺省编码"，区（端口）挂在服下面；点标签右侧的 × 可删服（会连带删掉它下面的区）。
    </p>
  </div>

  <div class="card">
    <h3>端口与部署约定</h3>
    <el-descriptions :column="2" border size="small">
      <el-descriptions-item label="HTTP API"><span class="mono">:28082（--web-port 可改）</span></el-descriptions-item>
      <el-descriptions-item label="控制通道"><span class="mono">:27200（--ctrl-port 可改）—— 机器人端 config.py 的 ctrl_server_port 指向这里</span></el-descriptions-item>
      <el-descriptions-item label="机器人部署目录"><span class="mono">{{ st.deploy_dir || '--' }}（单份 config.py；切区靠改它 + 重启机器人）</span></el-descriptions-item>
      <el-descriptions-item label="前端 dev server"><span class="mono">:5273</span><span class="muted"> （仅开发模式；日常面板由中控在 :28082 直接托管 web/dist）</span></el-descriptions-item>
    </el-descriptions>
  </div>

  <div class="card" v-if="protocols">
    <h3>协议映射（只读，事实协议见 docs/03-协议/）</h3>
    <div class="row" style="margin-bottom: 10px">
      <el-tag size="small" type="primary" effect="plain">v{{ protocols.current?.version }}</el-tag>
      <el-tag size="small" type="info" effect="plain">{{ protocols.current?.coding }}</el-tag>
      <el-tag size="small" type="info" effect="plain">控制通道 {{ protocols.current?.ctrl_addr }}</el-tag>
      <el-tag size="small" type="info" effect="plain">当前区 {{ protocols.current?.current_zone || '--' }}</el-tag>
      <el-tag size="small" :type="protocols.current?.connected ? 'success' : 'danger'" effect="plain">
        {{ protocols.current?.connected ? '通道已连接' : '通道未连接' }}
      </el-tag>
    </div>

    <div v-for="sec in protoSections" :key="sec.key" style="margin-bottom: 18px">
      <div style="margin-bottom: 6px">
        <b>{{ sec.title }}</b> <span class="muted">· {{ sec.desc }}</span>
      </div>
      <el-table :data="sec.data" size="small" max-height="40vh" style="width: 100%">
        <el-table-column
          v-for="(c, j) in sec.columns" :key="j"
          :label="c" :min-width="j === 0 ? 130 : 150" show-overflow-tooltip
        >
          <template #default="{ row }">
            <span :class="j === 0 ? 'mono' : ''">{{ row.cells[j] }}</span>
          </template>
        </el-table-column>
      </el-table>
    </div>
  </div>

  <!-- ---------------- 新增/编辑区 / 新增服 弹窗 ---------------- -->
  <el-dialog v-model="zoneDlg" :title="zoneEditing ? '更新区' : '新增区'" width="560px">
    <el-form ref="zoneFormRef" :model="zoneForm" :rules="zoneRules" label-width="104px" size="small">
      <el-form-item label="所属服" prop="server">
        <el-select v-model="zoneForm.server" style="width: 100%" placeholder="选择服">
          <el-option v-for="s in servers" :key="s.key" :value="s.key" :label="`${s.name}（${s.host}）`" />
        </el-select>
      </el-form-item>
      <el-form-item label="区 key" prop="key">
        <el-input v-model="zoneForm.key" placeholder="留空 = 用端口（如 2400）" />
      </el-form-item>
      <el-form-item label="展示名" prop="name">
        <el-input v-model="zoneForm.name" placeholder="如 1区" />
      </el-form-item>
      <el-form-item label="游戏服端口" prop="port">
        <el-input-number v-model="zoneForm.port" :min="1" :max="65535" style="width: 160px" />
      </el-form-item>
      <el-form-item label="编码" prop="coding">
        <el-select v-model="zoneForm.coding" style="width: 100%">
          <el-option value="" label="编码继承服" />
          <el-option value="UTF-8" label="UTF-8（默认）" />
          <el-option value="GBK" label="GBK" />
        </el-select>
      </el-form-item>
      <el-form-item label="说明" prop="note">
        <el-input v-model="zoneForm.note" placeholder="可选" />
      </el-form-item>
    </el-form>
    <p class="muted" style="margin: 0 0 0 104px">
      保存后可在上表「切到此区 / 应用到机器人」；同服同端口会覆盖已有区。
    </p>
    <template #footer>
      <el-button size="small" @click="zoneDlg = false">取消</el-button>
      <el-button size="small" type="primary" @click="saveZone">保存区</el-button>
    </template>
  </el-dialog>

  <el-dialog v-model="serverDlg" title="新增服" width="520px">
    <el-form ref="serverFormRef" :model="serverForm" :rules="serverRules" label-width="104px" size="small">
      <el-form-item label="服 key" prop="key">
        <el-input v-model="serverForm.key" placeholder="如 prod-240" />
      </el-form-item>
      <el-form-item label="展示名" prop="name">
        <el-input v-model="serverForm.name" placeholder="如 生产网关" />
      </el-form-item>
      <el-form-item label="游戏服 IP" prop="host">
        <el-input v-model="serverForm.host" placeholder="如 47.96.8.240（不能填域名）" />
      </el-form-item>
      <el-form-item label="缺省编码" prop="coding">
        <el-select v-model="serverForm.coding" style="width: 100%">
          <el-option value="UTF-8" label="UTF-8（默认）" />
          <el-option value="GBK" label="GBK" />
        </el-select>
      </el-form-item>
      <el-form-item label="说明" prop="note">
        <el-input v-model="serverForm.note" placeholder="可选" />
      </el-form-item>
    </el-form>
    <template #footer>
      <el-button size="small" @click="serverDlg = false">取消</el-button>
      <el-button size="small" type="primary" @click="saveServer">保存服</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.mb { margin-bottom: 12px; }
.srv-tag { margin: 0 6px 6px 0; }
/* 描述列表里的值可能是长路径/长地址：允许断行，避免撑出横向滚动 */
:deep(.el-descriptions__content) { word-break: break-all; }
</style>
