// 注册协议（106 取验证码 → 702；104 注册 → 700）编解码测试。字节由参考实现生成。
package gameproto_test

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"zyctrlcenter/internal/gameproto"
	"zyctrlcenter/test/testsupport"
)

type regFrames struct {
	Frames []frameFixture `json:"frames"`
}

func loadRegFrames(t *testing.T) []frameFixture {
	t.Helper()
	var f regFrames
	testsupport.LoadJSONFixture(t, "gameproto/register.json", &f)
	if len(f.Frames) == 0 {
		t.Fatal("注册协议夹具为空")
	}
	return f.Frames
}

func regByName(t *testing.T, name string) frameFixture {
	t.Helper()
	for _, f := range loadRegFrames(t) {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("夹具里没有 %s", name)
	return frameFixture{}
}

func TestRegisterFixturesMatchOurPacking(t *testing.T) {
	for _, f := range loadRegFrames(t) {
		if !f.Provenance.Real || strings.TrimSpace(f.Provenance.How) == "" {
			t.Fatalf("夹具 %s 缺 provenance", f.Name)
		}
		want, _ := hex.DecodeString(f.Hex)
		got, err := gameproto.PackFrame(f.MsgID, normFmt(f.Fmt), normVals(f.Data), f.Coding)
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s 字节不一致\n got=%X\nwant=%X", f.Name, got, want)
		}
	}
}

// 104 的 8 个字段顺序：[验证码, 账号, 密码, 确认密码, 激活码, 姓名, 身份证, QQ]
func TestBuildRegisterFrameMatchesFixture(t *testing.T) {
	f := regByName(t, "c2s_register")
	want, _ := hex.DecodeString(f.Hex)
	fields := make([]string, 0, len(f.Data))
	for _, v := range f.Data {
		s, _ := v.(string)
		fields = append(fields, s)
	}
	got, err := gameproto.BuildRegisterFrame(fields, gameproto.UTF8)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("104 字节不一致\n got=%X\nwant=%X", got, want)
	}
	if len(fields) != 8 {
		t.Fatalf("注册包必须是 8 个字段，实际 %d", len(fields))
	}
	if fields[2] != fields[3] {
		t.Fatalf("密码与确认密码必须一致: %v", fields)
	}
}

func TestBuildRegisterFrameRejectsFieldCount(t *testing.T) {
	if _, err := gameproto.BuildRegisterFrame([]string{"only-one"}, gameproto.UTF8); err == nil {
		t.Fatal("字段数不等于 8 必须报错")
	}
}

func TestParseRegisterResult(t *testing.T) {
	cases := []struct {
		fixture       string
		dbRet, errID  int32
		retryable     bool
		descContains  string
	}{
		{"s2c_register_ok", 0, 110, false, "成功"},
		{"s2c_register_exists", 1, 111, false, "已存在"},
		{"s2c_register_bad_code", 1, 116, false, "验证码"},
		{"s2c_register_risk", 1, 112, true, "风控"},
	}
	for _, c := range cases {
		f := regByName(t, c.fixture)
		raw, _ := hex.DecodeString(f.Hex)
		msgID, body, err := gameproto.ReadFrame(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("%s: %v", c.fixture, err)
		}
		if msgID != gameproto.MsgRegisterBack {
			t.Fatalf("%s: msgid=%d", c.fixture, msgID)
		}
		res, err := gameproto.ParseRegisterResult(body, f.Coding)
		if err != nil {
			t.Fatalf("%s: %v", c.fixture, err)
		}
		if res.DbRet != c.dbRet || res.ErrID != c.errID {
			t.Fatalf("%s: %+v 期望 dbRet=%d errID=%d", c.fixture, res, c.dbRet, c.errID)
		}
		if res.OK() != (c.dbRet == 0) {
			t.Fatalf("%s: OK() 判定不符: %+v", c.fixture, res)
		}
		if gameproto.RegisterRetryable(res.ErrID) != c.retryable {
			t.Fatalf("%s: 可重试判定不符（errid=%d）", c.fixture, res.ErrID)
		}
		if d := gameproto.RegisterErrDesc(res.ErrID); !strings.Contains(d, c.descContains) {
			t.Fatalf("%s: 说明 %q 应含 %q", c.fixture, d, c.descContains)
		}
	}
}

func TestParseRegisterResultTruncated(t *testing.T) {
	if _, err := gameproto.ParseRegisterResult([]byte{1, 0}, gameproto.UTF8); err == nil {
		t.Fatal("字段不足必须报错")
	}
}
