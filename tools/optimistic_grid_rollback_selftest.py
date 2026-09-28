# -*- coding: utf-8 -*-
"""乐观过图"网格相容性判据"自检（2026-09-28）。

背景（docs/04-测试/分析-20260928-卡在NPC附近排查.md §A/§P0②）:
  4 号(5048/5116/5212/5279)硬卡且永不自愈 —— 本地图被乐观改成 24(替加旁真值图 12),
  位置(4707,2768)越出图 24 网格覆盖(200×122 格=3200×1952px) → A* 起点越界必失败
  ("地图 24 无可行路径") → 既有 A 型(hop 比对)/CLICK(同 NPC 连点失败)两条判定都不
  经过此形态 → 报错/停链后重登又乐观跳图 → 闭环。
修复: `__optimistic_grid_mismatch_note`(四条件判定: 乐观标记挂着+本地==乐观图+
  服务端图!=本地+本地图网格存在+pos 越出网格覆盖) + `__grid_rollback_optimistic`
  (回滚服务端图/恢复改图前位置快照/清跨图残留下/免费优先; 单次回滚不做 hop 拉黑),
  在 __do_walk 失败路径(所有连通性兜底之后、报错之前)接管。

2026-09-28 R-1 追加(robot0005286 越网格回滚死循环, 当日 1172 次回滚):
  ① `__opt_rollback_guard`: 同一跳转点时间窗内重复越网格回滚 >= 2 次 → 拉黑
     (hop_black + hop_black_hard 硬黑名单) + __replan_after_bad_hop 换路重规划;
  ② `__find_dijkstra_route`: 硬黑名单在有效期内禁止"忽略黑名单再找一次"回退
     (普通黑名单保留旧回退行为不变);
  ③ `__teleport_click`: 硬黑名单活跃且仍无替代路线 → 报 OPTIMISTIC_LOOP_STOP
     停链 + 60s 规划节流(防上层每帧重试)。
  反例(必须不触发): 单次回滚 / 不同跳转点各 1 次 / 超窗计数重开 / 无归因 hop=None。

用法: python tools/optimistic_grid_rollback_selftest.py [script_dir]
"""
import os
import sys
import time
import types

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_DIR = os.path.normpath(os.path.join(
    HERE, "..", "deploy", "zones", "prod-240-2300", "script"))

results = []


def check(name, cond, detail=""):
    results.append((name, bool(cond), detail))


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


def main():
    if len(sys.argv) >= 2:
        script_dir = _resolve(sys.argv[1])
    else:
        script_dir = DEFAULT_DIR
    qe_path = os.path.join(script_dir, "quest_engine.py")
    if not os.path.exists(qe_path):
        print("[FAIL] 找不到 %s" % qe_path)
        return 2
    qe = open(qe_path, encoding="utf-8", errors="replace").read()
    if script_dir not in sys.path:
        sys.path.insert(0, script_dir)

    # ============================================================ A. 源码形状
    check("A1: 定义 __optimistic_grid_mismatch_note(四条件判定)",
          "def __optimistic_grid_mismatch_note(robot_object, quest, grid_data=None):" in qe)
    check("A2: 定义 __grid_rollback_optimistic(回滚接管)",
          "def __grid_rollback_optimistic(robot_object, quest, note):" in qe)
    check("A3: 判据条件文本齐全(乐观标记/服务端图/越界)",
          "if not _opt or not _srv or _opt != _now or _srv == _now:" in qe
          and "if 0 <= gx < g.w and 0 <= gy < g.h:" in qe)
    check("A4: 接管在 __do_walk 失败路径(报错之前)",
          0 < qe.find("_go_note = __optimistic_grid_mismatch_note(robot_object, quest, grid_data)")
          and qe.find("_go_note = __optimistic_grid_mismatch_note")
          > qe.find('_fail_msg = "地图 %d 无可行路径')
          and qe.find("_go_note = __optimistic_grid_mismatch_note")
          < qe.find('if now_ms >= getattr(quest, "walk_fail_report_ms", 0):'))
    check("A5: 接管调用 __cancel_walk + 回滚(不报错/不停链)",
          "__cancel_walk(quest)" in qe
          and "__grid_rollback_optimistic(robot_object, quest, _go_note)" in qe)
    check("A6b: 定义 __opt_rollback_guard(重复回滚保护)",
          "def __opt_rollback_guard(robot_object, quest, hop, note):" in qe)
    _frag_rb = _extract_func(qe, "__grid_rollback_optimistic") or ""
    check("A6: 单次回滚不拉黑; 重复回滚交 guard(归因在清乐观标记之前取)",
          "__hop_blacklist" not in _frag_rb
          and "__opt_rollback_guard(robot_object, quest, _rb_hop, note)" in _frag_rb
          and 0 <= _frag_rb.find("_opt_before = int(getattr(robot_object, \"m_mapid_optimistic\", 0) or 0)")
          < _frag_rb.find("robot_object.m_mapid_optimistic = 0"))
    check("A7: 独立日志锚点(位置越出本地图网格...)",
          "位置越出本地图网格且乐观过图未被服务端接受" in qe)
    check("A8: 既有 A 型/CLICK 回滚保留(不冲突)",
          "def __optimistic_mismatch_note(robot_object, hop):" in qe
          and "def __clk_rollback_optimistic(robot_object, quest, note):" in qe)

    # ============================================================ B. 判定真值表
    import robot_path  # 真模块(纯 py, 无依赖)
    ns_note = {
        "robot_path": robot_path,
        "__chain_grid_for": lambda q, m: None,
    }
    frag_note = _extract_func(qe, "__optimistic_grid_mismatch_note")
    check("B0: 提取 __optimistic_grid_mismatch_note", frag_note is not None)
    note = None
    if frag_note:
        try:
            exec(frag_note, ns_note)
            note = ns_note.get("__optimistic_grid_mismatch_note")
        except Exception as e:  # noqa
            check("B0: exec __optimistic_grid_mismatch_note", False, str(e))

    grid24 = {"w": 200, "h": 122, "rows": ["0" * 200] * 122}

    class _RO(object):
        def __init__(self, m=24, srv=12, opt=24, pose=(4707, 2768)):
            self.m_mapid = m
            self.m_srv_mapid = srv
            self.m_mapid_optimistic = opt
            self.m_pose = [pose[0], pose[1], 0]

    if note is not None:
        # B1 现场实况(5048/5116/5212/5279): 本地 24 / 服务端 12 / pos(4707,2768) 越界
        r = note(_RO(), None, grid24)
        check("B1: 现场实况(本地24/服务端12/pos越界) → 命中",
              isinstance(r, str) and "越出本地图24网格覆盖" in r and "服务端图12" in r, r)
        # B2 位置在网格内 → 不命中(正常走路/真延迟场景)
        r = note(_RO(pose=(100, 100)), None, grid24)
        check("B2(反例): pos 在网格覆盖内 → None", r is None, r)
        # B3 无乐观标记(B 型被拒回滚后/正常态) → 不命中
        r = note(_RO(opt=0), None, grid24)
        check("B3(反例): 无乐观标记 → None", r is None, r)
        # B4 本地==服务端图(已回滚/已确认) → 不命中
        r = note(_RO(m=12, srv=12, opt=12), None, grid24)
        check("B4(反例): srv==本地 → None", r is None, r)
        # B5 无网格(GRID_MISSING 场景, 两层都拿不到) → 不命中(交原路径)
        r = note(_RO(), None, None)
        check("B5(反例): 无网格(GRID_MISSING) → None", r is None, r)
        # B6 grid_data 缺省时用 __chain_grid_for 兜底(第二层: 网格缓存)
        ns_note["__chain_grid_for"] = lambda q, m: grid24
        r = note(_RO(), None, None)
        check("B6: grid_data 缺省 → __chain_grid_for 兜底仍命中",
              isinstance(r, str) and "越出本地图24" in r, r)
        # B7 服务端图缺失(=0) → 不命中
        r = note(_RO(srv=0), None, grid24)
        check("B7(反例): m_srv_mapid=0 → None", r is None, r)

    # ============================================================ C. 接管动作语义
    # C0 前置: 真提取 __opt_rollback_guard(R-1), 注入 mock 依赖 —— C/D 两区共用
    def _mock_hop_key(fm, e):
        return (int(fm or 0), e.get("destination_index"))

    guard_events = []
    guard_replans = []
    guard_blacklisted = []
    ns_guard = {
        "time": time,
        "__hop_key": _mock_hop_key,
        "__hop_blacklist": lambda ro, q, h: (guard_blacklisted.append(
            _mock_hop_key(h.get("_from_map", getattr(ro, "m_mapid", 0)), h)), 1)[1],
        "__replan_after_bad_hop": lambda ro, q, dest, note: guard_replans.append((dest, note)),
        "__emit": lambda ro, ev: guard_events.append(ev.get("msg") or ""),
        "HOP_BLACKLIST_MS": 600000,
        "OPT_ROLLBACK_LIMIT": 2,
        "OPT_ROLLBACK_WINDOW_MS": 5 * 60 * 1000,
    }
    frag_guard = _extract_func(qe, "__opt_rollback_guard")
    check("C0a: 提取 __opt_rollback_guard", frag_guard is not None)
    guard = None
    if frag_guard:
        try:
            exec(frag_guard, ns_guard)
            guard = ns_guard.get("__opt_rollback_guard")
        except Exception as e:  # noqa
            check("C0a: exec __opt_rollback_guard", False, str(e))

    events = []
    calls = []
    ns_rb = {
        "__emit": lambda ro, ev: events.append(ev.get("msg") or ""),
        "__restore_optimistic_snapshot": lambda ro, srv: (calls.append(srv), True)[1],
        "__opt_rollback_guard": guard,    # C/D 共用同一真实现
    }
    frag_rb = _extract_func(qe, "__grid_rollback_optimistic")
    rb = None
    if frag_rb:
        try:
            exec(frag_rb, ns_rb)
            rb = ns_rb.get("__grid_rollback_optimistic")
        except Exception as e:  # noqa
            check("C0: exec __grid_rollback_optimistic", False, str(e))
    if rb is not None:
        ro = _RO()
        q = types.SimpleNamespace(
            prefer_free=False, dijkstra_route=[{"destination_index": 1}],
            dijkstra_waiting=True, dijkstra_jumper={"x": 1}, dijkstra_final={"y": 2},
            walk_target=[1, 2], path_points=[[1, 2]])
        events[:] = []
        ok = rb(ro, q, "note-xyz")
        check("C1: 回滚服务端图 + 清乐观标记",
              ok is True and ro.m_mapid == 12 and ro.m_mapid_optimistic == 0,
              (ro.m_mapid, ro.m_mapid_optimistic))
        check("C2: 恢复'乐观改图前位置快照'(目标图=服务端图)",
              calls == [12], calls)
        check("C3: 清跨图/行走残留 + 免费优先",
              q.prefer_free is True and q.dijkstra_route == []
              and q.dijkstra_waiting is False and q.dijkstra_jumper is None
              and q.dijkstra_final is None and q.walk_target is None
              and q.path_points == [], (q.prefer_free, q.dijkstra_route))
        check("C4: 日志锚点'位置越出本地图网格...'",
              any("位置越出本地图网格" in m for m in events), events)
        # C5(反例): 归因成立(last_hop_done.target_map==乐观图)但仅单次回滚
        #   → guard 只记账(n=1), 不拉黑不换路 —— 既有单次回滚行为不变
        ro2 = _RO()
        q2 = types.SimpleNamespace(
            prefer_free=False, dijkstra_route=[], dijkstra_waiting=False,
            dijkstra_jumper=None, dijkstra_final=None, walk_target=None,
            path_points=[], last_hop_done={"_from_map": 12, "destination_index": 201,
                "target_map": 24, "npc_index": 13255, "npc_name": "替加"})
        guard_blacklisted[:] = []
        guard_replans[:] = []
        rb(ro2, q2, "note-c5")
        check("C5(反例): 归因成立但仅 1 次回滚 → 不拉黑不换路",
              not guard_blacklisted and not guard_replans
              and q2.opt_rb.get("n") == 1 and not getattr(q2, "hop_black_hard", None),
              (guard_blacklisted, guard_replans, getattr(q2, "opt_rb", None)))

    # ============================================================ D. R-1 重复回滚保护(真执行)
    if guard is not None:
        class _ROD(object):
            def __init__(self):
                self.m_mapid = 12
        hop201 = {"_from_map": 12, "destination_index": 201, "target_map": 24,
                  "npc_index": 13255, "npc_name": "替加"}
        roD = _ROD()
        # D1 第一次回滚: 只记账 → 不动作
        qD = types.SimpleNamespace()
        guard_blacklisted[:] = []
        guard_replans[:] = []
        guard_events[:] = []
        okD1 = guard(roD, qD, hop201, "note")
        check("D1(反例): 同点第 1 次回滚 → 不拉黑不换路, 仅记账 n=1",
              okD1 is False and not guard_blacklisted and not guard_replans
              and isinstance(qD.opt_rb, dict) and qD.opt_rb.get("n") == 1
              and qD.opt_rb.get("key") == (12, 201),
              (okD1, qD.opt_rb))
        # D2 同点第二次回滚(窗口内) → 拉黑(普通+硬) + 换路(真执行)
        guard_blacklisted[:] = []
        guard_replans[:] = []
        guard_events[:] = []
        okD2 = guard(roD, qD, hop201, "note2")
        check("D2: 同点第 2 次回滚 → 拉黑+硬黑名单+换路重规划",
              okD2 is True and guard_blacklisted == [(12, 201)]
              and isinstance(getattr(qD, "hop_black_hard", None), dict)
              and (12, 201) in qD.hop_black_hard
              and len(guard_replans) == 1 and guard_replans[0][0] == 201
              and qD.opt_rb is None,
              (okD2, guard_blacklisted, getattr(qD, "hop_black_hard", None), guard_replans))
        check("D2b: 拉黑日志含跳转点/次数锚点",
              any("越网格回滚循环" in m and "201" in m and "13255" in m for m in guard_events),
              guard_events)
        # D3(反例) 不同跳转点各 1 次(+交叉回到旧点) → 均不触发
        qD3 = types.SimpleNamespace()
        okD3a = guard(roD, qD3, {"_from_map": 12, "destination_index": 14,
                                 "target_map": 11}, "n")
        okD3b = guard(roD, qD3, {"_from_map": 11, "destination_index": 211,
                                 "target_map": 609}, "n")
        okD3c = guard(roD, qD3, {"_from_map": 12, "destination_index": 14,
                                 "target_map": 11}, "n")
        check("D3(反例): 不同跳转点各 1 次/交叉 → 均不触发(切换即重开)",
              okD3a is False and okD3b is False and okD3c is False
              and not getattr(qD3, "hop_black_hard", None),
              (okD3a, okD3b, okD3c, getattr(qD3, "opt_rb", None)))
        # D4(反例) 超窗(>5min) → 计数重开为 1, 不触发
        qD4 = types.SimpleNamespace(opt_rb={"key": (12, 201), "n": 1,
            "ts": int(time.time() * 1000) - (5 * 60 * 1000) - 5000})
        okD4 = guard(roD, qD4, hop201, "n")
        check("D4(反例): 超窗 >5min → 计数重开(1), 不触发",
              okD4 is False and qD4.opt_rb.get("n") == 1
              and not getattr(qD4, "hop_black_hard", None),
              (okD4, qD4.opt_rb))
        # D5(反例) 无归因(hop=None) → 不触发不记账
        qD5 = types.SimpleNamespace()
        okD5 = guard(roD, qD5, None, "n")
        check("D5(反例): hop=None(无归因) → 不触发不记账",
              okD5 is False and not getattr(qD5, "opt_rb", None))

    # ============================================================ E. 硬黑名单严格模式(__find_dijkstra_route)
    ns_find = {
        "time": time,
        "g_chain_dijkstra_cache": {},
        "BAD_JUMPERS": (10147, 13255),
        "__hop_key": _mock_hop_key,
    }
    frag_find = _extract_func(qe, "__find_dijkstra_route")
    check("E0: 提取 __find_dijkstra_route", frag_find is not None)
    find = None
    if frag_find:
        try:
            exec(frag_find, ns_find)
            find = ns_find.get("__find_dijkstra_route")
        except Exception as e:  # noqa
            check("E0: exec __find_dijkstra_route", False, str(e))
    if find is not None:
        _nowE = int(time.time() * 1000)
        _e201 = {"destination_index": 201, "target_map": 24, "kind": "npc_jumper",
                 "npc_index": 13255, "match_name": "幽冥界", "cost_money": 500}

        def _mkq(hard=None, extra12=None):
            tab = {"12": [_e201] + list(extra12 or []), "24": [],
                   "11": [{"destination_index": 211, "target_map": 24, "kind": "map_skip"}]}
            return types.SimpleNamespace(
                chain={"dijkstra": tab},
                hop_black={(12, 201): _nowE + 600000},
                hop_black_hard=dict(hard or {}),
                prefer_free=False, chain_id="")

        # E1 既有行为: 普通黑名单堵死 → 仍"忽略黑名单再找一次"(返回该死 hop)
        rE1 = find(_mkq(), 12, 24)
        check("E1(既有行为): 普通黑名单堵死 → 回退忽略黑名单(返回 dest201)",
              isinstance(rE1, list) and rE1 and rE1[0]["destination_index"] == 201,
              rE1)
        # E2 R-1: 硬黑名单堵死 → 不回退, 直接 None
        rE2 = find(_mkq(hard={(12, 201): _nowE + 600000}), 12, 24)
        check("E2(R-1): 硬黑名单堵死 → 不回退直接 None",
              rE2 is None, rE2)
        # E3 硬黑名单过期 → 普通回退行为恢复
        rE3 = find(_mkq(hard={(12, 201): _nowE - 1000}), 12, 24)
        check("E3: 硬黑名单过期 → 回退行为恢复",
              isinstance(rE3, list) and rE3 and rE3[0]["destination_index"] == 201,
              rE3)
        # E4 硬黑名单 + 有替代路线 → 正常走替代(不受影响)
        rE4 = find(_mkq(hard={(12, 201): _nowE + 600000},
                        extra12=[{"destination_index": 14, "target_map": 11,
                                  "kind": "map_skip"}]), 12, 24)
        check("E4: 硬黑名单 + 替代路线存在 → 走替代(dest14→dest211)",
              isinstance(rE4, list) and len(rE4) == 2
              and rE4[0]["destination_index"] == 14 and rE4[1]["destination_index"] == 211,
              rE4)
        # E5 prefer_free 递归分支不绕过硬黑名单
        qE5 = _mkq(hard={(12, 201): _nowE + 600000})
        qE5.prefer_free = True
        rE5 = find(qE5, 12, 24)
        check("E5: prefer_free + 硬黑名单(无免费替代) → None", rE5 is None, rE5)

    # ============================================================ F. 源码形状(__teleport_click 停链/节流)
    check("F1: __teleport_click 停链节流(opt_loop_protect_ms, 函数开头)",
          "_opt_stop = int(getattr(quest, \"opt_loop_protect_ms\", 0) or 0)" in qe)
    _i_opt = qe.find('"OPTIMISTIC_LOOP_STOP"')
    _i_nlr = qe.find("无合法跨图路径 %d→%d(NPC %s), 停链等待处理")
    check("F2: OPTIMISTIC_LOOP_STOP 报错在'无路'分支且先于一般 NO_LEGAL_ROUTE",
          0 < _i_opt < _i_nlr and "OPT_LOOP_STOP_RETRY_MS" in qe)
    check("F3: __find_dijkstra_route 硬黑名单不回退(_hard_skipped 判定)",
          "_hard_skipped" in qe and "and not _hard_skipped:" in qe
          and "_hard_bl.get(_hk, 0) > _now_bl" in qe)
    check("F4: A5 既有接管顺序未回退(__do_walk 里回滚在报错之前)",
          0 < qe.find("_go_note = __optimistic_grid_mismatch_note(robot_object, quest, grid_data)")
          < qe.find("if now_ms >= getattr(quest, \"walk_fail_report_ms\", 0):"))

    # ============================================================ 汇总
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
