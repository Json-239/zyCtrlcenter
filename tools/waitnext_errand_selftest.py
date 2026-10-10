# -*- coding: utf-8 -*-
"""errand 上下文不落 WAIT_NEXT 自检 —— 2026-10-08「47 号 STUCK_WAIT_NEXT 误杀」修复。

生产现象（2026-10-08，231 台 62 ERROR，其中 47 号）:
    err_msg = "状态 WAIT_NEXT 重试 4 次仍无进展"（STUCK_WAIT_NEXT → ST_ERROR）
链路（日志实证 1011/1039/1059/1140 同型）: 药品回补 auto_summon → shop_errand
（quest.active=True、chain_task_set=空集）→ 跨图赶路遇怪战斗 → 战斗收尾
（旧: ST_FIGHT → WAIT_NEXT）把 state 置成"等任务推送"死状态 → __recover 无从推进
→ **走路超时兜底**（共享 retry_count，不打序号=吃第 1 次）+ 状态看门狗 15s×3
= 60s 内判 STUCK_WAIT_NEXT（节拍: 走路超时+0s / 第2次+15s / 第3次+30s / STUCK+45s）。

修复（quest_engine.py）: 8 处"收尾状态"统一走 __env_rest_state（errand 在途 → ST_SHOP，
交采购自身重试/ST_SHOP 版 __recover 接管）；两处 STUCK 出口补 __abort_heal_buy_on_stuck
（释放商店锁，对齐 __report_stuck 的 2026-09-30 P1 收尾）。

本自检为源码级（不 import quest_engine：依赖机器人运行时）:
  S 静态: helper 存在 / 8 处 set_state 改走 helper / 旧写法清零 / 余留 2 处 ERROR 自愈
          保持 / 两处 STUCK 出口补收尾 / __report_stuck 原收尾未被误动。
  C 动态(提取真实源码直接执行):
    C1 __env_rest_state 判定矩阵（errand→SHOP / 普通链→WAIT_NEXT / 不活跃→IDLE /
       任务模式商店(无 errand 标记)→WAIT_NEXT）
    C2 回放 · 修复版: 战斗收尾→SHOP; 走路超时吃第 1 次后再跑看门狗 3 拍 → 无 error、
       recover 全部走采购(shop_ctx)，STUCK_WAIT_NEXT 不再出现。
    C2b 坏版复现(灵敏度): 把 helper 变异回旧式 "WAIT_NEXT if active else IDLE" 后同流程
       回放 → 必须复现 "状态 WAIT_NEXT 重试 4 次仍无进展"（走路超时+WAIT_NEXT 双吃计数）。
    C3 __abort_heal_buy_on_stuck: 调 heal_buy_abort(原因透传) / 依赖异常被吞。
    C4 STUCK 出口挂收尾: 回放里 STUCK_WAIT_NEXT 出口记录到 abort；走路超时
       STUCK_NAV 出口同样记录到 abort。

用法: python tools/waitnext_errand_selftest.py [quest_engine.py 路径]
      默认 = zyCtrlcenter/deploy/zones/prod-240-2300/script/quest_engine.py
      对**未打补丁**版本（如 .bak_20261008_map45rescue 或线上未应用版）: S1 即 FAIL。
"""
import io
import os
import re
import sys
import types

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_ENGINE = os.path.normpath(os.path.join(
    HERE, "..", "deploy", "zones", "prod-240-2300", "script", "quest_engine.py"))

path = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_ENGINE
src = io.open(path, encoding="utf-8").read()

SCRIPT_DIR = os.path.dirname(os.path.abspath(path))
sys.path.insert(0, SCRIPT_DIR)
import quest_state  # noqa: E402  纯枚举模块(只 import time), 离线可用

fails = 0


def check(name, ok, detail=""):
    global fails
    fails += 0 if ok else 1
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  " + detail) if detail else ""))


print("目标: %s" % path)

# ================================================================ 源码提取
def extract_def(name_text):
    m = re.search(r"(?ms)^def %s\(.*?(?=^\S)" % name_text, src)
    return m.group(0) if m else None


help_a = extract_def("__in_shop_errand")
help_b = extract_def("__env_rest_state")
help_c = extract_def("__abort_heal_buy_on_stuck")

m_wd = re.search(
    r"(?ms)^\t# 看门狗\n\tif quest\.timeout_exceeded\(\):\n.*?^\treturn 0\n", src)
wd_block = m_wd.group(0) if m_wd else None

m_wk = re.search(
    r"(?ms)^\tif quest\.walk_target != None and now_ms > quest\.walk_end_ms \+ 45000:\n.*?^\t\treturn 0\n",
    src)
wk_block = m_wk.group(0) if m_wk else None

# ================================================================ S 静态断言
check("S1-helpers 三个函数存在", bool(help_a and help_b and help_c),
      "in_shop=%s env_rest=%s abort=%s" % (
          bool(help_a), bool(help_b), bool(help_c)))

n_call = len(re.findall(r"set_state\(__env_rest_state\(quest\)\)", src))
check("S2 收尾状态 8 处改走 __env_rest_state", n_call == 8, "实际=%d" % n_call)

n_old_cond = len(re.findall(r"quest\.set_state\(quest_state\.ST_WAIT_NEXT if quest\.active", src))
n_old_rev = len(re.findall(r"quest\.set_state\(quest_state\.ST_IDLE if not quest\.active", src))
check("S3 旧写法(ST_WAIT_NEXT if active / ST_IDLE if not active)清零",
      n_old_cond == 0 and n_old_rev == 0, "cond=%d rev=%d" % (n_old_cond, n_old_rev))

n_bare3 = len(re.findall(r"\n\t\t\tquest\.set_state\(quest_state\.ST_WAIT_NEXT\)\n", src))
n_bare4 = len(re.findall(r"\n\t\t\t\tquest\.set_state\(quest_state\.ST_WAIT_NEXT\)\n", src))
check("S4 仅余 2 处 ERROR 自愈裸置 WAIT_NEXT(add_task/update_task), 4 缩进清零",
      n_bare3 == 2 and n_bare4 == 0, "bare3=%d bare4=%d" % (n_bare3, n_bare4))

n_abort_call = len(re.findall(r"(?<!def )__abort_heal_buy_on_stuck\(robot_object,", src))
check("S5 两处 STUCK 出口挂 __abort_heal_buy_on_stuck", n_abort_call == 2,
      "call=%d" % n_abort_call)

check("S6 __report_stuck 原 heal_buy 收尾未被误动",
      "_as.heal_buy_abort(robot_object, \"stuck: %s\" % (reason or \"\"))" in src)

check("S7 看门狗与走路超时块可提取", bool(wd_block and wk_block),
      "wd=%s wk=%s" % (bool(wd_block), bool(wk_block)))

HAS_FIX = bool(help_a and help_b and help_c and n_call == 8)
if not HAS_FIX:
    print("---- 未打补丁版本：动态用例按预期 FAIL（坏版灵敏度） ----")

# ================================================================ 动态执行环境
EV = []          # __emit 事件
REC = []         # __recover 调用记录
ABORT = []       # heal_buy_abort 记录
ADD = []         # __maybe_addtask_start 记录
NOW = [0.0]      # 模拟时钟(ms)


def _emit(ro, ev):
    EV.append(ev)


def _recover(ro, q):
    REC.append({"state": q.state, "errand": bool(getattr(q, "shop_ctx", None))})


def _addtask(ro, q):
    ADD.append(q.state)


# auto_summon stub（供 __abort_heal_buy_on_stuck 内 import）
MOD = {"raise": False}


def _heal_buy_abort(ro, why=""):
    if MOD["raise"]:
        raise RuntimeError("boom")
    ABORT.append(why)


def _install_stubs():
    mod = types.ModuleType("auto_summon")
    mod.heal_buy_abort = _heal_buy_abort
    sys.modules["auto_summon"] = mod


_install_stubs()

QUEST_MAX_RETRY = 3


class Cfg(object):
    quest_max_retry = QUEST_MAX_RETRY


cfg = Cfg()


class FakeQuest(object):
    def __init__(self, state, active=True, errand=False, chain_id=None):
        self.state = state
        self.state_since_ms = NOW[0]
        self.retry_count = 0
        self.walk_target = None
        self.walk_end_ms = 0
        self.walk_pending_hop = None
        self.walk_pending_click = None
        self.timeout_override_ms = 0
        self.active_task_index = 0
        self.active = active
        self.chain_id = chain_id if chain_id is not None else ("shop_errand" if errand else "ghost")
        self.shop_ctx = {"errand": True} if errand else None
        self.dialog_open = False
        self.done_base = 0
        self.addtask_sent = False
        self.last_error = None

    def set_state(self, s):
        if s != self.state:
            self.state = s
            self.state_since_ms = NOW[0]
            self.retry_count = 0

    def timeout_exceeded(self):
        t = quest_state.STATE_TIMEOUT_MS.get(self.state)
        if t is None:
            return False
        if self.timeout_override_ms > t:
            t = self.timeout_override_ms
        return NOW[0] - self.state_since_ms > t

    def mark_error(self, code, msg=""):
        self.last_error = (code, msg)


class FakeRobot(object):
    m_mapid = 1
    m_pose = (0, 0)
    m_fight_state = False


def wrap(block_text, fn_name, extra_names):
    lines = ["    " + l.replace("\t", "    ") if l.strip() else ""
             for l in block_text.splitlines()]
    args = ", ".join(extra_names)
    code = ("def %s(%s):\n" % (fn_name, args) + "\n".join(lines) + "\n")
    ns = {}
    exec(compile(code, "<%s>" % fn_name, "exec"), ns)
    return ns[fn_name]


_names_common = ["quest", "robot_object", "now_ms", "config", "quest_state",
                 "__emit", "__recover", "__abort_heal_buy_on_stuck"]
walk_fn = wrap(wk_block, "_walk_block", _names_common) if wk_block else None
wd_fn = wrap(wd_block, "_wd_block", _names_common + ["__maybe_addtask_start"]) if wd_block else None


def _ns_exec(fn_src, fn_name):
    if not fn_src:
        return None
    code = fn_src
    ns = {"quest_state": quest_state}
    exec(compile(code, "<%s>" % fn_name, "exec"), ns)
    return ns[fn_name]


env_rest = None
if help_a and help_b:
    # 两个 helper 同命名空间执行（__env_rest_state 依赖 __in_shop_errand）
    _ns2 = {"quest_state": quest_state}
    exec(compile(help_a + "\n\n" + help_b, "<env_rest>", "exec"), _ns2)
    env_rest = _ns2["__env_rest_state"]
abort_fn = _ns_exec(help_c, "__abort_heal_buy_on_stuck") if help_c else None

# 坏版 helper（变异: 删掉 errand 分支 = 修复前行为）
BAD_HELPER_SRC = (
    "def __env_rest_state(quest):\n"
    "    return quest_state.ST_WAIT_NEXT if quest.active else quest_state.ST_IDLE\n")
bad_env_rest = _ns_exec(BAD_HELPER_SRC, "__env_rest_state")

NAMES = dict(quest_state=quest_state, config=cfg, __emit=_emit, __recover=_recover,
             __abort_heal_buy_on_stuck=abort_fn, __maybe_addtask_start=_addtask)


def run_walk(q, now_ms):
    walk_fn(q, FakeRobot(), now_ms, cfg, quest_state, _emit, _recover, abort_fn)


def run_wd(q, now_ms):
    wd_fn(q, FakeRobot(), now_ms, cfg, quest_state, _emit, _recover, abort_fn, _addtask)


def errors():
    return [e for e in EV if e.get("type") == "error"]


# ================================================================ C1 判定矩阵
if env_rest:
    try:
        q1 = FakeQuest(quest_state.ST_FIGHT, active=True, errand=True)
        r1 = env_rest(q1)
        q2 = FakeQuest(quest_state.ST_FIGHT, active=True, errand=False)
        r2 = env_rest(q2)
        q3 = FakeQuest(quest_state.ST_FIGHT, active=False, errand=False)
        r3 = env_rest(q3)
        q4 = FakeQuest(quest_state.ST_FIGHT, active=True, errand=False, chain_id="shop_errand")
        r4 = env_rest(q4)
        q5 = FakeQuest(quest_state.ST_DIALOG, active=True, errand=False)
        q5.shop_ctx = {"item_index": 1}   # 任务模式商店(无 errand 标记)
        r5 = env_rest(q5)
        check("C1a errand 在途 → ST_SHOP", r1 == quest_state.ST_SHOP, "got=%s" % r1)
        check("C1b 普通任务链 → ST_WAIT_NEXT", r2 == quest_state.ST_WAIT_NEXT, "got=%s" % r2)
        check("C1c 不活跃 → ST_IDLE", r3 == quest_state.ST_IDLE, "got=%s" % r3)
        check("C1d chain_id=shop_errand（无 ctx）也判 errand → ST_SHOP",
              r4 == quest_state.ST_SHOP, "got=%s" % r4)
        check("C1e 任务模式商店(shop_ctx 无 errand 标记) → ST_WAIT_NEXT",
              r5 == quest_state.ST_WAIT_NEXT, "got=%s" % r5)
    except Exception as e:
        check("C1 判定矩阵", False, "异常 %r" % e)
else:
    check("C1a~e 判定矩阵", False, "helper 缺失(未打补丁)")


# ================================================================ C2/C2b 回放
def replay(env_rest_fn, tag):
    """事故回放: 战斗收尾→(env_rest)→走路超时(吃#1)→看门狗 t+15/30/45。

    返回 dict(errors=..., retry=..., rec=..., state=...)。"""
    del EV[:], REC[:], ABORT[:]
    NOW[0] = 1_000_000.0
    q = FakeQuest(quest_state.ST_FIGHT, active=True, errand=True)
    # ① 战斗进行中由 tick 置 ST_FIGHT；战斗结束事件走收尾状态:
    q.set_state(env_rest_fn(q))
    st_after_fight = q.state
    # ② 走路超时兜底: walk_target 已 45s 未达
    q.walk_target = (968, 904)
    q.walk_end_ms = NOW[0] - 46000
    run_walk(q, NOW[0])
    retry_after_walk = q.retry_count
    # ③ 状态看门狗 3 拍（~15s 节拍 + 少量 tick 抖动，与现场日志一致；
    #    timeout_exceeded 是严格 ">"，精确 15.000s 不触发）
    for step in (15.5, 31.0, 46.5):
        NOW[0] = 1_000_000.0 + step * 1000.0
        run_wd(q, NOW[0])
    return {
        "tag": tag, "st_after_fight": st_after_fight,
        "retry_after_walk": retry_after_walk, "retry": q.retry_count,
        "state": q.state, "errors": [dict(e) for e in errors()],
        "rec": [dict(r) for r in REC], "abort": list(ABORT),
    }


if walk_fn and wd_fn and env_rest:
    r_fix = replay(env_rest, "fixed")
    e_fix = [e for e in r_fix["errors"] if str(e.get("code", "")).startswith("STUCK_")]
    check("C2a 修复版: 战斗收尾后 state=ST_SHOP（不落 WAIT_NEXT）",
          r_fix["st_after_fight"] == quest_state.ST_SHOP, "got=%s" % r_fix["st_after_fight"])
    check("C2b 修复版: 走路超时吃掉第 1 次计数", r_fix["retry_after_walk"] == 1,
          "retry=%s" % r_fix["retry_after_walk"])
    check("C2c 修复版: 3 拍看门狗后无 STUCK_* 误杀（<4 计数, 走采购重试）",
          not e_fix and r_fix["retry"] < 4,
          "retry=%s errors=%s" % (r_fix["retry"], [e.get("code") for e in r_fix["errors"]]))
    check("C2d 修复版: __recover 全部走采购(shop_ctx)分支",
          len(r_fix["rec"]) >= 1 and all(x["errand"] for x in r_fix["rec"]),
          "rec=%s" % r_fix["rec"])

    r_bad = replay(bad_env_rest, "bad")
    e_bad = [e for e in r_bad["errors"] if e.get("code") == "STUCK_WAIT_NEXT"]
    msg_hit = bool(e_bad) and ("状态 WAIT_NEXT 重试 4 次仍无进展" in str(e_bad[0].get("msg", "")))
    # 注: STUCK 出口 set_state(ST_ERROR) 会按真实语义把 retry_count 归零,
    #     所以"4 次"以报错文案为准（脚本内核同样如此）。
    check("C2b-bad 坏版复现: 走路超时(吃#1)+WAIT_NEXT 15s×3 → STUCK_WAIT_NEXT 重试 4 次",
          r_bad["st_after_fight"] == quest_state.ST_WAIT_NEXT and msg_hit
          and r_bad["retry_after_walk"] == 1
          and r_bad["state"] == quest_state.ST_ERROR,
          "st=%s walk#1=%s end=%s msg=%s" % (
              r_bad["st_after_fight"], r_bad["retry_after_walk"], r_bad["state"],
              (e_bad[0].get("msg") if e_bad else None)))
    check("C4 STUCK_WAIT_NEXT 出口已挂 heal_buy 收尾（abort 被调）",
          len(r_bad["abort"]) >= 1, "abort=%s" % r_bad["abort"])
else:
    check("C2a~d 事故回放(修复版)", False, "helper/块缺失(未打补丁)")
    check("C2b-bad 坏版复现", False, "见 C2 说明")
    check("C4 出口收尾", False, "见 C2 说明")

# ================================================================ C3 abort helper
if abort_fn:
    try:
        del ABORT[:]
        abort_fn(FakeRobot(), "stuck: 测试")
        ok1 = ABORT == ["stuck: 测试"]
        MOD["raise"] = True
        abort_fn(FakeRobot(), "stuck: 异常吞掉")
        MOD["raise"] = False
        check("C3 __abort_heal_buy_on_stuck 透传原因且依赖异常被吞", ok1,
              "abort=%s" % ABORT)
    except Exception as e:
        check("C3 __abort_heal_buy_on_stuck", False, "异常 %r" % e)
else:
    check("C3 __abort_heal_buy_on_stuck", False, "helper 缺失(未打补丁)")

# ================================================================ C5 走路超时 STUCK_NAV 出口
if walk_fn:
    del EV[:], ABORT[:]
    NOW[0] = 2_000_000.0
    q = FakeQuest(quest_state.ST_NAV, active=True, errand=False)
    q.chain_id = "ghost"
    q.retry_count = QUEST_MAX_RETRY  # 本次 +1 → 4 > 3 → STUCK_NAV
    q.walk_target = (1, 1)
    q.walk_end_ms = NOW[0] - 46000
    run_walk(q, NOW[0])
    nav_err = [e for e in errors() if e.get("code") == "STUCK_NAV"]
    check("C5 走路超时 STUCK_NAV 出口已挂 heal_buy 收尾",
          bool(nav_err) and len(ABORT) >= 1,
          "err=%s abort=%s" % ([e.get("code") for e in errors()], ABORT))
else:
    check("C5 走路超时 STUCK_NAV 出口", False, "块缺失(未打补丁)")

# ================================================================ 汇总
print()
print("---- 汇总: %s ----" % ("全部 PASS" if fails == 0 else "%d 项 FAIL" % fails))
sys.exit(1 if fails else 0)
