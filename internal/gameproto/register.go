package gameproto

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

// 注册协议消息号（与客户端 protocol.py 一致）。
const (
	MsgGetRegCode   int32 = 106 // 获取注册验证码（空体）
	MsgRegCodeBack  int32 = 702 // 验证码返回 [验证码串]
	MsgRegister     int32 = 104 // 注册 [验证码, 账号, 密码, 确认密码, 激活码, 姓名, 身份证, QQ]
	MsgRegisterBack int32 = 700 // 注册结果 [db_ret, err_item, errid]
)

// RegisterFields 104 注册包的字段数（固定 8 个）。
const RegisterFields = 8

// 注册结果码（700 的 errid；与客户端 REGISTER_ERROR_* 同源）。
const (
	RegErrOK         int32 = 110 // 注册成功
	RegErrNameExist  int32 = 111 // 用户名已存在
	RegErrServerFail int32 = 112 // 服务端处理失败（多为同 IP 注册过频触发风控，适合退避重试）
	RegErrBadCode    int32 = 116 // 验证码出错
)

// registerErrDesc 注册结果码 → 人话（只列常见的，未收录给通用说明）。
var registerErrDesc = map[int32]string{
	110: "注册成功",
	111: "用户名已存在",
	112: "服务端处理失败（注册信息有误或同 IP 注册过频触发风控）",
	113: "注册成功但激活码无效，账号未激活",
	114: "注册成功且已激活",
	115: "注册成功但未激活",
	116: "验证码出错",
}

// RegisterErrDesc 把 errid 翻成可读说明。
func RegisterErrDesc(errID int32) string {
	if d, ok := registerErrDesc[errID]; ok {
		return d
	}
	return fmt.Sprintf("未知错误（errid=%d）", errID)
}

// RegisterRetryable 该错误是否适合"退避后重试"（风控类）。
func RegisterRetryable(errID int32) bool { return errID == RegErrServerFail }

// RegisterResult 注册结果（700 的 [db_ret, err_item, errid]）。
type RegisterResult struct {
	DbRet   int32 `json:"db_ret"`
	ErrItem int32 `json:"err_item"`
	ErrID   int32 `json:"err_id"`
}

// OK db_ret==0 即注册成功。
func (r RegisterResult) OK() bool { return r.DbRet == 0 }

// BuildRegisterFrame 构造 104 注册帧（字段顺序固定，全部按 coding 编码）。
func BuildRegisterFrame(fields []string, coding string) ([]byte, error) {
	if len(fields) != RegisterFields {
		return nil, fmt.Errorf("注册包需要 %d 个字段，实际 %d", RegisterFields, len(fields))
	}
	w := NewWriter(MsgRegister, coding)
	for _, f := range fields {
		w.Str(f)
	}
	if err := w.Err(); err != nil {
		return nil, err
	}
	return w.Bytes(), nil
}

// ParseRegisterResult 解析 700 数据体。
func ParseRegisterResult(body []byte, coding string) (RegisterResult, error) {
	vals, _, err := Unpack([]any{4, 4, 4}, body, 0, coding)
	if err != nil {
		return RegisterResult{}, err
	}
	return RegisterResult{
		DbRet:   int32(vals[0].(int64)),
		ErrItem: int32(vals[1].(int64)),
		ErrID:   int32(vals[2].(int64)),
	}, nil
}

// ---------------------------------------------------------------- 连接级（注册与探测共用）

// Handshake 完成连接握手与版本验证：100 → (200 回 255[200]) → (255 发 101[版本]) → 400。
// 收到无关帧会忽略；超时/失败返回错误（调用方据此给出"握手超时/版本验证失败"）。
func Handshake(conn net.Conn, version, coding string) error {
	if err := WriteFrame(conn, MsgConnect, nil, nil, coding); err != nil {
		return fmt.Errorf("发送握手包失败: %w", err)
	}
	for {
		msgID, body, err := ReadFrame(conn)
		if err != nil {
			return err
		}
		switch msgID {
		case MsgConnectQuery:
			if err := WriteFrame(conn, MsgConnectBack, []any{4}, []any{int64(200)}, coding); err != nil {
				return fmt.Errorf("回复握手包失败: %w", err)
			}
		case MsgConnectBack:
			if err := WriteFrame(conn, MsgVer, []any{""}, []any{version}, coding); err != nil {
				return fmt.Errorf("发送版本包失败: %w", err)
			}
		case MsgVerBack:
			vals, _, err := Unpack([]any{4, ""}, body, 0, coding)
			if err != nil {
				return fmt.Errorf("解析版本返回失败: %w", err)
			}
			if ret := vals[0].(int64); ret != 0 {
				return fmt.Errorf("版本验证失败 retcode=%d", ret)
			}
			return nil
		default:
			// 无关帧忽略
		}
	}
}

// ReadUntil 读到指定消息号为止（忽略其它帧），返回其数据体。
func ReadUntil(conn net.Conn, want int32) ([]byte, error) {
	for {
		msgID, body, err := ReadFrame(conn)
		if err != nil {
			return nil, err
		}
		if msgID == want {
			return body, nil
		}
	}
}

// IsTimeout 错误是否属于"超时"（含 deadline / ctx 取消导致的立即超时）。
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, os.ErrDeadlineExceeded)
}

// SetDeadlineSoon 立刻让连接上的阻塞读写返回（用于 ctx 取消时解阻塞）。
func SetDeadlineSoon(conn net.Conn) { _ = conn.SetDeadline(time.Now()) }
