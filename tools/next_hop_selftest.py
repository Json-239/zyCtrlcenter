# -*- coding: utf-8 -*-
"""跨图"中转点堆积"修复自检（2026-09-21）。

生产现象（2026-09-21 晚）：抓鬼号跨图经跳转NPC(13115 有来有去等)过图后
**静默停在服务端落点**。19:20-20:20 一小时内 86 个号触发 286 次 daily_ghost
"抓鬼 NAV 跨图 120s 仍无进展, 强制重推"，每次静默 86~102s（中位 91s）；
图11(长安东市集) 出现"4 个号挤在同一格"的堆积。样本 robot0001028@xy3.com：
19:59:38 "跳转NPC过图成功 → 地图 11 (主动同步)" + "点击对话选项 #0" 之后
直到 20:01:10 无任何日志/动作（92s），全靠 120s 兜底救回。

根因（源码级）：
  quest_engine.__handle_dialog "点中目的地主动同步"分支先调
  __start_next_hop(...) 排入下一跳 walk（quest.pending = walk），紧接着的
  __schedule(dialog_click) 是**无条件单槽赋值**（__schedule: quest.pending = p）
  → walk 被 dialog_click 覆盖丢弃、永不执行 → 跨图链路静默停住。

修复（本文件校验的就是上线那份源码）：
  1) 主动同步不再直接排下一跳；改为把 then_next_hop_ms 挂在 dialog_click 的
     data 上（__handle_dialog），点击动作真实发出、pending 被消费清空后由
     __do_action 再调 __start_next_hop → 两个动作不再抢同一个 pending 槽。
  2) __start_next_hop 的 final==None 分支不再静默 return：
     quest 链模式（dijkstra_plan 非空）→ 用 plan 记的原目标重规划（复用
     __teleport_click），同一时间窗限 NAV_REPLAN_MAX 次 + 递增退避，用尽报错
     交回上层（NAV_NO_FINAL + __report_stuck）；daily_ghost 模式（plan 空，
     见 __goto_xy 显式 final=None）→ 打 diag 日志交回上层状态机。

本脚本为**源码级自检**（不 import quest_engine：它依赖机器人运行时 cnetwork 等）：
  - 静态断言：分支结构/常量/关键字段；
  - 动态断言：提取真实源码块直接执行 —— ①路由有 hop ②路由空+final ③路由空+
    final None 重规划（含旧实现反例）④限次/退避/时间窗 ⑤daily_ghost 模式，
    以及 ⑥端到端：主动同步 + dialog_click 覆盖复现 + 修复后下一跳真的被排入。

用法：python next_hop_selftest.py [quest_engine.py 路径]
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
FUNCS = ["__schedule", "__start_next_hop", "__do_action"]

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

# __handle_dialog 的 "点中目的地" 决策块（best != None 分支）
m_best = re.search(r'(?ms)^\t\tif best != None:\n(.*?)(?=^\t\telse:)', src)
assert m_best, "未找到 __handle_dialog best 分支（缩进/写法变了?）"
best_block = m_best.group(1)

# __do_action 的 dialog_click 分支
m_dc = re.search(r'(?ms)^\tif t == "dialog_click":\n.*?(?=^\treturn 0\n)', src)
assert m_dc, "未找到 dialog_click 分支"
dc_block = m_dc.group(0)


# ---------------------------------------------------------------- 静态断言
_direct = re.search(r'(?m)^\s*__start_next_hop\(', best_block)
check("S1 主动同步分支不再直接 __start_next_hop(消除同栈覆盖)",
	_direct is None, "块内直接调用=%s" % (_direct.group(0).strip() if _direct else "无"))
check("S2 主动同步把推进挂在 dialog_click.then_next_hop_ms 上",
	'"then_next_hop_ms": _adv' in best_block and "_adv = 800" in best_block)
check("S3 dialog_click 动作执行完再 __start_next_hop",
	"then_next_hop_ms" in dc_block
	and "__start_next_hop(robot_object, quest, _adv)" in dc_block)
check("S4 __start_next_hop final==None 分支不再裸 return",
	"NAV_REPLAN_MAX" in STARTHOP_SRC and "__teleport_click(" in STARTHOP_SRC
	and "NEXT_HOP_DONE" in STARTHOP_SRC)


# ---------------------------------------------------------------- 动态执行
ns = {}
exec(compile(ENGINE_EXCERPT, "<quest_engine excerpt>", "exec"), ns)

check("S5 常量: NAV_REPLAN_MAX=3 / WINDOW=60s / BACKOFF=1500ms",
	ns["NAV_REPLAN_MAX"] == 3 and ns["NAV_REPLAN_WINDOW_MS"] == 60 * 1000
	and ns["NAV_REPLAN_BACKOFF_MS"] == 1500,
	"max=%s win=%s backoff=%s" % (ns["NAV_REPLAN_MAX"], ns["NAV_REPLAN_WINDOW_MS"],
		ns["NAV_REPLAN_BACKOFF_MS"]))

CLOCK = [10 ** 12]
EMITS = []
TELEPORTS = []
STUCKS = []
SCHED_HISTORY = []


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


class FakeProtocol3(object):
	C2S_CLICK_DIALOG = "C2S_CLICK_DIALOG"
	C2S_CLICKNPC = "C2S_CLICKNPC"


class FakeError(object):
	NET_CLOSED = -1
	ROBOT_DELETED = -2


class _Mgr(object):
	def drop_robot(self, robot):
		pass


class FakeRobotMgr(object):
	g_mgr = _Mgr()


ns.update({
	"time": time,
	"diag": DIAG,
	"__emit": emit_fn,
	"__now_ms": now_fn,
	"__human_delay": human_fn,
	"__teleport_click": teleport_fn,
	"__report_stuck": report_fn,
	"__do_walk": lambda robot_object, quest, d, now_ms: 0,
	"protocol3": FakeProtocol3,
	"error": FakeError,
	"robot_mgr": FakeRobotMgr,
})

# 带覆盖记录的 __schedule 探针（执行真实单槽赋值，附记本次动作与覆盖前 pending）
def sched_probe(quest, p):
	_old = quest.pending.get("type") if isinstance(quest.pending, dict) else None
	SCHED_HISTORY.append((p.get("type"), _old))
	ns["__schedule"](quest, p)


class FakeQuest(object):
	def __init__(self, **kw):
		self.dijkstra_route = []
		self.dijkstra_final = None
		self.dijkstra_plan = None
		self.dijkstra_waiting = False
		self.dijkstra_jumper = None
		self.dialog_open = False
		self.dialog = None
		self.pending = None
		self.nav_replan = 0
		self.nav_replan_ts = 0
		self.errors = []
		for k, v in kw.items():
			setattr(self, k, v)

	def mark_error(self, code, msg):
		self.errors.append((code, msg))


class FakeRobot(object):
	def __init__(self, mapid=609):
		self.m_mapid = mapid
		self.msgs = []

	def send_message(self, proto, data=None):
		self.msgs.append((proto, data))
		return 0


def reset():
	del EMITS[:]
	del TELEPORTS[:]
	del STUCKS[:]
	del DIAG.logs[:]
	del SCHED_HISTORY[:]


def indent_body(text, extra="\t"):
	"""整体平移缩进：源码块（n 层 tab）包进函数体（+1 层）。"""
	return "\n".join((extra + l) if l.strip() else "" for l in text.splitlines())


def build_pick(block_text):
	"""把 __handle_dialog 的 best 决策块包成函数执行（依赖全部参数注入）。"""
	code = ("def _pick(best, target, option_list, quest, robot_object,\n"
		"          __now_ms, __human_delay, __schedule, __emit, __start_next_hop):\n"
		+ indent_body(block_text) + "\n")
	nsx = {}
	exec(compile(code, "<best branch>", "exec"), nsx)
	return nsx["_pick"]


def run_pick(pick_fn, quest, robot, best=0, target="长安东市集"):
	return pick_fn(best, target, [("长安东市集", 0), ("离开", 1)], quest, robot,
		now_fn, human_fn, sched_probe, emit_fn, ns["__start_next_hop"])


start_next_hop = ns["__start_next_hop"]
do_action = ns["__do_action"]

# ① 路由有 hop → 正常走 hop 分支（不重规划、不告警）
reset()
q = FakeQuest()
hop = {"kind": "map_skip", "x": 111, "y": 222, "from_map": 11, "target_map": 12}
q.dijkstra_route = [hop]
q.dijkstra_final = {"pos": [13, 5, 6], "npc_id": 999, "click_type": 0}
q.dijkstra_plan = {"npc_id": 1}
start_next_hop("robot", q, 500)
ok = (isinstance(q.pending, dict) and q.pending.get("type") == "walk"
	and q.pending["data"].get("hop") is hop
	and q.pending["data"].get("to_x") == 111 and q.pending["data"].get("to_y") == 222
	and len(TELEPORTS) == 0 and len(EMITS) == 0 and len(DIAG.logs) == 0)
check("① 路由有 hop → 正常排下一跳 walk(不重规划)", ok,
	"pending=%s teleport=%d emits=%d" % (
		q.pending.get("type") if q.pending else None, len(TELEPORTS), len(EMITS)))

# ② 路由空 + final 有 → 走 walk(final)（原行为不变）
reset()
q = FakeQuest()
q.dijkstra_final = {"pos": [13, 77, 88], "npc_id": 555, "click_type": 0}
q.dijkstra_plan = {"npc_id": 1}
start_next_hop("robot", q, 500)
ok = (q.pending is not None and q.pending.get("type") == "walk"
	and q.pending["data"]["to_x"] == 77 and q.pending["data"]["to_y"] == 88
	and q.pending["data"]["npc_id"] == 555
	and q.dijkstra_final is None
	and len(TELEPORTS) == 0 and len(EMITS) == 0)
check("② 路由空 + final 有 → 走 walk(final)(原行为不变)", ok,
	"pending=%s final=%s" % (q.pending.get("type") if q.pending else None, q.dijkstra_final))

# ③ 路由空 + final None + 有 plan → 重规划（修复点）
reset()
q = FakeQuest()
q.dijkstra_plan = {"npc_id": 13115, "npc_index": 13115, "click_type": 0,
	"delay_ms": 500, "no_npc_jumper": False, "pos": [11, 129, 82]}
start_next_hop("robot", q, 800)
_warn3 = [e for e in EMITS if e.get("level") == "warn"]
ok = (len(TELEPORTS) == 1 and TELEPORTS[0]["npc_id"] == 13115
	and TELEPORTS[0]["delay_ms"] == 500 + 1500 * 1
	and q.pending is None
	and any("重新规划" in e.get("msg", "") for e in _warn3))
check("③ 路由空 + final None + 有 plan → 重规划 + warn(不再什么都不做)", ok,
	"teleport=%s warn=%s" % (TELEPORTS[:1], [e.get("msg") for e in _warn3][:1]))

# ③b 反例：还原旧实现（final==None 直接 return）→ 复现"什么都不做"
_old_src = re.sub(
	r'(?ms)^\t\tif final == None:\n.*?^\t\t__schedule\(quest, \{',
	'\t\tif final == None:\n\t\t\treturn\n\t\t__schedule(quest, {',
	STARTHOP_SRC)
assert "nav_replan" not in _old_src, "反例替换失败（旧实现仍含新逻辑）"
ns_old = dict(ns)
exec(compile(_old_src, "<old start_next_hop>", "exec"), ns_old)
reset()
q2 = FakeQuest()
q2.dijkstra_plan = {"npc_id": 13115, "npc_index": 13115, "click_type": 0,
	"delay_ms": 500, "no_npc_jumper": False, "pos": [11, 129, 82]}
ns_old["__start_next_hop"]("robot", q2, 800)
ok = (len(TELEPORTS) == 0 and len(EMITS) == 0 and q2.pending is None)
check("③b 反例(旧实现): 同输入 → 无重规划/无日志/无动作(什么都不做)", ok,
	"teleport=%d emits=%d pending=%s" % (len(TELEPORTS), len(EMITS), q2.pending))

# ④a 同一时间窗内限 3 次 + 递增退避（delay = 500 + 1500*N）
reset()
q = FakeQuest()
q.dijkstra_plan = {"npc_id": 13115, "npc_index": 13115, "delay_ms": 500, "click_type": 0}
for _i in range(3):
	start_next_hop("robot", q, 800)
ok = (len(TELEPORTS) == 3
	and [t["delay_ms"] for t in TELEPORTS] == [2000, 3500, 5000]
	and q.nav_replan == 3)
check("④a 同一时间窗内限 3 次 + 递增退避(2000/3500/5000ms)", ok,
	"delays=%s nav_replan=%s" % ([t["delay_ms"] for t in TELEPORTS], q.nav_replan))

# ④b 第 4 次用尽 → 不重规划, 报 NAV_NO_FINAL + __report_stuck（交回上层）
start_next_hop("robot", q, 800)
ok = (len(TELEPORTS) == 3 and len(STUCKS) == 1
	and any("用尽" in e.get("msg", "") for e in EMITS if e.get("level") == "warn")
	and q.dijkstra_plan is None and q.nav_replan == 0
	and any(c == "NAV_NO_FINAL" for c, _m in q.errors))
check("④b 第 4 次用尽 → 报错交回上层(不无限重规划)", ok,
	"teleport=%d stuck=%s errors=%s" % (len(TELEPORTS), STUCKS, q.errors))

# ④c 距上次重规划超 60s 时间窗 → 计数重开（不误伤长跑号）
reset()
q = FakeQuest()
q.dijkstra_plan = {"npc_id": 13115, "delay_ms": 500, "click_type": 0}
q.nav_replan = 3
q.nav_replan_ts = int(time.time() * 1000) - 61000
start_next_hop("robot", q, 800)
ok = (len(TELEPORTS) == 1 and q.nav_replan == 1
	and TELEPORTS[0]["delay_ms"] == 2000)
check("④c 超时间窗 → 计数重开(窗口滑动, 不误伤)", ok,
	"teleport=%d nav_replan=%s" % (len(TELEPORTS), q.nav_replan))

# ⑤ daily_ghost 模式（plan 空, final=None）→ diag 交回上层, 不误报/不重规划
reset()
q = FakeQuest()
start_next_hop("robot", q, 500)
ok = (len(TELEPORTS) == 0 and len(EMITS) == 0 and len(STUCKS) == 0
	and q.pending is None
	and any("NEXT_HOP_DONE" in s for s in DIAG.logs))
check("⑤ daily_ghost 模式(plan 空) → diag 交回上层, 不误报/不重规划", ok,
	"teleport=%d emits=%d diag=%s" % (len(TELEPORTS), len(EMITS), DIAG.logs[-1:]))

# ⑥ 端到端：主动同步 + dialog_click 覆盖复现 + 修复后下一跳真被排入
hop1 = {"kind": "npc_jumper", "x": 2365, "y": 610, "from_map": 609, "target_map": 11,
	"npc_index": 13115, "target_name": "长安东市集"}
hop2 = {"kind": "map_skip", "x": 76, "y": 541, "from_map": 11, "target_map": 12}
hop3 = {"kind": "map_skip", "x": 100, "y": 200, "from_map": 12, "target_map": 13}

# ⑥a/⑥b 修复后：主动同步 → pending 是 dialog_click(带 then_next_hop_ms)；
# 动作执行完 → 下一跳 walk 真被排入
reset()
q = FakeQuest()
q.dijkstra_route = [hop1, hop2, hop3]
robot = FakeRobot(mapid=609)
run_pick(build_pick(best_block), q, robot)
seq_new = [t for t, _old in SCHED_HISTORY]
ok_a = (isinstance(q.pending, dict) and q.pending.get("type") == "dialog_click"
	and q.pending["data"].get("then_next_hop_ms") == 800
	and robot.m_mapid == 11 and len(q.dijkstra_route) == 2
	and seq_new == ["dialog_click"] and SCHED_HISTORY[0][1] is None)
act = q.pending
q.pending = None
do_action(robot, q, act, CLOCK[0])
ok_b = (isinstance(q.pending, dict) and q.pending.get("type") == "walk"
	and q.pending["data"].get("hop") is hop2)
check("⑥a 修复后: 主动同步后 pending=dialog_click(带 then_next_hop_ms)", ok_a,
	"pending=%s seq=%s mapid=%s route=%d" % (
		q.pending.get("type") if q.pending else None, seq_new, robot.m_mapid,
		len(q.dijkstra_route)))
check("⑥b 修复后: 点击动作执行完 → 下一跳 walk 真被排入", ok_b,
	"pending=%s" % (q.pending.get("type") if q.pending else None))

# ⑥c 反例(修复前): 主动同步里直接 __start_next_hop → walk 被 dialog_click 覆盖丢失
_old_block = re.sub(r'(?m)^\t\t\t_adv = 0\t.*\n', '', best_block)
_old_block = re.sub(
	r'(?ms)^\t\t\t\t\t\t# 2026-09-21 修复\(跨图中转点堆积\).*?^\t\t\t\t\t\t_adv = 800\n',
	'\t\t\t\t\t\t__start_next_hop(robot_object, quest, 800)\n', _old_block)
_old_block = re.sub(r'(?m)^\t{5}_adv = 0$', '\t' * 5 + 'pass', _old_block)
_old_block = _old_block.replace(
	'"data": {"option_index": best, "then_next_hop_ms": _adv},',
	'"data": {"option_index": best},')
assert "_adv" not in _old_block and "__start_next_hop(robot_object, quest, 800)" in _old_block, \
	"反例块替换失败"
reset()
q = FakeQuest()
q.dijkstra_route = [hop1, hop2, hop3]
robot = FakeRobot(mapid=609)
run_pick(build_pick(_old_block), q, robot)
seq_old = [t for t, _old in SCHED_HISTORY]
# 覆盖证据: 探针记录到 dialog_click 落槽时, 被覆盖的 pending 正是 walk
_cov = SCHED_HISTORY[-1] if SCHED_HISTORY else (None, None)
ok_c = (isinstance(q.pending, dict) and q.pending.get("type") == "dialog_click"
	and _cov == ("dialog_click", "walk")
	and robot.m_mapid == 11 and len(q.dijkstra_route) == 2)
act = q.pending
q.pending = None
do_action(robot, q, act, CLOCK[0])
ok_d = (q.pending is None)
check("⑥c 反例(修复前): 先排 walk 再被 dialog_click 覆盖 → 静默停住", ok_c and ok_d,
	"覆盖记录=%s 最终 pending=%s" % (_cov, q.pending))

# ---------------------------------------------------------------- 结果
print("\n自检目标: %s" % path)
print("sha1=%s" % hashlib.sha1(src.encode("utf-8")).hexdigest()[:12])
print("结果：%d 项，失败 %d 项" % (total, fails))
sys.exit(1 if fails else 0)
