// Package ctrl 机器人控制通道（TCP JSON-lines，对标 Python 版 ctrl_server.py）。
//
// 协议（详见 docs/协议规范.md §1）：
//   - 监听 127.0.0.1:17200（可配）；
//   - 单连接语义：新连接直接接管旧连接（同一时刻只有一个活跃 robot 连接）；
//   - 帧格式：UTF-8 JSON + '\n' 结尾（**上行**单行 ≤64KB；下行可以是 2MB 级链数据载荷，
//     见 WriteTimeoutFor）；
//   - 上行（robot→中控）= 事件，入队由 services/event 消费；
//   - 下行（中控→robot）= 命令，SendCmd 写入；写超时按载荷大小放宽，写失败**关连接**
//     （半行写入会与下一条命令粘连成非法行，宁可靠机器人重连恢复）。
package ctrl

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// MaxLineBytes **上行**单行最大字节数（协议约定 64KB：机器人上报的事件都是小报文）。
const MaxLineBytes = 64 * 1024

// 下行写超时：基础 5s + 每 256KB 加 1s，上限 60s。
//
// 为什么按大小放宽：抓鬼导航数据是 2MB 级载荷（真实文件 2,209,274B），机器人主循环读得慢时
// 5s 可能写不完；写超时会留下"半行"，机器人把半行和下一条命令拼起来 → 两条命令都被丢。
const (
	writeTimeoutBase = 5 * time.Second
	writeTimeoutStep = time.Second
	writeStepBytes   = 256 * 1024
	writeTimeoutMax  = 60 * time.Second
)

// WriteTimeoutFor 载荷大小 → 写超时（导出给测试与文档用）。
func WriteTimeoutFor(size int) time.Duration {
	if size < 0 {
		size = 0
	}
	d := writeTimeoutBase + time.Duration(size/writeStepBytes)*writeTimeoutStep
	if d > writeTimeoutMax {
		return writeTimeoutMax
	}
	return d
}

// Server 一个区的控制通道（一条机器人连接）。
type Server struct {
	Host string
	Port int
	// Zone 本通道所属区 key（"<服key>/<区key>"）；收到的事件会打上 "_zone" 标记，
	// 供事件处理/命令路由识别来源。空 = 单区模式（用监听地址当标记）。
	Zone string

	// Events 上行事件队列（容量 8192；满时丢弃并记日志，防止通道阻塞 robot）。
	// 多区并行时由 Hub 注入同一个队列，事件靠 "_zone" 区分来源。
	Events chan map[string]any

	logf func(string, ...any)

	// writeTimeoutFor 载荷大小 → 写超时（默认 WriteTimeoutFor；测试可注入短超时）
	writeTimeoutFor func(int) time.Duration

	mu        sync.Mutex
	ln        net.Listener
	conn      net.Conn
	connected bool
	closed    bool
}

// SetWriteTimeoutFor 覆盖"载荷大小 → 写超时"的计算（测试用：注入短超时验证写失败行为）。
func (s *Server) SetWriteTimeoutFor(f func(int) time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeTimeoutFor = f
}

func (s *Server) writeTimeout(size int) time.Duration {
	if s.writeTimeoutFor != nil {
		return s.writeTimeoutFor(size)
	}
	return WriteTimeoutFor(size)
}

// New 创建单区控制通道（自带事件队列）；logf 可为 nil。
func New(host string, port int, logf func(string, ...any)) *Server {
	return newServer(host, port, "", nil, logf)
}

// NewWithQueue 创建指定区、共用外部事件队列的控制通道（Hub 使用）。
func NewWithQueue(host string, port int, zone string, queue chan map[string]any, logf func(string, ...any)) *Server {
	return newServer(host, port, zone, queue, logf)
}

func newServer(host string, port int, zone string, queue chan map[string]any, logf func(string, ...any)) *Server {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if queue == nil {
		queue = make(chan map[string]any, 8192)
	}
	return &Server{Host: host, Port: port, Zone: zone, Events: queue, logf: logf}
}

// Addr 返回配置的 "host:port"。
func (s *Server) Addr() string { return fmt.Sprintf("%s:%d", s.Host, s.Port) }

// SetZone 更新本通道的区标记（单进程切区时由 API 调用；事件会带上新标记）。
func (s *Server) SetZone(zone string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Zone = strings.TrimSpace(zone)
}

// Tag 当前区标记（未设置时用监听地址）——用于 /api/status 透出。
func (s *Server) Tag() string { return s.zoneTag() }

// zoneTag 事件/日志里的区标记（未配置区时用监听地址）。
func (s *Server) zoneTag() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(s.Zone) != "" {
		return s.Zone
	}
	return s.Addr()
}

// Peer 当前机器人连接的远端地址（无连接返回空）。
func (s *Server) Peer() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return ""
	}
	return s.conn.RemoteAddr().String()
}

// BoundAddr 返回实际监听地址（Port=0 时为系统分配端口，测试用）。
func (s *Server) BoundAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln != nil {
		return s.ln.Addr().String()
	}
	return s.Addr()
}

// Start 启动监听；端口被占用时重试（最多 15 次，每次间隔 1 秒）——
// 兼容「点重启中控」时旧进程刚退出、Windows 端口未立即释放的场景。
func (s *Server) Start() error {
	var lastErr error
	for i := 0; i < 15; i++ {
		ln, err := net.Listen("tcp", s.Addr())
		if err == nil {
			s.mu.Lock()
			s.ln = ln
			s.mu.Unlock()
			s.logf("[CTRL] 控制通道监听 %s（等待 robot 连接）", s.Addr())
			go s.acceptLoop(ln)
			return nil
		}
		lastErr = err
		time.Sleep(time.Second)
	}
	return fmt.Errorf("控制通道监听失败 %s: %w", s.Addr(), lastErr)
}

func (s *Server) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if s.isClosed() {
				return
			}
			continue
		}
		go s.handleConn(conn)
	}
}

// handleConn 单连接处理：新连接接管旧连接。
func (s *Server) handleConn(conn net.Conn) {
	s.mu.Lock()
	old := s.conn
	s.conn = conn
	s.connected = true
	s.mu.Unlock()
	if old != nil && old != conn {
		_ = old.Close()
	}
	s.logf("[CTRL] robot 已连接：%s", conn.RemoteAddr())

	reader := bufio.NewReaderSize(conn, MaxLineBytes)
	for {
		line, err := readLineLimited(reader, MaxLineBytes)
		if err != nil {
			break
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var obj map[string]any
		if json.Unmarshal(line, &obj) != nil {
			continue // 非法行直接忽略（与参考实现一致）
		}
		// 打区标记：多区并行时事件处理/命令路由据此识别来源
		if _, exists := obj["_zone"]; !exists {
			obj["_zone"] = s.zoneTag()
		}
		select {
		case s.Events <- obj:
		default:
			s.logf("[CTRL] 事件队列已满，丢弃事件 type=%v zone=%s", obj["type"], s.zoneTag())
		}
	}

	// 只在「当前持有的连接就是本连接」时清状态：robot 重启瞬间旧连接断开
	// 可能晚于新连接接管，直接置 nil 会把新连接覆盖掉。
	s.mu.Lock()
	if s.conn == conn {
		s.connected = false
		s.conn = nil
	}
	s.mu.Unlock()
	_ = conn.Close()
	s.logf("[CTRL] robot 已断开：%s", conn.RemoteAddr())
}

// Listening 是否已进入监听（Start 成功后为 true）。
func (s *Server) Listening() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ln != nil && !s.closed
}

// Connected 当前是否有 robot 连接。
func (s *Server) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connected
}

// SendCmd 下发命令；返回是否发送成功（无连接/写超时/写失败均返回 false）。
//
// 写失败时**主动断开**这条连接：写了一半的行会与下一条命令粘连成非法行（机器人端宽松解析
// 会把两条都丢掉，还会静默丢命令）。断开后机器人按既有重连逻辑接回来，远好过流错位。
func (s *Server) SendCmd(cmd map[string]any) bool {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return false
	}
	data, err := json.Marshal(cmd)
	if err != nil {
		return false
	}
	data = append(data, '\n')
	_ = conn.SetWriteDeadline(time.Now().Add(s.writeTimeout(len(data))))
	if _, err := conn.Write(data); err != nil {
		s.logf("[CTRL] 命令下发失败 cmd=%v（%d 字节）: %v —— 断开该连接，等机器人重连",
			cmd["cmd"], len(data), err)
		s.dropConn(conn)
		return false
	}
	return true
}

// dropConn 关闭并清理连接（只在"当前持有的连接就是它"时清状态：机器人重连瞬间旧连接断开
// 可能晚于新连接接管，直接置 nil 会把新连接覆盖掉）。
func (s *Server) dropConn(conn net.Conn) {
	s.mu.Lock()
	if s.conn == conn {
		s.conn = nil
		s.connected = false
	}
	s.mu.Unlock()
	_ = conn.Close()
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Close 关闭监听与当前连接。
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	ln, conn := s.ln, s.conn
	s.conn = nil
	s.connected = false
	s.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	if ln != nil {
		_ = ln.Close()
	}
}

// readLineLimited 读取一行（\n 结尾），超过 limit 字节时丢弃该行剩余部分并返回错误。
func readLineLimited(r *bufio.Reader, limit int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if err == nil {
			if len(buf) > limit {
				return nil, fmt.Errorf("line too long (>%d bytes)", limit)
			}
			return buf, nil
		}
		if err == bufio.ErrBufferFull {
			if len(buf) > limit {
				// 丢弃剩余到行尾
				for {
					if _, e := r.ReadSlice('\n'); e == nil || e == io.EOF || e != bufio.ErrBufferFull {
						break
					}
				}
				return nil, fmt.Errorf("line too long (>%d bytes)", limit)
			}
			continue
		}
		return nil, err
	}
}
