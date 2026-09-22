// Package accountcreate 建号：中控**直接连游戏服**走注册协议（106 取验证码 → 104 注册 → 700 判结果）。
//
// 与"可用性验证"(`internal/accountverify`) 的区别：验证是拿库里的密码登录探活；建号是新建账号，
// 密码**由本包随机生成**，注册成功后由调用方写入账号库 —— 之后就用库里的密码登录
// （**没有"统一密码"这回事**，每个号一个随机密码）。
//
// 参考实现（Python 版 register_robot.py）只作参考，本包按其协议事实重新实现。
package accountcreate

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"

	"zyctrlcenter/internal/gameproto"
)

const (
	// DefaultTimeout 单个账号建号总超时（注册要跑两轮交互 + 服务端落库，比探测慢）。
	DefaultTimeout = 12 * time.Second
	// DefaultVersion 默认客户端协议版本（101 版本验证用）。
	DefaultVersion = "58740022"
	// MinPasswordLen 随机密码最短长度（服务端要求 6~20 位，这里取更安全的下限）。
	MinPasswordLen = 8
	// DefaultPasswordLen 默认随机密码长度。
	DefaultPasswordLen = 16
)

// passwordAlphabet 随机密码字符集：大小写 + 数字，**剔除易混字符**（0/O/1/l/I）。
const passwordAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// Options 建号选项。
type Options struct {
	Timeout  time.Duration // 单账号总超时（默认 12s）
	Version  string        // 客户端版本号（默认 DefaultVersion）
	Coding   string        // 字符串编码（默认 UTF-8；GBK 明确报错）
	AgentKey string        // 可选"密匙"：写入注册包"姓名"位（落库用于区分机器人）
}

// DefaultOptions 默认选项。
func DefaultOptions() Options {
	return Options{Timeout: DefaultTimeout, Version: DefaultVersion, Coding: gameproto.UTF8}
}

func (o Options) withDefaults() Options {
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if strings.TrimSpace(o.Version) == "" {
		o.Version = DefaultVersion
	}
	o.Coding = gameproto.NormalizeCoding(o.Coding)
	return o
}

// Result 单个账号的建号结果。
type Result struct {
	Account   string `json:"account"`
	Zone      string `json:"zone"`
	OK        bool   `json:"ok"`
	DbRet     int32  `json:"db_ret"`
	ErrItem   int32  `json:"err_item"`
	ErrID     int32  `json:"err_id"`
	Msg       string `json:"msg"`
	Retryable bool   `json:"retryable,omitempty"`
	Password  string `json:"password,omitempty"` // 新建成功时回带明文（供写入账号库）
	ElapsedMs int64  `json:"elapsed_ms"`
	At        int64  `json:"at"`
	Err       string `json:"err,omitempty"`
}

// NewPassword 生成随机密码（剔除易混字符；n < MinPasswordLen 时报错）。
func NewPassword(n int) (string, error) {
	if n < MinPasswordLen {
		return "", fmt.Errorf("随机密码至少 %d 位（要求 %d）", MinPasswordLen, n)
	}
	max := big.NewInt(int64(len(passwordAlphabet)))
	var b strings.Builder
	b.Grow(n)
	for i := 0; i < n; i++ {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("随机源不可用: %w", err)
		}
		b.WriteByte(passwordAlphabet[idx.Int64()])
	}
	return b.String(), nil
}

// Register 建号：握手 → 106 取验证码 → 104 注册（密码用调用方给的随机密码）→ 700 判结果。
//
// 成功返回 OK=true 且带 Password（调用方据此写库）。ctx 取消会立即中断。
func Register(ctx context.Context, addr, account, password string, opt Options) Result {
	opt = opt.withDefaults()
	start := time.Now()
	res := Result{Account: account, Zone: addr, At: start.Unix(), DbRet: -1}
	finish := func(err error) Result {
		if err != nil {
			res.Err = err.Error()
			if res.Msg == "" {
				res.Msg = err.Error()
			}
		}
		res.ElapsedMs = time.Since(start).Milliseconds()
		return res
	}
	if strings.TrimSpace(account) == "" {
		return finish(fmt.Errorf("账号不能为空"))
	}
	if strings.TrimSpace(password) == "" {
		return finish(fmt.Errorf("密码不能为空"))
	}
	// 编码不支持时立刻失败（连上再乱码会写进服务端库，后果不可逆）
	if _, err := gameproto.Pack([]any{""}, []any{account}, opt.Coding); err != nil {
		return finish(fmt.Errorf("编码检查失败: %w", err))
	}

	dialCtx, cancel := context.WithTimeout(ctx, opt.Timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", addr)
	if err != nil {
		return finish(fmt.Errorf("连接游戏服 %s 失败: %w", addr, err))
	}
	defer conn.Close()

	deadline := time.Now().Add(opt.Timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-ctx.Done():
			gameproto.SetDeadlineSoon(conn)
		case <-stopWatch:
		}
	}()

	// 1) 握手 + 版本
	if err := gameproto.Handshake(conn, opt.Version, opt.Coding); err != nil {
		return finish(waitErr(ctx, err, "握手/版本验证"))
	}

	// 2) 取注册验证码（服务端返回汉字串，必须原样回传）
	if err := gameproto.WriteFrame(conn, gameproto.MsgGetRegCode, nil, nil, opt.Coding); err != nil {
		return finish(fmt.Errorf("请求注册验证码失败: %w", err))
	}
	body, err := gameproto.ReadUntil(conn, gameproto.MsgRegCodeBack)
	if err != nil {
		return finish(waitErr(ctx, err, "注册验证码(702)"))
	}
	code := ""
	if v, _, err := gameproto.Unpack([]any{""}, body, 0, opt.Coding); err != nil {
		return finish(fmt.Errorf("解析验证码失败: %w", err))
	} else {
		code, _ = v[0].(string)
	}
	if strings.TrimSpace(code) == "" {
		return finish(fmt.Errorf("服务端未返回注册验证码"))
	}

	// 3) 注册 104：[验证码, 账号, 密码, 确认密码, 激活码, 姓名(可放密匙), 身份证, QQ]
	raw, err := gameproto.BuildRegisterFrame([]string{
		code, account, password, password, "", opt.AgentKey, "", "",
	}, opt.Coding)
	if err != nil {
		return finish(err)
	}
	if _, err := conn.Write(raw); err != nil {
		return finish(fmt.Errorf("发送注册包失败: %w", err))
	}

	// 4) 判结果 700
	body, err = gameproto.ReadUntil(conn, gameproto.MsgRegisterBack)
	if err != nil {
		return finish(waitErr(ctx, err, "注册结果(700)"))
	}
	rs, err := gameproto.ParseRegisterResult(body, opt.Coding)
	if err != nil {
		return finish(fmt.Errorf("解析注册结果失败: %w", err))
	}
	res.DbRet, res.ErrItem, res.ErrID = rs.DbRet, rs.ErrItem, rs.ErrID
	res.Msg = gameproto.RegisterErrDesc(rs.ErrID)
	res.Retryable = !rs.OK() && gameproto.RegisterRetryable(rs.ErrID)
	if rs.OK() {
		res.OK = true
		res.Password = password // 回带明文：调用方要写进账号库
	}
	return finish(nil)
}

// waitErr 把读失败归因成"超时"或"IO 失败"。
func waitErr(ctx context.Context, err error, what string) error {
	if ctx.Err() != nil {
		return fmt.Errorf("等待%s超时（%v）", what, ctx.Err())
	}
	if gameproto.IsTimeout(err) {
		return fmt.Errorf("等待%s超时（%v）", what, err)
	}
	return fmt.Errorf("读取%s失败: %w", what, err)
}
