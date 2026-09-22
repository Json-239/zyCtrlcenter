// Package store 运行历史日志（对标 Python 版 robot/ctrlcenter/store.py）。
//
// 日期级轮转（本项目的硬要求）：
//   - 运行历史：data/runs_YYYYMMDD.jsonl（append-only JSONL，面板 /api/logs 数据源）
//   - 单机器人：data/bot_logs/<account>/runs_YYYYMMDD.log
//
// 另含：保留窗口清理（默认 30 天）、单文件熔断（超过 RunsMaxMB 自动归档重建）、
// 尾部回读（readTail 只读文件末尾，与文件大小无关，避免面板轮询阻塞）。
package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store 运行历史存储。
type Store struct {
	dataDir  string
	keepDays int
	maxMB    int

	mu              sync.Mutex
	lastCleanupDay  string
	lastRotateCheck time.Time
}

// New 创建存储；keepDays<=0 时取 30，maxMB<=0 时取 500。
func New(dataDir string, keepDays, maxMB int) *Store {
	if keepDays <= 0 {
		keepDays = 30
	}
	if maxMB <= 0 {
		maxMB = 500
	}
	return &Store{dataDir: dataDir, keepDays: keepDays, maxMB: maxMB}
}

// DataDir 返回数据目录。
func (s *Store) DataDir() string { return s.dataDir }

// currentPathLocked 返回当天运行历史文件（调用方需持锁）。
func (s *Store) currentPathLocked() string {
	return filepath.Join(s.dataDir, "runs_"+time.Now().Format("20060102")+".jsonl")
}

// CurrentPath 返回当天运行历史文件路径。
func (s *Store) CurrentPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentPathLocked()
}

// LogEvent 追加一条事件到当天运行历史；自动补 ts（秒）。
// account 非空时同步写单机器人日志。
func (s *Store) LogEvent(obj map[string]any) {
	if obj == nil {
		return
	}
	out := make(map[string]any, len(obj)+1)
	for k, v := range obj {
		out[k] = v
	}
	if _, ok := out["ts"]; !ok {
		out["ts"] = time.Now().Unix()
	}
	line, err := json.Marshal(out)
	if err != nil {
		return
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.dataDir, 0o755); err != nil {
		return
	}
	s.maybeCleanupLocked()
	path := s.currentPathLocked()
	s.maybeRotateLocked(path)
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		_, _ = f.Write(line)
		_ = f.Close()
	}
	if acct, _ := out["account"].(string); acct != "" {
		s.writeBotLogLocked(acct, line)
	}
}

// writeBotLogLocked 写单机器人日志（调用方需持锁）。
func (s *Store) writeBotLogLocked(account string, line []byte) {
	dir := filepath.Join(s.dataDir, "bot_logs", account)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, "runs_"+time.Now().Format("20060102")+".log")
	s.maybeRotateLocked(path)
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		_, _ = f.Write(line)
		_ = f.Close()
	}
}

// maybeRotateLocked 单文件超过 maxMB 时归档重建（节流 1s 一次检查）。
func (s *Store) maybeRotateLocked(path string) {
	now := time.Now()
	if now.Sub(s.lastRotateCheck) < time.Second {
		return
	}
	s.lastRotateCheck = now
	st, err := os.Stat(path)
	if err != nil || st.Size() < int64(s.maxMB)*1024*1024 {
		return
	}
	bak := fmt.Sprintf("%s.over_%d", path, now.Unix())
	if _, err := os.Stat(bak); err == nil {
		return
	}
	if os.Rename(path, bak) == nil {
		// 只保留最新一份归档，防磁盘累积
		matches, _ := filepath.Glob(path + ".over_*")
		sort.Strings(matches)
		for _, f := range matches[:maxInt(0, len(matches)-1)] {
			_ = os.Remove(f)
		}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// maybeCleanupLocked 跨天首次写入时清理一次历史（调用方需持锁）。
func (s *Store) maybeCleanupLocked() {
	today := time.Now().Format("20060102")
	if s.lastCleanupDay == today {
		return
	}
	s.lastCleanupDay = today
	s.cleanupOldRunsLocked(s.keepDays)
	s.cleanupBotLogsLocked(s.keepDays)
}

// CleanupOldRuns 清理超过 keepDays 天的历史运行日志（不删当天文件），返回被删文件名。
func (s *Store) CleanupOldRuns(keepDays int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cleanupOldRunsLocked(keepDays)
}

func (s *Store) cleanupOldRunsLocked(keepDays int) []string {
	if keepDays < 1 {
		keepDays = s.keepDays
	}
	today := filepath.Base(s.currentPathLocked())
	cutoff := time.Now().Add(-time.Duration(keepDays) * 24 * time.Hour)
	entries, err := os.ReadDir(s.dataDir)
	if err != nil {
		return nil
	}
	var removed []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "runs_") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		if name == today {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if os.Remove(filepath.Join(s.dataDir, name)) == nil {
			removed = append(removed, name)
		}
	}
	sort.Strings(removed)
	return removed
}

// CleanupBotLogs 清理单机器人日志超过 keepDays 天的历史文件，返回删除数量。
func (s *Store) CleanupBotLogs(keepDays int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cleanupBotLogsLocked(keepDays)
}

func (s *Store) cleanupBotLogsLocked(keepDays int) int {
	if keepDays < 1 {
		keepDays = s.keepDays
	}
	cutoff := time.Now().Add(-time.Duration(keepDays) * 24 * time.Hour)
	today := filepath.Base(s.currentPathLocked())
	root := filepath.Join(s.dataDir, "bot_logs")
	dirs, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	removed := 0
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		dir := filepath.Join(root, d.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasPrefix(f.Name(), "runs_") || f.Name() == today {
				continue
			}
			info, err := f.Info()
			if err != nil || !info.ModTime().Before(cutoff) {
				continue
			}
			if os.Remove(filepath.Join(dir, f.Name())) == nil {
				removed++
			}
		}
		if rest, err := os.ReadDir(dir); err == nil && len(rest) == 0 {
			_ = os.Remove(dir)
		}
	}
	return removed
}

// ClearAll 清空所有运行日志（当天截断 + 历史删除 + bot_logs 清空），返回统计。
func (s *Store) ClearAll() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := map[string]int{"removed_runs": 0, "removed_bot": 0}
	if err := os.MkdirAll(s.dataDir, 0o755); err != nil {
		return res
	}
	today := filepath.Base(s.currentPathLocked())
	// 1) 当天 runs 截断（保留文件）
	if f, err := os.OpenFile(s.currentPathLocked(), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644); err == nil {
		_ = f.Close()
	}
	// 2) 历史 runs（含 .over_* 归档）删除
	entries, err := os.ReadDir(s.dataDir)
	if err == nil {
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasPrefix(name, "runs_") || name == today {
				continue
			}
			if os.Remove(filepath.Join(s.dataDir, name)) == nil {
				res["removed_runs"]++
			}
		}
	}
	// 3) bot_logs 清空
	root := filepath.Join(s.dataDir, "bot_logs")
	if dirs, err := os.ReadDir(root); err == nil {
		for _, d := range dirs {
			dir := filepath.Join(root, d.Name())
			if !d.IsDir() {
				continue
			}
			if files, err := os.ReadDir(dir); err == nil {
				for _, f := range files {
					if os.Remove(filepath.Join(dir, f.Name())) == nil {
						res["removed_bot"]++
					}
				}
			}
			_ = os.Remove(dir)
		}
	}
	return res
}

// ReadTail 读取当天运行历史尾部 limit 条（从文件末尾回读，与文件大小无关）。
func (s *Store) ReadTail(limit int) []map[string]any {
	if limit <= 0 {
		limit = 200
	}
	s.mu.Lock()
	path := s.currentPathLocked()
	s.mu.Unlock()
	return readTailJSON(path, limit)
}

// ReadBotLogs 读取指定机器人的独立日志尾部；无独立文件时回退全量日志过滤。
func (s *Store) ReadBotLogs(account string, limit int) []map[string]any {
	if account == "" {
		return nil
	}
	if limit <= 0 {
		limit = 200
	}
	s.mu.Lock()
	path := filepath.Join(s.dataDir, "bot_logs", account, "runs_"+time.Now().Format("20060102")+".log")
	s.mu.Unlock()
	if _, err := os.Stat(path); err == nil {
		return readTailJSON(path, limit)
	}
	tail := s.ReadTail(limit * 4)
	out := make([]map[string]any, 0, limit)
	for _, e := range tail {
		if acct, _ := e["account"].(string); acct == account {
			out = append(out, e)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// readTailJSON 从文件尾部回读最多 limit 行并解析为 JSON 对象。
func readTailJSON(path string, limit int) []map[string]any {
	lines := readTailLines(path, limit)
	out := make([]map[string]any, 0, len(lines))
	for _, ln := range lines {
		var obj map[string]any
		if json.Unmarshal(ln, &obj) == nil {
			out = append(out, obj)
		}
	}
	return out
}

// readTailLines 从文件末尾分块回读最多 limit 行（忽略不完整首行）。
func readTailLines(path string, limit int) [][]byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	size := st.Size()
	const chunk = 8192
	var buf []byte
	var lines [][]byte
	pos := size
	for pos > 0 && len(lines) <= limit {
		readLen := int64(chunk)
		if pos < readLen {
			readLen = pos
		}
		pos -= readLen
		tmp := make([]byte, readLen)
		if _, err := f.ReadAt(tmp, pos); err != nil {
			break
		}
		buf = append(tmp, buf...)
		lines = bytes.Split(buf, []byte("\n"))
	}
	out := make([][]byte, 0, len(lines))
	for _, ln := range lines {
		if len(bytes.TrimSpace(ln)) == 0 {
			continue
		}
		out = append(out, ln)
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}
