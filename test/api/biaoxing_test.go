// 镖行天下首期（2026-09-30，用户已批）：Go 中控侧契约测试。
//   - Kind 枚举/默认配置（默认关、target=0 → 不参与自动派发）；
//   - 手动直发 /api/daily/start（载荷与自动同构、不走台账、声明缺失硬失败）；
//   - 生产链数据 data/chains/biaoxing_nav.json 结构校验（12 变体 + 630/651 备点覆盖）
//   - 坏版灵敏度（校验器对残缺拷贝必须报问题）。
package api_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/chainlib"
	"zyctrlcenter/internal/config"
	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/test/testsupport"
)

func TestBiaoxingKindAndDefaults(t *testing.T) {
	if !autotask.KindBiaoxing.Valid() || autotask.KindBiaoxing.Label() != "镖行天下" {
		t.Fatalf("Kind 注册不符: valid=%v label=%q", autotask.KindBiaoxing.Valid(), autotask.KindBiaoxing.Label())
	}
	def := config.Default()
	if def.BiaoxingEnabled {
		t.Fatal("镖行天下默认必须关（首期只手动试点，不参与自动派发）")
	}
	if def.BiaoxingChainID != "biaoxing_nav" || def.BiaoxingKey != "share_daily_镖行天下" ||
		def.BiaoxingDailyLimit != 40 || def.BiaoxingMinLevel != 40 {
		t.Fatalf("默认配置不符: chain=%q key=%q limit=%d min=%d",
			def.BiaoxingChainID, def.BiaoxingKey, def.BiaoxingDailyLimit, def.BiaoxingMinLevel)
	}
}

func TestBiaoxingManualStart(t *testing.T) {
	env := newTestEnv(t, "")
	testsupport.InstallBiaoxingNav(t, env.cfg.ChainDir)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	acc := "bx_1@xy3.com"
	feedRobot(t, env, acc, nil)

	_, body := postJSON(t, env.srv.URL+"/api/daily/start", map[string]any{
		"kind": "biaoxing", "accounts": []string{acc}}, nil)
	if body["ok"] != true || body["sent"] != true || body["command"] != "share_daily_start" {
		t.Fatalf("手动直发应成功: %v", body)
	}
	if body["share_key"] != "share_daily_镖行天下" || body["chain_id"] != "biaoxing_nav" ||
		body["daily_limit"] != float64(40) {
		t.Fatalf("载荷口径应同自动派发（key/chain/日限）: %v", body)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "share_daily_start" || cmd["share_key"] != "share_daily_镖行天下" ||
		cmd["chain_id"] != "biaoxing_nav" {
		t.Fatalf("命令载荷不符: %v", cmd)
	}
	if a := asSlice(cmd["accounts"]); len(a) != 1 || a[0] != acc {
		t.Fatalf("应带目标号: %v", cmd["accounts"])
	}
	// 630/651 网格应随专属声明下发（基座缺这两图网格 → 专属补差量；NPC 本体在 630/651）
	chain, _ := cmd["chain"].(map[string]any)
	grids, _ := chain["map_grids"].(map[string]any)
	g630, _ := grids["630"].(map[string]any)
	if g630 == nil || g630["w"] != float64(4) || g630["h"] != float64(3) {
		t.Fatalf("应带 630 网格（mini w4 h3）: %v", grids["630"])
	}
	if rows, _ := g630["rows"].([]any); len(rows) != 3 {
		t.Fatalf("630 网格行数应与 h 一致: %v", g630["rows"])
	}
	if g651, _ := grids["651"].(map[string]any); g651 == nil {
		t.Fatalf("应带 651 网格: %v", grids["651"])
	}
	if to, _ := chain["task_order"].([]any); len(to) != 3 { // mini 夹具 3 条
		t.Fatalf("task_order 应随专属声明下发（mini=3）: %v", chain["task_order"])
	}

	// 不写台账（试点语义）：连续两次都真实下发
	_, b1 := postJSON(t, env.srv.URL+"/api/daily/start", map[string]any{"kind": "biaoxing", "accounts": []string{acc}}, nil)
	rb.ReadCmd(t, 2*time.Second)
	_, b2 := postJSON(t, env.srv.URL+"/api/daily/start", map[string]any{"kind": "biaoxing", "accounts": []string{acc}}, nil)
	if b1["sent"] != true || b2["sent"] != true {
		t.Fatalf("手动直发不应写自动在途台账（两次都应发出）: %v / %v", b1, b2)
	}
	rb.ReadCmd(t, 2*time.Second)

	// 非分享日常家族 kind：拒绝且零命令
	if _, bad := postJSON(t, env.srv.URL+"/api/daily/start", map[string]any{
		"kind": "ghost", "accounts": []string{acc}}, nil); bad["ok"] != false {
		t.Fatalf("非日常 kind 应拒绝: %v", bad)
	}
	if c := rb.TryReadCmd(300 * time.Millisecond); c != nil {
		t.Fatalf("非法请求不该发命令: %v", c)
	}
}

// 声明文件缺失（未装夹具）→ 硬失败、一条不发（与自动派发同口径）。
func TestBiaoxingManualStartMissingChain(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	acc := "bx_2@xy3.com"
	feedRobot(t, env, acc, nil)
	_, body := postJSON(t, env.srv.URL+"/api/daily/start", map[string]any{
		"kind": "biaoxing", "accounts": []string{acc}}, nil)
	if body["ok"] != false || !strings.Contains(toStrAny(body["msg"]), "链数据不可用") {
		t.Fatalf("缺声明应硬失败: %v", body)
	}
	if c := rb.TryReadCmd(300 * time.Millisecond); c != nil {
		t.Fatalf("硬失败不该发命令: %v", c)
	}
}

// validateBiaoxingChain 校验镖行天下生产链数据：12 主变体全列 + 抵押品入口 + thrower 董江 + 630/651 备点覆盖。
// 返回问题清单（空 = 通过）。放在测试里当"坏版灵敏度"的判据。
func validateBiaoxingChain(c *chainlib.Chain) []string {
	var problems []string
	if c.ChainID != "biaoxing_nav" {
		problems = append(problems, "chain_id 应为 biaoxing_nav: "+c.ChainID)
	}
	if c.StartTask != 2001101 {
		problems = append(problems, "start_task 应为 2001101")
	}
	want := []int{2001101, 2001102, 2001103, 2001104, 2001105, 2001106, 2001107,
		2001108, 2001109, 2001110, 2001111, 2001112, 2001113}
	got := map[int]bool{}
	for _, raw := range c.TaskOrder {
		var it struct {
			TaskIndex  int    `json:"task_index"`
			ThrowerNPC string `json:"thrower_npc"`
			CatcherNPC string `json:"catcher_npc"`
		}
		if err := json.Unmarshal(raw, &it); err != nil {
			problems = append(problems, "task_order 条目解析失败: "+err.Error())
			continue
		}
		got[it.TaskIndex] = true
		if it.ThrowerNPC != "13063" {
			problems = append(problems, "thrower 应全为董江 13063 @ "+itoaTest(it.TaskIndex))
		}
	}
	for _, w := range want {
		if !got[w] {
			problems = append(problems, "缺变体 "+itoaTest(w))
		}
	}
	// 630/651 网格必须在本文件（基座缺这两图；专属补差量，assembleShareDaily 按 key 合并）
	checkGrid := func(mapID string) {
		raw, ok := c.MapGrids[mapID]
		if !ok {
			problems = append(problems, "缺 map_grids["+mapID+"]（630/651 本体无网格会卡死）")
			return
		}
		var g struct {
			W    int      `json:"w"`
			H    int      `json:"h"`
			Rows []string `json:"rows"`
		}
		if err := json.Unmarshal(raw, &g); err != nil || g.W <= 0 || g.H <= 0 || len(g.Rows) != g.H {
			problems = append(problems, "map_grids["+mapID+"] 形状不符（w/h/rows）")
			return
		}
		blocked, walk := 0, 0
		for _, r := range g.Rows {
			if len(r) != g.W {
				problems = append(problems, "map_grids["+mapID+"] 行宽与 w 不符")
				return
			}
			for i := 0; i < len(r); i++ {
				if r[i] == '1' {
					blocked++
				} else {
					walk++
				}
			}
		}
		if blocked == 0 || walk == 0 {
			problems = append(problems, "map_grids["+mapID+"] 全阻挡或全可走（疑似导出错误）")
		}
	}
	checkGrid("630")
	checkGrid("651")
	return problems
}

func itoaTest(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// 生产链数据（data/chains/biaoxing_nav.json）结构校验 + 坏版灵敏度。
func TestBiaoxingChainDataProduction(t *testing.T) {
	c, err := chainlib.Build("biaoxing_nav", "../../data/chains")
	if err != nil {
		t.Fatalf("生产链数据应可加载: %v", err)
	}
	if ps := validateBiaoxingChain(c); len(ps) != 0 {
		t.Fatalf("生产链数据校验失败:\n%s", strings.Join(ps, "\n"))
	}

	// 坏版灵敏度：删掉一个主变体 + 去掉一个备点覆盖 → 校验器必须报问题
	raw, err := os.ReadFile("../../data/chains/biaoxing_nav.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var kept []any
	for _, it := range doc["task_order"].([]any) {
		if int(it.(map[string]any)["task_index"].(float64)) == 2001105 {
			continue // 故意删掉
		}
		kept = append(kept, it)
	}
	doc["task_order"] = kept
	delete(doc["map_grids"].(map[string]any), "651")
	mut, _ := json.Marshal(doc)
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/biaoxing_nav.json", mut, 0o644); err != nil {
		t.Fatal(err)
	}
	mc, err := chainlib.Build("biaoxing_nav", dir)
	if err != nil {
		t.Fatalf("残缺拷贝应仍可解析（校验靠 validator）: %v", err)
	}
	ps := validateBiaoxingChain(mc)
	if len(ps) < 2 {
		t.Fatalf("坏版应被校验器逮住（缺变体+缺 651 网格），实际 %v", ps)
	}
}
