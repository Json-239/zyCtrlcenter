// Package maplib 地图名表（map_index → 地图名）+ 客户端坐标口径。
//
// 数据来源：data/maps.json（由 tools/export_maps.py 从游戏 config/map.csv 导出，131 张图）。
// 用途：把机器人上报的 mapid 显示成中文地图名；把像素坐标换算成**客户端显示的格子坐标**。
//
// 坐标口径（与参考项目一致，已实测验证）：
//
//	机器人上报 pos = 服务端像素坐标（如 2108,809）
//	客户端地图显示 = 像素 ÷ 16（如 131,49）→ GridCell = 16
//
// 本包只依赖标准库；表文件改了会自动热更新（按 mtime+size 判断）。
package maplib

import (
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GridCell 1 格 = 16 像素（客户端坐标 = 像素 / 16）。
const GridCell = 16

// GridPos 像素坐标 → 客户端格子坐标（向下取整，与参考实现 Math.floor(x/16) 一致）。
func GridPos(x, y int) []int { return GridPosWith(x, y, GridCell) }

// GridPosWith 指定格子尺寸的换算（中控可用 --grid-cell / CTRL_GRID_CELL 覆盖）。
func GridPosWith(x, y, cell int) []int {
	if cell <= 0 {
		cell = GridCell
	}
	return []int{x / cell, y / cell}
}

// Table 地图名表（并发安全 + 文件热更新）。
type Table struct {
	path string

	mu    sync.RWMutex
	byID  map[int]string
	byStr map[string]string
	// 文件指纹（变了才重载）
	mtime time.Time
	size  int64
	ok    bool
}

// New 创建表（path 通常为 <数据目录>/maps.json）。
func New(path string) *Table {
	return &Table{path: path, byID: map[int]string{}, byStr: map[string]string{}}
}

// Path 表文件路径。
func (t *Table) Path() string { return t.path }

// Load 读取表文件（文件不存在不报错：视为空表，面板回退显示 #id）。
func (t *Table) Load() error {
	raw, err := os.ReadFile(t.path)
	if err != nil {
		if os.IsNotExist(err) {
			t.mu.Lock()
			t.byID, t.byStr, t.ok = map[int]string{}, map[string]string{}, false
			t.mu.Unlock()
			return nil
		}
		return err
	}
	// 允许 {"_comment": "...", "1": "彩荷清池", ...} 这种带注释的对象
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return err
	}
	byID := make(map[int]string, len(obj))
	byStr := make(map[string]string, len(obj))
	for k, v := range obj {
		name, _ := v.(string)
		if strings.HasPrefix(k, "_") || name == "" {
			continue
		}
		id, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		byID[id] = name
		byStr[strconv.Itoa(id)] = name
	}

	st, _ := os.Stat(t.path)
	t.mu.Lock()
	t.byID, t.byStr, t.ok = byID, byStr, len(byID) > 0
	if st != nil {
		t.mtime, t.size = st.ModTime(), st.Size()
	}
	t.mu.Unlock()
	return nil
}

// refresh 文件变化时重载（每次查询前调用，stat 开销极小）。
func (t *Table) refresh() {
	st, err := os.Stat(t.path)
	if err != nil {
		return
	}
	t.mu.RLock()
	same := t.ok && st.ModTime().Equal(t.mtime) && st.Size() == t.size
	t.mu.RUnlock()
	if !same {
		_ = t.Load()
	}
}

// Name 取地图名（未知返回空串，调用方回退显示 #id）。
func (t *Table) Name(mapid int) string {
	t.refresh()
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.byID[mapid]
}

// NameStr 取地图名（字符串键版本，便于直接吃来路不明的 mapid）。
func (t *Table) NameStr(mapid string) string {
	if mapid == "" {
		return ""
	}
	if id, err := strconv.Atoi(mapid); err == nil {
		return t.Name(id)
	}
	t.refresh()
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.byStr[mapid]
}

// All 返回全部映射（字符串键，便于 JSON 输出）。
func (t *Table) All() map[string]string {
	t.refresh()
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]string, len(t.byStr))
	for k, v := range t.byStr {
		out[k] = v
	}
	return out
}

// Count 条目数。
func (t *Table) Count() int {
	t.refresh()
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.byID)
}

// Loaded 是否成功加载到数据。
func (t *Table) Loaded() bool {
	t.refresh()
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.ok
}

// Suggest 便于排查：返回按 id 排序的前 n 条（测试/日志用）。
func (t *Table) Suggest(n int) []string {
	t.refresh()
	t.mu.RLock()
	ids := make([]int, 0, len(t.byID))
	for id := range t.byID {
		ids = append(ids, id)
	}
	t.mu.RUnlock()
	sort.Ints(ids)
	if n > 0 && len(ids) > n {
		ids = ids[:n]
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, strconv.Itoa(id)+"="+t.Name(id))
	}
	return out
}
