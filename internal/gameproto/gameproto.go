// Package gameproto 游戏服协议帧编解码（中控**直接连游戏服**时用，不经过机器人进程）。
//
// 帧格式（net_packet，无加密；与参考实现 register_robot.py 完全一致）：
//
//	[type int32 LE][pblen int32 LE][body(pblen 字节)]
//
// body 字段：1/2/4/8 = 有符号整数 LE；字符串 = [len int32 LE][编码后的字节]。
//
// 字符串编码（PROTOCOL_CODING）必须与目标区一致：本实现只内置 **UTF-8**
// （当前环境统一 UTF-8）；配到 GBK 的区会**明确报错**而不是乱码 —— 静默乱码会让
// 「账号不存在」这类判断全错，比暂时不支持更危险。需要 GBK 时在此处接入编码表即可。
package gameproto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

// 消息号（与客户端 xiyou3-client/script/protocol.py 一致）。
const (
	MsgConnect       int32 = 100   // 握手
	MsgConnectQuery  int32 = 200   // 服务端连接查询
	MsgConnectBack   int32 = 255   // 握手返回（双向，带 [200]）
	MsgVer           int32 = 101   // 版本验证 [版本号]
	MsgVerBack       int32 = 400   // 版本返回 [retcode, msg]
	MsgLogin         int32 = 102   // 登录 [账号, md5(密码)]
	MsgAckAccount    int32 = 300   // 登录验证返回 [retcode, msg]
	MsgQueryRole     int32 = 1000  // 查询角色 [0]
	MsgQueryRoleBack int32 = 90132 // 角色列表 [数量, (角色…)]
)

// 登录验证返回码（S2C_ACK_ACCOUNT=300 的 retcode，与 CLIENT_LOGIN_RESPONSE_CODE_* 一致）。
const (
	LoginOK             int32 = 0   // 登录成功
	LoginInvalidAccount int32 = 100 // 账号不存在
	LoginInvalidPassword int32 = 200 // 密码错误（账号存在）
	LoginFreezed        int32 = 300 // 账号被冻结（账号存在）
)

// 字符串编码。
const (
	UTF8 = "UTF-8"
	GBK  = "GBK"
)

// 帧大小限制。
const (
	HeaderSize   = 8
	MaxBodyBytes = 4 << 20 // 4MB：远大于正常报文，防异常长度打爆内存
)

// ErrUnsupportedCoding 该编码暂不支持（当前实现只含 UTF-8 表）。
var ErrUnsupportedCoding = errors.New("不支持的字符串编码")

// NormalizeCoding 归一化编码名（空 / utf8 / utf-8 / gbk / gb2312 …）。
func NormalizeCoding(c string) string {
	switch strings.ToLower(strings.TrimSpace(c)) {
	case "", "utf-8", "utf8":
		return UTF8
	case "gbk", "gb2312", "gb18030":
		return GBK
	default:
		return strings.TrimSpace(c)
	}
}

func encodeStr(s, coding string) ([]byte, error) {
	switch NormalizeCoding(coding) {
	case UTF8:
		return []byte(s), nil
	case GBK:
		return nil, fmt.Errorf("%w: GBK（本实现为纯标准库，只支持 UTF-8；该区编码需改为 UTF-8 或补编码表）", ErrUnsupportedCoding)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedCoding, coding)
	}
}

func decodeStr(b []byte, coding string) (string, error) {
	switch NormalizeCoding(coding) {
	case UTF8:
		return string(b), nil
	case GBK:
		return "", fmt.Errorf("%w: GBK（本实现为纯标准库，只支持 UTF-8；该区编码需改为 UTF-8 或补编码表）", ErrUnsupportedCoding)
	default:
		return "", fmt.Errorf("%w: %q", ErrUnsupportedCoding, coding)
	}
}

// ---------------------------------------------------------------- 打包 / 解包

// Pack 按 fmtv 打包数据体。fmtv 元素：int(1/2/4/8) 表示整数宽度；"" 表示字符串。
// vals 必须与 fmtv 等长（整数用 int/int64 等有符号整型，字符串用 string）。
func Pack(fmtv []any, vals []any, coding string) ([]byte, error) {
	if len(fmtv) != len(vals) {
		return nil, fmt.Errorf("字段数不匹配: fmt=%d vals=%d", len(fmtv), len(vals))
	}
	var buf bytes.Buffer
	for i, f := range fmtv {
		if err := packField(&buf, f, vals[i], coding); err != nil {
			return nil, fmt.Errorf("第 %d 个字段: %w", i, err)
		}
	}
	return buf.Bytes(), nil
}

func packField(buf *bytes.Buffer, kind any, val any, coding string) error {
	switch k := kind.(type) {
	case string:
		if k != "" {
			return fmt.Errorf("未知字段格式 %q（只支持 \"\" 表示字符串）", k)
		}
		s, ok := val.(string)
		if !ok {
			return fmt.Errorf("字符串字段需要 string，实际 %T", val)
		}
		raw, err := encodeStr(s, coding)
		if err != nil {
			return err
		}
		var h [4]byte
		binary.LittleEndian.PutUint32(h[:], uint32(len(raw)))
		buf.Write(h[:])
		buf.Write(raw)
		return nil
	case int:
		n, err := asInt64(val)
		if err != nil {
			return err
		}
		return writeInt(buf, k, n)
	default:
		return fmt.Errorf("未知字段格式 %T（整数宽度用 int，字符串用 \"\"）", kind)
	}
}

func writeInt(buf *bytes.Buffer, width int, n int64) error {
	switch width {
	case 1:
		if n < math.MinInt8 || n > math.MaxInt8 {
			return fmt.Errorf("值 %d 超出 int8 范围", n)
		}
		buf.WriteByte(byte(int8(n)))
	case 2:
		if n < math.MinInt16 || n > math.MaxInt16 {
			return fmt.Errorf("值 %d 超出 int16 范围", n)
		}
		var b [2]byte
		binary.LittleEndian.PutUint16(b[:], uint16(int16(n)))
		buf.Write(b[:])
	case 4:
		if n < math.MinInt32 || n > math.MaxInt32 {
			return fmt.Errorf("值 %d 超出 int32 范围", n)
		}
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(int32(n)))
		buf.Write(b[:])
	case 8:
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], uint64(n))
		buf.Write(b[:])
	default:
		return fmt.Errorf("不支持的整型宽度 %d（只支持 1/2/4/8）", width)
	}
	return nil
}

// Unpack 按 fmtv 解包 body[off:]，返回字段列表与新偏移。
// 整数一律返回 int64，字符串返回 string（编码不符/字节不足 → 报错）。
func Unpack(fmtv []any, body []byte, off int, coding string) ([]any, int, error) {
	out := make([]any, 0, len(fmtv))
	for _, f := range fmtv {
		switch k := f.(type) {
		case string:
			if k != "" {
				return nil, off, fmt.Errorf("未知字段格式 %q", k)
			}
			if off+4 > len(body) {
				return nil, off, fmt.Errorf("字符串长度字段越界: off=%d len=%d", off, len(body))
			}
			n := int(int32(binary.LittleEndian.Uint32(body[off:])))
			off += 4
			if n < 0 || off+n > len(body) {
				return nil, off, fmt.Errorf("字符串内容越界: 声明 %d 字节, 剩余 %d", n, len(body)-off)
			}
			s, err := decodeStr(body[off:off+n], coding)
			if err != nil {
				return nil, off, err
			}
			off += n
			out = append(out, s)
		case int:
			if off+k > len(body) {
				return nil, off, fmt.Errorf("整数字段越界: 需要 %d 字节, 剩余 %d", k, len(body)-off)
			}
			var v int64
			switch k {
			case 1:
				v = int64(int8(body[off]))
			case 2:
				v = int64(int16(binary.LittleEndian.Uint16(body[off:])))
			case 4:
				v = int64(int32(binary.LittleEndian.Uint32(body[off:])))
			case 8:
				v = int64(binary.LittleEndian.Uint64(body[off:]))
			default:
				return nil, off, fmt.Errorf("不支持的整型宽度 %d（只支持 1/2/4/8）", k)
			}
			off += k
			out = append(out, v)
		default:
			return nil, off, fmt.Errorf("未知字段格式 %T", f)
		}
	}
	return out, off, nil
}

// ---------------------------------------------------------------- 组帧 / 收帧

// PackFrameRaw 用现成的 body 组帧（变长体，如角色列表）。
func PackFrameRaw(msgID int32, body []byte) []byte {
	out := make([]byte, HeaderSize+len(body))
	binary.LittleEndian.PutUint32(out[0:4], uint32(msgID))
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(body)))
	copy(out[HeaderSize:], body)
	return out
}

// PackFrame 组完整帧。
func PackFrame(msgID int32, fmtv []any, vals []any, coding string) ([]byte, error) {
	body, err := Pack(fmtv, vals, coding)
	if err != nil {
		return nil, err
	}
	return PackFrameRaw(msgID, body), nil
}

// WriteFrame 写一帧。
func WriteFrame(w io.Writer, msgID int32, fmtv []any, vals []any, coding string) error {
	raw, err := PackFrame(msgID, fmtv, vals, coding)
	if err != nil {
		return err
	}
	_, err = w.Write(raw)
	return err
}

// ReadFrame 读一帧；返回消息号与数据体。
func ReadFrame(r io.Reader) (int32, []byte, error) {
	var hdr [HeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	msgID := int32(binary.LittleEndian.Uint32(hdr[0:4]))
	pblen := int32(binary.LittleEndian.Uint32(hdr[4:8]))
	if pblen < 0 || pblen > MaxBodyBytes {
		return 0, nil, fmt.Errorf("帧长度非法: msgid=%d pblen=%d", msgID, pblen)
	}
	body := make([]byte, pblen)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return msgID, body, nil
}

// ---------------------------------------------------------------- 流式读写

// Writer 顺序写字段（真实代码用；出错后各方法变 no-op，由 Err() 统一取出）。
type Writer struct {
	msgID  int32
	coding string
	body   bytes.Buffer
	err    error
}

// NewWriter 创建写端；msgID=0 表示只写 body（Bytes 时仍会组帧，见 Body()）。
func NewWriter(msgID int32, coding string) *Writer {
	return &Writer{msgID: msgID, coding: coding}
}

// Str 写字符串字段。
func (w *Writer) Str(s string) *Writer {
	if w.err != nil {
		return w
	}
	raw, err := encodeStr(s, w.coding)
	if err != nil {
		w.err = err
		return w
	}
	var h [4]byte
	binary.LittleEndian.PutUint32(h[:], uint32(len(raw)))
	w.body.Write(h[:])
	w.body.Write(raw)
	return w
}

// Int8 写 int8 字段。
func (w *Writer) Int8(v int8) *Writer { return w.intN(1, int64(v)) }

// Int16 写 int16 字段。
func (w *Writer) Int16(v int16) *Writer { return w.intN(2, int64(v)) }

// Int32 写 int32 字段。
func (w *Writer) Int32(v int32) *Writer { return w.intN(4, int64(v)) }

// Int64 写 int64 字段。
func (w *Writer) Int64(v int64) *Writer { return w.intN(8, v) }

func (w *Writer) intN(width int, v int64) *Writer {
	if w.err != nil {
		return w
	}
	w.err = writeInt(&w.body, width, v)
	return w
}

// Body 返回数据体（不含帧头）。
func (w *Writer) Body() []byte { return w.body.Bytes() }

// Bytes 返回完整帧（帧头 = 创建时的 msgID）。
func (w *Writer) Bytes() []byte { return PackFrameRaw(w.msgID, w.body.Bytes()) }

// Err 返回首个错误（nil = 全部成功）。
func (w *Writer) Err() error { return w.err }

// Reader 顺序读字段；错误是粘性的（出错后所有读返回零值 + 同一个错误）。
type Reader struct {
	buf    []byte
	off    int
	coding string
	err    error
}

// NewReader 创建读端。
func NewReader(body []byte, coding string) *Reader {
	return &Reader{buf: body, coding: coding}
}

// Remaining 剩余字节数。
func (r *Reader) Remaining() int { return len(r.buf) - r.off }

// Offset 当前偏移。
func (r *Reader) Offset() int { return r.off }

// Err 返回首个错误。
func (r *Reader) Err() error { return r.err }

// Skip 跳过 n 字节（用于中控不关心的字段，如角色外观）。
func (r *Reader) Skip(n int) *Reader {
	if r.err != nil {
		return r
	}
	if n < 0 || r.off+n > len(r.buf) {
		r.err = fmt.Errorf("跳过 %d 字节越界（剩余 %d）", n, r.Remaining())
		return r
	}
	r.off += n
	return r
}

// Int8 读 int8。
func (r *Reader) Int8() int8 {
	v := int8(0)
	r.readInt(1, func(n int64) { v = int8(n) })
	return v
}

// Int16 读 int16。
func (r *Reader) Int16() int16 {
	v := int16(0)
	r.readInt(2, func(n int64) { v = int16(n) })
	return v
}

// Int32 读 int32。
func (r *Reader) Int32() int32 {
	v := int32(0)
	r.readInt(4, func(n int64) { v = int32(n) })
	return v
}

// Int64 读 int64。
func (r *Reader) Int64() int64 {
	v := int64(0)
	r.readInt(8, func(n int64) { v = n })
	return v
}

func (r *Reader) readInt(width int, set func(int64)) {
	if r.err != nil {
		return
	}
	vals, off, err := Unpack([]any{width}, r.buf, r.off, r.coding)
	if err != nil {
		r.err = err
		return
	}
	r.off = off
	set(vals[0].(int64))
}

// Str 读字符串。
func (r *Reader) Str() string {
	if r.err != nil {
		return ""
	}
	vals, off, err := Unpack([]any{""}, r.buf, r.off, r.coding)
	if err != nil {
		r.err = err
		return ""
	}
	r.off = off
	return vals[0].(string)
}

// ---------------------------------------------------------------- 角色列表

// Role 账号下的一个角色（90132 里的首个角色信息）。
type Role struct {
	RoleID    int32  `json:"role_id"`
	RoleIndex int16  `json:"role_index"`
	Name      string `json:"name"`
	Level     int16  `json:"level"`
	Turn      int8   `json:"turn"`
}

// ParseRoleList 解析角色列表体：
//
//	数量 int32，随后每个角色 = role_id(4), role_index(2), name(""), level(2), turn(1),
//	                    外观 4×int32, contour(数量 int32 + n×int32)
//
// 数量 <= 0 → 返回空列表（无角色不算错误）。
func ParseRoleList(body []byte, coding string) ([]Role, error) {
	r := NewReader(body, coding)
	n := r.Int32()
	if err := r.Err(); err != nil {
		return nil, err
	}
	if n <= 0 {
		return nil, nil
	}
	out := make([]Role, 0, n)
	for i := int32(0); i < n; i++ {
		var role Role
		role.RoleID = r.Int32()
		role.RoleIndex = r.Int16()
		role.Name = r.Str()
		role.Level = r.Int16()
		role.Turn = r.Int8()
		r.Skip(4 * 4) // 发型/发色/脸型/脸纹
		nContour := r.Int32()
		if err := r.Err(); err != nil {
			return nil, err
		}
		if nContour < 0 {
			return nil, fmt.Errorf("contour 数量非法: %d", nContour)
		}
		r.Skip(int(nContour) * 4)
		if err := r.Err(); err != nil {
			return nil, err
		}
		out = append(out, role)
	}
	return out, nil
}

// BuildRoleList 反向构造角色列表体（测试夹具 / 将来建号复用；与 ParseRoleList 成对）。
func BuildRoleList(roles []Role, coding string) ([]byte, error) {
	w := NewWriter(0, coding)
	w.Int32(int32(len(roles)))
	for _, role := range roles {
		w.Int32(role.RoleID).Int16(role.RoleIndex).Str(role.Name).Int16(role.Level).Int8(role.Turn)
		w.Int32(1).Int32(2).Int32(3).Int32(4) // 外观占位（中控不关心，但要占位）
		w.Int32(0)                            // contour 数量
	}
	if err := w.Err(); err != nil {
		return nil, err
	}
	return w.Body(), nil
}

// BuildRoleFrame 反向构造角色列表**完整帧**（MsgQueryRoleBack）。
func BuildRoleFrame(roles []Role, coding string) ([]byte, error) {
	body, err := BuildRoleList(roles, coding)
	if err != nil {
		return nil, err
	}
	return PackFrameRaw(MsgQueryRoleBack, body), nil
}

// asInt64 有符号整型 → int64（其余类型报错）。
func asInt64(v any) (int64, error) {
	switch t := v.(type) {
	case int:
		return int64(t), nil
	case int8:
		return int64(t), nil
	case int16:
		return int64(t), nil
	case int32:
		return int64(t), nil
	case int64:
		return t, nil
	case uint8:
		return int64(t), nil
	case uint16:
		return int64(t), nil
	case uint32:
		return int64(t), nil
	default:
		return 0, fmt.Errorf("整数字段需要整型，实际 %T", v)
	}
}
