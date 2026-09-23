// 链数据客观闸门：不引用任何一方（含参考实现）的结论，只用**游戏服本机的客观数据**校验链数据。
//
// 依据：`<游戏服>/config/map_file/blockfile`（寻路网格，0=可走 1=阻挡，格子=坐标/16）。
// 这些断言对"我们生成"和"参考实现生成"的链数据一视同仁；链数据一变就会报警。
//
// 跳过条件：本机没有游戏服配置目录（CI/别的机器）→ 跳过（不误报）。
// 环境变量 ZY_GAME_CONFIG 可指定游戏服配置目录（默认本机 2d-xiyou-server/config）。
package chaindata_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"zyctrlcenter/internal/api"
	"zyctrlcenter/internal/chainlib"
	"zyctrlcenter/internal/chainplan"
	"zyctrlcenter/internal/config"
	"zyctrlcenter/internal/maplib"
	"zyctrlcenter/test/testsupport"
)

const (
	// DefaultGameConfig 本机游戏服配置目录（含 map.csv 与 map_file/blockfile）
	DefaultGameConfig = `F:\ZyBin\xm\2d-xiyou-server\config`
	// GridCell 客户端坐标口径：格子 = 坐标 / 16
	GridCell = 16
)

// dynamicNpcBaseline 「链数据里没有坐标」的 NPC 基线（= 等动态 NPC 推送 90351 的那类）。
// 链数据变化导致不一致时测试失败——**这是故意的**：新增/减少都要人复核一次。
var dynamicNpcBaseline = map[string][]string{
	"newbie_full": {"1", "13098", "17018", "18009", "18349"},
}

// knownBlockedLandings 已知"抓鬼落点落在阻挡格"的清单（"图:坐标"）。
//
// 2026-09-19 客观核查发现参考实现的落点算法（每图挑第一个 NPC 坐标）在图9 选到了阻挡格
// (481,604)；已用 tools/fixlanding（挑同图可走坐标 / BFS 最近可走格）修成 (787,780)，
// 因此这里**清空**。以后再出现（含新链数据）就会被这条断言抓到。
var knownBlockedLandings = map[string][]string{}

// knownBlockedNpcCounts 基线：**本链用到的图**里"坐标落在阻挡格"的 NPC 条数。
//
// 这些是**非任务 NPC**（任务引用的 NPC 由 TestTaskNpcPositionsAreWalkable 硬校验，必须可走）：
// 比如摊位/水面上的 NPC，机器人是"靠近点击"，站不到它身上也能点。所以登记基线而不是硬失败，
// 但**条数增加就报警**（新增的阻挡坐标要人看一眼）。
var knownBlockedNpcCounts = map[string]int{
	"newbie_full":  12,
	"zhongkui_nav": 12, // 钟馗抓鬼日常的导航数据（原 zhuaogui_nav，改名避免与支线捉鬼链混淆）
}

func chainsDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(testsupport.TestRoot(t), "..", "data", "chains")
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("链数据目录不存在（%s）：跳过", dir)
	}
	return dir
}

func gridReader(t *testing.T) *maplib.GridReader {
	t.Helper()
	dir := os.Getenv("ZY_GAME_CONFIG")
	if dir == "" {
		dir = DefaultGameConfig
	}
	r := maplib.NewGridReader(dir)
	if !r.Available() {
		t.Skipf("没有游戏服配置目录（%s，可用 ZY_GAME_CONFIG 指定）：跳过客观校验", dir)
	}
	return r
}

func loadChains(t *testing.T, dir string) map[string]*chainlib.Chain {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*chainlib.Chain{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, "_") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		c, err := chainlib.Build(id, dir)
		if err != nil {
			t.Fatalf("%s 加载失败: %v", name, err)
		}
		out[id] = c
	}
	if len(out) == 0 {
		t.Skip("链目录里没有真实链数据：跳过")
	}
	return out
}

type taskNode struct {
	TaskIndex  int    `json:"task_index"`
	CatcherNPC string `json:"catcher_npc"`
	ThrowerNPC string `json:"thrower_npc"`
}

type hintNode struct {
	Executor string `json:"executor"`
	FightNPC struct {
		NPCIndex any `json:"npc_index"`
	} `json:"fight_npc"`
	Shop struct {
		NPC any `json:"npc"`
	} `json:"shop"`
}

type npcRef struct {
	role string
	npc  string
}

// taskNpcs 任务链引用到的 NPC（catcher/thrower/前置战斗/商店），按 NPC 去重。
func taskNpcs(c *chainlib.Chain) map[string]npcRef {
	out := map[string]npcRef{}
	for _, raw := range c.TaskOrder {
		var o taskNode
		if json.Unmarshal(raw, &o) != nil {
			continue
		}
		if o.CatcherNPC != "" {
			out[o.CatcherNPC] = npcRef{"catcher", o.CatcherNPC}
		}
		if o.ThrowerNPC != "" && o.ThrowerNPC != o.CatcherNPC {
			out[o.ThrowerNPC] = npcRef{"thrower", o.ThrowerNPC}
		}
	}
	for _, raw := range c.TaskHints {
		var h hintNode
		if json.Unmarshal(raw, &h) != nil {
			continue
		}
		switch h.Executor {
		case "fight_npc":
			if s := anyToStr(h.FightNPC.NPCIndex); s != "" {
				out[s] = npcRef{"fight_npc", s}
			}
		case "shop_purchase":
			if s := anyToStr(h.Shop.NPC); s != "" {
				out[s] = npcRef{"shop_npc", s}
			}
		}
	}
	return out
}

func anyToStr(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		if t == 0 {
			return ""
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

// npcPositions npc_index → 坐标列表（兼容扁平 [map,x,y] 与嵌套 [[map,x,y],…]）。
func npcPositions(c *chainlib.Chain, npc string) [][3]int {
	raw, ok := c.NPCs[npc]
	if !ok {
		return nil
	}
	var flat []int
	if json.Unmarshal(raw, &flat) == nil && len(flat) == 3 {
		return [][3]int{{flat[0], flat[1], flat[2]}}
	}
	var nested [][]int
	if json.Unmarshal(raw, &nested) == nil {
		out := make([][3]int, 0, len(nested))
		for _, p := range nested {
			if len(p) == 3 {
				out = append(out, [3]int{p[0], p[1], p[2]})
			}
		}
		return out
	}
	return nil
}

// 5) npcs 全量抽查：**本链会用到的图**（map_grids 里有）里的 NPC 坐标必须可走
//    （机器人要导航到这些图去点 NPC）；其它图的只统计，供排查（靠近点击，不要求站得住）。
func TestAllNpcPositionsAudit(t *testing.T) {
	dir, gr := chainsDir(t), gridReader(t)
	for id, c := range loadChains(t, dir) {
		if len(c.NPCs) == 0 {
			continue // 声明型链（坐标在基座链里、发送时组装；如 shenbu_nav）→ 本文件无 NPC 可校
		}
		grids := gridKeys(c)
		var blockedInUsed, blockedElsewhere int
		samples := []string{}
		checked := 0
		for npc := range c.NPCs {
			for _, p := range npcPositions(c, npc) {
				g, err := gr.Grid(p[0])
				if err != nil || g == nil {
					continue // 该图没有 blockfile：跳过（不是所有图都有网格）
				}
				checked++
				pos := maplib.GridPosWith(p[1], p[2], GridCell)
				if !g.Blocked(pos[0], pos[1]) {
					continue
				}
				if grids[strconv.Itoa(p[0])] {
					blockedInUsed++
					if len(samples) < 8 {
						samples = append(samples, fmt.Sprintf("NPC%s 图%d(%d,%d)", npc, p[0], p[1], p[2]))
					}
				} else {
					blockedElsewhere++
				}
			}
		}
		want, ok := knownBlockedNpcCounts[id]
		if !ok {
			t.Fatalf("[%s] 没有基线：确认下面这些非任务 NPC 确实可以靠近点击后，把 %d 加进 knownBlockedNpcCounts\n  %s",
				id, blockedInUsed, strings.Join(samples, "\n  "))
		}
		if blockedInUsed > want {
			t.Errorf("[%s] 用到的图里阻挡坐标变多了（%d > 基线 %d）：**新增问题**，逐个确认：\n  %s",
				id, blockedInUsed, want, strings.Join(samples, "\n  "))
		}
		if blockedInUsed < want {
			t.Errorf("[%s] 阻挡坐标变少了（%d < 基线 %d）：请下调 knownBlockedNpcCounts", id, blockedInUsed, want)
		}
		t.Logf("[%s] NPC 全量抽查：检查 %d 条坐标；用到的图里阻挡 %d、其它图阻挡 %d（仅统计）",
			id, checked, blockedInUsed, blockedElsewhere)
	}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func gridKeys(c *chainlib.Chain) map[string]bool {
	out := map[string]bool{}
	for k := range c.MapGrids {
		out[k] = true
	}
	return out
}

// 1) 有坐标的任务 NPC：图必须有网格、坐标必须可走、图必须在 map_grids 里。
func TestTaskNpcPositionsAreWalkable(t *testing.T) {
	dir, gr := chainsDir(t), gridReader(t)
	for id, c := range loadChains(t, dir) {
		grids := gridKeys(c)
		var missingGrid, blocked, notInGrids []string
		refs := taskNpcs(c)
		for npc, ref := range refs {
			for _, p := range npcPositions(c, npc) {
				mapID := p[0]
				g, err := gr.Grid(mapID)
				if err != nil || g == nil {
					missingGrid = append(missingGrid, fmt.Sprintf("%s(%s) 图%d 没网格", npc, ref.role, mapID))
					continue
				}
				pos := maplib.GridPosWith(p[1], p[2], GridCell)
				if g.Blocked(pos[0], pos[1]) {
					blocked = append(blocked, fmt.Sprintf("%s(%s) 图%d 坐标(%d,%d) 落在阻挡格", npc, ref.role, mapID, p[1], p[2]))
				}
				if !grids[strconv.Itoa(mapID)] {
					notInGrids = append(notInGrids, fmt.Sprintf("%s(%s) 图%d 不在 map_grids", npc, ref.role, mapID))
				}
			}
		}
		sort.Strings(blocked)
		sort.Strings(missingGrid)
		sort.Strings(notInGrids)
		if len(blocked) > 0 {
			t.Errorf("[%s] NPC 坐标落在阻挡格（到不了/会穿墙），%d 处：\n  %s", id, len(blocked), strings.Join(blocked, "\n  "))
		}
		if len(missingGrid) > 0 {
			t.Errorf("[%s] NPC 所在图没有寻路网格，%d 处：\n  %s", id, len(missingGrid), strings.Join(missingGrid, "\n  "))
		}
		if len(notInGrids) > 0 {
			t.Errorf("[%s] NPC 所在图不在 map_grids（同图 A* 不可用），%d 处：\n  %s", id, len(notInGrids), strings.Join(notInGrids, "\n  "))
		}
		t.Logf("[%s] 任务 NPC 可达性：引用 %d 个，全部通过", id, len(refs))
	}
}

// 2) 没有坐标的 NPC 必须与基线一致（动态 NPC 白名单；链数据一变就报警让人复核）。
func TestDynamicNpcBaselineUnchanged(t *testing.T) {
	dir := chainsDir(t)
	for id, c := range loadChains(t, dir) {
		if len(c.TaskOrder) == 0 {
			continue // 导航数据（无任务节点）不适用
		}
		if len(c.NPCs) == 0 {
			continue // 声明型链（坐标由基座链提供、发送时组装；如 shenbu_nav）：
			// 本文件 npcs 为空，"任务 NPC 无坐标"不代表它是动态 NPC（坐标在基座里）
		}
		got := []string{}
		for npc := range taskNpcs(c) {
			if len(npcPositions(c, npc)) == 0 {
				got = append(got, npc)
			}
		}
		sort.Strings(got)
		want, ok := dynamicNpcBaseline[id]
		if !ok {
			t.Fatalf("[%s] 没有基线：把 %v 加进 dynamicNpcBaseline（先确认这些确实是动态 NPC）", id, got)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("[%s] 无坐标 NPC 与基线不一致（新增/减少都要复核，别静默放过）：\n  现在: %v\n  基线: %v", id, got, want)
		}
	}
}

// 3) 抓鬼落点必须可走、刷鬼图必须在 map_grids 里（这些字段是"未声明字段"，中控原样透传在 Extra）。
func TestGhostLandingPointsAreWalkable(t *testing.T) {
	dir, gr := chainsDir(t), gridReader(t)
	for id, c := range loadChains(t, dir) {
		var ghostMaps []int
		var ghostPos map[string][]int
		if raw, ok := c.Extra["ghost_maps"]; ok {
			_ = json.Unmarshal(raw, &ghostMaps)
		}
		if raw, ok := c.Extra["ghost_map_pos"]; ok {
			_ = json.Unmarshal(raw, &ghostPos)
		}
		if len(ghostMaps) == 0 && len(ghostPos) == 0 {
			continue
		}
		grids := gridKeys(c)
		var badLandings []string
		for m, p := range ghostPos {
			if len(p) < 2 {
				continue
			}
			mid, err := strconv.Atoi(m)
			if err != nil {
				continue
			}
			g, gerr := gr.Grid(mid)
			if gerr != nil || g == nil {
				t.Errorf("[%s] 抓鬼落点 图%s 没有寻路网格（到地方没法走）", id, m)
				continue
			}
			pos := maplib.GridPosWith(p[0], p[1], GridCell)
			if g.Blocked(pos[0], pos[1]) {
				key := fmt.Sprintf("%s:(%d,%d)", m, p[0], p[1])
				badLandings = append(badLandings, key)
			}
			if !grids[m] {
				t.Errorf("[%s] 抓鬼落点 图%s 不在 map_grids", id, m)
			}
		}
		for _, m := range ghostMaps {
			if !grids[strconv.Itoa(m)] {
				t.Errorf("[%s] 刷鬼图 %d 不在 map_grids（到刷鬼图没有 A* 网格）", id, m)
			}
		}
		sort.Strings(badLandings)
		want := knownBlockedLandings[id]
		for _, b := range badLandings {
			if !containsStr(want, b) {
				t.Errorf("[%s] 抓鬼落点 %s 落在阻挡格（筋斗云落地会卡）：**新增问题**，修生成逻辑或登记基线",
					id, b)
			}
		}
		for _, b := range want {
			if !containsStr(badLandings, b) {
				t.Errorf("[%s] 落点 %s 已不在阻挡格：请从 knownBlockedLandings 删掉这条", id, b)
			}
		}
		t.Logf("[%s] 刷鬼图 %v · 落点 %d 个（已知阻挡 %d）：可达性检查完成",
			id, ghostMaps, len(ghostPos), len(badLandings))
	}
}

// 4) dijkstra 每条路由两端都要能加载网格（跨图才不卡）；真实链数据还不能有组装错误。
func TestDijkstraEndpointsAndPlan(t *testing.T) {
	dir, gr := chainsDir(t), gridReader(t)
	for id, c := range loadChains(t, dir) {
		type oneRoute struct {
			FromMap   int `json:"from_map"`
			TargetMap int `json:"target_map"`
		}
		total, bad := 0, 0
		for _, raw := range c.Dijkstra {
			list := []oneRoute{}
			var single oneRoute
			if json.Unmarshal(raw, &single) == nil && (single.FromMap != 0 || single.TargetMap != 0) {
				list = append(list, single)
			} else {
				var many []oneRoute
				if json.Unmarshal(raw, &many) == nil {
					list = many
				}
			}
			for _, r := range list {
				total++
				for _, m := range []int{r.FromMap, r.TargetMap} {
					if m == 0 {
						continue
					}
					if g, err := gr.Grid(m); err != nil || g == nil {
						bad++
						t.Errorf("[%s] dijkstra 路由用了没有网格的图 %d（跨图会卡）", id, m)
					}
				}
			}
		}
		plan, err := chainplan.Build(c)
		if err != nil {
			t.Errorf("[%s] 真实链数据不该有组装错误: %v", id, err)
		}
		t.Logf("[%s] dijkstra %d 条路由（%d 端缺网格）· 组装任务 %d 个 · 模块 %v",
			id, total, bad, len(plan.Tasks), plan.Modules)
	}
}

// 抓鬼导航「地址」完备性（真实链数据；口径同参考实现 services/chain.py:ghost_nav_payload）：
// 中控侧载荷是**组装**出来的 —— 基座链（newbie_full）给 npcs/map_grids/dijkstra，
// 抓鬼专属文件（zhongkui_nav）给 ghost_maps/ghost_map_pos。这里用真实数据钉住"地址齐不齐"：
// 钟馗在 npcs 里、每张刷鬼图有寻路网格、每张刷鬼图有筋斗云落点。
// 缺任何一个，机器人的表现就是"原地不动 / 空点钟馗连点 15 次无对话"（生产踩过这个坑）。
func TestGhostNavAddressesAreComplete(t *testing.T) {
	dir := chainsDir(t)
	p := api.NewPayloads(&config.Config{ChainDir: dir}) // 空字段走默认：zhongkui_nav + newbie_full
	nav, err := p.Ghost()
	if err != nil {
		t.Fatalf("抓鬼导航载荷组装失败（真实链数据）: %v", err)
	}
	if _, ok := nav.NPCs["10146"]; !ok {
		t.Fatalf("npcs 里应有钟馗 10146（机器人靠它导航过去接任务）")
	}
	rawMaps, ok := nav.Extra["ghost_maps"]
	if !ok {
		t.Fatal("组装结果缺 ghost_maps（刷鬼图列表）")
	}
	var ghostMaps []int
	if err := json.Unmarshal(rawMaps, &ghostMaps); err != nil {
		t.Fatalf("ghost_maps 形状不对: %v", err)
	}
	landings := map[string][]int{}
	if raw, ok := nav.Extra["ghost_map_pos"]; ok {
		_ = json.Unmarshal(raw, &landings)
	}
	missingGrid, missingPos := []string{}, []string{}
	for _, m := range ghostMaps {
		key := strconv.Itoa(m)
		if _, ok := nav.MapGrids[key]; !ok {
			missingGrid = append(missingGrid, key)
		}
		if _, ok := landings[key]; !ok {
			missingPos = append(missingPos, key)
		}
	}
	if len(missingGrid) > 0 || len(missingPos) > 0 {
		t.Fatalf("刷鬼图地址不全：缺网格 %v、缺落点 %v（缺了机器人到不了刷鬼图）", missingGrid, missingPos)
	}
	t.Logf("抓鬼导航地址完备：钟馗在 npcs、刷鬼图 %v 均有网格（共 %d 张图）与落点", ghostMaps, len(nav.MapGrids))
}
