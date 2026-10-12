// 游荡下发（POST /api/random_walk）扩展：随机图（mapid="random" / mode=random 别名）+ maps 白名单。
//
// 契约（机器人端 random_walk.py:resolve_roam_target，2026-09-22）：
//   - mapid 数字 = 指定图；"random" = 机器人端从"链数据里有网格的图"里随机挑一张（每号不同）；
//   - maps 白名单（可选）：随机图时只从白名单里挑（如孵化图 6/17/34/40）；指定图时必须在内；
//   - 参数层/校验层就地拒绝，不静默改图；链载荷缺网格/缺路由一条命令都不发。
package api_test

import (
	"strings"
	"testing"
	"time"

	"zyctrlcenter/test/testsupport"
)

// 随机图 + 白名单：命令下发 mapid="random"、白名单透传、档位/限时照传，链载荷带网格与路由。
func TestRandomWalkRandomMapWithWhitelist(t *testing.T) {
	env := newTestEnv(t, "")
	installWalkChain(t, env) // 夹具网格：图 6 / 图 10
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	acc := "rw_rand@xy3.com"
	feedRobot(t, env, acc, nil)

	_, body := postJSON(t, env.srv.URL+"/api/random_walk", map[string]any{
		"accounts": []string{acc}, "mapid": "random", "maps": []any{6, 10},
		"mode": "dense", "minutes": 30}, nil)
	if body["ok"] != true || body["sent"] != float64(1) || body["random"] != true {
		t.Fatalf("随机图下发应成功且 random=true: %v", body)
	}
	if got := asSlice(body["maps"]); len(got) != 2 {
		t.Fatalf("响应应回带白名单: %v", body["maps"])
	}

	cmd := rb.ReadCmd(t, 2*time.Second)
	if toStrAny(cmd["cmd"]) != "random_walk" {
		t.Fatalf("命令应为 random_walk: %v", cmd)
	}
	if toStrAny(cmd["mapid"]) != "random" {
		t.Fatalf("随机图命令的 mapid 应为字符串 random（机器人端据此抽签）: %v", cmd["mapid"])
	}
	if cmd["mode"] != "dense" {
		t.Fatalf("档位应透传: %v", cmd["mode"])
	}
	if cmd["minutes"] != float64(30) {
		t.Fatalf("限时应透传（机器人端到点自动停）: %v", cmd["minutes"])
	}
	got := asSlice(cmd["maps"])
	if len(got) != 2 || got[0] != float64(6) || got[1] != float64(10) {
		t.Fatalf("白名单应原样透传: %v", cmd["maps"])
	}
	chain, _ := cmd["chain"].(map[string]any)
	if chain == nil {
		t.Fatalf("必须带链载荷（机器人端只认命令里的链数据）: %v", cmd)
	}
	grids, _ := chain["map_grids"].(map[string]any)
	if _, ok := grids["6"]; !ok {
		t.Fatalf("链载荷里必须有图 6 的寻路网格: %v", grids)
	}
	if _, ok := grids["10"]; !ok {
		t.Fatalf("链载荷里必须有图 10 的寻路网格: %v", grids)
	}
	if dj, _ := chain["dijkstra"].(map[string]any); len(dj) == 0 {
		t.Fatalf("链载荷里必须有跨图路由（dijkstra）: %v", chain["dijkstra"])
	}
}

// mode=random 等价别名：与 mapid="random" 同义（档位改走 profile 字段）。
func TestRandomWalkModeRandomAlias(t *testing.T) {
	env := newTestEnv(t, "")
	installWalkChain(t, env)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	acc := "rw_alias@xy3.com"
	feedRobot(t, env, acc, nil)

	_, body := postJSON(t, env.srv.URL+"/api/random_walk", map[string]any{
		"accounts": []string{acc}, "mode": "random", "maps": "6", "profile": "dense"}, nil)
	if body["ok"] != true || body["random"] != true {
		t.Fatalf("mode=random 应等价于随机图: %v", body)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if toStrAny(cmd["mapid"]) != "random" {
		t.Fatalf("命令 mapid 应为 random: %v", cmd["mapid"])
	}
	if cmd["mode"] != "dense" {
		t.Fatalf("别名写法下档位应走 profile 字段: %v", cmd["mode"])
	}
	if got := asSlice(cmd["maps"]); len(got) != 1 || got[0] != float64(6) {
		t.Fatalf("字符串白名单应被解析: %v", cmd["maps"])
	}
}

// 白名单里混入"没有网格的图"：只要有一张可用就放行（抽签由机器人端在可用图里做）。
// 2026-10-12 起 34 通天河已入游荡排除表（连通域残缺）→ 本用例的"无网格"样本改用 40；
// 排除表的剔除/拒绝行为见 TestRandomWalkExcludeListedMapsInWhitelist。
func TestRandomWalkRandomWhitelistPartiallyUsable(t *testing.T) {
	env := newTestEnv(t, "")
	installWalkChain(t, env)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	acc := "rw_part@xy3.com"
	feedRobot(t, env, acc, nil)

	_, body := postJSON(t, env.srv.URL+"/api/random_walk", map[string]any{
		"accounts": []string{acc}, "mapid": "random", "maps": []any{40, 6}}, nil)
	if body["ok"] != true {
		t.Fatalf("白名单里有一张可用图就应放行: %v", body)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if got := asSlice(cmd["maps"]); len(got) != 2 {
		t.Fatalf("白名单应原样透传（可用性与抽签由机器人端处理）: %v", cmd["maps"])
	}
}

// 排除表回归（2026-10-12 34 通天河入表）：白名单部分含排除图 → 剔除保留其余；
// 全含 → 明确拒绝（不静默降级为"全图随机"）且零命令。
func TestRandomWalkExcludeListedMapsInWhitelist(t *testing.T) {
	env := newTestEnv(t, "")
	installWalkChain(t, env)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	acc := "rw_excl@xy3.com"
	feedRobot(t, env, acc, nil)

	// ① [34, 6] → 34 剔除，剩 [6]（响应与命令一致）。
	_, body := postJSON(t, env.srv.URL+"/api/random_walk", map[string]any{
		"accounts": []string{acc}, "mapid": "random", "maps": []any{34, 6}}, nil)
	if body["ok"] != true {
		t.Fatalf("部分含排除图应放行（剔除后仍有可用图）: %v", body)
	}
	if got := asSlice(body["maps"]); len(got) != 1 || got[0] != float64(6) {
		t.Fatalf("响应白名单应剔除 34 只剩 [6]: %v", body["maps"])
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if got := asSlice(cmd["maps"]); len(got) != 1 || got[0] != float64(6) {
		t.Fatalf("命令白名单应剔除 34 只剩 [6]: %v", cmd["maps"])
	}

	// ② [34] → 全部被剔除 → 明确拒绝，零命令。
	acc2 := "rw_excl2@xy3.com"
	feedRobot(t, env, acc2, nil)
	_, body2 := postJSON(t, env.srv.URL+"/api/random_walk", map[string]any{
		"accounts": []string{acc2}, "mapid": "random", "maps": []any{34}}, nil)
	if body2["ok"] != false {
		t.Fatalf("白名单全为排除图应拒绝: %v", body2)
	}
	if !strings.Contains(toStrAny(body2["msg"]), "排除表") {
		t.Fatalf("拒绝原因应说明排除表（实际 %v）", body2["msg"])
	}
	if cmd := rb.TryReadCmd(300 * time.Millisecond); cmd != nil {
		t.Fatalf("被拒的请求不该下发命令: %v", cmd)
	}
}

// 非法参数：明确拒绝 + 一条命令都不发（不静默改图/不静默降级）。
func TestRandomWalkRejectsBadMapsAndRandom(t *testing.T) {
	cases := []struct {
		name        string
		body        map[string]any
		wantMsgPart string
	}{
		{"白名单里没有有网格的图", map[string]any{"mapid": "random", "maps": []any{40}}, "网格"},
		{"白名单里的图都在游荡排除表内", map[string]any{"mapid": "random", "maps": []any{34}}, "排除"},
		{"白名单项非法", map[string]any{"mapid": "random", "maps": []any{"x"}}, "非法"},
		{"白名单为空数组", map[string]any{"mapid": "random", "maps": []any{}}, "空"},
		{"白名单类型不对", map[string]any{"mapid": "random", "maps": map[string]any{"a": 1}}, "类型"},
		{"指定图不在白名单", map[string]any{"mapid": 6, "maps": []any{10}}, "白名单"},
		{"mapid 与 mode=random 冲突", map[string]any{"mapid": 6, "mode": "random"}, "冲突"},
		{"缺 mapid", map[string]any{"mode": "dense"}, "mapid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, "")
			installWalkChain(t, env)
			rb := testsupport.ConnectFakeRobot(t, env.ctrl)
			defer rb.Close()
			acc := "rw_badmap@xy3.com"
			feedRobot(t, env, acc, nil)

			req := map[string]any{"accounts": []string{acc}}
			for k, v := range tc.body {
				req[k] = v
			}
			_, body := postJSON(t, env.srv.URL+"/api/random_walk", req, nil)
			if body["ok"] != false {
				t.Fatalf("非法参数必须拒绝: %v", body)
			}
			if !strings.Contains(toStrAny(body["msg"]), tc.wantMsgPart) {
				t.Fatalf("报错应说明原因（含 %q）: %v", tc.wantMsgPart, body["msg"])
			}
			if cmd := rb.TryReadCmd(200 * time.Millisecond); cmd != nil {
				t.Fatalf("不该下发任何命令: %v", cmd)
			}
		})
	}
}
