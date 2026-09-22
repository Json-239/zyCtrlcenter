# -*- coding: utf-8 -*-
"""钟馗空菜单「退避重试优先、重登降级为最后手段」自检（2026-09-21 修复）。

生产现象（19:34 现场，正在扩散）：活跃抓鬼会话 80 → 45；stuck_count>=3 的号
12 → 29 个（全 IDLE）。19:20 后日志 `钟馗菜单无可动作项(仅说明/离开), 重登恢复`
369 次 —— daily_ghost 空菜单 B 路径（需买令但菜单无"领取助战令/捉鬼"项）立即
__need_relogin，中控当日卡死计数快速累加，达 3 触发 churn 防护 → 不再自动重登/
补发 → 号停摆。而空菜单多为偶发（早间 43 个同类号重登救活后正常跑了数小时）。

修复（本文件校验的就是上线那份源码）：
  1) 空菜单未达下线阈值：关对话 + 设 no_action_retry_at = now + 退避
     （第 1 次 5s、第 2 次 10s）+ 回 READY；不再立即重登。
  2) READY「找钟馗接取」前加退避判定：未到期原地等（return 0），不点钟馗。
  3) 退避重试上限 BROKER_EMPTY_RETRY_MAX=2；用尽才走 __need_relogin 兜底。
  4) 拿到正常菜单/接到任务 → 清空退避与重试计数。
  5) no_action_rounds（"没钱/没令 → 3 次下线换号"）语义保持不变，两套计数分开。

本脚本为**源码级自检**（不 import daily_ghost：它依赖机器人运行时 cnetwork 等）：
  - 静态断言：函数/常量存在、调用顺序、清零点覆盖、旧计数未被污染；
  - 动态断言：提取真实源码块直接执行 —— 空菜单分支（A/B 两条路径）、READY 退避
    守卫、重试序列、清零分支，另有反例（删掉退避调用 → 复现修复前"立即重登/立即重点"）。

用法：python broker_empty_backoff_selftest.py [daily_ghost.py 路径]
      不带参数默认校验仓库副本；传线上副本路径可再验一次线上文件。
"""
import hashlib
import os
import re
import sys
import textwrap

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_GHOST = os.path.normpath(os.path.join(
    HERE, "..", "deploy", "zones", "prod-240-2300", "script", "daily_ghost.py"))

path = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_GHOST
src = open(path, encoding="utf-8").read()

fails = 0


def check(name, ok, detail=""):
    global fails
    fails += 0 if ok else 1
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  " + detail) if detail else ""))


# ---------------------------------------------------------------- 源码提取
CONSTS = ["BROKER_EMPTY_RETRY_MAX", "BROKER_EMPTY_BACKOFF_MS",
          "BROKER_NPC_ID", "TOKEN_FAIL_MAX", "NO_ACTION_OFFLINE_LIMIT"]
FUNCS = ["__clear_broker_empty_backoff", "__broker_empty_waiting",
         "__start_broker_empty_backoff", "__set_state"]

parts = []
for name in CONSTS:
    m = re.search(r'(?m)^%s\s*=.*$' % re.escape(name), src)
    assert m, "缺少常量 %s" % name
    parts.append(m.group(0))
for fn in FUNCS:
    m = re.search(r'(?ms)^def %s\(.*?(?=^\S)' % re.escape(fn), src)
    assert m, "缺少函数 %s" % fn
    parts.append(m.group(0))

# 空菜单分支（on_show_dialog：first is None 且 npc 为钟馗）
m_empty = re.search(
    r'(?ms)^\t\tif first is None and npc_id == BROKER_NPC_ID:\n(.*?)(?=^\tif first is not None:)',
    src)
assert m_empty, "未找到空菜单分支（缩进/写法变了?）"
empty_block = m_empty.group(1)

# 「拿到可动作项」分支（清零调用点）
m_first = re.search(r'(?ms)^\tif first is not None:\n(.*?)(?=^\telse:)', src)
assert m_first, "未找到 first is not None 分支"
first_block = m_first.group(1)

# READY 退避守卫（两行真实源码）
GUARD_PAT = r'(?m)^\t\tif __broker_empty_waiting\(g, now_ms\):\n\t\t\treturn 0\n'
m_guard = re.search(GUARD_PAT, src)
assert m_guard, "未找到 READY 退避守卫（缩进/写法变了?）"
guard_block = m_guard.group(0)

ns_exec = {}


def indent_body(text, extra="\t"):
    """整体平移缩进：源码块（n 层 tab）包进函数体（+1 层）。"""
    return "\n".join((extra + l) if l.strip() else "" for l in text.splitlines())


# 真实 __now_ms / __log / __set_state 由测试注入；其余提取的真实函数留在 ns_exec
exec(compile("\n".join(parts), "<daily_ghost excerpt>", "exec"), ns_exec)

CLOCK = [10 ** 12]          # 可控毫秒时钟


def now_fn():
    return CLOCK[0]


LOGS = []


def log_fn(robot_object, level, msg):
    LOGS.append((level, msg))


ns_exec["__now_ms"] = now_fn
ns_exec["__log"] = log_fn
real_backoff = ns_exec["__start_broker_empty_backoff"]
real_waiting = ns_exec["__broker_empty_waiting"]
real_clear = ns_exec["__clear_broker_empty_backoff"]


# ---------------------------------------------------------------- 静态断言
check("S1 常量: RETRY_MAX=2 / BACKOFF=(5000, 10000)（2026-09-22 缩短：闲聊菜单=当日不可用，快速二连判下线）",
      ns_exec["BROKER_EMPTY_RETRY_MAX"] == 2 and
      ns_exec["BROKER_EMPTY_BACKOFF_MS"] == (5 * 1000, 10 * 1000),
      "max=%s backoff=%s" % (ns_exec["BROKER_EMPTY_RETRY_MAX"],
                             ns_exec["BROKER_EMPTY_BACKOFF_MS"]))

# S2: 空菜单 A(4 tab)/B(3 tab) 两条路径都先退避
calls_a = empty_block.count("if __start_broker_empty_backoff(robot_object, g, quest):")
check("S2 空菜单分支两处调用 __start_broker_empty_backoff", calls_a == 2,
      "调用数=%d" % calls_a)

# S3: B 路径的 __need_relogin 在退避之后（重登是最后手段）
i_bf, i_rl = empty_block.rfind("__start_broker_empty_backoff"), empty_block.rfind("__need_relogin(")
check("S3 B 路径 __need_relogin 位于退避调用之后", 0 <= i_bf < i_rl,
      "bf@%s relogin@%s" % (i_bf, i_rl))

# S4: READY 守卫在「找钟馗接取」之后、__goto 钟馗之前
i_sec = src.find("# 没有进行中任务: 找钟馗接取")
i_gd = src.find("if __broker_empty_waiting(g, now_ms):")
i_go = src.find("__goto(robot_object, g, BROKER_NPC_ID, BROKER_NPC_ID, 0, 500)", i_sec)
check("S4 READY 守卫位于找钟馗接取与 __goto 之间",
      0 <= i_sec < i_gd < i_go,
      "sec@%s guard@%s goto@%s" % (i_sec, i_gd, i_go))

# S5: 清零调用点覆盖（自愈/新会话/TASK_LOAD/任务派发/已有任务菜单/拿到可动作项）
n_clear = src.count("__clear_broker_empty_backoff(g)")
check("S5 清零调用点 >= 6 处", n_clear >= 6, "count=%d" % n_clear)

# S6: no_action_rounds 既有语义未动（仍 +1 计到 NO_ACTION_OFFLINE_LIMIT）
m_note = re.search(r'(?ms)^def __note_no_action\(.*?(?=^\S)', src)
note_src = m_note.group(0)
check("S6 no_action_rounds 计数/上限逻辑保持原样",
      'g.no_action_rounds = int(getattr(g, "no_action_rounds", 0)) + 1' in note_src
      and "NO_ACTION_OFFLINE_LIMIT" in note_src,
      "note 函数内 no_action_rounds/上限判定完整")

# S7: 空菜单分支不读写 no_action_rounds（两套计数分开）
check("S7 空菜单分支未改动 no_action_rounds",
      "no_action_rounds =" not in empty_block and 'getattr(g, "no_action_rounds"' not in empty_block)


# ---------------------------------------------------------------- 动态执行
class FakeG(object):
    def __init__(self, **kw):
        self.state = "ACCEPT"
        self.state_since_ms = 0
        self.no_action_rounds = 0
        self.no_action_retry = 0
        self.no_action_retry_at = 0
        self.token_no_money = False
        self.token_fail = 0
        for k, v in kw.items():
            setattr(self, k, v)


class FakeQuest(object):
    def __init__(self):
        self.dialog_open = True
        self.dialog = ["stale-dialog"]
        self.pending = {"stale": 1}
        self.walk_target = (24, 100, 100)
        self.dijkstra_route = [{"target_map": 24}]


class FakeEngine(object):
    def __init__(self):
        self.scheduled = []

    def _schedule(self, quest, action):
        self.scheduled.append(action)

    def _human_delay(self, ms):
        return 0


# 源码里调用 quest_engine.__schedule/__human_delay；模块级赋值不触发名字改写
FakeEngine.__schedule = FakeEngine._schedule
FakeEngine.__human_delay = FakeEngine._human_delay


def build_empty_branch(block_text):
    """包成函数执行真实分支源码；依赖全部以参数注入（含命令式私有名）。"""
    code = ("def _empty(first, npc_id, need_token, g, quest, robot_object,\n"
            "           BROKER_NPC_ID, TOKEN_FAIL_MAX, __note_no_action,\n"
            "           __start_broker_empty_backoff, __need_relogin,\n"
            "           __set_state, __now_ms, __goto):\n"
            + indent_body(block_text) + "\n")
    ns = {}
    exec(compile(code, "<empty branch>", "exec"), ns)
    return ns["_empty"]


def build_first_branch(block_text):
    code = ("def _first(first, npc_id, g, quest, quest_engine,\n"
            "           __clear_broker_empty_backoff, __set_state, __now_ms, BROKER_NPC_ID):\n"
            + indent_body(block_text) + "\n")
    ns = {}
    exec(compile(code, "<first branch>", "exec"), ns)
    return ns["_first"]


def build_ready_guard(block_text):
    """真实守卫两行 + 一个"继续去钟馗"的 __goto（用于验证退避期内不发点击）。"""
    body = indent_body(textwrap.dedent(block_text))
    code = ("def _guard(g, now_ms, __broker_empty_waiting, __goto):\n"
            + body + "\n\t__goto()\n\treturn 1\n")
    ns = {}
    exec(compile(code, "<ready guard>", "exec"), ns)
    return ns["_guard"]


empty_branch = build_empty_branch(empty_block)
first_branch = build_first_branch(first_block)
ready_guard = build_ready_guard(guard_block)

# 反例：删掉两处退避调用 → 还原修复前行为（A 立即重点 / B 立即重登）
old_empty_block = re.sub(
    r'(?m)^\t\t\t+if __start_broker_empty_backoff\(robot_object, g, quest\):\n\t\t\t+return True\n',
    "", empty_block)
old_empty_branch = build_empty_branch(old_empty_block)


def run_empty(branch_fn, need_token, g=None, empty_note_ret=False):
    g = g if g is not None else FakeG()
    quest = FakeQuest()
    calls = {"note": 0, "relogin": 0, "goto": 0}
    notes = []

    def note_no_action(*a, **k):
        calls["note"] += 1
        return empty_note_ret

    def need_relogin(*a, **k):
        calls["relogin"] += 1
        return True

    def goto(*a, **k):
        calls["goto"] += 1

    ret = branch_fn(None, ns_exec["BROKER_NPC_ID"], need_token, g, quest, "robot",
                    ns_exec["BROKER_NPC_ID"], ns_exec["TOKEN_FAIL_MAX"],
                    note_no_action, real_backoff, need_relogin,
                    ns_exec["__set_state"], now_fn, goto)
    return ret, g, quest, calls, notes


# C1: 空菜单第 1 次(A 路径: 没钱/没令, 未达下线阈值) → 退避 5s, 不重登/不立即重点
del LOGS[:]
CLOCK[0] = 10 ** 12
ret, g, quest, calls, _ = run_empty(empty_branch, need_token=False)
ok = (ret is True and calls["relogin"] == 0 and calls["goto"] == 0
      and g.no_action_retry == 1
      and g.no_action_retry_at == CLOCK[0] + 5 * 1000
      and g.state == "READY"
      and quest.dialog_open is False and quest.dialog is None
      and quest.pending is None and quest.walk_target is None
      and quest.dijkstra_route == []
      and any("退避 5s 后重试(第 1/2 次)" in m for _l, m in LOGS))
check("C1 空菜单第1次(A路径) → 退避5s/回READY/不重登不发点击", ok,
      "ret=%s relogin=%d goto=%d retry=%s at=%s state=%s log=%s" % (
          ret, calls["relogin"], calls["goto"], g.no_action_retry,
          g.no_action_retry_at, g.state, [m for _l, m in LOGS][-1:]))

# C1b: B 路径(需买令但菜单无领令/接取项, 修复前立即重登) → 同样先退避
del LOGS[:]
CLOCK[0] = 10 ** 12
ret, g, quest, calls, _ = run_empty(empty_branch, need_token=True)
ok = (ret is True and calls["relogin"] == 0 and calls["goto"] == 0
      and g.no_action_retry == 1
      and g.no_action_retry_at == CLOCK[0] + 5 * 1000
      and g.state == "READY"
      and any("退避 5s 后重试" in m for _l, m in LOGS))
check("C1b 空菜单第1次(B路径) → 退避5s/不重登(修复前此处立即重登)", ok,
      "ret=%s relogin=%d retry=%s log=%s" % (
          ret, calls["relogin"], g.no_action_retry, [m for _l, m in LOGS][-1:]))

# C2: 退避期内 READY 守卫原地等（不发 __goto）；到期/无退避则继续去接取
g_wait = FakeG(no_action_retry_at=CLOCK[0] + 30000)
g_due = FakeG(no_action_retry_at=CLOCK[0] - 1)
g_none = FakeG()
gotos = {"n": 0}


def goto_fn(*a, **k):
    gotos["n"] += 1


r_wait = ready_guard(g_wait, CLOCK[0], real_waiting, goto_fn)
n_after_wait = gotos["n"]
r_due = ready_guard(g_due, CLOCK[0], real_waiting, goto_fn)
r_none = ready_guard(g_none, CLOCK[0], real_waiting, goto_fn)
ok = (r_wait == 0 and n_after_wait == 0
      and r_due == 1 and r_none == 1 and gotos["n"] == 2
      and real_waiting(g_wait, CLOCK[0]) is True
      and real_waiting(g_due, CLOCK[0]) is False
      and real_waiting(g_none, CLOCK[0]) is False)
check("C2 退避未到期 → READY 原地等(不发 __goto); 到期/无退避 → 正常去接取", ok,
      "wait_ret=%s wait_goto=%d due_ret=%s none_ret=%s" % (
          r_wait, n_after_wait, r_due, r_none))

# C3: 重试序列 5s → 10s → 用尽; B 路径用尽 → 走 __need_relogin 兜底
CLOCK[0] = 10 ** 12
g = FakeG()
ok_seq = []
for _i in range(3):
    _t = real_backoff("robot", g, FakeQuest())
    ok_seq.append((_t, g.no_action_retry, g.no_action_retry_at - CLOCK[0]))
ok_seq_ok = (ok_seq[0] == (True, 1, 5 * 1000)
             and ok_seq[1] == (True, 2, 10 * 1000)
             and ok_seq[2] == (False, 2, 10 * 1000))
check("C3a 退避序列: 5s → 10s → 用尽(返回 False)", ok_seq_ok, "seq=%s" % (ok_seq,))

del LOGS[:]
g2 = FakeG(no_action_retry=2)
ret, g2, quest2, calls2, _ = run_empty(empty_branch, need_token=True, g=g2)
ok = (ret is True and calls2["relogin"] == 1 and g2.no_action_retry == 2)
check("C3b 退避用尽(B路径) → 调用 __need_relogin 兜底", ok,
      "ret=%s relogin=%d retry=%s" % (ret, calls2["relogin"], g2.no_action_retry))
# 重登 reason 文案（从真实调用参数里取）
reason_seen = []


def run_empty_reason(branch_fn, g):
    quest = FakeQuest()

    def need_relogin(robot_object, gg, reason):
        reason_seen.append(reason)
        return True

    def note_no_action(*a, **k):
        return False

    return branch_fn(None, ns_exec["BROKER_NPC_ID"], True, g, quest, "robot",
                     ns_exec["BROKER_NPC_ID"], ns_exec["TOKEN_FAIL_MAX"],
                     note_no_action, real_backoff, need_relogin,
                     ns_exec["__set_state"], now_fn, lambda *a, **k: None)


run_empty_reason(empty_branch, FakeG(no_action_retry=2))
check("C3c 重登 reason 含「钟馗空菜单重试仍无动作项」",
      bool(reason_seen) and "钟馗空菜单重试仍无动作项" in reason_seen[-1],
      "reason=%s" % (reason_seen[-1:] or ["<无>"]))

# C4: 拿到有效菜单/接到任务 → 清零
g3 = FakeG(no_action_retry=2, no_action_retry_at=CLOCK[0] + 99999)
real_clear(g3)
ok_clear = (g3.no_action_retry == 0 and g3.no_action_retry_at == 0)

g4 = FakeG(no_action_retry=2, no_action_retry_at=CLOCK[0] + 99999)
eng = FakeEngine()
first_branch(0, ns_exec["BROKER_NPC_ID"], g4, FakeQuest(), eng, real_clear,
             ns_exec["__set_state"], now_fn, ns_exec["BROKER_NPC_ID"])
ok_first = (g4.no_action_retry == 0 and g4.no_action_retry_at == 0
            and len(eng.scheduled) == 1
            and eng.scheduled[0]["data"]["option_index"] == 0
            and g4.state == "ACCEPT")

g5 = FakeG(no_action_retry=2, no_action_retry_at=CLOCK[0] + 99999)
eng5 = FakeEngine()
first_branch(0, 999, g5, FakeQuest(), eng5, real_clear, ns_exec["__set_state"], now_fn,
             ns_exec["BROKER_NPC_ID"])
ok_nonbroker = (g5.no_action_retry == 2)
check("C4 拿到有效菜单 → 计数清零(钟馗); 非钟馗不清零", ok_clear and ok_first and ok_nonbroker,
      "clear=%s first_retry=%s scheduled=%d nonbroker_retry=%s" % (
          ok_clear, g4.no_action_retry, len(eng.scheduled), g5.no_action_retry))

# C5(反例): 删掉退避调用 → 复现修复前行为(A 立即重点钟馗 / B 立即重登)
ret_a, _ga, _qa, calls_a2, _ = run_empty(old_empty_branch, need_token=False)
ok_a = (calls_a2["goto"] == 1 and calls_a2["relogin"] == 0 and calls_a2["note"] == 1)
ret_b, _gb, _qb, calls_b2, _ = run_empty(old_empty_branch, need_token=True)
ok_b = (calls_b2["relogin"] == 1 and calls_b2["goto"] == 0)
check("C5 反例: 无退避调用 → A 立即重点(goto=1) / B 立即重登(relogin=1)", ok_a and ok_b,
      "A: goto=%d relogin=%d | B: goto=%d relogin=%d" % (
          calls_a2["goto"], calls_a2["relogin"], calls_b2["goto"], calls_b2["relogin"]))

# ---------------------------------------------------------------- 结果
print("\n自检目标: %s" % path)
print("sha1=%s" % hashlib.sha1(src.encode("utf-8")).hexdigest()[:12])
total = 7 + 8
print("结果：%d 项，失败 %d 项" % (total, fails))
sys.exit(1 if fails else 0)
