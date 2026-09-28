# -*- coding: utf-8 -*-
"""NAV 超时分支回归自检（2026-09-28 P0）。

背景（docs/04-测试/分析-20260928-抓鬼停摆深挖.md §7）:
  09-24 "找鬼追踪" 改动把 daily_ghost.py 看门狗超时分支改坏 —— `if g.state == "NAV":`
  只剩一条日志，"刷新时间戳 + 清理 + 追鬼重试 + 清鬼"整段被误包进 else(非 NAV)：
    ① NAV 时间戳永不刷新 → 看门狗每帧判超时, rounds 毫秒级冲到 5 → GHOST_STUCK →
       自动重启 → 回等鬼循环（09-24 15:32 起，"重试 5 次" 300~390/小时）；
    ② 追鬼重试/清鬼的 `if g.state == "NAV"...` 在 else 里恒假 = 死代码，
       "鬼 X 移动了, 重新导航追" 恒 0（抓鬼完成断崖）。

覆盖两段:
  A 静态: 结构钉（无条件刷新时间戳 / 追鬼重试段 NAV 可达 / 坏结构不存在 / 判据与
           日志原文保留 / 非 NAV 等价路径 / P1 判据 _over_r 与注释锚点）。
  B 行为: 截取真实代码段（`g.rounds += 1` … `return 0`）真执行:
           单次 NAV 超时（时间戳刷新 + 追鬼重试可达 + 不误触 __stuck）；
           连续 tick 不虚冲（模拟 12s 看门狗门限, rounds 只 +1）；
           追鬼判据失效 → 清鬼回 WAIT_GHOST；非 NAV 等价（清理 + READY, 不误清鬼）；
           非 NAV rounds>4 → stuck（原判据逐字保留,B5）；
           NAV 共享债务(rounds=4)不误伤首超时(追鬼重试可达,B6)；
           NAV 本状态真实计数到 5 → stuck（>4 防线保留,兜异常路径,B7）。
  S 灵敏度: 对同名备份 .bak_20260928_navfix（修复前坏版）跑同一"连续 tick"循环 →
           必须复现回归（rounds 虚冲/__stuck 触发），证明本用例测的是真差异；
           S4 直接以 B1b 同一断言跑坏版 → "追鬼重试"不可达（死代码 FAIL 留档）；
           S5/S6 变异判别：把 P1 判据回退为共享 rounds → B6 同场景必须复现误伤。

用法: python tools/ghost_nav_timeout_selftest.py [script_dir]
"""
import hashlib
import io
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


def _load_region(path):
    """截取看门狗超时段的测试区间: `g.rounds += 1`(2 tab, 下一行为找鬼追踪注释)
    到块尾首个 2-tab `return 0`。返回行列表(含首尾)。"""
    lines = io.open(path, encoding="utf-8", errors="replace").read().split("\n")
    si = None
    for i in range(len(lines) - 1):
        if lines[i] == "\t\tg.rounds += 1" \
                and lines[i + 1].startswith("\t\t# 2026-09-24 找鬼追踪: NAV 本状态真实超时独立计数"):
            si = i
            break
    if si is None:
        return None
    ei = None
    for j in range(si, len(lines)):
        if lines[j] == "\t\treturn 0":
            ei = j
            break
    if ei is None:
        return None
    return lines[si:ei + 1]


def _make_body(region):
    """去一层缩进后包成可调函数（依赖全部走形参, 零 import 依赖）。"""
    body = "\n".join(ln[1:] if ln.startswith("\t") else ln for ln in region)
    src = ("def _nav_tick(robot_object, g, quest, now_ms, __stuck, __log, __set_state, "
           "__emit_state, __goto_xy, quest_engine, __nav_ghost_follow_ok, NAV_FOLLOW_MAX):\n"
           + body + "\n")
    ns = {}
    exec(src, ns)
    return ns["_nav_tick"]


def _caps():
    caps = {"stuck": [], "log": [], "state": [], "teleport": [], "goto": []}

    def _set_state(g, st, now):
        g.state = st
        caps["state"].append(st)

    caps["follow_ok"] = lambda g: int(getattr(g, "nav_rounds", 0) or 0) < 3
    caps["fn_stuck"] = lambda ro, g, msg: caps["stuck"].append(msg)
    caps["fn_log"] = lambda ro, lvl, msg: caps["log"].append(msg)
    caps["fn_state"] = _set_state
    caps["fn_emit"] = lambda ro, g: None
    caps["fn_goto"] = lambda ro, quest, g, gm, x, y, now: (caps["goto"].append(gm), False)[1]
    caps["qe"] = types.SimpleNamespace(
        __teleport_click=lambda ro, q, npc, ci, ct, delay, no_npc_jumper=False:
            caps["teleport"].append((npc, int(no_npc_jumper))))
    return caps


def _call(body, caps, ro, g, quest, now_ms):
    return body(ro, g, quest, now_ms, caps["fn_stuck"], caps["fn_log"],
                caps["fn_state"], caps["fn_emit"], caps["fn_goto"], caps["qe"],
                caps["follow_ok"], 3)


def _gate_loop(body, caps, ro, g, quest, t0=13000, frames=5, step=100, gate_ms=12000):
    """模拟主循环: 每帧 `if (t - state_since_ms) > NAV 门限: <本段>`。"""
    for k in range(frames):
        t = t0 + k * step
        if (t - int(g.state_since_ms or 0)) > gate_ms:
            _call(body, caps, ro, g, quest, t)


def _mk_quest(ghost_id=777, ghost_map=24):
    q = types.SimpleNamespace()
    q.dynamic_npcs = {ghost_id: [ghost_map, 100, 100]} if ghost_id else {}
    q.dyn_npc_meta = {ghost_id: {"x": 1}} if ghost_id else {}
    q.pending = "P"
    q.walk_target = [1, 2]
    q.dijkstra_route = [{"d": 1}]
    return q


def _mk_g(state="NAV", rounds=0, nav_rounds=0, ghost_npc_id=777):
    g = types.SimpleNamespace()
    g.state = state
    g.state_since_ms = 0
    g.rounds = rounds
    g.nav_rounds = nav_rounds
    g.ghost_npc_id = ghost_npc_id
    return g


def main():
    if len(sys.argv) >= 2:
        script_dir = _resolve(sys.argv[1])
    else:
        script_dir = DEFAULT_DIR
    dh_path = os.path.join(script_dir, "daily_ghost.py")
    if not os.path.exists(dh_path):
        print("[FAIL] 找不到 %s" % dh_path)
        return 2
    dh = io.open(dh_path, encoding="utf-8", errors="replace").read()
    print("  自检目标: %s (sha1=%s)" % (
        dh_path, hashlib.sha1(dh.encode("utf-8")).hexdigest()[:12]))

    # ============================================================ A. 静态结构
    check("A1: 时间戳无条件刷新(2-tab, 在 NAV/非NAV 分流之后)",
          "\t\tg.state_since_ms = now_ms\t# 2026-08-24 强制刷新时间戳" in dh)
    check("A2: 坏结构不存在(时间戳不再嵌在 else 里 = 3-tab+原注释)",
          "\t\t\tg.state_since_ms = now_ms\t# 2026-08-24 强制刷新时间戳" not in dh)
    check("A3: 追鬼重试/清鬼段 NAV 可达(2-tab 判据)",
          "\t\tif g.state == \"NAV\" and g.ghost_npc_id:" in dh)
    check("A4: 坏结构不存在(追鬼重试不再是 else 内 3-tab 死代码)",
          "\t\t\tif g.state == \"NAV\" and g.ghost_npc_id:" not in dh)
    i_refresh = dh.find("\t\tg.state_since_ms = now_ms\t# 2026-08-24 强制刷新时间戳")
    i_follow = dh.find("\t\tif g.state == \"NAV\" and g.ghost_npc_id:")
    i_ready = dh.find('\t\t__set_state(g, "READY", now_ms)', i_follow)
    check("A5: 顺序=刷新时间戳 → 追鬼段 → READY 兜底",
          0 <= i_refresh < i_follow < i_ready, (i_refresh, i_follow, i_ready))
    check("A6: NAV 两段式日志保留(共享计数/本状态真实次数)",
          "抓鬼状态 NAV 超时, 重试 %d 次(本状态真实第 %d 次)" in dh)
    check("A7: 非 NAV 日志原文保留(既有统计口径不破)",
          "抓鬼状态 %s 超时, 第 %d 次重试" in dh)
    check("A8: 追鬼指据/清鬼/计数归零保留(__nav_ghost_follow_ok / 清鬼 / nav_rounds=0)",
          "__nav_ghost_follow_ok(g)" in dh
          and "目标鬼 %s 消失或追不上, 清除等新鬼" in dh
          and "本轮追鬼结束, 本状态计数归零" in dh)
    check("A9: 修复注释锚点(指向深挖文档 §7)",
          "2026-09-28 修复(NAV 超时分支回归" in dh
          and "分析-20260928-抓鬼停摆深挖.md" in dh)
    check("A10: P1 判据存在(NAV 用本状态真实计数, 其余状态用共享 rounds)",
          '_over_r = _nav_r if g.state == "NAV" else g.rounds' in dh)
    check("A11: 旧共享判据不再分流 stuck(if g.rounds > 4: 不存在)",
          "\t\tif g.rounds > 4:" not in dh)
    check("A12: P1 注释锚点(共享计数残留误伤)",
          "2026-09-28 P1 修复(共享计数残留误伤" in dh)

    # ============================================================ B. 行为(截段真执行)
    region = _load_region(dh_path)
    check("B0: 截取看门狗超时段", region is not None)
    body = None
    if region:
        try:
            body = _make_body(region)
            check("B0b: 截段可执行为函数", callable(body))
        except Exception as e:  # noqa
            check("B0b: 截段可执行为函数", False, str(e))

    if body is not None:
        ro = types.SimpleNamespace(m_mapid=24)
        # B1 单次 NAV 超时: 时间戳刷新 + 追鬼重试可达 + 不误触 __stuck
        g, q, caps = _mk_g(), _mk_quest(), _caps()
        ret = _call(body, caps, ro, g, q, 13000)
        check("B1: NAV 超时后 state_since_ms 刷新(=防每帧虚冲)",
              int(g.state_since_ms) == 13000, g.state_since_ms)
        check("B1b: 追鬼重试可达(死代码已放行, '重新导航追' + teleport_click)",
              any("重新导航追" in m for m in caps["log"])
              and caps["teleport"] == [(777, 1)], (caps["log"][-1:], caps["teleport"]))
        check("B1c: 未误触 __stuck 且返回 0",
              caps["stuck"] == [] and ret == 0, (caps["stuck"], ret))

        # B2 连续 tick 不虚冲(模拟 12s 门限, 5 帧 × 100ms)
        g, q, caps = _mk_g(), _mk_quest(), _caps()
        _gate_loop(body, caps, ro, g, q)
        check("B2: 连续 5 帧只超时 1 次(rounds==1, 不虚冲)",
              int(g.rounds) == 1, g.rounds)
        check("B2b: 连续帧未误触 __stuck", caps["stuck"] == [], caps["stuck"])

        # B3 追鬼判据失效(nav_rounds>=3) → 清鬼回 WAIT_GHOST
        g, q, caps = _mk_g(nav_rounds=3), _mk_quest(), _caps()
        ret = _call(body, caps, ro, g, q, 13000)
        check("B3: 追不上 → 清鬼 + 回 WAIT_GHOST",
              g.ghost_npc_id == 0 and g.nav_rounds == 0
              and q.dynamic_npcs == {} and g.state == "WAIT_GHOST"
              and "WAIT_GHOST" in caps["state"], (g.ghost_npc_id, g.state))
        check("B3b: 清鬼日志保留", any("消失或追不上" in m for m in caps["log"]), caps["log"][-1:])

        # B4 非 NAV 等价: 清理 + READY 兜底, 不误清鬼/不追鬼
        g, q, caps = _mk_g(state="READY", ghost_npc_id=777), _mk_quest(), _caps()
        ret = _call(body, caps, ro, g, q, 13000)
        check("B4: 非 NAV → 清理 + READY(等价原 else)",
              int(g.state_since_ms) == 13000 and g.state == "READY"
              and q.pending is None and q.walk_target is None and q.dijkstra_route == []
              and caps["teleport"] == [] and caps["stuck"] == [],
              (g.state, q.pending, caps["teleport"]))
        check("B4b: 非 NAV 不误清鬼(ghost_npc_id 保留)",
              int(g.ghost_npc_id) == 777, g.ghost_npc_id)

        # B5 非 NAV rounds>4 → __stuck（原判据对非 NAV 逐字保留, 不因本次修复松动）
        g, q, caps = _mk_g(state="READY", rounds=4), _mk_quest(), _caps()
        ret = _call(body, caps, ro, g, q, 13000)
        check("B5: 非 NAV rounds>4 → __stuck(判据保留)",
              len(caps["stuck"]) == 1 and "状态 READY 重试 5 次仍无进展" in caps["stuck"][0]
              and ret == 0, caps["stuck"])

        # B6 NAV 共享债务免疫(P1): 共享 rounds=4(旧版会误伤) + 本状态真实首超时 →
        #    不 stuck、追鬼重试仍可达(样本 robot0001000/5050 的误伤场景)
        g, q, caps = _mk_g(rounds=4, nav_rounds=0), _mk_quest(), _caps()
        ret = _call(body, caps, ro, g, q, 13000)
        check("B6: NAV 共享债务(rounds=4)不再误伤首超时(不 stuck)",
              caps["stuck"] == [] and ret == 0, (caps["stuck"], ret))
        check("B6b: 追鬼重试可达(第 1/3 次) + teleport",
              any("重新导航追" in m and "(第 1/3 次)" in m for m in caps["log"])
              and caps["teleport"] == [(777, 1)], (caps["log"][-1:], caps["teleport"]))

        # B7 NAV >4 防线保留: 本状态真实计数 _nav_r 构造到 5 → stuck（兜 NAV+无鬼等异常路径）
        g, q, caps = _mk_g(nav_rounds=4), _mk_quest(), _caps()
        ret = _call(body, caps, ro, g, q, 13000)
        check("B7: NAV 本状态真实 5 次 → stuck(防线保留)",
              len(caps["stuck"]) == 1 and "状态 NAV 重试 5 次仍无进展" in caps["stuck"][0]
              and ret == 0, caps["stuck"])

    # ============================================================ S. 灵敏度(修复前坏版必复现)
    bak_path = os.path.join(script_dir, "daily_ghost.py.bak_20260928_navfix")
    if os.path.exists(bak_path):
        region_bad = _load_region(bak_path)
        check("S0: 修复前备份可截段", region_bad is not None)
        if region_bad:
            try:
                body_bad = _make_body(region_bad)
                ro = types.SimpleNamespace(m_mapid=24)
                g, q, caps = _mk_g(), _mk_quest(), _caps()
                _gate_loop(body_bad, caps, ro, g, q)
                check("S1: 坏版连续帧 rounds 虚冲(≥5)",
                      int(g.rounds) >= 5, g.rounds)
                check("S2: 坏版误触 __stuck(回归复现)",
                      len(caps["stuck"]) >= 1, caps["stuck"])
                check("S3: 坏版 NAV 时间戳不刷新(修复点直接对照)",
                      int(g.state_since_ms or 0) == 0, g.state_since_ms)
                # S4 追鬼重试死代码直接证明: 用 B1b 同一断言跑坏版单次超时,
                #    修复版 PASS(可达)、坏版必 FAIL(不可达) —— 判别力留档。
                g, q, caps = _mk_g(), _mk_quest(), _caps()
                _call(body_bad, caps, ro, g, q, 13000)
                check("S4: 坏版单次超时'重新导航追'不可达"
                      "(=B1b 同断言在修复前 FAIL 留档; pending 也未清理)",
                      not any("重新导航追" in m for m in caps["log"])
                      and caps["teleport"] == []
                      and q.pending == "P" and not caps["stuck"],
                      (caps["log"][-1:], caps["teleport"], q.pending))
            except Exception as e:  # noqa
                check("S1: 坏版复现执行", False, str(e))
    else:
        check("S0: 修复前备份存在(灵敏度对照)", False, bak_path)

    # ============================================================ S2. 变异判别(P1 判据回退)
    # 把 P1 判据行回退为共享 rounds（等价于修复前语义）→ B6 同场景必须复现误伤,
    # 证明 B6 用例对"判据回退"有判别力（不只是形状断言）。
    if region and body is not None:
        mut = [ln.replace('_over_r = _nav_r if g.state == "NAV" else g.rounds',
                          "_over_r = g.rounds") for ln in region]
        check("S5: 变异源生成(P1 判据回退为共享 rounds)",
              any("_over_r = g.rounds" in x for x in mut))
        try:
            body_mut = _make_body(mut)
            g, q, caps = _mk_g(rounds=4, nav_rounds=0), _mk_quest(), _caps()
            _call(body_mut, caps, ro, g, q, 13000)
            check("S6: 判据回退 → B6 同场景复现误伤 stuck(自检判别力)",
                  len(caps["stuck"]) >= 1, caps["stuck"])
        except Exception as e:  # noqa
            check("S6: 变异执行", False, str(e))
    else:
        check("S5: 变异源生成(P1 判据回退为共享 rounds)", False, "region/body 为空")

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
