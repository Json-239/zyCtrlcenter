# -*- coding: utf-8 -*-
"""随机走 dispatch_cmd 冒烟(临时脚本, 不进入生产): 用桩模块驱动 random_walk.dispatch_cmd。

覆盖: 随机图抽签 / maps 白名单 / 非法参数明确拒绝 / gather 占位回退 / 限时 deadline /
同图续约不重抽 / 指定图续约。
"""
import random
import sys
import types

SCRIPT_DIR = sys.argv[1] if len(sys.argv) > 1 else "."
sys.path.insert(0, SCRIPT_DIR)

# ---------------- 桩模块 ----------------
config = types.ModuleType("config")
diag = types.ModuleType("diag")
diag.log = lambda *a, **k: None
quest_state = types.ModuleType("quest_state")
quest_state.ST_IDLE = "IDLE"


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


GRIDS = {
    "6": {"w": 200, "h": 113, "rows": []},
    "10": {"w": 250, "h": 150, "rows": []},
    "17": {"w": 300, "h": 200, "rows": []},
}

quest_engine = types.ModuleType("quest_engine")
quest_engine.get_quest = lambda ro, create=False: (setattr(ro, "m_quest", ro.m_quest or _Quest()) or ro.m_quest)
quest_engine.__emit = lambda ro, ev: EVENTS.append(ev)
quest_engine.set_quest_chain = lambda q, chain: setattr(q, "chain", chain)
quest_engine.__chain_grid_for = lambda q, mapid: GRIDS.get(str(mapid))
CALLS = []


def _qe_dispatch(ro, cmd):
    CALLS.append(cmd)
    return {"cmd": cmd.get("cmd"), "result": "ok"}


quest_engine.dispatch_cmd = _qe_dispatch
quest_engine.__schedule = lambda *a, **k: None
quest_engine.__human_delay = lambda *a, **k: 0
quest_engine.__tick_walk = lambda *a, **k: None
quest_engine.__start_next_hop = lambda *a, **k: None
quest_engine.__do_action = lambda *a, **k: None
EVENTS = []

sys.modules["config"] = config
sys.modules["diag"] = diag
sys.modules["quest_state"] = quest_state
sys.modules["quest_engine"] = quest_engine

import random_walk  # noqa: E402

random_walk.quest_engine = quest_engine


class Robot(object):
    def __init__(self, mapid=5):
        self.m_collect_walk = None
        self.m_quest = None
        self.m_mapid = mapid
        self.m_logined = True
        self.m_fight_state = False
        self.m_account = ["rw_smoke@x.com"]


FAIL = []


def check(name, cond, detail=""):
    print("[%s] %s %s" % ("PASS" if cond else "FAIL", name, detail if not cond else ""))
    if not cond:
        FAIL.append(name)


CHAIN = {"map_grids": dict(GRIDS), "dijkstra": {"5": [{"target_map": 6}], "6": []}}

# ① 随机图 + 白名单 + dense + 限时
ro = Robot(mapid=5)
rep = random_walk.dispatch_cmd(ro, {"cmd": "random_walk", "mapid": "random", "maps": [6, 17],
                                    "mode": "dense", "minutes": 5, "chain": dict(CHAIN)})
check("随机图+白名单: ok", rep.get("result") == "ok", rep)
check("随机图: 目标在白名单内且避开当前图", rep.get("mapid") in (6, 17), rep)
check("随机图: random 标记", rep.get("random") is True, rep)
check("随机图: 白名单回带", rep.get("maps") == [6, 17], rep)
check("随机图: 档位 dense", rep.get("profile") == "dense", rep)
check("随机图: 限时 deadline 已设", rep.get("deadline_ms", 0) > 0 and rep.get("minutes") == 5, rep)
check("随机图: 自动范围(非东海村默认)", ro.m_collect_walk.range != [[200, 200], [3300, 3000]],
      ro.m_collect_walk.range)

# ② 同图(随机)续约: 不重抽 (不重置 deadline, 不带 minutes)
_first_map = rep["mapid"]
rep2 = random_walk.dispatch_cmd(ro, {"cmd": "random_walk", "mapid": "random", "maps": [6, 17],
                                     "mode": "dense", "chain": dict(CHAIN)})
check("随机图续约: 同图且不重抽", rep2.get("mapid") == _first_map and rep2.get("refreshed") is True, rep2)
check("随机图续约: deadline 不丢", rep2.get("deadline_ms", 0) == rep.get("deadline_ms", 0), rep2)

# ③ 指定图(与当前不同) → 重新启动
rep3 = random_walk.dispatch_cmd(ro, {"cmd": "random_walk", "mapid": 10, "chain": dict(CHAIN)})
check("指定图: ok", rep3.get("result") == "ok" and rep3.get("mapid") == 10, rep3)
check("指定图: random 标记为 False", rep3.get("random") is False, rep3)
check("指定图: 未带 minutes → 不限时", rep3.get("deadline_ms", -1) == 0, rep3)

# ④ 非法参数: 明确拒绝且不改状态、不动号上正在跑的任务链(零副作用)
ro2 = Robot(mapid=5)
ro2.m_quest = _Quest()
ro2.m_quest.active = True
CALLS[:] = []
rep4 = random_walk.dispatch_cmd(ro2, {"cmd": "random_walk", "mapid": 10, "maps": [6, 17],
                                      "chain": dict(CHAIN)})
check("非法: 目标图不在白名单 → error", rep4.get("result") == "error", rep4)
check("非法: 原因含白名单", "白名单" in rep4.get("detail", ""), rep4)
check("非法: 不启动游荡", getattr(ro2.m_collect_walk, "enabled", False) is False, ro2.m_collect_walk)
check("非法: 不动号上正在跑的任务链(零副作用)",
      not any(c.get("cmd") == "stop" for c in CALLS), CALLS)
rep5 = random_walk.dispatch_cmd(ro2, {"cmd": "random_walk", "mapid": 34, "chain": dict(CHAIN)})
check("非法: 没有网格图 → error", rep5.get("result") == "error" and "网格" in rep5.get("detail", ""), rep5)
rep6 = random_walk.dispatch_cmd(ro2, {"cmd": "random_walk", "mode": "dense", "chain": dict(CHAIN)})
check("非法: 缺 mapid → error", rep6.get("result") == "error", rep6)

# ⑤ gather 占位档位: 明确回退 + 日志 + reply 带 fallback
ro3 = Robot(mapid=6)
rep7 = random_walk.dispatch_cmd(ro3, {"cmd": "random_walk", "mapid": 6, "mode": "gather",
                                      "chain": dict(CHAIN)})
check("gather 占位: 回退 default", rep7.get("profile") == "default" and
      rep7.get("profile_req") == "gather" and rep7.get("profile_fallback") == "default", rep7)
msgs = [e.get("msg", "") for e in EVENTS if isinstance(e, dict)]
check("gather 占位: 有日志说明", any("gather" in m and "未实现" in m for m in msgs), msgs[-3:])

# ⑥ mode=random 别名
ro4 = Robot(mapid=5)
rep8 = random_walk.dispatch_cmd(ro4, {"cmd": "random_walk", "mode": "random", "maps": [10],
                                      "chain": dict(CHAIN)})
check("mode=random 别名: 随机图", rep8.get("result") == "ok" and rep8.get("random") is True and
      rep8.get("mapid") == 10, rep8)
check("mode=random 别名: 不带 profile 回退噪音",
      "profile_fallback" not in rep8 and
      not any("未知游荡档位" in e.get("msg", "") for e in EVENTS if isinstance(e, dict)))

# ⑦ 限时到点: tick 自动停
ro5 = Robot(mapid=6)
rep9 = random_walk.dispatch_cmd(ro5, {"cmd": "random_walk", "mapid": 6, "minutes": 1,
                                      "chain": dict(CHAIN)})
w = ro5.m_collect_walk
w.deadline_ms = 1000
random_walk.tick(ro5, 2000)
check("限时到点: 自动停(tick)", getattr(ro5.m_collect_walk, "enabled", None) in (None, False), ro5.m_collect_walk)

print("=== %s (失败 %d) ===" % ("PASS" if not FAIL else "FAIL", len(FAIL)))
sys.exit(1 if FAIL else 0)
