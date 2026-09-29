// zyctrlcenter —— 机器人中控骨架（Go，单机器人进程 / 多区可切换）
//
// 职责：
//   - 控制通道：一条 TCP 监听（cfg.CtrlPort），单连接 JSON-lines；
//   - HTTP API + WebSocket（默认 :28082）供面板使用（含多区配置：服/区增删改与切换）；
//   - 事件处理（表驱动）→ 状态表（带当前区标记）+ 运行历史 + 实时推送；
//   - 日志按日期落盘：data/runs_YYYYMMDD.jsonl、data/bot_logs/<账号>/runs_YYYYMMDD.log、
//     logs/ctrlcenter_YYYYMMDD.log；
//   - 机器人进程管理：**单进程**（cfg.DeployDir），默认不接管（--auto-robot 开启）。
//
// 多区：data/zones.json 里维护「服 → 区」候选（首次启动写种子：47.96.8.240 的 2400/2300）；
// 切换当前区 + 「应用到机器人」（写 config.py）+ 重启机器人 = 让机器人登录到目标区。
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"zyctrlcenter/internal/accountverify"
	"zyctrlcenter/internal/api"
	"zyctrlcenter/internal/config"
	"zyctrlcenter/internal/ctrl"
	"zyctrlcenter/internal/logging"
	"zyctrlcenter/internal/maplib"
	"zyctrlcenter/internal/services/accounts"
	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/internal/services/event"
	"zyctrlcenter/internal/services/intent"
	"zyctrlcenter/internal/services/livecount"
	"zyctrlcenter/internal/services/process"
	"zyctrlcenter/internal/services/reghost"
	"zyctrlcenter/internal/services/restorer"
	"zyctrlcenter/internal/services/roampool"
	"zyctrlcenter/internal/services/waterline"
	"zyctrlcenter/internal/services/zones"
	"zyctrlcenter/internal/state"
	"zyctrlcenter/internal/store"
	"zyctrlcenter/internal/taskname"
)

func main() {
	cfg := config.Load(nil)
	log := logging.New(cfg.LogsDir, cfg.LogKeepDays)
	defer log.Close()

	log.Printf("===== zyctrlcenter v%s 启动（单机器人进程 / 多区可切换） =====", config.Version)
	log.Printf("[MAIN] 项目根: %s", cfg.BaseDir)
	log.Printf("[MAIN] HTTP API: http://%s", cfg.WebAddr())
	log.Printf("[MAIN] 控制通道: %s（机器人连这里）", cfg.CtrlAddr())
	log.Printf("[MAIN] 数据目录: %s", cfg.DataDir)
	log.Printf("[MAIN] 日志目录: %s（按日期切分）", cfg.LogsDir)
	log.Printf("[MAIN] 机器人程序: %s（存在=%v）", cfg.RobotExe, fileExists(cfg.RobotExe))
	// 面板静态资源（web/dist）：存在则由中控直接托管（GET /），不存在时 / 返回 JSON 提示。
	if fileExists(filepath.Join(cfg.WebDistDir, "index.html")) {
		log.Printf("[MAIN] 面板静态资源: %s（中控直接托管，打开 http://%s/ 即面板）", cfg.WebDistDir, cfg.WebAddr())
	} else {
		log.Printf("[MAIN] 面板未构建（%s）：打开 http://%s/ 只有 JSON 提示；"+
			"运行 start.bat / ./start.sh 一键构建，或 cd web && npm install && npm run dev（http://localhost:5273）",
			cfg.WebDistDir, cfg.WebAddr())
	}

	// ---- 状态 / 运行历史 ----
	st := state.New()
	// 2026-09-24：分享日常"今日已派"台账落盘（机器人重启后心跳 daily 丢失时，
	// 「启动 = 恢复当前任务」靠它续跑神捕）。文件 <DataDir>/share_daily_assign.json。
	if n, err := st.EnableShareDailyAssignedPersist(filepath.Join(cfg.DataDir, "share_daily_assign.json")); err != nil {
		log.Printf("[SHAREDAILY] 已派台账加载失败（忽略；按未派处理）: %v", err)
	} else if n > 0 {
		log.Printf("[SHAREDAILY] 今日已派神捕台账已载入: %d 条（重启恢复用）", n)
	}
	// 2026-09-29：满额表落盘（重启后"✓ 完成可见"不丢——总览合成 done 分支靠它；
	// 现场 20:25 重启丢了当日 9 烽火+1 神捕的标记）。文件 <DataDir>/share_daily_full.json。
	if n, err := st.EnableShareDailyFullPersist(filepath.Join(cfg.DataDir, "share_daily_full.json")); err != nil {
		log.Printf("[SHAREDAILY] 满额表加载失败（忽略；按未满处理）: %v", err)
	} else if n > 0 {
		log.Printf("[SHAREDAILY] 今日满额表已载入: %d 条（重启恢复用）", n)
	}
	runStore := store.New(cfg.DataDir, cfg.RunsKeepDays, cfg.RunsMaxMB)
	if removed := runStore.CleanupOldRuns(cfg.RunsKeepDays); len(removed) > 0 {
		log.Printf("[STORE] 已清理超过 %d 天的历史运行日志: %v", cfg.RunsKeepDays, removed)
	}
	log.Printf("[STORE] 当天运行历史: %s", runStore.CurrentPath())

	// ---- 多区配置（服 → 区；单进程：只有一个"当前区"）----
	zreg := zones.New(filepath.Join(cfg.DataDir, "zones.json"))
	seeded, err := zreg.Load()
	if err != nil {
		log.Printf("[ZONES] 配置加载失败（将使用空配置）: %v", err)
	}
	if seeded {
		log.Printf("[ZONES] 首次启动：已写入默认种子区（可在面板「系统信息」页修改）")
	}
	log.Printf("[ZONES] 配置文件: %s", zreg.Path())

	// ---- 地图名表（mapid → 名称；面板显示 + 客户端坐标口径）----
	maps := maplib.New(cfg.MapsFile)
	if err := maps.Load(); err != nil {
		log.Printf("[MAPS] 地图名表加载失败（面板将回退显示 #mapid）: %v", err)
	}
	log.Printf("[MAPS] 地图名表: %s（%d 张图）；客户端坐标 = 像素 / %d",
		cfg.MapsFile, maps.Count(), cfg.GridCell)

	// ---- 地图网格（地图可视化：阻挡位图，按需读取 + 缓存）----
	taskNames := taskname.New(cfg.GameConfigDir) // 任务号 → 任务名（只读游戏配置 task/*.xml）
	grids := maplib.NewGridReader(cfg.GameConfigDir)
	if grids.Available() {
		log.Printf("[MAPS] 地图网格可用: %s", cfg.GameConfigDir)
	} else {
		log.Printf("[MAPS] 警告：地图网格不可用（%s 下缺 map.csv 或 map_file/blockfile）；地图页将无法显示底图",
			cfg.GameConfigDir)
	}

	// ---- 账号池（导入自 Python 中控账号库的 JSON；运行期读写）----
	accts := accounts.New(cfg.AccountsFile)
	if err := accts.Load(); err != nil {
		log.Printf("[ACCOUNTS] 账号池加载失败: %v", err)
	}
	if accts.Count() == 0 {
		log.Printf("[ACCOUNTS] 账号池为空：先运行 tools/import_accounts.py 导入（文件 %s）", cfg.AccountsFile)
	} else {
		log.Printf("[ACCOUNTS] 账号池: %s（%d 个账号）", cfg.AccountsFile, accts.Count())
	}
	for _, z := range zreg.Flat() {
		log.Printf("[ZONE] %-18s %s  %s  编码 %-6s%s", z.Key, z.Addr(), z.DisplayName(),
			z.Coding, map[bool]string{true: "  ← 当前区", false: ""}[z.Key == currentKey(zreg)])
	}

	// ---- 控制通道（单条；事件打上"当前区"标记）----
	ctrlSrv := ctrl.New(cfg.CtrlHost, cfg.CtrlPort, log.Printf)
	if cur, ok := zreg.Current(); ok {
		ctrlSrv.SetZone(cur.Key)
	}
	if err := ctrlSrv.Start(); err != nil {
		log.Printf("[CTRL] 警告：控制通道启动失败（面板仍可用，机器人无法接入）: %v", err)
	}
	defer ctrlSrv.Close()

	// ---- 事件处理 + WS 广播 ----
	proc := process.New(cfg.RobotExe, cfg.DeployDir, cfg.RobotImageName, log.Printf)
	// 2026-09-22 切区即切服：把"当前区"的游戏服地址注入机器人进程环境
	// （机器人 config.py 读 ROBOT_ZONE_IP / ROBOT_ZONE_PORT，缺省回落生产服）。
	// 效果：面板切到某个区 → 重启机器人 → 所有号登录到那个区（如内网测试服）。
	proc.SetExtraEnv(func() []string {
		if z, ok := zreg.Current(); ok && z.Host != "" && z.Port > 0 {
			return []string{"ROBOT_ZONE_IP=" + z.Host, "ROBOT_ZONE_PORT=" + fmt.Sprintf("%d", z.Port)}
		}
		return nil
	})
	ev := event.New(cfg, st, runStore, ctrlSrv, log)
	wsHub := api.NewWSHub(log.Printf)
	ev.SetBroadcast(wsHub.Broadcast)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ev.RunEventLoop(ctx)
	go ev.RunStatusPoller(ctx)

	// 恢复引擎的"派发前最后一道闸"：抓鬼等级门槛（api 层实现，晚绑定 —— restorer 先构造）
	var skipHook func(kind, account string) (bool, string)

	// ---- 链载荷（抓鬼必须带导航数据下发；两处共用一份缓存）----
	payloads := api.NewPayloads(cfg)

	// ---- 恢复引擎（P1：按意图补发命令；默认关，见 cfg.AutoRestore）----
	restore := restorer.New(restorer.Deps{
		Enabled:   func() bool { return cfg.AutoRestore },
		ChannelUp: func() bool { return ctrlSrv.Connected() },
		Intents: func() []intent.Intent {
			if ev.Intents == nil {
				return nil
			}
			return ev.Intents.Snapshot()
		},
		Robot:     func(a string) (state.Robot, bool) { return st.Get(a) },
		ErrRepeat: func(a string) int { r, _ := st.Get(a); return r.ErrRepeat },
		Send:      ev.SendCmd,
		Payload:   payloads.For, // 抓鬼补发也要带导航数据（否则掉线恢复后仍原地不动）
		Skip: func(kind, account string) (bool, string) {
			if skipHook == nil {
				return false, ""
			}
			return skipHook(kind, account)
		},
		GhostDailyLimit: func() int { return payloads.GhostDailyLimit() },
		// 分享日常家族（shenbu / fenghuo）：补发参数按意图 kind 取（share_key + daily_limit）。
		ShareDaily: payloads.ShareDailyParams,
		Log:        log.Printf,
	})
	go restore.Run(ctx)

	// ---- 机器人进程（默认不动：需要 --auto-robot 显式开启）----
	if cfg.AutoStartRobot {
		if err := proc.EnsureRunning(); err != nil {
			log.Printf("[PROC] 启动机器人失败: %v", err)
		}
	} else {
		log.Printf("[PROC] 未开启 --auto-robot：不自动拉起机器人，等待外部 robot_single_robot.exe 连接 %s", cfg.CtrlAddr())
	}

	// ---- HTTP API ----
	webAPI := api.New(api.Deps{
		Cfg: cfg, St: st, Store: runStore, Ctrl: ctrlSrv, Log: log,
		Proc: proc, Zones: zreg, Maps: maps, Grids: grids, TaskNames: taskNames, Accounts: accts,
		Verify: accountverify.NewManager(),
		Events: ev, WS: wsHub,
		Restorer: restore,
		Payloads: payloads,
	})
	// ---- 定时自动任务 + 卡死自动重登恢复（依赖 api 的候选/上线/注册/下发实现）----
	webAPI.AutoTask = autotask.New(webAPI.AutoTaskDeps())
	webAPI.Reghost = reghost.New(webAPI.ReghostDeps())
	ev.SetReghoster(webAPI.Reghost.Request)
	// 2026-09-29 A+C：机器人重启握手（hello）清忙态后自动补一次批量上线（开关 CTRL_RESTART_AUTO_ADD，默认开）
	ev.SetRestartAutoAdd(webAPI.OnRobotRestartHello)
	skipHook = webAPI.GhostSkipFunc()
	go webAPI.AutoTask.Run(ctx)
	go webAPI.Reghost.Run(ctx)
	webAPI.LoadAutoTask() // 恢复上次的定时任务参数（保持数/间隔/启用状态；2026-09-21 落盘）

	// ---- 服务端在线数直连数据源（参考 game_admin_web origin/hqm 的 /gm/online；默认关）----
	// 中控直接 HTTP 拉游戏服在线数（不需要 GM 授权），读到后 /api/status.svr_online 与在线水位
	// 保持器优先用它（source=svr_provider）；失败/过期自动回落 @online 回执 → 本地握手数。
	webAPI.LiveCount = livecount.New(livecount.Config{
		Enabled:     cfg.LiveCountEnabled,
		BaseURL:     cfg.LiveCountURL,
		ServerID:    cfg.LiveCountServerID,
		IntervalSec: cfg.LiveCountIntervalSec,
		TimeoutSec:  cfg.LiveCountTimeoutSec,
		Token:       cfg.LiveCountToken,
	}, func(format string, args ...any) { log.Printf(format, args...) })
	if webAPI.LiveCount.Ready() {
		go webAPI.LiveCount.Start(ctx)
		log.Printf("[LIVECOUNT] 服务端在线数直连数据源已启动：%s（间隔=%ds，无需 GM 授权）",
			webAPI.LiveCount.Config().Endpoint(), webAPI.LiveCount.Config().IntervalSec)
	} else {
		log.Printf("[LIVECOUNT] 服务端在线数直连数据源未启用（默认关闭；用 CTRL_LIVECOUNT_ENABLED=1 + CTRL_LIVECOUNT_URL=... 打开）")
	}

	// ---- 在线水位保持器（把当前区在线人数维持在目标附近；默认关，参数落盘 data/waterline.json）----
	webAPI.Waterline = waterline.New(filepath.Join(cfg.DataDir, "waterline.json"), webAPI.WaterlineDeps())
	webAPI.LoadWaterline()
	go webAPI.Waterline.Start(ctx)
	log.Printf("[WATERLINE] 水位保持器已装配（默认 enabled=false；参数文件 %s）", webAPI.Waterline.Path())

	// ---- 游荡池 keeper（任务池缺人 → **立刻回收**游荡号；余量 → 按图**均匀**派游荡；默认关）----
	// 口径（2026-09-22 用户拍板）：在线总数 200 = 抓鬼池 100 + 新手池 0 + 游荡池（余量，目标 100 左右）。
	// 下发/停止与面板 /api/random_walk 共用同一实现；缺口直接读 autotask 的 States()（不走 HTTP）。
	webAPI.Roampool = roampool.New(filepath.Join(cfg.DataDir, "roampool.json"), webAPI.RoampoolDeps())
	webAPI.LoadRoampool()
	go webAPI.Roampool.Start(ctx)
	log.Printf("[ROAMPOOL] 游荡池 keeper 已装配（默认 enabled=false；参数文件 %s）", webAPI.Roampool.Path())

	srv := &http.Server{Handler: webAPI.Handler(), ReadHeaderTimeout: 10 * time.Second}
	// 显式 Listen：端口被占用时立刻失败退出（而不是留下一个什么都不做的僵尸进程）
	ln, err := net.Listen("tcp", cfg.WebAddr())
	if err != nil {
		log.Printf("[FATAL] HTTP API 端口无法监听 %s: %v（用 --web-port 换端口）", cfg.WebAddr(), err)
		os.Exit(1)
	}
	go func() {
		log.Printf("[HTTP] API 服务已启动: http://%s/", cfg.WebAddr())
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[HTTP] API 服务异常退出: %v", err)
		}
	}()

	// ---- 等待退出信号 ----
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	log.Printf("[MAIN] 收到退出信号，正在关闭…")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
	log.Printf("[MAIN] 已退出")
}

func currentKey(zreg *zones.Registry) string {
	if cur, ok := zreg.Current(); ok {
		return cur.Key
	}
	return ""
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
