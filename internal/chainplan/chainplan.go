// Package chainplan 把链数据（提示表）展开成**模块序列**，并做结构校验。
//
// 定位：中控不执行链，机器人端才执行（见 docs/02-架构/任务链路模块化设计.md）。这里做的是
// "把提示表翻译成看得懂的模块序列 + 把明显的坑提前报出来"，供面板展示与排障；
// 同时它是**两边审查的锚点**：模块名与参考实现的执行器/动作一一对应。
//
// 数据契约（与参考实现 robot/ctrlcenter/chainlib.py:94-164 的生成物一致）：
//
//	task_order[]: {task_index,name,catcher_npc,catcher_name,thrower_npc,thrower_name,next[]}
//	task_hints{}: {"executor":"shop_purchase","shop":{npc,item_index,count}}
//	              {"executor":"alloc_point"}
//	              {"executor":"fight_npc","fight_npc":{npc_index,npc_name}}
//	npcs{}:       npc_index → 坐标（真实导出是扁平 [map,x,y]；模板里是嵌套 [[map,x,y],…]，两种都吃）
//
// **声明：模块序列是"骨架 + 排障地图"，不是固定脚本。** 实际推进由服务端推送驱动；抓鬼/捉鬼这类链要
// 自己寻路去找任务给予者 NPC 接取，刷鬼点/任务怪/动态 NPC 都由服务端给（每次跑不一样）；抓鬼日常更是
// 循环状态机、没有 task_order（此处只给导航数据）。拆模块的目的是"卡住时一眼看出卡在哪个模块"。
//
// 组装顺序（贴合参考实现的实际行为）：
//
//	前置战斗 NPC：寻路 → 移动 → 战斗 → 状态补充 →（再回去）catcher 寻路 → 移动 → 对话接取
//	                    → 购买 / 加点 → 交付（thrower 与 catcher 不同才再走一遍）→ 等下一环
package chainplan

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"zyctrlcenter/internal/chainlib"
)

// Module 行为模块（与参考实现里的动作/执行器对应；中控只描述，不执行）。
type Module string

const (
	// ModPathfind 寻路（同图网格 A*）
	ModPathfind Module = "pathfind"
	// ModMove 沿路径移动
	ModMove Module = "move"
	// ModCrossMap 跨图跳转（dijkstra 路由）
	ModCrossMap Module = "cross_map"
	// ModTalk 点击 NPC 对话（接取/交付）
	ModTalk Module = "talk"
	// ModBuy 商店购买
	ModBuy Module = "buy"
	// ModFight 战斗（前置战斗/打怪）
	ModFight Module = "fight"
	// ModAlloc 属性加点
	ModAlloc Module = "alloc"
	// ModRestore 状态补充（回血回蓝/补给；机器人端策略，链数据里没有该字段）
	ModRestore Module = "restore"
	// ModWait 等下一环任务推送（ADD_TASK）
	ModWait Module = "wait"
)

// ExecModules 链数据 task_hints[].executor → 模块（名字与参考实现一致，别改）。
var ExecModules = map[string]Module{
	"shop_purchase": ModBuy,
	"alloc_point":   ModAlloc,
	"fight_npc":     ModFight,
}

// moduleOrder 模块全集（稳定顺序，供文档/面板/测试用）。
var moduleOrder = []Module{
	ModPathfind, ModMove, ModCrossMap, ModTalk, ModBuy, ModFight, ModAlloc, ModRestore, ModWait,
}

// ModuleNames 模块全集（稳定顺序）。
func ModuleNames() []Module { return append([]Module(nil), moduleOrder...) }

// activeAcceptChains 允许"主动找给予者接取"的链（参考实现里按链 id 特判：quest_engine.py:1464-1476）。
var activeAcceptChains = map[string]bool{"zhuaogui": true}

// Step 一个模块步骤。
type Step struct {
	Order     int            `json:"order"`
	TaskIndex int            `json:"task_index,omitempty"`
	Module    Module         `json:"module"`
	Action    string         `json:"action,omitempty"` // walk|accept|submit|shop|hp_mp|...
	Args      map[string]any `json:"args,omitempty"`
	Source    string         `json:"source"` // data（链数据里有）/ derived（推导）/ policy（机器人端策略）
	Note      string         `json:"note,omitempty"`
}

// Task 一个任务节点展开出的步骤序列。
type Task struct {
	TaskIndex int    `json:"task_index"`
	Name      string `json:"name,omitempty"`
	Catcher   string `json:"catcher_npc,omitempty"`
	Thrower   string `json:"thrower_npc,omitempty"`
	Next      []int  `json:"next,omitempty"`
	Steps     []Step `json:"steps"`
}

// Plan 整条链的模块计划（面板用；不参与执行）。
type Plan struct {
	ChainID      string         `json:"chain_id"`
	StartTask    int            `json:"start_task"`
	EndTask      int            `json:"end_task"`
	ActiveAccept bool           `json:"active_accept"`
	Tasks        []Task         `json:"tasks"`
	Warnings     []string       `json:"warnings,omitempty"`
	Modules      map[string]int `json:"modules"`
}

// ---------------------------------------------------------------- 数据形状

type orderEntry struct {
	TaskIndex   int    `json:"task_index"`
	Name        string `json:"name"`
	CatcherNPC  string `json:"catcher_npc"`
	CatcherName string `json:"catcher_name"`
	ThrowerNPC  string `json:"thrower_npc"`
	ThrowerName string `json:"thrower_name"`
	Next        []int  `json:"next"`
}

type hintEntry struct {
	Executor string `json:"executor"`
	Shop     struct {
		NPC       any `json:"npc"`
		ItemIndex any `json:"item_index"`
		Count     any `json:"count"`
	} `json:"shop"`
	FightNPC struct {
		NPCIndex any `json:"npc_index"`
		NPCName  any `json:"npc_name"`
	} `json:"fight_npc"`
}

type routeEntry struct {
	FromMap   int    `json:"from_map"`
	TargetMap int    `json:"target_map"`
	X         int    `json:"x"`
	Y         int    `json:"y"`
	Kind      string `json:"kind"`
	TargetName string `json:"target_name"`
}

// pos 一个坐标（map + 格子/像素坐标）。
type pos struct {
	Map int
	X   int
	Y   int
}

// ---------------------------------------------------------------- 组装

// Build 组装模块序列；结构性错误返回 error（聚合所有问题），可疑之处进 Plan.Warnings。
func Build(c *chainlib.Chain) (*Plan, error) {
	if c == nil {
		return nil, errors.New("链数据为空")
	}
	orders, err := parseOrder(c.TaskOrder)
	if err != nil {
		return nil, err
	}
	hints, err := parseHints(c.TaskHints)
	if err != nil {
		return nil, err
	}
	npcs, err := parseNPCs(c.NPCs)
	if err != nil {
		return nil, err
	}
	routes := parseRoutes(c.Dijkstra)

	p := &Plan{
		ChainID: c.ChainID, StartTask: c.StartTask, EndTask: c.EndTask,
		ActiveAccept: activeAcceptChains[c.ChainID],
		Modules:      map[string]int{},
	}
	if len(orders) == 0 {
		p.Warnings = append(p.Warnings,
			"task_order 为空：只下发 chain_id，由机器人端自行决定怎么跑（中控不编步骤）")
		return p, nil
	}

	var problems []string
	inChain := map[int]bool{}
	for _, o := range orders {
		inChain[o.TaskIndex] = true
	}
	seen := map[int]bool{}

	for _, o := range orders {
		if o.TaskIndex == 0 {
			problems = append(problems, "task_order 里有节点缺 task_index")
			continue
		}
		if seen[o.TaskIndex] {
			problems = append(problems, fmt.Sprintf("task_index %d 重复", o.TaskIndex))
			continue
		}
		seen[o.TaskIndex] = true

		// next 引用：三种情况——
		//   1) 在 task_order 里 / 就是链尾 end_task → 正常；
		//   2) start..end **数值区间有意义**（start<=end）时缺失 → 硬错误（会卡 WAIT_NEXT）；
		//   3) 其它（跨 XML 段/下一段链）→ 只提示。
		// 注意：真实新手链是 start=7001001 … end=5010105（**跨 XML 段**，数值上 start>end），
		// 参考实现的 start/end 只是遍历边界（chainlib.py:448 遇 end_task 停），不是数值区间。
		for _, nx := range o.Next {
			switch {
			case inChain[nx] || nx == c.EndTask:
				// 链内后继或链尾：正常
			case c.StartTask <= c.EndTask && nx >= c.StartTask && nx <= c.EndTask:
				problems = append(problems, fmt.Sprintf(
					"任务 %d 的后继 %d 在链区间（%d~%d）内却不在 task_order 里（会卡在 WAIT_NEXT）",
					o.TaskIndex, nx, c.StartTask, c.EndTask))
			default:
				p.Warnings = append(p.Warnings, fmt.Sprintf(
					"任务 %d 的后继 %d 不在本链 task_order 里（跨 XML 段/下一段链）：等它推来即可",
					o.TaskIndex, nx))
			}
		}

		hint := hints[o.TaskIndex]
		task := Task{TaskIndex: o.TaskIndex, Name: o.Name, Catcher: o.CatcherNPC,
			Thrower: o.ThrowerNPC, Next: o.Next}
		steps := []Step{}

		// 1) 前置战斗：先打再点 catcher（参考实现注释：否则剧情不推进）
		if hint.Executor == "fight_npc" {
			npcIdx := intOf(hint.FightNPC.NPCIndex)
			if npcIdx == 0 {
				p.Warnings = append(p.Warnings, fmt.Sprintf(
					"任务 %d 标了前置战斗但没给 npc_index（只能等动态 NPC 推送）", o.TaskIndex))
			}
			steps = append(steps, travelSteps(fmt.Sprint(npcIdx), npcs, routes, o.TaskIndex, p)...)
			steps = append(steps, Step{
				Module: ModFight, Action: "fight", Source: "data",
				Args: map[string]any{"npc_index": npcIdx, "npc_name": strOf(hint.FightNPC.NPCName)},
				Note: "前置战斗：打完再点 catcher",
			})
			steps = append(steps, Step{
				Module: ModRestore, Action: "hp_mp", Source: "policy",
				Note: "战后状态补充（回血/回蓝/补给；机器人端策略，链数据无此字段）",
			})
		}

		// 2) 去接取点（catcher；主动接取链可找 thrower）
		if o.CatcherNPC != "" {
			steps = append(steps, travelSteps(o.CatcherNPC, npcs, routes, o.TaskIndex, p)...)
			steps = append(steps, Step{
				Module: ModTalk, Action: "accept", Source: "derived",
				Args: map[string]any{"npc": o.CatcherNPC, "npc_name": o.CatcherName},
				Note: "点 NPC 接取/推进任务",
			})
		} else if o.ThrowerNPC != "" && p.ActiveAccept {
			steps = append(steps, travelSteps(o.ThrowerNPC, npcs, routes, o.TaskIndex, p)...)
			steps = append(steps, Step{
				Module: ModTalk, Action: "accept", Source: "derived",
				Args: map[string]any{"npc": o.ThrowerNPC, "npc_name": o.ThrowerName},
				Note: "无 catcher：按参考实现主动找给予者接取（仅该链允许）",
			})
		} else if o.CatcherNPC == "" && o.ThrowerNPC == "" {
			p.Warnings = append(p.Warnings, fmt.Sprintf(
				"任务 %d 没有 catcher/thrower：只能等服务端推来（机器人端自行处理）", o.TaskIndex))
		}

		// 3) 提示类模块：购买 / 加点
		switch hint.Executor {
		case "shop_purchase":
			shopNPC := intOf(hint.Shop.NPC)
			if shopNPC == 0 {
				problems = append(problems, fmt.Sprintf(
					"任务 %d 的 shop_purchase 缺 shop.npc（不知道该找谁买）", o.TaskIndex))
			}
			if hint.Shop.ItemIndex == nil {
				p.Warnings = append(p.Warnings, fmt.Sprintf(
					"任务 %d 的 shop_purchase 缺 item_index（按商店默认/提示买）", o.TaskIndex))
			}
			steps = append(steps, Step{
				Module: ModBuy, Action: "shop", Source: "data",
				Args: map[string]any{
					"npc": shopNPC, "item_index": intOf(hint.Shop.ItemIndex),
					"count": intOfOr(hint.Shop.Count, 1),
				},
				Note: "按链数据提示购买",
			})
		case "alloc_point":
			steps = append(steps, Step{
				Module: ModAlloc, Action: "alloc", Source: "data",
				Note: "属性加点（任务要求）",
			})
		case "":
			// 无特殊提示：接取后等推进
		default:
			if _, ok := ExecModules[hint.Executor]; !ok {
				problems = append(problems, fmt.Sprintf(
					"任务 %d 的 executor=%q 是未知执行器（本侧只认 shop_purchase/alloc_point/fight_npc）",
					o.TaskIndex, hint.Executor))
			}
		}

		// 4) 交付：thrower 与 catcher 不同才再走一遍
		if o.ThrowerNPC != "" && o.ThrowerNPC != o.CatcherNPC {
			steps = append(steps, travelSteps(o.ThrowerNPC, npcs, routes, o.TaskIndex, p)...)
			steps = append(steps, Step{
				Module: ModTalk, Action: "submit", Source: "derived",
				Args: map[string]any{"npc": o.ThrowerNPC, "npc_name": o.ThrowerName},
				Note: "找给予者交付",
			})
		}

		// 5) 收尾：等下一环推送（ADD_TASK 到达即前进）
		steps = append(steps, Step{
			Module: ModWait, Action: "next_task", Source: "policy",
			Args: map[string]any{"next": o.Next},
			Note: "等下一环任务推送（S2C_ADD_TASK）",
		})

		for i := range steps {
			steps[i].Order = i + 1
			steps[i].TaskIndex = o.TaskIndex
			p.Modules[string(steps[i].Module)]++
		}
		task.Steps = steps
		p.Tasks = append(p.Tasks, task)
	}

	if len(problems) > 0 {
		return p, errors.New(strings.Join(problems, "；"))
	}
	return p, nil
}

// travelSteps 生成"去某个 NPC"的步骤：寻路 →（跨图）→ 移动。
func travelSteps(npcKey string, npcs map[string][]pos, routes []routeEntry, taskIndex int, p *Plan) []Step {
	ps := npcs[npcKey]
	if len(ps) == 0 {
		if npcKey != "" && npcKey != "0" {
			p.Warnings = append(p.Warnings, fmt.Sprintf(
				"任务 %d 的 NPC %s 在 npcs 里没有坐标（等动态 NPC 推送 S2C_ADD_TASK_DYNAMIC_NPC）",
				taskIndex, npcKey))
		}
		return []Step{{
			Module: ModPathfind, Action: "to_npc", Source: "derived",
			Args: map[string]any{"npc": npcKey, "maps": []int{}},
			Note: "无坐标：等动态 NPC 推送后寻路",
		}}
	}
	maps := make([]int, 0, len(ps))
	for _, q := range ps {
		maps = append(maps, q.Map)
	}
	sort.Ints(maps)
	out := []Step{{
		Module: ModPathfind, Action: "to_npc", Source: "derived",
		Args: map[string]any{"npc": npcKey, "maps": maps},
		Note: "同图网格 A*（map_grids）",
	}}
	if r, ok := pickRoute(routes, maps); ok {
		out = append(out, Step{
			Module: ModCrossMap, Action: "hop", Source: "derived",
			Args: map[string]any{"from_map": r.FromMap, "target_map": r.TargetMap,
				"x": r.X, "y": r.Y, "kind": r.Kind, "target_name": r.TargetName},
			Note: "跨图跳转（dijkstra 路由）",
		})
	}
	first := ps[0]
	out = append(out, Step{
		Module: ModMove, Action: "walk", Source: "derived",
		Args: map[string]any{"map": first.Map, "x": first.X, "y": first.Y, "candidates": len(ps)},
		Note: "沿路径移动（多候选坐标时逐个试）",
	})
	return out
}

// pickRoute 在 npcs 涉及多张图时，找一条能用的跨图路由（确定性：按 kind/from_map 排序取第一条）。
func pickRoute(routes []routeEntry, maps []int) (routeEntry, bool) {
	if len(routes) == 0 || len(maps) < 2 {
		return routeEntry{}, false
	}
	want := map[int]bool{}
	for _, m := range maps {
		want[m] = true
	}
	cands := make([]routeEntry, 0, len(routes))
	for _, r := range routes {
		if want[r.FromMap] && want[r.TargetMap] {
			cands = append(cands, r)
		}
	}
	if len(cands) == 0 {
		// 退一步：只要路由的图在候选里（说明确实要跨图）
		for _, r := range routes {
			if want[r.FromMap] || want[r.TargetMap] {
				cands = append(cands, r)
			}
		}
	}
	if len(cands) == 0 {
		return routeEntry{}, false
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].FromMap != cands[j].FromMap {
			return cands[i].FromMap < cands[j].FromMap
		}
		return cands[i].TargetMap < cands[j].TargetMap
	})
	return cands[0], true
}

// ---------------------------------------------------------------- 解析

func parseOrder(raw []json.RawMessage) ([]orderEntry, error) {
	out := make([]orderEntry, 0, len(raw))
	for i, r := range raw {
		var e orderEntry
		if err := json.Unmarshal(r, &e); err != nil {
			return nil, fmt.Errorf("task_order[%d] 解析失败: %w", i, err)
		}
		// catcher/thrower 可能是数字（不同导出形态）→ 统一成字符串
		e.CatcherNPC = normNPCID(r, "catcher_npc", e.CatcherNPC)
		e.ThrowerNPC = normNPCID(r, "thrower_npc", e.ThrowerNPC)
		out = append(out, e)
	}
	return out, nil
}

// normNPCID 兼容 "13035" 与 13035 两种写法。
func normNPCID(raw json.RawMessage, key, fallback string) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return fallback
	}
	v, ok := m[key]
	if !ok {
		return fallback
	}
	var s string
	if err := json.Unmarshal(v, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var n int
	if err := json.Unmarshal(v, &n); err == nil && n != 0 {
		return fmt.Sprint(n)
	}
	return fallback
}

func parseHints(raw map[string]json.RawMessage) (map[int]hintEntry, error) {
	out := map[int]hintEntry{}
	for k, v := range raw {
		var h hintEntry
		if err := json.Unmarshal(v, &h); err != nil {
			return nil, fmt.Errorf("task_hints[%s] 解析失败: %w", k, err)
		}
		var ti int
		if _, err := fmt.Sscanf(k, "%d", &ti); err != nil {
			continue // 非任务号键（预留字段）：忽略
		}
		out[ti] = h
	}
	return out, nil
}

// parseNPCs npc_index → 坐标列表；兼容扁平 [map,x,y] 与嵌套 [[map,x,y],…]。
func parseNPCs(raw map[string]json.RawMessage) (map[string][]pos, error) {
	out := map[string][]pos{}
	for k, v := range raw {
		var flat []int
		if err := json.Unmarshal(v, &flat); err == nil && len(flat) == 3 {
			out[k] = []pos{{Map: flat[0], X: flat[1], Y: flat[2]}}
			continue
		}
		var nested [][]int
		if err := json.Unmarshal(v, &nested); err == nil {
			ps := make([]pos, 0, len(nested))
			for _, t := range nested {
				if len(t) == 3 {
					ps = append(ps, pos{Map: t[0], X: t[1], Y: t[2]})
				}
			}
			if len(ps) > 0 {
				out[k] = ps
			}
			continue
		}
		// 其它形状（未来扩展）：当作没有坐标，别报错——中控对链数据是"只搬运 + 尽力解读"
	}
	return out, nil
}

// parseRoutes dijkstra 的一个键下可能是**单个路由对象**，也可能是**路由数组**
// （真实导出两种都有；用客观数据核查时抓出来的：数组形状曾被静默跳过 → 跨图模块全丢）。
func parseRoutes(raw map[string]json.RawMessage) []routeEntry {
	out := make([]routeEntry, 0, len(raw))
	for _, v := range raw {
		out = append(out, decodeRoutes(v)...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FromMap != out[j].FromMap {
			return out[i].FromMap < out[j].FromMap
		}
		return out[i].TargetMap < out[j].TargetMap
	})
	return out
}

// decodeRoutes 兼容"单对象 / 数组"两种形状（数组里的每条都要收）。
func decodeRoutes(raw json.RawMessage) []routeEntry {
	var one routeEntry
	if err := json.Unmarshal(raw, &one); err == nil && (one.FromMap != 0 || one.TargetMap != 0) {
		return []routeEntry{one}
	}
	var many []routeEntry
	if err := json.Unmarshal(raw, &many); err == nil {
		out := make([]routeEntry, 0, len(many))
		for _, r := range many {
			if r.FromMap != 0 || r.TargetMap != 0 {
				out = append(out, r)
			}
		}
		return out
	}
	return nil
}

func intOf(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case float64:
		return int(t)
	case string:
		var n int
		_, _ = fmt.Sscanf(strings.TrimSpace(t), "%d", &n)
		return n
	}
	return 0
}

func intOfOr(v any, def int) int {
	if n := intOf(v); n != 0 {
		return n
	}
	return def
}

func strOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
