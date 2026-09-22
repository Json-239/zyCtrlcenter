package accountverify_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/accountverify"
	"zyctrlcenter/internal/gameproto"
	"zyctrlcenter/test/testsupport"
)

// ---------------- 假游戏服脚本 ----------------

// ackScript 一段"登录探测"应答脚本：握手 → 版本 → 登录(300) → 查角色(90132)。
type ackScript struct {
	code  int32  // 300 的 retcode
	msg   string // 300 的 msg
	role  string // 角色名（code==0 时回；空 = 回"无角色"列表）
	level int16
	turn  int8
}

type gameHandler func(c *testsupport.GameConn, msgID int32, body []byte) [][]byte

// handler 把脚本翻译成假游戏服的应答。
func (s ackScript) handler(t *testing.T) gameHandler {
	return func(c *testsupport.GameConn, msgID int32, _ []byte) [][]byte {
		switch msgID {
		case gameproto.MsgConnect:
			// 100 → 服务端连接查询(200) + 握手回包(255)
			return [][]byte{
				testsupport.FrameBytes(gameproto.MsgConnectQuery, []any{}, []any{}, gameproto.UTF8),
				testsupport.FrameBytes(gameproto.MsgConnectBack, []any{4}, []any{int64(200)}, gameproto.UTF8),
			}
		case gameproto.MsgConnectBack:
			// 255 → 版本返回(400) retcode=0
			return [][]byte{testsupport.FrameBytes(gameproto.MsgVerBack, []any{4, ""},
				[]any{int64(0), "ok"}, gameproto.UTF8)}
		case gameproto.MsgLogin:
			// 102 → 登录验证返回(300)
			return [][]byte{testsupport.FrameBytes(gameproto.MsgAckAccount, []any{4, ""},
				[]any{int64(s.code), s.msg}, gameproto.UTF8)}
		case gameproto.MsgQueryRole:
			// 1000 → 角色列表(90132)
			if s.role == "" {
				return [][]byte{roleFrame(t, nil, gameproto.UTF8)}
			}
			return [][]byte{roleFrame(t, []gameproto.Role{{
				RoleID: 1001, RoleIndex: 1, Name: s.role, Level: s.level, Turn: s.turn,
			}}, gameproto.UTF8)}
		}
		return nil
	}
}

// roleFrame 用一个完整帧包住角色列表（借用被测包的编码器：编解码必须成对）。
func roleFrame(t *testing.T, roles []gameproto.Role, coding string) []byte {
	t.Helper()
	raw, err := gameproto.BuildRoleFrame(roles, coding)
	if err != nil {
		t.Fatalf("构造角色帧失败: %v", err)
	}
	return raw
}

// ---------------- 正常链路 ----------------

func TestProbeUsableWhenLoginOKWithRole(t *testing.T) {
	gs := testsupport.StartGameServer(t, ackScript{
		code: gameproto.LoginOK, msg: "登录成功", role: "测试甲", level: 21, turn: 0,
	}.handler(t))

	opt := accountverify.DefaultOptions()
	res := accountverify.Probe(context.Background(), gs.Addr, "robotA", "123456", opt)

	if res.Err != "" {
		t.Fatalf("探测应成功，实际 Err=%s", res.Err)
	}
	if !res.Usable || !res.Exists {
		t.Fatalf("登录成功应 exists+usable：%+v", res)
	}
	if res.RetCode != gameproto.LoginOK {
		t.Fatalf("retcode=%d 期望 0", res.RetCode)
	}
	if res.RoleName != "测试甲" || res.Level != 21 {
		t.Fatalf("角色信息不符: %+v", res)
	}
	if res.ElapsedMs < 0 || res.ElapsedMs > 2000 {
		t.Fatalf("本地假服务端不该这么慢: %+v", res)
	}
	if res.Zone != gs.Addr {
		t.Fatalf("结果应带上目标地址: %+v", res)
	}
	if res.At <= 0 {
		t.Fatalf("结果应记时间戳: %+v", res)
	}

	// 客户端发出的帧：顺序必须 100 → 255 → 101 → 102 → 1000
	ids := gs.MsgIDs()
	want := []int32{gameproto.MsgConnect, gameproto.MsgConnectBack, gameproto.MsgVer,
		gameproto.MsgLogin, gameproto.MsgQueryRole}
	if len(ids) < len(want) {
		t.Fatalf("应收到的帧不足: %v", ids)
	}
	for i, w := range want {
		if ids[i] != w {
			t.Fatalf("第 %d 帧 msgid=%d 期望 %d（序列 %v）", i, ids[i], w, ids)
		}
	}

	// 255 必须带 200
	if got := testsupport.UnpackInt32(gs.Received(gameproto.MsgConnectBack)[0]); got != 200 {
		t.Fatalf("握手回包应带 200，实际 %d", got)
	}
	// 101 必须带版本号
	verBody := gs.Received(gameproto.MsgVer)[0]
	if v, _, err := gameproto.Unpack([]any{""}, verBody, 0, gameproto.UTF8); err != nil {
		t.Fatal(err)
	} else if v[0].(string) != opt.Version {
		t.Fatalf("版本号 %q 期望 %q", v[0], opt.Version)
	}
	// 102 必须是 [账号, md5(密码)]
	sum := md5.Sum([]byte("123456"))
	vals, _, err := gameproto.Unpack([]any{"", ""}, gs.Received(gameproto.MsgLogin)[0], 0, gameproto.UTF8)
	if err != nil {
		t.Fatal(err)
	}
	if vals[0].(string) != "robotA" || vals[1].(string) != hex.EncodeToString(sum[:]) {
		t.Fatalf("登录帧不符: %#v", vals)
	}
	// 1000 必须带 [0]
	if got := testsupport.UnpackInt32(gs.Received(gameproto.MsgQueryRole)[0]); got != 0 {
		t.Fatalf("查角色包应带 0，实际 %d", got)
	}
}

func TestProbeUsableWhenLoginOKWithoutRole(t *testing.T) {
	gs := testsupport.StartGameServer(t, ackScript{code: gameproto.LoginOK, msg: "登录成功"}.handler(t))
	res := accountverify.Probe(context.Background(), gs.Addr, "robotA", "123456", accountverify.DefaultOptions())
	if res.Err != "" || !res.Usable {
		t.Fatalf("登录成功无角色也应可用: %+v", res)
	}
	if res.RoleName != "" {
		t.Fatalf("无角色时角色名应为空: %+v", res)
	}
	if !strings.Contains(res.Msg, "无角色") {
		t.Fatalf("说明里应提到无角色: %q", res.Msg)
	}
}

// ---------------- 服务端明确"不可用/不存在" ----------------

func TestProbeNoAccount(t *testing.T) {
	gs := testsupport.StartGameServer(t, ackScript{
		code: gameproto.LoginInvalidAccount, msg: "账号不存在",
	}.handler(t))
	res := accountverify.Probe(context.Background(), gs.Addr, "nobody", "123456", accountverify.DefaultOptions())

	if res.Err != "" {
		t.Fatalf("服务端明确回 100 属于正常结果，不应记 Err: %+v", res)
	}
	if res.Exists || res.Usable {
		t.Fatalf("100=账号不存在 → exists/usable 都应为 false: %+v", res)
	}
	if !strings.Contains(res.Msg, "不存在") {
		t.Fatalf("说明应含『不存在』: %q", res.Msg)
	}
	if n := len(gs.Received(gameproto.MsgQueryRole)); n != 0 {
		t.Fatalf("账号不存在时不应再查角色（发了 %d 次）", n)
	}
}

func TestProbeBadPasswordIsNotUsable(t *testing.T) {
	gs := testsupport.StartGameServer(t, ackScript{
		code: gameproto.LoginInvalidPassword, msg: "密码错误",
	}.handler(t))
	res := accountverify.Probe(context.Background(), gs.Addr, "robotA", "wrong", accountverify.DefaultOptions())

	if !res.Exists {
		t.Fatalf("200=密码错误说明账号存在: %+v", res)
	}
	if res.Usable {
		t.Fatalf("密码错的号不能算可用（否则会被反复拉起）: %+v", res)
	}
	if !strings.Contains(res.Msg, "密码") {
		t.Fatalf("说明应含『密码』: %q", res.Msg)
	}
}

func TestProbeFreezedIsNotUsable(t *testing.T) {
	gs := testsupport.StartGameServer(t, ackScript{
		code: gameproto.LoginFreezed, msg: "账号已冻结",
	}.handler(t))
	res := accountverify.Probe(context.Background(), gs.Addr, "robotA", "123456", accountverify.DefaultOptions())

	if !res.Exists || res.Usable {
		t.Fatalf("300=冻结 → 存在但不可用: %+v", res)
	}
	if !strings.Contains(res.Msg, "冻结") {
		t.Fatalf("说明应含『冻结』: %q", res.Msg)
	}
}

// ---------------- 选项 ----------------

func TestProbeSkipsRoleQueryWhenDisabled(t *testing.T) {
	gs := testsupport.StartGameServer(t, ackScript{
		code: gameproto.LoginOK, msg: "登录成功", role: "测试甲", level: 21,
	}.handler(t))

	opt := accountverify.DefaultOptions()
	opt.QueryRole = false
	res := accountverify.Probe(context.Background(), gs.Addr, "robotA", "123456", opt)

	if !res.Usable {
		t.Fatalf("应可用: %+v", res)
	}
	if n := len(gs.Received(gameproto.MsgQueryRole)); n != 0 {
		t.Fatalf("QueryRole=false 时不应查角色（发了 %d 次）", n)
	}
}

func TestProbeGBKZoneFailsFastNotSilently(t *testing.T) {
	// 区编码配成 GBK 时：本实现只内置 UTF-8 → 必须**明确报错**，
	// 绝不能静默乱码（乱码会让"账号是否存在"的判断全错）。
	gs := testsupport.StartGameServer(t, ackScript{code: gameproto.LoginOK, msg: "登录成功"}.handler(t))

	opt := accountverify.DefaultOptions()
	opt.Coding = gameproto.GBK
	res := accountverify.Probe(context.Background(), gs.Addr, "robotA", "123456", opt)

	if res.Err == "" {
		t.Fatalf("GBK 区应快速失败并给出原因，实际 %+v", res)
	}
	if !strings.Contains(res.Err, "GBK") {
		t.Fatalf("错误信息应点明 GBK 编码问题: %q", res.Err)
	}
	if res.Usable || res.Exists {
		t.Fatalf("失败时不得判为可用: %+v", res)
	}
	if res.ElapsedMs > 1000 {
		t.Fatalf("编码不支持应立即失败（不该等超时）: %dms", res.ElapsedMs)
	}
}

// ---------------- 异常路径 ----------------

func TestProbeTimeoutWhenNoAck(t *testing.T) {
	gs := testsupport.StartGameServer(t, func(c *testsupport.GameConn, msgID int32, _ []byte) [][]byte {
		switch msgID {
		case gameproto.MsgConnect:
			return [][]byte{
				testsupport.FrameBytes(gameproto.MsgConnectQuery, nil, nil, gameproto.UTF8),
				testsupport.FrameBytes(gameproto.MsgConnectBack, []any{4}, []any{int64(200)}, gameproto.UTF8),
			}
		case gameproto.MsgConnectBack:
			return [][]byte{testsupport.FrameBytes(gameproto.MsgVerBack, []any{4, ""}, []any{int64(0), ""}, gameproto.UTF8)}
		}
		return nil // 102 不回 → 客户端应超时
	})

	opt := accountverify.DefaultOptions()
	opt.Timeout = 300 * time.Millisecond
	start := time.Now()
	res := accountverify.Probe(context.Background(), gs.Addr, "robotA", "123456", opt)
	elapsed := time.Since(start)

	if res.Err == "" {
		t.Fatalf("无应答必须报错（不能让调用方以为成功）: %+v", res)
	}
	if res.Usable || res.Exists {
		t.Fatalf("超时不得判定为可用: %+v", res)
	}
	if elapsed < 250*time.Millisecond {
		t.Fatalf("应等满超时再放弃，实际 %v", elapsed)
	}
	if !strings.Contains(res.Err, "超时") {
		t.Fatalf("错误信息应说明超时: %q", res.Err)
	}
}

func TestProbeConnectionRefused(t *testing.T) {
	gs := testsupport.StartGameServer(t, nil)
	addr := gs.Addr
	gs.Close() // 关掉监听 → 连接必失败

	opt := accountverify.DefaultOptions()
	opt.Timeout = 500 * time.Millisecond
	res := accountverify.Probe(context.Background(), addr, "robotA", "123456", opt)
	if res.Err == "" || res.Usable {
		t.Fatalf("连不上必须报错且不可用: %+v", res)
	}
	if res.Exists {
		t.Fatalf("连不上时『存在』未知，应记 false: %+v", res)
	}
}

func TestProbeStopsOnContextCancel(t *testing.T) {
	gs := testsupport.StartGameServer(t, func(c *testsupport.GameConn, msgID int32, _ []byte) [][]byte {
		return nil // 完全不应答
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	opt := accountverify.DefaultOptions()
	opt.Timeout = 5 * time.Second // 故意设很长：必须由 context 提前结束
	start := time.Now()
	res := accountverify.Probe(ctx, gs.Addr, "robotA", "123456", opt)
	if res.Err == "" {
		t.Fatalf("context 取消必须报错: %+v", res)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("应随 context 取消提前返回，而不是等满 Timeout")
	}
}

// ---------------- 批量（每账号一条独立连接）----------------

func TestVerifyAllKeepsInputOrderAndUsesOneConnPerAccount(t *testing.T) {
	gs := testsupport.StartGameServer(t, func(c *testsupport.GameConn, msgID int32, body []byte) [][]byte {
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
			switch acc {
			case "a1":
				return [][]byte{testsupport.FrameBytes(gameproto.MsgAckAccount, []any{4, ""},
					[]any{int64(gameproto.LoginOK), "登录成功"}, gameproto.UTF8)}
			case "a2":
				return [][]byte{testsupport.FrameBytes(gameproto.MsgAckAccount, []any{4, ""},
					[]any{int64(gameproto.LoginInvalidAccount), "账号不存在"}, gameproto.UTF8)}
			default:
				return [][]byte{testsupport.FrameBytes(gameproto.MsgAckAccount, []any{4, ""},
					[]any{int64(gameproto.LoginInvalidPassword), "密码错误"}, gameproto.UTF8)}
			}
		case gameproto.MsgQueryRole:
			return [][]byte{roleFrame(t, []gameproto.Role{{RoleID: 7, Name: "甲一", Level: 30}}, gameproto.UTF8)}
		}
		return nil
	})

	targets := []accountverify.Target{
		{Account: "a1", Password: "p"},
		{Account: "a2", Password: "p"},
		{Account: "a3", Password: "p"},
	}
	results := accountverify.VerifyAll(context.Background(), gs.Addr, targets,
		accountverify.DefaultOptions(), 3)

	if len(results) != 3 {
		t.Fatalf("结果数 %d 期望 3", len(results))
	}
	for i, want := range []string{"a1", "a2", "a3"} {
		if results[i].Account != want {
			t.Fatalf("结果顺序必须与输入一致：第 %d 个是 %s，期望 %s", i, results[i].Account, want)
		}
	}
	if !results[0].Usable || results[0].RoleName != "甲一" || results[0].Level != 30 {
		t.Fatalf("a1 应可用且带角色: %+v", results[0])
	}
	if results[1].Usable || results[1].Exists {
		t.Fatalf("a2 应不存在: %+v", results[1])
	}
	if results[2].Usable || !results[2].Exists {
		t.Fatalf("a3 应存在但不可用: %+v", results[2])
	}
	if n := gs.ConnCount(); n != 3 {
		t.Fatalf("每个账号一次独立连接（互不影响），实际 %d 条", n)
	}
}

func TestVerifyAllEmptyAndBadConcurrency(t *testing.T) {
	if got := accountverify.VerifyAll(context.Background(), "127.0.0.1:1", nil,
		accountverify.DefaultOptions(), 4); len(got) != 0 {
		t.Fatalf("空输入应返回空结果: %v", got)
	}

	gs := testsupport.StartGameServer(t, ackScript{code: gameproto.LoginOK, msg: "登录成功"}.handler(t))
	// concurrency <= 0 → 按 1 处理（不能 panic / 不能死锁）
	results := accountverify.VerifyAll(context.Background(), gs.Addr,
		[]accountverify.Target{{Account: "a1", Password: "p"}}, accountverify.DefaultOptions(), 0)
	if len(results) != 1 || !results[0].Usable {
		t.Fatalf("concurrency=0 应按 1 处理: %+v", results)
	}
}

// ---------------- 编解码成对：与参考实现字节一致 ----------------

func TestBuildRoleFrameMatchesReferenceFixture(t *testing.T) {
	fx := struct {
		Frames []struct {
			Name string `json:"name"`
			Hex  string `json:"hex"`
		} `json:"frames"`
	}{}
	testsupport.LoadJSONFixture(t, "gameproto/frames.json", &fx)

	var wantHex string
	for _, f := range fx.Frames {
		if f.Name == "s2c_query_role" {
			wantHex = f.Hex
		}
	}
	if wantHex == "" {
		t.Fatal("缺少 s2c_query_role 夹具")
	}
	want, _ := hex.DecodeString(wantHex)

	got, err := gameproto.BuildRoleFrame([]gameproto.Role{{
		RoleID: 1001, RoleIndex: 1, Name: "测试甲", Level: 21, Turn: 0,
	}}, gameproto.UTF8)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("角色帧字节与参考实现不一致\n got=%X\nwant=%X", got, want)
	}
}
