# -*- coding: utf-8 -*-
"""导航 owner 互斥 + 采购跳转"无回执"看门狗自检（2026-09-23i）。

现场（robot0001009@xy3.com, 19:14~19:16, 日志实证）:
  抓鬼满额(50/50) → 同秒两路启动: 转野外游荡(random_walk) + 自动采购(shop_errand)。
  游荡 tick 在 auto_summon.tick 内层被停后**继续执行**, 用陈旧游荡状态覆盖采购刚建好
  的导航(采购"26→612 经 4 跳"被游荡"26→25 经 2 跳"覆盖) → 走到游荡第一跳(3638,658)
  发 C2S_DIJKSTRA_DESTINATION 44 → 服务端无任何回执 → quest.dijkstra_waiting 永挂;
  同刻 quest.state 被环境事件(对话关闭/战斗收尾)改成 WAIT_NEXT → ST_NAV 状态看门狗
  与 __recover 的"重发 destination"分支全部绕过 → 15s×3 超时 → STUCK_WAIT_NEXT 重登
  (当日第 3 次 → 熔断停 ERROR)。

本自检覆盖:
  ① 静态接线: 常量/__send_destination 统一出口/tester 状态无关接线/owner 守卫/闸
  ② __check_hop_noack: 无回执 → 重走+重发(2 次) → 超限分流(采购=换路 → 放弃+上报)
  ③ __cmd_shop_errand: 采购启动导航态清理 + mapid 对齐(乐观过图回滚) + 无路径不静默
  ④ random_walk.tick owner 守卫: 同帧内被"采购停游荡" → 弃权, 不覆盖采购导航
  ⑤ random_walk.dispatch: 采购进行中 → 零副作用拒绝启动游荡(反向顺序)
  ⑥ auto_summon.stop_collect_walk: 彻底交接(置 None)
用法：python tools/nav_owner_hop_watchdog_selftest.py
"""
import os
import re
import sys
import time

try:
	sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
	pass

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.normpath(os.path.join(HERE, ".."))
SCRIPT = os.path.normpath(os.path.join(ROOT, "deploy", "zones", "prod-240-2300", "script"))
sys.path.insert(0, SCRIPT)
from unittest.mock import MagicMock  # noqa: E402
sys.modules.setdefault("cnetwork", MagicMock(name="cnetwork"))
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


import quest_engine as qe  # noqa: E402
import random_walk as rw  # noqa: E402
import auto_summon as asm  # noqa: E402
import shop_errand as se  # noqa: E402
import quest_state as qs  # noqa: E402


# ---------------------------------------------------------------- fake 对象
class FakeRobot(object):
	def __init__(self, acc="robot_nav@xy3.com"):
		self.m_account = [acc]
		self.m_mapid = 26
		self.m_srv_mapid = 0
		self.m_mapid_optimistic = 0
		self.m_pose = [1432, 1448]
		self.m_stop = False
		self.m_logined = True
		self.m_fight_state = False
		self.m_collect_walk = None
		self.m_ghost = None
		self.m_quest = None
		self.m_bag_cache = {}
		self.m_npc_positions = {}
		self.sent = []

	def send_message(self, mid, data=None):
		self.sent.append((mid, data))
		return 0

	def get_role_name(self):
		return "测试角色"


class FakeQuest(object):
	def __init__(self):
		self.chain = None
		self.chain_id = ""
		self.chain_task_set = None
		self.active = False
		self.state = qs.ST_NAV
		self.state_since_ms = 0
		self.retry_count = 0
		self.active_task_index = 0
		self.tasks = {}
		self.dialog = None
		self.dialog_open = False
		self.pending = None
		self.dynamic_npcs = {}
		self.dyn_npc_meta = {}
		self.shop_ctx = None
		self.walk_target = None
		self.walk_end_ms = 0
		self.path_points = []
		self.path_cur = None
		self.walk_pending_hop = None
		self.walk_pending_click = None
		self.dijkstra_route = []
		self.dijkstra_waiting = False
		self.dijkstra_jumper = None
		self.dijkstra_final = None
		self.dijkstra_plan = None
		self.dijkstra_goal_map = 0
		self.nav_replan = 0
		self.nav_replan_ts = 0
		self.hop_black = {}
		self.npc_cand_npc_id = 0
		self.npc_cand_positions = []
		self.npc_cand_idx = 0
		self.click_dbg = None
		self.last_error = None
		self.done_tasks = []
		self.done_base = 0

	def set_state(self, s):
		self.state = s
		self.state_since_ms = time.time() * 1000
		self.retry_count = 0

	def mark_error(self, code, msg=""):
		self.last_error = {"code": code, "msg": msg, "ts": int(time.time() * 1000)}

	def clear_error(self):
		self.last_error = None

	def error_fields(self):
		return ("", 0)

	def timeout_exceeded(self):
		return False

	def get_npc_candidates(self, npc_id, npc_index):
		npcs = (self.chain or {}).get("npcs") or {}
		entry = npcs.get(str(npc_index)) or npcs.get(str(npc_id))
		if not entry:
			return []
		if isinstance(entry[0], (list, tuple)):
			return entry
		return [entry]


class StubWalk(object):
	def __init__(self, mapid=25):
		self.enabled = True
		self.mapid = mapid
		self.map_req = "map"
		self.state = "IDLE"
		self.account = "robot_nav@xy3.com"
		self.jumper_giveup = {}


# ---------------------------------------------------------------- ① 静态接线
qe_src = rd("deploy/zones/prod-240-2300/script/quest_engine.py")
rw_src = rd("deploy/zones/prod-240-2300/script/random_walk.py")
asm_src = rd("deploy/zones/prod-240-2300/script/auto_summon.py")
se_src = rd("deploy/zones/prod-240-2300/script/shop_errand.py")

check("常量: HOP_NOACK_MS/HOP_NOACK_RETRY/SHOP_HOP_REPLAN_MAX 就位",
	"HOP_NOACK_MS = 12 * 1000" in qe_src and "HOP_NOACK_RETRY = 2" in qe_src
	and "SHOP_HOP_REPLAN_MAX = 2" in qe_src)
check("发射口: __send_destination 统一出口(记 _sent_ms)",
	"def __send_destination(robot_object, quest, hop):" in qe_src
	and 'hop["_sent_ms"] = __now_ms()' in qe_src)
check("发射口: __execute_hop 走统一出口(不再裸发)",
	"__send_destination(robot_object, quest, hop)" in qe_src
	and qe_src.count("robot_object.send_message(protocol3.C2S_DIJKSTRA_DESTINATION,") == 1)
check("发射口: __recover 重发也走统一出口(刷新时间戳)",
	qe_src.count("__send_destination(robot_object, quest, quest.dijkstra_route[0])") == 1)
check("看门狗: __check_hop_noack + __shop_hop_noack_giveup + __hop_blacklist 就位",
	"def __check_hop_noack(robot_object, quest, now_ms):" in qe_src
	and "def __shop_hop_noack_giveup(robot_object, quest, hop, dest, n_bl=0):" in qe_src
	and "def __hop_blacklist(robot_object, quest, hop):" in qe_src)
check("接线: tester 状态无关调用(dijkstra_waiting 即检查, 在 active 分支之前)",
	re.search(r"if quest is not None and getattr\(quest, \"dijkstra_waiting\", False\):(.{0,220})"
		r"__check_hop_noack\(robot_object, quest, now_ms\)", qe_src, re.S) is not None)
check("采购收尾: 无回执超限 → SHOP_HOP_NOACK + IDLE(不停在 WAIT_NEXT)",
	'quest.mark_error("SHOP_HOP_NOACK", _msg)' in qe_src
	and qe_src.count("quest.set_state(quest_state.ST_IDLE)") >= 2)
check("采购启动: 清理补 dijkstra_goal_map/nav_replan + mapid 对齐(乐观过图回滚)",
	"quest.dijkstra_goal_map = 0" in qe_src and "quest.nav_replan = 0" in qe_src
	and "回滚乐观过图 %d → %d(服务端确认图)" in qe_src)
check("随机走: tick 内层被停 → owner 守卫弃权(auto_summon.tick 之后)",
	re.search(r"auto_summon\.tick\(robot_object, now_ms\)(.{0,1200}?)"
		r"if getattr\(robot_object, \"m_collect_walk\", None\) is not w or \\\n"
		r"\t\t\tnot getattr\(w, \"enabled\", False\):", rw_src, re.S) is not None)
check("随机走: 采购进行中 → 拒绝启动游荡(零副作用, shop_busy)",
	'"reason": "shop_busy"' in rw_src and "商店采购进行中, 不启动游荡" in rw_src)
_m_asm = re.search(r"def stop_collect_walk\(robot_object\):(.*?)\ndef ", asm_src, re.S)
_asm_body = _m_asm.group(1) if _m_asm else ""
check("auto_summon: stop_collect_walk 彻底交接(置 None, 不限 enabled)",
	"if cw is not None:" in _asm_body
	and "robot_object.m_collect_walk = None" in _asm_body
	and "if cw is not None and cw.enabled:" not in _asm_body)
check("shop_errand: start 停游荡不再要求 enabled(上游置 False 也置 None)",
	re.search(r"if cw is not None:\n\t\t\tcw\.enabled = False\n\t\t\trobot_object\.m_collect_walk = None",
		se_src) is not None)


# ---------------------------------------------------------------- ② 无回执看门狗(动态)
_NOW = int(time.time() * 1000)


def _mk_hop(dest=44, target=24):
	return {"kind": "map_skip", "destination_index": dest, "target_map": target,
		"x": 3638, "y": 658, "_from_map": 26, "_walk_done": True,
		"_sent_ms": _NOW - (qe.HOP_NOACK_MS + 1000)}


r1 = FakeRobot("robot_nav_hop@xy3.com")
q1 = FakeQuest()
q1.state = qs.ST_WAIT_NEXT		# 现场: 状态已被环境事件改成 WAIT_NEXT
q1.active = True
q1.shop_ctx = {"errand": True, "item_index": 101008, "count": 200, "npc_id": 13021}
hop1 = _mk_hop()
q1.dijkstra_route = [hop1]
q1.dijkstra_waiting = True

_ok1 = qe.__check_hop_noack(r1, q1, _NOW)
check("无回执①: 状态无关(WAIT_NEXT 也触发) → 重走跳转点+待重发",
	_ok1 is True and hop1.get("_noack") == 1 and hop1.get("_walk_done") is None
	and q1.dijkstra_waiting is False and isinstance(q1.pending, dict)
	and q1.pending.get("data", {}).get("force_walk") is True
	and q1.pending.get("data", {}).get("hop") is hop1,
	"ok=%s noack=%s pending=%s" % (_ok1, hop1.get("_noack"), q1.pending))

# 模拟重走完成 + __execute_hop 重发后仍无回执(第 2 次)
hop1["_walk_done"] = True
hop1["_sent_ms"] = _NOW - (qe.HOP_NOACK_MS + 1000)
q1.dijkstra_waiting = True
q1.pending = None
_ok2 = qe.__check_hop_noack(r1, q1, _NOW)
check("无回执②: 第 2/2 次仍重走+重发(_noack 累计到 2)",
	_ok2 is True and hop1.get("_noack") == 2 and hop1.get("_walk_done") is None
	and q1.dijkstra_waiting is False and q1.pending is not None)

# 第 3 次: 超限 → 采购分流(换路重规划; plan 非空)
hop1["_walk_done"] = True
hop1["_sent_ms"] = _NOW - (qe.HOP_NOACK_MS + 1000)
q1.dijkstra_waiting = True
q1.pending = None
q1.dijkstra_plan = {"npc_id": 13021, "npc_index": 13021, "click_type": 0,
	"delay_ms": 500, "no_npc_jumper": False}
_ok3 = qe.__check_hop_noack(r1, q1, _NOW)
check("无回执③: 超限 → 采购换路(立即拉黑 + 清路由 + 按 plan 重规划)",
	_ok3 is True and q1.shop_ctx is not None and q1.shop_ctx.get("hop_noack_n") == 1
	and q1.dijkstra_route == [] and q1.dijkstra_waiting is False
	and bool(q1.hop_black) and hop1.get("_fail") >= 1,
	"noack_n=%s black=%s err=%s" % (q1.shop_ctx and q1.shop_ctx.get("hop_noack_n"),
		bool(q1.hop_black), q1.last_error))

# 第 4 次: 换路额度已用尽(hop_noack_n=2) + 同一 hop 再超时(_noack 累计超限) →
#   放弃采购(回 IDLE + 上报错误码 + 通知编排层), 不静默停在 WAIT_NEXT 等状态超时
hop1["_walk_done"] = True
hop1["_sent_ms"] = _NOW - (qe.HOP_NOACK_MS + 1000)
q1.dijkstra_route = [hop1]
q1.dijkstra_waiting = True
q1.pending = None
q1.dijkstra_plan = {"npc_id": 13021, "npc_index": 13021, "click_type": 0, "delay_ms": 500}
q1.shop_ctx["hop_noack_n"] = qe.SHOP_HOP_REPLAN_MAX	# 换路额度已用尽
se._locks["robot_nav_hop@xy3.com"] = {"owner": "errand", "since": se._now_ms(),
	"item": 101008, "count": 200, "result": None, "reason": "", "session": 1}
_ok4 = qe.__check_hop_noack(r1, q1, _NOW)
check("无回执④: 换路用尽 → 放弃采购回明确状态(不静默停在 WAIT_NEXT)",
	_ok4 is True and q1.shop_ctx is None and q1.active is False and q1.state == qs.ST_IDLE
	and (q1.last_error or {}).get("code") == "SHOP_HOP_NOACK"
	and se.status(r1).get("result") == "failed",
	"state=%s err=%s st=%s" % (q1.state, q1.last_error, se.status(r1)))
se.release(r1)

# 已到达但推送丢失 → 按到达收尾(pop hop, 不误报)
r1b = FakeRobot("robot_nav_hop2@xy3.com")
q1b = FakeQuest()
hop2 = _mk_hop(dest=44, target=24)
hop2["_sent_ms"] = _NOW - (qe.HOP_NOACK_MS + 1000)
q1b.dijkstra_route = [hop2]
q1b.dijkstra_waiting = True
q1b.active = False
r1b.m_mapid = 24	# 本地已在目标图(换图推送丢失)
_okb = qe.__check_hop_noack(r1b, q1b, _NOW)
check("无回执⑤: 本地已到目标图 → 按到达收尾(不拉黑不报错)",
	_okb is True and len(q1b.dijkstra_route) == 0 and q1b.dijkstra_waiting is False
	and not q1b.hop_black and q1b.last_error is None)

# 未到 12 秒窗口 / 走路未完成 → 不误判
r1c = FakeRobot("robot_nav_hop3@xy3.com")
q1c = FakeQuest()
hop3 = _mk_hop()
hop3["_sent_ms"] = _NOW - 1000	# 才 1 秒
q1c.dijkstra_route = [hop3]
q1c.dijkstra_waiting = True
check("无回执⑥: 窗口内不误判", qe.__check_hop_noack(r1c, q1c, _NOW) is False)
hop3["_walk_done"] = False
hop3["_sent_ms"] = _NOW - (qe.HOP_NOACK_MS + 1000)
check("无回执⑦: 走路未完成(还没发/正在重走)不误判",
	qe.__check_hop_noack(r1c, q1c, _NOW) is False and hop3.get("_noack") is None)
q1c.walk_target = (100, 100)
hop3["_walk_done"] = True
check("无回执⑧: 走路在途不误判", qe.__check_hop_noack(r1c, q1c, _NOW) is False)


# ---------------------------------------------------------------- ③ 采购启动清理+对齐(动态)
r2 = FakeRobot("robot_nav_shop@xy3.com")
r2.m_mapid = 26
r2.m_mapid_optimistic = 26	# 本地乐观过图
r2.m_srv_mapid = 11		# 服务端确认图仍是 11
q2 = FakeQuest()
q2.dijkstra_route = [_mk_hop()]
q2.dijkstra_waiting = True
q2.dijkstra_jumper = {"npc_id": 10147}
q2.dijkstra_final = {"npc_id": 1}
q2.dijkstra_goal_map = 25
q2.nav_replan = 2
q2.nav_replan_ts = 123
q2.walk_target = (1, 2)
q2.walk_end_ms = 99999
q2.path_points = [(1, 2)]
q2.path_cur = (1, 2)
q2.walk_pending_hop = {"k": 1}
q2.walk_pending_click = {"k": 1}
q2.pending = {"type": "walk"}
r2.m_quest = q2
_res2 = qe.dispatch_cmd(r2, {"cmd": "shop_errand", "npc": 13011,
	"item_index": 102007, "count": 200})
check("采购启动: 导航残留全清(pending/walk_target/route/waiting/jumper/goal_map/replan)",
	q2.walk_target is None and q2.pending is None and q2.dijkstra_route == []
	and q2.dijkstra_waiting is False and q2.dijkstra_jumper is None
	and q2.dijkstra_goal_map == 0 and q2.nav_replan == 0,
	"route=%s wt=%s pend=%s goal=%s" % (q2.dijkstra_route, q2.walk_target,
		q2.pending, q2.dijkstra_goal_map))
check("采购启动: mapid 对齐(乐观过图 26→11 回滚 + optimistic 清零)",
	r2.m_mapid == 11 and r2.m_mapid_optimistic == 0, "mapid=%s opt=%s" % (
		r2.m_mapid, r2.m_mapid_optimistic))
check("采购启动: 无跨图路径 → 明确收尾(SHOP_NO_ROUTE + IDLE, 不静默)",
	q2.last_error is not None and q2.last_error.get("code") == "SHOP_NO_ROUTE"
	and q2.state == qs.ST_IDLE and q2.active is False,
	"err=%s state=%s" % (q2.last_error, q2.state))


# ---------------------------------------------------------------- ④ tick owner 守卫(动态)
_calls = {"goto": 0}
_orig_goto = getattr(rw, "__goto_map")
_orig_asm_tick = asm.tick
_orig_close = getattr(rw, "__close_dialog_if_open")


def _fake_goto(robot_object, quest, w, now_ms):
	_calls["goto"] += 1
	return True


setattr(rw, "__goto_map", _fake_goto)


def _run_tick_case(acc, w, stop_inner):
	"""跑一次 random_walk.tick, 返回 (__goto_map 被调次数, w)。

	stop_inner=True 模拟现场: auto_summon.tick 内层启动采购 → stop_collect_walk 把
	m_collect_walk 置 None(游荡被彻底停), tick 回调后应触发 owner 守卫弃权。
	"""
	r = FakeRobot(acc)
	r.m_mapid = 26
	r.m_collect_walk = w
	q = FakeQuest()
	q.dialog_open = False
	r.m_quest = q

	def _stub_tick(robot_object, now_ms):
		if stop_inner:
			robot_object.m_collect_walk = None

	asm.tick = _stub_tick
	_before = _calls["goto"]
	try:
		rw.tick(r, _NOW)
	finally:
		asm.tick = _orig_asm_tick
	return _calls["goto"] - _before, w


# case A(基线): 内层没停游荡 → tick 走到跨图分支并调 __goto_map
_calls_a, w_a = _run_tick_case("robot_nav_tickA@xy3.com", StubWalk(25), False)
# case B(现场): 内层被打断(采购停游荡, m_collect_walk=None) → 守卫弃权, 不调 __goto_map
_calls_b, w_b = _run_tick_case("robot_nav_tickB@xy3.com", StubWalk(25), True)
setattr(rw, "__goto_map", _orig_goto)

check("tick 守卫: 基线(游荡未被停)仍正常规划跨图",
	_calls_a == 1, "goto_calls=%d" % _calls_a)
check("tick 守卫: 同帧内被采购停游荡 → 弃权, 不用陈旧状态覆盖采购导航",
	_calls_b == 0 and w_b.state == "IDLE", "goto_calls=%d state=%s" % (_calls_b, w_b.state))


# ---------------------------------------------------------------- ⑤ dispatch 反向顺序拒绝(动态)
r4 = FakeRobot("robot_nav_disp@xy3.com")
q4 = FakeQuest()
q4.shop_ctx = {"errand": True, "item_index": 101008, "count": 200}
q4.state = qs.ST_NAV
r4.m_quest = q4
_res4 = rw.dispatch_cmd(r4, {"cmd": "random_walk", "mapid": 25})
check("反向顺序: 采购进行中 → 零副作用拒绝启动游荡(shop_busy)",
	_res4 is not None and _res4.get("reason") == "shop_busy"
	and q4.shop_ctx is not None and q4.state == qs.ST_NAV
	and not getattr(getattr(r4, "m_collect_walk", None), "enabled", False),
	repr(_res4))

# 采购结束后可以正常启动(对照组: 无 shop_ctx → 不拒)
r5 = FakeRobot("robot_nav_disp2@xy3.com")
q5 = FakeQuest()
r5.m_quest = q5
_res5 = rw.dispatch_cmd(r5, {"cmd": "random_walk", "mapid": 25})
check("对照组: 无采购会话 → 游荡正常启动",
	_res5 is not None and _res5.get("result") == "ok"
	and getattr(r5.m_collect_walk, "enabled", False) is True,
	repr(_res5))


# ---------------------------------------------------------------- ⑥ stop_collect_walk 彻底交接
r6 = FakeRobot("robot_nav_stop@xy3.com")
cw6 = StubWalk(25)
cw6.enabled = False	# 上游已先置 False(现场顺序)
r6.m_collect_walk = cw6
asm.stop_collect_walk(r6)
check("stop_collect_walk: enabled 已为 False 也置 None(彻底交接)",
	r6.m_collect_walk is None)

r7 = FakeRobot("robot_nav_stop2@xy3.com")
cw7 = StubWalk(25)
r7.m_collect_walk = cw7
asm.stop_collect_walk(r7)
check("stop_collect_walk: 活跃游荡 → enabled=False + 置 None",
	r7.m_collect_walk is None and cw7.enabled is False)


print("\n结果：%d 项，失败 %d 项" % (total, fails))
sys.exit(1 if fails else 0)
