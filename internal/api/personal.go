// 私人池（personal pool，2026-09-30 Wave 1）——用户手动添加的账号单独一个池，
// **不参与任何自动编排**；任务由用户在面板手动单独发起（抓鬼/神捕/烽火/新手链；游荡不做）。
//
// 设计（见 docs/04-测试/分析-20260930-私人池实施前勘查.md + 计划-20260930-私人池.md）：
//   - 注册表 = 独立文件 data/personal_pool.json（账号名列表；原子写；启动加载）——
//     不放 Account 字段：tools/import_accounts.py 会全量重建 accounts.json，标记会被清掉；
//   - IsPersonal(name) 是全局唯一判据：poolOf 分区、14 处排除闸、意图作废、自动下机豁免都用它；
//   - 手动任务端点 /api/personal/task 直发机器人命令（不写任何在途/配额/台账，只记审计事件）；
//   - 标记投递（机器人端 Wave 2 消费）：沿用 robot_manage 命令 + action:"personal" 全量推名单
//     （client.py 对该命令原样透传 action，机器人端 robot_mgr 加分支即可，协议层零改动）；
//     时机 = 名单变更即推 + hello（机器人重启/重连）后重推（先例 event.onHello → PushSkillConfig）。
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// personalPool 私人池注册表（账号名集合 + 落盘路径；路径空 = 纯内存，测试/未装配场景）。
type personalPool struct {
	mu    sync.RWMutex
	names map[string]bool
	path  string
}

func newPersonalPool(path string) *personalPool {
	return &personalPool{names: map[string]bool{}, path: path}
}

// Has 该账号是否在私人池。
func (p *personalPool) Has(name string) bool {
	if p == nil || name == "" {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.names[name]
}

// List 私人池账号（升序；面板/推送用）。
func (p *personalPool) List() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, 0, len(p.names))
	for n := range p.names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Count 私人池账号数。
func (p *personalPool) Count() int {
	if p == nil {
		return 0
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.names)
}

// Add 入池（幂等）；返回 (新增, 已存在)。有变更时原子落盘。
func (p *personalPool) Add(names []string) (added, existed int) {
	if p == nil {
		return 0, len(names)
	}
	changed := false
	p.mu.Lock()
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if p.names[n] {
			existed++
			continue
		}
		p.names[n] = true
		added++
		changed = true
	}
	path, raw := p.path, p.marshalLocked()
	p.mu.Unlock()
	if changed {
		writePersonalFileAtomic(path, raw)
	}
	return added, existed
}

// Remove 出池（幂等）；返回移除数。有变更时原子落盘。
func (p *personalPool) Remove(names []string) (removed int) {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || !p.names[n] {
			continue
		}
		delete(p.names, n)
		removed++
	}
	path, raw := p.path, p.marshalLocked()
	p.mu.Unlock()
	if removed > 0 {
		writePersonalFileAtomic(path, raw)
	}
	return removed
}

// personalPoolFile 落盘形态（JSON；可手工编辑/审计）。
type personalPoolFile struct {
	Comment   string   `json:"_comment,omitempty"`
	Accounts  []string `json:"accounts"`
	UpdatedAt string   `json:"updated_at,omitempty"`
}

// marshalLocked 序列化（须持锁调用；无路径返回 nil → 写盘空操作）。
func (p *personalPool) marshalLocked() []byte {
	if p.path == "" {
		return nil
	}
	list := make([]string, 0, len(p.names))
	for n := range p.names {
		list = append(list, n)
	}
	sort.Strings(list)
	b, err := json.MarshalIndent(personalPoolFile{
		Comment:   "私人池（个人号）：不参与自动编排；任务在面板手动单独发起。由中控维护，可手工编辑。",
		Accounts:  list,
		UpdatedAt: time.Now().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return nil
	}
	return b
}

// writePersonalFileAtomic tmp+rename 原子写（失败静默：注册表丢一条只影响隔离范围，
// 不该反过来阻塞加号/出池主流程；与"已派台账"落盘同口径）。
func writePersonalFileAtomic(path string, raw []byte) {
	if path == "" || raw == nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// LoadPersonalPool 启动装配：设定落盘路径并载入既有文件。返回载入条数；
// 文件不存在 = 首次运行（0, nil）；损坏 = (0, err, 按空表继续；下次变更覆盖重建)。
func (a *API) LoadPersonalPool(path string) (int, error) {
	p := a.personal
	if p == nil {
		p = newPersonalPool("")
		a.personal = p
	}
	p.mu.Lock()
	p.path = path
	p.mu.Unlock()
	if path == "" {
		return 0, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	var f personalPoolFile
	if err := json.Unmarshal(b, &f); err != nil {
		return 0, err
	}
	n := 0
	p.mu.Lock()
	for _, name := range f.Accounts {
		name = strings.TrimSpace(name)
		if name == "" || p.names[name] {
			continue
		}
		p.names[name] = true
		n++
	}
	p.mu.Unlock()
	return n, nil
}

// IsPersonal 是否私人池账号（全站唯一判据；各排除闸/意图/自动下机都用它）。
func (a *API) IsPersonal(name string) bool {
	if a == nil || a.personal == nil {
		return false
	}
	return a.personal.Has(name)
}

// dropPersonalAccounts 从批量自动派发里剔除私人池账号（2026-09-30 Wave 1；语义同 dropTeamAccounts）。
// 命中号合并记一条日志（每次调用一条）；返回（保留, 剔除）。
func (a *API) dropPersonalAccounts(accs []string, what string) (keep, skipped []string) {
	if len(accs) == 0 {
		return accs, nil
	}
	keep = make([]string, 0, len(accs))
	for _, acc := range accs {
		if a.IsPersonal(acc) {
			skipped = append(skipped, acc)
			continue
		}
		keep = append(keep, acc)
	}
	if len(skipped) > 0 {
		a.Log.Printf("[私人池] %s 跳过私号 %d 个：%s（不参与自动编排；手动任务用 /api/personal/task）",
			what, len(skipped), strings.Join(skipped, "、"))
	}
	return keep, skipped
}

// PushPersonalPool 把私人池**全量名单**推给机器人（Wave 2 消费；协议层零改动——
// 复用 robot_manage 命令分支，机器人端 robot_mgr 加 action:"personal" 处理即可）。
// 时机：①名单变更（add/remove 端点内）②hello 重推（event.onHello 注入回调）。
// 旧机器人/未实现侧：收到未知 action 无副作用（manage_robots 无分支原样返回）。
func (a *API) PushPersonalPool() bool {
	if a.Events == nil {
		return false
	}
	names := a.personal.List()
	cmd := map[string]any{"cmd": "robot_manage", "action": "personal", "accounts": names}
	return a.Events.SendCmd(cmd, "personal_pool")
}

// ---------------------------------------------------------------- HTTP 接口

// handlePersonalGet GET /api/personal → {ok, count, accounts:[...]}
func (a *API) handlePersonalGet(w http.ResponseWriter, r *http.Request) {
	names := a.personal.List()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(names), "accounts": names})
}

// handlePersonalAdd POST /api/personal/add {accounts|names} → 入池 + 推送 + 审计
func (a *API) handlePersonalAdd(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	names := bodyAccounts(body)
	if len(names) == 0 {
		names = splitList(toStr(body["names"]))
	}
	names = normAccounts(names)
	if len(names) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "accounts/names 为空"})
		return
	}
	added, existed := a.personal.Add(names)
	if added > 0 {
		a.PushPersonalPool()
	}
	if a.Store != nil {
		a.Store.LogEvent(map[string]any{"type": "api", "action": "personal_add",
			"zone": a.currentZoneKey(), "accounts": names, "added": added, "count": a.personal.Count()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "added": added, "existed": existed,
		"count": a.personal.Count(),
		"msg":   fmt.Sprintf("已加入私人池 %d 个（已存在 %d 个）：不参与自动编排，任务请用私人池卡手动发起", added, existed)})
}

// handlePersonalRemove POST /api/personal/remove {accounts|names} → 出池 + 推送 + 审计
func (a *API) handlePersonalRemove(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	names := bodyAccounts(body)
	if len(names) == 0 {
		names = splitList(toStr(body["names"]))
	}
	names = normAccounts(names)
	if len(names) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "accounts/names 为空"})
		return
	}
	removed := a.personal.Remove(names)
	if removed > 0 {
		a.PushPersonalPool()
	}
	if a.Store != nil {
		a.Store.LogEvent(map[string]any{"type": "api", "action": "personal_remove",
			"zone": a.currentZoneKey(), "accounts": names, "removed": removed, "count": a.personal.Count()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed,
		"count": a.personal.Count(),
		"msg":   fmt.Sprintf("已移出私人池 %d 个（之后将回到自动编排范围）", removed)})
}

// personalTaskDelay ensure_online 时"先上线、再下发任务"的间隔（与恢复引擎 RESTORE_DELAY=8s 同口径）。
const personalTaskDelay = 8 * time.Second

// personalTaskCommands 任务类型 → (start 命令构造, stop 命令名)（机器人端协议见勘查报告 §3）。
func (a *API) personalTaskCmdOf(task, action string, accs []string, dailyLimit int) (map[string]any, string, error) {
	switch task {
	case "ghost":
		if action == "stop" {
			return map[string]any{"cmd": "ghost_stop", "accounts": accs}, "ghost_stop", nil
		}
		cmd, err := a.ghostStartCmdOf(accs, "solo", dailyLimit)
		return cmd, "ghost_start", err
	case "shenbu", "fenghuo":
		if action == "stop" {
			return map[string]any{"cmd": "share_daily_stop", "accounts": accs}, "share_daily_stop", nil
		}
		k, ok := shareDailyKindFromString(task)
		if !ok {
			return nil, "", errors.New("task 必须是 ghost / shenbu / fenghuo / newbie")
		}
		cmd, err := a.shareDailyStartCmdOf(k, accs)
		return cmd, "share_daily_start", err
	case "newbie":
		if action == "stop" {
			// 机器人端 "stop" = 停任务链（quest_engine；client 兜底路由），同时兜底停抓鬼（机器人端实现）
			return map[string]any{"cmd": "stop", "accounts": accs}, "stop", nil
		}
		cmd, err := a.newbieStartCmdOf(accs)
		return cmd, "start_chain", err
	}
	return nil, "", errors.New("task 必须是 ghost / shenbu / fenghuo / newbie")
}

// handlePersonalTask POST /api/personal/task
//
// 请求：{accounts:[...], task:"ghost|shenbu|fenghuo|newbie", action:"start"|"stop",
//
//	ensure_online?:bool, daily_limit?:int}
//
// 语义（计划 §3.3）：**仅限私人池账号**（不在池内明确拒绝——防被当作"绕配额"通道）；
// 直发机器人命令，**不写任何在途/配额/台账**（只记 personal_task 审计事件）；
// 不绕机器人协议（仍走标准 SendCmd）。ensure_online=true 时先补 robot_manage add
// （手动语义、绕容量预算），离线号在 personalTaskDelay 后异步下发任务。
func (a *API) handlePersonalTask(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	accs := normAccounts(bodyAccounts(body))
	if len(accs) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "accounts 为空"})
		return
	}
	task := strings.ToLower(strings.TrimSpace(toStr(body["task"])))
	action := strings.ToLower(strings.TrimSpace(toStr(body["action"])))
	if action != "start" && action != "stop" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "action 必须是 start / stop"})
		return
	}
	// 只服务私人池账号：不在池内 → 明确拒绝并列出（不静默、防误用）
	var notPersonal []string
	for _, acc := range accs {
		if !a.IsPersonal(acc) {
			notPersonal = append(notPersonal, acc)
		}
	}
	if len(notPersonal) > 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false,
			"msg":          "以下账号不在私人池（自动编排账号请用「任务」页）：" + strings.Join(notPersonal, "、"),
			"not_personal": notPersonal})
		return
	}
	if a.Events == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "事件通道不可用（命令下发不了）"})
		return
	}
	limit := toInt(body["daily_limit"], 0)
	cmd, cmdName, err := a.personalTaskCmdOf(task, action, accs, limit)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	ensureOnline := toBool(body["ensure_online"], false) && action == "start"
	var onlineMsg string
	if ensureOnline {
		offline := make([]string, 0, len(accs))
		for _, acc := range accs {
			rb, ok := a.St.Get(acc)
			if !ok || !rb.Online {
				offline = append(offline, acc)
			}
		}
		if len(offline) > 0 {
			sent, _, noPwd := a.sendOnlineChunks(offline, a.gameAddrOf(""), 10, 300, "personal_online")
			onlineMsg = fmt.Sprintf("；其中 %d 个离线号已下发上线（%d 个发出，%d 个池内无密码）",
				len(offline), len(sent), len(noPwd))
			// 有号需要先上线 → 等其登录后再发任务（异步；桌面/海量操作均不阻塞 HTTP）
			go func() {
				time.Sleep(personalTaskDelay)
				ok := a.Events.SendCmd(cmd, "personal_task")
				a.logPersonalTask(task, action, accs, cmdName, ok, "ensure_online 延迟下发")
			}()
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "task": task, "action": action,
				"command": cmdName, "accounts": accs, "sent": false, "scheduled": true,
				"msg": fmt.Sprintf("私人池任务已受理：%s %s %d 个账号%s，%s 后下发任务",
					task, action, len(accs), onlineMsg, personalTaskDelay)})
			return
		}
	}
	sent := a.Events.SendCmd(cmd, "personal_task")
	a.logPersonalTask(task, action, accs, cmdName, sent, "")
	msg := fmt.Sprintf("私人池任务已下发：%s %s %d 个账号", task, action, len(accs))
	if !sent {
		msg = "下发失败：机器人控制通道未连接"
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": sent, "task": task, "action": action,
		"command": cmdName, "accounts": accs, "sent": sent, "msg": msg})
}

// logPersonalTask 审计事件（**只做审计**；不写配额/在途/台账——那是自动编排的账）。
func (a *API) logPersonalTask(task, action string, accs []string, cmdName string, sent bool, note string) {
	if a.Store == nil {
		return
	}
	ev := map[string]any{"type": "api", "action": "personal_task",
		"zone": a.currentZoneKey(), "task": task, "op": action,
		"command": cmdName, "accounts": accs, "sent": sent}
	if note != "" {
		ev["note"] = note
	}
	a.Store.LogEvent(ev)
}
