package process

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseStatOutSingle(t *testing.T) {
	out := "42388,380633088,435159040,9056\r\n"
	s, err := ParseStatOut(out)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if s.PID != 42388 || s.Peers != 1 {
		t.Fatalf("pid/peers 错误: %+v", s)
	}
	if got := s.WorkingSetMB; got < 362 || got > 364 {
		t.Fatalf("ws 换算错误: %.2f", got)
	}
	if got := s.CommitMB; got < 414 || got > 416 {
		t.Fatalf("commit 换算错误: %.2f", got)
	}
	if s.Uptime != 9056*time.Second {
		t.Fatalf("uptime 错误: %s", s.Uptime)
	}
	if s.MemMB() < 414 || s.MemMB() > 416 {
		t.Fatalf("MemMB 应取较大值: %.2f", s.MemMB())
	}
}

func TestParseStatOutMultiPicksLargestCommit(t *testing.T) {
	out := "100,100000000,200000000,10\n" +
		"200,300000000,900000000,20\n" +
		"300,400000000,500000000,30\n"
	s, err := ParseStatOut(out)
	if err != nil {
		t.Fatal(err)
	}
	if s.PID != 200 {
		t.Fatalf("应取 commit 最大的 200，实际 %d", s.PID)
	}
	if s.Peers != 3 {
		t.Fatalf("Peers 应为 3，实际 %d", s.Peers)
	}
}

func TestParseStatOutIgnoreNoise(t *testing.T) {
	// 空输出、PowerShell 报错文本、缺列、非数字 PID 都应被忽略 → ErrNotRunning
	for _, out := range []string{
		"",
		"   \r\n\r\n",
		"Get-CimInstance : Access denied",
		"42388,1,2",
		"abc,1,2,3",
		"-1,1,2,3",
		"0,1,2,3",
	} {
		if _, err := ParseStatOut(out); !errors.Is(err, ErrNotRunning) {
			t.Fatalf("输出 %q 应返回 ErrNotRunning，实际 %v", out, err)
		}
	}
}

func TestParseStatOutCRLFAndSpaces(t *testing.T) {
	out := " 42388 , 380633088 , 435159040 , 9056 \r\n\r\n"
	s, err := ParseStatOut(out)
	if err != nil {
		t.Fatalf("带空格/CRLF 应可解析: %v", err)
	}
	if s.PID != 42388 {
		t.Fatalf("pid 错误: %d", s.PID)
	}
}

func TestStatScriptFiltersByDeployDir(t *testing.T) {
	got := statScript("robot_single_robot.exe", `F:\ZyBin\xm\robot\deploy\single_robot_zy\`)
	if !strings.Contains(got, `$_.ExecutablePath -like 'F:\ZyBin\xm\robot\deploy\single_robot_zy\*'`) {
		t.Fatalf("脚本未按部署目录过滤:\n%s", got)
	}
	if !strings.Contains(got, "Name='robot_single_robot.exe'") {
		t.Fatalf("脚本缺少镜像名过滤:\n%s", got)
	}
	if !strings.Contains(got, "PrivatePageCount") || !strings.Contains(got, "WorkingSetSize") {
		t.Fatalf("脚本未取内存字段:\n%s", got)
	}
}

func TestStatScriptEscapesQuote(t *testing.T) {
	got := statScript("ro'bot.exe", `D:\a'b\c`)
	if strings.Contains(got, "ro'bot.exe") {
		t.Fatalf("镜像名单引号未转义:\n%s", got)
	}
	if !strings.Contains(got, "ro''bot.exe") {
		t.Fatalf("镜像名单引号应转成两个单引号:\n%s", got)
	}
}

func TestStatUsesInjectedRunner(t *testing.T) {
	m := New("x.exe", `F:\deploy\zy`, "robot_single_robot.exe", nil)
	called := ""
	m.SetStatRunner(func(ctx context.Context, script string) (string, error) {
		called = script
		return "42388,380633088,435159040,9056\n", nil
	})
	s, err := m.Stat()
	if err != nil {
		t.Fatalf("Stat 失败: %v", err)
	}
	if s.PID != 42388 || s.Peers != 1 {
		t.Fatalf("结果错误: %+v", s)
	}
	if !strings.Contains(called, `F:\deploy\zy\*`) {
		t.Fatalf("未按部署目录采样: %s", called)
	}
}

func TestStatRunnerErrorPropagates(t *testing.T) {
	m := New("x.exe", `F:\deploy\zy`, "robot_single_robot.exe", nil)
	m.SetStatRunner(func(ctx context.Context, script string) (string, error) {
		return "", errors.New("powershell boom")
	})
	if _, err := m.Stat(); err == nil || !strings.Contains(err.Error(), "powershell boom") {
		t.Fatalf("应透传执行错误，实际 %v", err)
	}
}

func TestStatNotRunning(t *testing.T) {
	m := New("x.exe", `F:\deploy\zy`, "robot_single_robot.exe", nil)
	m.SetStatRunner(func(ctx context.Context, script string) (string, error) { return "", nil })
	if _, err := m.Stat(); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("应返回 ErrNotRunning，实际 %v", err)
	}
}

func TestStatEmptyDeployDir(t *testing.T) {
	m := New("x.exe", "", "robot_single_robot.exe", nil)
	if _, err := m.Stat(); err == nil {
		t.Fatal("空部署目录应报错（避免误查全机同名进程）")
	}
}
