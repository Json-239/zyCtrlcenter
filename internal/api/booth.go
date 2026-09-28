// 摆摊配置（离线摆摊 · 单次挂多久）：中控侧配置 + 机器人端计划文件同步。
//
// 背景（2026-09-28 用户需求）：摆摊链路已全链打通并生产验证，但"单次挂多久"
// 写死在机器人端计划文件 `script/booth_selftest.json` 的 `offline_minutes` 字段里
// （robot0001032 的摊 = 480 分钟）。这里给面板一个配置入口。
//
// 机制（最小闭环）：
//  1. 中控维护 `data/booth_config.json`（不存在 = 默认 480 分钟）；
//  2. POST 校验 1..480（服务端 VIP0 单次上限 8h=480 分钟）→ 原子落盘（.tmp+rename）；
//  3. 再**尽力**把 offline_minutes 同步进机器人端计划文件（存在的副本都同步）——
//     只替换该字段的值（逐行文本替换），其它字段/顺序/缩进/_note 一律原样保留；
//     同步失败不影响配置保存（返回 plan_synced=false + plan_errors 明细）。
//
// 生效范围：机器人端**开摊时**才读计划文件 → 只影响**下一次**挂摊；
// 当前已挂的摊要改时长必须收摊重挂（面板文案同口径）。
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
)

// 摆摊单次时长（分钟）允许范围：下限 1；上限 480 = 服务端 VIP0 单次 8h。
const (
	boothMinutesMin     = 1
	boothMinutesMax     = 480
	boothMinutesDefault = 480
	boothPlanFile       = "booth_selftest.json"
	boothConfigFile     = "booth_config.json"
)

// boothMu 保护"配置落盘 + 计划文件同步"的写路径（两个并发 POST 不互相踩）。
var boothMu sync.Mutex

// BoothConfig 摆摊配置（落盘 data/booth_config.json，重启后保留）。
type BoothConfig struct {
	OfflineMinutes int `json:"offline_minutes"`
}

// boothConfigPath 中控侧配置文件路径。
func (a *API) boothConfigPath() string {
	return filepath.Join(a.Cfg.DataDir, boothConfigFile)
}

// boothPlanPaths 机器人端摆摊计划文件的候选路径（存在的都会同步；去重保序）：
//
//  1. 当前部署目录 `<DeployDir>/script/booth_selftest.json`
//     （生产由 run_ctrlcenter.bat 的 ZYROBOT_DEPLOY 指到 .../deploy/single_robot_zy）；
//  2. 本仓库保存的部署副本 `<BaseDir>/deploy/zones/prod-240-2300/script/booth_selftest.json`
//     （tools/*_selftest.py 校验的那份，2300 区）；
//  3. 参考项目部署副本（与 config.defaultDeployDir 同口径的兜底）：
//     `<BaseDir>/../xm/2d-xiyou-server/robot/deploy/single_robot_zy/script/booth_selftest.json`。
func (a *API) boothPlanPaths() []string {
	cands := []string{
		filepath.Join(a.Cfg.DeployDir, "script", boothPlanFile),
		filepath.Join(a.Cfg.BaseDir, "deploy", "zones", "prod-240-2300", "script", boothPlanFile),
		filepath.Join(a.Cfg.BaseDir, "..", "xm", "2d-xiyou-server", "robot", "deploy", "single_robot_zy", "script", boothPlanFile),
	}
	out := make([]string, 0, len(cands))
	seen := map[string]bool{}
	for _, p := range cands {
		p = filepath.Clean(p)
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// existingBoothPlans 当前真实存在的计划文件副本（GET 展示/排障用；不存在的不列）。
func (a *API) existingBoothPlans() []string {
	out := []string{}
	for _, p := range a.boothPlanPaths() {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			out = append(out, p)
		}
	}
	return out
}

// loadBoothConfig 读中控侧配置；不存在/损坏/值非法 → 默认 480（损坏时打日志，不拦面板）。
func (a *API) loadBoothConfig() BoothConfig {
	def := BoothConfig{OfflineMinutes: boothMinutesDefault}
	raw, err := os.ReadFile(a.boothConfigPath())
	if err != nil {
		if !os.IsNotExist(err) {
			a.Log.Printf("[BOOTH] 配置读取失败（用默认 %d 分钟）: %v", boothMinutesDefault, err)
		}
		return def
	}
	var cfg BoothConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		a.Log.Printf("[BOOTH] 配置 %s 解析失败（用默认 %d 分钟）: %v", a.boothConfigPath(), boothMinutesDefault, err)
		return def
	}
	if cfg.OfflineMinutes < boothMinutesMin || cfg.OfflineMinutes > boothMinutesMax {
		a.Log.Printf("[BOOTH] 配置值 %d 越界（%d..%d），用默认 %d 分钟", cfg.OfflineMinutes,
			boothMinutesMin, boothMinutesMax, boothMinutesDefault)
		return def
	}
	return cfg
}

// saveBoothConfig 原子落盘 data/booth_config.json（tmp + rename）。
func (a *API) saveBoothConfig(minutes int) error {
	path := a.boothConfigPath()
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(BoothConfig{OfflineMinutes: minutes}, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// boothOfflineMinutesRe 匹配计划文件里的 `"offline_minutes": <数字>`（行首缩进任意）。
var boothOfflineMinutesRe = regexp.MustCompile(`(?m)^(\s*"offline_minutes"\s*:\s*)\d+`)

// syncBoothPlanFiles 把 offline_minutes 写进机器人端计划文件（存在的候选副本全同步）。
//
// 只替换该字段的值（正则逐行替换），其它字段/顺序/缩进/备注一律原样保留；
// 先做 JSON 有效性 + 字段存在性校验（手工写坏的文件跳过并记明细）；.tmp + rename 原子写。
// 返回 (已同步路径, 失败说明)。
func (a *API) syncBoothPlanFiles(minutes int) (synced []string, errs []string) {
	repl := []byte("${1}" + strconv.Itoa(minutes))
	for _, path := range a.boothPlanPaths() {
		raw, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				errs = append(errs, fmt.Sprintf("%s 读取失败: %v", path, err))
			}
			continue // 不存在 = 不是这份副本，跳过（不算错误）
		}
		var probe map[string]any
		if err := json.Unmarshal(raw, &probe); err != nil {
			errs = append(errs, fmt.Sprintf("%s 不是合法 JSON，跳过: %v", path, err))
			continue
		}
		if _, ok := probe["offline_minutes"]; !ok {
			errs = append(errs, fmt.Sprintf("%s 里没有 offline_minutes 字段，跳过", path))
			continue
		}
		newRaw := boothOfflineMinutesRe.ReplaceAll(raw, repl)
		if bytes.Equal(newRaw, raw) {
			synced = append(synced, path) // 已是目标值：无需重写
			continue
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, newRaw, 0o644); err != nil {
			errs = append(errs, fmt.Sprintf("%s 写入失败: %v", path, err))
			continue
		}
		if err := os.Rename(tmp, path); err != nil {
			errs = append(errs, fmt.Sprintf("%s 重命名失败: %v", path, err))
			continue
		}
		synced = append(synced, path)
	}
	return synced, errs
}

// ---------------------------------------------------------------- 接口

// handleBoothConfigGet GET /api/booth/config（只读）：当前配置 + 允许范围 + 已发现的计划文件。
func (a *API) handleBoothConfigGet(w http.ResponseWriter, r *http.Request) {
	cfg := a.loadBoothConfig()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "offline_minutes": cfg.OfflineMinutes,
		"min": boothMinutesMin, "max": boothMinutesMax,
		"plan_files": a.existingBoothPlans(),
	})
}

// handleBoothConfigPost POST /api/booth/config {"offline_minutes":N}（写接口，可选鉴权）。
//
//	校验 1..480 → 落盘 data/booth_config.json → 尽力同步机器人端计划文件。
//	返回 {"ok":true,"saved":N,"plan_synced":bool,"plan_files":[...][,"plan_errors":[...]]}。
func (a *API) handleBoothConfigPost(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	minutes := toInt(body["offline_minutes"], 0)
	if minutes < boothMinutesMin || minutes > boothMinutesMax {
		msg := fmt.Sprintf("offline_minutes 必须是 %d..%d 之间的整数（分钟）", boothMinutesMin, boothMinutesMax)
		if minutes > boothMinutesMax {
			msg += "：服务端 VIP0 单次上限 8h=480 分钟，超了会被服务端拒绝"
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": msg})
		return
	}
	boothMu.Lock()
	err := a.saveBoothConfig(minutes)
	var synced, errs []string
	if err == nil {
		synced, errs = a.syncBoothPlanFiles(minutes)
	}
	boothMu.Unlock()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "配置落盘失败：" + err.Error()})
		return
	}
	note := "（仅影响下一次挂摊；当前已挂的摊需收摊重挂才能改）"
	resp := map[string]any{
		"ok": true, "saved": minutes,
		"plan_synced": len(synced) > 0, "plan_files": synced,
	}
	if len(errs) > 0 {
		resp["plan_errors"] = errs
	}
	switch {
	case len(synced) > 0:
		resp["msg"] = fmt.Sprintf("已保存 %d 分钟，并同步 %d 份摆摊计划文件%s", minutes, len(synced), note)
	case len(errs) > 0:
		resp["msg"] = fmt.Sprintf("已保存 %d 分钟；计划文件同步失败（见 plan_errors 明细）%s", minutes, note)
	default:
		resp["msg"] = fmt.Sprintf("已保存 %d 分钟；未找到机器人端摆摊计划文件（未同步）%s", minutes, note)
	}
	a.Log.Printf("[BOOTH] 配置更新：offline_minutes=%d；计划文件同步 %v；失败 %v", minutes, synced, errs)
	if a.Store != nil {
		a.Store.LogEvent(map[string]any{"type": "api", "action": "booth_config_set",
			"zone": a.currentZoneKey(), "offline_minutes": minutes, "plan_files": synced})
	}
	writeJSON(w, http.StatusOK, resp)
}
