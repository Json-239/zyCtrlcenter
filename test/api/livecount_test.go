// 「服务端在线数」数据源在 api 层的接线用例：
//   - /api/status.svr_online 的来源优先级：直连 provider（svr_provider）→ 机器人 @online
//     回执（svr）→ 无读数（local）；
//   - /api/status.livecount 诊断摘要（默认关闭时也可看到配置口径）；
//   - 水位保持器取数（WaterlineDeps 的 SvrOnline）与 /api/status 同口径。
//
// 全部用 httptest 假游戏服（/gm/online），不碰真实游戏服。
package api_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zyctrlcenter/internal/services/livecount"
	"zyctrlcenter/internal/state"
)

func TestSvrOnlineSourcePriority(t *testing.T) {
	env := newTestEnv(t, "")

	// ① 两路都没有 → source=local、不带 count（语义：没有服务端读数，用本地握手数兜底）
	st := getJSON(t, env.srv.URL+"/api/status")
	so, ok := st["svr_online"].(map[string]any)
	if !ok {
		t.Fatalf("svr_online 应为对象，实际 %T", st["svr_online"])
	}
	if so["source"] != "local" {
		t.Fatalf("无读数时 source 应为 local，实际 %v", so["source"])
	}
	if _, has := so["count"]; has {
		t.Fatalf("无读数时不应带 count：%v", so)
	}

	// ② 只有机器人 @online 回执（state.SvrOnline 上报）→ source=svr
	env.st.Update("robot0001000@xy3.com", func(r *state.Robot) {
		r.SvrOnline = map[string]any{"count": 55, "ts": float64(time.Now().UnixMilli())}
	})
	st = getJSON(t, env.srv.URL+"/api/status")
	so = st["svr_online"].(map[string]any)
	if so["source"] != "svr" || so["count"] != 55.0 {
		t.Fatalf("@online 回执应生效（source=svr count=55），实际 %v", so)
	}

	// ③ provider 直连拉取成功 → source=svr_provider 且优先于 @online
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gm/online" {
			t.Errorf("请求路径应为 /gm/online，实际 %s", r.URL.Path)
		}
		_, _ = fmt.Fprint(w, `{"online_count":88,"success":true,"serverId":1000}`)
	}))
	defer fs.Close()
	env.api.LiveCount = livecount.New(livecount.Config{Enabled: true, BaseURL: fs.URL, ServerID: "1000"}, nil)
	if err := env.api.LiveCount.Fetch(context.Background()); err != nil {
		t.Fatalf("provider 拉取应成功: %v", err)
	}
	st = getJSON(t, env.srv.URL+"/api/status")
	so = st["svr_online"].(map[string]any)
	if so["source"] != "svr_provider" || so["count"] != 88.0 {
		t.Fatalf("provider 读数应优先（source=svr_provider count=88），实际 %v", so)
	}
	// 诊断摘要：/api/status.livecount 能看到 url/ok/count
	lc, ok := st["livecount"].(map[string]any)
	if !ok {
		t.Fatalf("livecount 摘要应为对象，实际 %T", st["livecount"])
	}
	if lc["enabled"] != true || lc["ok"] != true || lc["count"] != 88.0 {
		t.Fatalf("livecount 摘要不符: %v", lc)
	}

	// ④ provider 拉取失败（端点不可达）→ 回落 @online 回执（source=svr）
	env.api.LiveCount = livecount.New(livecount.Config{Enabled: true, BaseURL: "http://127.0.0.1:1", TimeoutSec: 1}, nil)
	if err := env.api.LiveCount.Fetch(context.Background()); err == nil {
		t.Fatal("不可达端点应拉取失败")
	}
	st = getJSON(t, env.srv.URL+"/api/status")
	so = st["svr_online"].(map[string]any)
	if so["source"] != "svr" || so["count"] != 55.0 {
		t.Fatalf("provider 失败应回落 @online（source=svr count=55），实际 %v", so)
	}

	// ⑤ 水位保持器的取数与 /api/status 同口径：provider 成功后 WaterlineDeps 也拿 provider 的值
	env.api.LiveCount = livecount.New(livecount.Config{Enabled: true, BaseURL: fs.URL, ServerID: "1000"}, nil)
	if err := env.api.LiveCount.Fetch(context.Background()); err != nil {
		t.Fatalf("provider 拉取应成功: %v", err)
	}
	deps := env.api.WaterlineDeps()
	got, ts, ok := deps.SvrOnline()
	if !ok || got != 88 || ts <= 0 {
		t.Fatalf("水位保持器应取到 provider 读数 88，实际 count=%d ts=%v ok=%v", got, ts, ok)
	}
}

func TestLiveCountSnapshotWithoutProvider(t *testing.T) {
	// 未装配 provider（默认关闭）时：/api/status.livecount 为空对象，链路其余部分照常
	env := newTestEnv(t, "")
	st := getJSON(t, env.srv.URL+"/api/status")
	if lc, ok := st["livecount"].(map[string]any); !ok || len(lc) != 0 {
		t.Fatalf("未装配时 livecount 应为空对象，实际 %v", st["livecount"])
	}
	// 水位保持器未装配时 /api/status.waterline 也保持空对象（现状不回归）
	if wl, ok := st["waterline"].(map[string]any); !ok || len(wl) != 0 {
		t.Fatalf("未装配时 waterline 应为空对象，实际 %v", st["waterline"])
	}
}
