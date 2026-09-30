// 45 瑶池回廊过滤（2026-09-30 用户批准；Go 侧与机器人端 exclude+world+wild 三处同步）：
// 跳转点连通域残缺，部分域进去出不来（NO_LEGAL_ROUTE）→ 显式选图拒绝 + 随机白名单剔除（双闸）。
package api_test

import (
	"strings"
	"testing"
	"time"

	"zyctrlcenter/test/testsupport"
)

func TestRoamExcludeMap45(t *testing.T) {
	env := newTestEnv(t, "")
	installWalkChain(t, env) // 夹具网格：图 6 / 图 10
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	// ① 显式 mapid=45 → 直接拒绝（不动号上任务），且零命令
	feedRobot(t, env, "rw45_a@xy3.com", nil)
	_, body := postJSON(t, env.srv.URL+"/api/random_walk", map[string]any{
		"accounts": []string{"rw45_a@xy3.com"}, "mapid": 45}, nil)
	if body["ok"] != false || !strings.Contains(toStrAny(body["msg"]), "不在游荡范围") {
		t.Fatalf("45 应被明确拒绝，实际 %v", body)
	}
	if cmd := rb.TryReadCmd(300 * time.Millisecond); cmd != nil {
		t.Fatalf("被拒的选图不该下发命令: %v", cmd)
	}

	// ② 随机图白名单 [6,45] → 45 被剔除，只留 6（响应与命令载荷一致）
	feedRobot(t, env, "rw45_b@xy3.com", nil)
	_, body2 := postJSON(t, env.srv.URL+"/api/random_walk", map[string]any{
		"accounts": []string{"rw45_b@xy3.com"}, "mapid": "random", "maps": []any{6, 45}}, nil)
	if body2["ok"] != true {
		t.Fatalf("随机游荡应下发成功: %v", body2)
	}
	got := asSlice(body2["maps"])
	if len(got) != 1 || got[0] != float64(6) {
		t.Fatalf("响应白名单应剔除 45 只剩 [6]，实际 %v", body2["maps"])
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	cmdMaps := asSlice(cmd["maps"])
	if len(cmdMaps) != 1 || cmdMaps[0] != float64(6) {
		t.Fatalf("命令白名单应剔除 45 只剩 [6]，实际 %v", cmd["maps"])
	}
	if toStrAny(cmd["mapid"]) != "random" {
		t.Fatalf("mapid 应为 random: %v", cmd["mapid"])
	}
}
