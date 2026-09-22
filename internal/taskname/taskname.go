// Package taskname 任务号 → 任务名（**只读游戏配置**，给面板显示用）。
//
// 机器人上报的是任务**编号**（`task_index`，如 2019508），大屏上只看编号没人看得懂。
// 名字本来就在游戏配置里：`<游戏配置目录>/task/*.xml` 的 `task_entry` 上带着
// `episode_name`（章节名，例：捉鬼 / 交付捉鬼任务）或 `name`。
//
// 口径（与 maplib.GridReader 同款：按需加载 + 缓存）：
//   - **懒加载**：第一次查询才扫目录（356 个 XML ≈ 7.7MB，一次约 1 秒，之后走缓存）；
//   - 目录不可用（没配 GameConfigDir / CI）→ 返回空表，调用方回退显示编号（不报错）；
//   - 只读，不改游戏数据；中控依旧"不生产链数据"，这里只是把**已有**的名字读出来给面板看。
package taskname

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Table 任务名表（task_index → 名称）。
type Table struct {
	dir string

	mu    sync.Mutex
	once  bool
	names map[int]string
	err   error
}

// New 创建任务名表（dir = 游戏配置目录，读 <dir>/task/*.xml）。
func New(dir string) *Table { return &Table{dir: dir} }

// Available 游戏配置目录是否存在（面板可据此提示"未配置游戏配置目录"）。
func (t *Table) Available() bool {
	if t == nil || strings.TrimSpace(t.dir) == "" {
		return false
	}
	st, err := os.Stat(filepath.Join(t.dir, "task"))
	return err == nil && st.IsDir()
}

// Lookup 查任务名（没加载到就返回空串；调用方回退显示编号）。
func (t *Table) Lookup(taskIndex int) string {
	t.load()
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.names[taskIndex]
}

// All 全表（面板/排障用；返回副本）。
func (t *Table) All() map[int]string {
	t.load()
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[int]string, len(t.names))
	for k, v := range t.names {
		out[k] = v
	}
	return out
}

// Count 已加载的名字条数（-1 = 目录不可用）。
func (t *Table) Count() int {
	if !t.Available() {
		return -1
	}
	t.load()
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.names)
}

// Dir 游戏配置目录（面板显示用）。
func (t *Table) Dir() string { return t.dir }

// load 扫一遍 <dir>/task/*.xml，抽出 task_entry 的名字（只跑一次）。
func (t *Table) load() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.once {
		return
	}
	if !t.Available() {
		return // 目录还不可用：**不缓存**，下次调用再试（游戏配置后挂上也能认，不必重启）
	}
	t.once = true
	t.names = map[int]string{}
	dir := filepath.Join(t.dir, "task")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.err = err
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".xml") {
			continue
		}
		t.parseFile(filepath.Join(dir, e.Name()))
	}
}

// parseFile 解析单个任务 XML：<task_story><task_entry task_index="…" episode_name="…">。
func (t *Table) parseFile(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	dec := xml.NewDecoder(strings.NewReader(string(raw)))
	dec.Strict = false // 游戏导出的 XML 偶有未转义字符，容错（顺序读，不用 DOM）
	for {
		tok, err := dec.Token()
		if err != nil {
			return
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "task_entry" {
			continue
		}
		idx, name := 0, ""
		for _, a := range se.Attr {
			switch a.Name.Local {
			case "task_index", "index", "id":
				if idx == 0 {
					idx = atoi(a.Value)
				}
			case "episode_name":
				name = strings.TrimSpace(a.Value)
			case "name", "task_name":
				if name == "" {
					name = strings.TrimSpace(a.Value)
				}
			}
		}
		if idx > 0 && name != "" {
			if _, exists := t.names[idx]; !exists {
				t.names[idx] = name
			}
		}
	}
}

// Err 加载过程中的致命错误（目录不存在不算）。
func (t *Table) Err() error {
	t.load()
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// SortedIndexes 有名字的任务号（升序；测试/排障用）。
func (t *Table) SortedIndexes() []int {
	t.load()
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]int, 0, len(t.names))
	for k := range t.names {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// atoi 宽松数字解析（带空白的属性值也能吃）。
func atoi(s string) int {
	n, ok := 0, false
	for _, r := range strings.TrimSpace(s) {
		if r < '0' || r > '9' {
			return 0
		}
		n, ok = n*10+int(r-'0'), true
	}
	if !ok {
		return 0
	}
	return n
}
