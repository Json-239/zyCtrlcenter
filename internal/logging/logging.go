// Package logging 提供「按日期切分」的应用日志（logs/ctrlcenter_YYYYMMDD.log）。
//
// 与运行历史日志（internal/store 的 data/runs_YYYYMMDD.jsonl）职责区分：
//   - 应用日志：进程 stdout/运行诊断，人读，按日切分，保留 N 天后清理；
//   - 运行历史：机器人事件 JSONL，机器读，供面板 /api/logs 展示。
//
// 本包为纯基础设施，不依赖任何业务模块。
package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Logger 按日切分的应用日志写入器（并发安全）。
type Logger struct {
	dir      string
	keepDays int

	mu  sync.Mutex
	day string
	f   *os.File
}

// New 创建日志器；dir 为日志目录（通常为 <项目根>/logs）。
func New(dir string, keepDays int) *Logger {
	_ = os.MkdirAll(dir, 0o755)
	if keepDays < 1 {
		keepDays = 30
	}
	return &Logger{dir: dir, keepDays: keepDays}
}

// Dir 返回日志目录。
func (l *Logger) Dir() string { return l.dir }

// Path 返回当天日志文件路径（logs/ctrlcenter_YYYYMMDD.log）。
func (l *Logger) Path() string {
	return filepath.Join(l.dir, "ctrlcenter_"+time.Now().Format("20060102")+".log")
}

// Printf 写一行带时间戳的日志（同时落盘当天文件与 stdout）。
func (l *Logger) Printf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if line == "" || line[len(line)-1] != '\n' {
		line += "\n"
	}
	stamped := time.Now().Format("2006-01-02 15:04:05") + " " + line

	var removed []string // 跨日/首次访问时清理掉的历史日志（在锁外再输出提示）
	l.mu.Lock()
	if f, err := l.fileLocked(&removed); err == nil {
		_, _ = f.WriteString(stamped)
	}
	l.mu.Unlock()
	_, _ = os.Stdout.WriteString(stamped)
	// 2026-10-23 P0 修复：清理提示必须在释放 l.mu 之后输出。
	// 原实现由 cleanupLocked 在持锁路径回调 l.Printf → 对同一把 sync.Mutex 重入 →
	// 永久死锁：跨天时整进程挂起（10-20/10-22 两次午夜静默死亡），启动首行时单 goroutine
	// 被 Go 运行时判定全局死锁直接退出（0 字节日志）。
	if len(removed) > 0 {
		l.Printf("[LOG] 清理历史应用日志 %d 个: %s", len(removed), strings.Join(removed, ", "))
	}
}

// fileLocked 取当天文件句柄（跨日/首次访问时自动切换并清理历史）；removed 带回被清理的文件名。
func (l *Logger) fileLocked(removed *[]string) (*os.File, error) {
	day := time.Now().Format("20060102")
	if l.f != nil && l.day == day {
		return l.f, nil
	}
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
	path := filepath.Join(l.dir, "ctrlcenter_"+day+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	l.f = f
	l.day = day
	if removed != nil {
		*removed = l.cleanupLocked()
	}
	return f, nil
}

// cleanupLocked 清理超过保留窗口的历史日志（当前文件不删），返回被删文件名。
// 注意：本函数在 l.mu 持有期间调用，**不得**再调用 l.Printf/任何会加锁的方法（2026-10-23 死锁事故）。
func (l *Logger) cleanupLocked() []string {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return nil
	}
	cutoff := time.Now().Add(-time.Duration(l.keepDays) * 24 * time.Hour)
	today := filepath.Base(l.Path())
	var removed []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "ctrlcenter_") || !strings.HasSuffix(name, ".log") {
			continue
		}
		if name == today {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if os.Remove(filepath.Join(l.dir, name)) == nil {
				removed = append(removed, name)
			}
		}
	}
	if len(removed) > 0 {
		sort.Strings(removed)
	}
	return removed
}

// Close 关闭当前文件句柄。
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		err := l.f.Close()
		l.f = nil
		return err
	}
	return nil
}
