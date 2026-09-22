// Package chainlib 链数据（骨架层：**文件驱动、零内置数据**）。
//
// 设计约定：
//   - 链数据文件：<DataDir>/chains/<chain_id>.json（可选）。有文件就用文件内容，
//     没有文件时只下发 chain_id（chain 字段省略），由机器人端决定怎么跑；
//   - 文件内容**原样透传**：未声明字段与字段形状都不丢（见 passthrough.go）；
//   - 文件名以 "_" 开头视为模板/内部文件，不出现在 /api/chains 列表里；
//   - 链数据由使用方自己准备（导出方式见 docs/05-运维/链数据.md）。
package chainlib

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrNotFound 链数据文件不存在。
var ErrNotFound = errors.New("链数据文件不存在")

// Chain 链数据（只声明了常用字段，其余原样透传）。
//
// ⚠️ npcs / task_hints / map_grids / dijkstra / task_order 用 RawMessage 原样透传：
// 真实数据里同一字段存在多种形状（对象/数组/扁平三元组），中控只做搬运，
// 用强类型会在解析真实数据时失败或悄悄改形。
type Chain struct {
	ChainID   string                     `json:"chain_id"`
	Name      string                     `json:"name,omitempty"`
	StartTask int                        `json:"start_task"`
	EndTask   int                        `json:"end_task"`
	NPCs      map[string]json.RawMessage `json:"npcs"`
	TaskHints map[string]json.RawMessage `json:"task_hints"`
	TaskOrder []json.RawMessage          `json:"task_order"`
	GridCell  int                        `json:"grid_cell"`
	MapGrids  map[string]json.RawMessage `json:"map_grids"`
	Dijkstra  map[string]json.RawMessage `json:"dijkstra"`
	Maps      map[string]string          `json:"maps,omitempty"`

	// Extra 保存文件里的其它字段，原样透传给机器人。
	Extra map[string]json.RawMessage `json:"-"`
}

// knownKeys 是 Chain 已显式声明的字段名（其余进 Extra）。
var knownKeys = []string{
	"chain_id", "name", "start_task", "end_task", "npcs", "task_hints",
	"task_order", "grid_cell", "map_grids", "dijkstra", "maps",
}

// UnmarshalJSON 解析链数据：已知字段进结构体，未知字段原样保留到 Extra。
func (c *Chain) UnmarshalJSON(data []byte) error {
	type alias Chain // 别名无方法集，避免递归调用本方法
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	extra, err := splitExtra(data, knownKeys)
	if err != nil {
		return err
	}
	*c = Chain(a)
	c.Extra = extra
	return nil
}

// MarshalJSON 输出链数据：已知字段 + Extra 原样合并。
func (c Chain) MarshalJSON() ([]byte, error) {
	type alias Chain
	base, err := json.Marshal(alias(c))
	if err != nil {
		return nil, err
	}
	return mergeExtra(base, c.Extra)
}

// Build 读取链数据文件 <chainDir>/<chainID>.json 并规范化缺省字段。
// 文件不存在返回 ErrNotFound（调用方决定是"省略 chain 字段"还是报错）。
func Build(chainID, chainDir string) (*Chain, error) {
	if strings.TrimSpace(chainID) == "" {
		return nil, errors.New("chain_id 为空")
	}
	path := FilePath(chainID, chainDir)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var c Chain
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	normalize(&c, chainID)
	return &c, nil
}

// FilePath 链数据文件路径。
func FilePath(chainID, chainDir string) string {
	return filepath.Join(chainDir, chainID+".json")
}

// Info 链列表条目（面板用）。
//
// ID 恒等于**文件名**（去掉 .json），保证「列表里看到的 id 一定能通过 ?id= 查到」；
// 文件内部写的 chain_id 放在 ChainID 字段（展示用，下发时以它为准）。
type Info struct {
	ID        string `json:"id"`                // 文件名（可寻址，用于 ?id=）
	ChainID   string `json:"chain_id,omitempty"` // 文件内声明的 chain_id（下发时以它为准）
	Name      string `json:"name,omitempty"`
	StartTask int    `json:"start_task"`
	EndTask   int    `json:"end_task"`
	TaskCount int    `json:"task_count"`
	File      string `json:"file"`
	Error     string `json:"error,omitempty"` // 文件损坏时给出原因（列表不因此中断）
}

// List 扫描链数据目录（跳过 "_" 开头的模板文件与子目录）。
func List(chainDir string) []Info {
	entries, err := os.ReadDir(chainDir)
	if err != nil {
		return []Info{}
	}
	out := make([]Info, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, "_") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		info := Info{ID: id, File: filepath.ToSlash(filepath.Join("chains", name))}
		raw, err := os.ReadFile(filepath.Join(chainDir, name))
		if err != nil {
			info.Error = err.Error()
			out = append(out, info)
			continue
		}
		var c Chain
		if err := json.Unmarshal(raw, &c); err != nil {
			info.Error = "JSON 解析失败: " + err.Error()
			out = append(out, info)
			continue
		}
		info.ChainID = c.ChainID
		info.Name = c.Name
		info.StartTask = c.StartTask
		info.EndTask = c.EndTask
		info.TaskCount = len(c.TaskOrder)
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// normalize 补齐缺省字段（空表必须是空对象而非 nil，机器人端按 JSON 对象解析）。
func normalize(c *Chain, chainID string) {
	if c.ChainID == "" {
		c.ChainID = chainID
	}
	if c.NPCs == nil {
		c.NPCs = map[string]json.RawMessage{}
	}
	if c.TaskHints == nil {
		c.TaskHints = map[string]json.RawMessage{}
	}
	if c.TaskOrder == nil {
		c.TaskOrder = []json.RawMessage{}
	}
	if c.MapGrids == nil {
		c.MapGrids = map[string]json.RawMessage{}
	}
	if c.Dijkstra == nil {
		c.Dijkstra = map[string]json.RawMessage{}
	}
	if c.GridCell == 0 {
		c.GridCell = 16
	}
	if c.Extra == nil {
		c.Extra = map[string]json.RawMessage{}
	}
}
