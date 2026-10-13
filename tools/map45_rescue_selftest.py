# -*- coding: utf-8 -*-
"""map45 救援 + 跨图禁穿 45 护栏 自检 —— 2026-10-08

背景（见 docs/04-测试/分析-20260930-map45死域与ERROR处置.md + 2026-10-08 复查）：
  45（瑶池回廊）7 连通域；BFS 会规划「12→45→…」「49→45→…」的穿 45 中转路线
  （13348 下凡小仙 / 13559 小彤 付费传送进 45），而 45 出图 map_skip 被服务端拒绝
  （dest57/60/62 实测）→ 进 45 即困死。服主给定官方出图路线：
  本域 NPC →「跳到瑶池回廊中心（1银）」→ 主域中心 → 灵儿 →「长安城中（10银）」→ 图12。

本批改动（机器人端，双副本）：
  ① 新增 map_rescue.py（救援状态机：ALIGN 校准回滚 / PICK A* 选 NPC / GOTO 走+点 /
     WAIT 对话选项匹配 / LAND 等落地）；
  ② share_daily.py 三处转交（dispatch_cmd / on_task_event / tick —— 均零影响未激活路径）；
  ③ quest_engine.py：__find_dijkstra_route 加「跨图禁穿 45」（from/to 均非 45 时跳过
     进 45 的边；保留 45 出图与目标=45）+ stop 联动终止救援；
  ④ auto_roam.py / auto_summon.py 加救援占位闸（防自发游荡/空闲采购抢导航）。

断言分组：
  S45:*  = 45 救援/护栏行为 —— 对"变异坏版"（去掉护栏两行 / 去掉救援分支）跑同断言必 FAIL
  S48:*  = 48/47 前置段（2026-10-10）—— exec 真实 __tick/__tick_seg4847/__enter_hop_seg，
           用例：48→TO47 hop 参数 / 47→TO45 / srv 优先完成判定 / 90s 超时重试≤3 超限 FAIL /
           段外图终态 / 非 45/47/48 不触发新段（CTRL）/ hop 与链数据交叉（FIX）
  S34:*  = 34 出图段（2026-10-12）—— exec 真实 __tick/__tick_seg34/__enter_hop34/__pick_hop34，
           用例：主域位置→选主域候选（203 最近）/ 排除后次选（25）/ 域0 位置→选 23/219 /
           超时重试≤3 超限 FAIL（失败候选排除）/ srv 优先完成与校正 / 全不可达 FAIL /
           跳转NPC 对话（空格选项）选目标 / 真网格域隔离（主域↔域0 互不可达）/ 与链数据交叉
  CTRL:* = 控制组（既有行为不回归；坏版也应 PASS —— 证明断言不是"全盘必挂"）
  FIX:*  = 形状/一致性（双副本一致或"批内待部署"、NPC 坐标与链数据交叉、行尾 LF）

用法：
  python tools/map45_rescue_selftest.py            # 全量期望 PASS
  python tools/map45_rescue_selftest.py --expect-bad   # 坏版灵敏度（源码定向变异）

边界：只读、不启动机器人、不改任何文件（变异在内存中做）。
"""
import glob
import json
import math
import os
import random
import re
import sys
import time
import types
from collections import deque

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ACT = os.path.join(ROOT, "deploy", "zones", "prod-240-2300", "script")   # zyCtrlcenter 副本
XM = os.path.join(os.path.dirname(ROOT), "xm", "2d-xiyou-server", "robot",
                  "deploy", "single_robot_zy", "script")                  # 活跃副本
CHAIN = os.path.join(ROOT, "data", "chains", "newbie_full.json")

PASS, FAIL = [], []
EXPECT_BAD = "--expect-bad" in sys.argv


def ok(name, cond, detail=""):
    (PASS if cond else FAIL).append(name)
    print("  [%s] %s%s" % ("OK" if cond else "FAIL", name,
                           (" —— " + str(detail)) if detail else ""))


def read(path):
    with open(path, encoding="utf-8", errors="replace") as f:
        return f.read()


def extract_func(src, name):
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


# ==================================================================
# 一、BFS 护栏行为（exec 真实 __find_dijkstra_route）
# ==================================================================
class _FakeDiag(object):
    def log(self, *a):
        pass


def _inject_batch_consts(ns, src_text, fn_text):
    """并发批次兼容：fn 里引用的全大写模块常量 → 从源码逐行解析进 exec 命名空间。

    背景：批 I（2026-10-10，C1 死边硬跳过）给 __find_dijkstra_route 加了
    STATIC_DEAD_HOPS / _MAP45_BRIDGE_FROM 等模块级常量；exec 提取函数时需一并注入。
    找不到/表达式不可求值的名字跳过（旧版源码本就不含）。
    """
    for name in sorted(set(re.findall(r"\b([A-Z_][A-Z0-9_]{2,})\b", fn_text))):
        if name in ns:
            continue
        m = re.search(r"^%s\s*=\s*([^\n#]+)" % re.escape(name), src_text, re.M)
        if not m:
            continue
        try:
            ns[name] = eval(m.group(1).strip(), {})
        except Exception:
            pass


def make_bfs(src_text, mutate=None):
    fn = extract_func(src_text, "__find_dijkstra_route")
    if fn is None:
        return None
    if mutate:
        fn2 = mutate(fn)
        if fn2 is None:
            return None
        fn = fn2
    ns = {
        "time": time,
        "diag": _FakeDiag(),
        "g_chain_dijkstra_cache": {},
        "__hop_key": (lambda fm, e: (int(fm or 0), e.get("destination_index"))),
    }
    _inject_batch_consts(ns, src_text, fn)
    try:
        exec(compile(fn, "<bfs>", "exec"), ns)
    except Exception as e:
        print("    [exec 失败] %s" % e)
        return None
    return ns.get("__find_dijkstra_route")


def _mut_strip_guard(fn):
    """变异坏版：删除护栏语句 —— _avoid_45 赋值（含反斜杠续行）+ if/continue 两行。"""
    out = []
    lines = fn.splitlines()
    i = 0
    while i < len(lines):
        s = lines[i].strip()
        if s.startswith("if _avoid_45 and nxt == 45"):
            i += 1
            continue
        if s.startswith("continue") and "# 2026-10-08 护栏" in s:
            i += 1
            continue
        if s.startswith("_avoid_45"):
            # 删除整条语句（含后续反斜杠续行；2026-10-10 C1 批后该语句为多行）
            while i < len(lines) and lines[i].rstrip().endswith("\\"):
                i += 1
            i += 1
            continue
        out.append(lines[i])
        i += 1
    return "\n".join(out)


def make_quest(dijkstra):
    return types.SimpleNamespace(
        chain={"dijkstra": dijkstra}, chain_id="", prefer_free=False,
        hop_black={}, hop_black_hard={}, opt_loop_protect_ms=0,
        nav_replan=0, nav_replan_ts=0)


def route_maps(route):
    return [(int(e.get("_from_map", 0) or 0), int(e.get("target_map", 0) or 0)) for e in route]


def test_bfs():
    print("== 一、BFS 护栏行为（exec 真实函数体 + 真链数据） ==")
    # 坏版灵敏度优先用备份旧版（.bak_20261008_map45rescue，含真实"无护栏"代码）；
    # 备份缺失时退回内存定向变异。
    qe = read(os.path.join(ACT, "quest_engine.py"))
    qe_bad = None
    for _d in (ACT, XM):
        _bak = os.path.join(_d, "quest_engine.py.bak_20261008_map45rescue")
        if os.path.exists(_bak):
            qe_bad = read(_bak)
            break
    if EXPECT_BAD and qe_bad is not None:
        qe = qe_bad        # 坏版模式：主断言也跑在旧版上（护栏断言应 FAIL）
    data = json.load(open(CHAIN, encoding="utf-8"))
    dijk = data["dijkstra"]
    bfs = make_bfs(qe)
    if bfs is None:
        ok("S45:bfs-function-present", False, "未提取到 __find_dijkstra_route")
        return
    q = make_quest(dijk)

    # S45-1: 12→49 原唯一路线穿 45（12→45→49）→ 护栏后必须无路径
    r = bfs(q, 12, 49)
    ok("S45:no-route-12-49-bypass45", r is None,
       "12→49 = %s（应 None：唯一路线是穿 45 的 12→45→49）" % (route_maps(r) if r else None))
    # S45-2: 45→12（出图）保留：仍命中灵儿边 dest258
    r = bfs(q, 45, 12)
    ok("S45:45-to-12-kept", bool(r) and r[0].get("destination_index") == 258,
       "45→12 = %s" % (route_maps(r) if r else None))
    # S45-3: 目标=45 保留：12→45（13348）仍可用
    r = bfs(q, 12, 45)
    ok("S45:12-to-45-kept", bool(r) and r[0].get("destination_index") == 295,
       "12→45 = %s" % (route_maps(r) if r else None))
    # S45-4: 49→24 不穿 45，护栏前后一致
    r = bfs(q, 49, 24)
    ok("S45:49-to-24-no45", bool(r) and all(int(e.get("target_map") or 0) != 45 for e in r),
       "49→24 = %s" % (route_maps(r) if r else None))
    # S45-5: 45→15（困号回主世界路线）保留且首跳=灵儿
    r = bfs(q, 45, 15)
    ok("S45:45-to-15-kept", bool(r) and r[0].get("destination_index") == 258,
       "45→15 = %s" % (route_maps(r) if r else None))
    # S45-6: 全表扫描——凡 from/to 均非 45 的结果里不得出现"45 作为中间节点"
    bad = []
    maps = [int(k) for k in dijk.keys()]
    sample = [(a, b) for a in maps[:20] for b in (12, 24, 49, 5, 11, 23)]
    for a, b in sample:
        if a == b or a == 45 or b == 45:
            continue
        r = bfs(q, a, b)
        if not r:
            continue
        mids = [int(e.get("target_map") or 0) for e in r[:-1]]
        if 45 in mids:
            bad.append((a, b, route_maps(r)))
    ok("S45:no-transit-45-scan", not bad, "穿 45 样本: %s" % bad[:3])

    # CTRL: 无关图对不受影响
    # 注：48→49 断言随批 I（2026-10-10 C1 死边硬跳过）更新 —— STATIC_DEAD_HOPS 把
    #   dest144 永久拒边直接跳过（原"护栏不碰 48→49 有路 dest144"已被 C1 取代；
    #   从 48 出图改由救援链 48→47→45 承担，见 map_rescue TO47/TO45 段）。
    r = bfs(q, 48, 49)
    ok("S48:bfs-48-49-dead-edge-skipped", r is None,
       "48→49 = %s（应 None：C1 死边硬跳过 dest144）" % (route_maps(r) if r else None))
    r = bfs(q, 12, 24)
    ok("CTRL:12-24-untouched", bool(r) and all(int(e.get("target_map") or 0) != 45 for e in r),
       "12→24 = %s" % (route_maps(r) if r else None))

    # 2026-10-12 S34: 34→23/219 硬跳过（域0 跳转点，主域号 A* 到不了 → NO_LEGAL_ROUTE）
    r = bfs(q, 34, 12)
    ok("S34:bfs-34-12-first-hop-main",
       bool(r) and int(r[0].get("destination_index", 0)) == 25,
       "34→12 首跳 dest=%s（应 25 主域；dest219 已硬跳过）" % (
           r[0].get("destination_index") if r else None))
    r = bfs(q, 34, 35)
    ok("S34:bfs-34-35-no-dom0-hop",
       r is None or int(r[0].get("destination_index", 0)) not in (23, 219),
       "34→35 = %s（不得以 dest23 域0 首跳规划）" % (route_maps(r) if r else None))
    r = bfs(q, 34, 17)
    ok("S34:bfs-34-17-main-edge-kept",
       bool(r) and int(r[0].get("destination_index", 0)) == 25,
       "34→17 = %s（主域 dest25 保留）" % (route_maps(r) if r else None))
    # 坏版灵敏度：去掉 (34,23)/(34,219) 硬跳过 → 34→12 首跳应变回 dest219
    qe_ng = qe.replace(" (34, 23), (34, 219)", "")
    bfs_ng = make_bfs(qe_ng)
    if bfs_ng is None:
        ok("S34:mutant-build", False, "无护栏变异版构建失败")
    else:
        rng = bfs_ng(q, 34, 12)
        ok("S34:mutant-no-guard-34-12-crosses-dom0",
           bool(rng) and int(rng[0].get("destination_index", 0)) == 219,
           "无护栏版 34→12 首跳 = %s（应 219，S34:bfs-34-12-first-hop-main 应 FAIL）" % (
               rng[0].get("destination_index") if rng else None))

    # 坏版灵敏度：去掉护栏两行后 12→49 应变为"穿 45"（S45-1 断言 FAIL）
    bad_bfs = make_bfs(qe, mutate=_mut_strip_guard)
    if bad_bfs is None:
        ok("S45:mutant-build", False, "变异版构建失败")
        return
    rb = bad_bfs(q, 12, 49)
    ok("S45:mutant-12-49-crosses45",
       bool(rb) and any(int(e.get("target_map") or 0) == 45 for e in rb),
       "变异版 12→49 = %s（应穿 45，S45:no-route-12-49-bypass45 应 FAIL）" % (
           route_maps(rb) if rb else None))


# ==================================================================
# 二、选项匹配/防抖（exec 真实 __on_show_dialog + __click_option）
# ==================================================================
class _FakeRobot(object):
    def __init__(self):
        self.account = ["t@x"]
        self.sent = []
        self.m_pose = [600, 440]
        self.m_mapid = 45

    def send_message(self, mid, data):
        self.sent.append((mid, list(data)))
        return 0


def make_dialog(src_text, mutate=None):
    on = extract_func(src_text, "__on_show_dialog")
    ck = extract_func(src_text, "__click_option")
    on34 = extract_func(src_text, "__on_show_dialog_34")
    ck34 = extract_func(src_text, "__click_option_34")
    if on is None or ck is None or on34 is None or ck34 is None:
        return None
    ns = {
        "time": time,
        "_now_ms": (lambda: int(time.time() * 1000)),
        "_log": (lambda rb, lv, msg: None),
        "protocol3": types.SimpleNamespace(C2S_CLICK_DIALOG=90075, C2S_NOTIFY_POSITION=90076),
        "OPT_CENTER": "瑶池回廊中心",
        "OPT_CHANGAN": "长安城中",
        "S_LAND": "LAND", "S_WAIT": "WAIT_DLG", "S_GOTO": "GOTO",
        "S_ALIGN": "ALIGN", "S_PICK": "PICK", "S_DONE": "DONE", "S_FAIL": "FAIL",
        # 2026-10-12：__on_show_dialog 的 34 分支改判 state==S_EXIT34（原 seg=hop34 为死代码）。
        "S_EXIT34": "EXIT34",
    }
    try:
        exec(compile(ck34, "<ck34>", "exec"), ns)
        exec(compile(on34, "<on34>", "exec"), ns)
        exec(compile(ck, "<ck>", "exec"), ns)
        exec(compile(on, "<on>", "exec"), ns)
    except Exception as e:
        print("    [exec 失败] %s" % e)
        return None
    return ns["__on_show_dialog"]


def test_dialog():
    print("== 二、对话选项匹配（exec 真实函数体） ==")
    mr = read(os.path.join(ACT, "map_rescue.py"))
    on = make_dialog(mr)
    if on is None:
        ok("S45:dialog-functions-present", False, "未提取到 __on_show_dialog/__click_option")
        return
    robot = _FakeRobot()

    # S45: 长安票匹配（灵儿，seg=changan）
    st = {"active": True, "state": "WAIT_DLG", "seg": "changan", "npc": 13338,
          "last_option_ms": 0}
    d = [0, 0, 13338, "content", 0, [("请送我去长安城中（10银）", 0), ("离开", 1)]]
    r = on(robot, st, d)
    ok("S45:dialog-changan-pick", r is True and st["state"] == "LAND" and
       robot.sent and robot.sent[-1][1][1] == 0,
       "r=%s state=%s sent=%s" % (r, st["state"], robot.sent[-1:]))
    # S45: 中心票匹配（兰儿，seg=center）
    st = {"active": True, "state": "GOTO", "seg": "center", "npc": 13340,
          "last_option_ms": 0}
    d = [0, 0, 13340, "content", 0,
         [("请送我去瑶池回廊中心（1银）", 0), ("请送我去月宫入口（1银）", 0), ("离开", 1)]]
    r = on(robot, st, d)
    ok("S45:dialog-center-pick", r is True and st["state"] == "LAND" and
       robot.sent[-1][1][1] == 0,
       "r=%s state=%s" % (r, st["state"]))
    # S45: 无目标票 → 点"离开"收尾（不挂起）
    st = {"active": True, "state": "WAIT_DLG", "seg": "changan", "npc": 13338,
          "last_option_ms": 0}
    d = [0, 0, 13338, "content", 0, [("兑换物品", 0), ("离开", 1)]]
    r = on(robot, st, d)
    ok("S45:dialog-fallback-close", r is True and robot.sent[-1][1][1] == 1,
       "r=%s sent=%s" % (r, robot.sent[-1:]))
    # S45: 防抖（3 秒内重复推送不重复点）
    st = {"active": True, "state": "LAND", "seg": "changan", "npc": 13338,
          "last_option_ms": int(time.time() * 1000)}
    n0 = len(robot.sent)
    d = [0, 0, 13338, "content", 0, [("请送我去长安城中（10银）", 0), ("离开", 1)]]
    r = on(robot, st, d)
    ok("S45:dialog-debounce", r is True and len(robot.sent) == n0, "r=%s" % r)
    # CTRL: 非目标 NPC 不消化
    st = {"active": True, "state": "WAIT_DLG", "seg": "changan", "npc": 13340,
          "last_option_ms": 0}
    d = [0, 0, 13338, "content", 0, [("请送我去长安城中（10银）", 0), ("离开", 1)]]
    r = on(robot, st, d)
    ok("CTRL:dialog-other-npc-passthrough", r is False, "r=%s" % r)
    # CTRL: npc=0 引导对话不消化
    st = {"active": True, "state": "WAIT_DLG", "seg": "changan", "npc": 13338,
          "last_option_ms": 0}
    d = [0, 0, 0, "content", 0, [("接受任务", 0), ("离开", 1)]]
    r = on(robot, st, d)
    ok("CTRL:dialog-npc0-passthrough", r is False, "r=%s" % r)

    # ---- 34 段（2026-10-12）：跳转NPC 选项按目标名"去空格包容"匹配 ----
    # S34: 河伯 dialog 选项带空格（服务端 dest_names "女 儿国"）→ 仍选女儿国（#0）
    st = {"active": True, "state": "EXIT34", "seg": "hop34", "npc": 13264,
          "hop34_match": "女儿国", "hop34_target": "女儿国", "last_option_ms": 0}
    d = [0, 0, 13264, "content", 0,
         [("请送我去女 儿国（1银）", 0), ("请送我去通天河右岸（1银）", 0), ("离开", 1)]]
    r = on(robot, st, d)
    ok("S34:dialog-hebo-space-pick-daughter",
       r is True and robot.sent[-1][1] == [0, 0] and st["state"] == "EXIT34",
       "r=%s sent=%s state=%s" % (r, robot.sent[-1:], st["state"]))
    # S34: 杨帆 dialog 选项带空格（"长安城中 品墨斋附近"）→ 仍选长安（#0）
    st = {"active": True, "state": "EXIT34", "seg": "hop34", "npc": 13287,
          "hop34_match": "长安城中品墨斋附近", "hop34_target": "长安城中",
          "last_option_ms": 0}
    d = [0, 0, 13287, "content", 0,
         [("请送我去长安城中 品墨斋附近（6银）", 0), ("请送我去通天河左岸（1银）", 0), ("离开", 1)]]
    r = on(robot, st, d)
    ok("S34:dialog-yangfan-space-pick-changan",
       r is True and robot.sent[-1][1] == [0, 0] and st["state"] == "EXIT34",
       "r=%s sent=%s" % (r, robot.sent[-1:]))
    # S34: 无目标选项 → 点"离开"收尾（不挂起、不误点其它目的地）
    st = {"active": True, "state": "EXIT34", "seg": "hop34", "npc": 13287,
          "hop34_match": "长安城中品墨斋附近", "hop34_target": "长安城中",
          "last_option_ms": 0}
    d = [0, 0, 13287, "content", 0, [("兑换物品", 0), ("离开", 1)]]
    r = on(robot, st, d)
    ok("S34:dialog-nomatch-close",
       r is True and robot.sent[-1][1] == [0, 1], "r=%s sent=%s" % (r, robot.sent[-1:]))
    # S34: 防抖（3 秒内重复推送不重复点）
    st = {"active": True, "state": "EXIT34", "seg": "hop34", "npc": 13264,
          "hop34_match": "女儿国", "hop34_target": "女儿国",
          "last_option_ms": int(time.time() * 1000)}
    n0 = len(robot.sent)
    d = [0, 0, 13264, "content", 0, [("请送我去女 儿国（1银）", 0), ("离开", 1)]]
    r = on(robot, st, d)
    ok("S34:dialog-debounce", r is True and len(robot.sent) == n0, "r=%s" % r)
    # CTRL: 34 段激活时其它 NPC 对话不消化（放行默认处理）
    st = {"active": True, "state": "EXIT34", "seg": "hop34", "npc": 13264,
          "hop34_match": "女儿国", "hop34_target": "女儿国", "last_option_ms": 0}
    d = [0, 0, 13287, "content", 0, [("请送我去长安城中 品墨斋附近（6银）", 0), ("离开", 1)]]
    r = on(robot, st, d)
    ok("CTRL:dialog-34-other-npc-passthrough", r is False, "r=%s" % r)


# ==================================================================
# 三、A*/域假设（真网格数据；救援选 NPC 的可行性前提）
# ==================================================================
def test_domain():
    print("== 三、A*/域判定（真网格数据） ==")
    try:
        sys.path.insert(0, os.path.join(ROOT, "tools"))
        import roam_map_connectivity_scan as scan
        rp = scan.load_grid_module(ACT)
        data = json.load(open(CHAIN, encoding="utf-8"))
        g = rp.MapGrid(45, data["map_grids"]["45"])
        comp_id, sizes = scan.components(g)
        fp = rp.GridPathFinder(g)
        pts = {
            "域2起点": (600, 440), "域5起点": (450, 1778), "域3起点": (1400, 900),
            "域6起点": (1528, 1608),
        }
        npcs = {"灵儿": (1162, 848), "兰儿": (438, 425), "琴儿": (682, 1524)}
        dom = {k: comp_id.get(g.to_grid(*v)) for k, v in pts.items()}
        ok("S45:domain-map", dom == {"域2起点": 2, "域5起点": 5, "域3起点": 3, "域6起点": 6},
           dom)
        def reach(a, b):
            return fp.find_path(a[0], a[1], b[0], b[1]) is not None
        ok("S45:domain2-cannot-reach-linger",
           not reach(pts["域2起点"], npcs["灵儿"]) and reach(pts["域2起点"], npcs["兰儿"]),
           "域2→灵儿=%s 域2→兰儿=%s" % (
               reach(pts["域2起点"], npcs["灵儿"]), reach(pts["域2起点"], npcs["兰儿"])))
        ok("S45:domain5-cannot-reach-linger",
           not reach(pts["域5起点"], npcs["灵儿"]) and reach(pts["域5起点"], npcs["琴儿"]),
           "域5→灵儿=%s 域5→琴儿=%s" % (
               reach(pts["域5起点"], npcs["灵儿"]), reach(pts["域5起点"], npcs["琴儿"])))
        ok("S45:domain3-can-reach-linger", reach(pts["域3起点"], npcs["灵儿"]),
           "域3→灵儿=%s" % reach(pts["域3起点"], npcs["灵儿"]))
        ok("S45:domain6-cannot-reach-any",
           not any(reach(pts["域6起点"], n) for n in npcs.values()),
           "域6 三 NPC 可达: %s" % {k: reach(pts["域6起点"], v) for k, v in npcs.items()})
        # 中心点（桥落点）在域3，且从中心可走到灵儿（桥后第二段）
        ok("S45:center-domain3", comp_id.get(g.to_grid(1074, 903)) == 3)
        ok("S45:center-reach-linger", reach((1074, 903), npcs["灵儿"]),
           "中心→灵儿=%s" % reach((1074, 903), npcs["灵儿"]))
    except Exception as e:
        ok("S45:domain-suite", False, "异常: %s" % e)


# ==================================================================
# 四、48/47 前置段（exec 真实 __tick/__tick_seg4847/__enter_hop_seg）
# ==================================================================
_SEG_CONSTS = ("S_ALIGN", "S_PICK", "S_GOTO", "S_WAIT", "S_LAND", "S_DONE", "S_FAIL",
               "S_TO47", "S_TO45", "MAP_BRIDGE48", "MAP_YUHUA47", "DEST_4847", "DEST_4745",
               "HOP_TIMEOUT_MS", "HOP_RETRIES",
               "S_EXIT34", "MAP_TONGHE34", "HOP34_TIMEOUT_MS", "HOP34_RETRIES")


def _src_const(src, name):
    """从源码解析常量 RHS（int/字符串/简单表达式），保证测试绑定真实值。"""
    m = re.search(r"^%s\s*=\s*([^\n#]+)" % re.escape(name), src, re.M)
    if not m:
        return None
    try:
        return eval(m.group(1).strip(), {})
    except Exception:
        return None


def _list_const(src, name):
    """从源码解析整型列表常量（可跨行）。"""
    m = re.search(r"^%s\s*=\s*\[([^\]]*)\]" % re.escape(name), src, re.M)
    if not m:
        return None
    return [int(x) for x in re.findall(r"\d+", m.group(1))]


def _expr_const(src, name):
    """从源码解析任意 Python 字面量表达式常量（括号配平扫描；用于 HOPS34 这类 dict 列表）。"""
    m = re.search(r"^%s\s*=\s*" % re.escape(name), src, re.M)
    if not m:
        return None
    start = m.end()
    depth, i, in_str = 0, start, None
    while i < len(src):
        ch = src[i]
        if in_str:
            if ch == "\\":
                i += 2
                continue
            if ch == in_str:
                in_str = None
        elif ch in "'\"":
            in_str = ch
        elif ch in "([{":
            depth += 1
        elif ch in ")]}":
            depth -= 1
            if depth == 0:
                try:
                    return eval(src[start:i + 1], {})
                except Exception:
                    return None
        i += 1
    return None


class _SegFakeQE(object):
    """假 quest_engine：记录 __execute_hop 等调用（属性名用 setattr 防 mangle）。"""

    def __init__(self):
        self.execute_hop_calls = []   # [(hop_copy, now_ms)]
        self.do_action_calls = 0
        self.tick_walk_calls = 0
        self.rollbacks = []
        setattr(self, "__execute_hop", self._execute_hop)
        setattr(self, "__do_action", self._do_action)
        setattr(self, "__tick_walk", self._tick_walk)
        setattr(self, "__restore_optimistic_snapshot", self._restore)

    def _execute_hop(self, robot, quest, hop, now_ms):
        self.execute_hop_calls.append((dict(hop), now_ms))
        # 模拟真实：map_skip 未 _walk_done 时调度走路（quest.pending）
        if not hop.get("_walk_done"):
            quest.pending = {"type": "walk", "at_ms": now_ms + 200,
                             "data": {"to_x": hop.get("x", 0), "to_y": hop.get("y", 0)}}

    def _do_action(self, robot, quest, p, now_ms):
        self.do_action_calls += 1
        quest.pending = None

    def _tick_walk(self, robot, quest, now_ms):
        self.tick_walk_calls += 1

    def _restore(self, robot, srv):
        self.rollbacks.append(srv)
        return False


class _SegFakeRobot(object):
    def __init__(self, mapid, srv=0):
        self.m_mapid = mapid
        self.m_srv_mapid = srv
        self.m_mapid_optimistic = 0
        self.m_account = ["t@x"]
        self.m_pose = [100, 200]
        self.m_quest = None


class _SegFakeQuest(object):
    def __init__(self):
        self.pending = None
        self.walk_target = None
        self.dijkstra_waiting = False


class _Seg34FakeQE(_SegFakeQE):
    """34 段假 quest_engine：额外提供 __build_path（域规则可达性）。

    same_domain_only=True（默认，模拟真实网格）：主域 x<3000、域0 x>=3000，跨域不可达
    （真数据实测同型：主域 (840,2024) 到不了 (4107,252)/(3707,435)）；
    False = 全可达（用于"重试超限"等与域无关的用例）。
    """

    def __init__(self, same_domain_only=True):
        _SegFakeQE.__init__(self)
        self.same_domain_only = same_domain_only
        self.build_calls = 0
        setattr(self, "__build_path", self._build_path)

    def _build_path(self, quest, mapid, fx, fy, tx, ty):
        self.build_calls += 1
        if int(mapid) != 34:
            return None
        if not self.same_domain_only:
            return [(int(tx), int(ty))]
        if (int(fx) >= 3000) == (int(tx) >= 3000):
            return [(int(tx), int(ty))]
        return None


def _use_fake_qe(qe):
    saved = sys.modules.get("quest_engine")
    sys.modules["quest_engine"] = qe
    return saved


def _restore_qe(saved):
    if saved is None:
        sys.modules.pop("quest_engine", None)
    else:
        sys.modules["quest_engine"] = saved


def _load_seg_ns(src):
    fns = []
    for nm in ("__make_hop", "__cur_map", "__sync_srv_map", "__enter_45_flow",
               "__enter_hop_seg", "__tick_seg4847", "__finish", "__tick",
               "__reach", "__pick_hop34", "__enter_hop34", "__tick_seg34"):
        f = extract_func(src, nm)
        if f is None:
            print("    [缺失函数] %s" % nm)
            return None
        fns.append(f)
    miss = [c for c in _SEG_CONSTS if _src_const(src, c) is None]
    if miss:
        print("    [缺失常量] %s" % miss)
        return None
    ns = {"time": time, "math": math, "_log": (lambda rb, lv, msg: None)}
    for c in _SEG_CONSTS:
        ns[c] = _src_const(src, c)
    ns["HOPS34"] = _expr_const(src, "HOPS34")
    if ns["HOPS34"] is None:
        print("    [缺失常量] HOPS34")
        return None
    # 既有 45 流程桩（控制组验证"45 号仍走 ALIGN"，不被新段接管）
    ns["__st_align"] = (lambda rb, st2, q: st2.__setitem__("_align_called", True))
    ns["__st_pick"] = (lambda rb, st2, q, n: None)
    ns["__st_goto"] = (lambda rb, st2, q, n: None)
    ns["__st_land"] = (lambda rb, st2, q, n: None)
    exec(compile("\n\n\n".join(fns), "<mr-seg>", "exec"), ns)
    return ns


def test_seg4847():
    print("== 四、48/47 前置段（exec 真实函数体） ==")
    mr = read(os.path.join(ACT, "map_rescue.py"))
    ns = _load_seg_ns(mr)
    if ns is None:
        ok("S48:functions-present", False, "未提取到 __tick_seg4847/__enter_hop_seg 等")
        return
    TO47, TO45 = ns["S_TO47"], ns["S_TO45"]
    HO, HR = ns["HOP_TIMEOUT_MS"], ns["HOP_RETRIES"]

    # S48-1: 图中 48 → 真实 __tick 分发 → 开 48→47 段（hop 参数逐项断言）
    qe = _SegFakeQE(); rb = _SegFakeRobot(48, 0); q = _SegFakeQuest(); rb.m_quest = q
    st = {"active": True, "state": ns["S_ALIGN"], "seg": "changan"}
    saved = _use_fake_qe(qe)
    try:
        ns["__tick"](rb, st, 1000000)
    finally:
        _restore_qe(saved)
    h0 = qe.execute_hop_calls[0][0] if qe.execute_hop_calls else {}
    ok("S48:tick-48-enters-to47",
       st.get("state") == TO47 and len(qe.execute_hop_calls) == 1 and
       h0.get("kind") == "map_skip" and int(h0.get("destination_index", 0)) == 64 and
       int(h0.get("x", 0)) == 97 and int(h0.get("y", 0)) == 1088 and
       int(h0.get("from_map", 0)) == 48 and int(h0.get("target_map", 0)) == 47 and
       q.pending is not None and st.get("hop_retry") == 0,
       "state=%s hop=%s" % (st.get("state"), h0))

    # S48-2: 图中 47 → 开 47→45 段（dest62 @(1687,1640)）
    qe = _SegFakeQE(); rb = _SegFakeRobot(47, 0); q = _SegFakeQuest(); rb.m_quest = q
    st = {"active": True, "state": ns["S_ALIGN"], "seg": "changan"}
    saved = _use_fake_qe(qe)
    try:
        ns["__tick"](rb, st, 1000000)
    finally:
        _restore_qe(saved)
    h0 = qe.execute_hop_calls[0][0] if qe.execute_hop_calls else {}
    ok("S48:tick-47-enters-to45",
       st.get("state") == TO45 and len(qe.execute_hop_calls) == 1 and
       int(h0.get("destination_index", 0)) == 62 and
       int(h0.get("x", 0)) == 1687 and int(h0.get("y", 0)) == 1640 and
       int(h0.get("from_map", 0)) == 47 and int(h0.get("target_map", 0)) == 45,
       "state=%s hop=%s" % (st.get("state"), h0))

    # S48-3: 48→47 完成（srv=47）→ 自动接 47→45 段；清 dijkstra_waiting
    qe = _SegFakeQE(); rb = _SegFakeRobot(47, 47); q = _SegFakeQuest(); rb.m_quest = q
    q.dijkstra_waiting = True
    st = {"active": True, "state": TO47, "seg": "changan",
          "hop": ns["__make_hop"](64, 97, 1088, 48, 47), "hop_ms": 2000000, "hop_retry": 0}
    saved = _use_fake_qe(qe)
    try:
        ns["__tick"](rb, st, 2000000)
    finally:
        _restore_qe(saved)
    h0 = qe.execute_hop_calls[0][0] if qe.execute_hop_calls else {}
    ok("S48:complete-48-47-next-seg",
       st.get("state") == TO45 and q.dijkstra_waiting is False and
       int(h0.get("destination_index", 0)) == 62 and int(h0.get("from_map", 0)) == 47 and
       int(h0.get("target_map", 0)) == 45,
       "state=%s calls=%d hop=%s" % (st.get("state"), len(qe.execute_hop_calls), h0))

    # S48-4: 服务端确认优先 —— 本地 47（幻象）但 srv=48 → 不完成、不转段
    qe = _SegFakeQE(); rb = _SegFakeRobot(47, 48); q = _SegFakeQuest(); rb.m_quest = q
    st = {"active": True, "state": TO47, "seg": "changan",
          "hop": ns["__make_hop"](64, 97, 1088, 48, 47), "hop_ms": 2000000, "hop_retry": 0}
    saved = _use_fake_qe(qe)
    try:
        ns["__tick"](rb, st, 2000000)
    finally:
        _restore_qe(saved)
    ok("S48:srv-priority-blocks-false-complete",
       st.get("state") == TO47 and not qe.execute_hop_calls,
       "state=%s calls=%d (m=47 srv=48)" % (st.get("state"), len(qe.execute_hop_calls)))

    # S48-5: 90s 无完成 → 重试 ≤3（重走跳转点：_walk_done 清 False 并重发）；第 4 次超时 → FAIL
    qe = _SegFakeQE(); rb = _SegFakeRobot(48, 0); q = _SegFakeQuest(); rb.m_quest = q
    hop = ns["__make_hop"](64, 97, 1088, 48, 47)
    hop["_walk_done"] = True
    st = {"active": True, "state": TO47, "seg": "changan", "hop": hop,
          "hop_ms": 3000000, "hop_retry": 0}
    saved = _use_fake_qe(qe)
    t = 3000000
    try:
        for _i in range(HR):
            t += HO + 1000
            q.pending = None          # 模拟上一轮 pending 已被消费
            q.walk_target = None
            ns["__tick"](rb, st, t)
            walk_done_reset = (hop.get("_walk_done") is False)
            hop["_walk_done"] = True  # 模拟重走又走完（等下次完成判定/超时）
        retried = len(qe.execute_hop_calls)
        t += HO + 1000
        q.pending = None
        q.walk_target = None
        ns["__tick"](rb, st, t)
    finally:
        _restore_qe(saved)
    ok("S48:timeout-retry3-then-fail",
       st.get("state") == ns["S_FAIL"] and st.get("active") is False and
       st.get("hop_retry") == HR + 1 and retried == HR and walk_done_reset and
       "交人工" in (st.get("verdict") or ""),
       "state=%s retry=%s calls=%d verdict=%s" % (
           st.get("state"), st.get("hop_retry"), retried, st.get("verdict")))

    # S48-6: 段中到段外图（653）→ 终态 DONE（交中控/人工，不误继续）
    qe = _SegFakeQE(); rb = _SegFakeRobot(653, 0); q = _SegFakeQuest(); rb.m_quest = q
    st = {"active": True, "state": TO47, "seg": "changan",
          "hop": ns["__make_hop"](64, 97, 1088, 48, 47), "hop_ms": 4000000, "hop_retry": 0}
    saved = _use_fake_qe(qe)
    try:
        ns["__tick"](rb, st, 4000000)
    finally:
        _restore_qe(saved)
    ok("S48:out-of-seg-done",
       st.get("state") == ns["S_DONE"] and st.get("active") is False and
       "653" in (st.get("verdict") or "") and not qe.execute_hop_calls,
       "state=%s verdict=%s" % (st.get("state"), st.get("verdict")))

    # CTRL: 非 45/47/48 图（25）不触发新段（保持既有"已不在 45"终态）
    qe = _SegFakeQE(); rb = _SegFakeRobot(25, 0); q = _SegFakeQuest(); rb.m_quest = q
    st = {"active": True, "state": ns["S_ALIGN"], "seg": "changan"}
    saved = _use_fake_qe(qe)
    try:
        ns["__tick"](rb, st, 1000000)
    finally:
        _restore_qe(saved)
    ok("CTRL:other-map-no-new-seg",
       st.get("state") == ns["S_DONE"] and not qe.execute_hop_calls and
       "已不在 45" in (st.get("verdict") or ""),
       "state=%s verdict=%s" % (st.get("state"), st.get("verdict")))

    # CTRL: 45 图仍走既有 ALIGN 流程（不被新段接管）
    qe = _SegFakeQE(); rb = _SegFakeRobot(45, 0); q = _SegFakeQuest(); rb.m_quest = q
    st = {"active": True, "state": ns["S_ALIGN"], "seg": "changan"}
    saved = _use_fake_qe(qe)
    try:
        ns["__tick"](rb, st, 1000000)
    finally:
        _restore_qe(saved)
    ok("CTRL:45-flow-unchanged",
       bool(st.get("_align_called")) and not qe.execute_hop_calls and
       st.get("state") == ns["S_ALIGN"],
       "align=%s calls=%d state=%s" % (
           st.get("_align_called"), len(qe.execute_hop_calls), st.get("state")))

    # FIX: hop 参数与链数据交叉核对（48→47 dest64 @(97,1088)；47→45 dest62 @(1687,1640)）
    try:
        data = json.load(open(CHAIN, encoding="utf-8"))
        dj = data["dijkstra"]
        e1 = [e for e in dj.get("48", []) if int(e.get("target_map", 0)) == 47]
        e2 = [e for e in dj.get("47", []) if int(e.get("target_map", 0)) == 45]
        ok("FIX:hop-4847-chain-cross",
           bool(e1) and e1[0]["destination_index"] == 64 and
           int(e1[0]["x"]) == 97 and int(e1[0]["y"]) == 1088, str(e1[:1]))
        ok("FIX:hop-4745-chain-cross",
           bool(e2) and e2[0]["destination_index"] == 62 and
           int(e2[0]["x"]) == 1687 and int(e2[0]["y"]) == 1640, str(e2[:1]))
    except Exception as e:
        ok("FIX:hop-chain-cross", False, str(e))


# ==================================================================
# 四之二、34 出图段（exec 真实 __tick/__tick_seg34/__enter_hop34/__pick_hop34）
# ==================================================================
def test_seg34():
    print("== 四之二、34 出图段（exec 真实函数体） ==")
    mr = read(os.path.join(ACT, "map_rescue.py"))
    ns = _load_seg_ns(mr)
    if ns is None:
        ok("S34:functions-present", False, "未提取到 __tick_seg34/__enter_hop34 等")
        return
    EXIT34 = ns["S_EXIT34"]
    HO34, HR34 = ns["HOP34_TIMEOUT_MS"], ns["HOP34_RETRIES"]

    # S34-1: 主域位置（840,2024）→ 选主域可达最近 = 河伯 203
    # （d²(203)=722k < d²(25)=2474k；域0 的 23/219 被 A* 判不可达）
    qe = _Seg34FakeQE(); rb = _SegFakeRobot(34, 0); rb.m_pose = [840, 2024]
    q = _SegFakeQuest(); rb.m_quest = q
    st = {"active": True, "state": ns["S_ALIGN"], "seg": "changan"}
    saved = _use_fake_qe(qe)
    try:
        ns["__tick"](rb, st, 1000000)
    finally:
        _restore_qe(saved)
    h0 = qe.execute_hop_calls[0][0] if qe.execute_hop_calls else {}
    ok("S34:tick-main-domain-picks-203",
       st.get("state") == EXIT34 and len(qe.execute_hop_calls) == 1 and
       int(h0.get("destination_index", 0)) == 203 and h0.get("kind") == "npc_jumper" and
       int(h0.get("npc_index", 0)) == 13264 and int(h0.get("from_map", 0)) == 34 and
       int(h0.get("target_map", 0)) == 17 and h0.get("match_name") == "女儿国" and
       st.get("npc") == 13264 and st.get("hop34_retry") == 0 and q.pending is not None,
       "state=%s hop=%s npc=%s" % (st.get("state"), h0, st.get("npc")))

    # S34-2: 主域 + 203 已排除 → 次选 25（map_skip @147,612，免费）
    qe = _Seg34FakeQE(); rb = _SegFakeRobot(34, 0); rb.m_pose = [840, 2024]
    q = _SegFakeQuest(); rb.m_quest = q
    st = {"active": True, "state": ns["S_ALIGN"], "seg": "changan",
          "hop34_bad": [203], "npc": 13264}
    saved = _use_fake_qe(qe)
    try:
        ns["__tick"](rb, st, 1000000)
    finally:
        _restore_qe(saved)
    h0 = qe.execute_hop_calls[0][0] if qe.execute_hop_calls else {}
    ok("S34:main-domain-exclude203-picks-25",
       st.get("state") == EXIT34 and
       int(h0.get("destination_index", 0)) == 25 and h0.get("kind") == "map_skip" and
       int(h0.get("x", 0)) == 147 and int(h0.get("y", 0)) == 612 and
       int(h0.get("target_map", 0)) == 17 and st.get("npc") == 0,
       "state=%s hop=%s" % (st.get("state"), h0))

    # S34-3: 域0 位置（4100,300）→ 选域0 可达最近 = 23（d²=2353 < d²(219)=172k）
    qe = _Seg34FakeQE(); rb = _SegFakeRobot(34, 0); rb.m_pose = [4100, 300]
    q = _SegFakeQuest(); rb.m_quest = q
    st = {"active": True, "state": ns["S_ALIGN"], "seg": "changan"}
    saved = _use_fake_qe(qe)
    try:
        ns["__tick"](rb, st, 1000000)
    finally:
        _restore_qe(saved)
    h0 = qe.execute_hop_calls[0][0] if qe.execute_hop_calls else {}
    ok("S34:dom0-picks-23",
       st.get("state") == EXIT34 and
       int(h0.get("destination_index", 0)) == 23 and h0.get("kind") == "map_skip" and
       int(h0.get("x", 0)) == 4107 and int(h0.get("y", 0)) == 252 and
       int(h0.get("target_map", 0)) == 35,
       "state=%s hop=%s" % (st.get("state"), h0))

    # S34-4: 域0 + 23 已排除 → 219（杨帆，npc_jumper；st["npc"]/dijkstra_jumper 同步）
    qe = _Seg34FakeQE(); rb = _SegFakeRobot(34, 0); rb.m_pose = [4100, 300]
    q = _SegFakeQuest(); rb.m_quest = q
    st = {"active": True, "state": ns["S_ALIGN"], "seg": "changan", "hop34_bad": [23]}
    saved = _use_fake_qe(qe)
    try:
        ns["__tick"](rb, st, 1000000)
    finally:
        _restore_qe(saved)
    h0 = qe.execute_hop_calls[0][0] if qe.execute_hop_calls else {}
    _dj = getattr(q, "dijkstra_jumper", None) or {}
    ok("S34:dom0-exclude23-picks-219",
       st.get("state") == EXIT34 and
       int(h0.get("destination_index", 0)) == 219 and h0.get("kind") == "npc_jumper" and
       int(h0.get("npc_index", 0)) == 13287 and int(h0.get("target_map", 0)) == 12 and
       h0.get("match_name") == "长安城中品墨斋附近" and st.get("npc") == 13287 and
       int(_dj.get("npc_id", 0)) == 13287 and
       _dj.get("match_name") == "长安城中品墨斋附近",
       "state=%s hop=%s npc=%s dj=%s" % (st.get("state"), h0, st.get("npc"), _dj))

    # S34-5: 90s 无完成 → 重试 ≤3（失败候选进排除表、重评估）；第 4 次超时 → FAIL
    # 全可达 fake（same_domain_only=False）+ 初始 hop=203：重试顺序 25→219→23
    qe = _Seg34FakeQE(same_domain_only=False)
    rb = _SegFakeRobot(34, 0); rb.m_pose = [100, 200]
    q = _SegFakeQuest(); rb.m_quest = q
    hop = dict(ns["HOPS34"][1])            # 203
    st = {"active": True, "state": EXIT34, "seg": "changan", "hop34": hop,
          "hop34_ms": 3000000, "hop34_retry": 0, "hop34_bad": [], "npc": 13264}
    saved = _use_fake_qe(qe)
    t = 3000000
    try:
        for _i in range(HR34):
            t += HO34 + 1000
            q.pending = None
            q.walk_target = None
            ns["__tick"](rb, st, t)
        retried = len(qe.execute_hop_calls)
        t += HO34 + 1000
        q.pending = None
        q.walk_target = None
        ns["__tick"](rb, st, t)
    finally:
        _restore_qe(saved)
    ok("S34:timeout-retry3-then-fail",
       st.get("state") == ns["S_FAIL"] and st.get("active") is False and
       st.get("hop34_retry") == HR34 + 1 and retried == HR34 and
       sorted(st.get("hop34_bad") or []) == [23, 25, 203, 219] and
       len(st.get("hop34_bad") or []) == 4 and "交人工" in (st.get("verdict") or ""),
       "state=%s retry=%s calls=%d bad=%s verdict=%s" % (
           st.get("state"), st.get("hop34_retry"), retried, st.get("hop34_bad"),
           st.get("verdict")))

    # S34-6: 完成判定（srv 优先）——本地 34 / srv 17 → DONE 并清残留
    qe = _Seg34FakeQE(); rb = _SegFakeRobot(34, 17); q = _SegFakeQuest(); rb.m_quest = q
    q.dijkstra_waiting = True
    st = {"active": True, "state": EXIT34, "seg": "changan",
          "hop34": dict(ns["HOPS34"][0]), "hop34_ms": 4000000, "npc": 13264}
    saved = _use_fake_qe(qe)
    try:
        ns["__tick"](rb, st, 4000000)
    finally:
        _restore_qe(saved)
    ok("S34:srv-priority-complete-done",
       st.get("state") == ns["S_DONE"] and st.get("active") is False and
       q.dijkstra_waiting is False and q.dijkstra_jumper is None and st.get("npc") == 0 and
       not qe.execute_hop_calls and "17" in (st.get("verdict") or ""),
       "state=%s verdict=%s" % (st.get("state"), st.get("verdict")))

    # S34-7: 开段前 srv 校正 —— 本地 34（幻象）/ srv 17 → 回滚后 DONE（不误进 34 段）
    qe = _Seg34FakeQE(); rb = _SegFakeRobot(34, 17); q = _SegFakeQuest(); rb.m_quest = q
    st = {"active": True, "state": ns["S_ALIGN"], "seg": "changan"}
    saved = _use_fake_qe(qe)
    try:
        ns["__tick"](rb, st, 1000000)
    finally:
        _restore_qe(saved)
    ok("S34:srv-sync-blocks-false-34",
       st.get("state") == ns["S_DONE"] and getattr(rb, "m_mapid", 0) == 17 and
       qe.rollbacks == [17] and not qe.execute_hop_calls,
       "state=%s mapid=%s rollbacks=%s" % (st.get("state"), rb.m_mapid, qe.rollbacks))

    # S34-8: 候选全不可达/全排除 → FAIL 记日志（交人工）
    qe = _Seg34FakeQE(); rb = _SegFakeRobot(34, 0); rb.m_pose = [100, 200]
    q = _SegFakeQuest(); rb.m_quest = q
    st = {"active": True, "state": ns["S_ALIGN"], "seg": "changan",
          "hop34_bad": [25, 203]}
    saved = _use_fake_qe(qe)
    try:
        ns["__tick"](rb, st, 1000000)
    finally:
        _restore_qe(saved)
    ok("S34:all-unreachable-fail",
       st.get("state") == ns["S_FAIL"] and st.get("active") is False and
       not qe.execute_hop_calls and "不可达" in (st.get("verdict") or ""),
       "state=%s verdict=%s" % (st.get("state"), st.get("verdict")))

    # S34-9 (2026-10-13): 战斗中暂停超时——不烧重试额度、不倒计候选、刷新计时基准
    qe = _Seg34FakeQE(); rb = _SegFakeRobot(34, 0); rb.m_pose = [840, 2024]
    rb.m_fight_state = 1
    q = _SegFakeQuest(); rb.m_quest = q
    st = {"active": True, "state": EXIT34, "seg": "changan",
          "hop34": dict(ns["HOPS34"][0]), "hop34_ms": 100000, "npc": 0}
    t = 100000 + HO34 + 5000  # 早已超过超时窗口，但处于战斗 → 应暂停而非重试
    saved = _use_fake_qe(qe)
    try:
        ns["__tick"](rb, st, t)
    finally:
        _restore_qe(saved)
    ok("S34:fight-pauses-timeout",
       st.get("state") == EXIT34 and int(st.get("hop34_retry", 0) or 0) == 0 and
       int(st.get("hop34_ms", 0) or 0) == t and not (st.get("hop34_bad") or []) and
       st.get("hop34_was_fight") is True and not qe.execute_hop_calls,
       "retry=%s ms=%s bad=%s was_fight=%s calls=%d" % (
           st.get("hop34_retry"), st.get("hop34_ms"), st.get("hop34_bad"),
           st.get("hop34_was_fight"), len(qe.execute_hop_calls)))

    # S34-10: 战斗结束 → 自动重发跳转（重新选路执行一次；was_fight 清除；不烧重试额度）
    qe = _Seg34FakeQE(); rb = _SegFakeRobot(34, 0); rb.m_pose = [840, 2024]
    q = _SegFakeQuest(); rb.m_quest = q
    st = {"active": True, "state": EXIT34, "seg": "changan",
          "hop34": dict(ns["HOPS34"][0]), "hop34_ms": 100000,
          "hop34_was_fight": True, "npc": 0}
    saved = _use_fake_qe(qe)
    try:
        ns["__tick"](rb, st, 100000 + HO34 + 5000)
    finally:
        _restore_qe(saved)
    h0 = qe.execute_hop_calls[0][0] if qe.execute_hop_calls else {}
    ok("S34:fight-end-resends",
       st.get("state") == EXIT34 and len(qe.execute_hop_calls) == 1 and
       st.get("hop34_was_fight") is False and int(st.get("hop34_retry", 0) or 0) == 0 and
       int(h0.get("destination_index", 0)) == 203,
       "state=%s calls=%d was_fight=%s hop=%s" % (
           st.get("state"), len(qe.execute_hop_calls), st.get("hop34_was_fight"), h0))

    # S34-11: 战斗结束时已换图成功 → 完成判定优先（不误重发）
    qe = _Seg34FakeQE(); rb = _SegFakeRobot(34, 17); q = _SegFakeQuest(); rb.m_quest = q
    st = {"active": True, "state": EXIT34, "seg": "changan",
          "hop34_was_fight": True, "hop34_ms": 100000}
    saved = _use_fake_qe(qe)
    try:
        ns["__tick"](rb, st, 100000 + HO34 + 5000)
    finally:
        _restore_qe(saved)
    ok("S34:fight-end-already-left-done",
       st.get("state") == ns["S_DONE"] and st.get("active") is False and
       not qe.execute_hop_calls,
       "state=%s calls=%d" % (st.get("state"), len(qe.execute_hop_calls)))

    # S34-12: 战斗中不开段（不把跳转发进战斗被吞；状态不动、零命令）
    qe = _Seg34FakeQE(); rb = _SegFakeRobot(34, 0); rb.m_pose = [840, 2024]
    rb.m_fight_state = 1
    q = _SegFakeQuest(); rb.m_quest = q
    st = {"active": True, "state": ns["S_ALIGN"], "seg": "changan"}
    saved = _use_fake_qe(qe)
    try:
        ns["__tick"](rb, st, 1000000)
    finally:
        _restore_qe(saved)
    ok("S34:fight-blocks-open-segment",
       st.get("state") == ns["S_ALIGN"] and not qe.execute_hop_calls and
       st.get("hop34_ms") is None,
       "state=%s calls=%d" % (st.get("state"), len(qe.execute_hop_calls)))

    # CTRL: 非 34 图（35）不触发新段（保持既有"已不在 45"终态）
    qe = _Seg34FakeQE(); rb = _SegFakeRobot(35, 0); q = _SegFakeQuest(); rb.m_quest = q
    st = {"active": True, "state": ns["S_ALIGN"], "seg": "changan"}
    saved = _use_fake_qe(qe)
    try:
        ns["__tick"](rb, st, 1000000)
    finally:
        _restore_qe(saved)
    ok("CTRL:other-map-35-no-34-seg",
       st.get("state") == ns["S_DONE"] and not qe.execute_hop_calls and
       "已不在 45" in (st.get("verdict") or ""),
       "state=%s verdict=%s" % (st.get("state"), st.get("verdict")))

    # CTRL: 34 图 + 48/47 段中状态（TO45）→ 4847 段接管（34 段不越权）
    qe = _Seg34FakeQE(); rb = _SegFakeRobot(34, 0); q = _SegFakeQuest(); rb.m_quest = q
    st = {"active": True, "state": ns["S_TO45"], "seg": "changan",
          "hop": ns["__make_hop"](62, 1687, 1640, 47, 45), "hop_ms": 1000000,
          "hop_retry": 0}
    saved = _use_fake_qe(qe)
    try:
        ns["__tick"](rb, st, 1000000)
    finally:
        _restore_qe(saved)
    ok("CTRL:34-map-mid-4847-state-not-hijacked",
       st.get("state") == ns["S_DONE"] and not qe.execute_hop_calls and
       "48/47" in (st.get("verdict") or ""),
       "state=%s verdict=%s" % (st.get("state"), st.get("verdict")))

    # FIX: HOPS34 与链数据 newbie_full.dijkstra["34"] 逐字段交叉 + 排除项不动
    try:
        data = json.load(open(CHAIN, encoding="utf-8"))
        dj34 = data["dijkstra"].get("34", [])
        by_dest = {int(e["destination_index"]): e for e in dj34}
        bad, excluded_ok = [], True
        for h in ns["HOPS34"]:
            e = by_dest.get(h["destination_index"])
            if e is None or int(e.get("x")) != h["x"] or int(e.get("y")) != h["y"] or \
                    int(e.get("target_map", 0)) != h["target_map"] or e.get("kind") != h["kind"]:
                bad.append(h["destination_index"])
                continue
            if h["kind"] == "npc_jumper" and (
                    int(e.get("npc_index", 0)) != h["npc_index"] or
                    e.get("match_name") != h["match_name"]):
                bad.append("meta%s" % h["destination_index"])
        picked = [h["destination_index"] for h in ns["HOPS34"]]
        for d in (141, 202, 220):            # 水牢 / 两个图内自跳 → 不得出现在候选
            if d in picked:
                excluded_ok = False
        ok("FIX:hops34-chain-cross", not bad, "不一致: %s" % bad)
        ok("FIX:hops34-exclusions-not-picked", excluded_ok and
           by_dest.get(141, {}).get("target_map") == 655 and
           by_dest.get(202, {}).get("target_map") == 34 and
           by_dest.get(220, {}).get("target_map") == 34,
           "候选=%s" % picked)
        # BFS 黑名单（STATIC_DEAD_HOPS 34→23/219）不影响救援段直投：候选仍含 23/219
        ok("S34:rescue-keeps-23-219-despite-bfs-guard",
           23 in picked and 219 in picked,
           "候选=%s（域0 救援仍可用 dest23/219）" % picked)
    except Exception as e:
        ok("FIX:hops34-chain-cross", False, str(e))


# ==================================================================
# 四之三、34 域隔离（真网格数据；救援可行性前提）
# ==================================================================
def test_domain34():
    print("== 四之三、34 域隔离（真网格数据） ==")
    try:
        sys.path.insert(0, os.path.join(ROOT, "tools"))
        import roam_map_connectivity_scan as scan
        rp = scan.load_grid_module(ACT)
        data = json.load(open(CHAIN, encoding="utf-8"))
        g = rp.MapGrid(34, data["map_grids"]["34"])
        comp_id, sizes = scan.components(g)
        fp = rp.GridPathFinder(g)
        main = (840, 2024)
        dom0 = (4100, 300)
        hid = comp_id.get(g.to_grid(*main))
        ok("S34:grid-two-domains-plus-singleton",
           sizes.count(3225) == 1 and sizes.count(888) == 1 and 1 in sizes and
           comp_id.get(g.to_grid(*dom0)) != hid and hid is not None,
           "sizes=%s main_id=%s" % (sizes, hid))

        def reach(a, b):
            return fp.find_path(a[0], a[1], b[0], b[1]) is not None

        pts_main = ((147, 612), (1682, 1908))
        pts_dom0 = ((4107, 252), (3707, 435))
        ok("S34:grid-main-reaches-25-203-not-dom0",
           all(reach(main, p) for p in pts_main) and
           not any(reach(main, p) for p in pts_dom0),
           "主域→25/203=%s；→23/219=%s" % (
               [reach(main, p) for p in pts_main], [reach(main, p) for p in pts_dom0]))
        ok("S34:grid-dom0-reaches-23-219-not-main",
           all(reach(dom0, p) for p in pts_dom0) and
           not any(reach(dom0, p) for p in pts_main),
           "域0→23/219=%s；→25/203=%s" % (
               [reach(dom0, p) for p in pts_dom0], [reach(dom0, p) for p in pts_main]))
        # 三个入图落点都在主域（35/655 为可走格；17→34 落阻挡格=就近落点）
        ok("S34:grid-entry-points-main",
           comp_id.get(g.to_grid(86, 1318)) == hid and
           comp_id.get(g.to_grid(368, 928)) == hid and
           bool(g.blocked(*g.to_grid(4269, 1963))),
           "35入口=%s 655入口=%s 17入口blocked=%s" % (
               comp_id.get(g.to_grid(86, 1318)), comp_id.get(g.to_grid(368, 928)),
               g.blocked(*g.to_grid(4269, 1963))))
    except Exception as e:
        ok("S34:grid-suite", False, "异常: %s" % e)


# ==================================================================
# 五、形状/一致性
# ==================================================================
def test_shape():
    print("== 五、形状/一致性 ==")
    mr = read(os.path.join(ACT, "map_rescue.py"))
    sd = read(os.path.join(ACT, "share_daily.py"))
    qe = read(os.path.join(ACT, "quest_engine.py"))
    ar = read(os.path.join(ACT, "auto_roam.py"))
    asm = read(os.path.join(ACT, "auto_summon.py"))

    for tok in ("S_ALIGN", "S_PICK", "S_GOTO", "S_WAIT", "S_LAND", "S_DONE", "S_FAIL",
                "NPC_LINGER = 13338", "NPC_QINER = 13339", "NPC_LANER = 13340",
                "CENTER_XY = (1074, 903)", "CHANGAN_MAP = 12",
                "OPT_CENTER = \"瑶池回廊中心\"", "OPT_CHANGAN = \"长安城中\"",
                "on_quest_stop", "on_task_event", "def tick(",
                "S_TO47 = \"TO47\"", "S_TO45 = \"TO45\"",
                "MAP_BRIDGE48 = 48", "MAP_YUHUA47 = 47",
                "DEST_4847 = 64", "DEST_4745 = 62",
                "HOP_TIMEOUT_MS = 90 * 1000", "HOP_RETRIES = 3",
                "def __tick_seg4847(", "def __enter_hop_seg(",
                "S_EXIT34 = \"EXIT34\"", "MAP_TONGHE34 = 34",
                "HOP34_TIMEOUT_MS = 90 * 1000", "HOP34_RETRIES = 3",
                "HOPS34 = [", "def __pick_hop34(", "def __enter_hop34(",
                "def __tick_seg34(", "def __on_show_dialog_34("):
        ok("S45:map_rescue-has-%s" % tok, tok in mr)
    # share_daily 三处转交
    ok("S45:share_daily-dispatch-transfer", 'map45_rescue' in sd and
       sd.count("map_rescue") >= 3, "map_rescue 出现 %d 次" % sd.count("map_rescue"))
    ok("S45:quest_engine-guard-present", "_avoid_45" in qe)
    ok("S45:quest_engine-stop-hook", "map_rescue.on_quest_stop" in qe)
    ok("S45:auto_roam-gate", "map_rescue" in ar and "is_active" in ar)
    ok("S45:auto_summon-gate", "map_rescue" in asm and "is_active" in asm)
    # CTRL: 既有键未被破坏
    ok("CTRL:share_daily-old-start-branch", "return __cmd_start(robot_object, g, cmd)" in sd)
    ok("CTRL:quest_engine-bad-jumpers-kept", "BAD_JUMPERS = (10147, 13255)" in qe)
    # config 游荡名单（2026-10-10：world 删 48/49，wild 删 49；25/45 在排除表）
    cfg = read(os.path.join(ACT, "config.py"))
    world = _list_const(cfg, "robot_roam_world_maps")
    wild = _list_const(cfg, "robot_roam_wild_maps")
    excl = _list_const(cfg, "robot_roam_exclude_maps")
    ok("FIX:cfg-world-no-48-49", world is not None and 48 not in world and 49 not in world
       and 47 in world and 46 in world and 609 in world,
       "world n=%s 48=%s 49=%s 47=%s" % (len(world) if world else None,
                                         48 in (world or []), 49 in (world or []),
                                         47 in (world or [])))
    ok("FIX:cfg-wild-no-49", wild is not None and 49 not in wild,
       "wild n=%s 49=%s" % (len(wild) if wild else None, 49 in (wild or [])))
    ok("FIX:cfg-exclude-25-45", excl is not None and 25 in excl and 45 in excl, str(excl))
    # 2026-10-12：通天河 34 双端剔除（world 32 / wild 17 / exclude 含 34 + 注释标记）
    ok("FIX:cfg-world-no-34-n32", world is not None and 34 not in world and
       len(world) == 32, "world n=%s 34=%s" % (len(world) if world else None,
                                              34 in (world or [])))
    ok("FIX:cfg-wild-no-34-n17", wild is not None and 34 not in wild and
       len(wild) == 17, "wild n=%s 34=%s" % (len(wild) if wild else None,
                                            34 in (wild or [])))
    ok("FIX:cfg-exclude-34", excl is not None and 34 in excl, str(excl))
    ok("FIX:cfg-34-comment-marker", "2026-10-12" in cfg and "通天河" in cfg)
    # NPC 坐标与链数据交叉一致
    try:
        data = json.load(open(CHAIN, encoding="utf-8"))
        npcs = data["npcs"]
        same = (npcs["13338"][0][:3] == [45, 1162, 848] and
                npcs["13339"][0][:3] == [45, 682, 1524] and
                npcs["13340"][0][:3] == [45, 438, 425])
        ok("FIX:npc-pos-cross-check", same, str({k: npcs[k] for k in ("13338", "13339", "13340")}))
    except Exception as e:
        ok("FIX:npc-pos-cross-check", False, str(e))
    # 双副本一致性（5 文件）：允许"批内待部署"态 —— XM 仍等于该文件的某份 .bak_* 备份
    # （ACT 为待部署前向版；批 I 实测 map_rescue/quest_engine 在途）。其余不一致=FAIL。
    aligned = True
    detail = []
    for f in ("map_rescue.py", "share_daily.py", "quest_engine.py", "auto_roam.py",
              "auto_summon.py"):
        pa, px = os.path.join(ACT, f), os.path.join(XM, f)
        if not os.path.exists(px):
            aligned = False
            detail.append("%s 缺失" % f)
            continue
        ta, tx = read(pa), read(px)
        if ta == tx:
            continue
        baks = sorted(glob.glob(pa + ".bak_*"))
        if any(read(bp) == tx for bp in baks):
            detail.append("%s 待部署（XM=批次前备份）" % f)
            continue
        aligned = False
        detail.append("%s 不一致" % f)
    ok("FIX:dual-copy-sync", aligned, "; ".join(detail) or "5 文件一致（或批内待部署）")
    # 行尾 LF（新文件）
    ok("FIX:lf-map_rescue", "\r" not in mr)
    ok("FIX:lf-config", "\r" not in cfg)
    ok("FIX:has-map_rescue-module", "def dispatch_cmd" in mr and "def is_active" in mr)


def main():
    print("map45 救援 + 护栏自检（%s）" % ("坏版灵敏度" if EXPECT_BAD else "常规"))
    if EXPECT_BAD:
        # 坏版模式：只跑 BFS 组（备份旧版无护栏；map_rescue 等新文件不存在，不参与）
        test_bfs()
        print("\n== 汇总（坏版） ==")
        print("PASS %d / FAIL %d" % (len(PASS), len(FAIL)))
        if FAIL:
            print("FAIL 列表:")
            for n in FAIL:
                print("  -", n)
        target = "S45:no-route-12-49-bypass45"
        ctrl_fail = [n for n in FAIL if n.startswith("CTRL")]
        if target in FAIL and not ctrl_fail:
            print("坏版灵敏度 OK：%s 按预期 FAIL 且 CTRL 全 PASS" % target)
            return 0
        print("坏版灵敏度异常：target FAIL=%s / CTRL FAIL=%s" % (
            target in FAIL, ctrl_fail))
        return 1
    test_bfs()
    test_dialog()
    test_domain()
    test_seg4847()
    test_seg34()
    test_domain34()
    test_shape()
    print("\n== 汇总 ==")
    print("PASS %d / FAIL %d" % (len(PASS), len(FAIL)))
    if FAIL:
        print("FAIL 列表:")
        for n in FAIL:
            print("  -", n)
    return 1 if FAIL else 0


if __name__ == "__main__":
    sys.exit(main())
