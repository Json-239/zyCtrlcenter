// 创建向导（"只准备不启动"）：
//  1. 预检（端口试绑/注册表冲突；zone 合法；deploy/data 目标不存在；启动脚本不覆盖）；
//  2. 执行（全部预检通过才做，先 .tmp 后 rename）：
//     复制模板 deploy（排除运行态垃圾）→ 生成 data（chains/只读件/zones/accounts/waterline）
//     → 生成 _start_<server>_<zone>.bat（ASCII+CRLF）→ 注册表追加（ctrl_match=`--data-dir "<新 data_dir>"`）。
//
// 命名说明：目录/脚本名一律用 <server>_<zone> 组合 slug（2026-10-14 修复）——
// zone_key 只在单个服内唯一（生产 prod-240 与测试服 201-test 都有 2300 区），
// 只用 zone_key 命名会跨服撞名。
//
// 不自动启动（首启由人工确认后点"启动"或跑生成的脚本）。
package instances

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"zyctrlcenter/internal/services/waterline"
	"zyctrlcenter/internal/services/zones"
)

// CreateRequest 创建向导请求（字段名与前端契约对齐）。
type CreateRequest struct {
	Name       string `json:"name"`
	ZoneServer string `json:"zone_server"`
	ZoneKey    string `json:"zone_key"`
	WebPort    int    `json:"web_port"`
	CtrlPort   int    `json:"ctrl_port"`
	Note       string `json:"note,omitempty"`
}

// CheckResult 单个预检项。
type CheckResult struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

// CreateOutcome 创建结果。
type CreateOutcome struct {
	OK          bool
	ID          string
	StartScript string
	Warnings    []string
	Checks      []CheckResult
	Msg         string
}

// deployExcludes 复制模板 deploy 时的文件排除（历史备份/运行态；不含模板本体）。
var deployExcludes = []string{"*.bak_*", "*.snap_*", "quest_progress_*.json", "diag.log*", "console*.log"}

// deployExcludeDirs 目录排除（__pycache__=编译缓存；state/=运行态）。
var deployExcludeDirs = []string{"__pycache__", "state"}

// ---------------------------------------------------------------- 路径推导

// instanceSlug 新实例的目录/脚本命名 slug = <server>_<zone>（两段各自 sanitize）。
//
// 为什么带 server：zone_key 只在单个服内唯一——如生产 prod-240 与测试服
// 201-test 都有 2300 区；V1 只用 zone_key 命名会撞名（2026-10-14 修复）。
func instanceSlug(serverKey, zoneKey string) string {
	return sanitizeKey(serverKey) + "_" + sanitizeKey(zoneKey)
}

// sanitizeKey 路径安全化：仅保留 [A-Za-z0-9_-]，其余字符替换为 '_'
// （防路径分隔符/点点遍历混进目录名）。
func sanitizeKey(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// dataDirFor 新实例 data 目标 = <exe目录>/data-<server>_<zone>（如 data-201-test_2300）。
func (s *Service) dataDirFor(serverKey, zoneKey string) string {
	return filepath.Join(s.opts.ExeDir, "data-"+instanceSlug(serverKey, zoneKey))
}

// baseDirFor 新实例 base 目标 = <exe目录同级>/<exe目录名>_<server>_<zone>。
func (s *Service) baseDirFor(serverKey, zoneKey string) string {
	exeDir := filepath.Clean(s.opts.ExeDir)
	return filepath.Join(filepath.Dir(exeDir), filepath.Base(exeDir)+"_"+instanceSlug(serverKey, zoneKey))
}

// startScriptFor 新实例启动脚本文件名 = _start_<server>_<zone>.bat（放 exe 目录）。
func startScriptFor(serverKey, zoneKey string) string {
	return "_start_" + instanceSlug(serverKey, zoneKey) + ".bat"
}

// templateDeployDir 模板源 = 注册表里第一个"存在的" deploy_dir。
func (s *Service) templateDeployDir() (string, bool) {
	for _, inst := range s.reg.List() {
		if st, err := os.Stat(inst.DeployDir); err == nil && st.IsDir() {
			return inst.DeployDir, true
		}
	}
	return "", false
}

// deployDirFor 新实例 deploy 目标 = <模板 deploy 父目录>/single_robot_zy_<server>_<zone>。
func (s *Service) deployDirFor(serverKey, zoneKey string) (string, bool) {
	tpl, ok := s.templateDeployDir()
	if !ok {
		return "", false
	}
	return filepath.Join(filepath.Dir(filepath.Clean(tpl)), "single_robot_zy_"+instanceSlug(serverKey, zoneKey)), true
}

// lookupZone 校验并取区信息：优先主 zones 注册表，退回 SeedServers。
func (s *Service) lookupZone(serverKey, zoneKey string) (zones.Flat, bool) {
	if s.opts.Zones != nil {
		if f, ok := s.opts.Zones.FlatOf(serverKey, zoneKey); ok {
			return f, true
		}
	}
	for _, srv := range zones.SeedServers() {
		if srv.Key != serverKey {
			continue
		}
		for _, z := range srv.Zones {
			if z.Key != zoneKey {
				continue
			}
			coding := z.Coding
			if coding == "" {
				coding = srv.Coding
			}
			if coding == "" {
				coding = zones.DefaultCoding
			}
			return zones.Flat{
				Key: zones.MakeKey(srv.Key, z.Key), ServerKey: srv.Key, ServerName: srv.Name,
				ZoneKey: z.Key, Name: z.Name, Host: srv.Host, Port: z.Port, Coding: coding,
			}, true
		}
	}
	return zones.Flat{}, false
}

// zoneAddrKey 账号池 zone key（accounts.json 的 zones map 键："host:port"）。
func zoneAddrKey(f zones.Flat) string { return fmt.Sprintf("%s:%d", f.Host, f.Port) }

// ---------------------------------------------------------------- 预检

// Create 创建（预检 + 执行，写操作锁内完成；有任何预检失败 → 返回 checks，不做变更）。
func (s *Service) Create(req CreateRequest) CreateOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	checks := s.checkCreate(req)
	for _, c := range checks {
		if !c.Pass {
			return CreateOutcome{OK: false, Checks: checks, Msg: "预检未通过，未做任何变更"}
		}
	}

	flat, _ := s.lookupZone(req.ZoneServer, req.ZoneKey)
	dataDir := s.dataDirFor(req.ZoneServer, req.ZoneKey)
	deployDir, ok := s.deployDirFor(req.ZoneServer, req.ZoneKey)
	if !ok {
		return CreateOutcome{OK: false, Checks: checks, Msg: "找不到模板 deploy 目录（注册表里没有现存实例）"}
	}
	tpl, _ := s.templateDeployDir()
	scriptName := startScriptFor(req.ZoneServer, req.ZoneKey)
	batPath := filepath.Join(s.opts.ExeDir, scriptName)
	warnings := []string{}
	id := fmt.Sprintf("p%d", req.WebPort)

	// 失败清理：目标目录/脚本由预检保证"原先不存在"，清理只删本次半成品（best-effort）。
	deployDone, dataDone := false, false
	cleanup := func() {
		_ = os.RemoveAll(deployDir + ".tmp")
		_ = os.RemoveAll(dataDir + ".tmp")
		_ = os.Remove(batPath + ".tmp")
		if deployDone {
			_ = os.RemoveAll(deployDir)
		}
		if dataDone {
			_ = os.RemoveAll(dataDir)
		}
	}

	// 1) 复制模板 deploy（.tmp → rename）
	if err := copyDeployTree(tpl, deployDir+".tmp"); err != nil {
		cleanup()
		return CreateOutcome{OK: false, Checks: checks, Msg: "复制模板 deploy 失败: " + err.Error()}
	}
	if err := os.Rename(deployDir+".tmp", deployDir); err != nil {
		cleanup()
		return CreateOutcome{OK: false, Checks: checks, Msg: "deploy 目录就位失败: " + err.Error()}
	}
	deployDone = true

	// 2) 生成 data（.tmp → rename）
	if err := s.initDataDir(flat, req, dataDir, &warnings); err != nil {
		cleanup()
		return CreateOutcome{OK: false, Checks: checks, Msg: "生成 data 目录失败: " + err.Error()}
	}
	dataDone = true

	// 3) 生成启动脚本（.tmp → rename；ASCII+CRLF）
	inst := Instance{
		ID: id, Name: strings.TrimSpace(req.Name), ZoneServer: req.ZoneServer, ZoneKey: req.ZoneKey,
		WebPort: req.WebPort, CtrlPort: req.CtrlPort,
		BaseDir: s.baseDirFor(req.ZoneServer, req.ZoneKey), DataDir: dataDir, DeployDir: deployDir,
		StartScript: scriptName,
		// 新实例默认"未启动（人工停止态）"：向导承诺"只准备不启动"——看门狗不自动拉起，
		// 点「启动」才清标记并运行（2026-10-14 口径修复：防新建实例被看门狗即刻拉走）。
		Stopped: true,
		// ctrl_match：只按命令行定位（与手工注册的 B 实例同款）。
		CtrlMatch: `--data-dir "` + dataDir + `"`,
		Note:      strings.TrimSpace(req.Note), CreatedAt: s.now().Unix(),
	}
	if inst.Name == "" {
		inst.Name = "实例(" + req.ZoneKey + ")"
	}
	bat := s.renderStartBat(inst, flat)
	if err := os.WriteFile(batPath+".tmp", []byte(bat), 0o644); err != nil {
		cleanup()
		return CreateOutcome{OK: false, Checks: checks, Msg: "写启动脚本失败: " + err.Error()}
	}
	if err := os.Rename(batPath+".tmp", batPath); err != nil {
		cleanup()
		return CreateOutcome{OK: false, Checks: checks, Msg: "启动脚本就位失败: " + err.Error()}
	}

	// 4) 注册表追加
	if err := s.reg.Add(inst); err != nil {
		cleanup()
		return CreateOutcome{OK: false, Checks: checks, Msg: "注册表追加失败: " + err.Error()}
	}
	s.logf("[INSTANCES] create %s（zone %s/%s web:%d ctrl:%d deploy=%s data=%s）",
		id, req.ZoneServer, req.ZoneKey, req.WebPort, req.CtrlPort, deployDir, dataDir)
	return CreateOutcome{OK: true, ID: id, StartScript: scriptName, Warnings: warnings, Checks: checks}
}

// checkCreate 预检：返回全部检查项（前端逐项展示）。
func (s *Service) checkCreate(req CreateRequest) []CheckResult {
	checks := make([]CheckResult, 0, 6)
	add := func(name string, pass bool, detail string) {
		checks = append(checks, CheckResult{Name: name, Pass: pass, Detail: detail})
	}

	// 1) 基础参数
	paramsOK := req.WebPort > 0 && req.WebPort <= 65535 && req.CtrlPort > 0 && req.CtrlPort <= 65535 &&
		req.WebPort != req.CtrlPort && strings.TrimSpace(req.ZoneKey) != ""
	add("params", paramsOK, fmt.Sprintf("web_port=%d ctrl_port=%d zone_key=%q（两端口须不同且合法）",
		req.WebPort, req.CtrlPort, req.ZoneKey))

	// 2) 端口：注册表冲突 + 本地试绑
	conflictPort := func(port int) (string, bool) {
		for _, old := range s.reg.List() {
			if old.WebPort == port || old.CtrlPort == port {
				return fmt.Sprintf("已被实例 %s 占用", old.ID), true
			}
		}
		return "", false
	}
	checkPort := func(name string, port int) {
		if port <= 0 || port > 65535 {
			add(name, false, fmt.Sprintf("端口非法: %d", port))
			return
		}
		if who, bad := conflictPort(port); bad {
			add(name, false, who)
			return
		}
		if err := portListenable(port); err != nil {
			add(name, false, "试绑失败（端口被占用？）: "+err.Error())
			return
		}
		add(name, true, fmt.Sprintf("端口 %d 可用（试绑已释放）", port))
	}
	checkPort("web_port", req.WebPort)
	checkPort("ctrl_port", req.CtrlPort)

	// 3) zone 合法（主 zones 注册表优先，SeedServers 兜底）
	flat, zoneOK := s.lookupZone(req.ZoneServer, req.ZoneKey)
	zoneDetail := "未找到该服/区（检查 zone_server / zone_key）"
	if zoneOK {
		zoneDetail = fmt.Sprintf("区有效: %s → %s", flat.Key, flat.Addr())
	}
	add("zone", zoneOK, zoneDetail)

	// 4) 目标目录/脚本不存在（防覆盖现场）
	deployDir, deployOK := s.deployDirFor(req.ZoneServer, req.ZoneKey)
	switch {
	case strings.TrimSpace(req.ZoneKey) == "":
		add("deploy_target", false, "zone_key 为空，无法推导目标路径")
	case !deployOK:
		add("deploy_target", false, "找不到模板 deploy 目录（注册表里没有现存实例）")
	default:
		if _, err := os.Stat(deployDir); err == nil {
			add("deploy_target", false, "目标已存在: "+deployDir)
		} else {
			add("deploy_target", true, "目标不存在，可创建: "+deployDir)
		}
	}

	dataDir := s.dataDirFor(req.ZoneServer, req.ZoneKey)
	if strings.TrimSpace(req.ZoneKey) == "" {
		add("data_target", false, "zone_key 为空，无法推导目标路径")
	} else if _, err := os.Stat(dataDir); err == nil {
		add("data_target", false, "目标已存在: "+dataDir)
	} else {
		add("data_target", true, "目标不存在，可创建: "+dataDir)
	}

	scriptPath := filepath.Join(s.opts.ExeDir, startScriptFor(req.ZoneServer, req.ZoneKey))
	if strings.TrimSpace(req.ZoneKey) == "" {
		add("start_script", false, "zone_key 为空，无法推导脚本名")
	} else if _, err := os.Stat(scriptPath); err == nil {
		add("start_script", false, "启动脚本已存在（防覆盖）: "+scriptPath)
	} else {
		add("start_script", true, "启动脚本名可用: "+filepath.Base(scriptPath))
	}
	return checks
}

// ---------------------------------------------------------------- 执行（目录/数据/脚本）

// copyDeployTree 复制模板 deploy（含排除规则；目标应为 *.tmp）。
func copyDeployTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		if d.IsDir() {
			for _, ex := range deployExcludeDirs {
				if d.Name() == ex {
					return filepath.SkipDir
				}
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		for _, pat := range deployExcludes {
			if ok, _ := filepath.Match(pat, d.Name()); ok {
				return nil
			}
		}
		return copyFile(path, filepath.Join(dst, rel))
	})
}

// copyFile 单文件复制。
func copyFile(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, raw, 0o644)
}

// copyTreeIfExists 目录存在才复制（不存在返回 false）。
func copyTreeIfExists(src, dst string) (bool, error) {
	if _, err := os.Stat(src); err != nil {
		return false, nil
	}
	if err := copyDeployTree(src, dst); err != nil {
		return true, err
	}
	return true, nil
}

// initDataDir 生成新实例 data 目录（.tmp → rename）。
func (s *Service) initDataDir(flat zones.Flat, req CreateRequest, dataDir string, warnings *[]string) error {
	tmp := dataDir + ".tmp"
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	srcData := s.opts.DataDir

	// 1) 只读件：chains/ + 四个单文件（存在才复制；缺失记 warning）
	if ok, err := copyTreeIfExists(filepath.Join(srcData, "chains"), filepath.Join(tmp, "chains")); err != nil {
		return err
	} else if !ok {
		*warnings = append(*warnings, "源 data/chains 不存在，未复制（机器人链路数据可能缺失）")
	}
	for _, name := range []string{"maps.json", "item_usage_catalog.json", "autotask.json", "booth_config.json"} {
		src := filepath.Join(srcData, name)
		if _, err := os.Stat(src); err != nil {
			*warnings = append(*warnings, "源 data/"+name+" 不存在，未复制（运行时按默认值）")
			continue
		}
		if err := copyFile(src, filepath.Join(tmp, name)); err != nil {
			return err
		}
	}

	// 2) zones.json：复制主文件 + current 指向该区（读不到 → 用注册表/种子构造）
	if err := s.writeZonesFile(srcData, flat, filepath.Join(tmp, "zones.json")); err != nil {
		return err
	}

	// 3) accounts.json：过滤该区 usable 的账号（保留条目原样；顶层字段保留）
	n, err := s.writeAccountsFile(srcData, flat, filepath.Join(tmp, "accounts.json"))
	if err != nil {
		return err
	}
	*warnings = append(*warnings, fmt.Sprintf("账号分片：仅按 %s 区 usable 过滤（%d 个号），未按等级/号段校验重叠", zoneAddrKey(flat), n))

	// 4) waterline.json：enabled=true, target=30（低位起步，人工再调）
	wl := waterline.DefaultConfig()
	wl.Enabled = true
	wl.Target = 30
	raw, err := json.MarshalIndent(wl, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, "waterline.json"), append(raw, '\n'), 0o644); err != nil {
		return err
	}

	return os.Rename(tmp, dataDir)
}

// writeZonesFile 写新实例 zones.json：主文件全量 + current=该 server/zone。
func (s *Service) writeZonesFile(srcData string, flat zones.Flat, dst string) error {
	var doc map[string]any
	if raw, err := os.ReadFile(filepath.Join(srcData, "zones.json")); err == nil {
		_ = json.Unmarshal(raw, &doc)
	}
	if doc == nil {
		// 兜底：用注册表内存里的服列表（或种子）构造
		var servers []zones.Server
		if s.opts.Zones != nil {
			servers = s.opts.Zones.Servers()
		}
		if len(servers) == 0 {
			servers = zones.SeedServers()
		}
		doc = map[string]any{"servers": servers}
	}
	doc["current"] = map[string]any{"server": flat.ServerKey, "zone": flat.ZoneKey}
	doc["updated_at"] = s.now().Unix()
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(dst, append(raw, '\n'), 0o644)
}

// writeAccountsFile 写新实例 accounts.json：从主池过滤"该区 usable"的账号。
// 顶层保留 _comment/generated_at/source/servers/accounts（契约格式）；条目原样保留
// （用 json.RawMessage，不丢任何字段）。返回保留的账号数。
func (s *Service) writeAccountsFile(srcData string, flat zones.Flat, dst string) (int, error) {
	type accountsFile struct {
		Comment     string            `json:"_comment,omitempty"`
		GeneratedAt int64             `json:"generated_at,omitempty"`
		Source      string            `json:"source,omitempty"`
		Servers     map[string]string `json:"servers,omitempty"`
		Accounts    []json.RawMessage `json:"accounts"`
	}
	raw, err := os.ReadFile(filepath.Join(srcData, "accounts.json"))
	if err != nil {
		// 源池不存在：写空分片（B 可跑空池，人工再补）
		out := accountsFile{Comment: "空分片（源 accounts.json 不存在）", GeneratedAt: s.now().Unix(),
			Source: "split of data/accounts.json", Accounts: []json.RawMessage{}}
		blob, _ := json.MarshalIndent(out, "", " ")
		return 0, os.WriteFile(dst, append(blob, '\n'), 0o644)
	}
	var fx accountsFile
	if err := json.Unmarshal(raw, &fx); err != nil {
		return 0, fmt.Errorf("源 accounts.json 解析失败: %w", err)
	}
	addrKey := zoneAddrKey(flat)
	kept := make([]json.RawMessage, 0, len(fx.Accounts))
	for _, item := range fx.Accounts {
		var probe struct {
			Zones map[string]struct {
				Usable bool `json:"usable"`
			} `json:"zones"`
		}
		if err := json.Unmarshal(item, &probe); err != nil {
			continue
		}
		if z, ok := probe.Zones[addrKey]; ok && z.Usable {
			kept = append(kept, item)
		}
	}
	fx.Comment = fmt.Sprintf("%s 区实例账号分片：来自主池，仅取 %s 区 usable（%d 个）。",
		flat.ZoneKey, addrKey, len(kept))
	fx.GeneratedAt = s.now().Unix()
	fx.Source = "split of data/accounts.json"
	fx.Accounts = kept
	blob, err := json.MarshalIndent(fx, "", " ")
	if err != nil {
		return 0, err
	}
	return len(kept), os.WriteFile(dst, append(blob, '\n'), 0o644)
}

// renderStartBat 生成启动脚本全文（ASCII+CRLF；set ROBOT_CTRL_PORT=<ctrl_port>
// 必须存在——中控不注入该值，机器人默认连 17200，即 py 参考实现）。
func (s *Service) renderStartBat(inst Instance, flat zones.Flat) string {
	// 仅用 sanitize 后的 slug 拼进 bat（保证纯 ASCII，防 zone key 混入非 ASCII）。
	zoneTag := instanceSlug(inst.ZoneServer, inst.ZoneKey)
	lines := []string{
		"@echo off",
		"REM ============================================================",
		"REM  Instance starter (server_zone " + zoneTag + ") - generated by /api/instances @ " + s.now().Format("2006-01-02 15:04:05"),
		"REM",
		"REM  Stop = kill the controller PID whose command line contains",
		"REM  ctrl_match (see instances.json). NEVER taskkill /IM: all",
		"REM  instances share the image name zyctrlcenter.exe.",
		"REM",
		"REM  ROBOT_CTRL_PORT is inherited by the robot process so it",
		"REM  connects back to THIS instance (the controller does not",
		"REM  inject it; the robot default is 17200 = the py controller).",
		"REM  Keep this file ASCII-only (cmd code-page rules).",
		"REM ============================================================",
		"cd /d " + s.opts.ExeDir,
		"set ROBOT_CTRL_HOST=127.0.0.1",
		fmt.Sprintf("set ROBOT_CTRL_PORT=%d", inst.CtrlPort),
		"set CTRL_AUTO_RESTORE=1",
		"set CTRL_FENGHUO=1",
		"set CTRL_SHARE_DAILY=1",
		"set CTRL_GHOST_STAGGER=1",
		"set CTRL_LIVECOUNT_ENABLED=0",
		"set CTRL_LIVECOUNT_URL=http://" + flat.Host + ":8080",
		"set CTRL_LIVECOUNT_SERVER_ID=1000",
	}
	args := []string{
		"--web-port", fmt.Sprintf("%d", inst.WebPort),
		"--ctrl-port", fmt.Sprintf("%d", inst.CtrlPort),
		"--base-dir", inst.BaseDir,
		"--data-dir", inst.DataDir,
		"--deploy", inst.DeployDir,
	}
	if strings.TrimSpace(s.opts.WebDistDir) != "" {
		args = append(args, "--web-dist", s.opts.WebDistDir)
	}
	if strings.TrimSpace(s.opts.GameConfigDir) != "" {
		args = append(args, "--game-config", s.opts.GameConfigDir)
	}
	args = append(args, "--auto-robot")
	quoted := make([]string, 0, len(args))
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--"):
			quoted = append(quoted, a)
		case isDigits(a): // 端口等数值参数不加引号（参照 _start_2300.bat）
			quoted = append(quoted, a)
		default:
			quoted = append(quoted, `"`+a+`"`)
		}
	}
	lines = append(lines, "start \"zyctrlcenter-"+zoneTag+"\" /min \""+filepath.Join(s.opts.ExeDir, ctrlImageName)+"\" "+strings.Join(quoted, " "))
	lines = append(lines, "")
	return strings.Join(lines, "\r\n")
}

// isDigits 纯数字串（bat 端口参数是否加引号用）。
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
