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
