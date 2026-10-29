# -*- coding: utf-8 -*-
"""C 型(图已共识)接换图推送空转修复自检 —— 2026-10-14

现场铁证 robot0005215 (10-13 01:26-01:27):
  WAIT_DIAG: dest=12 fm=10 srv=12 m=12 opt=0 retry=0 wait=1
  ——服务端(任务对话/越过多跳)把号传送到比本跳更远的图: 本地(m=12)与服务端
  (srv=12)双方共识, 但本跳 target 不是 12(A 分支不命中)、opt=0(乐观回滚不命中),
  于是等待一个永远不会再来的"换图推送", 空转 12 次×2.5s≈30s → TASK_STUCK
  → reghost 重登救回; 一天卡 3 次即熔断(10-13 8.7h 已 25 号, 10-12 全天 15 号)。

修复: __do_walk 等待分支(乐观检查之后、12 次兜底之前)新增 C 型判定 ——
  等满 2 次(≈5s, 与乐观门限同口径)后, 若 srv>0 且 m==srv 且 srv!=本跳起点
  (m!=from_map 已由外层保证) → 图已共识(换图事实已完成) → 按 A 分支同款
  清理(walk_target/path_points/pending_click/pending_hop/count/jumper) + return 0。

本脚本为**源码级自检**(不 import quest_engine: 它依赖机器人运行时 cnetwork 等):
  1) 静态断言 —— C 块位置(乐观之后/12 次兜底之前)/清理字段与 A 分支逐行一致/
     12 次兜底与 WAIT_DIAG 保留;
  2) 动态断言 —— 提取 __do_walk 与 __optimistic_mismatch_note 真实源码 exec 到
     桩命名空间(假 quest/robot), 跑用例 a/b/c/d;
  3) 坏版灵敏度 —— C 条件改 `if False:`(等价旧逻辑) → 用例 a 必须 FAIL。

用法: python tools/push_wait_consensus_selftest.py [quest_engine.py 路径 或 script 目录]
      不带参数默认校验仓库副本(deploy/zones/prod-240-2300/script/)。
"""
import math
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

_arg = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_ENGINE
qpath = _arg if os.path.isfile(_arg) else os.path.join(_arg, "quest_engine.py")
if not os.path.exists(qpath):
    print("[FAIL] 找不到 %s" % qpath)
    sys.exit(2)
qe_src = open(qpath, encoding="utf-8", errors="replace").read()

SCRIPT_DIR = os.path.dirname(os.path.abspath(qpath))
sys.path.insert(0, SCRIPT_DIR)
import quest_state  # noqa: E402  纯枚举模块(只 import time), 离线可用

results = []


def check(name, ok, detail=""):
    results.append((name, bool(ok), detail))


# ================================================================ 源码提取
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


# ================================================================ A. 静态形状
C_ANCHOR = "# 2026-10-14 C 型(图已共识)"
OPT_ANCHOR = "_opt_note = __optimistic_mismatch_note(robot_object, hop)"
SVC_ANCHOR = '_svc = int(getattr(robot_object, "m_srv_mapid", 0) or 0)'
CAP_ANCHOR = "if quest.walk_hop_wait_count > 12:"

i_c = qe_src.find(C_ANCHOR)
i_opt = qe_src.find(OPT_ANCHOR)
i_svc = qe_src.find(SVC_ANCHOR)
i_cap = qe_src.find(CAP_ANCHOR)
check("S1: C 型块存在且在乐观检查之后、12 次兜底之前",
      -1 not in (i_c, i_opt, i_svc, i_cap) and i_opt < i_svc < i_cap and i_c < i_svc,
      "c@%s opt@%s svc@%s cap@%s" % (i_c, i_opt, i_svc, i_cap))

# C 块文本: 注释锚点 → 12 次兜底(不含)
c_txt = qe_src[i_c:i_cap]
# A 分支文本: target_map 判定 → B 分支注释(不含)
a_start = qe_src.find('if robot_object.m_mapid == int(hop.get("target_map", 0)):')
a_end = qe_src.find("\t\t# 2026-09-24 双保险")
a_txt = qe_src[a_start:a_end]
check("S2a: 提取到 A 分支清理块与 C 块", a_start != -1 and a_end != -1 and len(c_txt) > 0)


def _clean_lines(text):
    return [l.split("#")[0].strip() for l in text.splitlines()
            if l.split("#")[0].strip().startswith("quest.")]


A_CLEAN = ["quest.walk_target = None", "quest.path_points = []",
           "quest.walk_pending_click = None", "quest.walk_pending_hop = None",
           "quest.walk_hop_wait_count = 0", "quest.dijkstra_jumper = None"]
a_clean, c_clean = _clean_lines(a_txt), _clean_lines(c_txt)
check("S2b: C 块清理字段与 A 分支逐行一致(walk_target/path_points/pending_click/"
      "pending_hop/count/jumper 6 项)",
      a_clean == A_CLEAN and c_clean == A_CLEAN,
      "A=%s C=%s" % (a_clean, c_clean))

check("S3: C 门限与乐观同口径(>=2 次, '等满 2 次'出现两处)",
      qe_src.count("if quest.walk_hop_wait_count >= 2:") >= 2)
check("S4: 12 次空转兜底保留(判不出/真延迟场景行为不变)",
      CAP_ANCHOR in qe_src and '"换图推送迟迟未到, 无法走向跳转点"' in qe_src)
check("S5: WAIT_DIAG 临时诊断保留(观察期信息)",
      "WAIT_DIAG(临时)" in qe_src)

# ================================================================ 动态装载
class Recorder(object):
    def __init__(self):
        self.events = []
        self.scheduled = []
        self.stuck = []
        self.rollback = []
        self.started = []


class FakeRobot(object):
    def __init__(self, m=0, srv=0, opt=0, pose=(100, 100)):
        self.m_mapid = m
        self.m_srv_mapid = srv
        self.m_mapid_optimistic = opt
        self.m_pose = list(pose)
        self.m_stop = True
        self.m_move_data = None


class FakeQuest(object):
    def __init__(self):
        self.walk_hop_wait_count = 0
        self.walk_target = [1, 2]          # 非 None: 证明被清理
        self.path_points = [(1, 2)]        # 非空: 证明被清理
        self.walk_pending_click = {"npc_id": 1}
        self.walk_pending_hop = {"x": 1}
        self.dijkstra_jumper = {"j": 1}
        self.state = None
        self.active = False

    def set_state(self, st):
        self.state = st


def load_engine(src):
    """提取 __do_walk + __optimistic_mismatch_note 真源码, exec 到桩命名空间。"""
    rec = Recorder()
    ns = {
        "math": math,
        "config": types.SimpleNamespace(
            walk_synced_skip_dist=20, quest_path_dither_px=0,
            quest_walk_speed=6.0, quest_walk_speed_jitter=0.0),
        "quest_state": quest_state,
        "__emit": lambda ro, ev: rec.events.append(ev),
        "__schedule": lambda q, item: rec.scheduled.append(item),
        "__report_stuck": lambda ro, q, msg: rec.stuck.append(msg),
        "__rollback_optimistic_and_replan": lambda ro, q, hop, note: (
            rec.rollback.append(note) or True),
        "__start_walk": lambda ro, q, now_ms, d: (rec.started.append(d) or 0),
        "__finish_walk": lambda ro, q, d, now_ms: None,
        "__chain_grid_for": lambda q, mapid: None,
        "__set_pose": lambda ro, x, y, src_name="": None,
        "__build_path": lambda q, mapid, fx, fy, tx, ty: [(tx, ty)],
    }
    frag_note = _extract_func(src, "__optimistic_mismatch_note")
    frag_walk = _extract_func(src, "__do_walk")
    if not frag_note or not frag_walk:
        return None, rec
    exec(frag_note, ns)
    exec(frag_walk, ns)
    return ns["__do_walk"], rec


def _has_clog(rec):
    return any("换图已到位" in (e.get("msg") or "") for e in rec.events)


def make_d(fm=10, target=11, dest=13, retry=0):
    return {"to_x": 9000, "to_y": 9000,
            "hop": {"from_map": fm, "target_map": target,
                    "destination_index": dest, "retry": retry}}


def checks_case_a(walk, rec):
    """5215 现场: srv==m==12, 本跳 fm=10/target=11, wait 0 → 第 2 次命中 C。"""
    ro = FakeRobot(m=12, srv=12, opt=0)
    q = FakeQuest()
    d = make_d()
    r1 = walk(ro, q, d, 1000)
    mid = dict(count=getattr(q, "walk_hop_wait_count", None),
               scheduled=len(rec.scheduled), stuck=len(rec.stuck), clog=_has_clog(rec))
    r2 = walk(ro, q, d, 3500)
    c = []
    c.append(("a1 第 1 次等待不命中(仅调度重试, 计数=1)",
              r1 == 0 and mid["count"] == 1 and mid["scheduled"] == 1
              and mid["stuck"] == 0 and not mid["clog"], mid))
    c.append(("a2 第 2 次(等满 2 次)命中 C: 清 6 项 walk 状态 + 计数归零",
              r2 == 0 and q.walk_hop_wait_count == 0
              and q.walk_target is None and q.path_points == []
              and q.walk_pending_click is None and q.walk_pending_hop is None
              and q.dijkstra_jumper is None,
              (r2, q.walk_hop_wait_count, q.walk_target, q.path_points,
               q.walk_pending_click, q.walk_pending_hop, q.dijkstra_jumper)))
    c.append(("a3 命中 C 后不触发 __report_stuck、不再调度重试",
              len(rec.stuck) == 0 and len(rec.scheduled) == 1,
              (rec.stuck, len(rec.scheduled))))
    c.append(("a4 C 日志锚点输出('换图已到位(本地=服务端=12...)')",
              _has_clog(rec) and any("本地=服务端=12" in (e.get("msg") or "")
                                     for e in rec.events),
              [e.get("msg") for e in rec.events]))
    c.append(("a5 不误触乐观回滚(opt=0)", len(rec.rollback) == 0, rec.rollback))
    return c


def checks_case_b(walk, rec):
    """反例(真推送延迟语义保留): srv=10(==fm) 与 m=12 不一致 → 不命中 C,
    照旧等 13 次 → __report_stuck。"""
    ro = FakeRobot(m=12, srv=10, opt=0)
    q = FakeQuest()
    d = make_d()
    rs = [walk(ro, q, d, 1000 * i) for i in range(13)]
    c = []
    c.append(("b1 srv≠m 13 次等待 → __report_stuck 恰 1 次(真推送延迟不被误杀)",
              all(r == 0 for r in rs) and len(rec.stuck) == 1
              and "换图推送迟迟未到" in rec.stuck[0],
              (len(rec.stuck), rec.stuck)))
    c.append(("b2 不命中 C: 无'换图已到位'日志, 计数未被清(13)",
              not _has_clog(rec) and q.walk_hop_wait_count == 13,
              (q.walk_hop_wait_count, [e.get("msg") for e in rec.events])))
    c.append(("b3 前 12 次全部按原逻辑调度重试(仅第 13 次报 stuck)",
              len(rec.scheduled) == 12, len(rec.scheduled)))
    return c


def checks_case_c(walk, rec):
    """防御用例: srv==m==from_map(外层 m != from_map 已挡) → 不进等待分支。"""
    ro = FakeRobot(m=10, srv=10, opt=0)
    q = FakeQuest()
    d = make_d()
    r = walk(ro, q, d, 1000)
    c = []
    c.append(("c1(防御) m==from_map 被外层条件挡 → 不进等待分支(计数=0/无重试调度)",
              r == 0 and getattr(q, "walk_hop_wait_count", 0) == 0
              and len(rec.scheduled) == 0,
              (r, getattr(q, "walk_hop_wait_count", 0), len(rec.scheduled))))
    c.append(("c2 不产生 C 日志, 走正常走路路径(__start_walk 被调)",
              not _has_clog(rec) and len(rec.started) == 1,
              (len(rec.started), [e.get("msg") for e in rec.events])))
    return c


def checks_case_d(walk, rec):
    """A 型现场(1013): opt!=0 且命中 __optimistic_mismatch_note → 仍走 A 型回滚,
    不被 C 抢(C 在乐观检查之后)。"""
    ro = FakeRobot(m=11, srv=609, opt=11)
    q = FakeQuest()
    d = make_d(fm=609, target=13, dest=61)
    r1 = walk(ro, q, d, 1000)
    r2 = walk(ro, q, d, 3500)
    c = []
    c.append(("d1 A 型场景第 2 次等待仍走回滚(回滚被调且含说明)",
              r1 == 0 and r2 == 0 and len(rec.rollback) == 1
              and "乐观过图未被服务端接受" in rec.rollback[0],
              (rec.rollback,)))
    c.append(("d2 C 未抢: 无'换图已到位'日志、无 __report_stuck",
              not _has_clog(rec) and len(rec.stuck) == 0,
              (len(rec.stuck), [e.get("msg") for e in rec.events])))
    c.append(("d3 回滚命中后不再调度重试", len(rec.scheduled) == 1,
              len(rec.scheduled)))
    return c


def run_case(fn):
    """每个用例独立装载(独立 Recorder, 避免用例间观测数据互相污染)。"""
    walk, rec = load_engine(qe_src)
    if walk is None:
        return None
    return fn(walk, rec)


walk_fn, _ = load_engine(qe_src)
check("D0: 提取并装载 __do_walk + __optimistic_mismatch_note 成功", walk_fn is not None)
if walk_fn is not None:
    for name, ok, detail in run_case(checks_case_a):
        check(name, ok, detail)
    for name, ok, detail in run_case(checks_case_b):
        check(name, ok, detail)
    for name, ok, detail in run_case(checks_case_c):
        check(name, ok, detail)
    for name, ok, detail in run_case(checks_case_d):
        check(name, ok, detail)

# ================================================================ E. 坏版灵敏度
BAD_COND = 'if _svc > 0 and _loc == _svc and _svc != int(hop.get("from_map", 0) or 0):'
bad_src, nsub = re.subn(re.escape(BAD_COND), "if False:", qe_src)
check("E1: 坏版变异成功(C 条件 → if False, 等价旧逻辑, 恰 1 处)",
      nsub == 1, "subn=%s" % nsub)
if nsub == 1:
    walk_bad, rec_bad = load_engine(bad_src)
    if walk_bad is None:
        check("E2: 坏版装载", False)
    else:
        bad_checks = checks_case_a(walk_bad, rec_bad)
        nfail_bad = sum(1 for (_n, ok, _d) in bad_checks if not ok)
        fails_bad = [n for (n, ok, _d) in bad_checks if not ok]
        check("E2: 坏版用例 a FAIL ≥1(旧逻辑复现: 第 2 次不清状态、继续空转)",
              nfail_bad >= 1, "坏版失败项=%s" % fails_bad)
        # 坏版下 b 反例仍应 PASS(变异只影响 C 条件, 不改变等待/报 stuck 语义)
        walk_bad2, rec_bad2 = load_engine(bad_src)
        bad_b = checks_case_b(walk_bad2, rec_bad2)
        check("E3: 坏版下用例 b 仍 PASS(C 是新增分支, 不改变原语义)",
              all(ok for (_n, ok, _d) in bad_b),
              [n for (n, ok, _d) in bad_b if not ok])

# ================================================================ 汇总
nfail = 0
for name, ok, detail in results:
    if ok:
        print("[PASS] %s" % name)
    else:
        nfail += 1
        print("[FAIL] %s%s" % (name, ("  (%s)" % (detail,)) if detail != "" else ""))
print("自检目标: %s" % qpath)
print("=== 结果: %s (共 %d 项断言, 失败 %d 项) ===" % (
    "PASS" if nfail == 0 else "FAIL", len(results), nfail))
sys.exit(0 if nfail == 0 else 1)
