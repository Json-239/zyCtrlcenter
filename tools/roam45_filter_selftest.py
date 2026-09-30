# -*- coding: utf-8 -*-
"""游荡过滤追加瑶池回廊 45 自检 —— 2026-09-30

背景:
  45（瑶池回廊）跳转点连通域残缺（出口 (2446,254) 与部分入口域不连通）→ 采购/补药类
  跨图硬失败 NO_LEGAL_ROUTE 卡 ERROR（现场 5031/5033/5092/5097/5244；见
  docs/04-测试/分析-20260930-map45死域与ERROR处置.md）。用户批准"45 有问题就加入过滤"。

本批机器人端 config 三处改动（双副本）:
  ① robot_roam_exclude_maps += 45（[24,25,45,653,654,655]）
  ② robot_roam_world_maps 删 45（36 → 35 张）
  ③ robot_roam_wild_maps  删 45（20 → 19 张）

断言分组:
  S45:*  = 45 相关 —— 对 .bak_20260930_roam45 坏版跑同断言必 FAIL（坏版灵敏度）
  CTRL:* = 控制组（24 等既有行为；坏版也应 PASS —— 证明断言不是"全盘必挂"）
  FIX:*  = random_walk 既有拒发代码形状（两版相同，均应 PASS）

用法:
  python tools/roam45_filter_selftest.py [script_dir]                 # 全量期望 PASS
  python tools/roam45_filter_selftest.py [script_dir] \
        --config <bak config 路径> --expect-bad
        # 坏版灵敏度：要求 S45:* 全部 FAIL、CTRL:*/FIX:* 全部 PASS

边界（本自检只读、不启动机器人、不改任何文件）:
  - config 静态解析三表 + 变更注释标记；
  - 行为面：抽取 random_walk 真实函数体在本进程 exec 驱动 ——
    __roam_excluded_maps / __map_roam_banned / pick_random_map（3000 次抽签）/
    resolve_roam_target（1000 次）/**真实 dispatch_cmd 拒发路径**（显式 mapid=45 →
    reason=excluded_map + 零副作用；显式 24 为控制组；随机+白名单[45] 剔除后明确拒绝）。
"""
import os
import re
import sys
import types
import time
import random

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DEFAULT_SCRIPT = os.path.join(ROOT, "deploy", "zones", "prod-240-2300", "script")
PASS, FAIL = [], []


def ok(name, cond, detail=""):
    (PASS if cond else FAIL).append(name)
    print("  [%s] %s%s" % ("OK" if cond else "FAIL", name,
                           (" —— " + str(detail)) if detail else ""))


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


def _extract_class(src, name):
    lines = src.splitlines()
    for i, ln in enumerate(lines):
        if ln.startswith("class %s(" % name) or ln.startswith("class %s:" % name):
            out = [ln]
            for ln2 in lines[i + 1:]:
                if ln2.strip() and not ln2[0].isspace():
                    break
                out.append(ln2)
            return "\n".join(out)
    return None


def _parse_maps(text, key):
    m = re.search(r"^%s\s*=\s*\[([^\]]*)\]" % key, text, re.M)
    if not m:
        return None
    return [int(x) for x in re.findall(r"-?\d+", m.group(1))]


def main():
    args = sys.argv[1:]
    script_dir = None
    config_path = None
    expect_bad = False
    i = 0
    while i < len(args):
        a = args[i]
        if a == "--config" and i + 1 < len(args):
            config_path = args[i + 1]
            i += 2
        elif a == "--expect-bad":
            expect_bad = True
            i += 1
        elif script_dir is None:
            script_dir = a
            i += 1
        else:
            i += 1
    script_dir = script_dir or DEFAULT_SCRIPT
    cfg_path = config_path or os.path.join(script_dir, "config.py")
    rw_path = os.path.join(script_dir, "random_walk.py")
    for p in (cfg_path, rw_path):
        if not os.path.exists(p):
            print("[FAIL] 找不到 %s" % p)
            return 2

    print("config  : %s" % cfg_path)
    print("random_walk: %s" % rw_path)
    cfg_text = open(cfg_path, encoding="utf-8", errors="replace").read()
    rw = open(rw_path, encoding="utf-8", errors="replace").read()

    world = _parse_maps(cfg_text, "robot_roam_world_maps")
    wild = _parse_maps(cfg_text, "robot_roam_wild_maps")
    excl = _parse_maps(cfg_text, "robot_roam_exclude_maps")

    # ================================================================ 1) config 静态
    print("== 1) config 静态（三表 + 计数 + 变更注释）")
    ok("FIX:三表可解析", world is not None and wild is not None and excl is not None)
    if world is None or wild is None or excl is None:
        print("\n结果: PASS=%d FAIL=%d（config 解析失败，终止）" % (len(PASS), len(FAIL)))
        return 1
    print("     world n=%d / wild n=%d / excl=%s" % (len(world), len(wild), excl))

    ok("S45:exclude 含 45", 45 in excl, excl)
    ok("S45:world 不含 45", 45 not in world)
    ok("S45:wild 不含 45", 45 not in wild)
    ok("S45:world 计数 == 35（删 45 后）", len(world) == 35, len(world))
    ok("S45:wild 计数 == 19（删 45 后）", len(wild) == 19, len(wild))
    ok("S45:注释含 exclude 变更标记", "2026-09-30 追加瑶池回廊 45" in cfg_text)
    ok("S45:注释含 world 变更标记", "2026-09-30 再删瑶池回廊 45" in cfg_text)
    ok("S45:注释含 wild 变更标记", "2026-09-30 删瑶池回廊 45" in cfg_text)

    ok("CTRL:exclude 含 24（既有口径不变）", 24 in excl, excl)
    ok("CTRL:world∩exclude 为空（无配置矛盾）", not (set(world) & set(excl)),
       sorted(set(world) & set(excl)))
    ok("CTRL:world 仍有 6/609（未误删）", 6 in world and 609 in world)

    # ================================================================ 2) random_walk 代码形状
    print("== 2) random_walk 拒发路径代码形状")
    ok("FIX:有排除表接入(_excl = __roam_excluded_maps())",
       "_excl = __roam_excluded_maps()" in rw)
    n_reason = rw.count('"reason": "excluded_map"')
    ok("FIX:有 excluded_map 明确拒绝(≥2 处: 单图+白名单剔空)", n_reason >= 2, n_reason)
    ok("FIX:有 __maps_gate_reject(_raw_map, _raw_maps, _maps_given) 调用",
       "__maps_gate_reject(_raw_map, _raw_maps, _maps_given)" in rw)
    ok("FIX:有白名单一致性校验调用(__check_maps_consistency)",
       "__check_maps_consistency(lambda m:" in rw)

    # ================================================================ 3) 行为: 抽签/判据
    print("== 3) 行为（抽取真实函数体 exec 驱动）")
    logs = []
    ns = {}
    ns["_ROAM_EXCL_CACHE"] = [None]
    ns["_WARNED_MAPS_MISMATCH"] = [False]
    ns["__log"] = lambda ro, lvl, msg: logs.append((str(lvl), str(msg)))
    ns["time"] = time
    ns["random"] = random
    for _m in re.finditer(r"^(ROAM_\w+)\s*=\s*(\d+)", rw, re.M):
        ns[_m.group(1)] = int(_m.group(2))

    cfg = types.ModuleType("config")
    cfg.robot_roam_exclude_maps = list(excl)
    cfg.robot_roam_world_maps = list(world)
    cfg.robot_roam_wild_maps = list(wild)
    sys.modules["config"] = cfg
    ns["config"] = cfg

    qe_stub = types.ModuleType("quest_engine")
    qe_stub.get_quest = lambda ro, create=False: getattr(ro, "m_quest", None)
    qe_stub.g_chain_grid_cache = {}
    sys.modules["quest_engine"] = qe_stub
    ns["quest_engine"] = qe_stub

    for name in ("is_random_map", "norm_map_list", "pick_random_map", "resolve_roam_target",
                 "__roam_excluded_maps", "__map_roam_banned", "__maps_gate_reject",
                 "__check_maps_consistency", "__now_ms", "__far_snap_cooling",
                 "__req_same_map", "__quest", "__grid_mapids", "__rng"):
        frag = _extract_func(rw, name)
        ok("FIX:提取 random_walk.%s" % name, frag is not None)
        if frag:
            try:
                exec(compile(frag, rw_path, "exec"), ns)
            except Exception as e:  # noqa
                ok("FIX:exec random_walk.%s" % name, False, repr(e))
    frag_ws = _extract_class(rw, "CollectWalkState")
    ok("FIX:提取 CollectWalkState", frag_ws is not None)
    if frag_ws:
        try:
            exec(compile(frag_ws, rw_path, "exec"), ns)
        except Exception as e:  # noqa
            ok("FIX:exec CollectWalkState", False, repr(e))

    ex_set = ns.get("__roam_excluded_maps")
    banned = ns.get("__map_roam_banned")
    pick = ns.get("pick_random_map")
    resolve = ns.get("resolve_roam_target")

    if ex_set is not None:
        s = ex_set()
        ok("S45:__roam_excluded_maps() 含 45", 45 in s, sorted(s))
        ok("CTRL:__roam_excluded_maps() 含 24", 24 in s, sorted(s))
    if banned is not None:
        ok("S45:__map_roam_banned(45) → True", banned(45) is True)
        ok("CTRL:__map_roam_banned(24) → True", banned(24) is True)
        ok("CTRL:__map_roam_banned(11) → False", banned(11) is False)

    if pick is not None:
        rng = random.Random(20260930)
        drawn = [pick(world, exclude=None, rng=rng) for _ in range(3000)]
        ok("S45:world 池 3000 次抽签不含 45", all(v != 45 for v in drawn),
           "drawn45=%d" % drawn.count(45))
        ok("CTRL:world 池抽签都在池内", all(v in set(world) for v in drawn))
        rng = random.Random(20260930)
        drawn_w = [pick(wild, exclude=None, rng=rng) for _ in range(3000)]
        ok("S45:wild 池 3000 次抽签不含 45", all(v != 45 for v in drawn_w),
           "drawn45=%d" % drawn_w.count(45))
        ok("CTRL:wild 池抽签都在池内", all(v in set(wild) for v in drawn_w))

    if resolve is not None:
        rng = random.Random(20260930)
        tgts, errs = [], []
        for _ in range(1000):
            t, _mn, err = resolve("random", world, grid_mapids=world,
                                  current_map=6, rng=rng, reachable=None)
            tgts.append(t)
            if err:
                errs.append(err)
        ok("S45:resolve_roam_target 1000 次不抽到 45", all(v != 45 for v in tgts),
           "hit45=%d" % tgts.count(45))
        ok("CTRL:resolve_roam_target 结果都在池内且无 err",
           not errs and all(v in set(world) for v in tgts), errs[:1])

    # ================================================================ 4) 行为: 真实 dispatch_cmd 拒发
    print("== 4) 行为（真实 dispatch_cmd 拒发路径）")
    ns_d = dict(ns)
    ns_d["__ghost_busy"] = lambda ro: False   # 控制组用例要往下走一步；45 拒绝在其之前
    frag_dc = _extract_func(rw, "dispatch_cmd")
    ok("FIX:提取 dispatch_cmd", frag_dc is not None)
    if frag_dc:
        try:
            exec(compile(frag_dc, rw_path, "exec"), ns_d)
        except Exception as e:  # noqa
            ok("FIX:exec dispatch_cmd", False, repr(e))
    disp = ns_d.get("dispatch_cmd")

    def _mk_ro():
        ro = types.SimpleNamespace()
        ro.m_mapid = 5
        ro.m_account = ["selftest"]
        ro.m_collect_walk = None
        ro.m_quest = None
        ro.m_ghost = None
        ro.m_share_daily = None
        return ro

    def _drive(cmd):
        ro = _mk_ro()
        n0 = len(logs)
        try:
            resp = disp(ro, dict(cmd))
        except Exception as e:
            resp = {"result": "exception", "err": "%s: %s" % (type(e).__name__, e)}
        new_logs = logs[n0:]
        w = getattr(ro, "m_collect_walk", None)
        return resp, new_logs, w

    if disp is not None:
        # -- 显式 mapid=45: 新配置 → excluded_map 明确拒绝 + 零副作用（坏版灵敏度核心）
        resp45, l45, w45 = _drive({"cmd": "random_walk", "mapid": 45})
        ok("S45:显式 mapid=45 → 明确拒绝(excluded_map)",
           isinstance(resp45, dict) and resp45.get("result") == "error"
           and resp45.get("reason") == "excluded_map", resp45)
        ok("S45:显式 mapid=45 → 拒发日志(游荡排除图/45)",
           any(("游荡排除图" in m) and ("45" in m) for _l, m in l45), l45)
        ok("S45:显式 mapid=45 拒绝零副作用(未置 enabled/未改目标图)",
           w45 is not None and getattr(w45, "enabled", True) is False
           and int(getattr(w45, "mapid", -1) or -1) == 5,
           (getattr(w45, "enabled", None), getattr(w45, "mapid", None)))

        # -- 控制: 显式 mapid=24（两版都在排除表）→ 同样明确拒绝（坏版也应 PASS）
        resp24, l24, w24 = _drive({"cmd": "random_walk", "mapid": 24})
        ok("CTRL:显式 mapid=24 → 明确拒绝(excluded_map)",
           isinstance(resp24, dict) and resp24.get("result") == "error"
           and resp24.get("reason") == "excluded_map", resp24)
        ok("CTRL:显式 mapid=24 → 拒发日志",
           any(("游荡排除图" in m) and ("24" in m) for _l, m in l24), l24)
        ok("CTRL:显式 mapid=24 拒绝零副作用",
           w24 is not None and getattr(w24, "enabled", True) is False
           and int(getattr(w24, "mapid", -1) or -1) == 5,
           (getattr(w24, "enabled", None), getattr(w24, "mapid", None)))

        # -- 控制: 显式 mapid=11（非排除图）→ 不走"排除图"拒发
        resp11, l11, _w11 = _drive({"cmd": "random_walk", "mapid": 11})
        ok("CTRL:显式 mapid=11 未被'排除图'拒发",
           not (isinstance(resp11, dict) and resp11.get("reason") == "excluded_map")
           and not any("游荡排除图" in m for _l, m in l11),
           (resp11, [m for _l, m in l11][:2]))

        # -- 随机 + 白名单 [45]: 45 被剔除 → 明确拒绝（不启动；坏版会放行到 45）
        resp_rw, _l_rw, w_rw = _drive({"cmd": "random_walk", "mapid": "random", "maps": [45]})
        ok("S45:随机图+白名单[45] → 45 被剔除后明确拒绝(不启动)",
           isinstance(resp_rw, dict) and resp_rw.get("result") == "error"
           and int(getattr(w_rw, "mapid", -1) or -1) != 45,
           (resp_rw, getattr(w_rw, "mapid", None)))

    # ================================================================ 5) 结果/灵敏度
    s45 = [n for n in PASS + FAIL if n.startswith("S45:")]
    s45_fail = [n for n in FAIL if n.startswith("S45:")]
    ctrl_fail = [n for n in FAIL if n.startswith("CTRL:")]
    fix_fail = [n for n in FAIL if n.startswith("FIX:")]
    total = len(PASS) + len(FAIL)
    nfail = len(FAIL)

    if expect_bad:
        sens_ok = (len(s45) >= 10 and len(s45_fail) == len(s45)
                   and not ctrl_fail and not fix_fail)
        print("\n=== 坏版灵敏度: %s ===" % ("OK" if sens_ok else "BAD"))
        print("    S45 预期失败: %d/%d（应全部 FAIL）" % (len(s45_fail), len(s45)))
        print("    CTRL 意外失败: %d（应 0）; FIX 意外失败: %d（应 0）"
              % (len(ctrl_fail), len(fix_fail)))
        for n in s45_fail:
            print("      [预期FAIL] %s" % n)
        return 0 if sens_ok else 1

    print("\n=== 结果: %s (共 %d 项断言, 失败 %d 项) ===" % (
        "PASS" if nfail == 0 else "FAIL", total, nfail))
    for n in FAIL:
        print("  [FAIL] %s" % n)
    return 0 if nfail == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
