// 账号可用性验证接口测试（POST /api/accounts/verify）：
// 直连游戏服跑 102 登录探测 → 结果写回账号池 → 面板可按区检索到 usable/verified/角色。
package api_test

import (
	"net/http"
	"strings"
	"testing"

	"zyctrlcenter/internal/gameproto"
	"zyctrlcenter/test/testsupport"
)

// fakeGame 假游戏服：ack 决定 300 的 retcode（按账号）；retcode=0 时回一个角色。
// gate 非空时，slowAccount 的登录会被卡住（用于测试"停止批量任务"）。
type fakeGame struct {
	// ack 账号 → (retcode, roleName, level)
	ack map[string]struct {
		code  int32
		role  string
		level int16
	}
	gate        <-chan struct{}
	slowAccount string
}

func (f fakeGame) handler() func(c *testsupport.GameConn, msgID int32, body []byte) [][]byte {
	return func(c *testsupport.GameConn, msgID int32, body []byte) [][]byte {
		switch msgID {
		case gameproto.MsgConnect:
			return [][]byte{
				testsupport.FrameBytes(gameproto.MsgConnectQuery, nil, nil, gameproto.UTF8),
				testsupport.FrameBytes(gameproto.MsgConnectBack, []any{4}, []any{int64(200)}, gameproto.UTF8),
			}
		case gameproto.MsgConnectBack:
			return [][]byte{testsupport.FrameBytes(gameproto.MsgVerBack, []any{4, ""}, []any{int64(0), ""}, gameproto.UTF8)}
		case gameproto.MsgLogin:
			vals, _, _ := gameproto.Unpack([]any{"", ""}, body, 0, gameproto.UTF8)
			acc, _ := vals[0].(string)
			if f.gate != nil && acc == f.slowAccount {
				<-f.gate // 卡住这条登录（测"停止任务"）
			}
			rule, ok := f.ack[acc]
			if !ok {
				rule.code = gameproto.LoginInvalidAccount
			}
			return [][]byte{testsupport.FrameBytes(gameproto.MsgAckAccount, []any{4, ""},
				[]any{int64(rule.code), ""}, gameproto.UTF8)}
		case gameproto.MsgQueryRole:
			// 从连接上无法得知账号，这里回"第一个请求账号"的角色由用例保证只有一个成功账号
			for _, rule := range f.ack {
				if rule.code == gameproto.LoginOK {
					raw, err := gameproto.BuildRoleFrame([]gameproto.Role{
						{RoleID: 1, RoleIndex: 1, Name: rule.role, Level: rule.level},
					}, gameproto.UTF8)
					if err != nil {
						return nil
					}
					return [][]byte{raw}
				}
			}
			raw, _ := gameproto.BuildRoleFrame(nil, gameproto.UTF8)
			return [][]byte{raw}
		}
		return nil
	}
}

func TestAccountsVerifyWritesBackToPoolAndIsSearchable(t *testing.T) {
	env := newTestEnv(t, "")
	gs := testsupport.StartGameServer(t, fakeGame{ack: map[string]struct {
		code  int32
		role  string
		level int16
	}{
		"robot0001000@xy3.com": {code: gameproto.LoginOK, role: "测试甲", level: 21},
		"robot0001001@xy3.com": {code: gameproto.LoginInvalidAccount},
	}}.handler())

	code, body := postJSON(t, env.srv.URL+"/api/accounts/verify", map[string]any{
		"accounts": []string{"robot0001000@xy3.com", "robot0001001@xy3.com"},
		"zone":     gs.Addr,
	}, nil)
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("验证接口失败: %d %v", code, body)
	}
	if body["game_addr"] != gs.Addr {
		t.Fatalf("game_addr=%v 期望 %s", body["game_addr"], gs.Addr)
	}
	if body["coding"] != gameproto.UTF8 {
		t.Fatalf("未配区时应按 UTF-8：%v", body["coding"])
	}

	sum, _ := body["summary"].(map[string]any)
	if sum["usable"] != float64(1) || sum["not_exists"] != float64(1) {
		t.Fatalf("汇总不符: %v", sum)
	}
	results, _ := body["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("结果数 %d", len(results))
	}
	first, _ := results[0].(map[string]any)
	if first["usable"] != true || first["role_name"] != "测试甲" || first["level"] != float64(21) {
		t.Fatalf("成功账号结果不符: %v", first)
	}
	second, _ := results[1].(map[string]any)
	if second["usable"] != false || second["exists"] != false {
		t.Fatalf("不存在账号结果不符: %v", second)
	}
	if body["pool_updated"] != float64(2) {
		t.Fatalf("结果应写回账号池: %v", body["pool_updated"])
	}

	// 池里能按区检索到验证结果（面板"账号池"页的数据源）
	list := getJSON(t, env.srv.URL+"/api/accounts?zone="+gs.Addr+"&keyword=robot0001000")
	rows, _ := list["accounts"].([]any)
	if len(rows) != 1 {
		t.Fatalf("检索应命中 1 行，实际 %d", len(rows))
	}
	row, _ := rows[0].(map[string]any)
	if row["usable"] != true || row["verified"] != true {
		t.Fatalf("池里应标记可用+已验证: %v", row)
	}
	if msg, _ := row["verify_msg"].(string); !strings.Contains(msg, "测试甲") {
		t.Fatalf("验证信息应含角色名: %v", row["verify_msg"])
	}
	if row["level"] != float64(21) || row["role_name"] != "测试甲" {
		t.Fatalf("等级/角色名应写回池: %v", row)
	}

	// 只看可用的检索
	only := getJSON(t, env.srv.URL+"/api/accounts?zone="+gs.Addr+"&usable=1")
	onlyRows, _ := only["accounts"].([]any)
	if len(onlyRows) != 1 {
		t.Fatalf("usable=1 应只剩 1 行，实际 %d", len(onlyRows))
	}
}

// 网络失败（连不上）不能把"可用"结论改坏：只记"验证失败"，usable 保持原样。
func TestAccountsVerifyNetworkErrorDoesNotFlipUsable(t *testing.T) {
	env := newTestEnv(t, "")
	gs := testsupport.StartGameServer(t, fakeGame{ack: map[string]struct {
		code  int32
		role  string
		level int16
	}{
		"robot0001000@xy3.com": {code: gameproto.LoginOK, role: "测试甲", level: 21},
	}}.handler())

	acc := "robot0001000@xy3.com"
	if _, body := postJSON(t, env.srv.URL+"/api/accounts/verify", map[string]any{
		"accounts": []string{acc}, "zone": gs.Addr,
	}, nil); body["ok"] != true {
		t.Fatalf("首次验证应成功: %v", body)
	}

	addr := gs.Addr
	gs.Close() // 关掉游戏服 → 后续连线必失败

	_, body := postJSON(t, env.srv.URL+"/api/accounts/verify", map[string]any{
		"accounts": []string{acc}, "zone": addr, "timeout_sec": 1,
	}, nil)
	results, _ := body["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("结果数 %d", len(results))
	}
	res, _ := results[0].(map[string]any)
	if errStr, _ := res["err"].(string); errStr == "" {
		t.Fatalf("连不上应记 Err: %v", res)
	}
	if res["usable"] != false {
		t.Fatalf("本次探测本身不得判为可用: %v", res)
	}

	// 池里的"可用"结论必须保持（不能被一次网络抖动改坏）
	list := getJSON(t, env.srv.URL+"/api/accounts?zone="+addr+"&keyword="+acc)
	rows, _ := list["accounts"].([]any)
	if len(rows) != 1 {
		t.Fatalf("应有 1 行: %v", list["accounts"])
	}
	row, _ := rows[0].(map[string]any)
	if row["usable"] != true {
		t.Fatalf("网络失败不应改坏 usable: %v", row)
	}
	if msg, _ := row["verify_msg"].(string); !strings.Contains(msg, "验证失败") {
		t.Fatalf("应记录验证失败原因: %v", row["verify_msg"])
	}
}

func TestAccountsVerifyNeedsTokenWhenConfigured(t *testing.T) {
	env := newTestEnv(t, "secret-token")
	body := map[string]any{"accounts": []string{"robot0001000@xy3.com"}, "zone": "127.0.0.1:1"}

	if code, _ := postJSON(t, env.srv.URL+"/api/accounts/verify", body, nil); code != http.StatusUnauthorized {
		t.Fatalf("配了 token 时未带 token 应 401，实际 %d", code)
	}
	hdr := map[string]string{"X-API-Token": "secret-token"}
	if code, resp := postJSON(t, env.srv.URL+"/api/accounts/verify", body, hdr); code != http.StatusOK || resp["ok"] != true {
		t.Fatalf("带 token 应放行: %d %v", code, resp)
	}
}
