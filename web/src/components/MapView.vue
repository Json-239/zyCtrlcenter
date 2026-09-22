<script setup>
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { apiGet } from '../api'
import { state, mapLabel, posLabel, taskLabel, taskHint } from '../store'

const mapid = ref(0)
const cellPx = ref(3)
const grid = ref(null)
const mapName = ref('')
const err = ref('')
const loading = ref(false)
const picked = ref('')       // 选中的机器人账号
const hoverName = ref('')
const canvasRef = ref(null)

const maps = computed(() => state.maps || {})
const robots = computed(() => (state.status?.robots || []).filter((r) => r.online && r.mapid))

// 每张图当前在线人数
const onlineByMap = computed(() => {
  const m = {}
  for (const r of robots.value) m[r.mapid] = (m[r.mapid] || 0) + 1
  return m
})
// 地图下拉：有人的图优先，其次按 id
const mapOptions = computed(() => {
  const list = Object.entries(maps.value).map(([id, name]) => ({ id: Number(id), name, online: onlineByMap.value[id] || 0 }))
  list.sort((a, b) => (b.online - a.online) || (a.id - b.id))
  return list
})
const robotsOnMap = computed(() => robots.value.filter((r) => r.mapid === mapid.value))
const detail = computed(() => state.status?.robots?.find((r) => r.account === picked.value) || null)

async function loadGrid() {
  if (!mapid.value) return
  loading.value = true
  try {
    const d = await apiGet(`/api/map/grid?mapid=${mapid.value}`)
    if (!d.ok) { err.value = d.msg || '网格不可用'; grid.value = null; return }
    grid.value = d.grid
    mapName.value = d.name || ''
    err.value = ''
  } catch (e) {
    err.value = e.message
  } finally {
    loading.value = false
  }
}
watch(mapid, () => { loadGrid(); picked.value = '' })

onMounted(async () => {
  // 默认选"在线人数最多"的图，没有则选 1
  const first = mapOptions.value.find((m) => m.online > 0) || mapOptions.value.find((m) => m.id === 1) || mapOptions.value[0]
  if (first) mapid.value = first.id
})

// ---------------------------------------------------------------- 绘制

const COL_BLOCK = [41, 49, 60]
const COL_FREE = [122, 132, 148]
const STATE_COLORS = {
  FIGHT: '#ff5d6c', ERROR: '#f5b83d', DIALOG: '#4c8dff', NAV: '#46c6e6',
  SHOP: '#b18cff', ALLOC: '#b18cff', DONE: '#3ecf8e', IDLE: '#93a1c0',
}
let raf = 0
let lastDraw = 0
let sigPrev = ''
const FRAME_MIN_MS = 33      // 上限 ~30fps：地图是静态底图 + 少量点，没必要每帧重画
const anim = new Map()   // account -> {x, y, tx, ty}
let gridCache = { key: '', img: null }

function colorOf(r) {
  return STATE_COLORS[r.state] || '#4c8dff'
}

// 变化指纹：位置目标/选中/悬停/缩放/地图都没变就不重绘（空闲时不烧 CPU）
function frameSig() {
  let s = `${mapid.value}:${cellPx.value}:${picked.value}:${hoverName.value}`
  for (const r of robotsOnMap.value) s += `|${r.account},${r.pos?.[0] || 0},${r.pos?.[1] || 0},${r.state}`
  return s
}

function settled() {
  for (const a of anim.values()) {
    if (Math.abs(a.tx - a.x) > 0.6 || Math.abs(a.ty - a.y) > 0.6) return false
  }
  return true
}

function draw(now) {
  raf = requestAnimationFrame(draw)
  if (!grid.value) return
  const sig = frameSig()
  if (sig === sigPrev && settled()) return           // 静止且无变化：跳过（关键优化）
  if (now - lastDraw < FRAME_MIN_MS) return           // 限帧
  lastDraw = now
  sigPrev = sig

  const cvs = canvasRef.value
  const g = grid.value
  const cp = cellPx.value
  const W = g.w * cp
  const H = g.h * cp
  if (cvs.width !== W || cvs.height !== H) { cvs.width = W; cvs.height = H }

  const ctx = cvs.getContext('2d')
  // 底图（按 mapid+cp 缓存）
  const key = `${mapid.value}:${cp}:${g.w}x${g.h}`
  if (gridCache.key !== key) {
    const off = document.createElement('canvas')
    off.width = W; off.height = H
    const octx = off.getContext('2d')
    const img = octx.createImageData(W, H)
    for (let gy = 0; gy < g.h; gy++) {
      const row = g.rows[gy] || ''
      for (let gx = 0; gx < g.w; gx++) {
        const blocked = gx >= row.length || row[gx] === '1'
        const c = blocked ? COL_BLOCK : COL_FREE
        for (let py = 0; py < cp; py++) {
          for (let px = 0; px < cp; px++) {
            const idx = (((gy * cp) + py) * W + ((gx * cp) + px)) * 4
            img.data[idx] = c[0]; img.data[idx + 1] = c[1]; img.data[idx + 2] = c[2]; img.data[idx + 3] = 255
          }
        }
      }
    }
    octx.putImageData(img, 0, 0)
    gridCache = { key, img: off }
  }
  ctx.clearRect(0, 0, W, H)
  ctx.drawImage(gridCache.img, 0, 0)
  // 网格线（放大时）
  if (cp >= 6) {
    ctx.strokeStyle = 'rgba(0,0,0,.12)'
    ctx.beginPath()
    for (let x = 0; x <= g.w; x++) { ctx.moveTo(x * cp, 0); ctx.lineTo(x * cp, H) }
    for (let y = 0; y <= g.h; y++) { ctx.moveTo(0, y * cp); ctx.lineTo(W, y * cp) }
    ctx.stroke()
  }

  // 机器人点位（简易插值：向目标位置靠拢）
  const seen = new Set()
  for (const r of robotsOnMap.value) {
    seen.add(r.account)
    const tx = (r.pos?.[0] || 0) / 16 * cp
    const ty = (r.pos?.[1] || 0) / 16 * cp
    let a = anim.get(r.account)
    if (!a) { a = { x: tx, y: ty, tx, ty }; anim.set(r.account, a) }
    a.tx = tx; a.ty = ty
    a.x += (a.tx - a.x) * 0.18
    a.y += (a.ty - a.y) * 0.18

    const gx = Math.floor((r.pos?.[0] || 0) / 16)
    const gy = Math.floor((r.pos?.[1] || 0) / 16)
    const wall = g.rows[gy] ? (g.rows[gy][gx] === '1') : true
    const isPicked = r.account === picked.value
    const rad = Math.max(3, cp * 0.6)
    ctx.beginPath()
    ctx.arc(a.x, a.y, isPicked ? rad + 2 : rad, 0, Math.PI * 2)
    ctx.fillStyle = wall ? '#ff2d55' : colorOf(r)
    ctx.fill()
    if (isPicked || hoverName.value === r.account) {
      ctx.lineWidth = 2
      ctx.strokeStyle = '#fff'
      ctx.stroke()
      ctx.fillStyle = '#fff'
      ctx.font = '12px Consolas, monospace'
      ctx.fillText(r.account, a.x + rad + 2, a.y - rad)
    }
  }
  for (const k of Array.from(anim.keys())) if (!seen.has(k)) anim.delete(k)
}

onMounted(() => { raf = requestAnimationFrame(draw) })
onUnmounted(() => cancelAnimationFrame(raf))

// ---------------------------------------------------------------- 交互

function onClick(e) {
  if (!grid.value) return
  const rect = canvasRef.value.getBoundingClientRect()
  const mx = e.clientX - rect.left
  const my = e.clientY - rect.top
  const cp = cellPx.value
  let hit = ''
  for (const r of robotsOnMap.value) {
    const a = anim.get(r.account)
    if (!a) continue
    if (Math.hypot(a.x - mx, a.y - my) <= Math.max(6, cp)) { hit = r.account; break }
  }
  picked.value = hit || ''
}

function onMove(e) {
  if (!grid.value) return
  const rect = canvasRef.value.getBoundingClientRect()
  const mx = e.clientX - rect.left
  const my = e.clientY - rect.top
  const cp = cellPx.value
  let hit = ''
  for (const r of robotsOnMap.value) {
    const a = anim.get(r.account)
    if (!a) continue
    if (Math.hypot(a.x - mx, a.y - my) <= Math.max(6, cp)) { hit = r.account; break }
  }
  hoverName.value = hit
}

function hpPct(v) {
  if (!Array.isArray(v) || v.length < 2 || !v[1]) return 0
  return Math.max(0, Math.min(100, Math.round(v[0] / v[1] * 100)))
}
function bagList(r) { return Array.isArray(r.bag) ? r.bag : [] }
function summonsList(r) { return Array.isArray(r.summons) ? r.summons : [] }
</script>

<template>
  <div class="card">
    <div class="row">
      <select v-model.number="mapid" style="min-width: 300px">
        <option v-for="m in mapOptions" :key="m.id" :value="m.id">
          {{ m.name || ('#' + m.id) }}（{{ m.id }}）{{ m.online ? ' · 在线 ' + m.online : '' }}
        </option>
      </select>
      <label class="muted">缩放</label>
      <input v-model.number="cellPx" type="range" min="2" max="8" step="1" />
      <span class="tag dim">{{ cellPx }} px/格</span>
      <span class="spacer" />
      <span class="tag" :class="robotsOnMap.length ? 'ok' : 'dim'">本图在线 {{ robotsOnMap.length }}</span>
      <span class="tag dim">图上共 {{ grid ? `${grid.w}×${grid.h} 格` : '--' }}</span>
      <span v-if="loading" class="tag info">加载中…</span>
    </div>
  </div>

  <div v-if="err" class="card"><span class="tag danger">{{ err }}</span></div>

  <div class="map-layout" :class="{ 'has-detail': !!detail }">
    <div class="card map-canvas-card">
      <div class="row" style="margin-bottom:8px">
        <b>{{ mapName || mapLabel(mapid) }}</b>
        <span class="muted">mapid={{ mapid }} · 点击圆点查看机器人详情</span>
      </div>
      <div class="canvas-wrap">
        <canvas ref="canvasRef" @click="onClick" @mousemove="onMove" @mouseleave="hoverName = ''" />
      </div>
      <div class="row" style="margin-top:8px">
        <span class="tag dim">可走</span><span class="swatch free"></span>
        <span class="tag dim">阻挡</span><span class="swatch block"></span>
        <span class="muted">点位颜色 = 状态；<span style="color:#ff2d55">红点</span>=站在阻挡格（异常）</span>
      </div>
    </div>

    <!-- 详情面板 -->
    <div v-if="detail" class="card detail">
      <div class="row" style="margin-bottom:8px">
        <b class="mono">{{ detail.account }}</b>
        <span class="spacer" />
        <button class="btn sm" @click="picked = ''">关闭</button>
      </div>
      <table class="kv-table">
        <tbody>
          <tr><td class="muted">角色</td><td>{{ detail.role_name || '--' }} Lv{{ detail.level || '--' }}</td></tr>
          <tr><td class="muted">状态</td><td><span class="tag info">{{ detail.state || '--' }}</span>
            <span class="tag dim">{{ detail.online ? '在线' : '离线' }}</span>
            <span class="tag" :class="detail.hs ? 'ok' : 'warn'">{{ detail.hs ? '已握手' : '未握手' }}</span></td></tr>
          <tr><td class="muted">区 / 地图</td><td>{{ detail.zone || '--' }} · {{ mapLabel(detail.mapid) }}</td></tr>
          <tr><td class="muted">坐标(格) / 像素</td>
            <td class="mono">{{ posLabel(detail.pos) }} / {{ (detail.pos || []).join(', ') || '--' }}</td></tr>
          <tr><td class="muted">任务</td><td class="mono" :title="taskHint(detail.task_index)">{{ taskLabel(detail.task_index) }} · 进度 {{ detail.done || 0 }}</td></tr>
          <tr v-if="detail.err_code"><td class="muted">最近错误</td>
            <td><span class="tag danger">{{ detail.err_code }} ×{{ detail.err_repeat || 1 }}</span>
              <span class="muted">{{ detail.err_msg }}</span></td></tr>
        </tbody>
      </table>

      <!-- 血量/法力 -->
      <div class="bars">
        <div class="bar-row">
          <span class="lab">HP</span>
          <div class="bar"><div class="fill hp" :style="{ width: hpPct(detail.hp) + '%' }" /></div>
          <span class="mono">{{ (detail.hp || []).join(' / ') || '--' }}</span>
        </div>
        <div class="bar-row">
          <span class="lab">MP</span>
          <div class="bar"><div class="fill mp" :style="{ width: hpPct(detail.mp) + '%' }" /></div>
          <span class="mono">{{ (detail.mp || []).join(' / ') || '--' }}</span>
        </div>
      </div>

      <!-- 守护 -->
      <div class="sec">守护（{{ summonsList(detail).length }}）</div>
      <table v-if="summonsList(detail).length" class="mini">
        <thead><tr><th>名称</th><th>等级</th><th>忠诚</th><th>亲密度</th><th>HP</th><th>出战</th></tr></thead>
        <tbody>
          <tr v-for="(s, i) in summonsList(detail)" :key="i">
            <td>{{ s.name || ('#' + s.id) }}</td>
            <td>{{ s.level ?? '--' }}</td>
            <td>{{ s.happy ?? '--' }}</td>
            <td>{{ s.close ?? '--' }}</td>
            <td class="mono">{{ (s.hp || []).join('/') || '--' }}</td>
            <td><span class="tag" :class="s.fighting ? 'ok' : 'dim'">{{ s.fighting ? '出战' : '休息' }}</span></td>
          </tr>
        </tbody>
      </table>
      <div v-else class="muted" style="padding:4px 0">（无守护数据）</div>

      <!-- 背包 -->
      <div class="sec">背包（{{ bagList(detail).length }}）</div>
      <div v-if="bagList(detail).length" class="bag">
        <div v-for="(it, i) in bagList(detail)" :key="i" class="bag-item" :title="'itemid=' + it.id">
          <span class="bname">{{ it.name || ('#' + it.id) }}</span>
          <span class="bcount" v-if="it.count > 1">×{{ it.count }}</span>
        </div>
      </div>
      <div v-else class="muted" style="padding:4px 0">（无背包数据）</div>

      <!-- 摆摊 -->
      <div v-if="detail.booth" class="sec">摆摊</div>
      <table v-if="detail.booth" class="mini">
        <tbody>
          <tr><td class="muted">状态</td><td>{{ detail.booth.state }}</td>
            <td class="muted">已售</td><td>{{ detail.booth.sold ?? 0 }}</td>
            <td class="muted">收入</td><td>{{ detail.booth.income ?? 0 }}</td></tr>
        </tbody>
      </table>
    </div>
  </div>
</template>

<style scoped>
.map-layout { display: grid; grid-template-columns: 1fr; gap: 14px; }
.map-layout.has-detail { grid-template-columns: minmax(0, 1fr) 380px; }
@media (max-width: 1200px) { .map-layout.has-detail { grid-template-columns: 1fr; } }
.canvas-wrap { overflow: auto; max-height: 68vh; border: 1px solid var(--border); border-radius: 8px; background: #0b0f18; }
canvas { display: block; cursor: crosshair; }
.swatch { display: inline-block; width: 14px; height: 10px; border-radius: 2px; }
.swatch.free { background: rgb(122,132,148); }
.swatch.block { background: rgb(41,49,60); }
.detail { max-height: 78vh; overflow: auto; }
.kv-table { width: 100%; }
.kv-table td { padding: 3px 6px; border: none; }
.bars { margin: 8px 0 4px; }
.bar-row { display: flex; align-items: center; gap: 8px; margin-bottom: 4px; }
.bar-row .lab { width: 26px; color: var(--muted); font-size: 12px; }
.bar { flex: 1; height: 8px; background: var(--bg); border: 1px solid var(--border); border-radius: 6px; overflow: hidden; }
.fill { height: 100%; }
.fill.hp { background: linear-gradient(90deg, #3ecf8e, #2f9f6d); }
.fill.mp { background: linear-gradient(90deg, #4c8dff, #2f6fd0); }
.sec { margin: 12px 0 6px; color: var(--text-dim); font-size: 13px; font-weight: 600; border-bottom: 1px solid var(--border); padding-bottom: 4px; }
table.mini th, table.mini td { padding: 3px 6px; font-size: 12px; }
.bag { display: flex; flex-wrap: wrap; gap: 6px; }
.bag-item { background: var(--panel-2); border: 1px solid var(--border); border-radius: 6px; padding: 3px 8px; font-size: 12px; }
.bag-item .bcount { color: var(--accent); margin-left: 4px; }
</style>
