// 脚本热更接口：POST /api/robot/reload_scripts → 机器人收到 reload_scripts
// （进程内 importlib.reload，免重启机器人就能把脚本补丁铺到现场）。
package api_test

import (
	"testing"
	"time"

	"zyctrlcenter/test/testsupport"
)

func TestReloadScriptsSendsCommand(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	_, res := postJSON(t, env.srv.URL+"/api/robot/reload_scripts",
		map[string]any{"modules": []any{"quest_engine"}}, nil)
	if res["ok"] != true {
		t.Fatalf("应下发成功: %v", res)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "reload_scripts" {
		t.Fatalf("机器人应收到 reload_scripts: %v", cmd)
	}
	mods := asSlice(cmd["modules"])
	if len(mods) != 1 || mods[0] != "quest_engine" {
		t.Fatalf("应带 modules 原样下发: %v", cmd)
	}
}

// 省略 modules → 命令里不带该字段（机器人端用默认 quest_engine）。
func TestReloadScriptsOmitsModules(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	_, res := postJSON(t, env.srv.URL+"/api/robot/reload_scripts", map[string]any{}, nil)
	if res["ok"] != true {
		t.Fatalf("省略 modules 也应可下发: %v", res)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "reload_scripts" {
		t.Fatalf("机器人应收到 reload_scripts: %v", cmd)
	}
	if cmd["modules"] != nil {
		t.Fatalf("省略 modules 时命令里不该带该字段: %v", cmd)
	}
}
