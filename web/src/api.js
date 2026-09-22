// 中控 API 封装：GET/POST + WebSocket（自动重连）。
// 同源访问（dev 下由 vite proxy 转发到 18082）。

export async function apiGet(path) {
  const resp = await fetch(path, { headers: { Accept: 'application/json' } })
  if (!resp.ok) {
    throw new Error(`GET ${path} -> HTTP ${resp.status}`)
  }
  return resp.json()
}

export async function apiPost(path, body = {}) {
  const resp = await fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  const data = await resp.json().catch(() => ({}))
  if (resp.status === 401) {
    throw new Error(data.msg || '未授权（需要 API token）')
  }
  if (!resp.ok) {
    throw new Error(`POST ${path} -> HTTP ${resp.status}`)
  }
  return data
}

// connectWS：连接 /ws；onEvent(evt) 收事件，onState(connected) 报告连接状态。
// 断线每 3 秒自动重连；每 25 秒发一次 ping 保活（服务端会回 pong）。
export function connectWS(onEvent, onState) {
  let ws = null
  let closed = false
  let pingTimer = null
  let retryTimer = null

  const open = () => {
    if (closed) return
    const proto = location.protocol === 'https:' ? 'wss' : 'ws'
    ws = new WebSocket(`${proto}://${location.host}/ws`)

    ws.onopen = () => {
      onState?.(true)
      clearInterval(pingTimer)
      pingTimer = setInterval(() => {
        try { ws.send('ping') } catch (e) { /* 忽略：下次重连处理 */ }
      }, 25000)
    }
    ws.onmessage = (e) => {
      try {
        onEvent?.(JSON.parse(e.data))
      } catch (err) { /* 非法消息忽略 */ }
    }
    ws.onclose = () => {
      onState?.(false)
      clearInterval(pingTimer)
      if (!closed) retryTimer = setTimeout(open, 3000)
    }
    ws.onerror = () => { try { ws.close() } catch (e) { /* 忽略 */ } }
  }

  open()
  return () => {
    closed = true
    clearTimeout(retryTimer)
    clearInterval(pingTimer)
    try { ws?.close() } catch (e) { /* 忽略 */ }
  }
}
