# -*- coding: utf-8 -*-
"""乐观过图"网格相容性判据"自检（2026-09-28）。

背景（docs/04-测试/分析-20260928-卡在NPC附近排查.md §A/§P0②）:
  4 号(5048/5116/5212/5279)硬卡且永不自愈 —— 本地图被乐观改成 24(替加旁真值图 12),
  位置(4707,2768)越出图 24 网格覆盖(200×122 格=3200×1952px) → A* 起点越界必失败
  ("地图 24 无可行路径") → 既有 A 型(hop 比对)/CLICK(同 NPC 连点失败)两条判定都不
  经过此形态 → 报错/停链后重登又乐观跳图 → 闭环。
修复: `__optimistic_grid_mismatch_note`(四条件判定: 乐观标记挂着+本地==乐观图+
  服务端图!=本地+本地图网格存在+pos 越出网格覆盖) + `__grid_rollback_optimistic`
  (回滚服务端图/恢复改图前位置快照/清跨图残留下/免费优先; 不做 hop 拉黑),
  在 __do_walk 失败路径(所有连通性兜底之后、报错之前)接管。

用法: python tools/optimistic_grid_rollback_selftest.py [script_dir]
"""
import os
import sys
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
    _frag_rb = _extract_func(qe, "__grid_rollback_optimistic") or ""
    check("A6: 网格回滚不做 hop 拉黑(纯走路无 hop 可归因, 防误伤)",
          "__hop_blacklist" not in _frag_rb)
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
    events = []
    calls = []
    ns_rb = {
        "__emit": lambda ro, ev: events.append(ev.get("msg") or ""),
        "__restore_optimistic_snapshot": lambda ro, srv: (calls.append(srv), True)[1],
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
