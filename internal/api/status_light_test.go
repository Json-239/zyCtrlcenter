// /api/status 瘦身（?bag=1 兼容）+ 单机端点 + gzip 中间件 + WS 广播减负
// （2026-10-12 性能止血）测试：
//
//	① 默认剥 bag / ?bag=1 恢复完整行 / 其余字段保留 / 顶部 stagger 摘要块；
//	② GET /api/status/robot 200（含 bag）/ 404（未知账号、缺参）；
//	③ gzip：带 Accept-Encoding: gzip 时解压后与未压缩逐字节一致；小响应/非 200/未声明不压；
//	④ Broadcast：>64KB 跳过（含边界纯函数）+ status_reply 跳过名单 + 超限日志限频。
package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/config"
	"zyctrlcenter/internal/services/event"
	"zyctrlcenter/internal/services/process"
	"zyctrlcenter/internal/state"
	"zyctrlcenter/internal/store"
	"zyctrlcenter/test/testsupport"
)

// perfEnv 性能批测试环境（只装配 /api/status 相关依赖）。
type perfEnv struct {
	a  *API
	st *state.State
}

func newPerfEnv(t *testing.T) *perfEnv {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ChainDir = filepath.Join(cfg.DataDir, "chains")
	cfg.DeployDir = t.TempDir()
	cfg.RobotExe = filepath.Join(cfg.DeployDir, "robot_single_robot.exe")
	st := state.New()
	runStore := store.New(t.TempDir(), 30, 500)
	log := testsupport.NewTestLogger(t) // 自带 t.Cleanup(Close)
	ch := testsupport.NewTestChannel(t)
	ev := event.New(cfg, st, runStore, ch, log)
	a := New(Deps{
		Cfg: cfg, St: st, Store: runStore, Ctrl: ch, Log: log,
		Proc:   process.New(cfg.RobotExe, cfg.DeployDir, "robot_single_robot.exe", nil),
		Zones:  testsupport.NewTestZones(t),
		Maps:   testsupport.NewTestMaps(t),
		Events: ev, WS: NewWSHub(nil),
	})
	return &perfEnv{a: a, st: st}
}

// doGetRaw 直发 GET 并取原始字节（Accept-Encoding 按传入设置；空 = 不设置）。
func doGetRaw(t *testing.T, rawURL, acceptEncoding string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求 %s 失败: %v", rawURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应失败: %v", err)
	}
	return resp, body
}

// getJSONDoc GET 并解析 JSON（identity 编码，避免 gzip 干扰）。
func getJSONDoc(t *testing.T, rawURL string) map[string]any {
	t.Helper()
	_, body := doGetRaw(t, rawURL, "identity")
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		head := body
		if len(head) > 120 {
			head = head[:120]
		}
		t.Fatalf("响应不是合法 JSON: %v（前 120B: %q）", err, head)
	}
	return doc
}

// robotRowOf 从 status 文档的 robots[] 里按 account 找行。
func robotRowOf(t *testing.T, doc map[string]any, account string) map[string]any {
	t.Helper()
	rows, _ := doc["robots"].([]any)
	for _, it := range rows {
		if m, _ := it.(map[string]any); m["account"] == account {
			return m
		}
	}
	t.Fatalf("robots 里找不到 %s", account)
	return nil
}

// ① 默认剥 bag / ?bag=1 恢复 / 其余字段保留 / stagger 块。
func TestStatusDefaultStripsBagBagParamRestores(t *testing.T) {
	env := newPerfEnv(t)
	env.st.Update("perf1@xy3.com", func(r *state.Robot) {
		r.Online = true
		r.State = "NEXT"
		r.Level = 42
		r.HS = true
		r.Bag = map[string]any{"items": []any{map[string]any{"slot": 1, "item": "青牛"}}}
	})
	srv := httptest.NewServer(env.a.Handler())
	t.Cleanup(srv.Close)

	// 默认：无 bag 键；其余字段保留
	doc := getJSONDoc(t, srv.URL+"/api/status")
	row := robotRowOf(t, doc, "perf1@xy3.com")
	if v, ok := row["bag"]; ok {
		t.Fatalf("默认 /api/status 不应带 bag，实得 %v", v)
	}
	if row["state"] != "NEXT" || row["hs"] != true {
		t.Fatalf("行内其余字段应保留：%v", row)
	}
	if lv, _ := row["level"].(float64); lv != 42 {
		t.Fatalf("level 应保留（42），实得 %v", row["level"])
	}

	// ?bag=1：恢复完整 bag
	doc2 := getJSONDoc(t, srv.URL+"/api/status?bag=1")
	row2 := robotRowOf(t, doc2, "perf1@xy3.com")
	bag, ok := row2["bag"].(map[string]any)
	if !ok {
		t.Fatalf("?bag=1 应恢复 bag，实得 %#v", row2["bag"])
	}
	if !strings.Contains(fmt.Sprintf("%v", bag), "青牛") {
		t.Fatalf("bag 内容应完整，实得 %v", bag)
	}

	// ?bag=0：仍剥（工具脚本默认路径不受影响）
	doc3 := getJSONDoc(t, srv.URL+"/api/status?bag=0")
	if _, ok := robotRowOf(t, doc3, "perf1@xy3.com")["bag"]; ok {
		t.Fatal("?bag=0 不应带 bag")
	}

	// stagger 摘要块（读 Cfg）
	blk, ok := doc["stagger"].(map[string]any)
	if !ok {
		t.Fatalf("顶部应有 stagger 摘要块，实得 %#v", doc["stagger"])
	}
	if blk["enabled"] != false {
		t.Fatalf("stagger.enabled 应与 Cfg 一致（默认 false），实得 %v", blk["enabled"])
	}
	if _, ok := blk["per_hour"]; !ok {
		t.Fatal("stagger 应含 per_hour")
	}
	if pct, _ := blk["sample_pct"].(float64); pct != 100 {
		t.Fatalf("stagger.sample_pct 应与 Cfg 一致（默认 100），实得 %v", blk["sample_pct"])
	}

	// rescue48 摘要块（另一批合入 a.Rescue48Summary；前端已容错，这里只作存在性断言）
	rs, ok := doc["rescue48"].(map[string]any)
	if !ok {
		t.Fatalf("顶部应有 rescue48 摘要块，实得 %#v", doc["rescue48"])
	}
	if rs["enabled"] != true {
		t.Fatalf("rescue48.enabled 应与 Cfg 一致（默认开），实得 %v", rs["enabled"])
	}
}

// ② 单机端点：200（含 bag）/ 404（未知、缺参）。
func TestStatusRobotEndpoint(t *testing.T) {
	env := newPerfEnv(t)
	env.st.Update("solo@xy3.com", func(r *state.Robot) {
		r.Online = true
		r.State = "FIGHT"
		r.Bag = map[string]any{"items": []any{map[string]any{"slot": 9}}}
	})
	srv := httptest.NewServer(env.a.Handler())
	t.Cleanup(srv.Close)

	q := url.Values{"account": {"solo@xy3.com"}}.Encode()
	resp, body := doGetRaw(t, srv.URL+"/api/status/robot?"+q, "identity")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("已知账号应 200，实得 %d（%s）", resp.StatusCode, body)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if doc["ok"] != true {
		t.Fatalf("应 ok=true：%v", doc)
	}
	rb, _ := doc["robot"].(map[string]any)
	if rb == nil || rb["bag"] == nil {
		t.Fatalf("单机端点应含 bag：%#v", rb)
	}
	if rb["state"] != "FIGHT" {
		t.Fatalf("state 应为 FIGHT，实得 %v", rb["state"])
	}

	q2 := url.Values{"account": {"nobody@xy3.com"}}.Encode()
	resp2, body2 := doGetRaw(t, srv.URL+"/api/status/robot?"+q2, "identity")
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("未知账号应 404，实得 %d", resp2.StatusCode)
	}
	var d2 map[string]any
	_ = json.Unmarshal(body2, &d2)
	if d2["ok"] != false {
		t.Fatalf("404 应 ok=false：%v", d2)
	}

	resp3, _ := doGetRaw(t, srv.URL+"/api/status/robot", "identity")
	if resp3.StatusCode != http.StatusNotFound {
		t.Fatalf("缺 account 参数应 404，实得 %d", resp3.StatusCode)
	}
}

// ③ gzip：解码一致 / 不压边界（小响应、未声明）。
func TestGzipMiddleware(t *testing.T) {
	env := newPerfEnv(t)
	for i := 0; i < 8; i++ {
		acc := fmt.Sprintf("gz%02d@xy3.com", i)
		env.st.Update(acc, func(r *state.Robot) {
			r.Online = true
			r.State = "NEXT"
			r.Hatch = map[string]any{"active": false, "battles": i}
		})
	}
	srv := httptest.NewServer(env.a.Handler())
	t.Cleanup(srv.Close)

	plainResp, plain := doGetRaw(t, srv.URL+"/api/status", "identity")
	if ce := plainResp.Header.Get("Content-Encoding"); ce != "" {
		t.Fatalf("identity 请求不应压缩，实得 %q", ce)
	}
	if len(plain) < gzipMinBytes {
		t.Fatalf("样本 %dB 小于 %dB，未覆盖压缩路径（需补测试数据）", len(plain), gzipMinBytes)
	}

	gzResp, gzBody := doGetRaw(t, srv.URL+"/api/status", "gzip")
	if ce := gzResp.Header.Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("gzip 请求应压缩，实得 %q", ce)
	}
	if !strings.Contains(gzResp.Header.Get("Vary"), "Accept-Encoding") {
		t.Fatalf("压缩响应应带 Vary: Accept-Encoding，实得 %q", gzResp.Header.Get("Vary"))
	}
	zr, err := gzip.NewReader(bytes.NewReader(gzBody))
	if err != nil {
		t.Fatalf("解压失败: %v", err)
	}
	decoded, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("读解压流失败: %v", err)
	}
	if !bytes.Equal(decoded, plain) {
		t.Fatalf("解压后应与未压缩响应逐字节一致（got %dB / want %dB）", len(decoded), len(plain))
	}
	var doc map[string]any
	if err := json.Unmarshal(decoded, &doc); err != nil {
		t.Fatalf("解压后 JSON 非法: %v", err)
	}
	if rows, _ := doc["robots"].([]any); len(rows) != 8 {
		t.Fatalf("robots 应有 8 行，实得 %d", len(rows))
	}

	// 非 200 小响应（缺参 404）不压
	smallResp, _ := doGetRaw(t, srv.URL+"/api/status/robot", "gzip")
	if smallResp.StatusCode != http.StatusNotFound {
		t.Fatalf("预期 404，实得 %d", smallResp.StatusCode)
	}
	if smallResp.Header.Get("Content-Encoding") == "gzip" {
		t.Fatal("非 200 小响应不应压缩")
	}

	// 未声明 gzip：不压
	noResp, _ := doGetRaw(t, srv.URL+"/api/status", "")
	if ce := noResp.Header.Get("Content-Encoding"); ce != "" {
		t.Fatalf("未声明 gzip 不应压缩，实得 %q", ce)
	}
}

// ④ Broadcast：超限跳过（边界）+ 跳过名单 + 限频日志。
func TestBroadcastSkipAndSizeLimit(t *testing.T) {
	// 边界纯函数
	if !broadcastSizeAllowed(BroadcastMaxBytes) {
		t.Fatalf("恰好 %d 字节应放行（<=）", BroadcastMaxBytes)
	}
	if broadcastSizeAllowed(BroadcastMaxBytes + 1) {
		t.Fatal("超上限 1 字节应拒绝")
	}

	var logs []string
	h := NewWSHub(func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) })
	c := &wsClient{ch: make(chan []byte, 8), done: make(chan struct{})}
	h.mu.Lock()
	h.clients[c] = true
	h.mu.Unlock()
	t.Cleanup(func() { // 不 close（conn 为 nil 的测试桩）
		h.mu.Lock()
		delete(h.clients, c)
		h.mu.Unlock()
	})

	// 超限（JSON 包装后 > 64KB）：跳过 + 同 type 60s 内只记一条日志
	big := map[string]any{"type": "huge", "payload": strings.Repeat("x", BroadcastMaxBytes)}
	h.Broadcast(big)
	h.Broadcast(big)
	drainNone(t, c, "超限广播不应送达")
	if n := countLog(logs, "[WS] 跳过超限广播"); n != 1 {
		t.Fatalf("超限日志应限频为 1 条，实得 %d 条：%v", n, logs)
	}
	if !strings.Contains(logs[0], "type=huge") {
		t.Fatalf("日志应含 type=huge：%s", logs[0])
	}

	// 正常消息送达
	h.Broadcast(map[string]any{"type": "small", "v": 1})
	select {
	case data := <-c.ch:
		if !strings.Contains(string(data), `"small"`) {
			t.Fatalf("应收到 small 消息，实得 %s", data)
		}
	case <-time.After(time.Second):
		t.Fatal("正常消息应送达")
	}

	// 跳过名单：status_reply（前端零消费，2026-10-12 实测 93% 流量）不广播
	h.Broadcast(map[string]any{"type": "status_reply", "robots": []any{map[string]any{"account": "x"}}})
	drainNone(t, c, "status_reply 在跳过名单，不应广播")
}

// drainNone 断言客户端队列里没有任何消息。
func drainNone(t *testing.T, c *wsClient, msg string) {
	t.Helper()
	select {
	case data := <-c.ch:
		t.Fatalf("%s（实收 %dB）", msg, len(data))
	default:
	}
}

func countLog(logs []string, substr string) int {
	n := 0
	for _, s := range logs {
		if strings.Contains(s, substr) {
			n++
		}
	}
	return n
}
