// 建号接口：随机密码 → 写入账号库 → 之后登录/验证用的就是这个库里的密码（没有统一密码）。
package api_test

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"zyctrlcenter/internal/gameproto"
	"zyctrlcenter/internal/services/accounts"
	"zyctrlcenter/test/testsupport"
)

// createGame 假游戏服：注册链路（106/104/700）+ 登录链路（102/1000）。
// existing 模拟"服务端上已经存在的账号"（用于测"先验证、再注册"分支）。
type createGame struct {
	mu       sync.Mutex
	loginMD5 map[string]string // 账号 → 登录时收到的 md5(密码)
	existing map[string]string // 账号 → 服务端上的明文密码（已存在）
	created  map[string]string // 本次会话里注册出来的账号 → 密码
	regErrID int32             // 非 0 时注册固定回这个 errid
	regCount int               // 收到过多少次 104（断言"已存在的号不再注册"）
}

func (g *createGame) handler() func(c *testsupport.GameConn, msgID int32, body []byte) [][]byte {
	return func(c *testsupport.GameConn, msgID int32, body []byte) [][]byte {
		switch msgID {
		case gameproto.MsgConnect:
			return [][]byte{
				testsupport.FrameBytes(gameproto.MsgConnectQuery, nil, nil, gameproto.UTF8),
				testsupport.FrameBytes(gameproto.MsgConnectBack, []any{4}, []any{int64(200)}, gameproto.UTF8),
			}
		case gameproto.MsgConnectBack:
			return [][]byte{testsupport.FrameBytes(gameproto.MsgVerBack, []any{4, ""}, []any{int64(0), ""}, gameproto.UTF8)}
		case gameproto.MsgGetRegCode:
			return [][]byte{testsupport.FrameBytes(gameproto.MsgRegCodeBack, []any{""}, []any{"动感高清"}, gameproto.UTF8)}
		case gameproto.MsgRegister:
			fields := readStr8(body)
			g.mu.Lock()
			g.regCount++
			acc, pwd := fields[1], fields[2]
			taken := false
			if _, ok := g.existing[acc]; ok {
				taken = true
			}
			if _, ok := g.created[acc]; ok {
				taken = true
			}
			if g.created == nil {
				g.created = map[string]string{}
			}
			if !taken && g.regErrID == 0 {
				g.created[acc] = pwd
			}
			g.mu.Unlock()

			code, dbRet := g.regErrID, int64(1)
			if code == 0 {
				if taken {
					code = gameproto.RegErrNameExist
				} else {
					dbRet, code = 0, 110
				}
			}
			return [][]byte{testsupport.FrameBytes(gameproto.MsgRegisterBack, []any{4, 4, 4},
				[]any{dbRet, int64(4), int64(code)}, gameproto.UTF8)}
		case gameproto.MsgLogin:
			vals, _, _ := gameproto.Unpack([]any{"", ""}, body, 0, gameproto.UTF8)
			acc, _ := vals[0].(string)
			md5pwd, _ := vals[1].(string)
			g.mu.Lock()
			if g.loginMD5 == nil {
				g.loginMD5 = map[string]string{}
			}
			g.loginMD5[acc] = md5pwd
			// 服务端判定：账号在 existing/created 里 → 比对密码；否则 100 不存在
			real, ok := g.existing[acc]
			if !ok {
				real, ok = g.created[acc]
			}
			sum := md5.Sum([]byte(real))
			g.mu.Unlock()
			code := int64(gameproto.LoginInvalidAccount)
			if ok {
				if hex.EncodeToString(sum[:]) == md5pwd {
					code = int64(gameproto.LoginOK)
				} else {
					code = int64(gameproto.LoginInvalidPassword)
				}
			}
			return [][]byte{testsupport.FrameBytes(gameproto.MsgAckAccount, []any{4, ""},
				[]any{code, ""}, gameproto.UTF8)}
		case gameproto.MsgQueryRole:
			raw, _ := gameproto.BuildRoleFrame([]gameproto.Role{{RoleID: 1, Name: "新建号", Level: 1}}, gameproto.UTF8)
			return [][]byte{raw}
		}
		return nil
	}
}

func TestAccountsCreateRandomPasswordThenLoginWithStoredPassword(t *testing.T) {
	env := newTestEnv(t, "")
	cg := &createGame{}
	gs := testsupport.StartGameServer(t, cg.handler())
	name := "robot0009001@xy3.com"

	code, body := postJSON(t, env.srv.URL+"/api/accounts/create", map[string]any{
		"accounts": []string{name}, "zone": gs.Addr, "password_len": 16, "interval_ms": 0,
	}, nil)
	if code != http.StatusOK || body["ok"] != true || body["created"] != float64(1) {
		t.Fatalf("建号应成功: %d %v", code, body)
	}
	results, _ := body["results"].([]any)
	res0, _ := results[0].(map[string]any)
	pwd, _ := res0["password"].(string)
	if len(pwd) != 16 {
		t.Fatalf("应回带随机密码（16 位），实际 %q: %v", pwd, res0)
	}
	if res0["err_id"] != float64(110) {
		t.Fatalf("errid 应为 110（注册成功）: %v", res0)
	}

	// 密码必须写进账号库（该区），且该号在该区"未验证"（还没登录过）
	pw, ok := env.pool.PasswordFor(name, gs.Addr)
	if !ok || pw != pwd {
		t.Fatalf("随机密码应写进库里该区，实际 ok=%v pw=%q 期望 %q", ok, pw, pwd)
	}
	acc, _ := env.pool.Get(name)
	if z := acc.Zone(gs.Addr); z == nil || z.Verified {
		t.Fatalf("建号后该区应为未验证: %+v", z)
	}

	// 关键：随后的验证**用库里的密码**登录（假服比对 md5 证明用的就是它）
	_, vbody := postJSON(t, env.srv.URL+"/api/accounts/verify",
		map[string]any{"accounts": []string{name}, "zone": gs.Addr}, nil)
	if vbody["ok"] != true {
		t.Fatalf("验证应成功: %v", vbody)
	}
	sum := md5.Sum([]byte(pwd))
	cg.mu.Lock()
	got := cg.loginMD5[name]
	cg.mu.Unlock()
	if got != hex.EncodeToString(sum[:]) {
		t.Fatalf("登录必须用库里那个随机密码（md5 不符）got=%s want=%s", got, hex.EncodeToString(sum[:]))
	}
	vres, _ := vbody["results"].([]any)
	vr0, _ := vres[0].(map[string]any)
	if vr0["usable"] != true {
		t.Fatalf("用库里密码应能登录成功: %v", vr0)
	}
}

func TestAccountsCreateFailureDoesNotStorePassword(t *testing.T) {
	env := newTestEnv(t, "")
	gs := testsupport.StartGameServer(t, (&createGame{regErrID: gameproto.RegErrNameExist}).handler())
	name := "robot0009002@xy3.com"

	_, body := postJSON(t, env.srv.URL+"/api/accounts/create", map[string]any{
		"accounts": []string{name}, "zone": gs.Addr, "interval_ms": 0,
	}, nil)
	if body["ok"] != false || body["created"] != float64(0) {
		t.Fatalf("注册失败不该算成功: %v", body)
	}
	results, _ := body["results"].([]any)
	res0, _ := results[0].(map[string]any)
	if msg, _ := res0["msg"].(string); !strings.Contains(msg, "已存在") {
		t.Fatalf("应带出失败原因: %v", res0)
	}
	if _, ok := env.pool.PasswordFor(name, gs.Addr); ok {
		t.Fatal("注册失败不该写入密码")
	}
}

func TestAccountsCreateValidation(t *testing.T) {
	env := newTestEnv(t, "")
	// 空账号
	if _, body := postJSON(t, env.srv.URL+"/api/accounts/create", map[string]any{}, nil); body["ok"] != false {
		t.Fatalf("空账号应拒绝: %v", body)
	}
	// 超量（> 200）
	many := make([]string, 0, 201)
	for i := 0; i < 201; i++ {
		many = append(many, fmt.Sprintf("acc%03d", i)) // 用数字后缀：避免不可见字符被去空白归一化
	}
	_, body := postJSON(t, env.srv.URL+"/api/accounts/create",
		map[string]any{"accounts": many, "zone": "127.0.0.1:1"}, nil)
	if body["ok"] != false {
		t.Fatalf("超量应拒绝: %v", body)
	}
	if msg, _ := body["msg"].(string); !strings.Contains(msg, "200") {
		t.Fatalf("提示应说明上限: %v", body["msg"])
	}
}

// 按编号建号：前缀 + 起始序号 + 数量 (+ 邮箱后缀) → 自动生成账号名，逐个建号并回带随机密码。
func TestAccountsCreateByNumberSpec(t *testing.T) {
	env := newTestEnv(t, "")
	cg := &createGame{}
	gs := testsupport.StartGameServer(t, cg.handler())

	_, body := postJSON(t, env.srv.URL+"/api/accounts/create", map[string]any{
		"prefix": "robot000", "start": 3004, "count": 3, "suffix": "@xy3.com",
		"zone": gs.Addr, "interval_ms": 0,
	}, nil)
	if body["ok"] != true || body["created"] != float64(3) {
		t.Fatalf("按编号建号应成功 3 个: %v", body)
	}
	if body["first"] != "robot0003004@xy3.com" || body["last"] != "robot0003006@xy3.com" {
		t.Fatalf("应回带首尾账号预览: %v", body)
	}
	results, _ := body["results"].([]any)
	if len(results) != 3 {
		t.Fatalf("结果数 %d", len(results))
	}
	pwds := map[string]bool{}
	for i, want := range []string{"robot0003004@xy3.com", "robot0003005@xy3.com", "robot0003006@xy3.com"} {
		r, _ := results[i].(map[string]any)
		if r["account"] != want {
			t.Fatalf("第 %d 个=%v 期望 %s", i, r["account"], want)
		}
		if r["status"] != "created" {
			t.Fatalf("应新建成功: %v", r)
		}
		pwd, _ := r["password"].(string)
		if len(pwd) != 16 {
			t.Fatalf("应回带 16 位随机密码: %v", r)
		}
		if pwds[pwd] {
			t.Fatalf("不同账号的随机密码不该相同: %s", pwd)
		}
		pwds[pwd] = true
		// 密码必须写进库里（该服）
		if got, ok := env.pool.PasswordFor(want, gs.Addr); !ok || got != pwd {
			t.Fatalf("%s 的密码应写进库: ok=%v got=%q want=%q", want, ok, got, pwd)
		}
	}
}

func TestAccountsCreateByNumberSpecValidation(t *testing.T) {
	env := newTestEnv(t, "")
	cases := []struct{ name, body, want string }{
		{"前缀为空", `{"start":1,"count":2,"zone":"127.0.0.1:1"}`, "前缀"},
		{"数量为 0", `{"prefix":"robot000","count":0,"zone":"127.0.0.1:1"}`, "数量"},
		{"数量超上限", `{"prefix":"robot000","count":201,"zone":"127.0.0.1:1"}`, "上限"},
	}
	for _, c := range cases {
		var req map[string]any
		_ = json.Unmarshal([]byte(c.body), &req)
		_, body := postJSON(t, env.srv.URL+"/api/accounts/create", req, nil)
		if body["ok"] != false {
			t.Fatalf("%s: 应被拒: %v", c.name, body)
		}
		if msg, _ := body["msg"].(string); !strings.Contains(msg, c.want) {
			t.Fatalf("%s: 提示 %q 应含 %q", c.name, msg, c.want)
		}
	}
}

// 分批 + 并发：仍要每个账号一条独立连接、结果与输入同序。
func TestAccountsCreateInBatchesWithConcurrency(t *testing.T) {
	env := newTestEnv(t, "")
	cg := &createGame{}
	gs := testsupport.StartGameServer(t, cg.handler())

	_, body := postJSON(t, env.srv.URL+"/api/accounts/create", map[string]any{
		"prefix": "robot000", "start": 3100, "count": 6, "suffix": "@xy3.com",
		"zone": gs.Addr, "batch_size": 2, "concurrency": 2, "interval_ms": 0, "batch_interval_ms": 0,
	}, nil)
	if body["created"] != float64(6) {
		t.Fatalf("6 个都应建成: %v", body)
	}
	results, _ := body["results"].([]any)
	for i := 0; i < 6; i++ {
		r, _ := results[i].(map[string]any)
		want := "robot000310" + string(rune('0'+i)) + "@xy3.com"
		if r["account"] != want {
			t.Fatalf("结果顺序应稳定：第 %d 个=%v 期望 %s", i, r["account"], want)
		}
	}
}

// 库里是"脏密码"（老数据 3 位，服务端要求 6~20）→ 建号时不能拿它去撞，
// 改用新的随机密码注册，并把新密码写回库。
func TestAccountsCreateReplacesTooShortStoredPassword(t *testing.T) {
	env := newTestEnv(t, "")
	cg := &createGame{}
	gs := testsupport.StartGameServer(t, cg.handler())
	name := "robot0003300@xy3.com"

	env.pool.Add([]string{name}, "", gs.Addr, "")
	env.pool.SetZoneState(name, gs.Addr, accounts.ZoneState{Password: "123"}) // py 时代的老密码

	_, body := postJSON(t, env.srv.URL+"/api/accounts/create", map[string]any{
		"accounts": []string{name}, "zone": gs.Addr, "interval_ms": 0,
	}, nil)
	if body["created"] != float64(1) {
		t.Fatalf("老密码不可用时应改用新随机密码建号: %v", body)
	}
	results, _ := body["results"].([]any)
	r0, _ := results[0].(map[string]any)
	pwd, _ := r0["password"].(string)
	if len(pwd) < 8 || pwd == "123" {
		t.Fatalf("应换用新的随机密码: %v", r0)
	}
	if got, ok := env.pool.PasswordFor(name, gs.Addr); !ok || got != pwd {
		t.Fatalf("新密码应写回库: ok=%v got=%q want=%q", ok, got, pwd)
	}
}

func TestAccountsCreateAutoStartFromPool(t *testing.T) {
	env := newTestEnv(t, "")
	cg := &createGame{}
	gs := testsupport.StartGameServer(t, cg.handler())
	// 池里最大是 robot0001002 → 自动接续应从 1003 开始
	_, body := postJSON(t, env.srv.URL+"/api/accounts/create", map[string]any{
		"prefix": "robot000", "count": 2, "suffix": "@xy3.com", "auto_start": true,
		"zone": gs.Addr, "interval_ms": 0,
	}, nil)
	if body["first"] != "robot0001003@xy3.com" {
		t.Fatalf("自动接续应从池内最大序号+1 开始: %v", body)
	}
}

// readStr8 读 104 的 8 个字符串字段（假服务端用）。
func readStr8(body []byte) []string {
	r := gameproto.NewReader(body, gameproto.UTF8)
	out := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		out = append(out, r.Str())
	}
	return out
}

// 账号已存在且库里密码对 → 走"验证账号"分支：**不再注册**，标可用并写回角色/等级。
func TestAccountsCreateReusesExistingAccount(t *testing.T) {
	env := newTestEnv(t, "")
	name := "robot0009200@xy3.com"
	cg := &createGame{existing: map[string]string{name: "OldPwd9x7Qm2Lp"}}
	gs := testsupport.StartGameServer(t, cg.handler())

	env.pool.Add([]string{name}, "", env.zoneKey, "")
	env.pool.SetZoneState(name, gs.Addr, accounts.ZoneState{Password: "OldPwd9x7Qm2Lp"})

	_, body := postJSON(t, env.srv.URL+"/api/accounts/create", map[string]any{
		"accounts": []string{name}, "zone": gs.Addr, "interval_ms": 0,
	}, nil)
	if body["existing"] != float64(1) || body["created"] != float64(0) {
		t.Fatalf("已存在的号应走 existing 分支: %v", body)
	}
	results, _ := body["results"].([]any)
	r0, _ := results[0].(map[string]any)
	if r0["status"] != "existing" || r0["usable"] != true || r0["exists"] != true {
		t.Fatalf("结果应标 existing + 可用: %v", r0)
	}
	if cg.regCount != 0 {
		t.Fatalf("已存在的号不该再注册（收到 %d 次 104）", cg.regCount)
	}
	acc, _ := env.pool.Get(name)
	z := acc.Zone(gs.Addr)
	if z == nil || !z.Verified || !z.Usable || z.RoleName == "" {
		t.Fatalf("池里应标可用并写回角色: %+v", z)
	}
	if z.Password != "OldPwd9x7Qm2Lp" {
		t.Fatalf("已存在时不该改密码: %+v", z)
	}
}

// 账号存在但库里密码不对（200）→ status=password_mismatch：不注册、标不可用并说明怎么处理。
func TestAccountsCreateDetectsPasswordMismatch(t *testing.T) {
	env := newTestEnv(t, "")
	name := "robot0009201@xy3.com"
	cg := &createGame{existing: map[string]string{name: "ServerRealPwd1"}}
	gs := testsupport.StartGameServer(t, cg.handler())

	env.pool.Add([]string{name}, "", env.zoneKey, "")
	env.pool.SetZoneState(name, gs.Addr, accounts.ZoneState{Password: "StaleWrongPwd9"})

	_, body := postJSON(t, env.srv.URL+"/api/accounts/create", map[string]any{
		"accounts": []string{name}, "zone": gs.Addr, "interval_ms": 0,
	}, nil)
	if body["password_mismatch"] != float64(1) {
		t.Fatalf("密码不符应单独统计: %v", body)
	}
	results, _ := body["results"].([]any)
	r0, _ := results[0].(map[string]any)
	if r0["status"] != "password_mismatch" || r0["exists"] != true || r0["usable"] != false {
		t.Fatalf("结果应标 password_mismatch: %v", r0)
	}
	if cg.regCount != 0 {
		t.Fatalf("账号已存在时注册必回 111，不该去试（收到 %d 次 104）", cg.regCount)
	}
	acc, _ := env.pool.Get(name)
	z := acc.Zone(gs.Addr)
	if z == nil || !z.Verified || z.Usable {
		t.Fatalf("池里应标『存在但不可用』: %+v", z)
	}
	if !strings.Contains(z.Msg, "密码") {
		t.Fatalf("说明应点明密码问题: %q", z.Msg)
	}
}
