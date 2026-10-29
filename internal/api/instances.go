// 实例管理 API（2026-10-14 实例管理页 V1）：
//
//	GET    /api/ping                 超轻探活（watchdog / 管理页；无状态聚合）
//	GET    /api/instances            实例列表 + 实时状态（进程定位 + HTTP 探活）
//	POST   /api/instances            创建向导（只准备不启动；预检失败返回 checks[]）
//	POST   /api/instances/{id}/start 执行实例启动脚本（等 3s 回 ctrl_pid）
//	POST   /api/instances/{id}/stop  按 ctrl_match / deploy_dir 定位 PID 逐个杀（绝不 /IM）
//	POST   /api/instances/{id}/restart  mode=controller(默认)|cold|robot（robot 走目标实例 API 代理）
//	DELETE /api/instances/{id}       删除（confirm=id；先停 → 目录 rename 归档 → 注册表移除）
//
// 安全：全部仅接受本机（127.0.0.1/::1）访问；写操作挂 requireToken（项目同族）；
// 全部操作写主日志 + 运行历史（Store.LogEvent）。
package api

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"zyctrlcenter/internal/config"
	"zyctrlcenter/internal/services/instances"
	"zyctrlcenter/internal/services/zones"
)

// NewInstancesService 用本进程配置构造实例管理服务（main 装配；不含 Load）。
func NewInstancesService(cfg *config.Config, zreg *zones.Registry, logf func(string, ...any)) *instances.Service {
	return instances.New(instances.Options{
		ExeDir:        execDir(),
		DataDir:       cfg.DataDir,
		WebPort:       cfg.WebPort,
		WebDistDir:    cfg.WebDistDir,
		GameConfigDir: cfg.GameConfigDir,
		Zones:         zreg,
		Logf:          logf,
	})
}

// LoadInstances 装配 + 加载实例注册表（文件不存在时以"本进程自身"为种子落盘）。
func (a *API) LoadInstances() (seeded bool, err error) {
	a.Instances = NewInstancesService(a.Cfg, a.Zones, a.Log.Printf)
	return a.Instances.Load(instancesSeed(a.Cfg))
}

// execDir 当前可执行文件所在目录（instances.json 与启动脚本的落点）。
func execDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// instancesSeed 本进程自身的种子实例（注册表不存在时写入；id 由 web_port 命名避免撞名）。
func instancesSeed(cfg *config.Config) instances.Instance {
	return instances.Instance{
		ID:          fmt.Sprintf("p%d", cfg.WebPort),
		Name:        "本进程实例",
		ZoneServer:  "",
		ZoneKey:     "",
		WebPort:     cfg.WebPort,
		CtrlPort:    cfg.CtrlPort,
		BaseDir:     cfg.BaseDir,
		DataDir:     cfg.DataDir,
		DeployDir:   cfg.DeployDir,
		StartScript: "",
		CtrlMatch:   ctrlMatchHint(os.Args[1:]),
		CreatedAt:   time.Now().Unix(),
	}
}

// ctrlMatchHint 从本进程命令行推导 ctrl_match（种子用）：--data-dir 优先，其次 --deploy；
// 都拿不到时返回空（Registry.normalize 会按 data_dir 兜底派生）。
//
// 注意：不能无条件用 `--web-port`——默认端口的实例其命令行里可能根本没有该参数。
func ctrlMatchHint(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--data-dir":
			return `--data-dir "` + args[i+1] + `"`
		case "--deploy":
			return `--deploy "` + args[i+1] + `"`
		}
	}
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--data-dir="); ok && v != "" {
			return `--data-dir "` + v + `"`
		}
		if v, ok := strings.CutPrefix(a, "--deploy="); ok && v != "" {
			return `--deploy "` + v + `"`
		}
	}
	return ""
}

// ---------------------------------------------------------------- 中间件

// localOnly 仅允许本机（127.0.0.1 / ::1）访问——实例管理是高权限接口（杀进程/写目录）。
func (a *API) localOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !remoteIsLoopback(r.RemoteAddr) {
			writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "msg": "仅接受本机（127.0.0.1）访问"})
			return
		}
		next(w, r)
	}
}

// remoteIsLoopback 解析 RemoteAddr（"host:port"）判断是否回环地址。
func remoteIsLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// instancesOr503 取实例服务；未装配（加载失败/未启用）时回 503 并返回 nil。
func (a *API) instancesOr503(w http.ResponseWriter) *instances.Service {
	if a.Instances == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "msg": "实例管理未装配（instances.json 加载失败？见中控日志）"})
		return nil
	}
	return a.Instances
}

// ---------------------------------------------------------------- handlers

// handlePing 超轻探活：不聚合任何状态（watchdog 每周期探它；旧版实例回退 /api/status）。
//
// 不挂 requireToken 的理由：watchdog 读不到 token（instances.json 无 token 字段），
// 若鉴权会导致"实例被误判不可达 → 误拉"（2026-10-10 事故同类风险）；本机限制已挡住外部。
func (a *API) handlePing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ts": time.Now().Unix()})
}

// instanceRow 列表行 = 注册字段 + 实时状态。
type instanceRow struct {
	instances.Instance
	Status instances.Status `json:"status"`
}

// handleInstancesList 实例列表（status 实时探测，逐实例并行走）。
func (a *API) handleInstancesList(w http.ResponseWriter, r *http.Request) {
	svc := a.instancesOr503(w)
	if svc == nil {
		return
	}
	list := svc.List()
	rows := make([]instanceRow, len(list))
	var wg sync.WaitGroup
	for i, inst := range list {
		wg.Add(1)
		go func(i int, inst instances.Instance) {
			defer wg.Done()
			rows[i] = instanceRow{Instance: inst, Status: svc.Status(inst)}
		}(i, inst)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "instances": rows})
}

// handleInstancesCreate 创建向导（只准备不启动）。预检失败 → {"ok":false,"checks":[...]}。
func (a *API) handleInstancesCreate(w http.ResponseWriter, r *http.Request) {
	svc := a.instancesOr503(w)
	if svc == nil {
		return
	}
	body := readBody(r)
	req := instances.CreateRequest{
		Name:       toStr(body["name"]),
		ZoneServer: toStr(body["zone_server"]),
		ZoneKey:    toStr(body["zone_key"]),
		WebPort:    toInt(body["web_port"], 0),
		CtrlPort:   toInt(body["ctrl_port"], 0),
		Note:       toStr(body["note"]),
	}
	out := svc.Create(req)
	a.Log.Printf("[INSTANCES] create zone=%s/%s web=%d ctrl=%d ok=%v msg=%s",
		req.ZoneServer, req.ZoneKey, req.WebPort, req.CtrlPort, out.OK, out.Msg)
	if !out.OK {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": out.Msg, "checks": out.Checks})
		return
	}
	a.Store.LogEvent(map[string]any{
		"type": "api", "action": "instances_create", "id": out.ID,
		"zone_server": req.ZoneServer, "zone_key": req.ZoneKey,
		"web_port": req.WebPort, "ctrl_port": req.CtrlPort,
		"start_script": out.StartScript, "warnings": out.Warnings,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "id": out.ID, "start_script": out.StartScript,
		"warnings": out.Warnings, "checks": out.Checks,
	})
}

// handleInstanceStart 执行实例启动脚本。
func (a *API) handleInstanceStart(w http.ResponseWriter, r *http.Request) {
	svc := a.instancesOr503(w)
	if svc == nil {
		return
	}
	id := r.PathValue("id")
	res, err := svc.Start(id)
	if err != nil {
		a.Log.Printf("[INSTANCES] start %s 失败: %v", id, err)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	a.Store.LogEvent(map[string]any{"type": "api", "action": "instances_start", "id": id, "ctrl_pid": res.CtrlPID})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": res.Msg, "ctrl_pid": res.CtrlPID})
}

// handleInstanceStop 停止实例（中控 + 机器人；自身实例拒绝）。
func (a *API) handleInstanceStop(w http.ResponseWriter, r *http.Request) {
	svc := a.instancesOr503(w)
	if svc == nil {
		return
	}
	id := r.PathValue("id")
	res, err := svc.Stop(id)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, instances.ErrSelfOperation) {
			msg = "不能停止自身实例"
		}
		a.Log.Printf("[INSTANCES] stop %s 拒绝/失败: %v", id, err)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": msg})
		return
	}
	a.Store.LogEvent(map[string]any{"type": "api", "action": "instances_stop", "id": id,
		"ctrl": res.Ctrl, "robot": res.Robot, "stopped": true})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stopped": true,
		"killed":   map[string]any{"ctrl": res.Ctrl, "robot": res.Robot},
		"warnings": res.Warnings})
}

// handleInstanceRestart 重启实例（mode=controller|cold|robot）。
func (a *API) handleInstanceRestart(w http.ResponseWriter, r *http.Request) {
	svc := a.instancesOr503(w)
	if svc == nil {
		return
	}
	id := r.PathValue("id")
	body := readBody(r)
	mode := strings.TrimSpace(toStr(body["mode"]))
	if mode == "" {
		mode = "controller"
	}
	res, err := svc.Restart(id, mode)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, instances.ErrSelfOperation) {
			msg = "不能重启自身实例（" + mode + "）"
		}
		a.Log.Printf("[INSTANCES] restart %s mode=%s 拒绝/失败: %v", id, mode, err)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": msg})
		return
	}
	if res.Mode == "robot" {
		// 转发目标实例 /api/robot/restart 的原始响应（含状态码与 JSON 体）
		a.Store.LogEvent(map[string]any{"type": "api", "action": "instances_restart", "id": id,
			"mode": mode, "proxy_status": res.ProxyStatus})
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		code := res.ProxyStatus
		if code == 0 {
			code = http.StatusOK
		}
		w.WriteHeader(code)
		_, _ = w.Write(res.ProxyBody)
		return
	}
	a.Store.LogEvent(map[string]any{"type": "api", "action": "instances_restart", "id": id,
		"mode": mode, "ctrl_pid": res.CtrlPID})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "mode": res.Mode, "msg": res.Msg, "ctrl_pid": res.CtrlPID})
}

// handleInstanceDelete 删除实例（confirm=id；先停 → 归档 → 注册表移除；自身实例拒绝）。
func (a *API) handleInstanceDelete(w http.ResponseWriter, r *http.Request) {
	svc := a.instancesOr503(w)
	if svc == nil {
		return
	}
	id := r.PathValue("id")
	body := readBody(r)
	res, err := svc.Delete(id, toStr(body["confirm"]))
	if err != nil {
		msg := err.Error()
		if errors.Is(err, instances.ErrSelfOperation) {
			msg = "不能删除自身实例"
		}
		a.Log.Printf("[INSTANCES] delete %s 拒绝/失败: %v", id, err)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": msg})
		return
	}
	a.Store.LogEvent(map[string]any{"type": "api", "action": "instances_delete", "id": id,
		"archived": res.Archived, "warnings": res.Warnings, "killed": res.Killed})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "archived": res.Archived, "warnings": res.Warnings, "killed": res.Killed})
}
