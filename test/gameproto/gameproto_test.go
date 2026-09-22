package gameproto_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"zyctrlcenter/internal/gameproto"
	"zyctrlcenter/test/testsupport"
)

// ---------------- 夹具 ----------------

type frameFixture struct {
	Name   string `json:"name"`
	Desc   string `json:"desc"`
	MsgID  int32  `json:"msg_id"`
	Fmt    []any  `json:"fmt"`
	Data   []any  `json:"data"`
	Coding string `json:"coding"`
	Hex    string `json:"hex"`
	// Provenance 真实性声明（与 events 夹具同规则：必须说明"怎么来的"）
	Provenance struct {
		Real bool   `json:"real"`
		How  string `json:"how"`
	} `json:"provenance"`
}

type framesFile struct {
	Comment string         `json:"_comment"`
	Frames  []frameFixture `json:"frames"`
}

func loadFrames(t *testing.T) []frameFixture {
	t.Helper()
	var f framesFile
	testsupport.LoadJSONFixture(t, "gameproto/frames.json", &f)
	if len(f.Frames) == 0 {
		t.Fatal("协议字节夹具为空")
	}
	return f.Frames
}

// normFmt 夹具里的 fmt 形如 [4, ""]（JSON 数字 → float64），统一成 Pack 需要的 int/string。
func normFmt(in []any) []any {
	out := make([]any, 0, len(in))
	for _, v := range in {
		switch t := v.(type) {
		case float64:
			out = append(out, int(t))
		default:
			out = append(out, v)
		}
	}
	return out
}

// normVals data 形如 [200] / ["账号","md5"]，数字统一成 int64（够放 90132 这类大值）。
func normVals(in []any) []any {
	out := make([]any, 0, len(in))
	for _, v := range in {
		switch t := v.(type) {
		case float64:
			out = append(out, int64(t))
		default:
			out = append(out, v)
		}
	}
	return out
}

func fixtureByName(t *testing.T, name string) frameFixture {
	t.Helper()
	for _, f := range loadFrames(t) {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("夹具里没有 %s", name)
	return frameFixture{}
}

// 夹具必须是"实际的字节"，不是手写的：provenance 要说明来源。
func TestFrameFixturesHaveProvenance(t *testing.T) {
	for _, f := range loadFrames(t) {
		if !f.Provenance.Real || strings.TrimSpace(f.Provenance.How) == "" {
			t.Fatalf("夹具 %s 缺少 provenance（real/how）：协议字节必须具备实际数据支撑", f.Name)
		}
		if _, err := hex.DecodeString(f.Hex); err != nil {
			t.Fatalf("夹具 %s 的 hex 非法: %v", f.Name, err)
		}
	}
}

// ---------------- 打包：必须与参考实现生成的字节完全一致 ----------------

func TestPackMatchesReferenceFixtureBytes(t *testing.T) {
	for _, f := range loadFrames(t) {
		if len(f.Fmt) > 0 && f.Fmt[0] == "raw" { // 复杂/变长体（角色列表）单独测解析
			continue
		}
		want, _ := hex.DecodeString(f.Hex)
		got, err := gameproto.PackFrame(f.MsgID, normFmt(f.Fmt), normVals(f.Data), f.Coding)
		if err != nil {
			t.Fatalf("%s: PackFrame 失败: %v", f.Name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: 字节不一致\n got=%X\nwant=%X", f.Name, got, want)
		}
	}
}

// ---------------- 解包：夹具字节必须解析出夹具里声明的字段 ----------------

func TestUnpackReferenceFixtureBytes(t *testing.T) {
	for _, f := range loadFrames(t) {
		if len(f.Fmt) > 0 && f.Fmt[0] == "raw" {
			continue
		}
		raw, _ := hex.DecodeString(f.Hex)
		msgID, body, err := gameproto.ReadFrame(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("%s: ReadFrame 失败: %v", f.Name, err)
		}
		if msgID != f.MsgID {
			t.Fatalf("%s: msgid=%d 期望 %d", f.Name, msgID, f.MsgID)
		}
		vals, off, err := gameproto.Unpack(normFmt(f.Fmt), body, 0, f.Coding)
		if err != nil {
			t.Fatalf("%s: Unpack 失败: %v", f.Name, err)
		}
		if off != len(body) {
			t.Fatalf("%s: 解包后剩余 %d 字节（应恰好读完）", f.Name, len(body)-off)
		}
		want := normVals(f.Data)
		if len(vals) != len(want) {
			t.Fatalf("%s: 字段数 %d != %d", f.Name, len(vals), len(want))
		}
		for i := range vals {
			if !sameVal(vals[i], want[i]) {
				t.Fatalf("%s: 第 %d 个字段 %#v != %#v", f.Name, i, vals[i], want[i])
			}
		}
	}
}

// ---------------- 流式读写（真实代码用的 API）----------------

func TestWriterMatchesLoginFixtureAndReaderParsesIt(t *testing.T) {
	f := fixtureByName(t, "c2s_login")
	acc, _ := f.Data[0].(string)
	md5pwd, _ := f.Data[1].(string)

	w := gameproto.NewWriter(gameproto.MsgLogin, gameproto.UTF8)
	w.Str(acc).Str(md5pwd)

	want, _ := hex.DecodeString(f.Hex)
	if !bytes.Equal(w.Bytes(), want) {
		t.Fatalf("Writer 字节不一致\n got=%X\nwant=%X", w.Bytes(), want)
	}

	_, body, err := gameproto.ReadFrame(bytes.NewReader(w.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	r := gameproto.NewReader(body, gameproto.UTF8)
	if got := r.Str(); got != acc {
		t.Fatalf("账号解析 %q err=%v", got, r.Err())
	}
	if got := r.Str(); got != md5pwd {
		t.Fatalf("密码解析 %q err=%v", got, r.Err())
	}
	if r.Remaining() != 0 {
		t.Fatalf("应恰好读完，剩余 %d", r.Remaining())
	}
	if err := r.Err(); err != nil {
		t.Fatalf("Err=%v", err)
	}
}

func TestHandshakeFixturesUseInt32Body(t *testing.T) {
	f := fixtureByName(t, "c2s_connect_back")
	raw, _ := hex.DecodeString(f.Hex)
	if _, body, err := gameproto.ReadFrame(bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	} else if n := testsupport.UnpackInt32(body); n != 200 {
		t.Fatalf("握手回包应带 200，实际 %d", n)
	}

	// 版本号是字符串（101）
	vf := fixtureByName(t, "c2s_ver")
	vraw, _ := hex.DecodeString(vf.Hex)
	_, vbody, err := gameproto.ReadFrame(bytes.NewReader(vraw))
	if err != nil {
		t.Fatal(err)
	}
	vals, _, err := gameproto.Unpack([]any{""}, vbody, 0, vf.Coding)
	if err != nil {
		t.Fatal(err)
	}
	if vals[0].(string) != "58740022" {
		t.Fatalf("版本号解析错误: %#v", vals[0])
	}
}

// ---------------- 角色列表解析 ----------------

func TestParseRoleListFixture(t *testing.T) {
	f := fixtureByName(t, "s2c_query_role")
	raw, _ := hex.DecodeString(f.Hex)
	msgID, body, err := gameproto.ReadFrame(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if msgID != gameproto.MsgQueryRoleBack {
		t.Fatalf("msgid=%d 期望 %d", msgID, gameproto.MsgQueryRoleBack)
	}
	roles, err := gameproto.ParseRoleList(body, f.Coding)
	if err != nil {
		t.Fatalf("ParseRoleList 失败: %v", err)
	}
	if len(roles) != 1 {
		t.Fatalf("应解析出 1 个角色，实际 %d", len(roles))
	}
	role := roles[0]
	if role.Name != "测试甲" || role.Level != 21 || role.Turn != 0 || role.RoleID != 1001 {
		t.Fatalf("角色字段不符: %+v", role)
	}
}

func TestParseRoleListEmpty(t *testing.T) {
	f := fixtureByName(t, "s2c_query_role_empty")
	raw, _ := hex.DecodeString(f.Hex)
	_, body, err := gameproto.ReadFrame(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	roles, err := gameproto.ParseRoleList(body, f.Coding)
	if err != nil {
		t.Fatalf("无角色不应报错: %v", err)
	}
	if len(roles) != 0 {
		t.Fatalf("应解析出 0 个角色，实际 %d", len(roles))
	}
}

func TestParseRoleListTruncated(t *testing.T) {
	f := fixtureByName(t, "s2c_query_role")
	raw, _ := hex.DecodeString(f.Hex)
	_, body, err := gameproto.ReadFrame(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(body) < 16 {
		t.Fatalf("夹具太短，无法测截断: %d", len(body))
	}
	// 砍掉一半（角色字段写不全）→ 必须报错，不能静默返回半条数据
	if _, err := gameproto.ParseRoleList(body[:len(body)/2], f.Coding); err == nil {
		t.Fatal("截断的角色体必须报错")
	}
}

// ---------------- 编码：UTF-8 可用；GBK 明确报错（宁可不支持，也不要乱码）----------------

func TestGBKCodingFailsFastInsteadOfCorrupting(t *testing.T) {
	name := "测试甲"

	// UTF-8 正常
	utf, err := gameproto.Pack([]any{""}, []any{name}, gameproto.UTF8)
	if err != nil {
		t.Fatalf("UTF-8 打包不应失败: %v", err)
	}
	if vals, _, err := gameproto.Unpack([]any{""}, utf, 0, gameproto.UTF8); err != nil {
		t.Fatal(err)
	} else if vals[0].(string) != name {
		t.Fatalf("UTF-8 解码错误: %q", vals[0])
	}

	// GBK 明确报错（而不是静默乱码）：配错编码必须立刻看得见
	if _, err := gameproto.Pack([]any{""}, []any{name}, gameproto.GBK); err == nil {
		t.Fatal("GBK 打包应明确报错（本实现只内置 UTF-8）")
	} else if !errors.Is(err, gameproto.ErrUnsupportedCoding) {
		t.Fatalf("应为 ErrUnsupportedCoding，实际 %v", err)
	}
	if _, _, err := gameproto.Unpack([]any{""}, utf, 0, gameproto.GBK); err == nil {
		t.Fatal("GBK 解包应明确报错")
	}
	// 未知编码同样报错
	if _, err := gameproto.Pack([]any{""}, []any{name}, "BIG5"); err == nil {
		t.Fatal("未知编码应报错")
	}
}

func TestNormalizeCoding(t *testing.T) {
	for _, in := range []string{"", "utf-8", "UTF8", " utf-8 "} {
		if got := gameproto.NormalizeCoding(in); got != gameproto.UTF8 {
			t.Fatalf("NormalizeCoding(%q)=%q 期望 %q", in, got, gameproto.UTF8)
		}
	}
	for _, in := range []string{"gbk", "GB2312"} {
		if got := gameproto.NormalizeCoding(in); got != gameproto.GBK {
			t.Fatalf("NormalizeCoding(%q)=%q 期望 %q", in, got, gameproto.GBK)
		}
	}
}

// ---------------- 异常路径 ----------------

func TestUnpackTruncatedBody(t *testing.T) {
	// 只有一个 int32，却要按 [4, ""] 解 → 必须报错而不是返回空串
	if _, _, err := gameproto.Unpack([]any{4, ""}, []byte{1, 0, 0, 0}, 0, gameproto.UTF8); err == nil {
		t.Fatal("字段不足必须报错")
	}
	// 字符串声明长度超过剩余字节
	body := []byte{5, 0, 0, 0, 'a'}
	if _, _, err := gameproto.Unpack([]any{""}, body, 0, gameproto.UTF8); err == nil {
		t.Fatal("字符串超长必须报错")
	}
}

func TestUnpackUnknownFormat(t *testing.T) {
	if _, _, err := gameproto.Unpack([]any{3}, []byte{1, 2, 3}, 0, gameproto.UTF8); err == nil {
		t.Fatal("不支持的整型宽度必须报错")
	}
}

func TestPackRejectsMismatchedFields(t *testing.T) {
	if _, err := gameproto.Pack([]any{4, ""}, []any{int64(1)}, gameproto.UTF8); err == nil {
		t.Fatal("字段数与 fmt 不匹配必须报错")
	}
}

func TestReadFrameTruncatedStream(t *testing.T) {
	// 头部声明 100 字节数据体，实际只给 3 字节
	raw := []byte{0, 0, 0, 0, 100, 0, 0, 0, 1, 2, 3}
	_, _, err := gameproto.ReadFrame(bytes.NewReader(raw))
	if err == nil {
		t.Fatal("半包必须报错（不能死等/返回残缺数据）")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) && !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("应为 EOF 类错误，实际 %v", err)
	}
}

func TestReaderErrorIsSticky(t *testing.T) {
	r := gameproto.NewReader([]byte{1, 2}, gameproto.UTF8)
	_ = r.Int32() // 长度不足 → 记错
	if r.Err() == nil {
		t.Fatal("读失败后 Err() 必须非空")
	}
	if got := r.Str(); got != "" {
		t.Fatalf("出错后应返回零值，实际 %q", got)
	}
	if r.Err() == nil {
		t.Fatal("出错后继续读必须继续报错（错误是粘性的）")
	}
}

func sameVal(a, b any) bool {
	switch av := a.(type) {
	case string:
		bs, ok := b.(string)
		return ok && av == bs
	case int64:
		switch bv := b.(type) {
		case int64:
			return av == bv
		case int:
			return av == int64(bv)
		}
	case int:
		return sameVal(int64(av), b)
	}
	return false
}
