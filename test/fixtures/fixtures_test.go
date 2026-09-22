// 夹具元测试：校验所有报文夹具的来源可查（provenance 规则）。
//
// 规则（docs/测试框架.md §4.3 铁律 8）：
//   - real=false 的构造夹具必须显式声明并说明构造依据；
//   - real=true 的真实夹具必须保留 raw 原文，且 event 与 raw 完全一致（不得改字段）。
package fixtures_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"zyctrlcenter/test/testsupport"
)

type fixtureFile struct {
	Provenance map[string]any `json:"provenance"`
	Event      map[string]any `json:"event"`
}

func TestFixtureProvenance(t *testing.T) {
	files := testsupport.FixtureFiles(t)
	if len(files) < 8 {
		t.Fatalf("关键事件夹具应不少于 8 个，实际 %d（新增事件请补夹具）", len(files))
	}
	for _, path := range files {
		name := filepath.Base(path)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", name, err)
		}
		var fx fixtureFile
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatalf("%s 不是合法 JSON: %v", name, err)
		}
		if fx.Provenance == nil {
			t.Fatalf("%s 缺少 provenance（来源必须可查）", name)
		}
		isReal, ok := fx.Provenance["real"].(bool)
		if !ok {
			t.Fatalf("%s provenance.real 必填（bool）", name)
		}
		if _, ok := fx.Provenance["trimmed"].(bool); !ok {
			t.Fatalf("%s provenance.trimmed 必填（bool）", name)
		}
		how, _ := fx.Provenance["how"].(string)
		if strings.TrimSpace(how) == "" {
			t.Fatalf("%s provenance.how 不能为空", name)
		}
		if fx.Event == nil || fx.Event["type"] == nil {
			t.Fatalf("%s event.type 必填", name)
		}

		if isReal {
			rawLine, _ := fx.Provenance["raw"].(string)
			if strings.TrimSpace(rawLine) == "" {
				t.Fatalf("%s 真实夹具必须保留 raw 原文", name)
			}
			var fromRaw map[string]any
			if err := json.Unmarshal([]byte(rawLine), &fromRaw); err != nil {
				t.Fatalf("%s provenance.raw 不是合法 JSON: %v", name, err)
			}
			if !reflect.DeepEqual(fromRaw, fx.Event) {
				t.Fatalf("%s 真实夹具的 event 必须与 raw 原文一致（不得改字段）\nraw  : %v\nevent: %v",
					name, fromRaw, fx.Event)
			}
		} else {
			if !strings.Contains(how, "构造") && !strings.Contains(how, "示例") {
				t.Fatalf("%s 构造夹具的 how 必须说明构造依据（含「构造」或「示例」），实际 %q", name, how)
			}
		}
	}
}

func TestRequiredFixtureTypesExist(t *testing.T) {
	want := []string{
		"task_progress", "robot_state", "error", "ghost_offline", "chain_done",
		"log", "robot_manage_reply", "robot_online", "status_reply",
	}
	have := map[string]bool{}
	for _, path := range testsupport.FixtureFiles(t) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var fx fixtureFile
		if json.Unmarshal(raw, &fx) != nil {
			continue
		}
		if tp, _ := fx.Event["type"].(string); tp != "" {
			have[tp] = true
		}
	}
	for _, tp := range want {
		if !have[tp] {
			t.Errorf("缺少事件 %q 的夹具（test/fixtures/events/）", tp)
		}
	}
}
