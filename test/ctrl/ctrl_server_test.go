// ctrl 模块测试：TCP JSON-lines 收发 / 单连接接管 / 异常行容错 / 无连接下发失败。
package ctrl_test

import (
	"net"
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/ctrl"
	"zyctrlcenter/test/testsupport"
)

func startServer(t *testing.T) *ctrl.Server {
	t.Helper()
	s := ctrl.New("127.0.0.1", 0, nil) // 端口 0 = 系统分配，测试互不干扰
	if err := s.Start(); err != nil {
		t.Fatalf("启动控制通道失败: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestRobotEventReachesQueue(t *testing.T) {
	s := startServer(t)
	rb := testsupport.ConnectFakeRobot(t, s)
	defer rb.Close()

	rb.SendEvent(t, map[string]any{"type": "hello", "robot_version": "1.0.0", "pid": 4242})
	ev := testsupport.WaitEvent(t, s, 2*time.Second)
	if ev["type"] != "hello" {
		t.Fatalf("事件 type 应为 hello，实际 %v", ev["type"])
	}
	if pid, _ := ev["pid"].(float64); pid != 4242 {
		t.Fatalf("事件字段应完整透传 pid=4242，实际 %v", ev["pid"])
	}
}

func TestSendCmdDeliversJSONLine(t *testing.T) {
	s := startServer(t)
	rb := testsupport.ConnectFakeRobot(t, s)
	defer rb.Close()

	if !s.SendCmd(map[string]any{"cmd": "status"}) {
		t.Fatal("已连接时 SendCmd 应返回成功")
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "status" {
		t.Fatalf("机器人端应收到 cmd=status，实际 %v", cmd)
	}
}

func TestSendCmdWithoutConnectionFails(t *testing.T) {
	s := ctrl.New("127.0.0.1", 0, nil) // 未启动、无连接
	if s.SendCmd(map[string]any{"cmd": "ping"}) {
		t.Fatal("无连接时 SendCmd 必须返回 false（面板据此提示「通道未连接」）")
	}
}

func TestNewConnectionTakesOverOld(t *testing.T) {
	s := startServer(t)
	first := testsupport.ConnectFakeRobot(t, s)
	defer first.Close()

	second := testsupport.ConnectFakeRobot(t, s)
	defer second.Close()
	// 用一条事件确认「服务端当前持有的连接」已是 second（接管完成的证据）
	second.SendEvent(t, map[string]any{"type": "hello", "marker": "second"})
	ev := testsupport.WaitEvent(t, s, 2*time.Second)
	if ev["marker"] != "second" {
		t.Fatalf("事件应来自新连接，实际 %v", ev)
	}

	// 旧连接应被服务端关闭（读到 EOF/错误）
	_ = first.Conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := first.Conn.Read(buf); err == nil {
		t.Fatal("旧连接应被新连接接管并关闭")
	}

	if !s.SendCmd(map[string]any{"cmd": "ping"}) {
		t.Fatal("接管后 SendCmd 应成功")
	}
	if cmd := second.ReadCmd(t, 2*time.Second); cmd["cmd"] != "ping" {
		t.Fatalf("命令应下发到新连接，实际 %v", cmd)
	}
}

// 写超时按载荷大小放宽（5s 基础 + 每 256KB 1s，上限 60s）——抓鬼导航数据是 2MB 级，
// 固定 5s 在机器人读得慢时会写一半就超时。
func TestWriteTimeoutScalesWithPayload(t *testing.T) {
	if d := ctrl.WriteTimeoutFor(0); d != 5*time.Second {
		t.Fatalf("空载荷应是基础 5s: %v", d)
	}
	if d := ctrl.WriteTimeoutFor(2 * 1024 * 1024); d != 13*time.Second {
		t.Fatalf("2MB 载荷应放宽到 13s（5s + 8×1s）: %v", d)
	}
	if d := ctrl.WriteTimeoutFor(100 * 1024 * 1024); d != 60*time.Second {
		t.Fatalf("超时要有上限 60s: %v", d)
	}
}

// 大载荷写超时：返回 false **并且断开连接**（半行会与下一条命令粘连成非法行、静默丢命令）。
//
// 机器人端故意不读 + 收缓冲压到最小 → 发送缓冲写满后必然阻塞；循环重试直到某一发超时
// （不依赖某个具体的内核缓冲大小，换机器也稳定）。
func TestSendCmdWriteTimeoutDropsConnection(t *testing.T) {
	s := startServer(t)
	s.SetWriteTimeoutFor(func(int) time.Duration { return 30 * time.Millisecond })
	rb := testsupport.ConnectFakeRobot(t, s)
	defer rb.Close()
	if tcp, ok := rb.Conn.(*net.TCPConn); ok {
		_ = tcp.SetReadBuffer(4096) // 收窗口压小：几 MB 就够把发送缓冲顶满
	}

	big := strings.Repeat("x", 4*1024*1024)
	failed := false
	for i := 0; i < 6 && !failed; i++ {
		if !s.SendCmd(map[string]any{"cmd": "ghost_start", "chain": big}) {
			failed = true
		}
	}
	if !failed {
		t.Fatal("机器人不读时，反复下发大载荷最终必须写超时（返回 false）")
	}
	testsupport.Eventually(t, 2*time.Second, func() bool { return !s.Connected() },
		"写失败后应断开这条连接，等机器人重连")
	if s.SendCmd(map[string]any{"cmd": "ping"}) {
		t.Fatal("连接已断开，后续下发必须失败")
	}
}

func TestGarbledLineIgnoredNextLineParsed(t *testing.T) {
	s := startServer(t)
	rb := testsupport.ConnectFakeRobot(t, s)
	defer rb.Close()

	rb.SendRaw(t, []byte("this-is-not-json\n"))
	rb.SendRaw(t, []byte("\n")) // 空行也应被忽略
	rb.SendEvent(t, map[string]any{"type": "pong"})

	ev := testsupport.WaitEvent(t, s, 2*time.Second)
	if ev["type"] != "pong" {
		t.Fatalf("非法行应被忽略且不影响后续行解析，实际 %v", ev)
	}
	if len(s.Events) != 0 {
		t.Fatalf("非法行不应入队，队列剩余 %d 条", len(s.Events))
	}
}
