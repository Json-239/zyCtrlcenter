// Package livecount 「服务端在线数」数据源：**中控直连游戏服 HTTP 接口**拉全服在线人数。
//
// 参考实现（只读）：game_admin_web 的 origin/hqm 分支
//   - app/admin/library/GMApi.php:18   baseUrl = env('GAME_SERVER_HOST', 'http://192.168.0.120:8080')
//   - app/admin/library/GMApi.php:136  onlineCount() → GET /gm/online?serverId=1000
//   - api-docs.json                   响应示例 {"online_count":32}；另有 /gm/robotOnline
//
// 该接口由游戏服自带（HTTP 查在线表），**不需要 admin_license 的 GM 授权**（实测 192.168.0.201:8080
// 裸 GET 可用，见 docs/04-测试/分析-20260922-参考hqm实现在线人数.md），比机器人端 `@online` 管理
// 命令更可靠：不占用任何一个机器人的在线、不受回执解析/号掉线影响。
//
// 本包职责：
//   - 周期（IntervalSec）拉一次 /gm/online，解析 online_count（含机器人）；可选 robot_online_count；
//   - 成功读数存入内存（带 ts），供 /api/status.svr_online 与平台「在线水位保持器」取用；
//   - **默认关闭**（Enabled=false 或 BaseURL 为空时 Start 什么都不做）；
//   - 全程 try/panic 兜底：任何异常只记日志，下一轮照跑（不许把调用方带崩）。
//
// 数据源优先级（由壳层 internal/api 组装，非本包）：provider（本包，新鲜时）→ 机器人 @online
// 回执 → 本地握手数兜底。
package livecount

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 默认参数（2026-09-22）。
const (
	DefaultServerID    = "1000" // 一区（api-docs.json：serverId 默认 1000）
	DefaultIntervalSec = 60     // 轮询间隔（秒）
	DefaultTimeoutSec  = 3      // 单次 HTTP 超时（秒）
	MinIntervalSec     = 5      // 间隔下限（防呆：别把游戏服打爆）
	MaxIntervalSec     = 3600
	MinTimeoutSec      = 1
	MaxTimeoutSec      = 60

	// OnlinePath 服务端在线数接口路径（参考实现 GMApi::onlineCount 的 '/gm/online'）。
	OnlinePath = "/gm/online"
)

// 数据来源标签（/api/status.svr_online.source；面板据此显示"读数从哪来"）。
const (
	SourceProvider = "svr_provider" // 中控直连游戏服 /gm/online（本包）
	SourceSvr      = "svr"          // 机器人 @online 管理命令回执（现状兜底）
	SourceLocal    = "local"        // 无服务端读数（面板/水位保持器用本地握手数兜底）
)

// Config 数据源参数（由 config.Config / main 装配；也可测试直给）。
type Config struct {
	Enabled     bool   `json:"enabled"`      // 总开关（默认 false）
	BaseURL     string `json:"base_url"`     // 游戏服 HTTP 服务基地址，如 http://192.168.0.201:8080（空=禁用）
	ServerID    string `json:"server_id"`    // 区号（默认 "1000"）
	IntervalSec int    `json:"interval_sec"` // 轮询间隔（秒）
	TimeoutSec  int    `json:"timeout_sec"`  // 单次 HTTP 超时（秒）
	Token       string `json:"token"`        // 可选鉴权头 X-GM-Token（当前接口无鉴权，留空即可）
}

// DefaultConfig 默认参数：enabled=false，必须人工开（不会自己动生产）。
func DefaultConfig() Config {
	return Config{
		Enabled:     false,
		BaseURL:     "",
		ServerID:    DefaultServerID,
		IntervalSec: DefaultIntervalSec,
		TimeoutSec:  DefaultTimeoutSec,
	}
}

// Normalize 把参数夹到合法范围（装配/测试都过一遍，脏配置不会引发异常动作）。
func (c Config) Normalize() Config {
	if c.ServerID == "" {
		c.ServerID = DefaultServerID
	}
	if c.IntervalSec < MinIntervalSec {
		c.IntervalSec = DefaultIntervalSec
	}
	if c.IntervalSec > MaxIntervalSec {
		c.IntervalSec = MaxIntervalSec
	}
	if c.TimeoutSec < MinTimeoutSec {
		c.TimeoutSec = DefaultTimeoutSec
	}
	if c.TimeoutSec > MaxTimeoutSec {
		c.TimeoutSec = MaxTimeoutSec
	}
	return c
}

// Ready 是否可以真正开始轮询（启用 + 配了端点）。
func (c Config) Ready() bool { return c.Enabled && strings.TrimSpace(c.BaseURL) != "" }

// StaleSec 读数超过这个秒数算过期（= 3 个轮询周期，最少 90 秒）：过期就让 @online 顶上。
func (c Config) StaleSec() int {
	s := 3 * c.Normalize().IntervalSec
	if s < 90 {
		s = 90
	}
	return s
}

// Endpoint 组装请求 URL：<base>/gm/online?serverId=<id>。
func (c Config) Endpoint() string {
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if base == "" {
		return ""
	}
	q := ""
	if c.Normalize().ServerID != "" {
		q = "?serverId=" + url.QueryEscape(c.Normalize().ServerID)
	}
	return base + OnlinePath + q
}

// ---------------------------------------------------------------- 解析（纯函数，便于单测）

// ParseOnlineResp 解析 /gm/online 的响应体，返回：(在线总数, 机器人在线数, 是否带机器人数, error)。
//
// 兼容三种形态（实测/参考实现都出现过）：
//  1. 标准：{"online_count":32,"success":true,"serverId":1000}
//  2. 包装：{"data":{"online_count":32},"success":true}（PHP 层二次包装）
//  3. 老版本/无 success：{"online_count":32}
//
// 口径：
//   - success 明确为 false → 报错（把 message/msg 带出来）；
//   - online_count 允许为 0（合法读数：服里没人）；
//   - 字段值兼容数字与字符串（"32"）；
//   - 找不到 online_count → 报错（宁可没有读数，也别把 0 当成真值）。
func ParseOnlineResp(body []byte) (count int, robot int, hasRobot bool, err error) {
	txt := strings.TrimSpace(string(body))
	if txt == "" {
		return 0, 0, false, fmt.Errorf("响应为空")
	}
	var obj map[string]any
	if e := json.Unmarshal([]byte(txt), &obj); e != nil {
		return 0, 0, false, fmt.Errorf("响应不是 JSON 对象: %w", e)
	}
	if ok, isBool := obj["success"].(bool); isBool && !ok {
		msg := strOf(obj["message"])
		if msg == "" {
			msg = strOf(obj["msg"])
		}
		if msg == "" {
			msg = "服务端返回 success=false"
		}
		return 0, 0, false, fmt.Errorf("%s", msg)
	}
	// 顶层优先；顶层没有 online_count 字段时看 data 包装
	inner := obj
	if _, exists := obj["online_count"]; !exists {
		if d, ok := obj["data"].(map[string]any); ok {
			inner = d
		}
	}
	v, exists := inner["online_count"]
	if !exists {
		return 0, 0, false, fmt.Errorf("响应里没有 online_count 字段")
	}
	n, ok := asInt(v)
	if !ok {
		return 0, 0, false, fmt.Errorf("online_count 不是数字: %v", v)
	}
	count = n
	if rv, exists := inner["robot_online_count"]; exists {
		if rn, ok := asInt(rv); ok {
			robot, hasRobot = rn, true
		}
	}
	return count, robot, hasRobot, nil
}

func asInt(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	case int:
		return t, true
	case int64:
		return int(t), true
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(n), true
		}
		return 0, false
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			return n, true
		}
		return 0, false
	}
	return 0, false
}

func strOf(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// ---------------------------------------------------------------- Provider

// Provider 在线数数据源（并发安全）。Latest 与 Start/Fetch 可同时调用。
type Provider struct {
	cfg    Config
	client *http.Client
	logf   func(format string, args ...any)

	mu        sync.Mutex
	count     int
	robot     int
	hasRobot  bool
	tsMS      float64 // 最近一次**成功**读数的时间戳（ms）
	ok        bool    // 是否查到过读数
	lastErr   string  // 最近一次错误（成功后被清空）
	lastTryMS float64 // 最近一次尝试时间（ms，含失败）
	tries     int64   // 累计尝试次数
	fails     int64   // 累计失败次数
}

// New 创建 Provider（不自动轮询；Start 负责）。
func New(cfg Config, logf func(format string, args ...any)) *Provider {
	cfg = cfg.Normalize()
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Provider{
		cfg: cfg,
		client: &http.Client{
			Timeout: time.Duration(cfg.TimeoutSec) * time.Second,
		},
		logf: logf,
	}
}

// Config 参数快照。
func (p *Provider) Config() Config { return p.cfg }

// current 线程安全读当前配置（循环里每轮取一次，运行时 SetConfig 立即生效）。
func (p *Provider) current() Config {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg
}

// SetConfig 运行时替换配置（2026-09-22：映射地址/间隔改了不必重启中控）。
// 下一轮取数即生效；Ready() 为 false 时循环不再工作（保持静默）。
func (p *Provider) SetConfig(cfg Config) {
	cfg = cfg.Normalize()
	p.mu.Lock()
	p.cfg = cfg
	p.mu.Unlock()
}

// Ready 是否可以真正轮询。
func (p *Provider) Ready() bool { return p.cfg.Ready() }

// Fetch 单次拉取（成功则更新读数；失败只返回 error，不 panic）。
// 可手动调用（试跑/诊断）；Start 里每轮也走它。
func (p *Provider) Fetch(ctx context.Context) error {
	if p == nil {
		return fmt.Errorf("provider 为 nil")
	}
	p.mu.Lock()
	p.tries++
	p.mu.Unlock()
	endpoint := p.cfg.Endpoint()
	if endpoint == "" {
		return p.fail("未配置端点（base_url 为空）")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return p.fail("构造请求失败: " + err.Error())
	}
	req.Header.Set("Accept", "application/json")
	if p.cfg.Token != "" {
		req.Header.Set("X-GM-Token", p.cfg.Token)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return p.fail("请求失败: " + err.Error())
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB 上限，防脏响应
	if err != nil {
		return p.fail("读响应失败: " + err.Error())
	}
	if resp.StatusCode != http.StatusOK {
		return p.fail(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body))))
	}
	count, robot, hasRobot, err := ParseOnlineResp(body)
	if err != nil {
		return p.fail(err.Error())
	}
	p.mu.Lock()
	p.count, p.robot, p.hasRobot = count, robot, hasRobot
	p.tsMS = float64(time.Now().UnixMilli())
	p.ok = true
	p.lastTryMS = p.tsMS
	p.lastErr = ""
	p.mu.Unlock()
	return nil
}

// fail 记一次失败（lastErr/失败计数）并原样返回 error。
func (p *Provider) fail(msg string) error {
	p.mu.Lock()
	p.fails++
	p.lastTryMS = float64(time.Now().UnixMilli())
	p.lastErr = msg
	p.mu.Unlock()
	return fmt.Errorf("%s", msg)
}

// Start 周期拉取（ctx 结束即退出）。**Enabled=false 或没配端点时什么都不做**。
// 起步先立刻拉一次（不用等一个间隔）；单轮异常只记日志，下一轮继续。
func (p *Provider) Start(ctx context.Context) {
	if p == nil || !p.cfg.Ready() {
		return
	}
	p.logf("[LIVECOUNT] 服务端在线数数据源启动：url=%s 间隔=%ds 超时=%ds（参考 /gm/online，无需 GM 授权）",
		p.cfg.Endpoint(), p.cfg.IntervalSec, p.cfg.TimeoutSec)
	p.tryOnce(ctx)
	// 2026-09-22 间隔每轮现取（SetConfig 改间隔也能立即生效；旧实现 ticker 固定）
	for {
		iv := p.current().IntervalSec
		if iv <= 0 {
			iv = DefaultIntervalSec
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(iv) * time.Second):
			p.tryOnce(ctx)
		}
	}
}

// tryOnce 一轮拉取：带超时 + panic 兜底，失败只记日志。
func (p *Provider) tryOnce(ctx context.Context) {
	defer func() {
		if v := recover(); v != nil {
			p.logf("[LIVECOUNT] 本轮异常（已忽略，下一轮继续）: %v", v)
		}
	}()
	cfg := p.current()
	tctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSec)*time.Second)
	defer cancel()
	if err := p.Fetch(tctx); err != nil {
		p.logf("[LIVECOUNT] 拉取失败（不影响其它模块）: %v", err)
		return
	}
	c, _, ok := p.Latest()
	if ok {
		p.logf("[LIVECOUNT] 服务端全服在线: %d（url=%s）", c, cfg.Endpoint())
	}
}

// Latest 最近一次**成功**读数：count、时间戳(ms)、是否有读数。
// 新鲜度由调用方判断（见 Config.StaleSec）。
func (p *Provider) Latest() (count int, tsMS float64, ok bool) {
	if p == nil {
		return 0, 0, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.count, p.tsMS, p.ok
}

// Snapshot 诊断快照（/api/status.livecount 展示；未启用也可看配置）。
func (p *Provider) Snapshot() map[string]any {
	if p == nil {
		return map[string]any{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]any{
		"enabled":      p.cfg.Enabled,
		"ready":        p.cfg.Ready(),
		"url":          p.cfg.Endpoint(),
		"server_id":    p.cfg.ServerID,
		"interval_sec": p.cfg.IntervalSec,
		"timeout_sec":  p.cfg.TimeoutSec,
		"ok":           p.ok,
		"count":        p.count,
		"robot_count":  p.robot,
		"has_robot":    p.hasRobot,
		"tries":        p.tries,
		"fails":        p.fails,
		"last_err":     p.lastErr,
	}
	if p.ok {
		out["ts"] = int64(p.tsMS)
		out["age_sec"] = ageSecOf(p.tsMS)
	} else {
		out["age_sec"] = -1
	}
	if p.lastTryMS > 0 {
		out["last_try_ts"] = int64(p.lastTryMS)
	}
	return out
}

// ageSecOf 距今秒数（tsMS<=0 返回 -1；未来时间夹到 0）。
func ageSecOf(tsMS float64) int {
	if tsMS <= 0 {
		return -1
	}
	age := int(time.Now().UnixMilli()/1000 - int64(tsMS)/1000)
	if age < 0 {
		age = 0
	}
	return age
}
