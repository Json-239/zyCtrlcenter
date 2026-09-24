# -*- coding: utf-8 -*-
"""新手链"寻找草船"(7001115) × 跨图付费跳/点击死循环 修复自检 —— 2026-09-24

现场（只读取证: data/bot_logs/robot0003001/5017/5084/5333/5338, runs_20260924.log）:
  · 5 个 lv20 新手号全部 ERROR/STUCK_CLICK, 今日合计 58 次 STUCK_CLICK / 159 次点 13003 /
    35 次服务端 1116(储备金不足) / 51 次乐观改图 / 68 次 13021(买草船)导航。
  · 死循环链: 任务 7001115 hint 是 shop_purchase(去 13021 买草船 1 个, 链数据 executor);
    5→612 跨图规划(最短 2 跳)选中 5→11 的**付费边**(dest 156, 13003"长安东市集广场",
    cost_money=300) → 点选项被 1116 拒 → **点中目的地即乐观改图(11)**(设计不动) →
    本地按"图11 坐标"同图走路 1900+px(服务端位置被真实带偏, 上报连续被接受) →
    点 13520/13003 全被**服务端 600 距离校验静默拒**(CLICK 超时 ×4) → 重登 → 再来。
  · 回滚(09-24 CLICK 乐观残留接管)只回滚本地(图+位置快照), **服务端位置留在被带偏处**
    → 回滚后"本地已在目标附近, 跳过走路直接执行"→ 用陈旧本地位置点跳转NPC → 依旧静默拒。

修复（quest_engine.py 三处 + shop_errand.py 一处, 全是窄条件/独立守卫）:
  Q1 __find_dijkstra_route: newbie_full 链默认免费优先(与"跳转被拒后 prefer_free"同一
     两阶段逻辑, 先免费/无解回退付费) —— 从源头不再选付费边 156, 5→9→10→11→612 全免费。
  Q2 __execute_hop(npc_jumper): 与 map_skip 分支对称 —— 没"真走过最后一程"(_walk_done)
     时先就地真走一小步(force_walk, 不走近距捷径) + 点击前校准上报(__sync_position),
     再点跳转NPC; 让"本地已到位但服务端漂移"的自愈闭环补齐(点 13003 无回执根因)。
  Q3 __teleport_click/__start_next_hop: 跨图重试(__recover ST_CLICK 传 force_walk=True)
     的 force_walk 不再在半途丢失 —— 记入 dijkstra_final/plan, final 走路据此真走
     (带 ±12px 抖动), 服务端位置跟上后再点目标 NPC。
  S1 shop_errand.start: 反向互斥闸补齐(与 nav-owner"采购中拒绝游荡"对称) —— 任务链
     激活中(quest.active 且非本模块自己的采购上下文)拒绝启动 errand/food 采购。

用法: python tools/newbie_shop_conflict_selftest.py [script_dir]
"""
import os
import sys
import types

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass


def _resolve(path):
    if os.path.isdir(path):
        return path
    return os.path.dirname(path)


def _extract_func(src, name):
    lines = src.splitlines()
    for i, ln in enumerate(lines):
        if ln.startswith("def %s(" % name):
            out = [ln]
            for ln2 in lines[i + 1:]:
                if ln2.strip() and not ln2[0].isspace():
                    break
                out.append(ln2)
            return "\n".join(out)
    return None


class _RO(object):
    def __init__(self, m_mapid=5, x=1800, y=344):
        self.m_mapid = m_mapid
        self.m_pose = [x, y, 0]
        self.m_stop = False
        self.m_account = ["t@x"]
        self.sent = []

    def send_message(self, proto, args):
        self.sent.append((proto, args))
        return 1


def main():
    if len(sys.argv) < 2:
        print("用法: python tools/newbie_shop_conflict_selftest.py <script 目录 或 quest_engine.py 路径>")
        return 2
    script_dir = _resolve(sys.argv[1])
    qe_path = os.path.join(script_dir, "quest_engine.py")
    se_path = os.path.join(script_dir, "shop_errand.py")
    if not os.path.exists(qe_path):
        print("[FAIL] 找不到 %s" % qe_path)
        return 2
    qe = open(qe_path, encoding="utf-8", errors="replace").read()
    se = open(se_path, encoding="utf-8", errors="replace").read() if os.path.exists(se_path) else ""

    results = []

    def check(name, cond, detail=""):
        results.append((name, bool(cond), detail))

    # ============================================================ A. 源码形状(修复新增)
    check("Q1a: newbie_full 默认免费优先(chain_id 窄条件)",
          '"newbie_full"' in qe and 'chain_id' in qe
          and 'if not _pf and str(getattr(quest, "chain_id", "") or "") == "newbie_full":' in qe)
    _i_q1 = qe.find('if not _pf and str(getattr(quest, "chain_id", "") or "") == "newbie_full":')
    _seg_q1 = qe[max(0, _i_q1 - 1500): _i_q1 + 700] if _i_q1 > 0 else ""
    check("Q1b: 复用既有两阶段(先 _free_only=True 试算, 无解回退全量)",
          "_free_only=True" in _seg_q1 and "_free_only = False" in _seg_q1)
    check("Q1c: 修复注释可溯源(7001115/1116/dest 156)",
          ("7001115" in _seg_q1 or "1116" in _seg_q1 or "156" in _seg_q1))

    check("Q2a: __execute_hop npc_jumper 分支加'真走最后一程'门闸",
          'if not hop.get("_walk_done") and int(hop.get("npc_index", 0) or 0) != 0:' in qe)
    _i_q2w = qe.find('if not hop.get("_walk_done") and int(hop.get("npc_index", 0) or 0) != 0:')
    _seg_q2 = qe[_i_q2w: _i_q2w + 1400] if _i_q2w > 0 else ""
    check("Q2b: 微走 data 带 force_walk=True(不走近距捷径)", '"force_walk": True' in _seg_q2)
    check("Q2c: 点击前校准上报 __sync_position(与 map_skip/__recover 同口径)",
          "__sync_position(robot_object)" in _seg_q2)
    check("Q2d: 微走目标=本地位置±20(不拽向 NPC, 不占 NPC 格; 防 0 位移)",
          "random.randint(-20, 20)" in _seg_q2 and "+ 16" in _seg_q2)

    _i_fin = qe.find("quest.dijkstra_final = {")
    _seg_fin = qe[_i_fin: _i_fin + 400] if _i_fin > 0 else ""
    check("Q3a: __teleport_click 跨图分支记 force_walk 到 dijkstra_final/plan",
          '"force_walk": bool(force_walk)' in _seg_fin, _seg_fin[-200:])
    check("Q3b: __start_next_hop final 走路读取 force_walk(带抖动)",
          'final.get("force_walk")' in qe)

    check("S1a: shop_errand.start 加任务链激活反向闸(errand/food)",
          'if owner in ("errand", "food"):' in se
          and "任务链进行中" in se)
    _i_s1 = se.find("任务链进行中")
    _seg_s1 = se[max(0, _i_s1 - 900): _i_s1 + 200] if _i_s1 > 0 else ""
    check("S1b: 闸条件=quest.active 且 shop_ctx 为空(排除本模块自己的采购上下文)",
          'getattr(q, "active", False)' in _seg_s1 and "shop_ctx" in _seg_s1, _seg_s1[-200:])
    check("S1c: 闸在 acquire/dispatch 之前(早拒, 不占锁不下发)",
          _i_s1 > 0 and _i_s1 < se.find("ok, held = acquire(robot_object, owner") and
          _i_s1 < se.find("res = quest_engine.dispatch_cmd"))

    # ============================================================ B. Q1 真值表(真跑 __find_dijkstra_route)
    dj = {
        "5": [
            {"destination_index": 155, "from_map": 5, "target_map": 9, "kind": "npc_jumper",
             "x": 1792, "y": 351, "target_name": "大唐东-码头", "npc_index": 13003,
             "match_name": "大唐东-码头", "cost_money": 0},
            {"destination_index": 156, "from_map": 5, "target_map": 11, "kind": "npc_jumper",
             "x": 1792, "y": 351, "target_name": "长安东市集", "npc_index": 13003,
             "match_name": "长安东市集广场", "cost_money": 300},
        ],
        "9": [
            {"destination_index": 9, "from_map": 9, "target_map": 10, "kind": "map_skip",
             "x": 95, "y": 842, "target_name": "大唐东-野林"},
        ],
        "10": [
            {"destination_index": 12, "from_map": 10, "target_map": 11, "kind": "map_skip",
             "x": 347, "y": 315, "target_name": "长安东市集"},
        ],
        "11": [
            {"destination_index": 83, "from_map": 11, "target_map": 612, "kind": "map_skip",
             "x": 3900, "y": 649, "target_name": "琳琅阁"},
        ],
        # 只给付费路线的兜底场景: 7→8(付费 2 跳)
        "7": [
            {"destination_index": 301, "from_map": 7, "target_map": 8, "kind": "npc_jumper",
             "x": 100, "y": 100, "npc_index": 99999, "match_name": "某地", "cost_money": 500},
        ],
    }
    import time as _time
    ns_rt = {
        "time": _time,
        "g_chain_dijkstra_cache": None,
        "BAD_JUMPERS": (10147, 13255),
        "__hop_key": lambda from_map, e: (int(from_map or 0), e.get("destination_index")),
        "HOP_BLACKLIST_MS": 600000,
        "HOP_FAIL_LIMIT": 3,
    }
    frag_rt = _extract_func(qe, "__find_dijkstra_route")
    check("B0: 提取 __find_dijkstra_route", frag_rt is not None)
    if frag_rt:
        try:
            exec(frag_rt, ns_rt)
        except Exception as e:  # noqa
            check("B0: exec __find_dijkstra_route", False, str(e))
    rt = ns_rt.get("__find_dijkstra_route")

    class _Q(object):
        def __init__(self, chain_id="", prefer_free=False, black=None):
            self.chain = {"dijkstra": dj}
            self.chain_id = chain_id
            self.prefer_free = prefer_free
            self.hop_black = black or {}

    def _dests(route):
        return [e.get("destination_index") for e in (route or [])]

    if rt is not None:
        # B1 现场命中: newbie_full + 未置 prefer_free → 必须全免费(5→9→10→11→612)
        r = rt(_Q(chain_id="newbie_full"), 5, 612)
        check("B1: newbie_full 未置 prefer_free → 免费路线(155/9/12/83, 无 156)",
              _dests(r) == [155, 9, 12, 83], _dests(r))
        # B2 对照(反例): 其它链(如抓鬼)未置 prefer_free → 行为不变(最短 2 跳含付费 156)
        r = rt(_Q(chain_id="zhuaogui"), 5, 612)
        check("B2(反例): 非新手链 → 仍按最短跳数(156,83), 行为等价",
              _dests(r) == [156, 83], _dests(r))
        # B3 反例: newbie_full + prefer_free 已置(既有失败后路径) → 同样免费(不回归)
        r = rt(_Q(chain_id="newbie_full", prefer_free=True), 5, 612)
        check("B3(反例): prefer_free=True 的既有路径 → 免费优先不回归",
              _dests(r) == [155, 9, 12, 83], _dests(r))
        # B4 反例: 免费不可达(7→8 只有付费边) → 回退付费, 不 NO_LEGAL_ROUTE
        r = rt(_Q(chain_id="newbie_full"), 7, 8)
        check("B4(反例): 全免费无解 → 回退付费边(301), 不空手",
              _dests(r) == [301], _dests(r))
        # B5 反例: 无链数据 → None(不崩)
        q5 = _Q(chain_id="newbie_full")
        q5.chain = None
        r = rt(q5, 5, 612)
        check("B5(反例): 无 dijkstra 表 → None(不崩)", r is None, r)
        # B6 闭环: 回滚(prefer_free=True) → 规划不再选付费 156(回滚后重入循环被闸)
        r = rt(_Q(chain_id="", prefer_free=True), 5, 612)
        check("B6: 回滚置 prefer_free 后 → 免费路线(无 156), 重入循环被闸",
              _dests(r) == [155, 9, 12, 83], _dests(r))

    # ============================================================ C. Q2 真跑 __execute_hop(npc_jumper)
    _events = []
    _proto = types.SimpleNamespace(C2S_NOTIFY_POSITION=9600, C2S_DIJKSTRA_DESTINATION=9601,
                                   C2S_CLICKNPC=9602)
    ns_hop = {
        "random": __import__("random"),
        "config": types.SimpleNamespace(hop_jitter_px=25),
        "__now_ms": lambda: 100000,
        "__human_delay": lambda d: int(d),
        "__schedule": lambda q, p: _events.append(("SCHED", p)),
        "__emit": lambda ro, ev: _events.append(("EMIT", ev.get("msg") or "")),
        "__sync_position": lambda ro, times=3: _events.append(("SYNC", times)),
        "protocol3": _proto,
    }
    # 提供真实 __hop_walk_target / __send_destination / __set_pose 的简化 stub
    ns_hop["__hop_walk_target"] = lambda x, y: (x + 1, y - 1)
    ns_hop["__send_destination"] = lambda ro, q, hop: _events.append(
        ("DEST", hop.get("destination_index")))
    ns_hop["__set_pose"] = lambda ro, x, y, src="": (x, y)
    ns_hop["__start_next_hop"] = lambda ro, q, ms: None
    frag_hop = _extract_func(qe, "__execute_hop")
    check("C0: 提取 __execute_hop", frag_hop is not None)
    if frag_hop:
        try:
            exec(frag_hop, ns_hop)
        except Exception as e:  # noqa
            check("C0: exec __execute_hop", False, str(e))
    fx = ns_hop.get("__execute_hop")
    if fx is not None:
        class _Qh(object):
            def __init__(self):
                self.walk_hop_wait_count = 0
                self.state = None
        # C1 首次(未 _walk_done): 必须先"真走最后一程", 不得直接点击
        hop1 = {"kind": "npc_jumper", "npc_index": 13003, "x": 1792, "y": 351,
                "from_map": 5, "target_map": 9, "destination_index": 155}
        ro = _RO()
        _events[:] = []
        fx(ro, _Qh(), hop1, 100000)
        sched = [d for (t, d) in _events if t == "SCHED"]
        clicked = [d for d in sched if d.get("type") == "click"
                   and d.get("data", {}).get("npc_id") == 13003]
        walked = [d for d in sched if d.get("type") == "walk"
                  and d.get("data", {}).get("force_walk")]
        check("C1: 首次(未走完) → 调度 force_walk 真走, 不点击",
              len(walked) == 1 and len(clicked) == 0,
              "walked=%d clicked=%d events=%s" % (len(walked), len(clicked), _events[:6]))
        check("C1b: 微走目标为本地位置±20(而非 hop 坐标)",
              len(walked) == 1
              and abs(walked[0]["data"]["to_x"] - 1800) <= 20
              and abs(walked[0]["data"]["to_y"] - 344) <= 20,
              walked[0]["data"] if walked else None)
        # C2 真走完成后(_walk_done): 校准上报 + 点击(顺序: SYNC 在 click 之前)
        hop2 = dict(hop1)
        hop2["_walk_done"] = True
        ro = _RO()
        _events[:] = []
        fx(ro, _Qh(), hop2, 100000)
        names = [t for (t, _d) in _events]
        clicked = [d for (t, d) in _events if t == "SCHED" and d.get("type") == "click"]
        check("C2: 走完后 → 先 SYNC 校准上报再点击跳转NPC",
              "SYNC" in names and "SCHED" in names
              and names.index("SYNC") < names.index("SCHED")
              and len(clicked) == 1
              and clicked[0]["data"]["npc_id"] == 13003,
              names)
        # C3 反例: map_skip 行为不变(未 _walk_done → 真走; 走完 → 校准+destination)
        hop3 = {"kind": "map_skip", "x": 3900, "y": 649, "from_map": 11,
                "target_map": 612, "destination_index": 83}
        ro = _RO(m_mapid=11)
        _events[:] = []
        fx(ro, _Qh(), hop3, 100000)
        sched = [d for (t, d) in _events if t == "SCHED"]
        check("C3(反例): map_skip 未走完 → 仍先真走(行为不变)",
              len([d for d in sched if d.get("type") == "walk"]) == 1
              and len([d for d in sched if d.get("type") == "click"]) == 0, sched)
        hop3["_walk_done"] = True
        _events[:] = []
        fx(ro, _Qh(), hop3, 100000)
        check("C3b(反例): map_skip 走完 → 校准上报+destination(行为不变)",
              any(t == "DEST" for (t, _d) in _events) and not any(
                  t == "SCHED" and d.get("type") == "walk" for (t, d) in _events), _events)
        # C4 反例: npc_jumper 无 npc_index(0) → 不做微走(防死循环), 走校准+点击
        hop4 = {"kind": "npc_jumper", "npc_index": 0, "x": 0, "y": 0,
                "from_map": 5, "target_map": 9, "destination_index": 155}
        _events[:] = []
        fx(_RO(), _Qh(), hop4, 100000)
        names = [t for (t, _d) in _events]
        check("C4(反例): npc_index=0 → 不调度微走(直接点击路径, 不空转)",
              "SYNC" in names and not any(
                  t == "SCHED" and d.get("type") == "walk" for (t, d) in _events), _events)

    # ============================================================ D. Q3 真跑 __start_next_hop(final)
    ns_nh = {
        "random": __import__("random"),
        "time": _time,
        "__now_ms": lambda: 100000,
        "__human_delay": lambda d: int(d),
        "__schedule": lambda q, p: _events.append(("SCHED", p)),
        "__emit": lambda ro, ev: _events.append(("EMIT", ev.get("msg") or "")),
        "NAV_REPLAN_MAX": 6, "NAV_REPLAN_WINDOW_MS": 60000, "NAV_REPLAN_BACKOFF_MS": 1500,
        "diag": types.SimpleNamespace(log=lambda *a, **k: None),
        "__teleport_click": lambda *a, **k: _events.append(("REPLAN", a)),
    }
    frag_nh = _extract_func(qe, "__start_next_hop")
    check("D0: 提取 __start_next_hop", frag_nh is not None)
    if frag_nh:
        try:
            exec(frag_nh, ns_nh)
        except Exception as e:  # noqa
            check("D0: exec __start_next_hop", False, str(e))
    nh = ns_nh.get("__start_next_hop")
    if nh is not None:
        class _Qn(object):
            def __init__(self, fw):
                self.dijkstra_route = []
                self.dijkstra_final = {"npc_id": 13520, "npc_index": 13520,
                                       "click_type": 0, "pos": [11, 3517, 1038],
                                       "force_walk": fw}
                self.dijkstra_goal_map = 0
                self.dijkstra_plan = None
                self.dijkstra_waiting = False
                self.dijkstra_jumper = None
        # D1 重试(force_walk=True) → final 走路带 force_walk + ±12 抖动
        _events[:] = []
        nh(_RO(), _Qn(True), 500)
        sched = [d for (t, d) in _events if t == "SCHED"]
        ok_fw = (len(sched) == 1 and sched[0]["data"].get("force_walk") is True)
        jx = abs(sched[0]["data"]["to_x"] - 3517) if sched else -1
        jy = abs(sched[0]["data"]["to_y"] - 1038) if sched else -1
        check("D1: 重试跨图 final 走路 → force_walk=True(±12 抖动)",
              ok_fw and jx <= 12 and jy <= 12, sched[:1])
        # D2 反例: 首次(force_walk=False) → 行为等价(无 force_walk, 坐标不抖动)
        _events[:] = []
        nh(_RO(), _Qn(False), 500)
        sched = [d for (t, d) in _events if t == "SCHED"]
        check("D2(反例): 首次 final 走路 → 无 force_walk, 坐标保持原值(行为等价)",
              len(sched) == 1 and not sched[0]["data"].get("force_walk")
              and sched[0]["data"]["to_x"] == 3517 and sched[0]["data"]["to_y"] == 1038,
              sched[:1])
        # D3 反例: 旧版 final 无 force_walk 键 → 不崩, 默认 False
        _events[:] = []
        q = _Qn(False)
        del q.dijkstra_final["force_walk"]
        nh(_RO(), q, 500)
        sched = [d for (t, d) in _events if t == "SCHED"]
        check("D3(反例): 旧会话 final 无 force_walk 键 → 默认 False 不崩",
              len(sched) == 1 and not sched[0]["data"].get("force_walk"), sched[:1])

    # ============================================================ E. S1 真跑 shop_errand.start(链激活闸)
    se_ok = False
    sys_mod_qe = types.SimpleNamespace(
        dispatch_cmd=lambda ro, cmd: _events.append(("DISPATCH", cmd)) or {"result": "ok"})
    _old_qe = sys.modules.get("quest_engine")
    if se:
        ns_se = {"time": _time}
        # 函数体内的 import quest_engine 在**调用时**解析 → stub 须在调用期间驻留 sys.modules
        sys.modules["quest_engine"] = sys_mod_qe
        try:
            exec(compile(se, se_path, "exec"), ns_se)
            se_ok = True
        except Exception as e:  # noqa
            check("E0: exec shop_errand.py", False, str(e))
    start = None
    if se_ok:
        start = ns_se.get("start")
        check("E0: 提取 shop_errand.start", start is not None)

    class _ROse(object):
        def __init__(self, active, shop_ctx=None):
            self.m_account = ["robot0005017@xy3.com"]
            self.m_fight_state = False
            self.m_ghost = None
            self.m_quest = types.SimpleNamespace(active=active, shop_ctx=shop_ctx)

    if start is not None:
        # E1 现场命中: 任务链激活(active=True, 无采购上下文) → 拒绝启动 errand 采购
        ns_se["_locks"].clear()
        _events[:] = []
        res = start(_ROse(True), 101008, count=200, owner="food")
        check("E1: 链激活中 → food 采购被拒(busy) 且不下发执行器",
              isinstance(res, dict) and res.get("result") == "busy"
              and not any(t == "DISPATCH" for (t, _d) in _events),
              (res, _events))
        # E2 反例: 空闲(active=False) → 照旧放行(既有行为等价)
        ns_se["_locks"].clear()
        _events[:] = []
        res = start(_ROse(False), 101008, count=200, owner="food")
        check("E2(反例): 空闲 → 采购照旧启动(dispatch 被调用)",
              isinstance(res, dict) and any(t == "DISPATCH" for (t, _d) in _events),
              (res, _events))
        # E3 反例: 采购自己上下文(active=True 且 shop_ctx 非空, 如重复下发) → 不被本闸拦
        ns_se["_locks"].clear()
        _events[:] = []
        res = start(_ROse(True, shop_ctx={"errand": True}), 101008, count=200, owner="food")
        check("E3(反例): 本模块自己的采购上下文 → 本闸放行(交由既有同 owner busy 逻辑)",
              isinstance(res, dict) and any(t == "DISPATCH" for (t, _d) in _events),
              (res, _events))
        # E4 反例: drug(抓鬼疗伤)不受影响
        ns_se["_locks"].clear()
        _events[:] = []
        res = start(_ROse(True), 102007, count=100, owner="drug")
        check("E4(反例): owner=drug(抓鬼买药) → 不受本闸影响",
              isinstance(res, dict) and any(t == "DISPATCH" for (t, _d) in _events),
              (res, _events))

    if _old_qe is not None:
        sys.modules["quest_engine"] = _old_qe
    else:
        sys.modules.pop("quest_engine", None)

    # ============================================================ F. 不倒退(既有修复保留)
    check("F1: 09-24 CLICK 乐观残留接管保留(__clk_optimistic_stuck_note/__clk_rollback_optimistic)",
          "def __clk_optimistic_stuck_note(robot_object, fail_n):" in qe
          and "def __clk_rollback_optimistic(robot_object, quest, note):" in qe)
    check("F2: 位置快照恢复(set_pose)保留", "__restore_optimistic_snapshot" in qe
          and 'source="rollback_optimistic"' in qe)
    check("F3: prefer_free 语义保留(Q1 重构后仍读 quest.prefer_free)",
          'getattr(quest, "prefer_free", False)' in qe)
    check("F3b: __replan_after_bad_hop 仍置 prefer_free",
          "def __replan_after_bad_hop(robot_object, quest, dest, note):" in qe
          and "quest.prefer_free = True" in qe)
    check("F4: 乐观改图本身未改(主动同步+optimistic 标记)", 
          '"msg": "跳转NPC过图成功 → 地图 %d (主动同步)" % _done["target_map"]})' in qe
          and 'robot_object.m_mapid_optimistic = int(_done.get("target_map", 0) or 0)' in qe)
    check("F5: map_skip 分支行为未改(_walk_done/校准上报/destination)", 
          "if hop_x > 0 and hop_y > 0 and not hop.get(\"_walk_done\"):" in qe
          and "到达跳转点(%d,%d), 发 C2S_DIJKSTRA_DESTINATION %s" in qe)
    check("F6: __recover ST_CLICK 重试链保留(计数→判定→回滚→重试)",
          "__clk_track_fail(robot_object, t[2])" in qe
          and "force_walk=(state == quest_state.ST_CLICK)" in qe)
    check("F7: 抓鬼闸(owner errand/food 拒抓鬼中)保留",
          "抓鬼进行中, 手动采购仅限空闲号" in se)
    check("F8: 战斗闸保留(战斗中不启动商店采购)",
          "战斗中, 不启动商店采购" in se)

    nfail = 0
    for name, ok, detail in results:
        if ok:
            print("[PASS] %s" % name)
        else:
            nfail += 1
            print("[FAIL] %s %s" % (name, ("(%s)" % (detail,)) if detail else ""))
    print("=== 结果: %s (共 %d 项断言, 失败 %d 项) ===" % (
        "PASS" if nfail == 0 else "FAIL", len(results), nfail))
    return 0 if nfail == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
