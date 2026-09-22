// 任务号 → 任务名（只读游戏配置 task/*.xml）：把机器人上报的编号翻成人话。
//
// 现场背景：大屏"任务"列只显示 2019508 这种编号，没人看得懂；名字本来就在游戏配置里
// （task_entry 的 episode_name，例：捉鬼 / 交付捉鬼任务）。
package api_test

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTaskNamesEndpoint(t *testing.T) {
	env := newTestEnv(t, "")

	// 1) 没放 task 目录：空表但不报错（前端回退显示编号）
	body := getJSON(t, env.srv.URL+"/api/tasknames")
	if body["ok"] != true || body["available"] != false {
		t.Fatalf("没有游戏配置时应返回空表且不报错: %v", body)
	}

	// 2) 造一份最小任务 XML（形状照真实 20195.xml）
	dir := filepath.Join(env.gameConfig, "task")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<task_story>
  <task_entry task_index="2019508" episode_label="日常任务" episode_name="捉鬼" group_index=" 抓鬼" />
  <task_entry task_index="2019511" episode_name="交付捉鬼任务" />
  <task_entry task_index="7001001" name="天命所归" />
</task_story>`
	if err := os.WriteFile(filepath.Join(dir, "20195.xml"), []byte(xml), 0o644); err != nil {
		t.Fatal(err)
	}

	body = getJSON(t, env.srv.URL+"/api/tasknames")
	if body["ok"] != true || body["available"] != true {
		t.Fatalf("有游戏配置时 available 应为 true: %v", body)
	}
	names, _ := body["names"].(map[string]any)
	if names["2019508"] != "捉鬼" {
		t.Fatalf("2019508 应解出「捉鬼」: %v", names)
	}
	if names["2019511"] != "交付捉鬼任务" {
		t.Fatalf("2019511 应解出「交付捉鬼任务」: %v", names)
	}
	if names["7001001"] != "天命所归" {
		t.Fatalf("没有 episode_name 时应回退 name: %v", names)
	}

	// 3) 单查（面板 tooltip / 排障用）
	one := getJSON(t, env.srv.URL+"/api/tasknames?id=2019508")
	if one["name"] != "捉鬼" {
		t.Fatalf("单查应回带名字: %v", one)
	}
}
