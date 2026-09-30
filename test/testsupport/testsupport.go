// Package testsupport 测试统一基座（对标参考项目 test/testsupport）。
//
// 规范（单一事实源：docs/04-测试/测试规范.md）：
//   - 所有测试放 test/<模块>/，共享基座（夹具加载 / 假机器人 / 临时环境 / 断言）在本包；
//   - 报文夹具放 test/fixtures/events/*.json，带 provenance（real/raw/how）；
//   - 先隔离后使用：临时数据目录一律用 t.TempDir()，不碰真实 data/。
package testsupport

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"zyctrlcenter/internal/config"
	"zyctrlcenter/internal/ctrl"
	"zyctrlcenter/internal/logging"
	"zyctrlcenter/internal/maplib"
	"zyctrlcenter/internal/services/accounts"
	"zyctrlcenter/internal/services/event"
	"zyctrlcenter/internal/services/zones"
	"zyctrlcenter/internal/state"
	"zyctrlcenter/internal/store"
)

// TestZoneKey 测试用区 key（"<服key>/<区key>"）。
const TestZoneKey = "test-srv/z1"

// TestServerKey 测试用服 key。
const TestServerKey = "test-srv"

// TestRoot 返回 test/ 目录绝对路径。
func TestRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法定位 testsupport 源文件")
	}
	return filepath.Dir(filepath.Dir(file)) // test/
}

// Fixture 报文夹具（provenance + event）。
type Fixture struct {
	Path       string         `json:"-"`
	Provenance map[string]any `json:"provenance"`
	Event      map[string]any `json:"event"`
}

// LoadFixture 读取 test/fixtures/events/<name>。
func LoadFixture(t *testing.T, name string) Fixture {
	t.Helper()
	path := filepath.Join(TestRoot(t), "fixtures", "events", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取夹具 %s 失败: %v", path, err)
	}
	var f Fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("夹具 %s 不是合法 JSON: %v", path, err)
	}
	f.Path = path
	if f.Event == nil {
		t.Fatalf("夹具 %s 缺少 event 字段", path)
	}
	return f
}

// FixtureFiles 返回全部报文夹具文件路径（元测试用）。
func FixtureFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(TestRoot(t), "fixtures", "events", "*.json"))
	if err != nil {
		t.Fatalf("扫描夹具目录失败: %v", err)
	}
	return files
}

// FixturePath 返回 test/fixtures/<rel> 的绝对路径（rel 用 "/" 分隔）。
func FixturePath(t *testing.T, rel string) string {
	t.Helper()
	return filepath.Join(TestRoot(t), "fixtures", filepath.FromSlash(rel))
}

// LoadJSONFixture 读取 test/fixtures/<rel> 并反序列化到 out（非"事件报文"类夹具通用加载器：
// 协议字节表、链数据切片等都用它，避免各模块各写一份读文件的代码）。
func LoadJSONFixture(t *testing.T, rel string, out any) {
	t.Helper()
	path := FixturePath(t, rel)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取夹具 %s 失败: %v", path, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("夹具 %s 不是合法 JSON: %v", path, err)
	}
}

// ChainFixture 读链数据夹具（{provenance, chain} 包装，与 zhuaogui_nav.sample.json 同格式），
// 校验 provenance 后返回 chain 原文（可直接落盘成链数据文件）。
func ChainFixture(t *testing.T, rel string) json.RawMessage {
	t.Helper()
	var wrapped struct {
		Provenance map[string]any  `json:"provenance"`
		Chain      json.RawMessage `json:"chain"`
	}
	LoadJSONFixture(t, rel, &wrapped)
	if len(wrapped.Chain) == 0 {
		t.Fatalf("链数据夹具 %s 缺少 chain 字段", rel)
	}
	if real, _ := wrapped.Provenance["real"].(bool); !real {
		t.Fatalf("链数据夹具 %s 应来自真实数据（provenance.real=true）", rel)
	}
	if how, _ := wrapped.Provenance["how"].(string); how == "" {
		t.Fatalf("链数据夹具 %s 缺少 provenance.how（来源必须可查）", rel)
	}
	return wrapped.Chain
}

// InstallChainFixture 把链数据夹具装进链目录（文件名 <id>.json），返回写入路径。
//
// 为什么需要：api 测试的 cfg.ChainDir 是空临时目录，而"带载荷"类用例必须有一份**小**链数据
// （真实 2MB 载荷会让 handler 写阻塞到超时，用例自锁）。
func InstallChainFixture(t *testing.T, chainDir, id, rel string) string {
	t.Helper()
	if err := os.MkdirAll(chainDir, 0o755); err != nil {
		t.Fatalf("创建链目录失败: %v", err)
	}
	path := filepath.Join(chainDir, id+".json")
	if err := os.WriteFile(path, ChainFixture(t, rel), 0o644); err != nil {
		t.Fatalf("写入链数据夹具 %s 失败: %v", path, err)
	}
	return path
}

// InstallBiaoxingNav 装齐"镖行天下"载荷需要的**两份**链数据：
// 基座（newbie_full：坐标/网格/路由）+ 专属声明（biaoxing_nav：12 变体 task_order + 630/651 备点覆盖）。
func InstallBiaoxingNav(t *testing.T, chainDir string) {
	t.Helper()
	InstallChainFixture(t, chainDir, "newbie_full", "chains/newbie_full.mini.json")
	InstallChainFixture(t, chainDir, "biaoxing_nav", "chains/biaoxing_nav.mini.json")
}

// InstallGhostNav 装齐"抓鬼导航"需要的**两份**链数据：基座（newbie_full：坐标/网格/路由）
// + 抓鬼专属（zhongkui_nav：刷鬼图/落点/地图名）。
//
// 为什么要两份：中控按参考实现的口径**组装**载荷（见 internal/api/chainpayload.go），
// 抓鬼专属文件只需放 ghost_maps/ghost_map_pos（夹具就是这么裁的）。
func InstallGhostNav(t *testing.T, chainDir string) {
	t.Helper()
	InstallChainFixture(t, chainDir, "newbie_full", "chains/newbie_full.mini.json")
	InstallChainFixture(t, chainDir, "zhongkui_nav", "chains/zhongkui_nav.mini.json")
}

// InstallShareDailyNav 装齐"分享日常（大唐神捕）"载荷需要的**两份**链数据：
// 基座（newbie_full：坐标/网格/路由）+ 专属声明（shenbu_nav：task_order 全 4 个任务号）。
//
// 口径同抓鬼：专属文件只放玩法声明，发送时由中控组装（见 chainpayload.go:ShareDailyOf）。
func InstallShareDailyNav(t *testing.T, chainDir string) {
	t.Helper()
	InstallChainFixture(t, chainDir, "newbie_full", "chains/newbie_full.mini.json")
	InstallChainFixture(t, chainDir, "shenbu_nav", "chains/shenbu_nav.mini.json")
}

// InstallFenghuoNav 装齐"烽火大唐"载荷需要的**两份**链数据：
// 基座（newbie_full：坐标/网格/路由）+ 专属声明（fenghuo_nav：task_order 全 6 个任务号）。
//
// 与神捕同口径（2026-09-24 P1）：专属文件只放玩法声明，发送时由中控组装
// （见 chainpayload.go:ShareDailyOf）；两个玩法的声明文件互不共用。
func InstallFenghuoNav(t *testing.T, chainDir string) {
	t.Helper()
	InstallChainFixture(t, chainDir, "newbie_full", "chains/newbie_full.mini.json")
	InstallChainFixture(t, chainDir, "fenghuo_nav", "chains/fenghuo_nav.mini.json")
}

// WriteChainFile 往链目录写一份自定义链数据（用例要构造"地址不全"等异常形状时用），返回路径。
func WriteChainFile(t *testing.T, chainDir, id string, body any) string {
	t.Helper()
	if err := os.MkdirAll(chainDir, 0o755); err != nil {
		t.Fatalf("创建链目录失败: %v", err)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化链数据失败: %v", err)
	}
	path := filepath.Join(chainDir, id+".json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("写入链数据 %s 失败: %v", path, err)
	}
	return path
}

// CloneEvent 深拷贝一个事件（同一夹具在一个用例内多次使用时防串改）。
func CloneEvent(t *testing.T, ev map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("事件序列化失败: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("事件反序列化失败: %v", err)
	}
	return out
}

// ---------------------------------------------------------------- 隔离环境

// NewTestStore 临时数据目录的运行历史存储。
func NewTestStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	return store.New(dir, 30, 500), dir
}

// NewTestLogger 写临时目录的日志器（用例结束自动关闭句柄——Windows 下
// 不关闭会导致 t.TempDir 清理失败）。
func NewTestLogger(t *testing.T) *logging.Logger {
	t.Helper()
	l := logging.New(t.TempDir(), 30)
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// NewTestChannel 启动一条测试用控制通道（端口 0：系统分配），事件带 TestZoneKey 标记。
func NewTestChannel(t *testing.T) *ctrl.Server {
	t.Helper()
	c := ctrl.New("127.0.0.1", 0, nil)
	c.SetZone(TestZoneKey)
	if err := c.Start(); err != nil {
		t.Fatalf("启动测试控制通道失败: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// NewTestHandler 组装事件处理器（cfg 可为 nil）+ 一条已启动的测试控制通道。
// 需要"真实机器人连接"时用 ConnectFakeRobot(t, 返回的通道)。
func NewTestHandler(t *testing.T, cfg *config.Config) (*event.Handler, *state.State, *store.Store, *ctrl.Server) {
	t.Helper()
	st := state.New()
	runStore, _ := NewTestStore(t)
	c := NewTestChannel(t)
	return event.New(cfg, st, runStore, c, NewTestLogger(t)), st, runStore, c
}

// FeedEvent 直接往控制通道事件队列塞一条事件（可指定区标记），用于测试处理器分支。
func FeedEvent(t *testing.T, c *ctrl.Server, zone string, ev map[string]any) {
	t.Helper()
	out := CloneEvent(t, ev)
	if zone != "" {
		out["_zone"] = zone
	}
	select {
	case c.Events <- out:
	case <-time.After(time.Second):
		t.Fatal("事件入队超时（队列已满？）")
	}
}

// NewTestZones 写一个测试用 zones.json（单服单区）并加载；返回注册表。
func NewTestZones(t *testing.T) *zones.Registry {
	t.Helper()
	path := filepath.Join(t.TempDir(), "zones.json")
	content := fmt.Sprintf(`{
	  "current": {"server": %q, "zone": "z1"},
	  "servers": [{
	    "key": %q, "name": "测试服", "host": "127.0.0.1", "coding": "UTF-8",
	    "zones": [{"key": "z1", "name": "测试区", "port": 2300}]
	  }]
	}`, TestServerKey, TestServerKey)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := zones.New(path)
	if _, err := reg.Load(); err != nil {
		t.Fatalf("加载测试 zones.json 失败: %v", err)
	}
	return reg
}

// NewTestMaps 用裁剪后的**真实**地图表夹具（fixtures/maps/maps.sample.json）创建地图表。
func NewTestMaps(t *testing.T) *maplib.Table {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(TestRoot(t), "fixtures", "maps", "maps.sample.json"))
	if err != nil {
		t.Fatalf("读取地图表夹具失败: %v", err)
	}
	var fx struct {
		Provenance map[string]any    `json:"provenance"`
		Maps       map[string]string `json:"maps"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("地图表夹具非法: %v", err)
	}
	if real, _ := fx.Provenance["real"].(bool); !real {
		t.Fatal("地图表夹具应来自真实数据（provenance.real=true）")
	}
	body, _ := json.MarshalIndent(fx.Maps, "", "  ")
	path := filepath.Join(t.TempDir(), "maps.json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	tb := maplib.New(path)
	if err := tb.Load(); err != nil {
		t.Fatalf("加载地图表失败: %v", err)
	}
	return tb
}

// NewTestAccounts 用裁剪后的**真实账号池**夹具创建账号池（临时文件；返回池与文件路径）。
func NewTestAccounts(t *testing.T) *accounts.Pool {
	t.Helper()
	pool, _ := NewTestAccountsWithPath(t)
	return pool
}

// NewTestAccountsWithPath 同上，但返回池文件路径（用例需要"重新导入/热更新"时用）。
func NewTestAccountsWithPath(t *testing.T) (*accounts.Pool, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(TestRoot(t), "fixtures", "accounts", "accounts.sample.json"))
	if err != nil {
		t.Fatalf("读取账号池夹具失败: %v", err)
	}
	var fx struct {
		Provenance map[string]any    `json:"provenance"`
		Pool       json.RawMessage   `json:"pool"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("账号池夹具非法: %v", err)
	}
	if real, _ := fx.Provenance["real"].(bool); !real {
		t.Fatal("账号池夹具应来自真实数据（provenance.real=true）")
	}
	if how, _ := fx.Provenance["how"].(string); how == "" {
		t.Fatal("账号池夹具缺少 provenance.how（来源必须可查）")
	}
	path := filepath.Join(t.TempDir(), "accounts.json")
	if err := os.WriteFile(path, fx.Pool, 0o644); err != nil {
		t.Fatal(err)
	}
	pool := accounts.New(path)
	if err := pool.Load(); err != nil {
		t.Fatalf("加载账号池失败: %v", err)
	}
	return pool, path
}

// ---------------------------------------------------------------- 假机器人

// FakeRobot 假机器人（真实 TCP 连接，用于控制通道收发测试）。
type FakeRobot struct {
	Conn   net.Conn
	Reader *bufio.Reader
}

// ConnectFakeRobot 连接控制通道（等待服务端接入完成）。
func ConnectFakeRobot(t *testing.T, s *ctrl.Server) *FakeRobot {
	t.Helper()
	conn, err := net.DialTimeout("tcp", s.BoundAddr(), 2*time.Second)
	if err != nil {
		t.Fatalf("连接控制通道 %s 失败: %v", s.BoundAddr(), err)
	}
	f := &FakeRobot{Conn: conn, Reader: bufio.NewReader(conn)}
	Eventually(t, 2*time.Second, s.Connected, "控制通道应显示已连接")
	return f
}

// SendEvent 发送一条事件（JSON + \n）。
func (f *FakeRobot) SendEvent(t *testing.T, ev map[string]any) {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("事件序列化失败: %v", err)
	}
	if _, err := f.Conn.Write(append(raw, '\n')); err != nil {
		t.Fatalf("发送事件失败: %v", err)
	}
}

// SendRaw 发送原始字节（异常报文测试用）。
func (f *FakeRobot) SendRaw(t *testing.T, data []byte) {
	t.Helper()
	if _, err := f.Conn.Write(data); err != nil {
		t.Fatalf("发送原始数据失败: %v", err)
	}
}

// ReadCmd 读取一条下行命令（超时失败）。
func (f *FakeRobot) ReadCmd(t *testing.T, timeout time.Duration) map[string]any {
	t.Helper()
	_ = f.Conn.SetReadDeadline(time.Now().Add(timeout))
	line, err := f.Reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("读取下行命令失败: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(line, &out); err != nil {
		t.Fatalf("下行命令不是合法 JSON: %s", string(line))
	}
	return out
}

// TryReadCmd 尝试读取（超时返回 nil，不算失败）——用于断言"没有收到命令"。
func (f *FakeRobot) TryReadCmd(timeout time.Duration) map[string]any {
	_ = f.Conn.SetReadDeadline(time.Now().Add(timeout))
	line, err := f.Reader.ReadBytes('\n')
	if err != nil {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(line, &out) != nil {
		return nil
	}
	return out
}

// Close 关闭连接。
func (f *FakeRobot) Close() { _ = f.Conn.Close() }

// ---------------------------------------------------------------- 等待/断言

// Eventually 轮询等待条件成立（超时即用例失败）。
func Eventually(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", msg)
}

// WaitEvent 阻塞等待控制通道事件（超时失败）。
func WaitEvent(t *testing.T, s *ctrl.Server, timeout time.Duration) map[string]any {
	t.Helper()
	select {
	case ev := <-s.Events:
		return ev
	case <-time.After(timeout):
		t.Fatalf("等待控制通道事件超时")
		return nil
	}
}

// StoreHasType 运行历史（当天文件）中是否存在指定 type 的事件。
func StoreHasType(st *store.Store, etype string) bool {
	for _, e := range st.ReadTail(500) {
		if s, _ := e["type"].(string); s == etype {
			return true
		}
	}
	return false
}
