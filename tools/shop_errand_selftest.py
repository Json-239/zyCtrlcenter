# -*- coding: utf-8 -*-
"""商店采购重写自检（2026-09-23）。

覆盖：
  ① shop_errand 数据表（四样商品/价格/NPC/13031 已剔除）
  ② 互斥锁（across-owner 拒绝 / 错 owner 释放无效 / TTL 过期接管）
  ③ 库存水位 compute_need / bag_count
  ④ status/consume 状态机（无锁 / done / failed）
  ⑤ 各接线点静态核对（daily_ghost 买药改走 shop_errand、quest_engine 锁与通知、
     config 换 NPC/批量/开关、client.py 命令分发、auto_summon 采购、Go API）
  ⑥ 叠加购买重复扣款修复（2026-09-23）:
     · 90327 数量回执注册/解析/入背包缓存
     · 1276 通知参数"单元素元组"剥壳（旧实现静默解析失败 → 无任何入包证据）
     · 复买前本地核对（数量增加 → 判成功不再买 / 已扣款宽限判成功）
     · 库存≥请求量 → "已满足, 未购买"(skipped) 明确回执
     · force 绕过跳过（shop_errand.start → cmd.shop_force → quest_engine）
  ⑦ 回执签名口径：notify_done(reason/skipped) ↔ quest_engine 调用点一致
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
# 中控通道/诊断日志: 纯 Python 自检不起 socket、不写 diag.log
sys.modules.setdefault("ctrl_client", MagicMock(name="ctrl_client"))
sys.modules.setdefault("diag", MagicMock(name="diag"))

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

# ---------------------------------------------------------------- ⑥ 叠加购买重复扣款修复
_pt = rd("deploy/zones/prod-240-2300/script/protocol3.py")
_mh = rd("deploy/zones/prod-240-2300/script/msghandle.py")
_ro = rd("deploy/zones/prod-240-2300/script/robot_operator.py")

check("协议: FORMAT_MS 注册 90327(叠加数量回执)",
	"FORMAT_MS[S2C_UPDATE_ADD_ITEM_COUNT] = [8, 2]" in _pt)
check("协议: g_handle_map 注册 90327 → item_count_add_handle",
	"S2C_UPDATE_ADD_ITEM_COUNT : msghandle.item_count_add_handle" in _pt)
check("msghandle: 90327 处理函数转到 robot_operator",
	"def item_count_add_handle(fd, data_list)" in _mh
	and "robot_operator.on_update_add_item_count" in _mh)
# 2026-09-23 事故护栏: 严禁运行期补注册协议表(热更后进程崩溃, 230 号掉线)
check("护栏: robot_operator 不做运行期协议注册(禁 set_format_dict)",
	re.search(r"^\s*def\s+install_shop_protocol_hooks", _ro, re.M) is None
	and re.search(r"^\s*(?!\s*#).*cnet\.set_format_dict\s*\(", _ro, re.M) is None)
check("护栏: shop_errand 不做运行期协议注册",
	"install_protocol_hooks" not in _srt)

import robot_operator as _rop  # noqa: E402

check("协议: 冷启动静态注册即可(无需运行期补齐)",
	"S2C_UPDATE_ADD_ITEM_COUNT" in _pt and "90327" not in _ro.split("SHOP_PROTO_HOOKS")[0][-400:])

check("解析 90327: 扁平分包 [id,count]",
	_rop.__parse_item_count_records([1761, 250]) == [(1761, 250)])
check("解析 90327: 元组列表 [(id,count)] / [[(id,count)]]",
	_rop.__parse_item_count_records([(1761, '250')]) == [(1761, 250)]
	and _rop.__parse_item_count_records([[(1761, 250)]]) == [(1761, 250)])


class FakeRobot2(FakeRobot):
	def get_role_name(self):
		return "测试角色"
	def send_message(self, *a, **k):
		return 0


# ① 90327 → 背包计数刷新 + 商店采购判成功
r5 = FakeRobot2("robot_e@xy3.com")
r5.m_bag_cache = {101008: [1761, 200, 8199]}
r5.m_bag_meta = {}
_q5 = FakeQuest()
_q5.bought_tasks = {}
_q5.active_task_index = 0
_q5.shop_ctx = {"item_index": 101008, "count": 200, "bag_before": 200, "errand": True}
_q5.active = True
_q5.state = None
_q5.set_state = lambda s: setattr(_q5, "state", s)
r5.m_quest = _q5
import shop_errand as _se6  # noqa: E402
_se6._locks["robot_e@xy3.com"] = {"owner": "errand", "since": _se6._now_ms(),
	"item": 101008, "count": 200, "result": None, "reason": "", "session": 1}
_rop.on_update_add_item_count(r5, [1761, 400])
check("90327: 背包计数刷到 400", r5.m_bag_cache[101008][1] == 400)
check("90327: 商店采购判成功(shop_ctx 清空 + 通知 done)",
	_q5.shop_ctx is None and _se6.status(r5)["result"] == "done",
	repr(_se6.status(r5)))
_se6.release(r5)

# ② 1276 通知参数是"单元素元组" → 必须能解析(旧实现静默失败)
_pal = getattr(_qe_rt, "__parse_add_item_link")
check("1276 链接: 裸字符串两种形态",
	_pal("[item,17622171000906414;101403;5070322,储物袋]") == 101403
	and _pal("[item_entry,101403,草船]") == 101403)
check("1276 链接: 单元素元组(服务端 format_args_list 形态) —— 生产 0 次成功的根因",
	_pal(("[item,17610600359365304;101008;6030619,幻兽丹]",)) == 101008
	and _pal(("[item_entry,101008,幻兽丹]*1",)) == 101008)

# ③ 复买前本地核对: 数量增加 → 判成功, 不再买
r6 = FakeRobot2("robot_f@xy3.com")
r6.m_bag_cache = {101008: [1761, 400, 8199]}
_q6 = FakeQuest()
_q6.bought_tasks = {}
_q6.active_task_index = 0
_q6.shop_ctx = {"item_index": 101008, "count": 200, "bag_before": 200, "errand": True}
_q6.active = True
_q6.set_state = lambda s: setattr(_q6, "state", s)
r6.m_quest = _q6
_se6._locks["robot_f@xy3.com"] = {"owner": "errand", "since": _se6._now_ms(),
	"item": 101008, "count": 200, "result": None, "reason": "", "session": 1}
_retry = getattr(_qe_rt, "__shop_retry_check")
check("本地核对: 数量 200→400 → 判已买到(True)",
	_retry(r6, _q6) is True and _q6.shop_ctx is None and _se6.status(r6)["result"] == "done",
	"reason=%s" % _se6.status(r6).get("reason"))
_se6.release(r6)

# ④ 已扣款(1110)宽限判成功(无入包/数量回执时不再复买)
r7 = FakeRobot2("robot_g@xy3.com")
r7.m_bag_cache = {101008: [1761, 200, 8199]}
_q7 = FakeQuest()
_q7.bought_tasks = {}
_q7.active_task_index = 0
_q7.shop_ctx = {"item_index": 101008, "count": 200, "bag_before": 200, "errand": True,
	"buy_sent": 1, "paid": "16,000", "paid_ts": getattr(_qe_rt, "__now_ms")() - 30000}
_q7.active = True
_q7.set_state = lambda s: setattr(_q7, "state", s)
r7.m_quest = _q7
_se6._locks["robot_g@xy3.com"] = {"owner": "errand", "since": _se6._now_ms(),
	"item": 101008, "count": 200, "result": None, "reason": "", "session": 1}
check("本地核对: 已扣款且宽限期过 → 判成功不再复买",
	_retry(r7, _q7) is True and _q7.shop_ctx is None)
_se6.release(r7)

# ⑤ 库存≥请求量 → "已满足, 未购买"(明确回执, skipped=True)
_se6._locks["robot_h@xy3.com"] = {"owner": "errand", "since": _se6._now_ms(),
	"item": 101008, "count": 200, "result": None, "reason": "", "session": 1}
r8 = FakeRobot2("robot_h@xy3.com")
_se6.notify_done(r8, 101008, "已满足, 未购买(库存 300 ≥ 请求 200)", skipped=True)
_st8 = _se6.consume(r8, "errand")
check("库存跳过: reason 明确 + skipped=True",
	_st8["result"] == "done" and _st8["skipped"] is True
	and "已满足, 未购买" in _st8["reason"], repr(_st8))

# ⑥ force 链路(绕过"已满足"跳过) + 本地产出静态核对
check("force: shop_errand.start 参数 → cmd.shop_force",
	'shop_force": 1 if force else 0' in _srt)
check("force: quest_engine 认 shop_force/force 并绕过库存跳过",
	'force = bool(cmd.get("shop_force") or cmd.get("force"))' in qe
	and 'not quest.shop_ctx.get("force")' in qe)
check("force: client.py 透传 force(需重启机器人生效)",
	'cmd.get("force")' in cl and "force=_force" in cl)
check("Go: /api/shop_errand 支持 force 字段", "force := toBool(body[\"force\"], false)" in go)

check("接线: 复买前本地核对(25s 看门狗 ST_SHOP 分支)", "__shop_retry_check(robot_object, quest)" in qe)
check("接线: 超时重试前核对(__on_sale_goods 二次下单前)",
	'if int(_ctx.get("buy_sent", 0) or 0) >= 1 and __shop_retry_check(robot_object, quest):' in qe)
check("接线: 下单前快照 bag_before", 'if "bag_before" not in _ctx:' in qe)
check("接线: MAX_TRY 停链前先核对(不把已买到误报 TASK_STUCK)",
	re.search(r"if _ctx\[\"buy_sent\"\] > SHOP_BUY_MAX_TRY:(.{0,200})__shop_retry_check", qe, re.S) is not None)
check("接线: 90066 删物品之外的背包缓存由 90327 补齐(注释/实现)",
	"on_update_add_item_count" in _ro and "m_bag_cache" in _ro)


# ---------------------------------------------------------------- ⑦ 回执签名口径
# notify_done 新参数(reason/skipped)必须与 quest_engine 调用点一致 —— 缺参数会被调用处的
# except 静默吞掉 → "已买到"永不回执（这就是本次重做的直接原因）。
import inspect as _inspect  # noqa: E402

_nd = _inspect.signature(se.notify_done).parameters
check("签名: notify_done 收 reason/skipped(P1 回执口径)",
	"reason" in _nd and "skipped" in _nd)
check("签名: quest_engine 以 skipped=True 调 notify_done(两副本口径一致)",
	"skipped=True" in qe and "notify_done(robot_object, _want_item," in qe)


# 双副本(若存在)一致性
PROD_COPY = "F:/ZyBin/xm/2d-xiyou-server/robot/deploy/single_robot_zy/script"
if os.path.isdir(PROD_COPY):
	_diff = []
	# 2026-09-23 叠加购买修复: protocol3/msghandle/robot_operator 也是本次改动面
	#   (90327 注册/解析/入包数量刷新) —— 一并要求双副本一致
	for f in ("shop_errand.py", "daily_ghost.py", "quest_engine.py", "config.py",
			"client.py", "auto_summon.py", "protocol3.py", "msghandle.py",
			"robot_operator.py"):
		a = os.path.join(SCRIPT, f)
		b = os.path.join(PROD_COPY, f)
		if not os.path.exists(b) or open(a, "rb").read() != open(b, "rb").read():
			_diff.append(f)
	check("双副本一致(核心 9 文件: 采购/协议/消息)", not _diff, "不一致: %s" % _diff)
else:
	check("双副本检查跳过(生产副本不存在)", True)

print("\n结果：%d 项，失败 %d 项" % (total, fails))
sys.exit(1 if fails else 0)
