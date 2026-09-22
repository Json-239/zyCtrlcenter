// 「模块地图」：把"我们拆出来的功能模块 + 各自职责/数据来源/对应参考实现/关联用例"摆出来。
//
// 口径（唯一，别再各写一份）：
//   - **动作层 9 个模块的键集与顺序 == chainplan.ModuleNames()**（有单测钉住，防止漏登记）；
//   - 另外三项是中控侧的两层：链数据（chainlib/chainplan）+ 编排（intent/restorer/start 自动分配）；
//   - "最近一次测试结果"读 <DataDir>/test_report.json（由 tools/test_report 生成）；缺失就显示"—"，
//     不是错误（全新克隆/还没跑过报告很正常）。
package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"

	"zyctrlcenter/internal/chainplan"
)

// ModuleCard 模块地图上的一张卡片。
type ModuleCard struct {
	Key   string   `json:"key"`   // 动作层与 chainplan 的模块名一致
	Name  string   `json:"name"`  // 中文名（面板显示）
	Layer string   `json:"layer"` // actions / data / orchestration
	Desc  string   `json:"desc"`  // 干什么
	Data  string   `json:"data"`  // 数据来源（字段名）
	Py    string   `json:"py"`    // 对应参考实现（文件/函数）
	Tests []string `json:"tests"` // 关联用例（test/<模块>::TestXxx）
}

// moduleLayer 层的展示信息（面板按它分组，不在前端再写一份中文）。
type moduleLayer struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

var moduleLayers = []moduleLayer{
	{Key: "actions", Name: "① 动作原语（机器人端执行）"},
	{Key: "data", Name: "② 链数据（只搬运）"},
	{Key: "orchestration", Name: "③ 编排（中控）"},
}

// actionCards 9 个动作模块的中文名/职责/数据来源/参考实现（与
// docs/02-架构/任务链路模块化设计.md §2.1 的表逐行对应，改一处要同步另一处）。
var actionCards = map[string]ModuleCard{
	"pathfind": {Name: "寻路", Desc: "同图网格 A*", Data: "npcs 坐标 + map_grids",
		Py: "robot_path.GridPathFinder",
		Tests: []string{"test/chainplan::TestBuildExpandsModuleSequenceInOrder",
			"test/chaindata::TestTaskNpcPositionsAreWalkable"}},
	"move": {Name: "移动", Desc: "沿路径移动（分段/拟人化）", Data: "同上",
		Py: "quest_engine.__tick_walk",
		Tests: []string{"test/chainplan::TestStepsAreWellFormed",
			"test/chaindata::TestGhostLandingPointsAreWalkable"}},
	"cross_map": {Name: "跨图", Desc: "跨图跳转", Data: "dijkstra 路由",
		Py:    "__start_next_hop / C2S_DIJKSTRA_DESTINATION(80330)",
		Tests: []string{"test/chaindata::TestDijkstraEndpointsAndPlan"}},
	"talk": {Name: "对话", Desc: "点 NPC 对话（接取/交付）", Data: "catcher_npc / thrower_npc",
		Py: "__teleport_click + __handle_dialog",
		Tests: []string{"test/chainplan::TestBuildExpandsModuleSequenceInOrder",
			"test/chainplan::TestActiveAcceptOnlyForWhitelistedChain"}},
	"buy": {Name: "购买", Desc: "商店购买", Data: "task_hints[].shop{npc,item_index,count}",
		Py:    "shop_purchase 执行器",
		Tests: []string{"test/chainplan::TestValidationShopHintArgs"}},
	"fight": {Name: "战斗", Desc: "战斗（含前置战斗）", Data: "task_hints[].fight_npc{npc_index,npc_name}",
		Py:    "fight_npc 执行器 + fight_tester",
		Tests: []string{"test/chainplan::TestModuleNamesMatchReferenceExecutors"}},
	"alloc": {Name: "加点", Desc: "属性加点", Data: "task_hints[].executor=alloc_point",
		Py:    "alloc_point 执行器",
		Tests: []string{"test/chainplan::TestModuleNamesMatchReferenceExecutors"}},
	"restore": {Name: "状态补充", Desc: "回血回蓝/补给（机器人端策略，链数据里没有该字段）", Data: "policy（机器人端策略）",
		Py:    "__fight_end_heal / __use_heal_item / __start_shop",
		Tests: []string{"test/chainplan::TestBuildExpandsModuleSequenceInOrder"}},
	"wait": {Name: "等下一环", Desc: "等下一环任务推送", Data: "next[]",
		Py:    "等 S2C_ADD_TASK(90365)",
		Tests: []string{"test/chainplan::TestDanglingNextOnlyErrorsInsideMeaningfulRange"}},
}

// infraCards 中控侧两层（链数据 / 编排）。
var infraCards = []ModuleCard{
	{Key: "chainlib", Name: "链数据搬运", Layer: "data",
		Desc: "文件驱动加载 + 未声明字段/形状原样透传（中控不解释链数据）",
		Data: "data/chains/*.json（含 npcs/map_grids/dijkstra/ghost_maps…）",
		Py:   "等效参考实现 chainlib.build_chain / ghost_nav_payload",
		Tests: []string{"test/chainlib::TestBuildPreservesUnknownFieldsAndShapes",
			"test/api::TestStartAutoGhostCarriesNavPayload"}},
	{Key: "chainplan", Name: "链路组装与校验", Layer: "data",
		Desc: "把提示表展开成模块序列 + 结构校验（悬空 next / 未知执行器 / 缺参数）",
		Data: "task_order / task_hints / npcs",
		Py:   "quest_engine 执行器注册表（STEP_EXECUTOR_REGISTRY）",
		Tests: []string{"test/chainplan::TestModuleNamesMatchReferenceExecutors",
			"test/api::TestChainsEndpointIncludesModulePlan"}},
	{Key: "orchestration", Name: "编排（意图/恢复/启动）", Layer: "orchestration",
		Desc: "该跑哪条链（等级/chain_done 判据）+ 掉线恢复补发 + 启动按意图自动分配（抓鬼带导航载荷）",
		Data: "等级 / chain_done / 意图表 / 链数据",
		Py:   "state.INTENTS + intent_restore.py + auto_onboard.py",
		Tests: []string{"test/intent::TestDecideGhostAtThreshold",
			"test/intent::TestDecideGhostWhenChainDone",
			"test/recover::TestGhostRestoreCarriesPayload",
			"test/api::TestStartAutoGhostCarriesNavPayload",
			"test/api::TestStartGhostMissingNavFailsLoudly"}},
}

// ModuleCards 模块注册表（动作层顺序严格跟随 chainplan.ModuleNames()）。
func ModuleCards() []ModuleCard {
	out := make([]ModuleCard, 0, len(actionCards)+len(infraCards))
	for _, m := range chainplan.ModuleNames() {
		card, ok := actionCards[string(m)]
		if !ok {
			// 理论上不会发生（有单测钉住）：漏登记也要能看见，而不是静默少一张卡
			card = ModuleCard{Name: string(m), Desc: "（未登记：请在 internal/api/modules.go 补卡片）"}
		}
		card.Key = string(m)
		card.Layer = "actions"
		out = append(out, card)
	}
	return append(out, infraCards...)
}

// handleModules 模块地图（只读）：注册表 + 最近一次测试结果。
func (a *API) handleModules(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"ok":      true,
		"layers":  moduleLayers,
		"modules": ModuleCards(),
		"count":   len(ModuleCards()),
		"report":  nil,
	}
	if rep, path, err := a.lastTestReport(); err != nil {
		resp["report_error"] = err.Error()
	} else if rep != nil {
		resp["report"] = rep
		resp["report_path"] = filepath.ToSlash(path)
	}
	writeJSON(w, http.StatusOK, resp)
}

// lastTestReport 读 <DataDir>/test_report.json（tools/test_report 的产物）；不存在返回 (nil, path, nil)。
func (a *API) lastTestReport() (map[string]any, string, error) {
	path := filepath.Join(a.Cfg.DataDir, "test_report.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, path, nil // 还没跑过报告：页面显示"—"，不是错误
		}
		return nil, path, err
	}
	var rep map[string]any
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, path, err
	}
	return rep, path, nil
}
