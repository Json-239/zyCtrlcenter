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
//
// 一键下发/停止（2026-09-29 用户需求："怎么下发摆摊指令"→ 面板一键启停）：
//
//	POST /api/booth/start —— 写双份计划文件（enabled=true + 受控字段全量）+
//	  号不在目标图(11) 时复用游荡通道（DispatchRoamEx）把号送过去；到图后机器人端
//	  自动开摊（booth.py 的 tick 读计划文件签名，改文件即热生效，无需 reload）。
//	POST /api/booth/stop  —— 写 enabled=false（双份）；号离线（可能挂离线摊中）
//	  不自动拉起，只提示"需拉起号才会收摊"。
//
// 链路口径（为什么这么排）：
//   - 计划写入是"最终生效"的唯一条件（机器人端只认计划文件）；
//   - 送图先于写计划：送图失败时**不留** enabled=true 的"挂空"计划
//     （否则 booth 每轮 tick 打 ACTIVATE_SKIP 刷屏，且号永远到不了图）；
//   - 号已在图 11 → 不送图，写计划后数秒内自动开摊；
//   - 号离线 → 只写计划，拉起后（且到图 11）自动开摊。
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
	"strings"
	"sync"
	"time"

	"zyctrlcenter/internal/state"
)

// 摆摊单次时长（分钟）允许范围：下限 1；上限 480 = 服务端 VIP0 单次 8h。
const (
	boothMinutesMin     = 1
	boothMinutesMax     = 480
	boothMinutesDefault = 480
	boothPlanFile       = "booth_selftest.json"
	boothConfigFile     = "booth_config.json"
)

// 一键下发的固定口径（robot0001032 是唯一 50 级摆摊号；摊点/摊名/货单沿用 2026-09-28
// 生产实测通过的那一份计划——map 11 东市集 (1385,1545)，落点 1384,1544）。
const (
	boothTargetMapID    = 11                     // 目标图：长安东市集
	boothDefaultAccount = "robot0001032@xy3.com" // 默认账号（入参可覆盖）
	boothDefaultCellX   = 1385                   // 实测可摆点（格子坐标）
	boothDefaultCellY   = 1545
	boothDefaultName    = "杂货小摊"
	boothHoldSec        = 120 // 开摊后维持时长（秒），沿用生产计划
	boothMaxOpenRetry   = 3   // 开摊失败重试次数
	boothMapWaitSec     = 900 // 到图后等待开摊的上限（秒）
)

// boothDefaultItems 默认上架三件货（2026-09-28 生产实测通过的那一份）。
func boothDefaultItems() []map[string]any {
	return []map[string]any{
		{"item_index": 170001, "name": "云母粉", "price": 1000},
		{"item_index": 111029, "name": "乌金", "price": 1500},
		{"item_index": 108477, "name": "小颗亲密丹", "price": 1000},
	}
}

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

// handleBoothConfigGet GET /api/booth/config（只读）：当前配置 + 允许范围 + 已发现的计划文件
// + 当前计划摘要（plan：enabled/account/mapid/cell/offline_minutes/path；未发现 = null）——
// 面板摆摊卡据此开屏显示"当前计划：启用/停用"。
func (a *API) handleBoothConfigGet(w http.ResponseWriter, r *http.Request) {
	cfg := a.loadBoothConfig()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "offline_minutes": cfg.OfflineMinutes,
		"min": boothMinutesMin, "max": boothMinutesMax,
		"plan_files": a.existingBoothPlans(),
		"plan":       a.boothPlanSummary(),
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

// ---------------------------------------------------------------- 一键下发 / 停止

// boothPlanSummary 读第一份存在的计划文件的关键字段（面板"当前计划"显示用）；
// 一份都不存在 → nil；文件存在但 JSON 坏 → 标 bad_json（不挡面板）。
func (a *API) boothPlanSummary() map[string]any {
	for _, p := range a.boothPlanPaths() {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil || m == nil {
			return map[string]any{"path": p, "bad_json": true}
		}
		return map[string]any{
			"path": p, "enabled": toBool(m["enabled"], false), "account": toStr(m["account"]),
			"mapid": toInt(m["mapid"], 0), "cell": m["cell"],
			"offline_minutes": toInt(m["offline_minutes"], 0),
		}
	}
	return nil
}

// parseBoothCell 解析可选入参 cell（[x,y] 格子坐标）；缺省 = 实测可摆点；返回 (cell, 错误说明)。
func parseBoothCell(raw any) ([2]int, string) {
	def := [2]int{boothDefaultCellX, boothDefaultCellY}
	if raw == nil {
		return def, ""
	}
	arr, ok := raw.([]any)
	if !ok || len(arr) != 2 {
		return def, "cell 应为 [x,y] 两个格子坐标（如 [1385,1545]）"
	}
	x, y := toInt(arr[0], 0), toInt(arr[1], 0)
	if x <= 0 || y <= 0 {
		return def, "cell 坐标必须为正整数（格子坐标）"
	}
	return [2]int{x, y}, ""
}

// boothWriteFileAtomic 原子写（.tmp + rename；与配置落盘同口径）。
func boothWriteFileAtomic(path string, raw []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// buildBoothPlan 组装一份计划文件内容（map，便于保留旧文件里的手工调优字段）：
//
//	读第一份存在的副本作为底稿（保留我们不认识的字段，如 open_wait_ms）→ 覆盖受控字段：
//	  - enabled 恒写（start=true / stop=false）；
//	  - start 时全量写 account/mapid/cell/摊名/货单/hold_sec/重试/等待/offline_minutes；
//	  - stop 只翻 enabled（其它字段原样保留，"停止"不该顺手改计划内容）。
func (a *API) buildBoothPlan(enabled bool, acc string, cell [2]int, note string) map[string]any {
	out := map[string]any{}
	for _, p := range a.boothPlanPaths() {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var prev map[string]any
		if err := json.Unmarshal(raw, &prev); err == nil && prev != nil {
			out = prev
		}
		break // 只用第一份存在且读到的（生产 = DeployDir 下机器人端实际读取的那份）
	}
	out["enabled"] = enabled
	if enabled {
		out["account"] = acc
		out["mapid"] = boothTargetMapID
		out["cell"] = []int{cell[0], cell[1]}
		out["booth_name"] = boothDefaultName
		out["booth_poster"] = ""
		out["up_items"] = boothDefaultItems()
		out["hold_sec"] = boothHoldSec
		out["max_open_retry"] = boothMaxOpenRetry
		out["map_wait_sec"] = boothMapWaitSec
		out["offline_minutes"] = a.loadBoothConfig().OfflineMinutes
	} else if toStr(out["account"]) == "" {
		out["account"] = acc
	}
	out["_note"] = note
	return out
}

// writeBoothPlanFiles 把整份计划写进全部"存在的"候选副本（原子 .tmp+rename）。
// 一份都不存在 → 在 DeployDir（机器人端实际读取位置）新建一份，保证热生效链路成立。
// 返回 (写入路径, 失败说明)；写入 0 份且 errs 非空 = 调用方应报错。
func (a *API) writeBoothPlanFiles(plan map[string]any) (written []string, errs []string) {
	raw, err := json.MarshalIndent(plan, "", " ")
	if err != nil {
		return nil, []string{"计划序列化失败：" + err.Error()}
	}
	raw = append(raw, '\n')
	for _, p := range a.boothPlanPaths() {
		if _, err := os.Stat(p); err != nil {
			continue // 不存在的副本不创建（避免在无关目录凭空造文件）
		}
		if err := boothWriteFileAtomic(p, raw); err != nil {
			errs = append(errs, fmt.Sprintf("%s 写入失败: %v", p, err))
			continue
		}
		written = append(written, p)
	}
	if len(written) == 0 {
		p := filepath.Join(a.Cfg.DeployDir, "script", boothPlanFile)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			errs = append(errs, fmt.Sprintf("创建 %s 失败: %v", filepath.Dir(p), err))
			return written, errs
		}
		if err := boothWriteFileAtomic(p, raw); err != nil {
			errs = append(errs, fmt.Sprintf("%s 写入失败: %v", p, err))
			return written, errs
		}
		written = append(written, p)
	}
	return written, errs
}

// handleBoothStart POST /api/booth/start {"account":"...","cell":[x,y]?}（写接口，可选鉴权）。
//
// 一键启动摆摊（2026-09-29）：
//  1. 校验参数（account 缺省 1032；cell 缺省实测可摆点）；
//  2. 查号状态——号不在线 → 只写计划（提示先拉起）；号在线且不在图 11 →
//     **先送图**（复用游荡通道 DispatchRoamEx，与面板 random_walk 同一条实现），
//     送图失败则放弃写计划（不留"挂空"计划）；
//  3. 写计划文件（enabled=true + 全量受控字段；存在几份副本写几份）；
//  4. 返回 {ok, action: sent_map|already_in_map|offline, msg, hint, plan_written, ...}。
//
// 生效链条：机器人端 booth.py 的 tick 每 2s 节流检查计划文件签名（mtime+size），
// 变化即热生效（**无需 reload**）：enabled=true 且 account 匹配 → 激活；激活要求
// 号当前图 == 计划图（不等则打 ACTIVATE_SKIP 等送达）；到图后按 in-map cell 开摊。
func (a *API) handleBoothStart(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	acc := strings.TrimSpace(toStr(body["account"]))
	if acc == "" {
		acc = boothDefaultAccount
	}
	cell, cellErr := parseBoothCell(body["cell"])
	if cellErr != "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "action": "bad_request", "msg": cellErr})
		return
	}
	if a.Events == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "action": "no_channel",
			"msg": "事件通道不可用（命令下发不了）"})
		return
	}

	// 号状态：位置决定"要不要先送图"
	rob, known := state.Robot{}, false
	if a.St != nil {
		rob, known = a.St.Get(acc)
	}
	online := known && rob.Online

	action, curMap := "already_in_map", rob.MapID
	if !online {
		action = "offline"
	} else if rob.MapID != boothTargetMapID {
		// 先送图、成功才写计划：送图失败时不留 enabled=true 的"挂空"计划
		res := a.DispatchRoamEx(RoamReq{Accounts: []string{acc}, MapID: boothTargetMapID})
		if res.Err != "" || res.Sent == 0 {
			msg := res.Err
			if msg == "" {
				msg = res.Msg
			}
			hint := fmt.Sprintf("号没送到图 %d，已放弃启动摆摊（避免写入永不生效的计划）。"+
				"先确认机器人通道与链数据，再重试。", boothTargetMapID)
			a.Log.Printf("[BOOTH] 一键启动：送图失败 account=%s：%s", acc, msg)
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "action": "send_map_failed",
				"account": acc, "mapid": boothTargetMapID,
				"msg": "送图失败（计划未写入）：" + msg, "hint": hint})
			return
		}
		action = "sent_map"
	}

	offMin := a.loadBoothConfig().OfflineMinutes
	note := fmt.Sprintf("%s 面板一键启动（中控下发）: offline=%dmin map=%d cell=[%d,%d]",
		time.Now().Format("2006-01-02 15:04"), offMin, boothTargetMapID, cell[0], cell[1])
	plan := a.buildBoothPlan(true, acc, cell, note)

	boothMu.Lock()
	written, errs := a.writeBoothPlanFiles(plan)
	boothMu.Unlock()
	if len(written) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "action": "plan_write_failed",
			"msg": "计划文件写入失败（摆摊未启动）", "plan_errors": errs})
		return
	}

	// 回执：msg = 一句话（面板 toast）；hint = 详情（卡片状态行/排障）
	var msg, hint string
	switch action {
	case "sent_map":
		msg = fmt.Sprintf("已启动摆摊：计划已写入，号正被送往图 %d", boothTargetMapID)
		hint = fmt.Sprintf("计划已写入 %d 份（enabled=true, offline=%d 分钟）；号当时在图 %d，已下发送图命令 → 到图 %d 后机器人端自动开摊（走图通常几分钟）",
			len(written), offMin, curMap, boothTargetMapID)
	case "already_in_map":
		msg = fmt.Sprintf("已启动摆摊：号已在图 %d，即将开摊", boothTargetMapID)
		hint = fmt.Sprintf("计划已写入 %d 份（enabled=true, offline=%d 分钟）；号已在图 %d，机器人端数秒内自动开摊",
			len(written), offMin, boothTargetMapID)
	case "offline":
		msg = "计划已写入，但号不在线：请先拉起号"
		hint = fmt.Sprintf("计划已写入 %d 份（enabled=true, offline=%d 分钟）；号当前不在线，请先拉起（面板「选号上线」/ POST /api/robots/manage add）；号到图 %d 后自动开摊",
			len(written), offMin, boothTargetMapID)
	}
	resp := map[string]any{
		"ok": true, "action": action, "msg": msg, "hint": hint,
		"account": acc, "mapid": boothTargetMapID, "cell": []int{cell[0], cell[1]},
		"offline_minutes": offMin, "plan_written": written,
	}
	if len(errs) > 0 {
		resp["plan_errors"] = errs
	}
	a.Log.Printf("[BOOTH] 一键启动：account=%s action=%s 计划写入 %v；失败 %v", acc, action, written, errs)
	if a.Store != nil {
		a.Store.LogEvent(map[string]any{"type": "api", "action": "booth_start", "zone": a.currentZoneKey(),
			"account": acc, "result": action, "plan_files": written, "mapid": boothTargetMapID,
			"cell": []int{cell[0], cell[1]}})
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleBoothStop POST /api/booth/stop {"account":"..."}（写接口，可选鉴权）。
//
// 一键停止摆摊（2026-09-29）：写 enabled=false（存在几份副本写几份，其它字段原样保留）。
//   - 号在线：机器人端下一轮 tick 生效（正在摆摊则收摊）；
//   - 号离线（可能在离线挂摊中）：摊位还在服务端挂着，**需拉起号才会收摊**——
//     这里只提示、不自动拉起（拉起是有副作用的动作，交给用户决定）。
func (a *API) handleBoothStop(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	acc := strings.TrimSpace(toStr(body["account"]))
	if acc == "" {
		acc = boothDefaultAccount
	}

	note := time.Now().Format("2006-01-02 15:04") + " 面板一键停止（中控下发）: enabled=false"
	plan := a.buildBoothPlan(false, acc, [2]int{boothDefaultCellX, boothDefaultCellY}, note)
	boothMu.Lock()
	written, errs := a.writeBoothPlanFiles(plan)
	boothMu.Unlock()
	if len(written) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "action": "plan_write_failed",
			"msg": "计划文件写入失败（未停用）", "plan_errors": errs})
		return
	}

	online := false
	if a.St != nil {
		if rr, ok := a.St.Get(acc); ok {
			online = rr.Online
		}
	}
	action, msg, hint := "stopped", "", ""
	if online {
		msg = "已停止摆摊：计划已停用（号在线，下轮生效）"
		hint = fmt.Sprintf("计划已置 enabled=false（%d 份）：号在线，机器人端下一轮 tick 即停；正在摆摊则会收摊",
			len(written))
	} else {
		action = "offline"
		msg = "已停止摆摊：号离线，需拉起号收摊"
		hint = fmt.Sprintf("计划已置 enabled=false（%d 份）：号当前不在线；若在离线挂摊中，摊位仍在服务端挂着，"+
			"需拉起号才会收摊（POST /api/robots/manage add 或面板「上线」）——不会被自动拉起", len(written))
	}
	resp := map[string]any{"ok": true, "action": action, "msg": msg, "hint": hint,
		"account": acc, "plan_written": written}
	if len(errs) > 0 {
		resp["plan_errors"] = errs
	}
	a.Log.Printf("[BOOTH] 一键停止：account=%s action=%s 计划写入 %v；失败 %v", acc, action, written, errs)
	if a.Store != nil {
		a.Store.LogEvent(map[string]any{"type": "api", "action": "booth_stop", "zone": a.currentZoneKey(),
			"account": acc, "result": action, "plan_files": written})
	}
	writeJSON(w, http.StatusOK, resp)
}
