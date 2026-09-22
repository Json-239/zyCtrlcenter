// 假游戏服（TCP 服务端）——测试基座的一部分。
//
// 为什么需要它：控制通道那侧我们已有 FakeRobot（**客户端**）；而"中控直接连游戏服探测账号"
// 需要**服务端**侧才能测，所以这里补一个可脚本化的假游戏服：
//   - 监听 127.0.0.1:0（系统分配端口），按 gameproto 帧格式收发；
//   - 每个收到的帧都记录下来（可按 msgid 取回，供断言"客户端发了什么"）；
//   - 回什么完全由 handler 决定（返回完整帧字节；返回 nil = 不回，用于测超时）。
//
// 它**不懂协议语义**（不知道 300/90132 是什么），语义由用例自己描述 —— 这样基座可以复用到
// 任何"中控 ↔ 游戏服"的协议测试上。
package testsupport

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"zyctrlcenter/internal/gameproto"
)

// GameConn 一条被假游戏服接受的连接（记录该连接收到的帧）。
type GameConn struct {
	ID int

	mu   sync.Mutex
	recv [][2]any // [msgID(int32), body([]byte)]
}

// Received 返回该连接收到的指定 msgid 的帧体（按到达顺序）。
func (c *GameConn) Received(msgID int32) [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, 0, len(c.recv))
	for _, it := range c.recv {
		if it[0].(int32) == msgID {
			out = append(out, it[1].([]byte))
		}
	}
	return out
}

// All 返回该连接收到的全部 (msgID, body)（按到达顺序）。
func (c *GameConn) All() []struct {
	MsgID int32
	Body  []byte
} {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]struct {
		MsgID int32
		Body  []byte
	}, 0, len(c.recv))
	for _, it := range c.recv {
		out = append(out, struct {
			MsgID int32
			Body  []byte
		}{it[0].(int32), it[1].([]byte)})
	}
	return out
}

// GameServer 假游戏服。
type GameServer struct {
	Addr string

	t  *testing.T
	ln net.Listener

	mu    sync.Mutex
	recv  [][2]any // 全部连接合并
	conns []*GameConn
}

// StartGameServer 启动假游戏服；handler 收到 (连接, msgid, 帧体) 后返回要回写的**完整帧字节**
// （用 gameproto 构造）。返回 nil 表示这条不回（可用于测客户端超时）。
func StartGameServer(t *testing.T, handler func(c *GameConn, msgID int32, body []byte) [][]byte) *GameServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("假游戏服监听失败: %v", err)
	}
	gs := &GameServer{Addr: ln.Addr().String(), t: t, ln: ln}
	t.Cleanup(gs.Close)
	go gs.acceptLoop(handler)
	return gs
}

// QueueReplies 便捷 handler：按 msgid 队列回放（同 msgid 的第 n 次请求回第 n 条；用完不再回）。
// 适合"一次性脚本"式用例；需要按账号区分回包的用例请直接写 handler。
func QueueReplies(rules map[int32][][]byte) func(c *GameConn, msgID int32, body []byte) [][]byte {
	var mu sync.Mutex
	idx := map[int32]int{}
	return func(c *GameConn, msgID int32, body []byte) [][]byte {
		mu.Lock()
		defer mu.Unlock()
		list := rules[msgID]
		i := idx[msgID]
		if i >= len(list) {
			return nil
		}
		idx[msgID] = i + 1
		return [][]byte{list[i]}
	}
}

func (g *GameServer) acceptLoop(handler func(c *GameConn, msgID int32, body []byte) [][]byte) {
	for {
		conn, err := g.ln.Accept()
		if err != nil {
			return
		}
		g.mu.Lock()
		gc := &GameConn{ID: len(g.conns)}
		g.conns = append(g.conns, gc)
		g.mu.Unlock()
		go g.serve(conn, gc, handler)
	}
}

func (g *GameServer) serve(conn net.Conn, gc *GameConn, handler func(c *GameConn, msgID int32, body []byte) [][]byte) {
	defer conn.Close()
	if handler == nil {
		return
	}
	for {
		msgID, body, err := gameproto.ReadFrame(conn)
		if err != nil {
			return // 客户端断开/半包：结束该连接
		}
		gc.mu.Lock()
		gc.recv = append(gc.recv, [2]any{msgID, append([]byte(nil), body...)})
		gc.mu.Unlock()
		g.mu.Lock()
		g.recv = append(g.recv, [2]any{msgID, append([]byte(nil), body...)})
		g.mu.Unlock()

		for _, raw := range handler(gc, msgID, body) {
			if len(raw) == 0 {
				continue
			}
			if _, err := conn.Write(raw); err != nil {
				return
			}
		}
	}
}

// Received 全部连接合并后，指定 msgid 的帧体。
func (g *GameServer) Received(msgID int32) [][]byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([][]byte, 0, len(g.recv))
	for _, it := range g.recv {
		if it[0].(int32) == msgID {
			out = append(out, it[1].([]byte))
		}
	}
	return out
}

// MsgIDs 收到的全部 msgid（按到达顺序）——便于断言握手顺序。
func (g *GameServer) MsgIDs() []int32 {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]int32, 0, len(g.recv))
	for _, it := range g.recv {
		out = append(out, it[0].(int32))
	}
	return out
}

// ConnCount 已接受的连接数。
func (g *GameServer) ConnCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.conns)
}

// Conn 取第 i 条连接（不存在返回 nil）。
func (g *GameServer) Conn(i int) *GameConn {
	g.mu.Lock()
	defer g.mu.Unlock()
	if i < 0 || i >= len(g.conns) {
		return nil
	}
	return g.conns[i]
}

// WaitRecv 等某 msgid 收到至少 n 条（超时返回 false）。
func (g *GameServer) WaitRecv(msgID int32, n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(g.Received(msgID)) >= n {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// Close 关闭监听（已接受的连接随 handler 自然结束）。
func (g *GameServer) Close() {
	if g.ln != nil {
		_ = g.ln.Close()
	}
}

// FrameBytes 构造一帧（测试里写 handler 回包用；等价于 gameproto.WriteFrame 到内存）。
func FrameBytes(msgID int32, fmtv []any, vals []any, coding string) []byte {
	raw, err := gameproto.PackFrame(msgID, fmtv, vals, coding)
	if err != nil {
		panic(err)
	}
	return raw
}

// UnpackInt32 读帧体首个 int32（测试断言用）。
func UnpackInt32(body []byte) int32 {
	if len(body) < 4 {
		return -1
	}
	return int32(binary.LittleEndian.Uint32(body[:4]))
}

// ReadAllWithTimeout 读满 n 字节或超时（个别用例需要裸读）。
func ReadAllWithTimeout(r io.Reader, n int, timeout time.Duration) ([]byte, bool) {
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		buf := make([]byte, n)
		_, err := io.ReadFull(r, buf)
		ch <- res{buf, err}
	}()
	select {
	case r0 := <-ch:
		return r0.b, r0.err == nil
	case <-time.After(timeout):
		return nil, false
	}
}
