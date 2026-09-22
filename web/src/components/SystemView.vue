<script setup>
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { apiGet } from '../api'
import { state, post, zones, currentZoneKey, switchZone } from '../store'

const config = ref(null)
const protocols = ref(null)
const err = ref('')
const zoneSel = ref('')
const restartOnApply = ref(true)

// 新增/编辑区表单（单进程：区只描述"游戏服地址 + 编码"）
const zoneForm = reactive({ server: '', key: '', name: '', port: 2300, coding: '', note: '' })
// 新增服表单
const serverForm = reactive({ key: '', name: '', host: '', coding: 'UTF-8', note: '' })

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

function switchTo(key) { switchZone(key) }

async function applyZone(key, restart) {
  const z = zoneList.value.find((x) => x.key === key) || {}
  const tip = `把【${z.addr} / ${z.coding}】写进机器人 config.py？\n路径：${defaults.value.config_path || ''}\n（只改 ip/port/PROTOCOL_CODING 三个键，写前自动备份）`
  if (!confirm(tip)) return
  const res = await post('/api/config/apply', { zone: key, restart_robot: !!restart })
  if (res.ok) loadConfig()
}

async function saveZone() {
  const body = { server: zoneForm.server, zone: { ...zoneForm } }
  delete body.zone.server
  const res = await post('/api/config/zones', body)
  if (res.ok) { await loadConfig(); zoneForm.key = ''; zoneForm.name = ''; zoneForm.note = '' }
}

async function saveServer() {
  const res = await post('/api/config/servers', { server: { ...serverForm, zones: [] } })
  if (res.ok) { serverForm.key = ''; serverForm.name = ''; serverForm.host = ''; await loadConfig() }
}

async function delZone(z) {
  if (!confirm(`删除区 ${z.key}？（只删配置，不影响正在运行的机器人）`)) return
  const res = await post('/api/config/zones/delete', { server: z.server_key, zone: z.zone_key })
  if (res.ok) loadConfig()
}
async function delServer(s) {
  if (!confirm(`删除服 ${s.key}？其下所有区一并删除。`)) return
  const res = await post('/api/config/servers/delete', { server: s.key })
  if (res.ok) loadConfig()
}

function editZone(z) {
  zoneForm.server = z.server_key
  zoneForm.key = z.zone_key
  zoneForm.name = z.name
  zoneForm.port = z.port
  zoneForm.coding = z.coding
  zoneForm.note = z.note || ''
  window.scrollTo({ top: 0, behavior: 'smooth' })
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
</script>

<template>
  <div v-if="err" class="card"><span class="tag danger">{{ err }}</span></div>

  <div class="card">
    <h3>运行信息</h3>
    <table>
      <tbody>
        <tr v-for="[k, v] in infoRows" :key="k">
          <td class="muted" style="width: 200px">{{ k }}</td>
          <td class="mono">{{ v }}</td>
        </tr>
      </tbody>
    </table>
  </div>

  <!-- ---------------- 多区配置（单进程：切换式） ---------------- -->
  <div class="card">
    <h3>多区配置（服 → 区）— 单机器人进程，切换后需「应用到机器人」</h3>
    <div class="row" style="margin-bottom: 10px">
      <span class="muted">当前区：</span>
      <select v-model="zoneSel" @change="switchTo(zoneSel)" style="min-width: 280px">
        <option value="">（未选择）</option>
        <option v-for="z in zoneList" :key="z.key" :value="z.key">
          {{ z.name }}（{{ z.addr }} / {{ z.coding }}）
        </option>
      </select>
      <span class="tag" :class="currentKey ? 'ok' : 'warn'">{{ currentKey || '未配置区' }}</span>
      <span class="spacer" />
      <label class="row muted" style="gap: 4px">
        <input v-model="restartOnApply" type="checkbox" /> 应用后自动重启机器人
      </label>
    </div>

    <div class="table-wrap" style="max-height: 46vh">
      <table>
        <thead>
          <tr><th>区</th><th>游戏服地址</th><th>编码</th><th>说明</th><th>状态</th><th>操作</th></tr>
        </thead>
        <tbody>
          <tr v-for="z in zoneList" :key="z.key">
            <td>
              <span class="mono">{{ z.key }}</span>
              <div class="muted" style="font-size: 12px">{{ z.name }}</div>
            </td>
            <td class="mono">{{ z.addr }}</td>
            <td><span class="tag dim">{{ z.coding }}</span></td>
            <td class="muted" style="max-width: 320px; overflow: hidden; text-overflow: ellipsis">{{ z.note || '--' }}</td>
            <td><span v-if="z.current" class="tag ok">当前</span></td>
            <td>
              <div class="row" style="gap: 6px">
                <button class="btn sm" @click="switchTo(z.key)">切到此区</button>
                <button class="btn sm primary" @click="applyZone(z.key, restartOnApply)">应用到机器人</button>
                <button class="btn sm" @click="editZone(z)">编辑</button>
                <button class="btn sm danger" @click="delZone(z)">删除</button>
              </div>
            </td>
          </tr>
          <tr v-if="!zoneList.length">
            <td colspan="6"><div class="empty">还没有配置区：用下面的表单新增「服」和「区」</div></td>
          </tr>
        </tbody>
      </table>
    </div>

    <p class="muted" style="margin: 8px 0 0">
      「切换到某区」= 只改"当前选择"（面板上下文 + 事件标记），<b>不碰机器人</b>；
      「应用到机器人」= 把该区 <span class="mono">ip / port / PROTOCOL_CODING</span> 写进
      <span class="mono">{{ defaults.config_path || '&lt;部署目录&gt;/script/config.py' }}</span>
      （只改这三个键、其余原样、写前自动备份），重启机器人后生效。
    </p>
  </div>

  <div class="grid cols-2">
    <div class="card">
      <h3>新增 / 更新区</h3>
      <div class="row" style="margin-bottom: 8px">
        <select v-model="zoneForm.server" style="min-width: 180px">
          <option v-for="s in servers" :key="s.key" :value="s.key">{{ s.name }}（{{ s.host }}）</option>
        </select>
        <input v-model="zoneForm.key" type="text" placeholder="区 key（留空=用端口）" style="width: 170px" />
        <input v-model="zoneForm.name" type="text" placeholder="展示名，如 1区" style="width: 120px" />
      </div>
      <div class="row" style="margin-bottom: 8px">
        <input v-model.number="zoneForm.port" type="number" placeholder="游戏服端口(2300)" style="width: 150px" />
        <select v-model="zoneForm.coding" style="width: 150px">
          <option value="">编码继承服</option>
          <option value="UTF-8">UTF-8（默认）</option>
          <option value="GBK">GBK</option>
        </select>
        <input v-model="zoneForm.note" type="text" placeholder="说明（可选）" style="width: 200px" />
      </div>
      <div class="row">
        <button class="btn primary" @click="saveZone">保存区</button>
        <span class="muted">保存后可在上表「切到此区 / 应用到机器人」</span>
      </div>
    </div>

    <div class="card">
      <h3>新增服</h3>
      <div class="row" style="margin-bottom: 8px">
        <input v-model="serverForm.key" type="text" placeholder="服 key，如 prod-240" style="width: 170px" />
        <input v-model="serverForm.name" type="text" placeholder="展示名，如 生产网关" style="width: 150px" />
      </div>
      <div class="row" style="margin-bottom: 8px">
        <input v-model="serverForm.host" type="text" placeholder="游戏服 IP（不能填域名）" style="width: 220px" />
        <select v-model="serverForm.coding" style="width: 150px">
          <option value="UTF-8">UTF-8（默认）</option>
          <option value="GBK">GBK</option>
        </select>
      </div>
      <div class="row">
        <button class="btn primary" @click="saveServer">保存服</button>
        <span class="muted">保存后再到左侧为该服添加区</span>
      </div>
      <p class="muted" style="margin: 10px 0 0">
        服列表：
        <span v-for="s in servers" :key="s.key" class="tag dim" style="margin-right: 6px">
          {{ s.name }}（{{ s.host }}，{{ (s.zones || []).length }} 区）
          <a href="#" @click.prevent="delServer(s)" style="margin-left: 4px">删</a>
        </span>
      </p>
    </div>
  </div>

  <div class="card">
    <h3>端口与部署约定</h3>
    <table>
      <tbody>
        <tr><td class="muted" style="width: 200px">HTTP API</td><td class="mono">:28082（--web-port 可改）</td></tr>
        <tr><td class="muted">控制通道</td><td class="mono">:27200（--ctrl-port 可改）—— 机器人端 config.py 的 ctrl_server_port 指向这里</td></tr>
        <tr><td class="muted">机器人部署目录</td><td class="mono">{{ st.deploy_dir || '--' }}（单份 config.py；切区靠改它 + 重启机器人）</td></tr>
        <tr><td class="muted">前端 dev server</td><td class="mono">:5273</td></tr>
      </tbody>
    </table>
  </div>

  <div class="card" v-if="protocols">
    <h3>协议映射（只读，事实协议见 docs/03-协议/）</h3>
    <div class="row" style="margin-bottom: 10px">
      <span class="tag info">v{{ protocols.current?.version }}</span>
      <span class="tag dim">{{ protocols.current?.coding }}</span>
      <span class="tag dim">控制通道 {{ protocols.current?.ctrl_addr }}</span>
      <span class="tag dim">当前区 {{ protocols.current?.current_zone || '--' }}</span>
      <span class="tag" :class="protocols.current?.connected ? 'ok' : 'danger'">
        {{ protocols.current?.connected ? '通道已连接' : '通道未连接' }}
      </span>
    </div>

    <div v-for="sec in protocols.sections" :key="sec.key" style="margin-bottom: 18px">
      <div style="margin-bottom: 6px">
        <b>{{ sec.title }}</b> <span class="muted">· {{ sec.desc }}</span>
      </div>
      <div class="table-wrap" style="max-height: 40vh">
        <table>
          <thead><tr><th v-for="c in sec.columns" :key="c">{{ c }}</th></tr></thead>
          <tbody>
            <tr v-for="(row, i) in sec.rows" :key="i">
              <td v-for="(cell, j) in row" :key="j" :class="j === 0 ? 'mono' : ''">{{ cell }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  </div>
</template>
