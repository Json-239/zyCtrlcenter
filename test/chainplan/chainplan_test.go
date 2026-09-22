// 链路模块化组装：把"提示表"（task_order/task_hints/npcs/dijkstra）展开成
// **模块序列**（寻路 → 移动 →（跨图）→ 战斗 → 状态补充 → 对话接取 → 购买/加点 → 对话交付 → 等下一环），
// 并做结构校验（悬空 next / 未知执行器 / 缺参数）。
//
// 口径与参考实现对齐（robot/ctrlcenter/chainlib.py:94-164 生成的就是这些字段）：
//
//	task_order[]: {task_index,name,catcher_npc,catcher_name,thrower_npc,thrower_name,next[]}
//	task_hints{}: {"executor":"shop_purchase","shop":{npc,item_index,count}}
//	              {"executor":"alloc_point"} / {"executor":"fight_npc","fight_npc":{npc_index,npc_name}}
package chainplan_test

import (
	"encoding/json"
	"strings"
	"testing"

	"zyctrlcenter/internal/chainlib"
	"zyctrlcenter/internal/chainplan"
	"zyctrlcenter/test/testsupport"
)

// 造一条"用满所有模块"的链（字段名与参考实现的生成物一致）。
func fullChain(t *testing.T) *chainlib.Chain {
	t.Helper()
	return buildChain(t, `{
	  "chain_id": "newbie_full",
	  "name": "新手链",
	  "start_task": 7001001,
	  "end_task": 7001003,
	  "grid_cell": 16,
	  "npcs": {
	    "13035": [[5, 1504, 1120]],
	    "13036": [[12, 3931, 882], [15, 100, 200]],
	    "10103": [[5, 1600, 1200]],
	    "17016": [[5, 700, 800]]
	  },
	  "map_grids": {"5": {"w": 10, "h": 10, "rows": []}},
	  "dijkstra": {
	    "5": {"destination_index": 5, "from_map": 5, "target_map": 12, "kind": "map_skip",
	          "x": 1807, "y": 983, "target_name": "长安"}
	  },
	  "task_order": [
	    {"task_index": 7001001, "name": "天命所归", "catcher_npc": "10103", "catcher_name": "李捕头",
	     "thrower_npc": "10103", "next": [7001002]},
	    {"task_index": 7001002, "name": "酒中仙", "catcher_npc": "13035", "catcher_name": "店小二",
	     "thrower_npc": "13035", "next": [7001003]},
	    {"task_index": 7001003, "name": "采购", "catcher_npc": "13036", "catcher_name": "杂货商",
	     "thrower_npc": "13036", "next": [7001004]}
	  ],
	  "task_hints": {
	    "7001002": {"executor": "fight_npc", "fight_npc": {"npc_index": 17016, "npc_name": "恶修罗"}},
	    "7001003": {"executor": "shop_purchase", "shop": {"npc": 13520, "item_index": 101403, "count": 2}}
	  }
	}`)
}

func buildChain(t *testing.T, raw string) *chainlib.Chain {
	t.Helper()
	var c chainlib.Chain
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("测试链数据写错了: %v", err)
	}
	return &c
}

// 组装顺序：先到"前置战斗 NPC"打完再点 catcher（参考实现注释：
// "机器人须先触发战斗再点 catcher，否则剧情不推进"），最后等下一环推送。
func TestBuildExpandsModuleSequenceInOrder(t *testing.T) {
	plan, err := chainplan.Build(fullChain(t))
	if err != nil {
		t.Fatalf("应能组装: %v", err)
	}
	if plan.ChainID != "newbie_full" || len(plan.Tasks) != 3 {
		t.Fatalf("链信息不符: %+v", plan)
	}
	mods := func(i int) []string {
		out := []string{}
		for _, s := range plan.Tasks[i].Steps {
			out = append(out, string(s.Module))
		}
		return out
	}
	// 任务 1：无 hint，同一个人接取+交付 → 寻路/移动/对话/等下一环
	if got := strings.Join(mods(0), ","); got != "pathfind,move,talk,wait" {
		t.Fatalf("任务 1 模块序列不符: %s", got)
	}
	// 任务 2：有前置战斗 → 先打（+ 战后状态补充），再回去点 catcher
	if got := strings.Join(mods(1), ","); got != "pathfind,move,fight,restore,pathfind,move,talk,wait" {
		t.Fatalf("任务 2 模块序列不符: %s", got)
	}
	// 任务 3：catcher 有 12/15 两个候选图（必然跨图）→ 寻路→跨图→移动→对话接取→购买→等下一环
	got := strings.Join(mods(2), ",")
	if got != "pathfind,cross_map,move,talk,buy,wait" {
		t.Fatalf("任务 3 模块序列不符: %s", got)
	}
	// 购买必须在接取之后（任务要求买了才交）
	if strings.Index(got, "talk") > strings.Index(got, "buy") {
		t.Fatalf("购买不该排在接取之前: %s", got)
	}
}

// 模块名必须与参考实现里的执行器/动作对得上（两边审查用；名字漂移会让人找不到对照）。
func TestModuleNamesMatchReferenceExecutors(t *testing.T) {
	want := map[string]chainplan.Module{
		"shop_purchase": chainplan.ModBuy,
		"alloc_point":   chainplan.ModAlloc,
		"fight_npc":     chainplan.ModFight,
	}
	for exec, mod := range want {
		if got := chainplan.ExecModules[exec]; got != mod {
			t.Fatalf("执行器 %s 应映射到 %s，实际 %s", exec, mod, got)
		}
	}
	// 动作模块全集（拆分的功能单元）
	all := map[chainplan.Module]bool{}
	for _, m := range chainplan.ModuleNames() {
		all[m] = true
	}
	for _, m := range []chainplan.Module{
		chainplan.ModPathfind, chainplan.ModMove, chainplan.ModCrossMap, chainplan.ModTalk,
		chainplan.ModBuy, chainplan.ModFight, chainplan.ModAlloc, chainplan.ModRestore, chainplan.ModWait,
	} {
		if !all[m] {
			t.Fatalf("模块 %s 未登记（ModuleNames 要包含全集）", m)
		}
	}
}

// 未知执行器必须**报错**（链数据用了我们没实现的提示 → fail loud，别静默跳过导致机器人卡死）。
func TestValidationRejectsUnknownExecutor(t *testing.T) {
	c := fullChain(t)
	c.TaskHints["7001002"] = json.RawMessage(`{"executor":"teleport_hack"}`)
	if _, err := chainplan.Build(c); err == nil {
		t.Fatal("未知执行器应报错")
	} else if !strings.Contains(err.Error(), "teleport_hack") {
		t.Fatalf("错误信息要点名执行器: %v", err)
	}
}

// 区间内 next 悬空 = 硬错误（会造成 WAIT_NEXT 卡死）；区间外后继 = 只是提示。
func TestValidationDanglingNext(t *testing.T) {
	c := fullChain(t)
	// 链区间放到 7001005，但 task_order 只到 7001003：7001004 在区间内却缺失 → 必须报错
	c.EndTask = 7001005
	c.TaskOrder[1] = json.RawMessage(`{"task_index":7001002,"name":"x","catcher_npc":"13035","next":[7001004]}`)
	if _, err := chainplan.Build(c); err == nil {
		t.Fatal("区间内缺失的后继应报错")
	} else if !strings.Contains(err.Error(), "7001004") {
		t.Fatalf("错误信息要点名任务号: %v", err)
	}

	c = fullChain(t)
	c.TaskOrder[2] = json.RawMessage(`{"task_index":7001003,"name":"x","catcher_npc":"13036","next":[8000001]}`)
	plan, err := chainplan.Build(c)
	if err != nil {
		t.Fatalf("区间外后继（下一段链）不该报错: %v", err)
	}
	if !hasWarning(plan, "下一段链") {
		t.Fatalf("区间外后继应给出提示: %v", plan.Warnings)
	}
}

// 真实新手链形态：start=7001001 → end=5010105（跨 XML 段，数值上 start>end）。
// 此时 end 不能当"区间上界"用，next 指向链外只能提示，不能误报成缺数据。
func TestDanglingNextOnlyErrorsInsideMeaningfulRange(t *testing.T) {
	c := fullChain(t)
	c.StartTask = 7001001
	c.EndTask = 5010105 // 真实新手链就是这样（70010/70011/70012.xml + 50101.xml）
	c.TaskOrder = c.TaskOrder[:1]
	c.TaskOrder[0] = json.RawMessage(`{"task_index":7001001,"name":"天命所归","catcher_npc":"10103","next":[7001002]}`)
	plan, err := chainplan.Build(c)
	if err != nil {
		t.Fatalf("跨 XML 段的链不该报错（start>end 时区间无意义）: %v", err)
	}
	if !hasWarning(plan, "7001002") {
		t.Fatalf("链外后继应给出提示: %v", plan.Warnings)
	}

	// next 正好是链尾 end_task：正常，连提示都不该有
	c = fullChain(t)
	c.TaskOrder[2] = json.RawMessage(`{"task_index":7001003,"name":"采购","catcher_npc":"13036","next":[7001003]}`)
	p2, err := chainplan.Build(c)
	if err != nil {
		t.Fatalf("指向自身/链尾不该报错: %v", err)
	}
	has := false
	for _, w := range p2.Warnings {
		if strings.Contains(w, "7001003") && strings.Contains(w, "后继") {
			has = true
		}
	}
	if has {
		t.Fatalf("链尾后继不该被当成问题: %v", p2.Warnings)
	}
}

// 购买提示缺参数要报错；缺 item_index 只提示。
func TestValidationShopHintArgs(t *testing.T) {
	c := fullChain(t)
	c.TaskHints["7001003"] = json.RawMessage(`{"executor":"shop_purchase","shop":{"item_index":101403}}`)
	if _, err := chainplan.Build(c); err == nil {
		t.Fatal("缺商店 NPC 应报错（不知道该找谁买）")
	}

	c = fullChain(t)
	c.TaskHints["7001003"] = json.RawMessage(`{"executor":"shop_purchase","shop":{"npc":13520}}`)
	plan, err := chainplan.Build(c)
	if err != nil {
		t.Fatalf("缺 item_index 不该报错: %v", err)
	}
	if !hasWarning(plan, "item_index") {
		t.Fatalf("缺 item_index 应提示: %v", plan.Warnings)
	}
	// 购买参数要原样进 args
	found := false
	for _, s := range plan.Tasks[2].Steps {
		if s.Module == chainplan.ModBuy {
			found = true
			if s.Args["npc"] != 13520 {
				t.Fatalf("购买 NPC 未进 args: %v", s.Args)
			}
		}
	}
	if !found {
		t.Fatal("应产出购买步骤")
	}
}

// NPC 坐标两种形状都要吃得下：真实导出是扁平的 [map,x,y]，模板里是嵌套 [[map,x,y],…]。
func TestNpcPositionsAcceptFlatAndNested(t *testing.T) {
	c := fullChain(t)
	// 扁平（真实形状）
	c.NPCs["13035"] = json.RawMessage(`[5,1504,1120]`)
	// 嵌套多候选（模板形状）
	c.NPCs["13036"] = json.RawMessage(`[[12,3931,882],[15,100,200]]`)

	plan, err := chainplan.Build(c)
	if err != nil {
		t.Fatalf("两种形状都该能组装: %v", err)
	}
	for _, task := range plan.Tasks {
		for _, s := range task.Steps {
			switch s.Module {
			case chainplan.ModPathfind:
				maps, _ := s.Args["maps"].([]int)
				if len(maps) == 0 {
					t.Fatalf("寻路步骤要带目标图候选: %+v", s)
				}
			case chainplan.ModCrossMap:
				if tm, _ := s.Args["target_map"].(int); tm == 0 {
					t.Fatalf("跨图步骤要带 target_map: %+v", s)
				}
			}
		}
	}
	// 嵌套两候选 → 12 图那个任务应能看到两张图
	maps12 := []int{}
	for _, s := range plan.Tasks[2].Steps {
		if s.Module == chainplan.ModPathfind {
			maps12, _ = s.Args["maps"].([]int)
			break
		}
	}
	if len(maps12) != 2 {
		t.Fatalf("多候选坐标应全部带出（12/15）: %v", maps12)
	}
}

// catcher 在 npcs 里没有坐标：只提示（机器人端等动态 NPC 推送 90351），不报错。
func TestWarningWhenNpcHasNoPosition(t *testing.T) {
	c := fullChain(t)
	delete(c.NPCs, "13036")
	plan, err := chainplan.Build(c)
	if err != nil {
		t.Fatalf("缺坐标不该报错: %v", err)
	}
	if !hasWarning(plan, "13036") {
		t.Fatalf("缺坐标应提示（等动态 NPC 推送）: %v", plan.Warnings)
	}
}

// 真实夹具（task_order 为空）：能组装、给提示；**重复 50 次结果必须完全一致**
// （不拿单次结果做文章：确定性与稳定性本身就是断言）。
func TestRealFixtureEmptyOrderIsStable(t *testing.T) {
	var wrapped map[string]json.RawMessage
	testsupport.LoadJSONFixture(t, "chains/zhuaogui_nav.sample.json", &wrapped)
	var c chainlib.Chain
	if err := json.Unmarshal(wrapped["chain"], &c); err != nil {
		t.Fatalf("真实夹具解析失败: %v", err)
	}
	first, err := chainplan.Build(&c)
	if err != nil {
		t.Fatalf("真实夹具应能组装: %v", err)
	}
	if len(first.Tasks) != 0 {
		t.Fatalf("task_order 为空时不该编出任务: %+v", first.Tasks)
	}
	if !hasWarning(first, "task_order") {
		t.Fatalf("应提示『只有 chain_id』（机器人端自行决定）: %v", first.Warnings)
	}
	if !first.ActiveAccept {
		t.Fatal("zhuaogui 允许主动接取（参考实现按链 id 特判）")
	}
	raw1, _ := json.Marshal(first)
	for i := 0; i < 50; i++ {
		p, err := chainplan.Build(&c)
		if err != nil {
			t.Fatalf("第 %d 次组装失败: %v", i, err)
		}
		raw, _ := json.Marshal(p)
		if string(raw) != string(raw1) {
			t.Fatalf("第 %d 次组装结果不稳定（map 遍历顺序？）:\n%s\n%s", i, raw1, raw)
		}
	}
}

// 每个步骤都要能被面板/排障读懂：order 递增、source 合法、module 计数与步骤数一致。
func TestStepsAreWellFormed(t *testing.T) {
	plan, err := chainplan.Build(fullChain(t))
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	valid := map[string]bool{"data": true, "derived": true, "policy": true}
	for _, task := range plan.Tasks {
		if len(task.Steps) == 0 {
			t.Fatalf("任务 %d 没有步骤", task.TaskIndex)
		}
		for i, s := range task.Steps {
			if s.Order != i+1 {
				t.Fatalf("任务 %d 第 %d 步 order 应为 %d: %+v", task.TaskIndex, i, i+1, s)
			}
			if s.TaskIndex != task.TaskIndex {
				t.Fatalf("步骤要带所属任务号: %+v", s)
			}
			if !valid[s.Source] {
				t.Fatalf("source 只能是 data/derived/policy: %+v", s)
			}
			total++
		}
	}
	sum := 0
	for _, n := range plan.Modules {
		sum += n
	}
	if sum != total {
		t.Fatalf("模块计数 %d 应等于步骤总数 %d: %v", sum, total, plan.Modules)
	}
	// 状态补充是"机器人端策略"，不是链数据字段 → source=policy
	for _, task := range plan.Tasks {
		for _, s := range task.Steps {
			if s.Module == chainplan.ModRestore && s.Source != "policy" {
				t.Fatalf("状态补充应标 policy（链数据里没有这个字段）: %+v", s)
			}
		}
	}
}

// 主动接取只对参考实现里特判的链（zhuaogui）成立，别的链不应误标。
func TestActiveAcceptOnlyForWhitelistedChain(t *testing.T) {
	if p, err := chainplan.Build(fullChain(t)); err != nil || p.ActiveAccept {
		t.Fatalf("新手链不该标主动接取: %+v %v", p.ActiveAccept, err)
	}
	c := fullChain(t)
	c.ChainID = "zhuaogui"
	p, err := chainplan.Build(c)
	if err != nil || !p.ActiveAccept {
		t.Fatalf("zhuaogui 应标主动接取: %+v %v", p.ActiveAccept, err)
	}
}

func hasWarning(p *chainplan.Plan, substr string) bool {
	for _, w := range p.Warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}
