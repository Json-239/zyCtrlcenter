// 定时任务参数落盘：重启中控不丢"保持数/间隔/批量/启用状态"。
//
// 为什么：三套策略的配置与开关都在内存（autotask.Runner）里，中控一重启就全丢 ——
// 2026-09-21 生产实测：重启后 target_online 变 0（不限）、enabled=false，保持数失效
// （面板显示"目标 0"），得人工重新配。
//
//	存：每次 start/stop 把三套的 config + enabled 写 <DataDir>/autotask.json；
//	载：中控启动时读回，**只自动重新启用"上次是启用"的策略**（用户主动停过的保持停止）。
package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"zyctrlcenter/internal/services/autotask"
)

// autoTaskPersistItem 单套策略的落盘形态。
type autoTaskPersistItem struct {
	Config  autotask.Config `json:"config"`
	Enabled bool            `json:"enabled"`
}

// autoTaskPersistFile 落盘文件（key = kind：newbie / ghost）。
type autoTaskPersistFile struct {
	Tasks     map[string]autoTaskPersistItem `json:"tasks"`
	UpdatedAt time.Time                      `json:"updated_at"`
}

// autoTaskFilePath 落盘路径（<DataDir>/autotask.json）。
func (a *API) autoTaskFilePath() string {
	dir := "data"
	if a.Cfg != nil && a.Cfg.DataDir != "" {
		dir = a.Cfg.DataDir
	}
	return filepath.Join(dir, "autotask.json")
}

// SaveAutoTask 把三套策略的配置与启用状态写盘（失败只记日志，不影响主流程）。
func (a *API) SaveAutoTask() {
	if a.AutoTask == nil {
		return
	}
	st := a.AutoTask.States()
	out := autoTaskPersistFile{Tasks: map[string]autoTaskPersistItem{}, UpdatedAt: time.Now()}
	for _, k := range autotask.Kinds {
		s, ok := st[k]
		if !ok {
			continue
		}
		out.Tasks[string(k)] = autoTaskPersistItem{Config: s.Config, Enabled: s.Enabled}
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(a.autoTaskFilePath(), b, 0o644); err != nil {
		a.Log.Printf("[AUTOTASK] 参数落盘失败: %v", err)
		return
	}
	a.Log.Printf("[AUTOTASK] 参数已落盘: %s", a.autoTaskFilePath())
}

// LoadAutoTask 启动时恢复上次的策略（配置原样带回；只恢复上次启用的那套）。
func (a *API) LoadAutoTask() {
	if a.AutoTask == nil {
		return
	}
	b, err := os.ReadFile(a.autoTaskFilePath())
	if err != nil {
		return // 没有文件 = 首次运行，等人工 start
	}
	var f autoTaskPersistFile
	if err := json.Unmarshal(b, &f); err != nil {
		a.Log.Printf("[AUTOTASK] 参数文件解析失败（忽略，等人工重配）: %v", err)
		return
	}
	for _, k := range autotask.Kinds {
		it, ok := f.Tasks[string(k)]
		if !ok || !it.Enabled {
			// 2026-09-24：未启用的分享日常玩法 —— 顺手清"今日已派"台账（幂等兜底）。
			// 停用动作发生在 handleAutoTaskStop（那里已清）；这里是跨重启的历史残留兜底，
			// 保证"该玩法未启用 ⇒ 不让路 ⇒ 号回抓鬼"在重启后也成立（口径见 shareDailyBusyForGhost）。
			if _, isDaily := shareDailyKindFromString(string(k)); isDaily {
				a.clearShareDailyAssignedOf(k, "启动时该玩法未启用")
			}
			continue // 上次没开（或没记录）→ 保持停止，不擅自拉起
		}
		cfg := it.Config
		cfg.Kind = k
		if err := a.AutoTask.Start(k, cfg); err != nil {
			a.Log.Printf("[AUTOTASK] 恢复 %s 失败: %v", k, err)
			continue
		}
		a.Log.Printf("[AUTOTASK] 已恢复上次的 %s（间隔 %ds+随机 %ds，每轮 %d~%d，保持在线 %d）",
			k.Label(), cfg.IntervalSec, cfg.JitterSec, cfg.BatchMin, cfg.BatchMax, cfg.TargetOnline)
	}
}
