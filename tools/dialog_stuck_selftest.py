# -*- coding: utf-8 -*-
"""对话卡死根因修复自检（2026-09-22）。

生产现象（1 小时内）：
  - "对话超 30 秒未关闭, 强制关闭回 READY(第 N 轮)"  5412 次
  - "钟馗对话 N 轮超 30 秒未关闭(服务端对话状态卡死)" 1401 次（= 1401 次重登）
  - 467 个可测样本里"第 1 轮 → 第 7 轮重登"耗时全部 7~16 秒（p50=8s）。
    若判据真是"对话打开超 30 秒", 7 轮至少 210 秒 —— 实测排除该可能。

根因（三个缺陷叠加）：
  A. 计时基准错误: daily_ghost 看门狗用 g.state_since_ms(进入 READY 的时间) 当
     "对话已打开时长"。进 READY 早已 >30s 时对话刚打开即判超时; 且 __set_state 在
     状态不变(已是 READY)时不刷新时间戳 → 每 tick 都判超时, g.rounds 以 tick
     频率累加 → 8 秒攒满 7 轮触发重登。
  B. 守卫吞对话: quest_engine.show_dialog 的 `if quest.pending == None` 守卫在
     点击/走路动作在途时整段跳过 __handle_dialog → 选项永不点, 对话永久挂着。
  C. 空档重复走同一跳: daily_ghost READY 段在"动作刚消费完的空档期"立刻
     __start_next_hop, 重复点击同一跳转NPC(重开对话), 并把 pending 填上 → 触发 B。

本脚本为源码级自检（不 import daily_ghost/quest_engine 全量: 依赖机器人运行时）：
  1) 静态断言 —— 修复点存在且顺序正确（两文件）。
  2) 动态断言 —— 提取真实源码块 + fake 注入直接执行：
     A1~A5 看门狗(计时基准/同段只计一轮/真实卡死仍能重登/关闭项分支重计时)
     B1~B3 守卫 deferred 标记 + tick/__consume_nav 补处理
     C1~C2 READY 空档宽限
     反例   旧基准表达式在同一输入下必然误判（证明测试能抓住回归）

用法：python dialog_stuck_selftest.py [script_dir]
      不带参数默认校验仓库副本；传线上 script 目录可再验一次线上文件。
"""
import hashlib
import os
import re
import sys

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_DIR = os.path.normpath(os.path.join(
    HERE, "..", "deploy", "zones", "prod-240-2300", "script"))

script_dir = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_DIR
ghost_path = os.path.join(script_dir, "daily_ghost.py")
engine_path = os.path.join(script_dir, "quest_engine.py")
ghost_src = open(ghost_path, encoding="utf-8").read()
engine_src = open(engine_path, encoding="utf-8").read()

fails = 0


def check(name, ok, detail=""):
    global fails
    fails += 0 if ok else 1
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  " + detail) if detail else ""))


# ================================================================ 源码提取
# 看门狗块（daily_ghost: if quest.dialog_open 段内, 从 _dlg_ms 到块尾 return 0）
m_wd = re.search(
    r'(?ms)^\t\t_dlg_ms = int\(getattr\(quest, "dialog_open_ms", 0\) or 0\) \\\n.*?^\t\treturn 0\n',
    ghost_src)
assert m_wd, "未提取到 daily_ghost 看门狗块（_dlg_ms 开头; 缩进/写法变了?）"
wd_block = m_wd.group(0)

# READY 空档宽限块
m_grace = re.search(
    r'(?ms)^\t\t\t_nav_last = int\(getattr\(g, "nav_last_progress_ms", 0\) or 0\)\n.*?^\t\t\treturn 0\n',
    ghost_src)
assert m_grace, "未提取到 READY 空档宽限块"
grace_block = m_grace.group(0)

# __consume_nav pending 消费块
m_consume = re.search(
    r'(?ms)^\tif quest\.pending is not None:\n.*?^\treturn None\n', ghost_src)
assert m_consume, "未提取到 __consume_nav pending 消费块"
consume_block = m_consume.group(0)

# quest_engine show_dialog 分支块
m_sd = re.search(
    r'(?ms)^\telif event_name == "show_dialog":\n(.*?)(?=^\telif event_name == )', engine_src)
assert m_sd, "未提取到 show_dialog 分支"
sd_block = m_sd.group(1)

# quest_engine tick pending 消费块
m_tick = re.search(
    r'(?ms)^\tif quest\.pending != None:\n.*?^\t\treturn __do_action\(robot_object, quest, p, now_ms\)\n',
    engine_src)
assert m_tick, "未提取到 quest_engine tick pending 消费块"
tick_block = m_tick.group(0)

# ================================================================ 静态断言
# S-A1: 看门狗优先用 quest.dialog_open_ms, 兜底 state_since_ms
check("S-A1 看门狗计时基准=quest.dialog_open_ms(含 state_since_ms 兜底)",
      'getattr(quest, "dialog_open_ms", 0)' in wd_block
      and 'getattr(g, "state_since_ms", 0)' in wd_block
      and "now_ms - g.state_since_ms > 30000" not in wd_block)

# S-A2: 同段对话只计一轮
check("S-A2 看门狗同段对话只计一轮(dlg_count_ms 守卫)",
      'getattr(g, "dlg_count_ms", 0)' in wd_block
      and "g.dlg_count_ms = _dlg_ms" in wd_block)

# S-A3: 30 秒常量抽出 + 关闭项分支重计时用 dialog_open_ms
check("S-A3 DIALOG_STUCK_MS 常量 + 关闭项分支重计对话计时",
      "DIALOG_STUCK_MS = 30000" in ghost_src
      and 'quest.dialog_open_ms = now_ms' in wd_block)

# S-A4: show_dialog 记录时刻 / close_dialog 复位
check("S-A4 show_dialog 记录 dialog_open_ms; close_dialog 复位+清 deferred",
      "quest.dialog_open_ms = __now_ms()" in sd_block
      and "quest.dialog_open_ms = 0" in engine_src
      and sd_block.count("__handle_dialog") >= 1)

# S-B1: 守卫处 deferred 标记(仅 walk/click)
check("S-B1 show_dialog 守卫: pending=walk/click 时记 deferred",
      'quest.dialog_deferred = (quest.pending.get("type")' in sd_block
      and 'in ("walk", "click")' in sd_block)

# S-B2: 两处 pending 消费后补处理
check("S-B2 tick 与 __consume_nav 均有 deferred 补处理",
      'getattr(quest, "dialog_deferred", False)' in tick_block
      and '__handle_dialog(robot_object, quest, quest.dialog)' in tick_block
      and 'getattr(quest, "dialog_deferred", False)' in consume_block
      and 'quest_engine.__handle_dialog(robot_object, quest, quest.dialog)' in consume_block)

# S-B3: 补处理在 pending 清空之后（保证不与本次动作抢点击）
i_clear = tick_block.find("quest.pending = None")
i_defer = tick_block.find('getattr(quest, "dialog_deferred"')
check("S-B3 tick 补处理位于 pending 清空之后", 0 <= i_clear < i_defer,
      "clear@%s defer@%s" % (i_clear, i_defer))

# S-C1: 空档宽限在 __start_next_hop 之前 + 打点存在
i_grace_call = grace_block.find("now_ms - _nav_last < NAV_IDLE_GRACE_MS")
i_next_hop = grace_block.find("__start_next_hop")
check("S-C1 READY 空档宽限位于 __start_next_hop 之前",
      "NAV_IDLE_GRACE_MS = 1500" in ghost_src
      and "g.nav_last_progress_ms = now_ms" in ghost_src
      and 0 <= i_grace_call < i_next_hop,
      "grace@%s nexthop@%s" % (i_grace_call, i_next_hop))


# ================================================================ 动态执行
def wrap_fn(name, body, params):
    """把 tab 缩进的代码块包成可执行函数（return 需在函数内）。"""
    lines = ["    " + l.replace("\t", "    ") if l.strip() else ""
             for l in body.splitlines()]
    code = "def %s(%s):\n%s\n" % (name, ", ".join(params), "\n".join(lines))
    return code


class FakeQuest(object):
    def __init__(self, now=1000000):
        self.dialog_open = True
        self.dialog = [0, 0, 13255, "", 0,
                       [["请送我去幽冥界（5银）", 0], ["离开", 1]]]
        self.dialog_open_ms = now
        self.dialog_deferred = False
        self.pending = None
        self.walk_target = None
        self.dijkstra_route = []
        self.state = 0
        self.active = True
        self.npc_cand_npc_id = 0
        self.npc_cand_positions = []
        self.npc_cand_idx = 0

    def set_state(self, state):
        self.state = state


class FakeG(object):
    def __init__(self):
        self.state_since_ms = 0
        self.dlg_close_tries = 0
        self.dlg_count_ms = 0
        self.rounds = 0
        self.token_shop_ctx = None
        self.token_fail = 0
        self.ready_stall_ts = 0
        self.nav_last_progress_ms = 0


def build_watchdog():
    """注入 fake 后返回看门狗可执行函数（真实源码块）。"""
    body = m_wd.group(0)
    ns = {
        "DIALOG_CLOSE_AFTER_MS": 8000, "DIALOG_CLOSE_TRIES": 2,
        "DIALOG_STUCK_MS": 30000, "TOKEN_FAIL_MAX": 3,
        "BROKER_NPC_ID": 10146,
        "pick_ghost_dialog_close": lambda opts: (
            0 if opts and len(opts) > 1 and opts[1][1] else None),
        "quest_engine": type("QE", (), {}),
        "__now_ms": lambda: 0,
    }
    calls = {"relogin": 0, "set_state": 0, "goto": 0, "log": [], "schedule": []}
    ns["__need_relogin"] = lambda *a, **k: calls.__setitem__("relogin", calls["relogin"] + 1)
    ns["__set_state"] = lambda g, s, n=None: (
        calls.__setitem__("set_state", calls["set_state"] + 1), setattr(g, "state", s))
    ns["__goto"] = lambda *a, **k: calls.__setitem__("goto", calls["goto"] + 1)
    ns["__log"] = lambda ro, lv, msg, *a: calls["log"].append(msg % a if a else msg)
    ns["__emit"] = lambda *a, **k: None
    ns["quest_engine"].__schedule = staticmethod(
        lambda q, p: (q.__setattr__("pending", p), calls["schedule"].append(p)))
    ns["quest_engine"].__human_delay = staticmethod(lambda ms: ms)
    code = wrap_fn("_wd", body, ["robot_object", "g", "quest", "now_ms"])
    exec(compile(code, "<watchdog>", "exec"), ns)
    return ns["_wd"], calls


WD, WD_CALLS = build_watchdog()


def run_wd(g, q, now):
    return WD("robot", g, q, now)


# ---------------- A1: 核心回归 —— 进 READY 已久 + 对话刚打开 → 不得误判
NOW = 5000000
g, q = FakeG(), FakeQuest(now=NOW)
g.state_since_ms = NOW - 100000          # 进 READY 已 100 秒（旧基准会立即判超时）
g.dlg_close_tries = 2                    # 关闭项已用尽, 排除 8 秒分支干扰
run_wd(g, q, NOW)
check("A1 进READY已100秒+对话刚打开 → 不误判超时(rounds=0, 无重登)",
      g.rounds == 0 and WD_CALLS["relogin"] == 0 and q.dialog_open is True,
      "rounds=%d relogin=%d" % (g.rounds, WD_CALLS["relogin"]))

# 反例: 旧基准表达式在同样输入下必然为"超时"
old_hit = (NOW - g.state_since_ms) > 30000
check("A1-反例 旧基准(now-state_since>30s)在同样输入下立即判超时 → 证明修复有效",
      old_hit is True and g.rounds == 0)

# ---------------- A2: 对话真的开了 31 秒 → 计一轮并关对话回 READY
NOW2 = NOW + 1000
g, q = FakeG(), FakeQuest(now=NOW2 - 31000)
g.state_since_ms = NOW2 - 100000
g.dlg_close_tries = 2
run_wd(g, q, NOW2)
check("A2 对话真实时长31秒 → 计1轮+关对话回READY(不重登)",
      g.rounds == 1 and q.dialog_open is False and WD_CALLS["relogin"] == 0
      and WD_CALLS["set_state"] == 1,
      "rounds=%d open=%s relogin=%d" % (g.rounds, q.dialog_open, WD_CALLS["relogin"]))

# ---------------- A3: 连续 tick（同段对话）不再累加 —— 修复前 7 tick = 7 轮重登
g, q = FakeG(), FakeQuest(now=NOW2 - 31000)
g.state_since_ms = NOW2 - 100000
g.dlg_close_tries = 2
for i in range(7):
    q.dialog_open = True          # 模拟服务端/业务把对话又挂回来（同段)
    q.dialog_open_ms = NOW2 - 31000 - i * 0
    run_wd(g, q, NOW2 + i * 1000)
check("A3 同段对话连续7 tick → rounds 仍=1, 无重登(修复前=7轮重登)",
      g.rounds == 1 and WD_CALLS["relogin"] == 0,
      "rounds=%d relogin=%d" % (g.rounds, WD_CALLS["relogin"]))

# 反例: 旧实现同输入（state_since 早 + 每 tick 累加）→ 7 轮
_old_rounds = sum(1 for i in range(7) if (NOW2 + i * 1000) - g.state_since_ms > 30000)
check("A3-反例 旧实现同样 7 tick 累加 7 轮(≥7 必重登) → 证明修复有效",
      _old_rounds == 7)

# ---------------- A4: 真实卡死（7 段不同对话各开满 30 秒）→ 仍会重登（兜底有效）
g = FakeG()
g.state_since_ms = NOW - 100000
relog = 0
for i in range(7):
    q = FakeQuest(now=NOW - 31000 - i * 1000)   # 每段对话打开时刻不同(真实重开对话)
    g.dlg_close_tries = 2
    before = WD_CALLS["relogin"]
    run_wd(g, q, NOW)
    if WD_CALLS["relogin"] > before:
        relog += 1
check("A4 7段不同对话各超30秒 → 第7段触发重登(真实卡死兜底保留)",
      g.rounds == 7 and relog == 1,
      "rounds=%d relogin=%d" % (g.rounds, relog))

# ---------------- A5: 8 秒关闭项分支（非业务对话）→ 点关闭 + 对话计时重计
g, q = FakeG(), FakeQuest(now=NOW - 9000)
g.state_since_ms = NOW - 100000
n_sched = len(WD_CALLS["schedule"])
run_wd(g, q, NOW)
check("A5 对话开9秒+有离开项 → 点关闭项收尾并重计对话计时",
      len(WD_CALLS["schedule"]) == n_sched + 1
      and q.dialog_open_ms == NOW and g.rounds == 0,
      "sched+%d open_ms=%s" % (len(WD_CALLS["schedule"]) - n_sched, q.dialog_open_ms))


# ---------------- B: show_dialog 守卫 deferred 标记 ----------------
QS = type("QS", (), {"ST_DIALOG": 2, "ST_IDLE": 0, "ST_WAIT_NEXT": 1})


def build_sd():
    body = m_sd.group(1)
    ns = {"quest_state": QS, "__now_ms": lambda: 123456}
    code = wrap_fn("_sd", body, ["quest", "robot_object", "data", "quest_state",
                                 "__handle_dialog", "__emit", "__emit_state"])
    exec(compile(code, "<show_dialog>", "exec"), ns)
    return ns["_sd"]


SD = build_sd()
WITH_OPTS = [0, 0, 13255, "", 0,
             [["请送我去幽冥界（5银）", 0], ["离开", 1]]]


def run_sd(pending, data):
    q = FakeQuest()
    q.state = 2
    q.pending = pending
    calls = {"handle": 0}
    SD(q, "robot", data, QS, lambda *a, **k: calls.__setitem__("handle", calls["handle"] + 1),
       lambda *a, **k: None, lambda *a, **k: None)
    return q, calls


P_WALK = {"type": "walk", "at_ms": 0, "data": {"npc_id": 13255}}
P_DCLICK = {"type": "dialog_click", "at_ms": 0, "data": {"option_index": 0}}

qa, ca = run_sd(P_WALK, WITH_OPTS)
check("B1 pending=walk+有选项对话 → 标记 deferred, 不抢着处理",
      qa.dialog_deferred is True and ca["handle"] == 0 and qa.dialog_open is True,
      "deferred=%s handle=%d" % (qa.dialog_deferred, ca["handle"]))

qb, cb = run_sd(P_DCLICK, WITH_OPTS)
check("B1b pending=dialog_click(就在点选项) → 不标记 deferred",
      qb.dialog_deferred is False and cb["handle"] == 0)

qc, cc = run_sd(None, WITH_OPTS)
check("B1c pending=None → 直接处理并清 deferred",
      qc.dialog_deferred is False and cc["handle"] == 1)

# ---------------- B2/B3: pending 消费后补处理（真实源码块）
def build_tick():
    ns = {"__do_action": None, "__handle_dialog": None, "__emit": lambda *a, **k: None}
    calls = {"do": 0, "handle": 0}
    ns["__do_action"] = lambda *a, **k: calls.__setitem__("do", calls["do"] + 1) or 0
    ns["__handle_dialog"] = lambda *a, **k: calls.__setitem__("handle", calls["handle"] + 1)
    code = wrap_fn("_tick", m_tick.group(0), ["robot_object", "quest", "now_ms"])
    exec(compile(code, "<tick_pending>", "exec"), ns)
    return ns["_tick"], calls


def build_consume():
    ns = {"quest_engine": type("QE", (), {}),
          "__log": lambda *a, **k: None}
    calls = {"do": 0, "handle": 0}
    ns["quest_engine"].__do_action = staticmethod(
        lambda *a, **k: calls.__setitem__("do", calls["do"] + 1) or 0)
    ns["quest_engine"].__handle_dialog = staticmethod(
        lambda *a, **k: calls.__setitem__("handle", calls["handle"] + 1))
    code = wrap_fn("_consume", consume_block, ["robot_object", "quest", "now_ms"])
    exec(compile(code, "<consume_nav>", "exec"), ns)
    return ns["_consume"], calls


TICK, TICK_CALLS = build_tick()
CONSUME, CONSUME_CALLS = build_consume()

q = FakeQuest(now=NOW)
q.pending = {"type": "walk", "at_ms": NOW - 1, "data": {}}
q.dialog_deferred = True
TICK("robot", q, NOW)
check("B2 tick: pending消费后补处理被跳过的对话(deferred 清空)",
      TICK_CALLS["handle"] == 1 and TICK_CALLS["do"] == 0
      and q.dialog_deferred is False,
      "handle=%d do=%d" % (TICK_CALLS["handle"], TICK_CALLS["do"]))

q2 = FakeQuest(now=NOW)
q2.pending = {"type": "walk", "at_ms": NOW - 1, "data": {}}
q2.dialog_deferred = False
TICK("robot", q2, NOW)
check("B2b tick: 无 deferred → 正常执行动作(不误补)",
      TICK_CALLS["do"] == 1 and TICK_CALLS["handle"] == 1)

q3 = FakeQuest(now=NOW)
q3.pending = {"type": "walk", "at_ms": NOW - 1, "data": {}}
q3.dialog_deferred = True
CONSUME("robot", q3, NOW)
check("B3 __consume_nav: 同款补处理",
      CONSUME_CALLS["handle"] == 1 and CONSUME_CALLS["do"] == 0
      and q3.dialog_deferred is False)

# ---------------- C: READY 空档宽限
def build_grace():
    ns = {"NAV_IDLE_GRACE_MS": 1500, "quest_engine": type("QE", (), {})}
    calls = {"next_hop": 0}
    ns["quest_engine"].__start_next_hop = staticmethod(
        lambda *a, **k: calls.__setitem__("next_hop", calls["next_hop"] + 1))
    code = wrap_fn("_grace", grace_block, ["robot_object", "g", "quest", "now_ms"])
    exec(compile(code, "<grace>", "exec"), ns)
    return ns["_grace"], calls


GRACE, GRACE_CALLS = build_grace()

g = FakeG()
g.nav_last_progress_ms = NOW - 500          # 0.5 秒前刚有推进(服务端推送可能在路上)
GRACE("robot", g, None, NOW)
check("C1 空档期(0.5s<1.5s) → 不重走同一跳",
      GRACE_CALLS["next_hop"] == 0)

g = FakeG()
g.nav_last_progress_ms = NOW - 3000         # 3 秒无推进 → 正常推进
GRACE("robot", g, None, NOW)
check("C2 非空档(3s>1.5s) → 正常 __start_next_hop",
      GRACE_CALLS["next_hop"] == 1)

g = FakeG()                                  # 首次进入(无记录) → 立即推进(与旧行为一致)
GRACE("robot", g, None, NOW)
check("C3 首次进入(无推进记录) → 立即推进, 不拖延",
      GRACE_CALLS["next_hop"] == 2)

# ================================================================ 结果
print(
)
print("自检目标: %s" % script_dir)
for f, s in ((ghost_path, ghost_src), (engine_path, engine_src)):
    print("  %s sha1=%s" % (os.path.basename(f),
                            hashlib.sha1(s.encode("utf-8")).hexdigest()[:12]))
total = 8 + 7 + 6 + 3
print("结果：%d 项，失败 %d 项" % (total, fails))
sys.exit(1 if fails else 0)
