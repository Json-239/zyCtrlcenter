# -*- coding: utf-8 -*-
"""A 型(乐观过图静默)修复自检 —— 2026-09-24

背景（只读取证，见 docs/04-测试/方案-20260924-乐观过图静默型修复（只读产出未实施）.md）:
  "任务 0 换图推送迟迟未到, 无法走向跳转点" TASK_STUCK 两天 error 行 1286 次; 其中 A 型
  (STUCK 前 60 行有"跳转NPC过图成功"且无被拒/回滚) 876 次/290 号, 100% 抓鬼/游荡语境,
  100% 含乐观改图行, 97.8% 后随"前往图N…(寻路)"并发重规划(如抓鬼换图等鬼用服务端图作起点)。
根因: 乐观改图(本地=目标图) 与 并发 route 的起点图(hop.from_map=srv 图) 分叉 →
  __do_walk "等换图推送"分支等待一个永远不会来的推送(传送未被服务端接受) → 12 次×2.5s → STUCK。
修复(不改"乐观改图"本身, 不倒退 09-22 "推送丢失也能过图"设计):
  1) __optimistic_mismatch_note: 三条件判定(乐观标记挂着 + hop.from_map==m_srv_mapid +
     本地==乐观图) → 命中返回说明文本;
  2) __rollback_optimistic_and_replan: 回滚本地到服务端确认图 + 拉黑"把号带到乐观图的
     可疑跳"(last_hop_done, target_map==乐观图) + 复用 __replan_after_bad_hop 换路;
  3) __do_walk 等待分支: 等满 2 次(≈5s) 后判定(真·推送延迟 1-3s 先确认 → 不误伤)。
新日志锚点: "乐观过图未被服务端接受" (出现=本改动生效, 取代原 12 次空转 → STUCK)。

用法: python tools/optimistic_silent_selftest.py [script_dir]
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


def main():
    if len(sys.argv) < 2:
        print("用法: python tools/optimistic_silent_selftest.py <script 目录 或 quest_engine.py 路径>")
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

    # ============================================================ A. 源码形状
    check("A1: 定义 __optimistic_mismatch_note(三条件判定)",
          "def __optimistic_mismatch_note(robot_object, hop):" in qe)
    check("A2: 三条件文本齐全(乐观标记/from_map==srv/本地==乐观图)",
          'if not _opt or not _srv:' in qe
          and 'if _fm != _srv:' in qe
          and 'if int(getattr(robot_object, "m_mapid", 0) or 0) != _opt:' in qe)
    check("A3: 定义 __rollback_optimistic_and_replan(回滚+拉黑+换路)",
          "def __rollback_optimistic_and_replan(robot_object, quest, hop, note):" in qe
          and "__hop_blacklist(robot_object, quest, _lh)" in qe
          and "__replan_after_bad_hop(robot_object, quest, hop.get(\"destination_index\"), _note)" in qe)
    check("A4: 新日志锚点存在(乐观过图未被服务端接受)",
          '"msg": "乐观过图未被服务端接受: %s → 回滚服务端图并换路" % _note})' in qe)
    check("A5: __do_walk 中判定在'等满 2 次(≈5s)'门限后(真延迟 1-3s 不误伤)",
          "if quest.walk_hop_wait_count >= 2:" in qe
          and "_opt_note = __optimistic_mismatch_note(robot_object, hop)" in qe)
    check("A6: 判定/接管在 retry>0 双保险之后(A 型与 B 型互斥, 不抢占)",
          qe.find('if int(hop.get("retry", 0) or 0) > 0:')
          < qe.find('_opt_note = __optimistic_mismatch_note(robot_object, hop)'))
    check("A7: 09-22 '乐观改图'本身未被改动(主动同步 + m_mapid_optimistic 标记仍在)",
          "robot_object.m_mapid_optimistic = int(_done.get(\"target_map\", 0) or 0)" in qe
          and '"msg": "跳转NPC过图成功 → 地图 %d (主动同步)" % _done["target_map"]})' in qe)
    check("A8: 原 12 次空转 → STUCK 兜底保留(判不出/非乐观场景行为不变)",
          "if quest.walk_hop_wait_count > 12:" in qe
          and '"换图推送迟迟未到, 无法走向跳转点"' in qe)

    # ============================================================ B. 判定函数真值表
    ns_note = {}
    frag_note = _extract_func(qe, "__optimistic_mismatch_note")
    check("B0: 提取 __optimistic_mismatch_note", frag_note is not None)
    if frag_note:
        try:
            exec(frag_note, ns_note)
        except Exception as e:  # noqa
            check("B0: exec __optimistic_mismatch_note", False, str(e))
    note = ns_note.get("__optimistic_mismatch_note")

    class _RO(object):
        def __init__(self, m_mapid=0, srv=0, opt=0):
            self.m_mapid = m_mapid
            self.m_srv_mapid = srv
            self.m_mapid_optimistic = opt

    if note is not None:
        # B1 全真命中(1013 现场: 本地 11 / 服务端 609 / 本跳属 609)
        r = note(_RO(m_mapid=11, srv=609, opt=11), {"from_map": 609, "destination_index": 13})
        check("B1: 三条件全真 → 命中且说明含现场坐标",
              isinstance(r, str) and "本地 11/服务端 609" in r and "本跳属 609" in r, r)
        # B2 无乐观标记(B 型已被拒回滚后 / 正常态) → 不命中
        r = note(_RO(m_mapid=609, srv=609, opt=0), {"from_map": 609})
        check("B2(反例): 无乐观标记(B 型/正常态) → None", r is None, r)
        # B3 无服务端确认图 → 不命中
        r = note(_RO(m_mapid=11, srv=0, opt=11), {"from_map": 0})
        check("B3(反例): 无 m_srv_mapid → None", r is None, r)
        # B4 本跳从乐观图出发(真延迟场景/正常下一跳) → 不命中
        r = note(_RO(m_mapid=11, srv=609, opt=11), {"from_map": 11})
        check("B4(反例): 本跳起点=乐观图(正常过图/真延迟) → None", r is None, r)
        # B5 本地已非乐观图(已回滚/已确认) → 不命中
        r = note(_RO(m_mapid=609, srv=609, opt=11), {"from_map": 609})
        check("B5(反例): 本地≠乐观图(已回滚/已确认) → None", r is None, r)
        # B6 异常输入安全
        r = note(_RO(m_mapid=11, srv=609, opt=11), {})
        check("B6(反例): hop 缺 from_map(=0) → None(异常安全)", r is None, r)

    # ============================================================ C. 接管动作语义
    events = []
    ns_rb = {
        "__emit": lambda ro, ev: events.append(ev.get("msg") or ""),
        "__hop_blacklist": lambda ro, q, h: events.append("BL:%s:%s" % (
            h.get("destination_index"), h.get("_from_map"))),
        "__replan_after_bad_hop": lambda ro, q, dest, note2: events.append(
            "REPLAN:%s:%s" % (dest, note2)),
    }
    frag_rb = _extract_func(qe, "__rollback_optimistic_and_replan")
    check("C0: 提取 __rollback_optimistic_and_replan", frag_rb is not None)
    if frag_rb:
        try:
            exec(frag_rb, ns_rb)
        except Exception as e:  # noqa
            check("C0: exec __rollback_optimistic_and_replan", False, str(e))
    rb = ns_rb.get("__rollback_optimistic_and_replan")
    if rb is not None:
        # C1 1013 dry-run: last_hop=175(609→11), hop=dest13
        ro = _RO(m_mapid=11, srv=609, opt=11)
        q = types.SimpleNamespace(
            last_hop_done={"destination_index": 175, "_from_map": 609, "target_map": 11},
            walk_target=[1, 2], path_points=[[1, 2]])
        events[:] = []
        ok = rb(ro, q, {"from_map": 609, "destination_index": 13},
                "乐观过图未被服务端接受(本地 11/服务端 609, 本跳属 609)")
        check("C1: 回滚到服务端图 + 清乐观标记 + 清路点",
              ok is True and ro.m_mapid == 609 and ro.m_mapid_optimistic == 0
              and q.walk_target is None and q.path_points == [],
              (ro.m_mapid, ro.m_mapid_optimistic))
        check("C2: 拉黑'把号带到乐观图的可疑跳'(175/地图 609)",
              "BL:175:609" in events, events)
        check("C3: 新日志锚点 + 换路重规划(dest 13)被调用",
              any("乐观过图未被服务端接受" in m for m in events)
              and any(str(m).startswith("REPLAN:13") for m in events), events)
        # C4 反例: last_hop 的 target_map != 乐观图(更早的跳, 不该背锅) → 不拉黑
        ro2 = _RO(m_mapid=11, srv=609, opt=11)
        q2 = types.SimpleNamespace(
            last_hop_done={"destination_index": 61, "_from_map": 11, "target_map": 45},
            walk_target=[1, 2], path_points=[[1, 2]])
        events[:] = []
        ok = rb(ro2, q2, {"from_map": 609, "destination_index": 13}, "note")
        check("C4(反例): last_hop 非'带到乐观图'的跳 → 不拉黑, 但仍回滚+换路",
              ok is True and ro2.m_mapid == 609 and not any(str(m).startswith("BL:") for m in events)
              and any(str(m).startswith("REPLAN:13") for m in events), events)
        # C5 反例: 无 last_hop(旧会话/NPC 跳未记录) → 不拉黑不崩, 仍回滚+换路
        ro3 = _RO(m_mapid=11, srv=609, opt=11)
        q3 = types.SimpleNamespace(walk_target=None, path_points=[])
        events[:] = []
        ok = rb(ro3, q3, {"from_map": 609, "destination_index": 13}, "note")
        check("C5(反例): 无 last_hop → 安全回滚+换路(不拉黑)",
              ok is True and ro3.m_mapid == 609
              and not any(str(m).startswith("BL:") for m in events)
              and any(str(m).startswith("REPLAN:13") for m in events), events)

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
