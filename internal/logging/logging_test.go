package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPrintfCleansOldLogsWithoutDeadlock 2026-10-23 P0 回归：
// 跨日/首次写入触发"清理超过保留窗口的旧日志"时，Printf 不得因互斥重入死锁。
// 原实现 cleanupLocked 在持 l.mu 的路径里回调 l.Printf → 同一把 sync.Mutex 重入 →
// 进程挂起（运行中跨天）或直接退出（启动首行，Go 运行时判定全局死锁，0 字节日志）。
func TestPrintfCleansOldLogsWithoutDeadlock(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "ctrlcenter_20200101.log")
	if err := os.WriteFile(old, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	lg := New(dir, 30)
	defer lg.Close()

	done := make(chan struct{})
	go func() {
		lg.Printf("hello-deadlock-regression")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Printf 死锁（5s 超时）：清理旧日志时对 l.mu 互斥重入")
	}

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("超过保留窗口的旧日志应被清理，实际仍存在: %v", err)
	}
	raw, err := os.ReadFile(lg.Path())
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "hello-deadlock-regression") {
		t.Fatalf("当天日志应包含本次写入，实际内容:\n%s", text)
	}
	if !strings.Contains(text, "[LOG] 清理历史应用日志 1 个") {
		t.Fatalf("清理提示应在锁外输出并落盘，实际内容:\n%s", text)
	}
}

// TestPrintfKeepsFreshLogs 保留窗口内的旧日志不得被删除。
func TestPrintfKeepsFreshLogs(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "ctrlcenter_20261001.log")
	if err := os.WriteFile(fresh, []byte("recent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lg := New(dir, 30)
	defer lg.Close()
	lg.Printf("x")
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("保留窗口内的日志不应被删除: %v", err)
	}
}
