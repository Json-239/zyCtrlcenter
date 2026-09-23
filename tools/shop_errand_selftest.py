# -*- coding: utf-8 -*-
"""商店采购重写自检（2026-09-23）。

覆盖：
  ① shop_errand 数据表（四样商品/价格/NPC/13031 已剔除）
  ② 互斥锁（across-owner 拒绝 / 错 owner 释放无效 / TTL 过期接管）
  ③ 库存水位 compute_need / bag_count
  ④ status/consume 状态机（无锁 / done / failed）
  ⑤ 各接线点静态核对（daily_ghost 买药改走 shop_errand、quest_engine 锁与通知、
     config 换 NPC/批量/开关、client.py 命令分发、auto_summon 采购、Go API）。
用法：python tools/shop_errand_selftest.py
"""
import os
import re
import sys

try:
	sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
	pass

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.normpath(os.path.join(HERE, ".."))
SCRIPT = os.path.normpath(os.path.join(ROOT, "deploy", "zones", "prod-240-2300", "script"))
sys.path.insert(0, SCRIPT)
# 同上(冷启动自检): 打包内置的 C 扩展 cnetwork 在纯 Python 下用 MagicMock 顶替
from unittest.mock import MagicMock  # noqa: E402
sys.modules.setdefault("cnetwork", MagicMock(name="cnetwork"))

fails = 0
total = 0


def check(name, ok, detail=""):
	global fails, total
	total += 1
	fails += 0 if ok else 1
	print("[%s] %s%s" % ("PASS" if ok else "FAIL", name, (" — " + detail) if detail else ""))


def rd(rel):
	return open(os.path.join(ROOT, rel), encoding="utf-8").read()


# ---------------------------------------------------------------- ① 数据表
import shop_errand as se  # noqa: E402

check("四样商品在采购表", all(i in se.SHOP_ITEMS for i in (102007, 102010, 101008, 101009)),
	repr(sorted(se.SHOP_ITEMS)))
check("药品 → 刘三娘 13011", se.SHOP_ITEMS[102007]["npc"] == 13011 and se.SHOP_ITEMS[102010]["npc"] == 13011)
check("粮食 → 伍毅 13021", se.SHOP_ITEMS[101008]["npc"] == 13021 and se.SHOP_ITEMS[101009]["npc"] == 13021)
check("储备金单价 211/228/80/100", (
	se.SHOP_ITEMS[102007]["price"], se.SHOP_ITEMS[102010]["price"],
	se.SHOP_ITEMS[101008]["price"], se.SHOP_ITEMS[101009]["price"]) == (211, 228, 80, 100))
check("13031(无衣, 不卖货) 不在 NPC 表", 13031 not in se.SHOP_NPCS)
check("NPC 表含真杂货商 13021 + 兜底 13008/13220", all(n in se.SHOP_NPCS for n in (13021, 13008, 13220)))
check("start: 手动采购对抓鬼号有闸(errand/food 拒绝)", '手动采购仅限空闲号' in open(
	os.path.join(SCRIPT, "shop_errand.py"), encoding="utf-8").read())
check("start: 采购前停空闲游荡", "m_collect_walk" in open(
	os.path.join(SCRIPT, "shop_errand.py"), encoding="utf-8").read())
_srt = open(os.path.join(SCRIPT, "shop_errand.py"), encoding="utf-8").read()
check("start: 战斗中硬闸(不启动采购)", "战斗中, 不启动商店采购" in _srt)
check("shop_errand: env_snapshot 现场快照 + 储备金货架白名单",
	"def env_snapshot" in _srt and "RESERVE_SHELVES" in _srt)
check("单次上限 SHOP_MAX_COUNT=500", se.SHOP_MAX_COUNT == 500)
check("失败通知码含 552/63/1248/1255/1278", all(n in se.SHOP_FAIL_NOTICES for n in (552, 63, 1248, 1255, 1278)))


# ---------------------------------------------------------------- ② 互斥锁
class FakeRobot(object):
	def __init__(self, acc="robot_test@xy3.com"):
		self.m_account = [acc]
		self.m_bag_cache = {}
		self.m_quest = None


r = FakeRobot()
ok, _ = se.acquire(r, "errand")
check("acquire errand 成功", ok)
ok2, held = se.acquire(r, "token")
check("跨 owner 抢占被拒(返回占用者)", (not ok2) and held == "errand")
se.release(r, "token")
check("错 owner 释放无效", se.holder(r) == "errand")
se.release(r, "errand")
check("正确 owner 释放成功", se.holder(r) is None)
ok3, _ = se.acquire(r, "drug")
check("释放后可被新 owner 抢占", ok3 and se.holder(r) == "drug")
# TTL: 手动把 since 拨老, 应允许接管
se._locks[r.m_account[0]]["since"] -= (se.SHOP_LOCK_TTL_MS + 1000)
ok4, held4 = se.acquire(r, "food")
check("TTL 过期后可接管", ok4 and se.holder(r) == "food")
se.release(r, "food")
# TTL 过期 status 报 failed 并清锁
se.acquire(r, "errand")
se._locks[r.m_account[0]]["since"] -= (se.SHOP_LOCK_TTL_MS + 1000)
st = se.status(r)
check("TTL 过期 status→failed(锁已清)", st["result"] == "failed" and se.holder(r) is None)


# ---------------------------------------------------------------- ③ 水位
r2 = FakeRobot("robot_b@xy3.com")
r2.m_bag_cache = {102007: [1, 50, 8192]}
check("compute_need 缺口=150", se.compute_need(r2, 102007, 200) == 150)
check("bag_count 读到 50", se.bag_count(r2, 102007) == 50)
r2.m_bag_cache = {102007: [1, 300, 8192]}
check("超水位 need=0", se.compute_need(r2, 102007, 200) == 0)
r2.m_bag_cache = {}
check("空背包 need=target", se.compute_need(r2, 101008, 300) == 300)


# ---------------------------------------------------------------- ④ status/consume
r3 = FakeRobot("robot_c@xy3.com")
st = se.status(r3)
check("无会话 status 安静返回", (not st["active"]) and st["result"] is None)
ok, _ = se.acquire(r3, "errand")
se.notify_done(r3, 102007)
st = se.status(r3)
check("notify_done → result=done/active=False", st["result"] == "done" and not st["active"])
st2 = se.consume(r3, "errand")
check("consume 读走结果并释放锁", st2["result"] == "done" and se.holder(r3) is None)
ok, _ = se.acquire(r3, "errand")
se.notify_failed(r3, "测试失败")
st = se.consume(r3, "errand")
check("notify_failed → failed + reason", st["result"] == "failed" and "测试失败" in st["reason"])
ok, _ = se.acquire(r3, "errand")
se.notify_rejected(r3, 552, "货币(储备金/银票)不够")
st = se.status(r3)
check("rejected 记原因(不算 done)", st["active"] and "552" in st["reason"])
se.release(r3)


# ---------------------------------------------------------------- ④.5 执行器互斥放行（集成）
class FakeQuest(object):
	active = True
	shop_ctx = None
	state = None


r4 = FakeRobot("robot_d@xy3.com")
r4.m_quest = FakeQuest()
import quest_engine as _qe_rt  # noqa: E402

se.acquire(r4, "drug")
_res = _qe_rt.dispatch_cmd(r4, {"cmd": "shop_errand", "npc": 13011,
	"item_index": 102007, "count": 200, "shop_lock_ok": "drug"})
check("编排层带 shop_lock_ok: 不自锁(走到 quest.active 分支)",
	_res.get("result") == "busy" and "商店会话被" not in (_res.get("msg") or ""), repr(_res))
se.release(r4, "drug")
se.acquire(r4, "token")
_res2 = _qe_rt.dispatch_cmd(r4, {"cmd": "shop_errand", "npc": 13011,
	"item_index": 102007, "count": 200})
check("裸调用遇 token 占用: 明确 busy(带占用者)",
	_res2.get("result") == "busy" and "token" in (_res2.get("msg") or ""), repr(_res2))
se.release(r4, "token")


# ---------------------------------------------------------------- ⑤ 接线点静态核对
dq = rd("deploy/zones/prod-240-2300/script/daily_ghost.py")
check("daily_ghost: 不再引用 g.drug_shop_ctx(仅注释可提)",
	not re.search(r"^\s*g\.drug_shop_ctx", dq, re.M))
check("daily_ghost: 旧 DRUG_NPC_ID 常量已删", not re.search(r"^DRUG_NPC_ID\s*=", dq, re.M))
check("daily_ghost: __goto_drug_shop 已删", "def __goto_drug_shop" not in dq)
check("daily_ghost: 有 __heal_buy_tick/__heal_abort_shop", "__heal_buy_tick" in dq and "__heal_abort_shop" in dq)
check("daily_ghost: 买药走 shop_errand.start(owner=drug)",
	'shop_errand.start(robot_object, _want, count=_cnt, owner="drug")' in dq)
check("daily_ghost: 助战令抢锁 acquire('token')", '_se_lock.acquire(robot_object, "token")' in dq)
check("daily_ghost: 助战令释放 __release_token_lock", "__release_token_lock" in dq)
check("daily_ghost: DRUG_BUY_COUNT=200 批量", "DRUG_BUY_COUNT = 200" in dq)
check("daily_ghost: buy 进行中代推进导航(抓鬼模式 tester 被跳过)",
	"__consume_nav(robot_object, g, quest, now_ms)" in dq
	and re.search(r"def __heal_buy_tick\(robot_object, g, quest, now_ms\)", dq) is not None)
check("daily_ghost: 采购锁 owner 为 drug/food(与 token 隔离)",
	'owner="drug"' in dq and 'owner="food"' in rd("deploy/zones/prod-240-2300/script/auto_summon.py"))

qe = rd("deploy/zones/prod-240-2300/script/quest_engine.py")
check("quest_engine: __cmd_shop_errand 有会话锁检查", "shop_errand" in qe and "holder(robot_object)" in qe)
check("quest_engine: 采购完成回调 notify_done", "_se.notify_done" in qe)
check("quest_engine: 采购失败回调 notify_failed", "_se.notify_failed" in qe)
check("quest_engine: 识别 1110 扣储备金通知", "if _nid == 1110:" in qe)
check("quest_engine: 购买静默无应答检测(>25s 现场快照)", "商店购买静默无应答" in qe)
check("quest_engine: 记录购买会话货架/环境/发送时刻",
	'_ctx["sale_index"] = data[5]' in qe and '_ctx["buy_sent_ts"] = __now_ms()' in qe)
check("quest_engine: 点 NPC 无响应日志带环境快照", "静默拒绝点击" in qe)
check("quest_engine: begin_chase 记追捕时刻", "m_last_chase_ms" in qe)

cfg = rd("deploy/zones/prod-240-2300/script/config.py")
check("config: robot_food_shop_npc=13021(真杂货商)", "robot_food_shop_npc = 13021" in cfg)
check("config: 无 13031 生效配置(注释说明除外)",
	not re.search(r"robot_food_shop_npc\s*=\s*13031", cfg) and "13031:" not in cfg)
check("config: 自动采购开 + 批量 200 + 目标水位 300",
	"robot_auto_buy_food = True" in cfg and "robot_food_buy_count = 200" in cfg
	and "robot_food_target_count = 300" in cfg)
check("config: robot_shop_npc_positions 含 13021/13011, 不含 13031",
	"13021:" in cfg and "13011:" in cfg and "13031:" not in cfg)

cl = rd("deploy/zones/prod-240-2300/script/client.py")
check("client: 新增 shop_errand 命令分支", 'cmd_name == "shop_errand"' in cl)
_m = re.search(r'if cmd_name == "shop_errand":(.{0,1400})', cl, re.S)
check("client: shop_errand 分支有 accounts 过滤 + shop_errand.start",
	bool(_m) and "accounts" in _m.group(1) and "shop_errand.start" in _m.group(1))

au = rd("deploy/zones/prod-240-2300/script/auto_summon.py")
check("auto_summon: 采购走 shop_errand.start(owner=food)",
	'shop_errand.start(robot_object, _item, count=_cnt, owner="food"' in au)
check("auto_summon: 采购冷却 5 分钟", "_BUY_FOOD_COOLDOWN_MS = 300000" in au)

go = rd("internal/api/shop.go")
check("Go: /api/shop_errand handler 存在", "handleShopErrand" in go and "shop_errand" in go)
check("Go: 白名单含四样商品", all(str(i) in go for i in (102007, 102010, 101008, 101009)))
check("Go: 路由已注册", "POST /api/shop_errand" in rd("internal/api/api.go"))

# 双副本(若存在)一致性
PROD_COPY = "F:/ZyBin/xm/2d-xiyou-server/robot/deploy/single_robot_zy/script"
if os.path.isdir(PROD_COPY):
	_diff = []
	for f in ("shop_errand.py", "daily_ghost.py", "quest_engine.py", "config.py",
			"client.py", "auto_summon.py"):
		a = os.path.join(SCRIPT, f)
		b = os.path.join(PROD_COPY, f)
		if not os.path.exists(b) or open(a, "rb").read() != open(b, "rb").read():
			_diff.append(f)
	check("双副本一致(核心 6 文件)", not _diff, "不一致: %s" % _diff)
else:
	check("双副本检查跳过(生产副本不存在)", True)

print("\n结果：%d 项，失败 %d 项" % (total, fails))
sys.exit(1 if fails else 0)
