# -*- coding: utf-8 -*-
"""新手链 CLICK 死循环 / 回滚位置残留 修复自检 —— 2026-09-24

现场（只读取证，全部 bot_logs 时间线）:
  · robot0005063 (15:37→16:44, 495 次 CLICK 超时) / robot0005178 (10:35 起, 1111 次) /
    robot0005333 (12:39 起, 735 次) 三号同模式: 点陈有德(13003)选"长安东市集广场（3银）"
    → 服务端通知 1116(储备金不足) → **本地已乐观改图 11**(点中目的地即改)
    → 任务 7001115/7001170 的交付 NPC 13520 恰在**图11** → 后续全是"同图走路+点击"
    → 永不产生 destination → "跳转被拒回滚"/"A型等待分支"两个既有回滚路径都不经过
    → 服务端按真实图 5 处理点击 → 静默丢弃 → CLICK 超时死循环(永不自愈)。
  · robot0003001 ERROR=NO_LEGAL_ROUTE "地图 5 无可行路径 (3496,648)→(1792,351)":
    上轮回滚只回滚 m_mapid、没回滚 m_pose → 回滚 11→5 后本地位置仍是图11 坐标空间
    残留(3912,648 校正 3496,648) → 图5 网格无路径 → 停链。5063(10:32/13:11)/5333(11:00)
    同型。

修复（quest_engine.py，三条独立/窄条件，不改"乐观改图"本身）:
  1) __clk_optimistic_stuck_note: 三条件判定(乐观标记挂 + srv非0且!=本地 + 本地==乐观图)
     + **同一 NPC 连续点击失败 >= CLICK_OPT_ROLLBACK_N(=2)**(独立计数, __clk_track_fail,
     与 retry/dialog_retry 分离) → 判定"乐观过图未被服务端接受";
  2) __clk_rollback_optimistic: 回滚本地到服务端确认图 + 恢复"跳转前位置快照" +
     拉黑把号带到乐观图的可疑跳(last_hop_done) + prefer_free + 清跨图/行走残留;
     调用方(__recover)紧接着照旧重试 → 自动变跨图重规划(prefer_free 走 5→9→10→11 免费);
  3) 位置快照: 乐观改图时记 m_pose_before_optimistic=[mapid,x,y];
     __restore_optimistic_snapshot 在两处回滚(新增 CLICK 回滚 + 既有 __rollback_
     optimistic_and_replan)恢复 → 修 NO_LEGAL_ROUTE"回滚后起点残留";
  4) 坐标兜底写回: 第三级(g_nav_chain)命中后把坐标写回 quest.chain["npcs"](只增不减),
     后续查找直接命中静态表 —— 消"静态表缺失/仍无坐标"刷屏(5063 495 次)与其对
     g_nav_chain 的隐性依赖。
新日志锚点: "点击无响应且乐观过图未被服务端接受"。

用法: python tools/newbie_stuck_selftest.py [script_dir]
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
    def __init__(self, m_mapid=0, srv=0, opt=0):
        self.m_mapid = m_mapid
        self.m_srv_mapid = srv
        self.m_mapid_optimistic = opt
        self.m_pose = [0, 0, 0]
        self.m_pose_before_optimistic = None


def main():
    if len(sys.argv) < 2:
        print("用法: python tools/newbie_stuck_selftest.py <script 目录 或 quest_engine.py 路径>")
        return 2
    script_dir = _resolve(sys.argv[1])
    qe_path = os.path.join(script_dir, "quest_engine.py")
    if not os.path.exists(qe_path):
        print("[FAIL] 找不到 %s" % qe_path)
        return 2
    qe = open(qe_path, encoding="utf-8", errors="replace").read()

    results = []

    def check(name, cond, detail=""):
        results.append((name, bool(cond), detail))

    # ============================================================ A. 源码形状(修复新增)
    check("A1: 定义 __clk_optimistic_stuck_note(CLICK 乐观残留判定)",
          "def __clk_optimistic_stuck_note(robot_object, fail_n):" in qe)
    check("A2: 定义 __clk_rollback_optimistic(CLICK 回滚接管)",
          "def __clk_rollback_optimistic(robot_object, quest, note):" in qe)
    check("A3: 定义 __restore_optimistic_snapshot(位置快照恢复)",
          "def __restore_optimistic_snapshot(robot_object, srv_map):" in qe)
    check("A4: 定义 __clk_track_fail(同 NPC 连续失败独立计数)",
          "def __clk_track_fail(robot_object, npc_id):" in qe)
    check("A5: 常量 CLICK_OPT_ROLLBACK_N 存在(=2 门限)",
          "CLICK_OPT_ROLLBACK_N = 2" in qe)
    check("A6: 乐观改图处记录位置快照(乐观改图前图/坐标)",
          "robot_object.m_pose_before_optimistic = [" in qe
          and qe.find("robot_object.m_pose_before_optimistic = [")
          < qe.find('"msg": "跳转NPC过图成功 → 地图 %d (主动同步)"', 0) + 10 ** 9)
    check("A7: __recover 接入(先计数判定, 命中才回滚, 再照旧重试)",
          "__clk_track_fail(robot_object, t[2])" in qe
          and "__clk_rollback_optimistic(robot_object, quest, _note)" in qe)
    check("A8: 新增日志锚点(点击无响应且乐观过图未被服务端接受)",
          '"msg": "点击无响应且乐观过图未被服务端接受: %s → 回滚服务端图%s并重试"' in qe)

    # ============================================================ B. 判定函数真值表
    ns_note = {}
    frag_note = _extract_func(qe, "__clk_optimistic_stuck_note")
    check("B0: 提取 __clk_optimistic_stuck_note", frag_note is not None)
    if frag_note:
        ns_note["CLICK_OPT_ROLLBACK_N"] = 2
        try:
            exec(frag_note, ns_note)
        except Exception as e:  # noqa
            check("B0: exec __clk_optimistic_stuck_note", False, str(e))
    note = ns_note.get("__clk_optimistic_stuck_note")

    if note is not None:
        # B1 现场命中(5063: 本地 11 / 服务端 5 / 失败 2 次)
        r = note(_RO(m_mapid=11, srv=5, opt=11), 2)
        check("B1: 三条件全真+失败2次 → 命中且说明含现场坐标",
              isinstance(r, str) and "本地 11/服务端 5" in r, r)
        # B2 门限: 只失败 1 次 → 再等一轮
        r = note(_RO(m_mapid=11, srv=5, opt=11), 1)
        check("B2(反例): 失败仅 1 次 → None(先等一轮, 防真延迟误伤)", r is None, r)
        # B3 无乐观标记(正常态/已确认) → 不命中
        r = note(_RO(m_mapid=5, srv=5, opt=0), 9)
        check("B3(反例): 无乐观标记 → None", r is None, r)
        # B4 服务端确认图==本地(已同步) → 不命中
        r = note(_RO(m_mapid=11, srv=11, opt=11), 9)
        check("B4(反例): srv==本地(已确认) → None", r is None, r)
        # B5 本地≠乐观图(已回滚/已换图) → 不命中
        r = note(_RO(m_mapid=5, srv=5, opt=11), 9)
        check("B5(反例): 本地≠乐观图 → None", r is None, r)
        # B6 无服务端确认图 → 不猜, 不命中
        r = note(_RO(m_mapid=11, srv=0, opt=11), 9)
        check("B6(反例): 无 m_srv_mapid → None", r is None, r)

    # ============================================================ C. 快照恢复函数
    # __restore_optimistic_snapshot 现走统一位置写入点 robot_operator.set_pose
    #   (落墙校正+可观测); 隔离测试里注入 stub 并记录调用。
    _setpose_calls = []
    _ro_stub = types.SimpleNamespace(
        set_pose=lambda ro, x, y, source="": (
            _setpose_calls.append((x, y, source)),
            ro.m_pose.__setitem__(0, x), ro.m_pose.__setitem__(1, y)))
    sys.modules["robot_operator"] = _ro_stub
    ns_snap = {}
    frag_snap = _extract_func(qe, "__restore_optimistic_snapshot")
    check("C0: 提取 __restore_optimistic_snapshot", frag_snap is not None)
    if frag_snap:
        try:
            exec(frag_snap, ns_snap)
        except Exception as e:  # noqa
            check("C0: exec __restore_optimistic_snapshot", False, str(e))
    snap = ns_snap.get("__restore_optimistic_snapshot")
    if snap is not None:
        # C1 匹配(快照图==回滚目标图) → 走 set_pose 恢复 + 清快照
        ro = _RO(m_mapid=11, srv=5, opt=11)
        ro.m_pose = [3512, 1032, 0]
        ro.m_pose_before_optimistic = [5, 1800, 344]
        _setpose_calls[:] = []
        ok = snap(ro, 5)
        check("C1: 快照图匹配 → 恢复跳转前位置 + 清快照",
              ok is True and ro.m_pose[0] == 1800 and ro.m_pose[1] == 344
              and ro.m_pose_before_optimistic is None, (ro.m_pose, ro.m_pose_before_optimistic))
        check("C1b: 恢复走统一位置写入点 set_pose(source=rollback_optimistic)",
              _setpose_calls == [(1800, 344, "rollback_optimistic")], _setpose_calls)
        # C2 快照图 != 回滚目标图 → 不恢复(历史不同步, 不猜)
        ro2 = _RO(m_mapid=11, srv=609, opt=11)
        ro2.m_pose = [3512, 1032, 0]
        ro2.m_pose_before_optimistic = [5, 1800, 344]
        ok = snap(ro2, 609)
        check("C2(反例): 快照图!=服务端图 → 不恢复(位置不动)",
              ok is False and ro2.m_pose[0] == 3512 and ro2.m_pose[1] == 1032, ro2.m_pose)
        # C3 无快照(旧会话/未记录) → 安全 False
        ro3 = _RO(m_mapid=11, srv=5, opt=11)
        ro3.m_pose = [3512, 1032, 0]
        ok = snap(ro3, 5)
        check("C3(反例): 无快照 → False 且不崩", ok is False and ro3.m_pose[0] == 3512)

    # ============================================================ D. 计数函数
    ns_cnt = {}
    frag_cnt = _extract_func(qe, "__clk_track_fail")
    check("D0: 提取 __clk_track_fail", frag_cnt is not None)
    if frag_cnt:
        try:
            exec(frag_cnt, ns_cnt)
        except Exception as e:  # noqa
            check("D0: exec __clk_track_fail", False, str(e))
    cnt = ns_cnt.get("__clk_track_fail")
    if cnt is not None:
        ro = _RO()
        n1 = cnt(ro, 13520)
        n2 = cnt(ro, 13520)
        n3 = cnt(ro, 13520)
        check("D1: 同 NPC 连续失败累加(1→2→3)",
              (n1, n2, n3) == (1, 2, 3), (n1, n2, n3))
        n4 = cnt(ro, 99999)
        check("D2: 换 NPC 计数归 1(不跨 NPC 误累积)", n4 == 1, n4)

    # ============================================================ E. 回滚接管语义
    events = []
    ns_rb = {
        "__emit": lambda ro, ev: events.append(ev.get("msg") or ""),
        "__hop_blacklist": lambda ro, q, h: events.append("BL:%s:%s" % (
            h.get("destination_index"), h.get("_from_map"))),
    }
    # 使 __clk_rollback_optimistic 用真实 __restore_optimistic_snapshot
    frag_rb = _extract_func(qe, "__clk_rollback_optimistic")
    check("E0: 提取 __clk_rollback_optimistic", frag_rb is not None)
    if frag_rb:
        ns_rb["__restore_optimistic_snapshot"] = snap
        try:
            exec(frag_rb, ns_rb)
        except Exception as e:  # noqa
            check("E0: exec __clk_rollback_optimistic", False, str(e))
    rb = ns_rb.get("__clk_rollback_optimistic")
    if rb is not None and snap is not None:
        # E1 现场 dry-run(5063: 跳 156 把号带到 11; 快照=图5 陈有德旁)
        ro = _RO(m_mapid=11, srv=5, opt=11)
        ro.m_pose = [3512, 1032, 0]
        ro.m_pose_before_optimistic = [5, 1800, 344]
        q = types.SimpleNamespace(
            last_hop_done={"destination_index": 156, "_from_map": 5, "target_map": 11},
            walk_target=[1, 2], path_points=[[1, 2]],
            dijkstra_route=[{"destination_index": 83}], dijkstra_waiting=True,
            dijkstra_jumper={"free_race": "仙"}, dijkstra_final={"x": 1},
            prefer_free=False)
        events[:] = []
        ok = rb(ro, q, "点击 2 次无响应且乐观过图未被服务端接受(本地 11/服务端 5)")
        check("E1: 回滚到服务端图 + 清乐观标记 + 恢复跳转前位置",
              ok is True and ro.m_mapid == 5 and ro.m_mapid_optimistic == 0
              and ro.m_pose[0] == 1800 and ro.m_pose[1] == 344,
              (ro.m_mapid, ro.m_mapid_optimistic, ro.m_pose))
        check("E2: 拉黑'把号带到乐观图的可疑跳'(156/地图 5)",
              "BL:156:5" in events, events)
        check("E3: 免费优先置位 + 清跨图/行走残留",
              q.prefer_free is True and q.dijkstra_route == []
              and q.dijkstra_waiting is False and q.dijkstra_jumper is None
              and q.dijkstra_final is None and q.walk_target is None
              and q.path_points == [],
              (q.prefer_free, q.dijkstra_route, q.walk_target))
        check("E4: 日志锚点'点击无响应且乐观过图未被服务端接受'",
              any("点击无响应且乐观过图未被服务端接受" in m for m in events), events)
        # E5 反例: last_hop 是别的跳(更早的, target!=乐观图) → 不拉黑, 仍回滚
        ro2 = _RO(m_mapid=11, srv=5, opt=11)
        ro2.m_pose = [3512, 1032, 0]
        ro2.m_pose_before_optimistic = None
        q2 = types.SimpleNamespace(
            last_hop_done={"destination_index": 61, "_from_map": 11, "target_map": 45},
            walk_target=None, path_points=[], dijkstra_route=[],
            dijkstra_waiting=False, dijkstra_jumper=None, dijkstra_final=None,
            prefer_free=False)
        events[:] = []
        ok = rb(ro2, q2, "note")
        check("E5(反例): last_hop 非'带到乐观图'的跳 → 不拉黑, 仍回滚+免费优先",
              ok is True and ro2.m_mapid == 5 and q2.prefer_free is True
              and not any(str(m).startswith("BL:") for m in events), events)
        # E6 反例: 无 last_hop(旧会话) → 不拉黑不崩
        ro3 = _RO(m_mapid=11, srv=5, opt=11)
        ro3.m_pose = [3512, 1032, 0]
        q3 = types.SimpleNamespace(walk_target=None, path_points=[], dijkstra_route=[],
                                   dijkstra_waiting=False, dijkstra_jumper=None,
                                   dijkstra_final=None, prefer_free=False)
        events[:] = []
        ok = rb(ro3, q3, "note")
        check("E6(反例): 无 last_hop → 安全回滚(不拉黑)",
              ok is True and ro3.m_mapid == 5
              and not any(str(m).startswith("BL:") for m in events), events)

    # ============================================================ F. 坐标兜底写回形状
    check("F1: 第三级兜底写回 quest.chain['npcs'](只增不减)",
          '_npcs_q[str(npc_id)] = candidates' in qe
          or "_npcs_q[str(npc_id)] = candidates" in qe)
    _i_f1 = qe.find("_npcs_q[str(npc_id)] = candidates")
    _seg_f = qe[max(0, _i_f1 - 900): _i_f1 + 100] if _i_f1 > 0 else ""
    check("F2(反例): 只写回'已是 dict 的链', chain 为 None 不建最小链"
          "(防 __cmd_start_chain 空 task_order→chain_task_set=空集→忽略一切任务推送)",
          "if isinstance(quest.chain, dict):" in _seg_f
          and 'quest.chain = {"npcs": {}' not in _seg_f,
          _seg_f[-260:])
    check("F2b: 注释说明'不建链'的原因(chain_task_set 空集风险)",
          "chain_task_set" in _seg_f)
    check("F3: 写回在第三级兜底命中分支内(静态表缺失日志同段)",
          qe.find("_npcs_q[str(npc_id)] = candidates") > 0
          and abs(qe.find("_npcs_q[str(npc_id)] = candidates")
                  - qe.find("用全局导航链缓存坐标")) < 1500,
          (qe.find("_npcs_q[str(npc_id)] = candidates"), qe.find("用全局导航链缓存坐标")))

    # ============================================================ G. 不倒退(既有修复保留)
    check("G1: A 型修复保留(__optimistic_mismatch_note/__rollback_optimistic_and_replan)",
          "def __optimistic_mismatch_note(robot_object, hop):" in qe
          and "def __rollback_optimistic_and_replan(robot_object, quest, hop, note):" in qe)
    check("G2: 换路重规划 + prefer_free 两阶段保留",
          "def __replan_after_bad_hop(robot_object, quest, dest, note):" in qe
          # 2026-09-24 更新锚点: 两阶段入口重构为 _pf(同语义) + newbie_full 默认免费优先
          and '_pf = bool(getattr(quest, "prefer_free", False))' in qe
          and 'if _pf and not _ignore_blacklist:' in qe)
    check("G3: 坐标三级兜底(静态→实时→全局缓存)保留",
          "用全局导航链缓存坐标" in qe and "用实时坐标" in qe)
    check("G4: 乐观改图('主动同步'+m_mapid_optimistic 标记)本身未改",
          "robot_object.m_mapid_optimistic = int(_done.get(\"target_map\", 0) or 0)" in qe
          and '"msg": "跳转NPC过图成功 → 地图 %d (主动同步)" % _done["target_map"]})' in qe)
    check("G5: 既有回滚(__rollback_optimistic_and_replan)接入快照恢复",
          qe.find("__restore_optimistic_snapshot(robot_object, robot_object.m_mapid)")
          > 0)
    check("G6: 快照恢复走 set_pose(与位置校正链路同口径, 不绕过防穿墙兜底)",
          "robot_operator.set_pose(robot_object, int(_snap[1]), int(_snap[2])," in qe
          and 'source="rollback_optimistic"' in qe)

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
