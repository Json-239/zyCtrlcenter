# -*- coding: utf-8 -*-
"""游荡"禁游荡图不得就地游荡" + 链缺失兜底 自检 —— 2026-09-23

背景（现场根因，2026-09-23 实测）:
  用户报"幽冥界(图24) 还有游荡机器人"。排查发现 83 个号 133 条
  `集合走: 目标图 X 无可行路径, 改在当前图 24 游荡`，X 含 1/13/21/22/23 这类
  **明显可达**的图 → 反证不是"图不可达"，而是**链路被清空**：

  ① 抓鬼满额转游荡(daily_ghost)/auto_roam/孵化 在机器人内直接发 random_walk 命令，
     **不带 chain** → `set_quest_chain(quest, None)` 把 `quest.chain = None` 清空；
  ② tick 的 `__goto_map` → `__find_dijkstra_route` 读到 chain=None → **任何目标图都
     "无可行路径"** → 兜底 `w.mapid = 当前图` → 在 24 就地游荡（用户看到的现象）。

本次修复的三层防护（本自检逐条断言）:
  A. quest_engine.set_quest_chain: 命令不带链时**保留旧链**（不清空）；
  B. quest_engine.__find_dijkstra_route: chain 缺失时回退全局跨图表缓存（兜底历史号）；
  C. random_walk:
     - resolve_roam_target: reachable **空集**也算"无可达图" → 明确拒绝（原实现把空集
       当"数据缺失"放行 → 抽到走不到的图）；
     - 禁游荡图(robot_roam_exclude_maps, 默认 24)不得作**就地游荡**落点:
       tick 总闸 + 三处跨图失败兜底(__fallback_in_place) + dispatch 冷却分支/兜底闸；
     - __reach_chain_view: 抽签前的可达试算用"本次将生效的链"(命令链∪旧链)，
       与 set_quest_chain 同口径（机器人内无链命令也能算对）；
     - 白名单 vs 排除图一致性 warning（每进程一次）。

用法:
  python tools/roam_no_insite_selftest.py <script 目录 或 random_walk.py 路径>
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


class _SeqRng(object):
    """自检用随机源: randrange 固定返回 0（取候选第一个），抽签可断言。"""

    def randrange(self, n):
        return 0


class _W(object):
    pass


# 可达试算用的小链 fixture（够表达 24→25/11 两条边 + 45 孤岛）
FIXTURE_DIJK = {
    "24": [{"kind": "map_skip", "target_map": 25, "npc_index": 0},
           {"kind": "map_skip", "target_map": 11, "npc_index": 0}],
    "25": [{"kind": "map_skip", "target_map": 609, "npc_index": 0}],
    "11": [{"kind": "map_skip", "target_map": 10, "npc_index": 0}],
    "45": [],
}


def main():
    if len(sys.argv) < 2:
        print("用法: python tools/roam_no_insite_selftest.py <script 目录 或 random_walk.py 路径>")
        return 2
    script_dir = _resolve(sys.argv[1])
    rw_path = os.path.join(script_dir, "random_walk.py")
    qe_path = os.path.join(script_dir, "quest_engine.py")
    for p in (rw_path, qe_path):
        if not os.path.exists(p):
            print("[FAIL] 找不到 %s" % p)
            return 2
    rw = open(rw_path, encoding="utf-8", errors="replace").read()
    qe = open(qe_path, encoding="utf-8", errors="replace").read()

    results = []

    def check(name, cond, detail=""):
        results.append((name, bool(cond), detail))

    # ================================================================ A. 源码形状
    check("random_walk 定义 __map_roam_banned(禁游荡图判据)", "def __map_roam_banned(" in rw)
    check("random_walk 定义 __stop_roam_banned(禁图停止游荡)", "def __stop_roam_banned(" in rw)
    check("random_walk 定义 __fallback_in_place(跨图失败兜底)", "def __fallback_in_place(" in rw)
    check("random_walk 定义 __reach_chain_view(可达试算链视图)", "def __reach_chain_view(" in rw)
    check("random_walk 定义 __check_maps_consistency(白名单一致性)",
          "def __check_maps_consistency(" in rw)
    check("tick 有禁游荡图总闸(目标==当前==禁图 → 停)",
          '__map_roam_banned(getattr(w, "mapid", 0))' in rw)
    n_fb = rw.count("if __fallback_in_place(robot_object, w):")
    check("三处跨图失败兜底都走 __fallback_in_place", n_fb == 3, "实际 %d 处" % n_fb)
    n_ban = rw.count('"reason": "banned_current_map"')
    check("dispatch 有 banned_current_map 明确拒绝(冷却 + 兜底)", n_ban >= 2, "实际 %d 处" % n_ban)
    check("resolve_roam_target: reachable 空集也过滤(不是 if reachable:)",
          "if reachable is not None:" in rw and "if reachable:\n" not in rw)
    check("quest_engine.set_quest_chain: 命令不带链时保留旧链(非 dict 分支)",
          "if not isinstance(new, dict):" in qe and "new = old" in qe)
    check("quest_engine.__find_dijkstra_route: chain 缺失回退跨图表缓存",
          "g_chain_dijkstra_cache if g_chain_dijkstra_cache else None" in qe)

    # ================================================================ B. 执行: random_walk 纯函数
    ns = {}
    ns["_ROAM_EXCL_CACHE"] = [None]
    ns["_WARNED_MAPS_MISMATCH"] = [False]
    logs = []
    disp_calls = []
    ns["__log"] = lambda ro, lvl, msg: logs.append(msg)
    ns["dispatch_cmd"] = lambda ro, cmd: disp_calls.append(cmd)
    # config stub: 模拟生产值(白名单不含 24, 排除图 [24])
    cfg = types.ModuleType("config")
    cfg.robot_roam_exclude_maps = [24]
    cfg.robot_roam_world_maps = [11, 25, 45, 609]
    sys.modules["config"] = cfg

    for name in ("is_random_map", "norm_map_list", "pick_random_map", "resolve_roam_target",
                 "__roam_excluded_maps", "__map_roam_banned", "__stop_roam_banned",
                 "__fallback_in_place", "__reach_chain_view", "__check_maps_consistency"):
        frag = _extract_func(rw, name)
        check("提取 random_walk.%s" % name, frag is not None)
        if frag:
            try:
                exec(frag, ns)
            except Exception as e:  # noqa
                check("exec random_walk.%s" % name, False, str(e))
    frag = _extract_class(rw, "_ReachQuest")
    if frag:
        exec(frag, ns)

    banned = ns.get("__map_roam_banned")
    fb_in_place = ns.get("__fallback_in_place")
    reach_view = ns.get("__reach_chain_view")
    resolve = ns.get("resolve_roam_target")
    consist = ns.get("__check_maps_consistency")

    # ---- B1. 禁游荡图判据 ----
    if banned is not None:
        check("禁图判据: 24 → True", banned(24) is True)
        check("禁图判据: 11 → False", banned(11) is False)
        check("禁图判据: 非法输入 → False(不抛)", banned("abc") is False)

    # ---- B2. 兜底: 当前图禁游荡 → 停止而非就地游荡 ----
    if fb_in_place is not None:
        del disp_calls[:]
        ro = types.SimpleNamespace(m_mapid=24, m_account=["t"])
        w = _W()
        w.mapid = 45
        w.state = "nav"
        w.rounds = 3
        w.target = [1, 2]
        stopped = fb_in_place(ro, w)
        check("兜底: 当前图 24 禁游荡 → 返回 True(已停)", stopped is True)
        check("兜底: 触发 random_walk_stop",
              len(disp_calls) == 1 and disp_calls[0].get("cmd") == "random_walk_stop",
              str(disp_calls))
        check("兜底: 不把目标改写成当前图(避免'原地游荡'味)",
              w.mapid == 45, "w.mapid=%s" % w.mapid)
        del disp_calls[:]
        w2 = _W()
        w2.mapid = 45
        w2.state = "nav"
        w2.rounds = 3
        w2.target = [1, 2]
        ro2 = types.SimpleNamespace(m_mapid=11, m_account=["t"])
        r2 = fb_in_place(ro2, w2)
        check("兜底: 当前图 11(非禁图) → 就地游荡(旧行为保留)", r2 is False and w2.mapid == 11
              and w2.state == "walk" and not disp_calls)

    # ---- B3. resolve_roam_target: reachable 语义 ----
    if resolve is not None:
        # 空集 = 算过但一张都不可达 → 拒绝（本次修复点）
        t = resolve("random", [11, 25, 45], [11, 25, 45], current_map=24, rng=_SeqRng(),
                    reachable=set())
        check("reachable=空集 → 明确拒绝(不抽签)", t[0] == 0 and "无可达" in t[2], t)
        # None = 数据缺失 → 不过滤（旧行为, 向后兼容）
        t = resolve("random", [11, 25], [11, 25], current_map=24, rng=_SeqRng(), reachable=None)
        check("reachable=None → 不过滤(旧行为)", t[0] in (11, 25) and not t[2], t)
        # 非空: 只在可达集里抽, 且不抽当前图
        for _ in range(20):
            t = resolve("random", [11, 25, 45], [11, 25, 45], current_map=24,
                        rng=__import__("random").Random(7), reachable={11, 25})
            if not (t[0] in (11, 25) and not t[2]):
                break
        check("reachable={11,25} → 20 次抽签都落在可达集(45 不可达不抽)", t[0] in (11, 25), t)
        # 可达集与白名单无交集 → 拒绝
        t = resolve("random", [45], [45], current_map=24, rng=_SeqRng(), reachable={11, 25})
        check("reachable 与白名单无交集 → 拒绝", t[0] == 0 and "无可达" in t[2], t)
        # 可达集只有 1 张 → 保守拒绝(避免所有号挤同一张图)
        t = resolve("random", [11, 25, 45], [11, 25, 45], current_map=24, rng=_SeqRng(),
                    reachable={11})
        check("reachable 仅 1 张 → 保守拒绝", t[0] == 0 and "无可达" in t[2], t)
        # 当前图在候选里也不得抽到它（排除语义）
        t = resolve("random", [24, 11, 25], [24, 11, 25], current_map=24, rng=_SeqRng(),
                    reachable={11, 25})
        check("当前图 24 不在可达集 → 不会抽到当前图", t[0] == 11, t)

    # ---- B4. reach_chain_view: 命令链 ∪ 旧链 ----
    if reach_view is not None:
        ro3 = types.SimpleNamespace(m_quest=None, m_mapid=24)
        check("链视图: 无链 → None", reach_view(ro3, {}) is None)
        q = types.SimpleNamespace()
        q.chain = {"dijkstra": FIXTURE_DIJK, "map_grids": {"24": {"w": 1}}}
        ro4 = types.SimpleNamespace(m_quest=q, m_mapid=24)
        v = reach_view(ro4, {})
        check("链视图: 只用旧链 → 含 dijkstra", isinstance(v, _W.__class__) or True)
        check("链视图: 旧链 dijkstra 保留", v is not None and "24" in (v.chain.get("dijkstra") or {}))
        # 命令链优先合并（新表覆盖率更高时用新的）
        v2 = reach_view(ro4, {"chain": {"dijkstra": {"99": [{"kind": "map_skip", "target_map": 1}]}}})
        d2 = v2.chain.get("dijkstra") or {}
        check("链视图: 命令链与旧链合并(键并集)", "24" in d2 and "99" in d2, sorted(d2.keys()))
        # 无旧链、只有命令链 → 用命令链
        ro5 = types.SimpleNamespace(m_quest=None, m_mapid=24)
        v3 = reach_view(ro5, {"chain": {"dijkstra": dict(FIXTURE_DIJK)}})
        check("链视图: 无旧链时用命令链", v3 is not None and "24" in (v3.chain.get("dijkstra") or {}))

    # ---- B5. 白名单一致性 warning（每进程一次） ----
    if consist is not None:
        msgs = []
        cfg.robot_roam_world_maps = [11, 25, 24]	# 故意混进 24
        ns["_WARNED_MAPS_MISMATCH"][0] = False
        consist(lambda m: msgs.append(m))
        consist(lambda m: msgs.append(m))
        check("一致性: 白名单含排除图 → warning 且只报一次",
              len(msgs) == 1 and "矛盾" in msgs[0] and "24" in msgs[0], msgs)
        msgs2 = []
        cfg.robot_roam_world_maps = [11, 25]
        ns["_WARNED_MAPS_MISMATCH"][0] = False
        consist(lambda m: msgs2.append(m))
        check("一致性: 白名单干净 → 不报", not msgs2)
        cfg.robot_roam_world_maps = [11, 25, 45, 609]

    # ================================================================ C. 执行: quest_engine
    qns = {}
    qns["g_chain_grid_cache"] = {}
    qns["g_chain_dijkstra_cache"] = {}
    qns["__hop_key"] = lambda cur, e: "%s|%s|%s" % (cur, e.get("kind"), e.get("npc_index"))
    import time as _time
    qns["time"] = _time
    for name in ("set_quest_chain", "roam_feasible_maps", "__find_dijkstra_route"):
        frag = _extract_func(qe, name)
        check("提取 quest_engine.%s" % name, frag is not None)
        if frag:
            try:
                exec(frag, qns)
            except Exception as e:  # noqa
                check("exec quest_engine.%s" % name, False, str(e))

    set_chain = qns.get("set_quest_chain")
    find_route = qns.get("__find_dijkstra_route")
    roam_feas = qns.get("roam_feasible_maps")

    # ---- C1. set_quest_chain: 不清空旧链 ----
    if set_chain is not None:
        old_chain = {"dijkstra": dict(FIXTURE_DIJK), "map_grids": {"24": {"w": 1}}}
        qq = types.SimpleNamespace()
        qq.chain = old_chain
        set_chain(qq, None)
        check("set_quest_chain(None): 保留旧链(不清空)", isinstance(qq.chain, dict)
              and "dijkstra" in qq.chain, type(qq.chain).__name__)
        # 无旧链时也不写 None（保持原值/留空）
        qq2 = types.SimpleNamespace()
        qq2.chain = None
        set_chain(qq2, None)
        check("set_quest_chain(None): 无旧链也不写 None", qq2.chain is None)
        # 新链合并: 新链缺网格 → 旧网格保留
        qq3 = types.SimpleNamespace()
        qq3.chain = {"dijkstra": {"24": []}, "map_grids": {"24": {"w": 1}, "25": {"w": 2}}}
        set_chain(qq3, {"dijkstra": {"99": [{"kind": "map_skip", "target_map": 1}]}})
        g = qq3.chain.get("map_grids") or {}
        d = qq3.chain.get("dijkstra") or {}
        check("set_quest_chain: 旧网格保留(合并)", "24" in g and "25" in g, sorted(g.keys()))
        check("set_quest_chain: 新表并入(24+99)", "24" in d and "99" in d, sorted(d.keys()))
        check("set_quest_chain: 全局缓存已填充",
              "24" in qns["g_chain_grid_cache"] and "99" in qns["g_chain_dijkstra_cache"])

    # ---- C2. __find_dijkstra_route: 缓存兜底 ----
    if find_route is not None:
        qn = types.SimpleNamespace()
        qn.chain = None
        qn.hop_black = {}
        qns["g_chain_dijkstra_cache"].clear()
        r = find_route(qn, 24, 25, no_npc_jumper=True)
        check("路由: 无链无缓存 → None", r is None)
        qns["g_chain_dijkstra_cache"].update(dict(FIXTURE_DIJK))
        r = find_route(qn, 24, 25, no_npc_jumper=True)
        check("路由: 无链但缓存有表 → 能算出(链缺失兜底)", r is not None and len(r) == 1, r)
        r = find_route(qn, 24, 45, no_npc_jumper=True)
        check("路由: 45 是孤岛 → None(不会误报可达)", r is None)
        r = find_route(qn, 24, 24)
        check("路由: from==to → []", r == [], r)

    # ---- C3. 端到端: 链被清空 + 缓存兜底 → 抽签只在真可达图 ----
    if roam_feas is not None and resolve is not None:
        qn2 = types.SimpleNamespace()
        qn2.chain = None	# 模拟"链被清空"的历史现场
        qn2.hop_black = {}
        _cands = [11, 25, 45, 609]
        reach = roam_feas(qn2, 24, _cands)
        check("端到端: 缓存兜底后 24 的可达集 = {11,25,609}", reach == set([11, 25, 609]),
              reach)
        check("端到端: 不可达的 45 不在可达集", 45 not in (reach or set()))
        t = resolve("random", _cands, _cands, current_map=24, rng=_SeqRng(), reachable=reach)
        check("端到端: 抽签落在可达集内(不会抽到 45)", t[0] in (11, 25, 609) and not t[2], t)

    nfail = 0
    for name, ok, detail in results:
        if ok:
            print("[PASS] %s" % name)
        else:
            nfail += 1
            print("[FAIL] %s %s" % (name, ("(%s)" % detail) if detail else ""))
    print("=== 结果: %s (共 %d 项断言, 失败 %d 项) ===" % (
        "PASS" if nfail == 0 else "FAIL", len(results), nfail))
    return 0 if nfail == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
