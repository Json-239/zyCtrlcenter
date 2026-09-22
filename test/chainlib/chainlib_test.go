// chainlib 模块测试：文件驱动加载 / 缺省补齐 / 未知字段与形状透传 / 目录扫描容错。
package chainlib_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"zyctrlcenter/internal/chainlib"
	"zyctrlcenter/test/testsupport"
)

func TestBuildMissingFileReturnsErrNotFound(t *testing.T) {
	if _, err := chainlib.Build("nope", t.TempDir()); !errors.Is(err, chainlib.ErrNotFound) {
		t.Fatalf("链文件缺失应返回 ErrNotFound（调用方据此只下发 chain_id），实际 %v", err)
	}
}

func TestBuildRejectsEmptyID(t *testing.T) {
	if _, err := chainlib.Build("  ", t.TempDir()); err == nil {
		t.Fatal("chain_id 为空必须报错")
	}
}

func TestBuildLoadsAndNormalizes(t *testing.T) {
	dir := writeChain(t, "demo", `{"chain_id":"demo","start_task":1,"end_task":2}`)

	c, err := chainlib.Build("demo", dir)
	if err != nil {
		t.Fatalf("加载链数据失败: %v", err)
	}
	if c.ChainID != "demo" || c.StartTask != 1 || c.EndTask != 2 {
		t.Fatalf("字段未正确加载: %+v", c)
	}
	if c.GridCell != 16 {
		t.Fatalf("grid_cell 缺省应为 16，实际 %d", c.GridCell)
	}
	// 空表必须是空对象/空数组而非 nil（机器人端按 JSON 对象解析）
	if c.NPCs == nil || c.TaskHints == nil || c.MapGrids == nil || c.Dijkstra == nil {
		t.Fatalf("缺省表不应为 nil: %+v", c)
	}
	if c.TaskOrder == nil {
		t.Fatal("task_order 应为空数组而非 nil")
	}
}

// 真实导出数据（裁剪夹具）驱动：未知字段与字段形状必须原样透传。
func TestBuildPreservesUnknownFieldsAndShapes(t *testing.T) {
	fx := loadChainFixture(t, "zhuaogui_nav.sample.json")
	dir := writeChain(t, "nav", string(fx.Chain))

	c, err := chainlib.Build("nav", dir)
	if err != nil {
		t.Fatalf("加载真实形状链数据失败: %v", err)
	}
	if c.ChainID != "zhuaogui" {
		t.Fatalf("chain_id 不符: %s", c.ChainID)
	}

	var got, want map[string]any
	if err := json.Unmarshal(marshal(t, c), &got); err != nil {
		t.Fatalf("marshal 结果非法: %v", err)
	}
	if err := json.Unmarshal(fx.Chain, &want); err != nil {
		t.Fatal(err)
	}

	// 未声明字段（真实数据里存在，结构体里没有）必须保留且内容一致
	for _, k := range []string{"ghost_map_pos", "item_meta", "ghost_maps"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("未知字段 %s 必须原样透传（否则机器人端拿不到数据）: %v", k, got)
		}
		if !reflect.DeepEqual(got[k], want[k]) {
			t.Fatalf("%s 内容被改动\n got: %v\nwant: %v", k, got[k], want[k])
		}
	}
	// 形状差异字段：npcs 是扁平三元组；task_order 是任意形状数组
	if !reflect.DeepEqual(got["npcs"], want["npcs"]) {
		t.Fatalf("npcs 形状必须原样保留\n got: %v\nwant: %v", got["npcs"], want["npcs"])
	}
	if !reflect.DeepEqual(got["task_order"], want["task_order"]) {
		t.Fatalf("task_order 必须原样保留\n got: %v\nwant: %v", got["task_order"], want["task_order"])
	}
	if got["grid_cell"] != float64(16) {
		t.Fatalf("grid_cell 应保留文件值: %v", got["grid_cell"])
	}
}

func TestListScansDirectory(t *testing.T) {
	dir := t.TempDir()

	// 目录不存在：返回空列表而不是报错
	if items := chainlib.List(filepath.Join(dir, "missing")); len(items) != 0 {
		t.Fatalf("目录不存在应返回空列表，实际 %v", items)
	}
	if items := chainlib.List(dir); len(items) != 0 {
		t.Fatalf("空目录应返回空列表，实际 %v", items)
	}

	writeChainTo(t, dir, "chain_b", `{"chain_id":"chain_b","start_task":100,"end_task":200,"task_order":[{},{}]}`)
	// 文件名与文件内 chain_id 不同：列表 id 用文件名（可寻址），chain_id 另列
	writeChainTo(t, dir, "chain_file", `{"chain_id":"inner_id","start_task":1,"end_task":2}`)
	writeChainTo(t, dir, "_template", `{"chain_id":"template"}`) // "_" 开头：模板，不参与列表
	writeChainTo(t, dir, "broken", `{ this is not json }`)       // 损坏文件：计入但带 Error
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil { // 子目录：跳过
		t.Fatal(err)
	}

	items := chainlib.List(dir)
	if len(items) != 3 {
		t.Fatalf("应列出 3 个文件（跳过 _template 与子目录），实际 %v", items)
	}
	byID := map[string]chainlib.Info{}
	for _, it := range items {
		byID[it.ID] = it
	}
	good, ok := byID["chain_b"]
	if !ok {
		t.Fatalf("应有 chain_b，实际 %v", items)
	}
	if good.TaskCount != 2 || good.StartTask != 100 || good.EndTask != 200 {
		t.Fatalf("chain_b 条目字段不符: %+v", good)
	}
	if good.File == "" {
		t.Fatal("列表条目应给出文件相对路径")
	}
	// 文件名与文件内 chain_id 不同：ID 用文件名（保证 ?id= 可寻址），ChainID 单列
	mixed, ok := byID["chain_file"]
	if !ok {
		t.Fatalf("应有 chain_file，实际 %v", items)
	}
	if mixed.ChainID != "inner_id" {
		t.Fatalf("列表应透出文件内 chain_id，实际 %+v", mixed)
	}
	if _, ok := byID["inner_id"]; ok {
		t.Fatal("列表 id 必须是文件名（否则面板按 id 查询会查不到）")
	}
	bad, ok := byID["broken"]
	if !ok || bad.Error == "" {
		t.Fatalf("损坏文件应在列表中带 Error 且不影响其它条目: %+v", bad)
	}
}

func TestFilePath(t *testing.T) {
	got := chainlib.FilePath("demo", filepath.Join("data", "chains"))
	if got != filepath.Join("data", "chains", "demo.json") {
		t.Fatalf("FilePath 不符: %s", got)
	}
}

// ---------------- 工具 ----------------

type chainFixture struct {
	Provenance map[string]any  `json:"provenance"`
	Chain      json.RawMessage `json:"chain"`
}

func loadChainFixture(t *testing.T, name string) chainFixture {
	t.Helper()
	path := filepath.Join(testsupport.TestRoot(t), "fixtures", "chains", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取链夹具 %s 失败: %v", path, err)
	}
	var fx chainFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("链夹具 %s 非法: %v", path, err)
	}
	if fx.Chain == nil {
		t.Fatalf("链夹具 %s 缺少 chain 字段", path)
	}
	if real, _ := fx.Provenance["real"].(bool); !real {
		t.Fatalf("链夹具 %s 应来自真实导出数据（provenance.real=true）", name)
	}
	if how, _ := fx.Provenance["how"].(string); how == "" {
		t.Fatalf("链夹具 %s 缺少 provenance.how（来源必须可查）", name)
	}
	return fx
}

// writeChain 造一个独立临时链目录（只含一个链文件）。
func writeChain(t *testing.T, id, content string) string {
	t.Helper()
	dir := t.TempDir()
	writeChainTo(t, dir, id, content)
	return dir
}

// writeChainTo 往指定目录写链文件。
func writeChainTo(t *testing.T, dir, id, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func marshal(t *testing.T, v any) []byte {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal 失败: %v", err)
	}
	return out
}
