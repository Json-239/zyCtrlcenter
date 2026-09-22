// livecount 用例：/gm/online 响应解析（纯函数）+ 配置归一化/端点组装 + Provider 拉取
// （httptest 假服务端；不碰真实游戏服、不起进程）。
//
// 断言的是**结果证据**：解析出的数字/错误、请求 URL 与请求头、Latest 的读数与时间戳、
// Snapshot 的字段（ok/last_err/tries/fails）、Start 的行为（关闭时零请求）。
package livecount_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zyctrlcenter/internal/services/livecount"
)

// ---------------------------------------------------------------- 解析（纯函数）

func TestParseOnlineRespStandard(t *testing.T) {
	// 实测口径（192.168.0.201:8080）：{"online_count":0,"success":true,"serverId":1000}
	body := []byte(`{"online_count":32,"success":true,"serverId":1000}`)
	count, robot, hasRobot, err := livecount.ParseOnlineResp(body)
	if err != nil {
		t.Fatalf("标准响应应解析成功: %v", err)
	}
	if count != 32 {
		t.Fatalf("online_count 应为 32，实际 %d", count)
	}
	if hasRobot {
		t.Fatalf("没有 robot_online_count 字段时 hasRobot 应为 false，实际 %v(%d)", hasRobot, robot)
	}
}

func TestParseOnlineRespZeroIsValid(t *testing.T) {
	// 0 是合法读数（服里没人），不许当成"没有字段"
	body := []byte(`{"online_count":0,"success":true,"serverId":1000}`)
	count, _, _, err := livecount.ParseOnlineResp(body)
	if err != nil {
		t.Fatalf("online_count=0 应视为合法读数: %v", err)
	}
	if count != 0 {
		t.Fatalf("应解析出 0，实际 %d", count)
	}
}

func TestParseOnlineRespDataWrapped(t *testing.T) {
	// PHP 层二次包装：{"data":{"online_count":17},...}
	body := []byte(`{"success":true,"data":{"online_count":17,"robot_online_count":9}}`)
	count, robot, hasRobot, err := livecount.ParseOnlineResp(body)
	if err != nil {
		t.Fatalf("data 包装格式应解析成功: %v", err)
	}
	if count != 17 || !hasRobot || robot != 9 {
		t.Fatalf("data 包装解析不符：count=%d robot=%d has=%v", count, robot, hasRobot)
	}
}

func TestParseOnlineRespStringNumber(t *testing.T) {
	// 字段值兼容字符串（"45"）
	body := []byte(`{"online_count":"45","success":"true"}`)
	count, _, _, err := livecount.ParseOnlineResp(body)
	if err != nil {
		t.Fatalf("字符串数字应解析成功: %v", err)
	}
	if count != 45 {
		t.Fatalf("字符串数字应解析出 45，实际 %d", count)
	}
}

func TestParseOnlineRespSuccessFalse(t *testing.T) {
	body := []byte(`{"success":false,"message":"serverId 未登记"}`)
	_, _, _, err := livecount.ParseOnlineResp(body)
	if err == nil {
		t.Fatal("success=false 应报错")
	}
	if !strings.Contains(err.Error(), "serverId 未登记") {
		t.Fatalf("错误信息应带上服务端 message，实际: %v", err)
	}
}

func TestParseOnlineRespMissingField(t *testing.T) {
	for _, body := range []string{`{"success":true}`, `{"foo":1}`, `[1,2,3]`} {
		if _, _, _, err := livecount.ParseOnlineResp([]byte(body)); err == nil {
			t.Fatalf("缺 online_count 的响应应报错: %s", body)
		}
	}
}

func TestParseOnlineRespBadBody(t *testing.T) {
	for _, body := range []string{"", "   ", "<html>502</html>", "null"} {
		if _, _, _, err := livecount.ParseOnlineResp([]byte(body)); err == nil {
			t.Fatalf("非法响应体应报错: %q", body)
		}
	}
}

func TestParseOnlineRespRobotCount(t *testing.T) {
	// 只有 robot_online_count（那是 /gm/robotOnline 的响应形态）→ 没有 online_count 应报错
	if _, _, _, err := livecount.ParseOnlineResp([]byte(`{"robot_online_count":12}`)); err == nil {
		t.Fatal("只有 robot_online_count 时应报错（口径不同接口）")
	}
	// 两个字段都在的合并形态（online 主体 + robot 附带）：都解析出来
	body := []byte(`{"success":true,"serverId":1000,"online_count":32,"robot_online_count":12}`)
	count, robot, hasRobot, err := livecount.ParseOnlineResp(body)
	if err != nil {
		t.Fatalf("合并形态应解析成功: %v", err)
	}
	if count != 32 || !hasRobot || robot != 12 {
		t.Fatalf("合并形态解析不符：count=%d robot=%d has=%v", count, robot, hasRobot)
	}
}

// ---------------------------------------------------------------- 配置

func TestConfigNormalize(t *testing.T) {
	// serverID 空 → 默认 1000；越界 interval/timeout 夹回默认
	c := livecount.Config{ServerID: "", IntervalSec: 0, TimeoutSec: 0}.Normalize()
	if c.ServerID != livecount.DefaultServerID {
		t.Fatalf("serverID 空应回默认 %s，实际 %s", livecount.DefaultServerID, c.ServerID)
	}
	if c.IntervalSec != livecount.DefaultIntervalSec || c.TimeoutSec != livecount.DefaultTimeoutSec {
		t.Fatalf("越界间隔/超时应回默认 %d/%d，实际 %d/%d",
			livecount.DefaultIntervalSec, livecount.DefaultTimeoutSec, c.IntervalSec, c.TimeoutSec)
	}
	// 过小间隔夹到下限、过大夹到上限
	if got := (livecount.Config{IntervalSec: 2}).Normalize().IntervalSec; got != livecount.DefaultIntervalSec {
		t.Fatalf("interval=2 应重置为默认 %d，实际 %d", livecount.DefaultIntervalSec, got)
	}
	if got := (livecount.Config{IntervalSec: 99999}).Normalize().IntervalSec; got != livecount.MaxIntervalSec {
		t.Fatalf("interval 超大应夹到 %d，实际 %d", livecount.MaxIntervalSec, got)
	}
	if got := (livecount.Config{TimeoutSec: 999}).Normalize().TimeoutSec; got != livecount.MaxTimeoutSec {
		t.Fatalf("timeout 超大应夹到 %d，实际 %d", livecount.MaxTimeoutSec, got)
	}
}

func TestConfigReadyAndStaleSec(t *testing.T) {
	if (livecount.Config{Enabled: true}).Ready() {
		t.Fatal("没配 base_url 时 Ready 应为 false")
	}
	if (livecount.Config{BaseURL: "http://x:8080"}).Ready() {
		t.Fatal("enabled=false 时 Ready 应为 false")
	}
	if !(livecount.Config{Enabled: true, BaseURL: "http://x:8080"}).Ready() {
		t.Fatal("enabled + url 齐全时 Ready 应为 true")
	}
	// 新鲜阈值 = 3 个周期（默认 60s → 180s），最少 90s
	if got := livecount.DefaultConfig().StaleSec(); got != 180 {
		t.Fatalf("默认新鲜阈值应为 180s，实际 %d", got)
	}
	if got := (livecount.Config{IntervalSec: livecount.MinIntervalSec}).Normalize().StaleSec(); got != 90 {
		t.Fatalf("小间隔下限应保底 90s，实际 %d", got)
	}
}

func TestConfigEndpoint(t *testing.T) {
	cases := []struct {
		cfg  livecount.Config
		want string
	}{
		{livecount.Config{BaseURL: "http://192.168.0.201:8080"}, "http://192.168.0.201:8080/gm/online?serverId=1000"},
		{livecount.Config{BaseURL: "http://192.168.0.201:8080/"}, "http://192.168.0.201:8080/gm/online?serverId=1000"},
		{livecount.Config{BaseURL: "http://h:1", ServerID: "1001"}, "http://h:1/gm/online?serverId=1001"},
		{livecount.Config{}, ""},
	}
	for _, c := range cases {
		if got := c.cfg.Endpoint(); got != c.want {
			t.Fatalf("Endpoint(%+v) 应为 %q，实际 %q", c.cfg, c.want, got)
		}
	}
}

// ---------------------------------------------------------------- Provider（httptest 假服务端）

// fakeGameServer 假游戏服：/gm/online 返回可配置的响应，并记录收到的请求。
type fakeGameServer struct {
	srv      *httptest.Server
	hits     int64
	lastPath string
	lastQ    string
	lastTok  string
	body     atomic.Value // string
	status   atomic.Int64 // HTTP 状态码（200 默认）
}

func newFakeGameServer() *fakeGameServer {
	f := &fakeGameServer{}
	f.body.Store(`{"online_count":32,"success":true,"serverId":1000}`)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&f.hits, 1)
		f.lastPath = r.URL.Path
		f.lastQ = r.URL.RawQuery
		f.lastTok = r.Header.Get("X-GM-Token")
		code := int(f.status.Load())
		if code == 0 {
			code = http.StatusOK
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(f.body.Load().(string)))
	}))
	return f
}

func TestProviderFetchOK(t *testing.T) {
	fs := newFakeGameServer()
	defer fs.srv.Close()
	p := livecount.New(livecount.Config{
		Enabled: true, BaseURL: fs.srv.URL, ServerID: "1000",
		IntervalSec: 60, TimeoutSec: 3, Token: "tok-123",
	}, nil)

	if _, _, ok := p.Latest(); ok {
		t.Fatal("拉取前不应有读数")
	}
	if err := p.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch 应成功: %v", err)
	}
	count, tsMS, ok := p.Latest()
	if !ok || count != 32 || tsMS <= 0 {
		t.Fatalf("读数不符：count=%d ts=%v ok=%v", count, tsMS, ok)
	}
	// 请求口径：路径 /gm/online、serverId 透传、可选 token 头
	if fs.lastPath != "/gm/online" {
		t.Fatalf("请求路径应为 /gm/online，实际 %s", fs.lastPath)
	}
	if fs.lastQ != "serverId=1000" {
		t.Fatalf("query 应为 serverId=1000，实际 %q", fs.lastQ)
	}
	if fs.lastTok != "tok-123" {
		t.Fatalf("X-GM-Token 应透传，实际 %q", fs.lastTok)
	}
	// Snapshot 诊断字段
	snap := p.Snapshot()
	if snap["ok"] != true || snap["count"] != 32 || snap["last_err"] != "" {
		t.Fatalf("Snapshot 不符: %+v", snap)
	}
	if snap["tries"] != int64(1) || snap["fails"] != int64(0) {
		t.Fatalf("tries/fails 不符: %+v", snap)
	}
	if snap["url"] != fs.srv.URL+"/gm/online?serverId=1000" {
		t.Fatalf("Snapshot.url 不符: %v", snap["url"])
	}
}

func TestProviderFetchHTTPError(t *testing.T) {
	fs := newFakeGameServer()
	defer fs.srv.Close()
	fs.status.Store(http.StatusInternalServerError)
	fs.body.Store("boom")
	p := livecount.New(livecount.Config{Enabled: true, BaseURL: fs.srv.URL}, nil)

	err := p.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("HTTP 500 应报错并带状态码，实际: %v", err)
	}
	if _, _, ok := p.Latest(); ok {
		t.Fatal("失败后不应有读数")
	}
	snap := p.Snapshot()
	if snap["last_err"] == "" || snap["fails"] != int64(1) || snap["ok"] != false {
		t.Fatalf("失败应记入 Snapshot: %+v", snap)
	}
}

func TestProviderFetchBadJSON(t *testing.T) {
	fs := newFakeGameServer()
	defer fs.srv.Close()
	fs.body.Store(`<html>not json</html>`)
	p := livecount.New(livecount.Config{Enabled: true, BaseURL: fs.srv.URL}, nil)
	if err := p.Fetch(context.Background()); err == nil {
		t.Fatal("非 JSON 响应应报错")
	}
}

func TestProviderFetchNoEndpoint(t *testing.T) {
	p := livecount.New(livecount.DefaultConfig(), nil) // 没配 URL
	if err := p.Fetch(context.Background()); err == nil {
		t.Fatal("未配端点应报错")
	}
	if err := p.Fetch(context.Background()); !strings.Contains(err.Error(), "端点") {
		t.Fatalf("错误信息应说明端点缺失，实际: %v", err)
	}
}

func TestProviderFetchFailureThenSuccessClearsErr(t *testing.T) {
	fs := newFakeGameServer()
	defer fs.srv.Close()
	p := livecount.New(livecount.Config{Enabled: true, BaseURL: fs.srv.URL}, nil)

	fs.status.Store(503)
	if err := p.Fetch(context.Background()); err == nil {
		t.Fatal("第一次应失败")
	}
	if p.Snapshot()["last_err"] == "" {
		t.Fatal("失败后 last_err 应有值")
	}
	fs.status.Store(0) // 恢复 200
	fs.body.Store(`{"online_count":7,"success":true}`)
	if err := p.Fetch(context.Background()); err != nil {
		t.Fatalf("恢复后应成功: %v", err)
	}
	snap := p.Snapshot()
	if snap["last_err"] != "" {
		t.Fatalf("成功后 last_err 应清空，实际 %v", snap["last_err"])
	}
	if c, _, ok := p.Latest(); !ok || c != 7 {
		t.Fatalf("恢复后读数应为 7，实际 count=%d ok=%v", c, ok)
	}
	if snap["tries"] != int64(2) || snap["fails"] != int64(1) {
		t.Fatalf("tries/fails 应为 2/1，实际 %v/%v", snap["tries"], snap["fails"])
	}
}

func TestProviderStartDisabledMakesNoRequest(t *testing.T) {
	fs := newFakeGameServer()
	defer fs.srv.Close()
	// enabled=false（默认配置）→ Start 什么都不做，一个请求也不发
	p := livecount.New(livecount.Config{BaseURL: fs.srv.URL}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	p.Start(ctx)
	if n := atomic.LoadInt64(&fs.hits); n != 0 {
		t.Fatalf("未启用时不该发请求，实际 %d 次", n)
	}
	// 配了 URL 但 enabled=false 同样不发
	p2 := livecount.New(livecount.Config{Enabled: false, BaseURL: fs.srv.URL}, nil)
	p2.Start(ctx)
	if n := atomic.LoadInt64(&fs.hits); n != 0 {
		t.Fatalf("enabled=false 时不该发请求，实际 %d 次", n)
	}
}

func TestProviderStartFetchesImmediately(t *testing.T) {
	fs := newFakeGameServer()
	defer fs.srv.Close()
	p := livecount.New(livecount.Config{Enabled: true, BaseURL: fs.srv.URL, IntervalSec: livecount.MinIntervalSec}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Start(ctx); close(done) }()
	// 起步立刻拉一次（不用等一个间隔）
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c, _, ok := p.Latest(); ok && c == 32 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if c, _, ok := p.Latest(); !ok || c != 32 {
		t.Fatalf("Start 后应立刻有读数，实际 count=%d ok=%v", c, ok)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 Start 应退出")
	}
}

// TestProviderStartTicksOverTime 周期轮询：间隔取最小 5s，等一轮验证会再次拉取。
// 慢用例（~5.5s），go test -short 时跳过。
func TestProviderStartTicksOverTime(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过（周期性等待 5s+）")
	}
	fs := newFakeGameServer()
	defer fs.srv.Close()
	p := livecount.New(livecount.Config{Enabled: true, BaseURL: fs.srv.URL, IntervalSec: livecount.MinIntervalSec}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Start(ctx)
	time.Sleep(time.Duration(livecount.MinIntervalSec)*time.Second + 600*time.Millisecond)
	if n := atomic.LoadInt64(&fs.hits); n < 2 {
		t.Fatalf("5s 周期应至少拉 2 次（起步一次 + 一轮），实际 %d 次", n)
	}
}

func TestProviderFetchUsesTimeout(t *testing.T) {
	// 服务端慢响应：超时后 Fetch 报错、不留读数
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		_, _ = fmt.Fprint(w, `{"online_count":1}`)
	}))
	defer slow.Close()
	p := livecount.New(livecount.Config{Enabled: true, BaseURL: slow.URL, TimeoutSec: 1}, nil)
	start := time.Now()
	err := p.Fetch(context.Background())
	if err == nil {
		t.Fatal("慢响应应超时报错")
	}
	if elapsed := time.Since(start); elapsed > 1200*time.Millisecond {
		t.Fatalf("超时应 ~1s 生效，实际耗时 %v", elapsed)
	}
	if _, _, ok := p.Latest(); ok {
		t.Fatal("超时不应留下读数")
	}
}

// TestProbeRealServer 真实环境探针：**默认跳过**（不联网），只有显式设置环境变量才跑。
//
// 实测方法（2026-09-22 对测试服 192.168.0.201 验证通过）：
//
//	LIVECOUNT_PROBE_URL=http://192.168.0.201:8080 go test ./test/livecount/ -run TestProbeRealServer -v
//
// 可选：LIVECOUNT_PROBE_SERVER_ID=1001 指定区号。
// 只做一次只读 GET（/gm/online），不占用任何游戏在线、无副作用。
func TestProbeRealServer(t *testing.T) {
	base := strings.TrimSpace(os.Getenv("LIVECOUNT_PROBE_URL"))
	if base == "" {
		t.Skip("未设置 LIVECOUNT_PROBE_URL，跳过真实探针（默认不联网）")
	}
	p := livecount.New(livecount.Config{
		Enabled:     true,
		BaseURL:     base,
		ServerID:    strings.TrimSpace(os.Getenv("LIVECOUNT_PROBE_SERVER_ID")),
		IntervalSec: livecount.DefaultIntervalSec,
		TimeoutSec:  livecount.DefaultTimeoutSec,
	}, func(format string, args ...any) { t.Logf(format, args...) })
	if err := p.Fetch(context.Background()); err != nil {
		t.Fatalf("真实探针失败（%s）: %v", base, err)
	}
	count, tsMS, ok := p.Latest()
	if !ok || tsMS <= 0 {
		t.Fatalf("真实探针应拿到读数，实际 count=%d ts=%v ok=%v", count, tsMS, ok)
	}
	t.Logf("真实读数 OK：count=%d（url=%s） snapshot=%+v", count, p.Config().Endpoint(), p.Snapshot())
}
