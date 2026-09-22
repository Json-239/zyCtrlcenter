// 「服务端在线数」直连数据源：api 侧的壳。
//
// 引擎/HTTP 客户端在 internal/services/livecount（参考 game_admin_web origin/hqm 的
// GMApi::onlineCount → GET /gm/online）。这里只负责统一取数与诊断快照：
//
//   - svrOnlineReading：读数的**唯一入口**（按新鲜度选择来源）：
//     1) provider（中控直连 /gm/online，svr_provider）——启用且读到数、且不超过 StaleSec；
//     2) 机器人 `@online` 回执（state.SvrOnlineLatest，svr）——现状兜底，生产未授权时为空；
//     3) 都没有 → ok=false（source=local，调用方用本地握手数兜底）。
//   - liveCountSnapshot：/api/status.livecount 诊断（enabled/url/最近读数/最近错误）。
//
// 消费方（都不需要感知数据来自哪一路）：
//   - /api/status.svr_online（handlers.go，面板大屏）；
//   - 在线水位保持器（waterline.go 壳，Deps.SvrOnline）。
package api

import (
	"net/http"
	"strings"
	"time"

	"zyctrlcenter/internal/services/livecount"
)

// svrOnlineReading 统一取"全服在线（含真实玩家）"读数：provider → @online 回执 → 无。
// 返回 count、时间戳(ms)、来源（svr_provider / svr / local）、是否有读数。
func (a *API) svrOnlineReading() (count int, tsMS float64, source string, ok bool) {
	if a.LiveCount != nil {
		if c, ts, has := a.LiveCount.Latest(); has {
			age := int(time.Now().UnixMilli()/1000 - int64(ts)/1000)
			if age < 0 {
				age = 0
			}
			if age <= a.LiveCount.Config().StaleSec() {
				return c, ts, livecount.SourceProvider, true
			}
			a.Log.Printf("[LIVECOUNT] 读数过期（%d 秒前），本轮回落机器人 @online 回执", age)
		}
	}
	if c, ts, has := a.St.SvrOnlineLatest(); has {
		return c, ts, livecount.SourceSvr, true
	}
	return 0, 0, livecount.SourceLocal, false
}

// liveCountSnapshot 数据源诊断快照（未装配时返回空对象）。
func (a *API) liveCountSnapshot() map[string]any {
	if a.LiveCount == nil {
		return map[string]any{}
	}
	return a.LiveCount.Snapshot()
}

// handleLiveCountSet 运行时改"服务端在线数直连"配置（2026-09-22：映射地址/间隔改了
// 不必重启中控）。body: {enabled?, url?, server_id?, interval_sec?, timeout_sec?}
//
// 典型用法（映射地址终于定下来时）：
//
//	curl -X POST :28082/api/livecount -d '{"enabled":true,"url":"http://47.96.8.240:8080"}'
//	或是走 nginx：{"url":"http://47.96.8.240"}（路径自动拼 /gm/online）
func (a *API) handleLiveCountSet(w http.ResponseWriter, r *http.Request) {
	if a.LiveCount == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "中控未装配 livecount 数据源"})
		return
	}
	body := readBody(r)
	cfg := a.LiveCount.Config()
	if v, has := body["enabled"]; has {
		cfg.Enabled = toBool(v, cfg.Enabled)
	}
	if v, has := body["url"]; has {
		cfg.BaseURL = strings.TrimSpace(toStr(v))
	}
	if v, has := body["server_id"]; has {
		cfg.ServerID = strings.TrimSpace(toStr(v))
	}
	if v, has := body["interval_sec"]; has {
		cfg.IntervalSec = toInt(v, cfg.IntervalSec)
	}
	if v, has := body["timeout_sec"]; has {
		cfg.TimeoutSec = toInt(v, cfg.TimeoutSec)
	}
	cfg = cfg.Normalize()
	a.LiveCount.SetConfig(cfg)
	a.Log.Printf("[LIVECOUNT] 运行时改配置: enabled=%v url=%s serverId=%s 间隔=%ds（下一轮生效）",
		cfg.Enabled, cfg.Endpoint(), cfg.ServerID, cfg.IntervalSec)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "msg": "已更新（下一轮取数立即生效，无需重启）",
		"livecount": a.LiveCount.Snapshot(),
	})
}

// handleLiveCountGet 直连数据源诊断（与 /api/status.livecount 同源，便于单独查询）。
func (a *API) handleLiveCountGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "livecount": a.liveCountSnapshot()})
}
