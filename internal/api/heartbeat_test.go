package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"zyctrlcenter/internal/config"
	"zyctrlcenter/internal/logging"
)

func TestWriteHeartbeatAtomic(t *testing.T) {
	dir := t.TempDir()
	snap := heartbeatSnapshot{PID: 1234, TS: 1793169367, UptimeSec: 42, Goroutines: 37, Online: 280, Handshake: 280, Total: 287}
	if err := writeHeartbeat(dir, snap); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, heartbeatFileName+".tmp")); !os.IsNotExist(err) {
		t.Fatal("临时文件应已被 rename 消费")
	}
	raw, err := os.ReadFile(filepath.Join(dir, heartbeatFileName))
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	var got heartbeatSnapshot
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("JSON 解析失败: %v (%s)", err, raw)
	}
	if got != snap {
		t.Fatalf("内容不一致: %+v != %+v", got, snap)
	}
	for _, k := range []string{"pid", "ts", "uptime_sec", "goroutines", "online", "handshake", "total"} {
		if !containsKey(raw, k) {
			t.Fatalf("缺少字段 %q: %s", k, raw)
		}
	}
}

func containsKey(raw []byte, key string) bool {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

func TestWriteHeartbeatOverwrite(t *testing.T) {
	dir := t.TempDir()
	if err := writeHeartbeat(dir, heartbeatSnapshot{PID: 1, Goroutines: 10}); err != nil {
		t.Fatal(err)
	}
	if err := writeHeartbeat(dir, heartbeatSnapshot{PID: 2, Goroutines: 20}); err != nil {
		t.Fatalf("覆盖写失败: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, heartbeatFileName))
	var got heartbeatSnapshot
	_ = json.Unmarshal(raw, &got)
	if got.PID != 2 || got.Goroutines != 20 {
		t.Fatalf("应覆盖为第二次内容: %+v", got)
	}
}

func TestWriteHeartbeatCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := writeHeartbeat(dir, heartbeatSnapshot{PID: 3}); err != nil {
		t.Fatalf("目录不存在应自动创建: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, heartbeatFileName)); err != nil {
		t.Fatal("文件未生成")
	}
}

func TestWriteHeartbeatBadDir(t *testing.T) {
	f := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 把普通文件当目录 → MkdirAll 报错
	if err := writeHeartbeat(f, heartbeatSnapshot{PID: 4}); err == nil {
		t.Fatal("非法目录应报错")
	}
}

func TestStartHeartbeatDisabled(t *testing.T) {
	dir := t.TempDir()
	lg := logging.New(dir, 30)
	defer lg.Close()
	a := &API{Deps: Deps{Cfg: &config.Config{HeartbeatEnabled: false}, Log: lg}}
	done := make(chan struct{})
	go func() {
		a.StartHeartbeat(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("开关关闭时应立即返回")
	}
}

func TestStartHeartbeatWritesImmediately(t *testing.T) {
	dir := t.TempDir()
	lg := logging.New(dir, 30)
	a := &API{Deps: Deps{Cfg: &config.Config{HeartbeatEnabled: true}, Log: lg}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.StartHeartbeat(ctx)
		close(done)
	}()
	// 先 cancel 并等 goroutine 退出，再关 logger —— 否则 TempDir 清理会撞上句柄
	defer func() {
		cancel()
		<-done
		_ = lg.Close()
	}()

	path := filepath.Join(dir, heartbeatFileName)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			var got heartbeatSnapshot
			if json.Unmarshal(raw, &got) == nil && got.PID == os.Getpid() && got.Goroutines > 0 {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("启动后 2 秒内未见有效心跳文件")
}

func TestStartHeartbeatNilCfg(t *testing.T) {
	done := make(chan struct{})
	go func() {
		a := &API{}
		a.StartHeartbeat(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Cfg/Log 为 nil 时应立即返回")
	}
}
