// Package accountverify 账号可用性探测：中控**直接连游戏服**跑一遍登录协议，
// 判断某个账号在该区是否存在/可用（不真正进入游戏，无副作用）。
//
// 链路（与参考实现 register_robot.query_account 一致）：
//
//	TCP 连接
//	  → 100 握手（收到 200 回 255[200]；收到 255 发 101[版本号]；收到 400 表示版本通过）
//	  → 102 登录 [账号, md5(明文密码)]
//	  → 300 登录验证返回 [retcode, msg]
//	      0=成功(存在且可用) / 100=账号不存在 / 200=密码错误(存在) / 300=冻结(存在)
//	  →（仅 retcode=0 且开启 QueryRole）1000 查角色 → 90132 [角色…]（拿角色名/等级）
//
// 语义约定：
//   - retcode=100 → Exists=false（该区没有这个号）
//   - retcode=200/300 → Exists=true, Usable=false（**存在但不可用**：密码错/冻结的号
//     如果算可用，会被定时上线反复拉起，参考项目踩过这个坑）
//   - 网络/超时/编码错误 → Err 非空，且**绝不判为可用**
//   - 每个账号一条独立连接（互不影响），批量探测按并发上限并行
package accountverify

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"zyctrlcenter/internal/gameproto"
)

// DefaultVersion 默认客户端协议版本（101 版本验证用；与参考实现默认值一致）。
const DefaultVersion = "58740022"

// DefaultTimeout 单个账号的探测总超时。
const DefaultTimeout = 8 * time.Second

// 批量探测的并发上限。
//
// 2026-09-22 修复：旧版是硬编码 `const MaxConcurrency = 16`，面板里填 500 也只会跑 16
// （用户实测："填入500的并发实际只有16"）。改为：
//   - 默认 512（面板填几百能真正生效）；
//   - 环境变量 CTRL_VERIFY_MAX_CONCURRENCY 可调低/调高（受硬顶限制）；
//   - 硬顶 1024 防呆（异常输入不至于打爆本机/游戏服）。
//
// 注意：并发越高越容易触发游戏服"同 IP 频控"（通知码 112），失败率上升时请调低。
const (
	defaultMaxConcurrency = 512
	hardMaxConcurrency    = 1024
)

// MaxConcurrency 当前允许的最大验证并发（每次调用读环境变量，便于不重启调整）。
func MaxConcurrency() int {
	n := defaultMaxConcurrency
	if v := strings.TrimSpace(os.Getenv("CTRL_VERIFY_MAX_CONCURRENCY")); v != "" {
		if x, err := strconv.Atoi(v); err == nil && x > 0 {
			n = x
		}
	}
	if n > hardMaxConcurrency {
		n = hardMaxConcurrency
	}
	return n
}

// Options 探测选项（零值可用：withDefaults 会补齐）。
type Options struct {
	Timeout   time.Duration // 单个账号总超时（默认 8s）
	Version   string        // 客户端版本号（默认 DefaultVersion）
	Coding    string        // 字符串编码（默认 UTF-8；GBK 目前会明确报错）
	QueryRole bool          // 登录成功后是否查角色（默认由 DefaultOptions 打开）
}

// DefaultOptions 默认选项（QueryRole=true）。
func DefaultOptions() Options {
	return Options{Timeout: DefaultTimeout, Version: DefaultVersion,
		Coding: gameproto.UTF8, QueryRole: true}
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

// Target 一个待探测账号（必须带密码：登录协议需要）。
type Target struct {
	Account  string `json:"account"`
	Password string `json:"password"`
}

// Result 单个账号的探测结果。
type Result struct {
	Account   string `json:"account"`
	Zone      string `json:"zone"` // 目标游戏服 "host:port"
	Exists    bool   `json:"exists"`
	Usable    bool   `json:"usable"`
	RetCode   int32  `json:"ret_code"`
	Msg       string `json:"msg"`
	RoleName  string `json:"role_name,omitempty"`
	Level     int    `json:"level,omitempty"`
	Turn      int    `json:"turn,omitempty"`
	ElapsedMs int64  `json:"elapsed_ms"`
	At        int64  `json:"at"` // 探测时间（unix 秒）
	Err       string `json:"err,omitempty"`
}

// Probe 探测一个账号（阻塞直到出结果/超时）。ctx 取消会立即中断。
func Probe(ctx context.Context, addr, account, password string, opt Options) Result {
	opt = opt.withDefaults()
	start := time.Now()
	res := Result{Account: account, Zone: addr, At: start.Unix(), RetCode: -1}
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

	// 编码不支持时立刻失败：连上再乱码会让"是否存在"的判断全错
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

	// 总超时同时受 ctx 约束；ctx 取消时立刻解除阻塞
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
			_ = conn.SetDeadline(time.Now()) // Read/Write 立刻返回超时错误
		case <-stopWatch:
		}
	}()

	// ---- 1) 握手 + 版本验证 ----
	if err := gameproto.WriteFrame(conn, gameproto.MsgConnect, nil, nil, opt.Coding); err != nil {
		return finish(fmt.Errorf("发送握手包失败: %w", err))
	}
	for {
		msgID, body, err := gameproto.ReadFrame(conn)
		if err != nil {
			return finish(waitErr(ctx, err, "握手/版本验证应答"))
		}
		switch msgID {
		case gameproto.MsgConnectQuery:
			if err := gameproto.WriteFrame(conn, gameproto.MsgConnectBack,
				[]any{4}, []any{int64(200)}, opt.Coding); err != nil {
				return finish(fmt.Errorf("回复握手包失败: %w", err))
			}
		case gameproto.MsgConnectBack:
			if err := gameproto.WriteFrame(conn, gameproto.MsgVer,
				[]any{""}, []any{opt.Version}, opt.Coding); err != nil {
				return finish(fmt.Errorf("发送版本包失败: %w", err))
			}
		case gameproto.MsgVerBack:
			vals, _, err := gameproto.Unpack([]any{4, ""}, body, 0, opt.Coding)
			if err != nil {
				return finish(fmt.Errorf("解析版本返回失败: %w", err))
			}
			if ret := vals[0].(int64); ret != 0 {
				return finish(fmt.Errorf("版本验证失败 retcode=%d", ret))
			}
			goto login
		default:
			// 无关帧忽略（服务端可能先推别的）
		}
	}

login:
	// ---- 2) 登录 102 → 300 ----
	sum := md5.Sum([]byte(password))
	if err := gameproto.WriteFrame(conn, gameproto.MsgLogin, []any{"", ""},
		[]any{account, hex.EncodeToString(sum[:])}, opt.Coding); err != nil {
		return finish(fmt.Errorf("发送登录包失败: %w", err))
	}

	var retCode int64
	var serverMsg string
	for {
		msgID, body, err := gameproto.ReadFrame(conn)
		if err != nil {
			return finish(waitErr(ctx, err, "登录验证结果(300)"))
		}
		if msgID != gameproto.MsgAckAccount {
			continue
		}
		vals, _, err := gameproto.Unpack([]any{4, ""}, body, 0, opt.Coding)
		if err != nil {
			return finish(fmt.Errorf("解析登录验证结果失败: %w", err))
		}
		retCode, _ = vals[0].(int64)
		serverMsg, _ = vals[1].(string)
		break
	}
	res.RetCode = int32(retCode)

	switch int32(retCode) {
	case gameproto.LoginInvalidAccount:
		res.Msg = withServerMsg("账号不存在（服务端 100）", serverMsg)
	case gameproto.LoginInvalidPassword:
		res.Exists = true
		res.Msg = withServerMsg("账号存在（密码错误）", serverMsg)
	case gameproto.LoginFreezed:
		res.Exists = true
		res.Msg = withServerMsg("账号存在（已冻结）", serverMsg)
	case gameproto.LoginOK:
		res.Exists = true
		res.Usable = true
		res.Msg = withServerMsg("账号存在（登录成功）", serverMsg)
		if opt.QueryRole {
			role, note := queryRole(conn, opt)
			switch {
			case role != nil:
				res.RoleName = role.Name
				res.Level = int(role.Level)
				res.Turn = int(role.Turn)
				res.Msg += fmt.Sprintf(" 角色=%s 等级=%d", role.Name, role.Level)
			case note != "":
				res.Msg += "（" + note + "）"
			default:
				res.Msg += "（无角色）"
			}
		}
	default:
		res.Exists = true
		res.Msg = withServerMsg(fmt.Sprintf("账号存在（服务端 retcode=%d）", retCode), serverMsg)
	}
	return finish(nil)
}

// queryRole 登录成功后查角色（非致命：拿不到只影响展示，不影响"是否可用"的结论）。
func queryRole(conn net.Conn, opt Options) (*gameproto.Role, string) {
	if err := gameproto.WriteFrame(conn, gameproto.MsgQueryRole,
		[]any{4}, []any{int64(0)}, opt.Coding); err != nil {
		return nil, "查角色下发失败"
	}
	for {
		msgID, body, err := gameproto.ReadFrame(conn)
		if err != nil {
			return nil, "角色查询未返回"
		}
		if msgID != gameproto.MsgQueryRoleBack {
			continue
		}
		roles, err := gameproto.ParseRoleList(body, opt.Coding)
		if err != nil {
			return nil, "角色列表解析失败"
		}
		if len(roles) == 0 {
			return nil, ""
		}
		return &roles[0], ""
	}
}

// VerifyAll 批量探测：每个账号一条独立连接，按 concurrency 并行；结果**与输入同序**。
func VerifyAll(ctx context.Context, addr string, targets []Target, opt Options, concurrency int) []Result {
	out := make([]Result, len(targets))
	if len(targets) == 0 {
		return out
	}
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > MaxConcurrency() {
		concurrency = MaxConcurrency()
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, tg := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, tg Target) {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = Probe(ctx, addr, tg.Account, tg.Password, opt)
		}(i, tg)
	}
	wg.Wait()
	return out
}

// waitErr 把读失败归因成"超时"或"IO 失败"（面板据此提示）。
func waitErr(ctx context.Context, err error, what string) error {
	if ctx.Err() != nil {
		return fmt.Errorf("等待%s超时（%v）", what, ctx.Err())
	}
	var ne net.Error
	if ok := asNetError(err, &ne); ok && ne.Timeout() {
		return fmt.Errorf("等待%s超时（%v）", what, err)
	}
	return fmt.Errorf("读取%s失败: %w", what, err)
}

func asNetError(err error, out *net.Error) bool {
	for err != nil {
		if ne, ok := err.(net.Error); ok {
			*out = ne
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// withServerMsg 合并服务端文案（重复就不叠加，保持可读）。
func withServerMsg(base, srv string) string {
	srv = strings.TrimSpace(srv)
	if srv == "" || strings.Contains(base, srv) {
		return base
	}
	return base + "：" + srv
}
