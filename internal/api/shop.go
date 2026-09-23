package api

import "net/http"

// 采购白名单：item_index -> 缺省 NPC（与机器人端 shop_errand.SHOP_ITEMS 保持一致）。
// 货币全部为**储备金**（服务端货架 sale_index_school_contribution /
// sale_index_grocery[_adv]_contribution），NPC 位置：
//
//	13011 刘三娘 图616 回春药铺 / 图11 长安东市集（药品）
//	13021 伍毅   图612 琳琅阁   / 图11 长安东市集（杂货：幻兽丹/口粮等）
//
// 见 docs/04-测试/分析-20260923-商店购买调研.md。
var shopItemNPC = map[int]int{
	102007: 13011, // 金创药  +800HP  211 储备金
	102010: 13011, // 昙花霜  +800MP  228 储备金
	101008: 13021, // 幻兽丹  (守护欢乐度 +10) 80 储备金
	101009: 13021, // 宠物口粮 100 储备金
	101334: 13021, // 坐骑饲料 500 储备金
	101395: 13021, // 高级坐骑饲料 3500 储备金
}

// shopErrandMaxCount 单次下单上限（与机器人端 shop_errand.SHOP_MAX_COUNT 一致；
// 服务端物品叠加 999、携带上限默认无限，取 500 保守值，防"背包满"整批被拒）。
const shopErrandMaxCount = 500

// handleShopErrand 手动/冒烟采购：POST /api/shop_errand
//
//	{accounts?: ["robot0001000@xy3.com"], item_index: 101008, npc?: 13021, count?: 200}
//
// 下发 cmd=shop_errand 到机器人端（shop_errand.start → quest_engine 商店执行器：
// 导航 → 点 NPC → 选"#iBM#储备金购买…" → S2C_SALE_GOODS → C2S_ROLE_BUY(80108)；
// 以通知 1110 扣储备金 / 入包 90398 / 1276 得到物品为成功判据）。
// 省略 accounts = 广播给全部在线号（危险，调用方自负）。
func (a *API) handleShopErrand(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	item := toInt(body["item_index"], 0)
	if _, ok := shopItemNPC[item]; !ok {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false,
			"msg": "item_index 必须在采购白名单内：102007 金创药 / 102010 昙花霜 / " +
				"101008 幻兽丹 / 101009 宠物口粮 / 101334 坐骑饲料 / 101395 高级坐骑饲料"})
		return
	}
	npc := toInt(body["npc"], 0)
	if npc == 0 {
		npc = shopItemNPC[item]
	}
	count := toInt(body["count"], 200)
	if count <= 0 {
		count = 200
	}
	if count > shopErrandMaxCount {
		count = shopErrandMaxCount
	}
	accounts := bodyAccounts(body)
	cmd := map[string]any{"cmd": "shop_errand", "npc": npc,
		"item_index": item, "count": count}
	if len(accounts) > 0 {
		cmd["accounts"] = accounts
	}
	ok := a.Events != nil && a.Events.SendCmd(cmd, "shop_errand")
	a.Store.LogEvent(map[string]any{"type": "api", "action": "shop_errand",
		"zone": a.currentZoneKey(), "accounts": accounts,
		"item_index": item, "npc": npc, "count": count, "sent": ok})
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "item_index": item,
		"npc": npc, "count": count, "accounts": accounts,
		"msg": okMsg(ok, "已下发采购(储备金)")})
}
