# -*- coding: utf-8 -*-
"""24 死区修复自检（2026-09-30）——"跳级换图"路由剥离 + 主动推进。

生产现象（2026-09-30 全天量化, docs/04-测试/分析-20260930-幽冥界24扎堆量化.md）:
  抓鬼号打鬼完成回 24 幽冥界交付, 经 609 中转的多跳路线(如 11→609→24)在
  **走路途中**被服务端直接换到 24(S2C_CHANGE_MAP, 非 destination 回执路径) ——
  旧实现 __tick_walk"行走中换图"分支只取消走路; 路由里实际已被服务端跳过、
  但仍判"未达成"的跳永远悬挂(各模式"跨图推进"只比 route[0].target_map,
  不命中 → 谁都不推进) → 抓鬼 SUBMIT 的"导航活跃"豁免一直为真 → 白等
  SUBMIT_NAV_LIMIT_MS=120s 硬上限兜底。当天 5432 次慢回, p50=124s/次;
  而"单跳直达"路径(12/25/26→24)5~7s 正常 —— 两极分化即本缺陷指纹。

修复（本文件校验的就是上线那份源码）:
  1) 新增 quest_engine.__winnow_route_by_map(robot_object, quest):
     找 route 里第一个 target_map==当前图 的跳 k; k>0(跳级) → pop 0..k +
     清 waiting/jumper + (剥离后不连贯则整条作废) → 返回 True; k<=0 不动,
     返回 False(零行为变化)。
  2) __tick_walk"行走中换图"分支: __cancel_walk 后调 winnow, 命中即
     __start_next_hop 推进(route 空则走 final 交付段)。
  3) __check_hop_noack 的"无回执到达收尾": 增加 elif 跳级变体(等回执期被
     跳级换图 → 按到达推进, 不再重走/重发错向 route[0])。
  4) daily_ghost 与 quest_engine 两条"跨图推进"分支: 增加 elif winnow 兜底
     (静止中跳级换图; route[0] 命中的原逻辑逐字保留, 不命中零变化)。

本脚本为源码级自检(不 import quest_engine: 依赖机器人运行时 cnetwork 等),
直接提取真实源码块 exec 执行; 并对 .bak_20260930_navdead 旧版跑同场景取证
"旧版必然悬挂"(坏版灵敏度)。

用法: python navdead_selftest.py [engine_path] [daily_ghost_path]
      不带参数默认校验仓库副本 + 其 .bak; 传参可校验其它副本。
"""
import hashlib
import os
import random
import re
import sys
import time

try:
	sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
	pass

HERE = os.path.dirname(os.path.abspath(__file__))
DEF_ENGINE = os.path.normpath(os.path.join(
	HERE, "..", "deploy", "zones", "prod-240-2300", "script", "quest_engine.py"))
DEF_GHOST = os.path.normpath(os.path.join(
	HERE, "..", "deploy", "zones", "prod-240-2300", "script", "daily_ghost.py"))

engine_path = sys.argv[1] if len(sys.argv) > 1 else DEF_ENGINE
ghost_path = sys.argv[2] if len(sys.argv) > 2 else DEF_GHOST
src = open(engine_path, encoding="utf-8").read()
gsrc = open(ghost_path, encoding="utf-8").read()

total = 0
fails = 0


def check(name, ok, detail=""):
	global total, fails
	total += 1
	fails += 0 if ok else 1
	print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
		("  " + detail) if detail else ""))


def grab_func(text, name):
	m = re.search(r'(?ms)^def %s\(.*?(?=^\S)' % re.escape(name), text)
	return m.group(0) if m else None


def grab_const(text, name):
	m = re.search(r'(?m)^%s\s*=.*$' % re.escape(name), text)
	return m.group(0) if m else None


# ---------------------------------------------------------------- 源码提取
WINNOW_SRC = grab_func(src, "__winnow_route_by_map")
CANCEL_SRC = grab_func(src, "__cancel_walk")
TICKWALK_SRC = grab_func(src, "__tick_walk")
STARTHOP_SRC = grab_func(src, "__start_next_hop")
SCHEDULE_SRC = grab_func(src, "__schedule")
NOACK_SRC = grab_func(src, "__check_hop_noack")
NOACK_C = "\n".join(x for x in (grab_const(src, "HOP_NOACK_MS"),
	grab_const(src, "HOP_NOACK_RETRY"), grab_const(src, "SUBMIT_NAV_LIMIT_MS")) if x)

assert WINNOW_SRC, "缺少 __winnow_route_by_map（提取失败: 函数不存在?）"
assert TICKWALK_SRC and STARTHOP_SRC and CANCEL_SRC and SCHEDULE_SRC and NOACK_SRC, \
	"关键函数提取失败"

# ---------------------------------------------------------------- 静态断言
check("S1 换图分支: 取消走路后调 winnow 并推进",
	re.search(r'(?ms)walk_start_mapid != robot_object\.m_mapid:.{0,600}?'
		r'__cancel_walk\(quest\).{0,600}?__winnow_route_by_map\(robot_object, quest\).{0,200}?'
		r'__start_next_hop\(', TICKWALK_SRC) is not None)
check("S2 noack 看门狗: '已到目标图'收尾后增加跳级 elif",
	re.search(r'(?ms)跳转无回执但本地已到目标图.{0,900}?elif __winnow_route_by_map\(robot_object, quest\):',
		NOACK_SRC) is not None)
check("S3 daily_ghost 跨图推进分支: route[0] 命中后增加跳级 elif",
	re.search(r'(?ms)quest_engine\.__start_next_hop\(robot_object, quest, 800\)\n\t\t\treturn 0\n'
		r'\t\telif quest_engine\.__winnow_route_by_map\(robot_object, quest\):',
		gsrc) is not None)
check("S4 quest_engine 任务链推进分支: 同样增加跳级 elif",
	re.search(r'(?ms)跳转NPC过图成功 → 地图 %d.{0,900}?elif __winnow_route_by_map\(robot_object, quest\):',
		src) is not None)
check("S5 winnow 调用点=3处(换图分支/noack/任务链) + 1定义",
	src.count("__winnow_route_by_map(") == 4,
	"engine 中出现 %d 次" % src.count("__winnow_route_by_map("))
check("S6 120s 硬上限兜底保留(SUBMIT_NAV_LIMIT_MS=120000)",
	"SUBMIT_NAV_LIMIT_MS = 120000" in gsrc)

# ---------------------------------------------------------------- 执行环境
EMITS = []
CLOCK = [10 ** 12]


def build_ns(text):
	ns = {}
	blobs = [x for x in (
		grab_func(text, "__winnow_route_by_map"),
		grab_func(text, "__cancel_walk"),
		grab_func(text, "__schedule"),
		grab_func(text, "__start_next_hop"),
		(grab_const(text, "HOP_NOACK_MS") or "") + "\n"
		+ (grab_const(text, "HOP_NOACK_RETRY") or ""),
		grab_func(text, "__check_hop_noack"),
		grab_func(text, "__tick_walk"),
	) if x]
	exec(compile("\n".join(blobs), "<excerpt>", "exec"), ns)

	def emit_fn(robot_object, ev):
		EMITS.append(ev)

	def now_fn():
		return CLOCK[0]

	ns.update({
		"time": time, "random": random, "re": re,
		"__emit": emit_fn,
		"__now_ms": now_fn,
		"__human_delay": lambda ms: 0,
		"__hop_walk_target": lambda x, y: (x, y),
		"__hop_retry_delay_ms": lambda n: 0,
		"__hop_blacklist": lambda ro, q, hop: 0,
		"__shop_hop_noack_giveup": lambda ro, q, hop, dest, nb: True,
		"__send_destination": lambda ro, q, hop: None,
	})
	return ns


class FakeRobot(object):
	def __init__(self, mapid):
		self.m_mapid = mapid
		self.m_account = ["robot_TEST@xy3.com"]
		self.m_pose = [100, 100]
		self.m_stop = False

	def get_role_name(self):
		return "自检角色"


class FakeQuest(object):
	def __init__(self):
		self.dijkstra_route = []
		self.dijkstra_final = None
		self.dijkstra_plan = None
		self.dijkstra_waiting = False
		self.dijkstra_jumper = None
		self.dijkstra_goal_map = 0
		self.pending = None
		self.active = False
		self.nav_replan = 0
		self.nav_replan_ts = 0
		self.walk_target = None
		self.walk_end_ms = 0
		self.walk_report_next_ms = 0
		self.path_points = []
		self.path_cur = None
		self.path_end_ms = 0
		self.walk_pending_hop = None
		self.walk_pending_click = None
		self.walk_hop_wait_count = 0
		self.timeout_override_ms = 0
		self.walk_seg_pause_until_ms = 0
		self.walk_last_seg_ms = 0
		self.walk_pos_synced = False
		self.walk_synced_pos = None
		self.walk_sync_mapid = 0
		self.walk_start_mapid = 0


def hop(fm, tm, dest=77):
	return {"kind": "map_skip", "from_map": fm, "target_map": tm,
		"_from_map": fm, "x": 100, "y": 200, "destination_index": dest}


FINAL = {"npc_id": 10146, "npc_index": 10146, "click_type": 0,
	"pos": [24, 1728, 1056], "force_walk": False}

NS = build_ns(src)

# ---------------------------------------------------------------- 单元断言 winnow
def w_run(route, mid, waiting=False, jumper="j", final=None):
	q = FakeQuest()
	q.dijkstra_route = route
	q.dijkstra_waiting = waiting
	q.dijkstra_jumper = jumper
	q.dijkstra_final = final
	r = FakeRobot(mid)
	ok = NS["__winnow_route_by_map"](r, q)
	return ok, q


DEL = EMITS[:]
del EMITS[:]

# A1: 3 跳, 服务端一步送到终点 24(k=2) → 全剥离
ok, q = w_run([hop(11, 609), hop(609, 12), hop(12, 24)], 24)
check("A1 跳级 k=2: route 全剥离 + waiting/jumper 清空",
	ok is True and q.dijkstra_route == [] and q.dijkstra_waiting is False
	and q.dijkstra_jumper is None, "ok=%s route=%d" % (ok, len(q.dijkstra_route)))

# A2: 2 跳被送到中途且后面还有路 → 剥离到连贯剩余
ok, q = w_run([hop(11, 609), hop(609, 24), hop(24, 26)], 24)
check("A2 跳级 k=1: 剥离 2 跳, 剩余首跳连贯(_from_map==24)",
	ok is True and len(q.dijkstra_route) == 1
	and q.dijkstra_route[0]["target_map"] == 26
	and q.dijkstra_route[0]["_from_map"] == 24,
	"剩余=%d" % len(q.dijkstra_route))

# A3: 剥离后不连贯(假数据) → 整条作废
ok, q = w_run([hop(13, 609), hop(609, 24), {"kind": "map_skip", "target_map": 12,
	"_from_map": 777, "x": 1, "y": 2, "destination_index": 1}], 24)
check("A3 剥离后不连贯: 整条作废(交 final/plan 兜底)",
	ok is True and q.dijkstra_route == [], "route=%d" % len(q.dijkstra_route))

# A4: k=0 正常到站 → 不动(交既有推进逻辑; 快回场景逐字不变)
ok, q = w_run([hop(13, 609), hop(609, 24)], 609, waiting=False, jumper="j")
check("A4 k=0 正常到站: 不动 route/waiting/jumper(逐字不变)",
	ok is False and len(q.dijkstra_route) == 2 and q.dijkstra_jumper == "j",
	"ok=%s route=%d" % (ok, len(q.dijkstra_route)))

# A5: k<0(被换到路由之外的图) → 不动
ok, q = w_run([hop(13, 609), hop(609, 24)], 99)
check("A5 k<0(路由之外): 不动, 返回 False", ok is False and len(q.dijkstra_route) == 2)

# A6: 无路由
ok, q = w_run([], 24)
check("A6 空路由: 返回 False 不动", ok is False)

# ---------------------------------------------------------------- 集成断言
# A7: __tick_walk 换图分支全链(走路中被跳级换图 13→24)
q = FakeQuest()
q.dijkstra_route = [hop(13, 609), hop(609, 24)]
q.dijkstra_final = dict(FINAL)
q.dijkstra_goal_map = 24
q.walk_target = (1, 2)
q.walk_start_mapid = 13
r = FakeRobot(24)
del EMITS[:]
ret = NS["__tick_walk"](r, q, CLOCK[0])
walk_p = q.pending if isinstance(q.pending, dict) else None
check("A7 集成: 换图分支 → route 剥离 + final 交付段已排入(pending.walk→点钟馗)",
	ret is True and len(q.dijkstra_route) == 0 and walk_p is not None
	and walk_p.get("type") == "walk"
	and walk_p.get("data", {}).get("npc_id") == 10146,
	"ret=%s route=%d pending=%s" % (ret, len(q.dijkstra_route),
		walk_p.get("type") if walk_p else None))
check("A7b 集成: 换图日志仍保留'行走中换图 13→24'",
	any("行走中换图 13→24" in str(e.get("msg", "")) for e in EMITS))

# A8: __check_hop_noack 等回执期跳级变体(609==route[0] 不命中, k=1 命中)
q = FakeQuest()
h1 = hop(13, 609, dest=48)
h1["_walk_done"] = True
h1["_sent_ms"] = CLOCK[0] - 20000
q.dijkstra_route = [h1, hop(609, 24)]
q.dijkstra_waiting = True
q.dijkstra_final = dict(FINAL)
r = FakeRobot(24)
del EMITS[:]
ret = NS["__check_hop_noack"](r, q, CLOCK[0])
walk_p = q.pending if isinstance(q.pending, dict) else None
check("A8 集成: noack 等回执期跳级 → 按已到达推进(不重走/重发错向)",
	ret is True and len(q.dijkstra_route) == 0 and q.dijkstra_waiting is False
	and walk_p is not None and walk_p.get("type") == "walk",
	"ret=%s route=%d waiting=%s" % (ret, len(q.dijkstra_route), q.dijkstra_waiting))

# A9: noack k=0(已到 route[0] 目标图) 原分支逐字保留
q = FakeQuest()
h1 = hop(13, 609)
h1["_walk_done"] = True
h1["_sent_ms"] = CLOCK[0] - 20000
q.dijkstra_route = [h1, hop(609, 24)]
q.dijkstra_waiting = True
q.dijkstra_final = dict(FINAL)
r = FakeRobot(609)
del EMITS[:]
ret = NS["__check_hop_noack"](r, q, CLOCK[0])
check("A9 noack k=0 原'已到目标图'分支保留(逐字)",
	ret is True and len(q.dijkstra_route) == 1
	and any("按到达收尾" in str(e.get("msg", "")) for e in EMITS),
	"route=%d" % len(q.dijkstra_route))

# ---------------------------------------------------------------- 坏版灵敏度
BAK_ENGINE = engine_path + ".bak_20260930_navdead"
BAK_GHOST = ghost_path + ".bak_20260930_navdead"
assert os.path.exists(BAK_ENGINE), "缺少坏版基线 %s" % BAK_ENGINE
bsrc = open(BAK_ENGINE, encoding="utf-8").read()
bgsrc = open(BAK_GHOST, encoding="utf-8").read()

check("B1 坏版无 __winnow_route_by_map(旧版根本无此能力)",
	grab_func(bsrc, "__winnow_route_by_map") is None)
check("B2 坏版 daily_ghost 无 winnow 兜底",
	"__winnow_route_by_map" not in bgsrc)

BNS = build_ns(bsrc)

def b_tickwalk(route, mid, final):
	q = FakeQuest()
	q.dijkstra_route = route
	q.dijkstra_final = final
	q.walk_start_mapid = 13
	q.walk_target = (1, 2)
	r = FakeRobot(mid)
	ret = BNS["__tick_walk"](r, q, CLOCK[0])
	return ret, q

# B3: 同场景(走路中被跳级换图) 旧版必然悬挂 → 120s 死区成因实证
b_ret, bq = b_tickwalk([hop(13, 609), hop(609, 24)], 24, dict(FINAL))
check("B3 坏版同场景: route 不剥离 + 无推进(悬挂=120s 死区成因)",
	b_ret is True and len(bq.dijkstra_route) == 2 and bq.pending is None,
	"route=%d pending=%s" % (len(bq.dijkstra_route), bq.pending))

# B4: 旧版 noack 跳级 → 错向重走重发(而非按到达推进)
bq2 = FakeQuest()
bh = hop(13, 609, dest=48)
bh["_walk_done"] = True
bh["_sent_ms"] = CLOCK[0] - 20000
bq2.dijkstra_route = [bh, hop(609, 24)]
bq2.dijkstra_waiting = True
bq2.dijkstra_final = dict(FINAL)
br2 = FakeRobot(24)
b_ret2 = BNS["__check_hop_noack"](br2, bq2, CLOCK[0])
check("B4 坏版 noack 跳级: 落入错向'重走重发'(而非按到达推进)",
	b_ret2 is True and int(bh.get("_noack", 0)) == 1
	and len(bq2.dijkstra_route) == 2,
	"_noack=%s route=%d" % (bh.get("_noack"), len(bq2.dijkstra_route)))

print("\n自检目标: %s" % engine_path)
print("sha1=%s" % hashlib.sha1(src.encode("utf-8")).hexdigest()[:12])
print("ghost  sha1=%s" % hashlib.sha1(gsrc.encode("utf-8")).hexdigest()[:12])
print("结果：%d 项，失败 %d 项" % (total, fails))
sys.exit(1 if fails else 0)
