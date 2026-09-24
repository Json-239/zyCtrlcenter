// 定时任务参数落盘：start/stop 写 <DataDir>/autotask.json，重启后 LoadAutoTask 恢复
// （只有"上次是启用"的策略会自动重新启用；配置原样带回）。
package api_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"zyctrlcenter/internal/services/autotask"
)

func TestAutoTaskPersistSurvivesRestart(t *testing.T) {
	env := newTestEnv(t, "")

	// 启动两套策略（ghost 带保持数）
	_, res := postJSON(t, env.srv.URL+"/api/autotask/start", map[string]any{
		"kind": "ghost", "interval_sec": 300, "jitter_sec": 300,
		"batch_min": 1, "batch_max": 5, "target_online": 50,
	}, nil)
	if res["ok"] != true {
		t.Fatalf("启动 ghost 策略失败: %v", res)
	}
	if _, res2 := postJSON(t, env.srv.URL+"/api/autotask/start", map[string]any{
		"kind": "newbie", "interval_sec": 300, "jitter_sec": 300, "target_online": 5,
	}, nil); res2["ok"] != true {
		t.Fatalf("启动 newbie 策略失败: %v", res2)
	}

	// 文件已落盘且内容正确
	path := filepath.Join(env.cfg.DataDir, "autotask.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("应写出 %s: %v", path, err)
	}
	var f struct {
		Tasks map[string]struct {
			Config  autotask.Config `json:"config"`
			Enabled bool            `json:"enabled"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("落盘 JSON 应可解析: %v", err)
	}
	if g := f.Tasks["ghost"]; !g.Enabled || g.Config.TargetOnline != 50 || g.Config.IntervalSec != 300 {
		t.Fatalf("ghost 配置应原样落盘: %+v", f.Tasks)
	}

	// stop 一套 → 落盘同步更新（用户停过的重启后不许自动拉起）
	if _, res3 := postJSON(t, env.srv.URL+"/api/autotask/stop", map[string]any{"kind": "newbie"}, nil); res3["ok"] != true {
		t.Fatalf("停止 newbie 失败: %v", res3)
	}
	b, _ = os.ReadFile(path)
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if f.Tasks["newbie"].Enabled {
		t.Fatalf("停过的策略落盘应为 enabled=false: %+v", f.Tasks)
	}

	// 模拟"重启"：新建一个环境（同一 DataDir）+ LoadAutoTask
	env2 := newTestEnvInDir(t, "", env.cfg.DataDir)
	env2.api.LoadAutoTask()
	st := env2.api.AutoTask.States()
	if !st[autotask.KindGhost].Enabled || st[autotask.KindGhost].Target != 50 {
		t.Fatalf("重启后 ghost 应自动恢复（含保持数 50）: %+v", st[autotask.KindGhost])
	}
	if st[autotask.KindNewbie].Enabled {
		t.Fatalf("停过的 newbie 重启后不该自动拉起: %+v", st[autotask.KindNewbie])
	}
}

// 2026-09-24（17:23 事故防回归）：**未启用的分享日常玩法，LoadAutoTask 不得清"今日已派"台账**。
// 现场：fenghuo 灰度手动派 3 号（池未启用），重启被误清 3 条 share_daily_宫廷10 →
// 意图/候选链断 → 池永远没号（用户报"开了烽火没有号被拉起"）。
func TestLoadAutoTaskKeepsShareDailyLedgerForDisabledPool(t *testing.T) {
	env := newTestEnv(t, "")
	// 先启动一套无关策略，保证 autotask.json 存在（LoadAutoTask 才会进入逐项循环）
	if _, res := postJSON(t, env.srv.URL+"/api/autotask/start", map[string]any{
		"kind": "ghost", "interval_sec": 300, "jitter_sec": 300, "target_online": 50,
	}, nil); res["ok"] != true {
		t.Fatalf("启动 ghost 策略失败: %v", res)
	}
	// 等价"灰度手动派过烽火、池未启用"：台账有、池无记录
	acc := "robot0001024@xy3.com"
	env.st.MarkShareDailyAssigned(acc, "share_daily_宫廷10")

	env.api.LoadAutoTask() // 未启用的 fenghuo（!ok）→ 不得清台账

	if !env.st.ShareDailyAssignedToday(acc, "share_daily_宫廷10") {
		t.Fatalf("未启用玩法的台账不得被 LoadAutoTask 清掉（17:23 事故防回归）")
	}
}
