// 建号（注册协议）用例：握手 → 106 取验证码 → 104 注册 → 700 判结果；随机密码生成。
package accountcreate_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/accountcreate"
	"zyctrlcenter/internal/gameproto"
	"zyctrlcenter/test/testsupport"
)

const regCode = "动感高清"

// testPwd 合法长度的测试密码（实现要求 >= MinPasswordLen，密码长度另有专门用例覆盖）
const testPwd = "Ab3kQ9Zx7Lm2"

type regScript struct {
	dbRet, errItem, errID int32
	withCode              bool // 是否回 702 验证码（false → 服务端不答，测超时）
	gotFields             []string
}

func (s *regScript) handler() func(c *testsupport.GameConn, msgID int32, body []byte) [][]byte {
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
			if !s.withCode {
				return nil
			}
			return [][]byte{testsupport.FrameBytes(gameproto.MsgRegCodeBack, []any{""}, []any{regCode}, gameproto.UTF8)}
		case gameproto.MsgRegister:
			// 记录 8 个字段（建号包内容必须精确：验证码/账号/密码×2/其余空）
			r := gameproto.NewReader(body, gameproto.UTF8)
			fields := make([]string, 0, 8)
			for i := 0; i < 8; i++ {
				fields = append(fields, r.Str())
			}
			s.gotFields = fields
			return [][]byte{testsupport.FrameBytes(gameproto.MsgRegisterBack, []any{4, 4, 4},
				[]any{int64(s.dbRet), int64(s.errItem), int64(s.errID)}, gameproto.UTF8)}
		}
		return nil
	}
}

func TestRegisterSendsEightFieldsAndReportsSuccess(t *testing.T) {
	sc := &regScript{dbRet: 0, errItem: 4, errID: 110, withCode: true}
	gs := testsupport.StartGameServer(t, sc.handler())

	res := accountcreate.Register(context.Background(), gs.Addr, "robot0003100", "Ab3kQ9Zx7Lm2Np5R",
		accountcreate.DefaultOptions())

	if res.Err != "" || !res.OK {
		t.Fatalf("注册应成功: %+v", res)
	}
	if res.ErrID != 110 || res.Account != "robot0003100" || res.Zone != gs.Addr {
		t.Fatalf("结果字段不符: %+v", res)
	}
	if res.Password != "Ab3kQ9Zx7Lm2Np5R" {
		t.Fatalf("结果应回带明文密码（用于写入库）: %+v", res)
	}
	// 建号包必须带：服务端刚给的验证码 + 账号 + 密码/确认密码一致 + 其余为空
	want := []string{regCode, "robot0003100", res.Password, res.Password, "", "", "", ""}
	if len(sc.gotFields) != 8 {
		t.Fatalf("104 必须 8 个字段，实际 %d: %v", len(sc.gotFields), sc.gotFields)
	}
	for i := range want {
		if sc.gotFields[i] != want[i] {
			t.Fatalf("104 第 %d 个字段=%q 期望 %q", i, sc.gotFields[i], want[i])
		}
	}
	// 时序：100 → 255 → 101 → 106 → 104（注册不发登录 102）
	ids := gs.MsgIDs()
	expect := []int32{gameproto.MsgConnect, gameproto.MsgConnectBack, gameproto.MsgVer,
		gameproto.MsgGetRegCode, gameproto.MsgRegister}
	for i, w := range expect {
		if i >= len(ids) || ids[i] != w {
			t.Fatalf("帧顺序不符（第 %d 个应为 %d）: %v", i, w, ids)
		}
	}
}

func TestRegisterFailureReasons(t *testing.T) {
	cases := []struct {
		name       string
		errID      int32
		contains   string
		retryable  bool
	}{
		{"用户名已存在", 111, "已存在", false},
		{"验证码出错", 116, "验证码", false},
		{"风控失败可重试", 112, "风控", true},
	}
	for _, c := range cases {
		sc := &regScript{dbRet: 1, errItem: 2, errID: c.errID, withCode: true}
		gs := testsupport.StartGameServer(t, sc.handler())
		res := accountcreate.Register(context.Background(), gs.Addr, "acc", testPwd, accountcreate.DefaultOptions())

		if res.Err != "" {
			t.Fatalf("%s: 服务端明确回绝不算 Err: %+v", c.name, res)
		}
		if res.OK {
			t.Fatalf("%s: 不该判成功: %+v", c.name, res)
		}
		if !strings.Contains(res.Msg, c.contains) {
			t.Fatalf("%s: 说明 %q 应含 %q", c.name, res.Msg, c.contains)
		}
		if res.Retryable != c.retryable {
			t.Fatalf("%s: 可重试=%v 期望 %v", c.name, res.Retryable, c.retryable)
		}
	}
}

func TestRegisterTimeoutWhenNoRegCode(t *testing.T) {
	sc := &regScript{withCode: false}
	gs := testsupport.StartGameServer(t, sc.handler())

	opt := accountcreate.DefaultOptions()
	opt.Timeout = 300 * time.Millisecond
	res := accountcreate.Register(context.Background(), gs.Addr, "acc", testPwd, opt)

	if res.Err == "" || res.OK {
		t.Fatalf("拿不到验证码必须报错且不算成功: %+v", res)
	}
	if !strings.Contains(res.Err, "超时") {
		t.Fatalf("错误应说明超时: %q", res.Err)
	}
}

func TestRegisterConnectionRefused(t *testing.T) {
	gs := testsupport.StartGameServer(t, nil)
	addr := gs.Addr
	gs.Close()
	res := accountcreate.Register(context.Background(), addr, "acc", testPwd, accountcreate.DefaultOptions())
	if res.Err == "" || res.OK {
		t.Fatalf("连不上必须报错: %+v", res)
	}
}

func TestRegisterGBKFailsFast(t *testing.T) {
	sc := &regScript{dbRet: 0, errItem: 4, errID: 110, withCode: true}
	gs := testsupport.StartGameServer(t, sc.handler())
	opt := accountcreate.DefaultOptions()
	opt.Coding = gameproto.GBK
	res := accountcreate.Register(context.Background(), gs.Addr, "acc", testPwd, opt)
	if res.Err == "" || !strings.Contains(res.Err, "GBK") {
		t.Fatalf("GBK 应快速失败并点明编码: %+v", res)
	}
}

// 随机密码：长度/字符集/无歧义字符/不重复（"没有统一密码"的基础）
func TestNewPasswordRandomAndSafe(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		pw, err := accountcreate.NewPassword(16)
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) != 16 {
			t.Fatalf("长度应为 16: %q", pw)
		}
		for _, r := range pw {
			ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '2' && r <= '9')
			if !ok {
				t.Fatalf("密码含不安全字符 %q: %q", r, pw)
			}
		}
		if seen[pw] {
			t.Fatalf("出现重复密码: %q", pw)
		}
		seen[pw] = true
	}
	if _, err := accountcreate.NewPassword(4); err == nil {
		t.Fatal("长度过短应报错（默认至少 8 位）")
	}
}
