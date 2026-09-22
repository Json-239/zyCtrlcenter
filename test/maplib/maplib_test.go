// maplib 测试：地图名表加载/查询/热更新/容错 + 客户端格子坐标换算。
package maplib_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"zyctrlcenter/internal/maplib"
	"zyctrlcenter/test/testsupport"
)

// writeMapsFixture 把裁剪后的真实地图表（夹具）写到临时文件，返回路径与期望表。
func writeMapsFixture(t *testing.T) (string, map[string]string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(testsupport.TestRoot(t), "fixtures", "maps", "maps.sample.json"))
	if err != nil {
		t.Fatalf("读取地图夹具失败: %v", err)
	}
	var fx struct {
		Provenance map[string]any    `json:"provenance"`
		Maps       map[string]string `json:"maps"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("地图夹具非法: %v", err)
	}
	if real, _ := fx.Provenance["real"].(bool); !real {
		t.Fatal("地图夹具应来自真实数据（provenance.real=true）")
	}
	if how, _ := fx.Provenance["how"].(string); how == "" {
		t.Fatal("地图夹具缺少 provenance.how（来源必须可查）")
	}
	out := make(map[string]string, len(fx.Maps))
	for k, v := range fx.Maps {
		if k != "_comment" {
			out[k] = v
		}
	}
	path := filepath.Join(t.TempDir(), "maps.json")
	body, _ := json.MarshalIndent(fx.Maps, "", "  ")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, out
}

func TestLoadAndLookup(t *testing.T) {
	path, want := writeMapsFixture(t)
	tb := maplib.New(path)
	if err := tb.Load(); err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if tb.Count() != len(want) {
		t.Fatalf("条目数应为 %d，实际 %d（注释键应被跳过）", len(want), tb.Count())
	}
	if !tb.Loaded() {
		t.Fatal("Loaded 应为 true")
	}
	// 关键词抽查（真实数据：11=长安东市集、24=幽冥界）
	for _, c := range []struct {
		id   int
		name string
	}{{11, "长安东市集"}, {24, "幽冥界"}, {1, "彩荷清池"}} {
		if got := tb.Name(c.id); got != c.name {
			t.Fatalf("mapid=%d 应为 %q，实际 %q", c.id, c.name, got)
		}
	}
	// 未知 id → 空串（面板回退显示 #id）
	if got := tb.Name(9999); got != "" {
		t.Fatalf("未知 mapid 应返回空串，实际 %q", got)
	}
	if got := tb.NameStr("11"); got != "长安东市集" {
		t.Fatalf("NameStr 字符串键查询失败: %q", got)
	}
	if all := tb.All(); len(all) != len(want) || all["24"] != "幽冥界" {
		t.Fatalf("All() 内容不符: %v", all)
	}
}

func TestMissingFileIsEmptyNotError(t *testing.T) {
	tb := maplib.New(filepath.Join(t.TempDir(), "nope.json"))
	if err := tb.Load(); err != nil {
		t.Fatalf("文件不存在不应报错（视为空表）: %v", err)
	}
	if tb.Count() != 0 || tb.Loaded() {
		t.Fatalf("空表状态不符: count=%d loaded=%v", tb.Count(), tb.Loaded())
	}
	if got := tb.Name(11); got != "" {
		t.Fatalf("空表查询应为空串，实际 %q", got)
	}
}

func TestBrokenFileKeepsOldData(t *testing.T) {
	path, _ := writeMapsFixture(t)
	tb := maplib.New(path)
	if err := tb.Load(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tb.Load(); err == nil {
		t.Fatal("损坏文件应返回错误")
	}
	// 上一次成功加载的数据仍可用（Load 失败不应把表清空）
	if got := tb.Name(24); got != "幽冥界" {
		t.Fatalf("损坏后应保留上次数据，实际 %q", got)
	}
}

func TestHotReloadOnFileChange(t *testing.T) {
	path, _ := writeMapsFixture(t)
	tb := maplib.New(path)
	if err := tb.Load(); err != nil {
		t.Fatal(err)
	}
	if got := tb.Name(35); got != "建邺城" {
		t.Fatalf("初始值不符: %q", got)
	}
	// 改文件（新增一张图）→ 查询时应自动热更新
	raw, _ := os.ReadFile(path)
	var obj map[string]string
	_ = json.Unmarshal(raw, &obj)
	obj["99"] = "新地图"
	body, _ := json.MarshalIndent(obj, "", "  ")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := tb.Name(99); got != "新地图" {
		t.Fatalf("热更新失败（应检测到文件变化），实际 %q", got)
	}
	if tb.Count() != 7 {
		t.Fatalf("热更新后条目数应为 7，实际 %d", tb.Count())
	}
}

func TestGridPosClientCoordinate(t *testing.T) {
	// 口径（与参考实现一致，客户端地图显示 = 像素/16 向下取整）
	cases := []struct {
		x, y   int
		gx, gy int
	}{
		{2108, 809, 131, 50}, // 2108/16=131.75→131, 809/16=50.5→50
		{0, 0, 0, 0},
		{15, 31, 0, 1},
		{1736, 1064, 108, 66},
	}
	for _, c := range cases {
		got := maplib.GridPos(c.x, c.y)
		if len(got) != 2 || got[0] != c.gx || got[1] != c.gy {
			t.Fatalf("像素(%d,%d) → 格子应为 (%d,%d)，实际 %v", c.x, c.y, c.gx, c.gy, got)
		}
	}
	if maplib.GridCell != 16 {
		t.Fatalf("GridCell 应为 16，实际 %d", maplib.GridCell)
	}
	// 可覆盖格子尺寸（比如某图 8px/格）
	if got := maplib.GridPosWith(64, 32, 8); got[0] != 8 || got[1] != 4 {
		t.Fatalf("自定义格尺寸换算不符: %v", got)
	}
	// 非法格尺寸回退默认值
	if got := maplib.GridPosWith(64, 32, 0); got[0] != 4 || got[1] != 2 {
		t.Fatalf("格尺寸非法应回退 16: %v", got)
	}
}
