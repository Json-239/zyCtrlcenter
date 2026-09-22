# -*- coding: utf-8 -*-
"""跨图"到达误判"回归自检（2026-09-21）。

生产现象（2026-09-21 20:51 起，5+ 号反复）：抓鬼号跨图**正常到达目标图后被立刻
误重规划弹回**钟馗所在图，永远到不了刷鬼图。样本 robot0001051@xy3.com：
  20:51:47 接到捉鬼任务 2019503（鬼在图25, kill_area=[25,432,880]）→ 前往图25
  20:52:05 到达跳转点, 发 C2S_DIJKSTRA_DESTINATION 47 → 跨图跳转成功 → 地图 25
  20:52:05 【跨图路由已空但无最终目标, 第 1/3 次重新规划(NPC 10146)】→ 回钟馗(图24)
  20:52:10 跨图跳转成功 → 地图 24 → 走回钟馗附近（被弹回, 死循环）
同类 robot0001009/1024/1034/5286: TASK_STUCK "换图推送迟迟未到..." ×4~5、
stuck_count 7~9。

根因（今天"跨图中转点堆积"修复引入的回归）：__start_next_hop 的 final==None 分支把
两种"route 空 + final None"混为一谈：
  (a) 路由丢失（该重规划/该报错）;
  (b) 已正常到达目标图（跨图完成、route 刚好走空、没有额外 final 点）→ 不该重规划，
      应交给上层状态机（抓鬼 WAIT_GHOST 找鬼等）。
daily_ghost 的 __goto_xy 不清 __teleport_click 残留的 dijkstra_plan（钟馗 10146）→
到达图25 时 plan 非空 → 被当成 (a) → 重规划回钟馗。

修复（本文件校验的就是上线那份源码）：
  1) __teleport_click 建 route 时记 quest.dijkstra_goal_map = 本次跨图目标图;
  2) __start_next_hop 的 route 非空分支兜底记 route[-1].target_map（覆盖 __goto_xy
     等不经过 __teleport_click 的规划入口）;
  3) final==None 分支先判"当前图 == dijkstra_goal_map" → 视为跨图完成: 清残留状态、
     不重规划/不报错、return; 只有未到达才走原"重规划(限3次/60s) → NAV_NO_FINAL"。

本脚本为**源码级自检**（不 import quest_engine：它依赖机器人运行时 cnetwork 等）：
  - 静态断言：3 处修复点结构（S1~S4）;
  - 动态断言：①已到达→不重规划（含反证 ①b：剥掉新判断=今天回归版→同输入仍重规划
    弹回钟馗）②未到达→仍重规划（含限次：第 4 次 NAV_NO_FINAL）③route 有 hop 行为
    不变+兜底记录 ④final 有值行为不变 ⑤目标图未记录(0, 热更旧实例)不误判。

用法：python next_hop_goal_selftest.py [quest_engine.py 路径]
      不带参数默认校验仓库副本；传线上副本路径可再验一次线上文件。
"""
import hashlib
import os
import re
import sys
import time

try:
	sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
	pass

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_ENGINE = os.path.normpath(os.path.join(
	HERE, "..", "deploy", "zones", "prod-240-2300", "script", "quest_engine.py"))

path = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_ENGINE
src = open(path, encoding="utf-8").read()

total = 0
fails = 0


def check(name, ok, detail=""):
	global total, fails
	total += 1
	fails += 0 if ok else 1
	print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
		("  " + detail) if detail else ""))


# ---------------------------------------------------------------- 源码提取
CONSTS = ["NAV_REPLAN_MAX", "NAV_REPLAN_WINDOW_MS", "NAV_REPLAN_BACKOFF_MS"]
FUNCS = ["__schedule", "__start_next_hop"]

parts = []
for name in CONSTS:
	m = re.search(r'(?m)^%s\s*=.*$' % re.escape(name), src)
	assert m, "缺少常量 %s" % name
	parts.append(m.group(0))
for fn in FUNCS:
	m = re.search(r'(?ms)^def %s\(.*?(?=^\S)' % re.escape(fn), src)
	assert m, "缺少函数 %s" % fn
	parts.append(m.group(0))

ENGINE_EXCERPT = "\n".join(parts)
STARTHOP_SRC = re.search(r'(?ms)^def __start_next_hop\(.*?(?=^\S)', src).group(0)
TELEPORT_SRC = re.search(r'(?ms)^def __teleport_click\(.*?(?=^\S)', src).group(0)


# ---------------------------------------------------------------- 静态断言
_i_route = TELEPORT_SRC.index("quest.dijkstra_route = route")
_i_goal = TELEPORT_SRC.index("quest.dijkstra_goal_map = int(pos[0] or 0)")
check("S1 __teleport_click 建 route 后记录本次跨图目标图 dijkstra_goal_map",
	0 <= _i_route < _i_goal, "route@%d goal@%d" % (_i_route, _i_goal))

check("S2 __start_next_hop route 非空分支兜底记录 route[-1].target_map",
	"quest.dijkstra_route[-1].get(\"target_map\"" in STARTHOP_SRC
	and "quest.dijkstra_goal_map = _goal" in STARTHOP_SRC)

_i_goalmap = STARTHOP_SRC.index(
	'_goal_map = int(getattr(quest, "dijkstra_goal_map", 0) or 0)')
_i_plan = STARTHOP_SRC.index('_plan = getattr(quest, "dijkstra_plan", None)')
check("S3 final==None 分支: 到达判断位于 plan 分流/重规划之前",
	0 < _i_goalmap < _i_plan, "goal_map@%d plan@%d" % (_i_goalmap, _i_plan))

_blk = STARTHOP_SRC[_i_goalmap:_i_plan]
check("S4 到达分支清残留状态(route/waiting/jumper/goal_map), 不重规划不报错",
	all(k in _blk for k in (
		"quest.dijkstra_route = []", "quest.dijkstra_waiting = False",
		"quest.dijkstra_jumper = None", "quest.dijkstra_goal_map = 0",
		"return")) and "NAV_NO_FINAL" not in _blk
	and "重新规划" not in _blk)


# ---------------------------------------------------------------- 动态执行
ns = {}
exec(compile(ENGINE_EXCERPT, "<quest_engine excerpt>", "exec"), ns)

CLOCK = [10 ** 12]
EMITS = []
TELEPORTS = []
STUCKS = []


def now_fn():
	return CLOCK[0]


def human_fn(ms):
	return 0


def emit_fn(robot_object, ev):
	EMITS.append(ev)


def teleport_fn(robot_object, quest, npc_id, npc_index, click_type, delay_ms,
		no_npc_jumper=False, force_walk=False):
	TELEPORTS.append({"npc_id": npc_id, "npc_index": npc_index,
		"click_type": click_type, "delay_ms": delay_ms,
		"no_npc_jumper": no_npc_jumper})


def report_fn(robot_object, quest, reason):
	STUCKS.append(reason)


class FakeDiag(object):
	def __init__(self):
		self.logs = []

	def log(self, s):
		self.logs.append(s)


DIAG = FakeDiag()

ns.update({
	"time": time,
	"diag": DIAG,
	"__emit": emit_fn,
	"__now_ms": now_fn,
	"__human_delay": human_fn,
	"__teleport_click": teleport_fn,
	"__report_stuck": report_fn,
})


class FakeQuest(object):
	# 注意: 不默认定义 dijkstra_goal_map —— 等同"热更 reload 后旧 QuestState 实例",
	# 全部用例都跑在"旧实例无该字段 + getattr/setattr 补齐"的兼容前提上。
	def __init__(self, **kw):
		self.dijkstra_route = []
		self.dijkstra_final = None
		self.dijkstra_plan = None
		self.dijkstra_waiting = False
		self.dijkstra_jumper = None
		self.pending = None
		self.nav_replan = 0
		self.nav_replan_ts = 0
		self.errors = []
		for k, v in kw.items():
			setattr(self, k, v)

	def mark_error(self, code, msg):
		self.errors.append((code, msg))


class FakeRobot(object):
	def __init__(self, mapid=0):
		self.m_mapid = mapid


def reset():
	del EMITS[:]
	del TELEPORTS[:]
	del STUCKS[:]
	del DIAG.logs[:]


PLAN_BROKER = {"npc_id": 10146, "npc_index": 10146, "click_type": 0, "delay_ms": 500}
start_next_hop = ns["__start_next_hop"]

# ① 已到达目标图（route 空 + final None + 当前图 == 目标图）→ 跨图完成, 不重规划/不报错
reset()
q = FakeQuest()
q.dijkstra_plan = dict(PLAN_BROKER)
q.dijkstra_goal_map = 25
robot = FakeRobot(mapid=25)
start_next_hop(robot, q, 800)
_warn = [e for e in EMITS if e.get("level") == "warn"]
_done = [e for e in EMITS if "已到达目标图" in e.get("msg", "")]
ok = (len(TELEPORTS) == 0 and len(STUCKS) == 0 and q.pending is None
	and not any(c == "NAV_NO_FINAL" for c, _m in q.errors)
	and not _warn and len(_done) == 1
	and q.dijkstra_goal_map == 0 and q.dijkstra_route == []
	and q.dijkstra_waiting is False)
check("① route 空 + final None + 当前图==目标图 → 视为跨图完成(不重规划/不报错)", ok,
	"teleport=%d warn=%d stuck=%s goal=%s" % (
		len(TELEPORTS), len(_warn), STUCKS, getattr(q, "dijkstra_goal_map", None)))

# ①b 反证: 剥掉"到达判断"块(= 今天引入回归的那版逻辑) → 同输入仍重规划弹回钟馗
_old_src = re.sub(
	r'(?ms)^\t\t\t# 2026-09-21 修复\(回归: 跨图到达误判.*?^\t\t\t\treturn\n',
	'', STARTHOP_SRC)
# 注意不用 "_goal_map" 做残留判定: "dijkstra_goal_map" 含该子串, 会误报
assert "if _goal_map and" not in _old_src and \
	'getattr(quest, "dijkstra_goal_map", 0)' not in _old_src, \
	"反例剥离失败(修复块仍残留)"
ns_old = dict(ns)
exec(compile(_old_src, "<old start_next_hop>", "exec"), ns_old)
reset()
q2 = FakeQuest()
q2.dijkstra_plan = dict(PLAN_BROKER)
q2.dijkstra_goal_map = 25
ns_old["__start_next_hop"](FakeRobot(mapid=25), q2, 800)
ok = (len(TELEPORTS) == 1 and TELEPORTS[0]["npc_id"] == 10146
	and any("重新规划" in e.get("msg", "") for e in EMITS if e.get("level") == "warn"))
check("①b 反证(修复前): 同输入 → 重规划回钟馗(NPC10146), 复现生产弹回", ok,
	"teleport=%s" % (TELEPORTS[:1],))

# ② 未到达目标图（当前图 != 目标图）→ 仍走重规划路径（原兜底保留;
#    覆盖 444 行"跳转点被拒拉黑"后场景: 拉黑不清 goal_map, 位置未到 → 不应误伤）
reset()
q = FakeQuest()
q.dijkstra_plan = dict(PLAN_BROKER)
q.dijkstra_goal_map = 25
robot = FakeRobot(mapid=24)
start_next_hop(robot, q, 800)
ok = (len(TELEPORTS) == 1 and TELEPORTS[0]["npc_id"] == 10146
	and TELEPORTS[0]["delay_ms"] == 500 + 1500 * 1
	and any("重新规划" in e.get("msg", "") for e in EMITS if e.get("level") == "warn"))
check("② route 空 + final None + 当前图!=目标图 → 仍重规划(原兜底保留)", ok,
	"teleport=%s" % (TELEPORTS[:1],))

# ②b 限次不变: 未到达场景继续触发 → 第 3 次后第 4 次用尽报 NAV_NO_FINAL
start_next_hop(robot, q, 800)
start_next_hop(robot, q, 800)
start_next_hop(robot, q, 800)
ok = (len(TELEPORTS) == 3 and len(STUCKS) == 1
	and any("用尽" in e.get("msg", "") for e in EMITS if e.get("level") == "warn")
	and any(c == "NAV_NO_FINAL" for c, _m in q.errors))
check("②b 未到达场景限次不变: 3 次重规划后第 4 次用尽 → NAV_NO_FINAL", ok,
	"teleport=%d stuck=%s errors=%s" % (len(TELEPORTS), STUCKS, q.errors))

# ③ route 有 hop → 行为不变(排 walk), 且兜底记录本次目标图(覆盖 __goto_xy 入口)
reset()
q = FakeQuest()
hop = {"kind": "map_skip", "x": 111, "y": 222, "from_map": 24, "target_map": 25}
q.dijkstra_route = [hop]
robot = FakeRobot(mapid=24)
start_next_hop(robot, q, 500)
ok = (isinstance(q.pending, dict) and q.pending.get("type") == "walk"
	and q.pending["data"].get("hop") is hop
	and q.pending["data"].get("to_x") == 111 and q.pending["data"].get("to_y") == 222
	and getattr(q, "dijkstra_goal_map", None) == 25
	and len(TELEPORTS) == 0 and len(EMITS) == 0)
check("③ route 有 hop → 排 walk 行为不变 + 兜底记录目标图=25", ok,
	"pending=%s goal=%s" % (q.pending.get("type") if q.pending else None,
		getattr(q, "dijkstra_goal_map", None)))

# ④ final 有值 → 行为不变(走 walk final; 即使当前图==目标图也不被"到达"短路)
reset()
q = FakeQuest()
q.dijkstra_final = {"pos": [25, 77, 88], "npc_id": 555, "click_type": 0}
q.dijkstra_plan = {"npc_id": 1}
q.dijkstra_goal_map = 25
robot = FakeRobot(mapid=25)
start_next_hop(robot, q, 500)
ok = (isinstance(q.pending, dict) and q.pending.get("type") == "walk"
	and q.pending["data"]["to_x"] == 77 and q.pending["data"]["to_y"] == 88
	and q.pending["data"]["npc_id"] == 555
	and q.dijkstra_final is None
	and len(TELEPORTS) == 0
	and not any(e.get("level") == "warn" for e in EMITS))
check("④ route 空 + final 有值 → 走 walk(final) 行为不变(final 优先)", ok,
	"pending=%s final=%s" % (q.pending.get("type") if q.pending else None, q.dijkstra_final))

# ⑤ 目标图未记录(0/热更旧实例) → 不误判"已到达"(m_mapid=0 也不误判), 原路径不变
reset()
q = FakeQuest()
q.dijkstra_plan = dict(PLAN_BROKER)
robot = FakeRobot(mapid=0)
start_next_hop(robot, q, 800)
ok = (len(TELEPORTS) == 1 and TELEPORTS[0]["npc_id"] == 10146
	and any("重新规划" in e.get("msg", "") for e in EMITS if e.get("level") == "warn"))
check("⑤ 目标图未记录(0, 热更旧实例) → 不误判已到达, 原路径不变", ok,
	"teleport=%s" % (TELEPORTS[:1],))

# ---------------------------------------------------------------- 结果
print("\n自检目标: %s" % path)
print("sha1=%s" % hashlib.sha1(src.encode("utf-8")).hexdigest()[:12])
print("结果：%d 项，失败 %d 项" % (total, fails))
sys.exit(1 if fails else 0)
