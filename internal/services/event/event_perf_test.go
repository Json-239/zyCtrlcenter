// robot_state 广播剥 bag（2026-10-12 性能止血）测试：
//   - AfterEvent 广播的 robot_state 不含 bag（拷贝剥离，不动原事件）；
//   - 原事件对象保留 bag（状态表/历史日志等其他消费仍依赖）；
//   - 无 bag 的 robot_state 与其他类型原样广播；
//   - cloneWithout 键保留/去除语义。
package event

import (
	"testing"

	"zyctrlcenter/internal/config"
	"zyctrlcenter/internal/ctrl"
	"zyctrlcenter/internal/logging"
	"zyctrlcenter/internal/state"
	"zyctrlcenter/internal/store"
)

func TestAfterEventStripsRobotStateBag(t *testing.T) {
	cfg := config.Default()
	st := state.New()
	runStore := store.New(t.TempDir(), 30, 500)
	log := logging.New(t.TempDir(), 30)
	defer func() { _ = log.Close() }()
	ch := ctrl.New("127.0.0.1", 0, nil) // 只构造，不监听
	h := New(cfg, st, runStore, ch, log)

	var captured map[string]any
	h.SetBroadcast(func(obj map[string]any) { captured = obj })

	// 带 bag 的 robot_state：广播拷贝剥离，原对象不动
	ev := map[string]any{
		"type": "robot_state", "account": "strip@xy3.com", "state": "NAV",
		"bag": map[string]any{"items": []any{map[string]any{"slot": 2, "item": "盘龙棍"}}},
		"hp":  []any{1.0, 2.0},
	}
	h.AfterEvent(ev)
	if captured == nil {
		t.Fatal("robot_state 应被广播")
	}
	if _, ok := captured["bag"]; ok {
		t.Fatalf("广播对象不应带 bag：%#v", captured["bag"])
	}
	if captured["state"] != "NAV" || captured["account"] != "strip@xy3.com" {
		t.Fatalf("其余字段应原样保留：%v", captured)
	}
	if _, ok := captured["hp"]; !ok {
		t.Fatal("hp 等其他字段应保留")
	}
	bag, ok := ev["bag"].(map[string]any)
	if !ok {
		t.Fatal("原事件对象不应被就地修改（其他消费依赖 bag）")
	}
	if !containsItem(bag, "盘龙棍") {
		t.Fatalf("原 bag 内容应完整：%#v", bag)
	}

	// 无 bag 的 robot_state：原样广播
	captured = nil
	h.AfterEvent(map[string]any{"type": "robot_state", "account": "a@x.com", "state": "READY"})
	if captured == nil || captured["state"] != "READY" {
		t.Fatalf("无 bag 的 robot_state 应原样广播：%v", captured)
	}
	if _, ok := captured["bag"]; ok {
		t.Fatal("无 bag 的事件不应凭空多出 bag 键")
	}

	// 其他类型不受影响
	captured = nil
	h.AfterEvent(map[string]any{"type": "robot_online", "account": "a@x.com", "level": 40})
	if captured == nil || captured["type"] != "robot_online" {
		t.Fatalf("robot_online 应原样广播：%v", captured)
	}
}

func containsItem(bag map[string]any, want string) bool {
	items, _ := bag["items"].([]any)
	for _, it := range items {
		if m, _ := it.(map[string]any); m["item"] == want {
			return true
		}
	}
	return false
}

func TestCloneWithout(t *testing.T) {
	m := map[string]any{"a": 1, "b": 2}
	if got := cloneWithout(m, "c"); len(got) != 2 {
		t.Fatalf("未命中 drop 键应原样返回 2 键，实得 %d", len(got))
	}
	cp := cloneWithout(m, "a")
	if _, ok := cp["a"]; ok {
		t.Fatal("drop 键应被移除")
	}
	if cp["b"] != 2 {
		t.Fatal("其余键应保留")
	}
	if _, ok := m["a"]; !ok {
		t.Fatal("原 map 不应被修改")
	}
}
