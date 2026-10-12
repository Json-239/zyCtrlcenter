// Package api 提供 WebSocket 广播中心（WSHub）与 HTTP API。
package api

import (
	"encoding/json"
	"sync"
	"time"

	"zyctrlcenter/internal/wsutil"
)

// BroadcastMaxBytes 单条 WS 广播上限（序列化后，2026-10-12 性能止血）：
// 超限不广播 + 限频日志 —— 防止一笔异常大的事件（如误带全量数据的回复）把客户端队列打爆。
const BroadcastMaxBytes = 64 * 1024

// broadcastSkipTypes 前端零消费的高频事件类型（跳过广播；2026-10-12 实测确证）：
//
//	status_reply —— 中控每 3s 向机器人请求状态（轮询）的心跳回复：~41KB/条（robots[].bag 占大头），
//	实测 ~364KB/s = WS 广播总量的 93%。前端消费清单（web/src/store.js applyEventToRow:
//	robot_online/robot_offline/robot_state/task_progress/chain_done/error + robot_pos_batch）
//	不含它，web/src 全目录 grep "status_reply" 零引用；同数据已由 /api/status 与 robot_state 覆盖。
//	（机器人端该事件的 state 更新/日志落库不受影响——只跳过 WS 广播。）
var broadcastSkipTypes = map[string]bool{"status_reply": true}

// WSHub 前端 WebSocket 客户端中心（并发安全）。
//
// 每个客户端有独立发送队列（256 条）——慢客户端（浏览器卡顿）不阻塞事件循环：
// 队列写满时直接断开该客户端（前端会自动重连）。
type WSHub struct {
	mu      sync.Mutex
	clients map[*wsClient]bool
	logf    func(string, ...any)
	// skipMu / skipLogAt 超限广播日志限频（同 type 60s 至多一条；2026-10-12）。
	skipMu    sync.Mutex
	skipLogAt map[string]time.Time
}

type wsClient struct {
	conn *wsutil.Conn
	ch   chan []byte
	once sync.Once
	done chan struct{}
}

// NewWSHub 创建广播中心。
func NewWSHub(logf func(string, ...any)) *WSHub {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &WSHub{clients: map[*wsClient]bool{}, logf: logf, skipLogAt: map[string]time.Time{}}
}

// Count 当前连接数。
func (h *WSHub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// addClient 注册客户端并启动发送协程。
func (h *WSHub) addClient(conn *wsutil.Conn) *wsClient {
	c := &wsClient{conn: conn, ch: make(chan []byte, 256), done: make(chan struct{})}
	h.mu.Lock()
	h.clients[c] = true
	n := len(h.clients)
	h.mu.Unlock()
	go c.writer()
	h.logf("[WS] 客户端接入 %s（当前 %d）", conn.RemoteAddr(), n)
	return c
}

// removeClient 注销客户端。
func (h *WSHub) removeClient(c *wsClient) {
	h.mu.Lock()
	_, ok := h.clients[c]
	delete(h.clients, c)
	n := len(h.clients)
	h.mu.Unlock()
	if ok {
		c.close()
		h.logf("[WS] 客户端断开（当前 %d）", n)
	}
}

// Broadcast 向所有客户端推送一条 JSON 消息。
//
// 2026-10-12 性能止血两道闸（顺序：类型跳过 → 序列化 → 大小上限）：
//   - broadcastSkipTypes（前端零消费的高频类型，见名单注释）直接跳过；
//   - 序列化后 > BroadcastMaxBytes 不广播（限频日志，防刷屏）。
func (h *WSHub) Broadcast(obj map[string]any) {
	if t, _ := obj["type"].(string); broadcastSkipTypes[t] {
		return
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return
	}
	if !broadcastSizeAllowed(len(data)) {
		h.logOversize(obj, len(data))
		return
	}
	h.mu.Lock()
	targets := make([]*wsClient, 0, len(h.clients))
	for c := range h.clients {
		targets = append(targets, c)
	}
	h.mu.Unlock()
	for _, c := range targets {
		select {
		case c.ch <- data:
		default:
			h.logf("[WS] 客户端队列积压，断开慢客户端 %s", c.conn.RemoteAddr())
			h.removeClient(c)
		}
	}
}

// broadcastSizeAllowed 序列化后是否允许广播（纯函数，边界便于单测）。
func broadcastSizeAllowed(n int) bool { return n <= BroadcastMaxBytes }

// logOversize 超限广播的限频日志（同 type 60s 至多一条）。
func (h *WSHub) logOversize(obj map[string]any, size int) {
	t, _ := obj["type"].(string)
	if t == "" {
		t = "?"
	}
	now := time.Now()
	h.skipMu.Lock()
	if last, ok := h.skipLogAt[t]; ok && now.Sub(last) < time.Minute {
		h.skipMu.Unlock()
		return
	}
	h.skipLogAt[t] = now
	h.skipMu.Unlock()
	h.logf("[WS] 跳过超限广播 type=%s size=%d（上限 %d）", t, size, BroadcastMaxBytes)
}

func (c *wsClient) writer() {
	for {
		select {
		case <-c.done:
			return
		case data := <-c.ch:
			if err := c.conn.WriteText(data); err != nil {
				// 2026-10-10 修复"无征兆死亡"根因：原实现直接 close(c.done)，
				// 与 close() 的 once.Do 路径并发时=close of closed channel → panic → 进程崩溃
				// （现场 16:08/17:16/19:06 三例均紧随"[WS] 队列积压断开慢客户端"之后）。
				// 统一走 once 守卫的 close()。
				c.close()
				return
			}
		}
	}
}

func (c *wsClient) close() {
	c.once.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}
