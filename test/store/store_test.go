// store 模块测试：日期级轮转 / 尾部回读 / 单 bot 分流 / 清理 / 清空 / 熔断归档。
package store_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/store"
	"zyctrlcenter/test/testsupport"
)

func TestCurrentPathIsDateBased(t *testing.T) {
	st, dir := testsupport.NewTestStore(t)
	path := st.CurrentPath()
	want := filepath.Join(dir, "runs_"+time.Now().Format("20060102")+".jsonl")
	if path != want {
		t.Fatalf("当天运行历史路径应为日期级文件\n got: %s\nwant: %s", path, want)
	}
}

func TestLogEventWritesAndReadsTail(t *testing.T) {
	st, _ := testsupport.NewTestStore(t)
	for i := 1; i <= 5; i++ {
		st.LogEvent(map[string]any{"type": "log", "seq": i})
	}
	tail := st.ReadTail(3)
	if len(tail) != 3 {
		t.Fatalf("ReadTail(3) 应返回 3 条，实际 %d 条", len(tail))
	}
	// 应取尾部最后 3 条（3、4、5），且保持写入顺序
	for i, want := range []float64{3, 4, 5} {
		if got := tail[i]["seq"]; got != want {
			t.Fatalf("第 %d 条 seq 应为 %v，实际 %v", i, want, got)
		}
	}
	// ts 自动补齐（秒）
	if ts, ok := tail[0]["ts"].(float64); !ok || ts <= 0 {
		t.Fatalf("事件应自动补 ts（秒），实际 %v", tail[0]["ts"])
	}
}

func TestBotLogsSeparatedByAccount(t *testing.T) {
	st, dir := testsupport.NewTestStore(t)
	st.LogEvent(map[string]any{"type": "task_progress", "account": "robotA", "done": 1})
	st.LogEvent(map[string]any{"type": "task_progress", "account": "robotB", "done": 2})
	st.LogEvent(map[string]any{"type": "log", "account": "robotA", "msg": "hi"})

	botPath := filepath.Join(dir, "bot_logs", "robotA", "runs_"+time.Now().Format("20060102")+".log")
	if _, err := os.Stat(botPath); err != nil {
		t.Fatalf("单 bot 日志应按日期分流落盘 %s: %v", botPath, err)
	}
	logs := st.ReadBotLogs("robotA", 10)
	if len(logs) != 2 {
		t.Fatalf("robotA 独立日志应有 2 条，实际 %d 条", len(logs))
	}
	for _, e := range logs {
		if e["account"] != "robotA" {
			t.Fatalf("独立日志不应混入其它账号: %v", e)
		}
	}
}

func TestCleanupOldRunsKeepsTodayAndRecent(t *testing.T) {
	st, dir := testsupport.NewTestStore(t)
	st.LogEvent(map[string]any{"type": "log", "msg": "today"}) // 先建当天文件
	// 造 2 个历史文件：40 天前（应删）、3 天前（应留）；当天文件必须保留
	old := filepath.Join(dir, "runs_20200101.jsonl")
	recent := filepath.Join(dir, "runs_"+time.Now().AddDate(0, 0, -3).Format("20060102")+".jsonl")
	for _, p := range []string{old, recent} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("造历史文件失败: %v", err)
		}
	}
	_ = os.Chtimes(old, time.Now().AddDate(0, 0, -40), time.Now().AddDate(0, 0, -40))

	removed := st.CleanupOldRuns(30)
	if len(removed) != 1 || removed[0] != "runs_20200101.jsonl" {
		t.Fatalf("应只删除 40 天前的历史文件，实际删除 %v", removed)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("40 天前文件应已删除")
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("3 天前文件不应被删: %v", err)
	}
	if _, err := os.Stat(st.CurrentPath()); err != nil {
		t.Fatalf("当天文件不应被删: %v", err)
	}
}

func TestClearAllTruncatesTodayAndRemovesHistory(t *testing.T) {
	st, dir := testsupport.NewTestStore(t)
	st.LogEvent(map[string]any{"type": "log", "account": "robotA", "msg": "x"})
	hist := filepath.Join(dir, "runs_20200102.jsonl")
	if err := os.WriteFile(hist, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := st.ClearAll()
	if res["removed_runs"] != 1 {
		t.Fatalf("应删除 1 个历史 runs 文件，实际 %v", res["removed_runs"])
	}
	if n := len(st.ReadTail(10)); n != 0 {
		t.Fatalf("清空后当天运行历史应为空，实际 %d 条", n)
	}
	botDir := filepath.Join(dir, "bot_logs", "robotA")
	if _, err := os.Stat(botDir); !os.IsNotExist(err) {
		t.Fatalf("清空后单 bot 目录应删除: %v", err)
	}
	// 清空动作本身会写一条 api 事件（审计），允许存在
	if _, err := os.Stat(st.CurrentPath()); err != nil {
		t.Fatalf("当天文件应保留（截断而非删除）: %v", err)
	}
}

func TestRotationArchivesOversizedFile(t *testing.T) {
	dir := t.TempDir()
	st := store.New(dir, 30, 1) // 熔断阈值 1MB
	big := strings.Repeat("x", 200*1024)
	// 直接写入超过 1MB 的内容（绕过 LogEvent 的节流窗口）
	f, err := os.OpenFile(st.CurrentPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := f.WriteString(`{"pad":"` + big + `"}` + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()
	time.Sleep(1100 * time.Millisecond) // 越过 1s 检查节流
	st.LogEvent(map[string]any{"type": "log", "msg": "after-rotate"})

	matches, _ := filepath.Glob(st.CurrentPath() + ".over_*")
	if len(matches) != 1 {
		t.Fatalf("超阈值文件应被归档为 .over_*（且只留最新一份），实际 %v", matches)
	}
	raw, err := os.ReadFile(st.CurrentPath())
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &obj); err != nil {
		t.Fatalf("归档后新文件应只含归档后写入的事件: %v", err)
	}
	if obj["msg"] != "after-rotate" {
		t.Fatalf("归档后新文件内容不符合预期: %v", obj)
	}
}
