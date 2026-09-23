// 2026-09-23 P0：恢复引擎**单轮上限**（DefaultMaxPerRound=5）。
//
// 生产事故：批量上线瞬间"几百个号都没在跑"，一轮 Tick 把所有满足条件的号都补发
// （实测单轮补过 87 个），叠加直派把抓鬼会话推到 200+（目标 100）。
// 口径：每轮最多补 N 个；未补的号**状态不被改动**（不扣 Attempts、不设冷却），
// 下一轮仍在候选里。
package recover_test

import (
	"fmt"
	"testing"
	"time"

	"zyctrlcenter/internal/services/intent"
	"zyctrlcenter/internal/services/restorer"
)

func TestTickMaxPerRoundLimitsBatch(t *testing.T) {
	f := newFake()
	for i := 0; i < 10; i++ {
		acc := fmt.Sprintf("g%02d@x.com", i)
		f.items = append(f.items, intent.Intent{Account: acc, Kind: intent.KindGhost})
		f.robots[acc] = online(acc, "IDLE", 0)
	}
	r := restorer.New(f.deps()) // 默认 MaxPerRound = DefaultMaxPerRound(5)

	acts := r.Tick(f.now)
	if len(acts) != restorer.DefaultMaxPerRound {
		t.Fatalf("单轮应限 %d 个，实得 %d: %+v", restorer.DefaultMaxPerRound, len(acts), acts)
	}

	// 被补的 5 个"跑起来"（NAV）→ 不再需要恢复；剩下的 5 个下一轮才补
	for i := 0; i < restorer.DefaultMaxPerRound; i++ {
		acc := fmt.Sprintf("g%02d@x.com", i)
		rr := f.robots[acc]
		rr.State = "NAV"
		f.robots[acc] = rr
	}
	f.now = f.now.Add(10 * time.Second)
	acts2 := r.Tick(f.now)
	if len(acts2) != 5 {
		t.Fatalf("第二轮应补剩下的 5 个，实得 %d: %+v", len(acts2), acts2)
	}
	if acts2[0].Account != "g05@x.com" {
		t.Fatalf("第二轮应从 g05 开始（未补的号状态未被改动），实得 %s", acts2[0].Account)
	}
}

// MaxPerRound 可覆盖（0 → 默认值）。
func TestTickMaxPerRoundOverride(t *testing.T) {
	f := newFake()
	for i := 0; i < 4; i++ {
		acc := fmt.Sprintf("h%d@x.com", i)
		f.items = append(f.items, intent.Intent{Account: acc, Kind: intent.KindGhost})
		f.robots[acc] = online(acc, "IDLE", 0)
	}
	d := f.deps()
	d.MaxPerRound = 1
	r := restorer.New(d)
	if acts := r.Tick(f.now); len(acts) != 1 {
		t.Fatalf("MaxPerRound=1 → 单轮只补 1 个，实得 %d", len(acts))
	}
}
