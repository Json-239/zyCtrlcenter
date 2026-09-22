// Package config 集中配置（骨架层：只放基础设施配置，业务参数由使用方自行扩展）。
//
// 配置来源优先级（从高到低）：
//  1. CLI 参数（--web-port/--ctrl-port/--deploy/--data-dir/--base-dir/--auto-robot/--kill-robots/--api-token/--web-dist）
//  2. 环境变量（CTRL_WEB_PORT / CTRL_CTRL_PORT / CTRL_WEB_HOST / CTRL_CTRL_HOST /
//     CTRL_DEPLOY_DIR / CTRL_DATA_DIR / CTRL_RUNS_KEEP_DAYS / CTRL_RUNS_MAX_MB /
//     CTRL_LOG_KEEP_DAYS / CTRL_GHOST_NAV_CHAIN / CTRL_GHOST_BASE_CHAIN / CTRL_GHOST_DAILY_LIMIT /
//     CTRL_GAME_VERSION / CTRL_NEWBIE_MAX_LEVEL / CTRL_AUTO_RESTORE / API_TOKEN /
//     CTRL_LIVECOUNT_ENABLED / CTRL_LIVECOUNT_URL / CTRL_LIVECOUNT_SERVER_ID /
//     CTRL_LIVECOUNT_INTERVAL_SEC / CTRL_LIVECOUNT_TIMEOUT_SEC / CTRL_LIVECOUNT_TOKEN /
//     CTRL_CREATE_ADAPTIVE / CTRL_CREATE_MAX_CONCURRENCY / CTRL_CREATE_MIN_CONCURRENCY /
//     CTRL_CREATE_BATCH_INTERVAL_SEC / CTRL_CREATE_JITTER_SEC / CTRL_WEB_DIST）
//  3. config.local.json（本地覆盖，不入库：api_token）
//  4. 本文件内置默认值
//
// 本包为纯配置，不依赖任何业务模块。
package config

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Version 中控版本（/api/status 与启动横幅展示）。
const Version = "0.1.0"

// 端口分配（与参考项目 Python 版错开，避免同机互抢）：
//   - HTTP API 28082（Python 版 18082）
//   - 控制通道 27200（Python 版 17200）
//   - 前端 dev 5273（Python 版 5173）
const (
	DefaultWebPort  = 28082
	DefaultCtrlPort = 27200
)

// Config 全局配置（启动时解析一次，运行期只读）。
type Config struct {
	WebHost  string
	WebPort  int
	CtrlHost string
	CtrlPort int

	BaseDir         string
	DeployDir       string
	RobotExe        string
	RobotConfigPy   string
	DataDir         string
	ChainDir        string // 链数据目录（<DataDir>/chains，可选）
	MapsFile        string // 地图名表（<DataDir>/maps.json：map_index → 名称）
	AccountsFile    string // 账号池（<DataDir>/accounts.json，由 tools/import_accounts.py 导入）
	GameConfigDir   string // 游戏配置目录（含 map.csv 与 map_file/blockfile，用于地图网格可视化）
	GridCell        int    // 客户端坐标换算：1 格 = 多少像素（默认 16，客户端显示 = 像素/16）
	LogsDir         string
	LocalConfigFile string
	// WebDistDir 前端面板构建产物目录（Vue3 + Vite 的 dist，含 index.html）。
	// 目录存在时中控直接托管面板（GET / 与其余非 /api、/ws 的 GET 请求），不存在则 / 仍返回 JSON 提示。
	// 默认 <项目根>/web/dist，可用 --web-dist 或环境变量 CTRL_WEB_DIST 覆盖。
	WebDistDir string

	// DefaultChainID 未显式指定链 ID 时的默认值（仅透传给机器人，不内置链数据）。
	DefaultChainID string
	// GhostNavChainID 抓鬼导航数据（钟馗抓鬼日常用：ghost_maps/ghost_map_pos，可选 maps）
	// 对应的链数据文件名。**只需放抓鬼专属字段**：基座的 npcs/map_grids/dijkstra 会自动从
	// GhostBaseChainID 复用（口径对齐参考实现 services/chain.py 的 ghost_nav_payload）；
	// 文件缺失或地址不全即硬失败（不静默降级）。可用环境变量 CTRL_GHOST_NAV_CHAIN 覆盖。
	GhostNavChainID string
	// GhostBaseChainID 抓鬼导航的**基座链**（提供 npcs/map_grids/dijkstra/grid_cell/item_meta）。
	// 默认 newbie_full —— 与参考实现一致：抓鬼复用新手链的导航数据，只额外补刷鬼图与落点。
	// 可用环境变量 CTRL_GHOST_BASE_CHAIN 覆盖。
	GhostBaseChainID string
	// GhostDailyLimit 抓鬼每日上限（下发给机器人；与参考实现默认 50 一致）。
	// 可用环境变量 CTRL_GHOST_DAILY_LIMIT 覆盖。
	GhostDailyLimit int

	// AutoRegister* 定时任务的"没号时自动注册"用（默认按本项目的账号命名口径：
	// robot + 7 位序号 + @xy3.com，如 robot0001000@xy3.com）。可用环境变量
	// CTRL_AUTO_REGISTER_PREFIX / _SUFFIX / _PAD 覆盖。
	AutoRegisterPrefix string
	AutoRegisterSuffix string
	AutoRegisterPad    int
	// GameVersion 客户端协议版本：中控直连游戏服探测账号（101 版本验证）时用。
	// 可用环境变量 CTRL_GAME_VERSION 覆盖；接口也支持按次传 version。
	GameVersion string
	// AutoRemoveOnDone 机器人上报 chain_done/ghost_done/ghost_offline 时是否自动下机
	// （通知机器人移除账号 + 删行；业务处置如换号/冷却由使用方自行实现）。
	AutoRemoveOnDone bool
	// NewbieMaxLevel 新手链等级阈值：等级 ≥ 此值（或链已完成）→ 判抓鬼，否则新手链优先。
	// 与机器人端 config.start_chain_done_level 口径一致（默认 31）。
	// 可用环境变量 CTRL_NEWBIE_MAX_LEVEL 覆盖。
	NewbieMaxLevel int
	// AutoRestore 恢复引擎总开关：中控重启/机器人重连后按意图自动补发 start_chain/ghost_start。
	// **默认关**（会真给机器人发命令）；环境变量 CTRL_AUTO_RESTORE=1 打开。
	AutoRestore bool

	RunsKeepDays int // 运行历史保留天数（按日期切分）
	RunsMaxMB    int // 单日运行历史熔断阈值（MB）
	LogKeepDays  int // 应用日志保留天数

	// APIToken 高危写接口鉴权 token（空=关闭）。
	APIToken string

	// LiveCount* 「服务端在线数」直连数据源（internal/services/livecount，默认关闭）。
	// 背景：机器人端 `@online` 管理命令需要服务端 admin_license 的 DEPLOY 授权，生产服未授权时
	// 拿不到数；参考 game_admin_web(origin/hqm) 的做法改为**中控直接 HTTP 拉游戏服** /gm/online
	// （服务端自带接口，实测不需要 GM 授权，返回 {online_count}；含真实玩家，机器人数另有字段）。
	// 读到的数写进 /api/status.svr_online（source=svr_provider）并供在线水位保持器取用；
	// 拉取失败自动回落到 @online 回执 → 本地握手数，不影响主链路。
	// 环境变量：CTRL_LIVECOUNT_ENABLED / _URL / _SERVER_ID / _INTERVAL_SEC / _TIMEOUT_SEC / _TOKEN
	LiveCountEnabled     bool   // 总开关（默认 false）
	LiveCountURL         string // 游戏服 HTTP 基地址，如 http://192.168.0.201:8080（空=禁用）
	LiveCountServerID    string // 区号（默认 "1000"）
	LiveCountIntervalSec int    // 轮询间隔（秒，默认 60）
	LiveCountTimeoutSec  int    // 单次 HTTP 超时（秒，默认 3）
	LiveCountToken       string // 可选鉴权头 X-GM-Token（当前接口无鉴权，留空即可）

	// RoamWorldMaps 游荡世界图白名单（2026-09-22）：随机图游荡只从这些图里抽，排掉"大图里的小图"（房间/店铺/洞穴，如回春药铺 616）。来源：服务端 config/worldmap/worldmap.csv
	// （44 张，缺省内置）。环境变量 CTRL_ROAM_WORLD_MAPS="1,2,3" 可覆盖；空/全非法 = 不过滤。
	// 2026-09-22 用户口径：幽冥界(24) 已从缺省列表剔除（抓鬼专属，游荡不派）→ 43 张。
	RoamWorldMaps []int

	// RoamExcludeMaps 游荡排除图（2026-09-22 用户口径）：幽冥界 24 是抓鬼专属（钟馗所在图），
	// 游荡一律不去 —— 显式选图会被拒绝；随机图/显式 maps 白名单里的该图也会被剔除。
	// 环境变量 CTRL_ROAM_EXCLUDE_MAPS="24" 可覆盖（留空 = 不排除）。
	RoamExcludeMaps []int

	// Create* 建号（注册协议 106→104→700）的**节奏限制**：同 IP 过频会触发风控码 112
	// （服务端 C++ login_server 的规则，改不动），所以除了"并发 ≤8、批量 ≤20"的硬上限，
	// 再加一层进程内**自适应限速**（internal/api/handlers_create.go 的 CreateThrottle）：
	// 撞 112 降速（并发减半/批间隔加倍）、连续成功回升；批间隔带抖动避免整齐节拍。
	// **不动协议字段**（106/104/700 与 8 个字段不变），只改节奏。
	// 环境变量：CTRL_CREATE_ADAPTIVE / _MAX_CONCURRENCY / _MIN_CONCURRENCY / _BATCH_INTERVAL_SEC / _JITTER_SEC
	CreateAdaptive         bool // 自适应限速总开关（默认 true）
	CreateMaxConcurrency   int  // 并发上限（默认 8：再高更容易触发同 IP 频控）
	CreateMinConcurrency   int  // 并发下限（降速降到这里为止，默认 2）
	CreateBatchIntervalSec int  // 批间隔基准（秒，默认 5；请求里的 batch_interval_ms 只能更保守）
	CreateJitterSec        int  // 批间隔抖动（±秒，默认 2：避免整齐节拍被风控盯上）

	// AutoStartRobot=true 时启动中控会拉起 robot_single_robot.exe（默认关闭，
	// 避免与其它中控/生产实例互相抢账号；用 --auto-robot 显式开启）。
	AutoStartRobot bool
	// KillRobots=true 时重启机器人会先按镜像名清理残留进程（默认关闭）。
	KillRobots bool

	// RobotImageName 进程镜像名（Windows 下用于 tasklist/taskkill）。
	RobotImageName string
}

// CtrlAddr 控制通道地址（host:port）。
func (c *Config) CtrlAddr() string { return fmt.Sprintf("%s:%d", c.CtrlHost, c.CtrlPort) }

// WebAddr HTTP API 地址（host:port）。
func (c *Config) WebAddr() string { return fmt.Sprintf("%s:%d", c.WebHost, c.WebPort) }

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// envBool 布尔环境变量（"1"/"true"/"on"/"yes" 为真；其它按假）。
func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return def
	}
	switch v {
	case "1", "true", "on", "yes", "y":
		return true
	case "0", "false", "off", "no", "n":
		return false
	}
	return def
}

// Default 返回不解析 CLI 的默认配置（测试/内嵌调用用）。
func Default() *Config {
	base, err := os.Getwd()
	if err != nil {
		base = "."
	}
	return build(base, nil)
}

// Load 解析 CLI + 环境变量 + 本地覆盖配置，返回最终配置。
// args 传 nil 时使用 os.Args[1:]；测试中可显式传 []string{} 避免吃 go test 参数。
func Load(args []string) *Config {
	base, err := os.Getwd()
	if err != nil {
		base = "."
	}
	if args == nil {
		args = os.Args[1:]
	}

	fs := flag.NewFlagSet("zyctrlcenter", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	webPort := fs.Int("web-port", envInt("CTRL_WEB_PORT", DefaultWebPort), "中控 HTTP API 端口")
	ctrlPort := fs.Int("ctrl-port", envInt("CTRL_CTRL_PORT", DefaultCtrlPort), "机器人控制通道端口")
	baseDir := fs.String("base-dir", "", "项目根目录（默认当前工作目录）")
	deploy := fs.String("deploy", env("CTRL_DEPLOY_DIR", ""), "机器人部署目录（robot_single_robot.exe 所在）")
	dataDir := fs.String("data-dir", env("CTRL_DATA_DIR", ""), "数据目录（默认 <项目根>/data）")
	autoRobot := fs.Bool("auto-robot", false, "启动时自动拉起 robot_single_robot.exe")
	killRobots := fs.Bool("kill-robots", false, "重启机器人前先清理同名残留进程")
	apiToken := fs.String("api-token", "", "高危写接口鉴权 token（空=关闭）")
	gameConfig := fs.String("game-config", env("CTRL_GAME_CONFIG_DIR", ""), "游戏配置目录（含 map.csv 与 map_file/blockfile，用于地图可视化）")
	webDist := fs.String("web-dist", env("CTRL_WEB_DIST", ""), "前端面板构建产物目录（默认 <项目根>/web/dist；存在才由中控直接托管面板）")
	_ = fs.Parse(args) // 忽略未知参数（go test 等场景传入的参数不影响）

	if *baseDir != "" {
		base = absPath(*baseDir)
	}
	return build(base, &cliOpts{
		webPort:    webPort,
		ctrlPort:   ctrlPort,
		deploy:     deploy,
		dataDir:    dataDir,
		gameConfig: gameConfig,
		webDist:    webDist,
		autoRobot:  autoRobot,
		killRobots: killRobots,
		apiToken:   apiToken,
	})
}

type cliOpts struct {
	webPort, ctrlPort                              *int
	deploy, dataDir, apiToken, gameConfig, webDist *string
	autoRobot, killRobots                          *bool
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func build(base string, opts *cliOpts) *Config {
	c := &Config{
		WebHost:            env("CTRL_WEB_HOST", "127.0.0.1"),
		WebPort:            envInt("CTRL_WEB_PORT", DefaultWebPort),
		CtrlHost:           env("CTRL_CTRL_HOST", "127.0.0.1"),
		CtrlPort:           envInt("CTRL_CTRL_PORT", DefaultCtrlPort),
		BaseDir:            base,
		DefaultChainID:     "newbie_full",
		GhostNavChainID:    env("CTRL_GHOST_NAV_CHAIN", "zhongkui_nav"),
		GhostBaseChainID:   env("CTRL_GHOST_BASE_CHAIN", "newbie_full"),
		GhostDailyLimit:    envInt("CTRL_GHOST_DAILY_LIMIT", 50),
		AutoRegisterPrefix: env("CTRL_AUTO_REGISTER_PREFIX", "robot"),
		AutoRegisterSuffix: env("CTRL_AUTO_REGISTER_SUFFIX", "@xy3.com"),
		AutoRegisterPad:    envInt("CTRL_AUTO_REGISTER_PAD", 7),
		GameVersion:        env("CTRL_GAME_VERSION", "58740022"),
		AutoRemoveOnDone:   true,
		NewbieMaxLevel:     envInt("CTRL_NEWBIE_MAX_LEVEL", 31),
		AutoRestore:        envBool("CTRL_AUTO_RESTORE", false),
		// 服务端在线数直连数据源（默认关；启用需同时给 CTRL_LIVECOUNT_URL）
		LiveCountEnabled:     envBool("CTRL_LIVECOUNT_ENABLED", false),
		LiveCountURL:         env("CTRL_LIVECOUNT_URL", ""),
		LiveCountServerID:    env("CTRL_LIVECOUNT_SERVER_ID", "1000"),
		LiveCountIntervalSec: envInt("CTRL_LIVECOUNT_INTERVAL_SEC", 60),
		LiveCountTimeoutSec:  envInt("CTRL_LIVECOUNT_TIMEOUT_SEC", 3),
		LiveCountToken:       env("CTRL_LIVECOUNT_TOKEN", ""),
		RoamWorldMaps:        envIntList("CTRL_ROAM_WORLD_MAPS", []int{1, 2, 3, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 25, 26, 27, 31, 32, 34, 35, 37, 38, 39, 40, 41, 42, 43, 44, 45, 46, 47, 48, 49, 609}),
		// 2026-09-22 用户口径：幽冥界 24 是抓鬼专属，游荡排除（显式选图拒绝 + 白名单剔除）
		RoamExcludeMaps:      envIntList("CTRL_ROAM_EXCLUDE_MAPS", []int{24}),
		// 建号（注册）节奏：自适应限速（默认开、保守）+ 批间隔抖动
		CreateAdaptive:         envBool("CTRL_CREATE_ADAPTIVE", true),
		CreateMaxConcurrency:   envInt("CTRL_CREATE_MAX_CONCURRENCY", 8),
		CreateMinConcurrency:   envInt("CTRL_CREATE_MIN_CONCURRENCY", 2),
		CreateBatchIntervalSec: envInt("CTRL_CREATE_BATCH_INTERVAL_SEC", 5),
		CreateJitterSec:        envInt("CTRL_CREATE_JITTER_SEC", 2),
		RunsKeepDays:           envInt("CTRL_RUNS_KEEP_DAYS", 30),
		RunsMaxMB:              envInt("CTRL_RUNS_MAX_MB", 500),
		LogKeepDays:            envInt("CTRL_LOG_KEEP_DAYS", 30),
		GridCell:               envInt("CTRL_GRID_CELL", 16),
		RobotImageName:         "robot_single_robot.exe",
	}

	if opts != nil {
		if opts.webPort != nil {
			c.WebPort = *opts.webPort
		}
		if opts.ctrlPort != nil {
			c.CtrlPort = *opts.ctrlPort
		}
		if opts.deploy != nil && *opts.deploy != "" {
			c.DeployDir = absPath(*opts.deploy)
		}
		if opts.dataDir != nil && *opts.dataDir != "" {
			c.DataDir = absPath(*opts.dataDir)
		}
		if opts.gameConfig != nil && *opts.gameConfig != "" {
			c.GameConfigDir = absPath(*opts.gameConfig)
		}
		if opts.webDist != nil && *opts.webDist != "" {
			c.WebDistDir = absPath(*opts.webDist)
		}
		if opts.apiToken != nil {
			c.APIToken = *opts.apiToken
		}
		if opts.autoRobot != nil {
			c.AutoStartRobot = *opts.autoRobot
		}
		if opts.killRobots != nil {
			c.KillRobots = *opts.killRobots
		}
	}

	if c.DeployDir == "" {
		c.DeployDir = defaultDeployDir(base)
	}
	if c.DataDir == "" {
		c.DataDir = filepath.Join(base, "data")
	}
	c.ChainDir = filepath.Join(c.DataDir, "chains")
	c.MapsFile = filepath.Join(c.DataDir, "maps.json")
	c.AccountsFile = filepath.Join(c.DataDir, "accounts.json")
	if c.GridCell <= 0 {
		c.GridCell = 16
	}
	if c.GameConfigDir == "" {
		c.GameConfigDir = defaultGameConfigDir(base)
	}
	c.LogsDir = filepath.Join(base, "logs")
	c.LocalConfigFile = filepath.Join(base, "config.local.json")
	if c.WebDistDir == "" {
		c.WebDistDir = filepath.Join(base, "web", "dist")
	}
	// 环境变量兜底（优先级低于 CLI；CLI 未给时 flag 默认值已取到）
	if v := env("CTRL_WEB_DIST", ""); v != "" && (opts == nil || opts.webDist == nil || *opts.webDist == "") {
		c.WebDistDir = absPath(v)
	}
	c.RobotExe = filepath.Join(c.DeployDir, "robot_single_robot.exe")
	c.RobotConfigPy = filepath.Join(c.DeployDir, "script", "config.py")

	// 环境变量兜底（优先级低于 CLI；CLI 未给时已由 flag 默认值取到）
	if v := env("CTRL_DATA_DIR", ""); v != "" && (opts == nil || opts.dataDir == nil || *opts.dataDir == "") {
		c.DataDir = absPath(v)
		c.ChainDir = filepath.Join(c.DataDir, "chains")
		c.MapsFile = filepath.Join(c.DataDir, "maps.json")
	}

	// config.local.json（不入库）：api_token 本地覆盖
	if c.APIToken == "" {
		c.APIToken = localToken(c.LocalConfigFile)
	}
	if c.APIToken == "" {
		c.APIToken = strings.TrimSpace(os.Getenv("API_TOKEN"))
	}
	return c
}

// defaultDeployDir 选择机器人部署目录：
//  1. <项目根>/deploy/single_robot（本项目自带部署目录，推荐）
//  2. <项目根>/../xm/2d-xiyou-server/robot/deploy/single_robot（参考项目部署目录）
//
// 都找不到时返回候选 1（可用 --deploy 覆盖）。
func defaultDeployDir(base string) string {
	local := filepath.Join(base, "deploy", "single_robot")
	if fileExists(filepath.Join(local, "robot_single_robot.exe")) {
		return local
	}
	sibling := filepath.Join(base, "..", "xm", "2d-xiyou-server", "robot", "deploy", "single_robot")
	if fileExists(filepath.Join(sibling, "robot_single_robot.exe")) {
		return absPath(sibling)
	}
	return local
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// defaultGameConfigDir 游戏配置目录（地图网格用）：
//  1. <项目根>/game-config（本项目自带，含 map.csv）
//  2. <项目根>/../xm/2d-xiyou-server/config（参考项目）
//
// 都找不到时返回候选 1（可用 --game-config 覆盖）。
func defaultGameConfigDir(base string) string {
	local := filepath.Join(base, "game-config")
	if fileExists(filepath.Join(local, "map.csv")) {
		return local
	}
	sibling := filepath.Join(base, "..", "xm", "2d-xiyou-server", "config")
	if fileExists(filepath.Join(sibling, "map.csv")) {
		return absPath(sibling)
	}
	return local
}

// localToken 读取 config.local.json 的 api_token（占位符 CHANGE_ME* 视为未配置）。
func localToken(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var obj struct {
		APIToken string `json:"api_token"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	tok := strings.TrimSpace(obj.APIToken)
	if strings.HasPrefix(tok, "CHANGE_ME") {
		return ""
	}
	return tok
}

// envIntList 解析 "1,2,3" 形式的整数列表环境变量（空/非法项忽略；全空则用默认）。
func envIntList(key string, def []int) []int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	out := make([]int, 0, 16)
	for _, seg := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
		if n, err := strconv.Atoi(strings.TrimSpace(seg)); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}
