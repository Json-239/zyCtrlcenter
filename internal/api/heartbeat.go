package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// heartbeatFileName 心跳落盘文件名（日志目录下）。
const heartbeatFileName = "heartbeat.json"

// heartbeatInterval 心跳间隔。看门狗探测周期 10s：5s 的粒度足够覆盖
// "进程卡住 → 20 秒后消失" 的窗口（2026-10-28 14:13 事故实测）。
const heartbeatInterval = 5 * time.Second

// heartbeatSnapshot 心跳内容（静默死亡取证用）。
type heartbeatSnapshot struct {
	PID        int   `json:"pid"`
	TS         int64 `json:"ts"` // unix 秒
	UptimeSec  int64 `json:"uptime_sec"`
	Goroutines int   `json:"goroutines"` // runtime.NumGoroutine：死锁/泄漏一眼可见
	Online     int   `json:"online"`
	Handshake  int   `json:"handshake"`
	Total      int   `json:"total"`
}

// writeHeartbeat 原子写心跳（.tmp + rename），返回错误供测试断言。
func writeHeartbeat(dir string, snap heartbeatSnapshot) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, heartbeatFileName+".tmp")
	dst := filepath.Join(dir, heartbeatFileName)
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// StartHeartbeat 每 5s 写一次 logs/heartbeat.json（开关 CTRL_HEARTBEAT，默认开）。
//
// 用途：中控若静默死亡（无 FATAL / stderr 无输出 / 无系统事件），用最后一条心跳区分：
//   - 心跳停在死亡前、进程随后消失 → 被强制终止或自行退出；
//   - 心跳持续更新但 HTTP 不通 → HTTP/handler 层卡死（goroutine 数通常同步飙升）。
func (a *API) StartHeartbeat(ctx context.Context) {
	if a.Cfg == nil || !a.Cfg.HeartbeatEnabled {
		if a.Log != nil {
			a.Log.Printf("[HEARTBEAT] 未启用（CTRL_HEARTBEAT=0）")
		}
		return
	}
	if a.Log == nil {
		return
	}
	start := time.Now()
	warned := false
	reportOnce := func(format string, err error) {
		if warned {
			return
		}
		warned = true
		a.Log.Printf(format, err)
	}
	write := func() {
		dir := a.Log.Dir()
		if dir == "" {
			return
		}
		snap := heartbeatSnapshot{
			PID:        os.Getpid(),
			TS:         time.Now().Unix(),
			UptimeSec:  int64(time.Since(start).Seconds()),
			Goroutines: runtime.NumGoroutine(),
		}
		if a.St != nil {
			snap.Online, snap.Handshake, snap.Total = a.St.Counts()
		}
		if err := writeHeartbeat(dir, snap); err != nil {
			reportOnce("[HEARTBEAT] 写入失败: %v（后续静默）", err)
			return
		}
		warned = false
	}

	a.Log.Printf("[HEARTBEAT] 已启动：每 %s 写 %s", heartbeatInterval,
		filepath.Join(a.Log.Dir(), heartbeatFileName))
	write()
	tk := time.NewTicker(heartbeatInterval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			a.Log.Printf("[HEARTBEAT] 退出（ctx done）")
			return
		case <-tk.C:
			write()
		}
	}
}
