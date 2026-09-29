# -*- coding: utf-8 -*-
"""切换让路保护自检 —— 2026-09-29（random_walk + daily_ghost，真 import + 真跑 dispatch）。

用户口径（原文）:
  "可以做个保护，不管是下发什么或者游荡，在切换下一件时候一定是当前现有先做完，
   比如当前已经在抓鬼路上了，然后我让他去游荡，那也要先把这只鬼抓完去提交，
   才开始去游荡。"

机制:
  · random_walk.dispatch_cmd 遇抓鬼"在途工作单元"(ACCEPT/NAV/FIGHT/SUBMIT/WAIT_NEXT/
    HEAL, 或战斗中) → **不打断**, 指令挂起(robot_object.m_roam_switch_pending),
    回 {"result":"queued"}; 空等(READY/WAIT_GHOST)与 ERROR/DONE 不排队 → 直接切换;
  · 触发点(daily_ghost.py): ① 交付任务 2019511 FINISH(正常完成, 主路径)
    ② __emit_ghost_done 满额(拦截"转默认游荡", 改为执行挂起切换)
    ③ tick 顶部 tick_switch_pending(会话已停/回空等立即执行; 超 15 分钟判卡住执行);
  · 停止类(random_walk_stop)不排队, 并撤销挂起; 同指令重派(游荡池退避重发)保留首排计时。

本自检（stub 真跑, 不连服务器）:
  A. 行为矩阵 —— 6 个忙状态排队 / READY·WAIT_GHOST·ERROR·DONE 直接切 / 战斗中(跨帧窗口)排队;
  B. 完成点触发 —— 模拟"交付完成"调 run_switch_pending → 停抓鬼 + 游荡启动 + 挂起清空;
     反例: 无挂起时 run_switch_pending 不动作;
  C. 停止类不排队 + 撤销挂起; 覆盖(新目标重排) / 去重(同指纹保留计时);
  D. 超时兜底(16 分钟)执行切换; 会话已停 → 下一帧 tick 立即执行;
  E. 静态挂钩断言(daily_ghost 三触发点存在且顺序正确; random_walk 排队点在拒绝闸之后)。

用法: python tools/switch_guard_selftest.py [script_dir]
"""
import os
import sys
import types

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DEFAULT_SCRIPT = os.path.join(ROOT, "deploy", "zones", "prod-240-2300", "script")
SCRIPT_DIR = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_SCRIPT
if SCRIPT_DIR not in sys.path:
    sys.path.insert(0, SCRIPT_DIR)

PASS, FAIL = [], []


def ok(name, cond, detail=""):
    (PASS if cond else FAIL).append(name)
    print("  [%s] %s%s" % ("OK" if cond else "FAIL", name,
                           (" —— " + str(detail)) if detail else ""))


# ================================================================ stub 依赖（真 import random_walk）
config = types.ModuleType("config")
config.robot_roam_exclude_maps = [24]              # 幽冥界: 游荡排除(抓鬼专属)
config.robot_roam_world_maps = [5, 6, 10, 11, 25]
config.robot_roam_wild_maps = []
config.robot_auto_roam_map = "random"
config.robot_auto_roam_mode = "default"

diag = types.ModuleType("diag")
diag.log = lambda *a, **k: None

quest_state = types.ModuleType("quest_state")
quest_state.ST_IDLE = "IDLE"

GRIDS = {
    "5": {"w": 100, "h": 100, "rows": []},
    "6": {"w": 100, "h": 100, "rows": []},
    "10": {"w": 100, "h": 100, "rows": []},
    "11": {"w": 100, "h": 100, "rows": []},
    "25": {"w": 100, "h": 100, "rows": []},
}
EVENTS = []
QE_CALLS = []


class _Quest(object):
    def __init__(self):
        self.chain = None
        self.pending = None
        self.walk_target = None
        self.dijkstra_route = []
        self.dijkstra_waiting = False
        self.dijkstra_jumper = None
        self.dijkstra_final = None
        self.dialog = None
        self.dialog_open = False
        self.active = False
        self.chain_id = ""
        self.state = ""

    def set_state(self, s):
        self.state = s


quest_engine = types.ModuleType("quest_engine")
quest_engine.g_chain_grid_cache = {}
quest_engine.get_quest = lambda ro, create=False: (setattr(ro, "m_quest", ro.m_quest or _Quest()) or ro.m_quest)
quest_engine.__emit = lambda ro, ev: EVENTS.append(ev)
quest_engine.set_quest_chain = lambda q, chain: setattr(q, "chain", chain)
quest_engine.__chain_grid_for = lambda q, mapid: GRIDS.get(str(mapid))
quest_engine.roam_feasible_maps = lambda q, cur, cands: [m for m in cands if m != cur]


def _qe_dispatch(ro, cmd):
    QE_CALLS.append(dict(cmd))
    return {"cmd": cmd.get("cmd"), "result": "ok"}


quest_engine.dispatch_cmd = _qe_dispatch

GHOST_STOP_CALLS = []


def _dg_dispatch(ro, cmd):
    GHOST_STOP_CALLS.append(dict(cmd))
    if cmd.get("cmd") == "ghost_stop":
        g = getattr(ro, "m_ghost", None)
        if g is not None:
            g.enabled = False
    return {"cmd": cmd.get("cmd"), "result": "ok"}


daily_ghost = types.ModuleType("daily_ghost")
daily_ghost.dispatch_cmd = _dg_dispatch

sys.modules["config"] = config
sys.modules["diag"] = diag
sys.modules["quest_state"] = quest_state
sys.modules["quest_engine"] = quest_engine
sys.modules["daily_ghost"] = daily_ghost

import random_walk  # noqa: E402

random_walk.quest_engine = quest_engine

CHAIN = {"map_grids": dict(GRIDS), "dijkstra": {"5": [{"target_map": 6}], "6": []}}


class FakeGhost(object):
    def __init__(self, enabled=True, state="FIGHT"):
        self.enabled = enabled
        self.state = state


class Robot(object):
    def __init__(self, mapid=24, ghost=None, fight=False, logined=True):
        self.m_collect_walk = None
        self.m_quest = None
        self.m_mapid = mapid
        self.m_logined = logined
        self.m_fight_state = fight
        self.m_account = ["sw@x.com"]
        self.m_ghost = ghost


def make_ro(state="FIGHT", enabled=True, fight=False, mapid=24, logined=True):
    return Robot(mapid=mapid, ghost=FakeGhost(enabled, state), fight=fight, logined=logined)


def clear_trace():
    del EVENTS[:]
    del GHOST_STOP_CALLS[:]
    del QE_CALLS[:]


def has_log(kw):
    return any(kw in (e.get("msg", "") or "") for e in EVENTS if isinstance(e, dict))


CEL = lambda: random_walk.__now_ms()   # noqa: E731  当前毫秒(测试计时用)

print("脚本: %s/random_walk.py + daily_ghost.py" % SCRIPT_DIR)
print("== A) 状态矩阵: 忙状态排队 / 空等直接切 / 战斗窗口排队")

BUSY = ["ACCEPT", "NAV", "FIGHT", "SUBMIT", "WAIT_NEXT", "HEAL"]
for st in BUSY:
    clear_trace()
    ro = make_ro(state=st, fight=(st == "FIGHT"))
    rep = random_walk.dispatch_cmd(ro, {"cmd": "random_walk", "mapid": 11, "chain": dict(CHAIN)})
    pend = getattr(ro, "m_roam_switch_pending", None)
    ok("A1[%s] 排队不打断: result=queued, 未启动游荡, 未停抓鬼, 挂起就位" % st,
       rep.get("result") == "queued" and rep.get("reason") == "ghost_busy"
       and getattr(ro.m_collect_walk, "enabled", False) is False
       and not GHOST_STOP_CALLS and (pend or {}).get("cmd", {}).get("mapid") == 11,
       "rep=%s pend=%s" % (rep, pend))

clear_trace()
ro = make_ro(state="NAV", fight=True)   # 点鬼后同帧(还没上 FIGHT 态)的战斗窗口
rep = random_walk.dispatch_cmd(ro, {"cmd": "random_walk", "mapid": 11, "chain": dict(CHAIN)})
ok("A2 战斗中(m_fight_state, 状态仍 NAV)也排队",
   rep.get("result") == "queued" and not GHOST_STOP_CALLS, rep)

for st in ["READY", "WAIT_GHOST", "ERROR"]:
    clear_trace()
    ro = make_ro(state=st)
    before = getattr(ro, "m_roam_switch_pending", None)
    rep = random_walk.dispatch_cmd(ro, {"cmd": "random_walk", "mapid": 11, "chain": dict(CHAIN)})
    ok("A3[%s] 空等/异常 → 直接切换(保持既有行为): ok + 停抓鬼 + 游荡启动" % st,
       rep.get("result") == "ok" and bool(GHOST_STOP_CALLS)
       and getattr(ro.m_collect_walk, "enabled", False) is True
       and getattr(ro, "m_roam_switch_pending", None) is None, rep)

clear_trace()
ro = make_ro(state="DONE", enabled=False)
rep = random_walk.dispatch_cmd(ro, {"cmd": "random_walk", "mapid": 11, "chain": dict(CHAIN)})
ok("A4[DONE/会话已停] 直接切换(无需停抓鬼, 会话本就不在)",
   rep.get("result") == "ok" and not GHOST_STOP_CALLS
   and getattr(ro.m_collect_walk, "enabled", False) is True, rep)

clear_trace()
ro = make_ro(state="IDLE", enabled=False)
rep = random_walk.dispatch_cmd(ro, {"cmd": "random_walk", "mapid": 11, "chain": dict(CHAIN)})
ok("A5[无抓鬼会话] 直接启动(现状不变)", rep.get("result") == "ok", rep)

print("== B) 完成点触发: 交付完成 → 停抓鬼 + 游荡启动 + 挂起清空")
clear_trace()
ro = make_ro(state="SUBMIT")
random_walk.dispatch_cmd(ro, {"cmd": "random_walk", "mapid": 11, "chain": dict(CHAIN)})
assert getattr(ro, "m_roam_switch_pending", None), "前置: 应已排队"
clear_trace()
ran = random_walk.run_switch_pending(ro, reason="ghost_submit_done")
w = ro.m_collect_walk
ok("B1 交付完成事件 → 执行切换: run=True, 挂起清空, ghost_stop 已发, 游荡启动(图 11)",
   ran is True and getattr(ro, "m_roam_switch_pending", None) is None
   and any(c.get("cmd") == "ghost_stop" for c in GHOST_STOP_CALLS)
   and getattr(w, "enabled", False) is True and int(getattr(w, "mapid", 0)) == 11
   and has_log("当前工作单元完成"),
   "ran=%s stop=%s w.enabled=%s" % (ran, GHOST_STOP_CALLS, getattr(w, "enabled", None)))

clear_trace()
ran2 = random_walk.run_switch_pending(ro, reason="ghost_submit_done")
ok("B2 反例: 无挂起时 run_switch_pending 不动作(False, 不再发停/启动)",
   ran2 is False and not GHOST_STOP_CALLS, ran2)

print("== C) 停止类不排队 + 覆盖/去重")
clear_trace()
ro = make_ro(state="FIGHT", fight=True)
random_walk.dispatch_cmd(ro, {"cmd": "random_walk", "mapid": 25, "chain": dict(CHAIN)})
assert getattr(ro, "m_roam_switch_pending", None)
rep_stop = random_walk.dispatch_cmd(ro, {"cmd": "random_walk_stop"})
ok("C1 停止类不排队: 立即停游荡 + 撤销挂起",
   rep_stop.get("result") == "ok" and getattr(ro, "m_roam_switch_pending", None) is None
   and has_log("已撤销挂起的切换指令"), rep_stop)

clear_trace()
ro = make_ro(state="FIGHT", fight=True)
random_walk.dispatch_cmd(ro, {"cmd": "random_walk", "mapid": 11, "chain": dict(CHAIN)})
at1 = ro.m_roam_switch_pending["at_ms"]
rep2 = random_walk.dispatch_cmd(ro, {"cmd": "random_walk", "mapid": 25, "chain": dict(CHAIN)})
at2 = ro.m_roam_switch_pending["at_ms"]
ok("C2 新目标覆盖(最后生效): mapid 11→25, 重新计时",
   rep2.get("result") == "queued" and ro.m_roam_switch_pending["cmd"]["mapid"] == 25
   and at2 >= at1, "at1=%s at2=%s" % (at1, at2))

n_log_before = sum(1 for e in EVENTS if isinstance(e, dict) and "指令已排队" in (e.get("msg") or ""))
rep3 = random_walk.dispatch_cmd(ro, {"cmd": "random_walk", "mapid": 25, "chain": dict(CHAIN)})
at3 = ro.m_roam_switch_pending["at_ms"]
ok("C3 同指令重派(游荡池退避重发)去重: 保留首排计时, 不重复记日志",
   rep3.get("result") == "queued" and abs(float(at3) - float(at2)) < 1.0
   and sum(1 for e in EVENTS if isinstance(e, dict) and "指令已排队" in (e.get("msg") or "")) == n_log_before,
   "at2=%s at3=%s" % (at2, at3))

print("== D) 超时兜底 / 会话结束立即执行 / 未登录保护")
clear_trace()
ro = make_ro(state="FIGHT", fight=True)
random_walk.dispatch_cmd(ro, {"cmd": "random_walk", "mapid": 11, "chain": dict(CHAIN)})
ro.m_roam_switch_pending["at_ms"] = CEL() - random_walk.ROAM_SWITCH_TIMEOUT_MS - 1000   # 16 分钟前入队
random_walk.tick_switch_pending(ro, None)
ok("D1 超时兜底(16 分钟未完成): 判卡住 → 执行切换(停抓鬼+启动)",
   getattr(ro, "m_roam_switch_pending", None) is None
   and any(c.get("cmd") == "ghost_stop" for c in GHOST_STOP_CALLS)
   and getattr(ro.m_collect_walk, "enabled", False) is True
   and has_log("疑卡住"), "log=%s" % [e.get("msg") for e in EVENTS if isinstance(e, dict)][-2:])

clear_trace()
ro = make_ro(state="FIGHT", fight=True)
random_walk.dispatch_cmd(ro, {"cmd": "random_walk", "mapid": 11, "chain": dict(CHAIN)})
ro.m_ghost.enabled = False          # 模拟会话被停(ghost_stop/满额/下线)
random_walk.tick_switch_pending(ro, None)
ok("D2 会话已停 → 下一帧 tick 立即执行(不必等超时), 且不再重复停抓鬼",
   getattr(ro, "m_roam_switch_pending", None) is None
   and not GHOST_STOP_CALLS
   and getattr(ro.m_collect_walk, "enabled", False) is True, GHOST_STOP_CALLS)

clear_trace()
ro = make_ro(state="FIGHT", fight=True, logined=False)
random_walk.dispatch_cmd(ro, {"cmd": "random_walk", "mapid": 11, "chain": dict(CHAIN)})
random_walk.tick_switch_pending(ro, None)
ok("D3 未登录号: tick 兜底不动作(挂起保留, 不动号)",
   getattr(ro, "m_roam_switch_pending", None) is not None
   and getattr(ro.m_collect_walk, "enabled", False) is False, "")

clear_trace()
ro = make_ro(state="WAIT_GHOST")
random_walk.dispatch_cmd(ro, {"cmd": "random_walk", "mapid": 11, "chain": dict(CHAIN)})
random_walk.tick_switch_pending(ro, None)
ok("D4 空等(WAIT_GHOST) → tick 兜底不重复动作(下发时已直接切, 无挂起)",
   getattr(ro, "m_roam_switch_pending", None) is None
   and getattr(ro.m_collect_walk, "enabled", False) is True, "")

print("== E) 静态挂钩断言（daily_ghost 三触发点 + random_walk 排队点顺序）")
rw_src = open(os.path.join(SCRIPT_DIR, "random_walk.py"), encoding="utf-8").read()
dg_src = open(os.path.join(SCRIPT_DIR, "daily_ghost.py"), encoding="utf-8").read()

ok("E1 忙状态常量与超时常量就位",
   'ROAM_GHOST_BUSY_STATES = ("ACCEPT", "NAV", "FIGHT", "SUBMIT", "WAIT_NEXT", "HEAL")' in rw_src
   and "ROAM_SWITCH_TIMEOUT_MS = 900000" in rw_src)

i_busy = rw_src.find("if __ghost_busy(robot_object):")
i_shop = rw_src.find("shop errand in progress")
i_share = rw_src.find("share daily in progress")
i_stopq = rw_src.find("# 互斥: 停止当前任务(新手链/抓鬼)", i_busy if i_busy > 0 else 0)
ok("E2 排队点位于 shop_busy / share_daily 拒绝闸之后、停任务之前(只排队能执行的指令)",
   0 < i_shop < i_busy and 0 < i_share < i_busy and 0 < i_busy < i_stopq,
   "shop@%s share@%s busy@%s stop@%s" % (i_shop, i_share, i_busy, i_stopq))

ok("E3 random_walk_stop 分支撤销挂起",
   "集合走停止: 已撤销挂起的切换指令" in rw_src)

i_fin = dg_src.find("def on_finish_task(")
i_fin_hook = dg_src.find("run_switch_pending(robot_object, reason=\"ghost_submit_done\")", i_fin)
ok("E4 daily_ghost.on_finish_task 挂交付完成触发(条件 = GHOST_SUBMIT_TASK)",
   i_fin > 0 and i_fin_hook > i_fin
   and 'if int(ti) == GHOST_SUBMIT_TASK:' in dg_src[max(0, i_fin_hook - 700):i_fin_hook],
   "fin@%s hook@%s" % (i_fin, i_fin_hook))

i_done = dg_src.find("def __emit_ghost_done(")
i_done_hook = dg_src.find('run_switch_pending(robot_object, reason="ghost_done")', i_done)
i_roam_def = dg_src.find('"抓鬼满额 → 已转野外游荡', i_done)
ok("E5 __emit_ghost_done 满额拦截: 有挂起 → 执行切换(在'转默认游荡'之前)",
   0 < i_done < i_done_hook < i_roam_def, "done@%s hook@%s roam@%s" % (i_done, i_done_hook, i_roam_def))

i_tick = dg_src.find("def tick(robot_object, now):")
i_tick_hook = dg_src.find("random_walk.tick_switch_pending(robot_object, None)", i_tick)
i_g_enabled = dg_src.find("if g is None or not g.enabled:", i_tick)
ok("E6 daily_ghost.tick 顶部挂 tick_switch_pending(在抓鬼 enabled 判断之前, 所有号都跑)",
   0 < i_tick < i_tick_hook < i_g_enabled,
   "tick@%s hook@%s g_enabled@%s" % (i_tick, i_tick_hook, i_g_enabled))

ok("E7 版本号已递增(热更/重启可核对)",
   'SCRIPT_VERSION = "2026-09-29a"' in rw_src and 'SCRIPT_VERSION = "2026-09-29a"' in dg_src)

print()
total = len(PASS) + len(FAIL)
print("结果：%d 项，失败 %d 项" % (total, len(FAIL)))
if FAIL:
    print("失败项:")
    for f in FAIL:
        print("  - %s" % f)
sys.exit(1 if FAIL else 0)
