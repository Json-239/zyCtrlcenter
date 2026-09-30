// config 模块测试：默认值（含新端口）/ CLI 优先级 / 环境变量 / 占位符 token。
package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"zyctrlcenter/internal/config"
)

func TestDefaultPortsAreProjectSpecific(t *testing.T) {
	cfg := config.Default()
	// 与参考项目 Python 版错开：18082/17200 → 28082/27200
	if cfg.WebPort != 28082 || cfg.CtrlPort != 27200 {
		t.Fatalf("默认端口应为 28082/27200，实际 %d/%d", cfg.WebPort, cfg.CtrlPort)
	}
	if config.DefaultWebPort != 28082 || config.DefaultCtrlPort != 27200 {
		t.Fatalf("导出常量应与默认值一致: %d/%d", config.DefaultWebPort, config.DefaultCtrlPort)
	}
}

func TestDefaultPathsUnderWorkDir(t *testing.T) {
	cfg := config.Default()
	if filepath.Base(cfg.DataDir) != "data" {
		t.Fatalf("数据目录应为 <项目根>/data，实际 %s", cfg.DataDir)
	}
	if cfg.ChainDir != filepath.Join(cfg.DataDir, "chains") {
		t.Fatalf("链数据目录应为 <数据目录>/chains，实际 %s", cfg.ChainDir)
	}
	if filepath.Base(cfg.LogsDir) != "logs" {
		t.Fatalf("日志目录应为 <项目根>/logs，实际 %s", cfg.LogsDir)
	}
	if filepath.Base(cfg.RobotExe) != "robot_single_robot.exe" {
		t.Fatalf("机器人程序名不符: %s", cfg.RobotExe)
	}
	if cfg.RunsKeepDays != 30 || cfg.RunsMaxMB != 500 {
		t.Fatalf("日志保留/熔断默认值应为 30 天/500MB，实际 %d/%d", cfg.RunsKeepDays, cfg.RunsMaxMB)
	}
	if cfg.DefaultChainID != "newbie_full" {
		t.Fatalf("默认链不符: %s", cfg.DefaultChainID)
	}
	if !cfg.AutoRemoveOnDone {
		t.Fatal("AutoRemoveOnDone 默认应为 true")
	}
}

func TestLoadAppliesCLIFlags(t *testing.T) {
	dataDir := t.TempDir()
	deployDir := t.TempDir()
	cfg := config.Load([]string{
		"--web-port", "28099",
		"--ctrl-port", "27299",
		"--data-dir", dataDir,
		"--deploy", deployDir,
		"--auto-robot",
		"--kill-robots",
		"--api-token", "tok123",
	})
	if cfg.WebPort != 28099 || cfg.CtrlPort != 27299 {
		t.Fatalf("CLI 端口未生效: %d/%d", cfg.WebPort, cfg.CtrlPort)
	}
	if cfg.DataDir != dataDir {
		t.Fatalf("CLI 数据目录未生效: %s", cfg.DataDir)
	}
	if cfg.ChainDir != filepath.Join(dataDir, "chains") {
		t.Fatalf("链数据目录应随数据目录切换: %s", cfg.ChainDir)
	}
	if cfg.RobotExe != filepath.Join(deployDir, "robot_single_robot.exe") {
		t.Fatalf("--deploy 未生效: %s", cfg.RobotExe)
	}
	if !cfg.AutoStartRobot || !cfg.KillRobots {
		t.Fatal("--auto-robot / --kill-robots 开关未生效")
	}
	if cfg.APIToken != "tok123" {
		t.Fatalf("--api-token 未生效: %q", cfg.APIToken)
	}
}

func TestEnvOverridesDefaults(t *testing.T) {
	t.Setenv("CTRL_WEB_PORT", "28077")
	t.Setenv("CTRL_CTRL_PORT", "27277")
	t.Setenv("CTRL_RUNS_KEEP_DAYS", "7")
	cfg := config.Load([]string{})
	if cfg.WebPort != 28077 || cfg.CtrlPort != 27277 {
		t.Fatalf("环境变量端口未生效: %d/%d", cfg.WebPort, cfg.CtrlPort)
	}
	if cfg.RunsKeepDays != 7 {
		t.Fatalf("环境变量保留天数未生效: %d", cfg.RunsKeepDays)
	}
}

// 新手链等级阈值：默认 31（与机器人端 config.start_chain_done_level 一致），可用环境变量覆盖。
func TestNewbieMaxLevelDefaultAndEnv(t *testing.T) {
	if got := config.Default().NewbieMaxLevel; got != 31 {
		t.Fatalf("默认阈值应为 31（与参考实现口径一致）: %d", got)
	}
	t.Setenv("CTRL_NEWBIE_MAX_LEVEL", "45")
	if got := config.Load([]string{}).NewbieMaxLevel; got != 45 {
		t.Fatalf("环境变量应能覆盖阈值: %d", got)
	}
}

// 抓鬼导航：专属文件名 / 基座链 / 每日上限都有默认值，且都能用环境变量覆盖。
func TestGhostNavChainIDDefaultAndEnv(t *testing.T) {
	def := config.Default()
	if def.GhostNavChainID != "zhongkui_nav" {
		t.Fatalf("默认抓鬼导航链应为 zhongkui_nav: %q", def.GhostNavChainID)
	}
	if def.GhostBaseChainID != "newbie_full" {
		t.Fatalf("抓鬼导航基座默认应为 newbie_full（复用新手链的坐标/网格/路由）: %q", def.GhostBaseChainID)
	}
	if def.GhostDailyLimit != 50 {
		t.Fatalf("抓鬼每日上限默认应为 50（与机器人默认同口径）: %d", def.GhostDailyLimit)
	}
	t.Setenv("CTRL_GHOST_NAV_CHAIN", "ghost_nav_alt")
	t.Setenv("CTRL_GHOST_BASE_CHAIN", "main_chain")
	t.Setenv("CTRL_GHOST_DAILY_LIMIT", "20")
	got := config.Load([]string{})
	if got.GhostNavChainID != "ghost_nav_alt" || got.GhostBaseChainID != "main_chain" || got.GhostDailyLimit != 20 {
		t.Fatalf("环境变量应能覆盖抓鬼导航配置: %q / %q / %d",
			got.GhostNavChainID, got.GhostBaseChainID, got.GhostDailyLimit)
	}
}

// 烽火大唐（fenghuo，分享日常第二玩法）：开关默认关 + 声明文件/玩法键/日限/门槛默认值，
// 且都能用独立环境变量覆盖（**不影响** shenbu 的 CTRL_SHARE_DAILY* 一族）。
func TestFenghuoDefaultsAndEnv(t *testing.T) {
	def := config.Default()
	if def.FenghuoEnabled {
		t.Fatal("FenghuoEnabled 默认必须关闭（灰度期由 CTRL_FENGHUO=1 显式打开）")
	}
	if def.FenghuoChainID != "fenghuo_nav" {
		t.Fatalf("默认声明文件应为 fenghuo_nav: %q", def.FenghuoChainID)
	}
	if def.FenghuoKey != "share_daily_宫廷10" {
		t.Fatalf("默认玩法键应为 share_daily_宫廷10（20021.xml share_daily_key）: %q", def.FenghuoKey)
	}
	if def.FenghuoDailyLimit != 20 {
		t.Fatalf("默认日限应为 20（20021.xml daily_limit）: %d", def.FenghuoDailyLimit)
	}
	if def.FenghuoMinLevel != 40 {
		t.Fatalf("默认等级门槛应为 40（票条件）: %d", def.FenghuoMinLevel)
	}
	// shenbu 的默认值不受影响（两族配置互不干扰）
	if def.ShareDailyEnabled || def.ShareDailyKey != "share_daily_大唐神捕" || def.ShareDailyDailyLimit != 10 {
		t.Fatalf("shenbu 默认配置被破坏: enabled=%v key=%q limit=%d",
			def.ShareDailyEnabled, def.ShareDailyKey, def.ShareDailyDailyLimit)
	}

	t.Setenv("CTRL_FENGHUO", "1")
	t.Setenv("CTRL_FENGHUO_CHAIN", "fenghuo_nav_alt")
	t.Setenv("CTRL_FENGHUO_KEY", "share_daily_alt")
	t.Setenv("CTRL_FENGHUO_LIMIT", "30")
	t.Setenv("CTRL_FENGHUO_MIN_LEVEL", "45")
	got := config.Load([]string{})
	if !got.FenghuoEnabled || got.FenghuoChainID != "fenghuo_nav_alt" ||
		got.FenghuoKey != "share_daily_alt" || got.FenghuoDailyLimit != 30 || got.FenghuoMinLevel != 45 {
		t.Fatalf("环境变量应能覆盖烽火大唐配置: enabled=%v chain=%q key=%q limit=%d min_level=%d",
			got.FenghuoEnabled, got.FenghuoChainID, got.FenghuoKey, got.FenghuoDailyLimit, got.FenghuoMinLevel)
	}
	// 覆盖 fenghuo 不应改变 shenbu 的开关（键名独立）
	if got.ShareDailyEnabled {
		t.Fatal("打开 CTRL_FENGHUO 不该连带打开 CTRL_SHARE_DAILY（两族开关独立）")
	}
}

func TestCLIWinsOverEnv(t *testing.T) {
	t.Setenv("CTRL_WEB_PORT", "28077")
	cfg := config.Load([]string{"--web-port", "28088"})
	if cfg.WebPort != 28088 {
		t.Fatalf("CLI 应优先于环境变量，实际 %d", cfg.WebPort)
	}
}

func TestLocalConfigTokenTemplateIgnored(t *testing.T) {
	base := t.TempDir()
	// 模板占位符（CHANGE_ME）必须视为未配置，避免误开启鉴权
	if err := os.WriteFile(filepath.Join(base, "config.local.json"),
		[]byte(`{"api_token": "CHANGE_ME_TOKEN"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Load([]string{"--base-dir", base})
	if cfg.APIToken != "" {
		t.Fatalf("占位符 token 不应启用鉴权，实际 %q", cfg.APIToken)
	}
}

func TestAddrHelpers(t *testing.T) {
        cfg := config.Default()
        if cfg.CtrlAddr() != "127.0.0.1:27200" {
                t.Fatalf("CtrlAddr 不符: %s", cfg.CtrlAddr())
        }
        if cfg.WebAddr() != "127.0.0.1:28082" {
                t.Fatalf("WebAddr 不符: %s", cfg.WebAddr())
        }
}

func TestLiveCountDefaultsAndEnv(t *testing.T) {
        // 默认关闭且不配端点：不会自己动生产（启用需同时给 URL）
        cfg := config.Default()
        if cfg.LiveCountEnabled || cfg.LiveCountURL != "" {
                t.Fatalf("LiveCount 默认应为关闭且无端点，实际 enabled=%v url=%q",
                        cfg.LiveCountEnabled, cfg.LiveCountURL)
        }
        if cfg.LiveCountServerID != "1000" || cfg.LiveCountIntervalSec != 60 || cfg.LiveCountTimeoutSec != 3 {
                t.Fatalf("LiveCount 默认参数不符：server=%q interval=%d timeout=%d",
                        cfg.LiveCountServerID, cfg.LiveCountIntervalSec, cfg.LiveCountTimeoutSec)
        }
        if cfg.LiveCountToken != "" {
                t.Fatalf("LiveCount 默认不应带 token，实际 %q", cfg.LiveCountToken)
        }
        // 环境变量整套可覆盖（参考实现所在测试服口径）
        t.Setenv("CTRL_LIVECOUNT_ENABLED", "1")
        t.Setenv("CTRL_LIVECOUNT_URL", "http://192.168.0.201:8080")
        t.Setenv("CTRL_LIVECOUNT_SERVER_ID", "1001")
        t.Setenv("CTRL_LIVECOUNT_INTERVAL_SEC", "30")
        t.Setenv("CTRL_LIVECOUNT_TIMEOUT_SEC", "5")
        t.Setenv("CTRL_LIVECOUNT_TOKEN", "tok-abc")
        got := config.Load([]string{})
        if !got.LiveCountEnabled || got.LiveCountURL != "http://192.168.0.201:8080" ||
                got.LiveCountServerID != "1001" || got.LiveCountIntervalSec != 30 ||
                got.LiveCountTimeoutSec != 5 || got.LiveCountToken != "tok-abc" {
                t.Fatalf("环境变量应覆盖 LiveCount 配置，实际 enabled=%v url=%q server=%q interval=%d timeout=%d token=%q",
                        got.LiveCountEnabled, got.LiveCountURL, got.LiveCountServerID,
                        got.LiveCountIntervalSec, got.LiveCountTimeoutSec, got.LiveCountToken)
        }
}

// 游荡排除图（2026-09-28 追加轮回司 25）：默认排除表含 25；世界图白名单不含 25（同口径剔除）。
func TestRoamExcludeMapsIncludesMap25(t *testing.T) {
	def := config.Default()
	if !intListHas(def.RoamExcludeMaps, 25) {
		t.Fatalf("默认游荡排除表应含轮回司 25，实际 %v", def.RoamExcludeMaps)
	}
	if intListHas(def.RoamWorldMaps, 25) {
		t.Fatalf("世界图白名单不应含已排除的 25，实际 %v", def.RoamWorldMaps)
	}
	// 既有排除图不被破坏；白名单其余世界图保留（如长安 11 / 609）
	for _, m := range []int{24, 653, 654, 655} {
		if !intListHas(def.RoamExcludeMaps, m) {
			t.Fatalf("既有排除图 %d 丢失: %v", m, def.RoamExcludeMaps)
		}
	}
	if !intListHas(def.RoamWorldMaps, 11) || !intListHas(def.RoamWorldMaps, 609) {
		t.Fatalf("世界图白名单被误删（长安 11 / 609 应保留）: %v", def.RoamWorldMaps)
	}
	// 环境变量覆盖仍生效（只给 24,25 → 就这两张）
	t.Setenv("CTRL_ROAM_EXCLUDE_MAPS", "24,25")
	got := config.Load([]string{})
	if len(got.RoamExcludeMaps) != 2 || got.RoamExcludeMaps[0] != 24 || got.RoamExcludeMaps[1] != 25 {
		t.Fatalf("CTRL_ROAM_EXCLUDE_MAPS 覆盖失败: %v", got.RoamExcludeMaps)
	}
}

// 游荡排除图（2026-09-30 追加瑶池回廊 45，用户批准）：默认排除表含 45；世界图白名单不含 45（同 25 口径）。
func TestRoamExcludeMapsIncludesMap45(t *testing.T) {
	def := config.Default()
	if !intListHas(def.RoamExcludeMaps, 45) {
		t.Fatalf("默认游荡排除表应含瑶池回廊 45，实际 %v", def.RoamExcludeMaps)
	}
	if intListHas(def.RoamWorldMaps, 45) {
		t.Fatalf("世界图白名单不应含已排除的 45，实际 %v", def.RoamWorldMaps)
	}
	// 既有排除图不被破坏；45 的相邻世界图（44/46）应保留
	for _, m := range []int{24, 25, 653, 654, 655} {
		if !intListHas(def.RoamExcludeMaps, m) {
			t.Fatalf("既有排除图 %d 丢失: %v", m, def.RoamExcludeMaps)
		}
	}
	if !intListHas(def.RoamWorldMaps, 44) || !intListHas(def.RoamWorldMaps, 46) {
		t.Fatalf("45 相邻世界图（44/46）应保留: %v", def.RoamWorldMaps)
	}
}

// intListHas 整数切片是否包含 n（测试小工具）。
func intListHas(list []int, n int) bool {
	for _, v := range list {
		if v == n {
			return true
		}
	}
	return false
}
