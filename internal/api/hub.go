// Package api 提供 WebSocket 广播中心（WSHub）与 HTTP API。
package api

import (
	"encoding/json"
	"sync"

	"zyctrlcenter/internal/wsutil"
)

// WSHub 前端 WebSocket 客户端中心（并发安全）。
//
// 每个客户端有独立发送队列（256 条）——慢客户端（浏览器卡顿）不阻塞事件循环：
// 队列写满时直接断开该客户端（前端会自动重连）。
type WSHub struct {
	mu      sync.Mutex
	clients map[*wsClient]bool
	logf    func(string, ...any)
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
	return &WSHub{clients: map[*wsClient]bool{}, logf: logf}
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
func (h *WSHub) Broadcast(obj map[string]any) {
	data, err := json.Marshal(obj)
	if err != nil {
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

func (c *wsClient) writer() {
	for {
		select {
		case <-c.done:
			return
		case data := <-c.ch:
			if err := c.conn.WriteText(data); err != nil {
				_ = c.conn.Close()
				close(c.done)
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
