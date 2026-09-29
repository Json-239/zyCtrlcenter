// 2026-09-29 线上复核补（Go 侧 ba2b3e5 的 live 缺陷回归）：回收→日常池的**每玩法配额闸**。
//
// 现场（14:32-14:39）：服务侧 dailyDeficit 是"神捕+烽火"聚合缺口 —— 出现过"神捕缺 3、
// 烽火已满"仍触发回收，挑选对象恰好只对烽火就绪 → ReclaimDaily 里 cutByPoolQuota 把整批
// 截断（全被裁）→ 每 30s 报一条 "回收→日常池失败：下发失败（池闸全拦）"，keeper.LastErr
// 被污染、日志空转。修法：shareDailyReclaimReady 前置 `poolQuota(kind,false)==0 → 不就绪`，
// 配额满=该玩法无候选，自然走 noop（LastErr 清空）。
package api_test

import (
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/test/testsupport"
)

func TestRoampoolDailyReclaimRespectsPerKindQuota(t *testing.T) {
	env := newTestEnv(t, "")
	testsupport.InstallShareDailyNav(t, env.cfg.ChainDir)
	testsupport.InstallFenghuoNav(t, env.cfg.ChainDir) // 正向对照要投烽火：需 fenghuo_nav 声明
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	// 神捕缺员（target 5 / 在跑 0）→ ddef>0 会触发 ①b；烽火 target 1 且已被占满 → 无配额。
	if err := env.api.AutoTask.Start(autotask.KindShenbu, autotask.Config{
		Kind: autotask.KindShenbu, IntervalSec: 600, TargetOnline: 5, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动大唐神捕池失败: %v", err)
	}
	if err := env.api.AutoTask.Start(autotask.KindFenghuo, autotask.Config{
		Kind: autotask.KindFenghuo, IntervalSec: 600, TargetOnline: 1, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动烽火大唐池失败: %v", err)
	}
	// 占满烽火的在跑号（心跳 daily 让 autotaskOnlineCount(fenghuo)=1=target → 配额 0）
	feedRobot(t, env, "occ@xy3.com", map[string]any{"daily": map[string]any{
		"share_key": "share_daily_宫廷10", "done": 0, "limit": 20, "state": "RUNNING"}})

	// 主体：抓鬼满额 + 神捕今日已满（只能投烽火）+ 正游荡
	sub := "sub@xy3.com"
	roamingGhostFull(t, env, sub, nil)
	env.st.MarkShareDailyFull(sub, "share_daily_大唐神捕")

	k := newDailyReclaimKeeper(t, env, nil)
	if k.Tick(time.Now()) {
		t.Fatal("烽火无配额时不该回收转投（旧行为会整批截断后反复报失败）")
	}
	if cmd := rb.TryReadCmd(300 * time.Millisecond); cmd != nil {
		t.Fatalf("不该下发任何命令，实际 %v", cmd)
	}
	if st := k.Status(); st.LastErr != "" {
		t.Fatalf("配额满应是 noop 而非失败（LastErr 应为空），实际 %q", st.LastErr)
	}
	if !strings.Contains(k.Status().LastAction, "没有可回收的游荡号") {
		t.Fatalf("应走 noop 说明，实际 %q", k.Status().LastAction)
	}

	// 正向对照：烽火扩容到 target 5（有配额）→ 同一号应被回收转投。
	if err := env.api.AutoTask.Start(autotask.KindFenghuo, autotask.Config{
		Kind: autotask.KindFenghuo, IntervalSec: 600, TargetOnline: 5, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("烽火扩容失败: %v", err)
	}
	if !k.Tick(time.Now()) {
		t.Fatal("配额释放后应回收转投")
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "share_daily_start" || cmd["share_key"] != "share_daily_宫廷10" {
		t.Fatalf("应收 share_daily_start(烽火): %v", cmd)
	}
	accs := asSlice(cmd["accounts"])
	if len(accs) != 1 || accs[0] != sub {
		t.Fatalf("应只派 %s，实际 %v", sub, accs)
	}
	if st := k.Status(); st.LastErr != "" {
		t.Fatalf("成功路径不该有 LastErr，实际 %q", st.LastErr)
	}
}
