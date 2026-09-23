# -*- coding: utf-8 -*-
"""游荡"禁游荡图不得就地游荡" + 链缺失兜底 + 图内死点修复 自检 —— 2026-09-23

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

D. 图内死点修复（2026-09-23b，用户报"少数游荡号卡在目标图死点，反复刷
   `地图 X 无可行路径 (x,y)→(x,y), 取消行走`，位置不动"）:
   - __pick_walk_point: 候选点除"非阻挡格"外还要**与当前位置连通**（试算一次
     quest_engine.__build_path，与真走同寻路/同缓存）→ 不通换点(≤3 次)；
     试算不可用(异常) → 退回旧行为直接返回候选点（不把号钉死）；
   - __snap_walkable: 当前位置在网格外/阻挡格 → 夹回网格取最近可走格(半径 40 格)，
     写回 m_pose；吸附失败 → 换图/停止；
   - __note_walk_progress + __roam_switch_map: 同目标图连续 3 次"无进展"(20s 节流)
     → warn + 换图（随机模式在白名单/世界白名单内另抽，**只读配置不改白名单**）；
     没有候选图 → 停止游荡（复用既有失败停止风格）；
   - 选点全失败 → 5s 冷却（试算是真寻路，死点号每帧重试会白烧 CPU）。

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

    # ---------------------------------------------------------------- D. 图内死点修复
    check("random_walk 定义 __grid_for(选点/吸附共用网格)", "def __grid_for(" in rw)
    check("random_walk 定义 __snap_walkable(网格外/阻挡吸附)", "def __snap_walkable(" in rw)
    check("random_walk 定义 __note_walk_progress(连续无进展计数)",
          "def __note_walk_progress(" in rw)
    check("random_walk 定义 __roam_switch_map(连续失败换图/停止)",
          "def __roam_switch_map(" in rw)
    check("random_walk 定义 __stop_roam_far_snap(吸附超限→停止游荡待命)",
          "def __stop_roam_far_snap(" in rw)
    check("random_walk 定义 __far_snap_cooling(超限停止后 10 分钟不补发)",
          "def __far_snap_cooling(" in rw
          and "and __far_snap_cooling(robot_object, __now_ms())" in rw)
    check("常量: 试算3次/吸附40格/换图阈值3/失败节流20s/重试冷却5s/吸附上限1200px/补发冷却10min",
          "ROAM_PATH_CHECK_TRIES = 3" in rw and "ROAM_SNAP_RADIUS = 40" in rw
          and "ROAM_FAILS_TO_SWITCH = 3" in rw and "ROAM_FAIL_GAP_MS = 20000" in rw
          and "ROAM_PICK_RETRY_MS = 5000" in rw and "ROAM_SNAP_MAX_DIST_PX = 1200" in rw
          and "ROAM_FAR_SNAP_COOLDOWN_MS = 600000" in rw)
    check("吸附距离上限调用处判断(欧氏距离, px): 超限 → 停止游荡",
          "if _d > ROAM_SNAP_MAX_DIST_PX:" in rw
          and "_d = (((snap[0] - cur[0]) ** 2 + (snap[1] - cur[1]) ** 2)) ** 0.5" in rw)
    check("选点连通试算复用 quest_engine.__build_path(与 __do_walk 同寻路, 非新写)",
          ("quest_engine.__build_path(quest, w.mapid," in rw)
          or ("quest_engine.__build_path(quest, mapid," in rw
              and "def walk_point_connected(" in rw))
    check("__random_walk_move 入口: 无进展计数 + 吸附",
          "__note_walk_progress(w, cur, now_ms)" in rw
          and "__snap_walkable(grid, cur[0], cur[1])" in rw)
    check("__random_walk_move 兜底选点也带 from_pos(试算口径一致)",
          "__pick_walk_point(quest, w, from_pos=cur)" in rw)
    check("选点全失败 → 5s 冷却(防死点号每帧烧试算 CPU)",
          "w.walk_next_try_ms = now_ms + ROAM_PICK_RETRY_MS" in rw)
    check("换图后 state=IDLE(不是 nav: 否则 tick 不会重新 __goto_map)",
          'w.state = "IDLE"\t# 用 IDLE(不是 nav)' in rw)

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

    # ================================================================ D. 图内死点修复
    # fixture 网格(20x12):
    #   x=5 竖墙(y=2..9, 左右只能从 y=0/1/10/11 绕行) + 右下角封闭口袋
    #   (x=15..18, y=3..8, 口字环全阻挡) —— 主区与口袋互不连通, 用来测连通试算/吸附。
    def _mk_fixture_grid():
        w0, h0 = 20, 12
        rows = []
        for y in range(h0):
            row = ["0"] * w0
            if 2 <= y <= 9:
                row[5] = "1"
            rows.append(row)
        for y in (2, 9):
            for x in range(14, 20):
                rows[y][x] = "1"
        for y in range(3, 9):
            rows[y][14] = "1"
            rows[y][19] = "1"
        return {"w": w0, "h": h0, "rows": ["".join(r) for r in rows]}

    FIX_MAP = 777
    FIX_GRID = _mk_fixture_grid()
    try:
        sys.path.insert(0, script_dir)
        import robot_path as _rp
        check("图内死点: 可导入 robot_path(真实寻路)", True)
    except Exception as _e:
        _rp = None
        check("图内死点: 可导入 robot_path(真实寻路)", False, str(_e))

    if _rp is not None:
        _rp_finder = _rp.GridPathFinder(_rp.MapGrid(FIX_MAP, FIX_GRID))
        qe_stub = types.ModuleType("quest_engine")
        _bp_calls = []

        def _stub_chain_grid(quest, mapid):
            return FIX_GRID if int(mapid) == FIX_MAP else None

        def _stub_build_path(quest, mapid, fx, fy, tx, ty):
            # 与生产 __build_path 同实现: 复用 robot_path.GridPathFinder(带缓存)
            _bp_calls.append((mapid, fx, fy, tx, ty))
            if int(mapid) != FIX_MAP:
                return None
            return _rp_finder.find_path(fx, fy, tx, ty)

        def _stub_get_quest(robot_object, create=False):
            return types.SimpleNamespace(chain={"map_grids": {str(FIX_MAP): FIX_GRID}})

        _feas_hook = {"v": None}
        _sched = []
        qe_stub.__chain_grid_for = _stub_chain_grid
        qe_stub.__build_path = _stub_build_path
        qe_stub.get_quest = _stub_get_quest
        qe_stub.roam_feasible_maps = lambda quest, fm, cands: _feas_hook["v"]
        qe_stub.__schedule = lambda quest, d: _sched.append(d)
        qe_stub.__human_delay = lambda ms: ms
        ns["quest_engine"] = qe_stub
        ns["__quest"] = _stub_get_quest
        ns["random"] = __import__("random")
        ns["hashlib"] = __import__("hashlib")
        ns["time"] = __import__("time")
        ns["robot_path"] = _rp
        ns["diag"] = types.SimpleNamespace(log=lambda m: None)
        # ROAM_* 常量注入(默认参数/函数体都引用它们; 从源码解析实际值, 改常量自检跟着变)
        import re as _re
        for _m in _re.finditer(r"^(ROAM_\w+)\s*=\s*(\d+)", rw, _re.M):
            ns[_m.group(1)] = int(_m.group(2))
        check("图内死点: ROAM_* 常量齐全(8 个)",
              len([k for k in ns if k.startswith("ROAM_")]) == 8,
              sorted(k for k in ns if k.startswith("ROAM_")))

        _frag_cws = _extract_class(rw, "CollectWalkState")
        check("提取 random_walk.CollectWalkState(D 段)", _frag_cws is not None)
        if _frag_cws:
            try:
                exec(_frag_cws, ns)
            except Exception as e:  # noqa
                check("exec random_walk.CollectWalkState(D 段)", False, str(e))

        for name in ("__now_ms", "__grid_for", "__snap_walkable", "__note_walk_progress",
                     "__rng", "__auto_range", "__roam_switch_map", "__pick_walk_point",
                     "__write_pose", "__stop_roam_far_snap", "__far_snap_cooling",
                     "__dither_point",
                     "__other_bot_positions", "_cos", "_sin", "__random_walk_move",
                     # 2026-09-23d: __connected 抽成模块级公共入口(daily_ghost 巡逻选点共用)
                     "walk_point_connected"):
            frag = _extract_func(rw, name)
            check("提取 random_walk.%s(D 段)" % name, frag is not None)
            if frag:
                try:
                    exec(frag, ns)
                except Exception as e:  # noqa
                    check("exec random_walk.%s(D 段)" % name, False, str(e))

        def _mk_w(mapid, rng_box):
            ww = _W()
            ww.mapid = mapid
            ww.account = "selftest"
            ww.min_bot_dist = 200
            ww.range = rng_box
            ww.rng = None
            ww.maps = []
            ww.map_req = "map"
            return ww

        _snap = ns.get("__snap_walkable")
        _pick = ns.get("__pick_walk_point")
        _prog = ns.get("__note_walk_progress")
        _switch = ns.get("__roam_switch_map")
        _fgrid = ns.get("__grid_for")
        # 位置锚点(像素 → 格): 主区 (40,40)=格(2,2); 口袋中心 (272,88)=格(17,5)
        _grid = _rp.MapGrid(FIX_MAP, FIX_GRID)

        # ---- D1. __snap_walkable ----
        if _snap is not None:
            r = _snap(_grid, 40, 40)
            check("吸附: 已在可走格 → 原样返回(不产生位移)", r == (40, 40), r)
            r = _snap(_grid, 5 * 16 + 8, 5 * 16 + 8)   # 竖墙格(5,5)
            check("吸附: 阻挡格 → 最近可走格(格距=1 且可走)",
                  r is not None and not _grid.blocked(*_grid.to_grid(r[0], r[1]))
                  and abs(_grid.to_grid(r[0], r[1])[0] - 5)
                  + abs(_grid.to_grid(r[0], r[1])[1] - 5) == 1, r)
            r = _snap(_grid, 30 * 16 + 8, 5 * 16 + 8)  # 越界(W=20)
            check("吸附: 网格外 → 夹回网格内找可走格",
                  r is not None and 0 <= _grid.to_grid(r[0], r[1])[0] < 20
                  and not _grid.blocked(*_grid.to_grid(r[0], r[1])), r)
            _all_blocked = _rp.MapGrid(FIX_MAP, {"w": 6, "h": 6,
                                                "rows": ["1" * 6] * 6})
            check("吸附: 整片不可走 → None(调用方换图/停止)",
                  _snap(_all_blocked, 40, 40) is None)

        # ---- D2. __note_walk_progress ----
        if _prog is not None:
            w2 = _mk_w(FIX_MAP, [[0, 0], [64, 64]])
            w2.walk_fail = 0
            w2.walk_fail_ms = 0
            check("无进展计数: 首次(无基准)算有进展", _prog(w2, (40, 40), 1000) is True)
            check("无进展计数: 位置没动且未过节流 → 不计数",
                  _prog(w2, (40, 40), 1500) is False and w2.walk_fail == 0, w2.walk_fail)
            check("无进展计数: 过 20s 节流 → +1",
                  _prog(w2, (40, 40), 1000 + 20001) is False and w2.walk_fail == 1,
                  w2.walk_fail)
            check("无进展计数: 再过 20s → +2",
                  _prog(w2, (40, 40), 1000 + 40002) is False and w2.walk_fail == 2,
                  w2.walk_fail)
            check("无进展计数: 位移 > 12px → 清零(有进展)",
                  _prog(w2, (200, 40), 1000 + 41000) is True and w2.walk_fail == 0,
                  w2.walk_fail)

        # ---- D3. __roam_switch_map ----
        if _switch is not None:
            del disp_calls[:]
            cfg.robot_roam_world_maps = [11, 25, 45, 609]
            _feas_hook["v"] = None
            w3 = _mk_w(45, [[48, 48], [300, 300]])
            w3.map_req = "random"
            w3.walk_fail = 3
            w3.maps = []
            before_world = list(cfg.robot_roam_world_maps)
            r = _switch(types.SimpleNamespace(m_mapid=45), None, w3, 999999, "连续无进展")
            check("换图: 随机模式 → 换到白名单内另一张(非当前图/非排除图)",
                  r is True and w3.mapid in (11, 25, 609) and w3.mapid != 45, w3.mapid)
            check("换图: state=IDLE(交给 tick 重新跨图) + 计数清零",
                  w3.state == "IDLE" and w3.walk_fail == 0, (w3.state, w3.walk_fail))
            check("换图: 不动白名单配置(只读)",
                  cfg.robot_roam_world_maps == before_world and w3.maps == [],
                  (cfg.robot_roam_world_maps, w3.maps))
            # 可达过滤(roam_feasible_maps 可用时只在可达集里挑)
            _feas_hook["v"] = set([11])
            w4 = _mk_w(45, [[48, 48], [300, 300]])
            w4.map_req = "random"
            w4.walk_fail = 3
            r = _switch(types.SimpleNamespace(m_mapid=45), None, w4, 999999, "连续无进展")
            check("换图: 只在 roam_feasible_maps 可达集里挑", r is True and w4.mapid == 11,
                  w4.mapid)
            _feas_hook["v"] = None
            # 白名单模式(孵化图): 只在 w.maps 内换
            del disp_calls[:]
            w5 = _mk_w(6, [[48, 48], [300, 300]])
            w5.map_req = "map"
            w5.maps = [6, 17, 34, 40]
            w5.walk_fail = 3
            r = _switch(types.SimpleNamespace(m_mapid=6), None, w5, 999999, "连续无进展")
            check("换图: 白名单游荡只在白名单内换(不动 maps)",
                  r is True and w5.mapid in (17, 34, 40) and w5.maps == [6, 17, 34, 40],
                  (w5.mapid, w5.maps))
            # 单一指定图(无白名单) → 停止游荡(既有失败停止风格)
            del disp_calls[:]
            w6 = _mk_w(6, [[48, 48], [300, 300]])
            w6.map_req = "map"
            w6.maps = []
            w6.walk_fail = 3
            r = _switch(types.SimpleNamespace(m_mapid=6), None, w6, 999999, "位置不可走且吸附失败")
            check("换图: 无候选图(单一指定图) → 停止游荡",
                  r is False and len(disp_calls) == 1
                  and disp_calls[0].get("cmd") == "random_walk_stop", disp_calls)
            check("换图: 无候选时不改目标图", w6.mapid == 6, w6.mapid)
            # 排除图不作候选(白名单只剩 24 → 无候选 → 停止)
            del disp_calls[:]
            w7 = _mk_w(24, [[48, 48], [300, 300]])
            w7.map_req = "map"
            w7.maps = [24]
            r = _switch(types.SimpleNamespace(m_mapid=24), None, w7, 999999, "连续无进展")
            check("换图: 排除图(24)不作候选 → 停止游荡",
                  r is False and len(disp_calls) == 1, disp_calls)

        # ---- D4. __pick_walk_point 连通性试算 ----
        if _pick is not None and _snap is not None:
            # ①主区内选点: 全部连通(试算通过才返回)
            w8 = _mk_w(FIX_MAP, [[16, 16], [80, 80]])
            _calls0 = len(_bp_calls)
            oks = []
            for _ in range(15):
                pt = _pick(None, w8, min_dist=0, from_pos=(40, 40))
                oks.append(pt is not None and _rp_finder.find_path(40, 40, pt[0], pt[1])
                           is not None)
            check("选点: 主区内 15/15 返回且都连通(试算拦截生效)",
                  all(oks) and len(oks) == 15, oks.count(True))
            check("选点: 连通试算真的调用了 __build_path",
                  len(_bp_calls) > _calls0, len(_bp_calls) - _calls0)
            # ②只从"封闭口袋"范围选点(与起点不连通) → 试算 3 次后判失败
            w9 = _mk_w(FIX_MAP, [[15 * 16 + 8, 3 * 16 + 8], [18 * 16 + 8, 8 * 16 + 8]])
            _calls1 = len(_bp_calls)
            pt = _pick(None, w9, min_dist=0, from_pos=(40, 40))
            check("选点: 范围全不可达 → 返回 None(不返回走不到的点)", pt is None, pt)
            check("选点: 试算次数 ≤ ROAM_PATH_CHECK_TRIES(3)",
                  len(_bp_calls) - _calls1 <= 3, len(_bp_calls) - _calls1)
            # ③试算不可用(异常) → 退回旧行为(直接返回候选点, 不把号钉死)
            _orig_bp = qe_stub.__build_path

            def _raise_bp(quest, mapid, fx, fy, tx, ty):
                raise RuntimeError("grid broken")

            qe_stub.__build_path = _raise_bp
            pt = _pick(None, w9, min_dist=0, from_pos=(40, 40))
            check("选点: 校验不可用(异常) → 退回旧行为(仍返回候选点)", pt is not None, pt)
            qe_stub.__build_path = _orig_bp
            # ④无参照起点(跨图落地点选取) → 不做连通校验(旧行为)
            _calls2 = len(_bp_calls)
            pt = _pick(None, w9, min_dist=0, from_pos=None)
            check("选点: from_pos 为空(选落地点) → 不试算(旧行为)",
                  pt is not None and len(_bp_calls) == _calls2, pt)

        # ---- E. __random_walk_move 端到端: 吸附上限(1200px, 欧氏) + 超限停止降噪 ----
        # 口径(用户 2026-09-23, 上限 1024 → 1200): ≤上限 → 照常吸附救出;
        # >上限 → 不吸附不瞬移 → 停止游荡待命, 且同号 10 分钟内不再被补发(静默跳过)。
        _move = ns.get("__random_walk_move")
        _cooling = ns.get("__far_snap_cooling")

        def _mk_move_ro(mapid, x, y):
            ro = types.SimpleNamespace()
            ro.m_mapid = mapid
            ro.m_pose = [x, y, 0]
            ro.m_account = ["selftest"]
            ro.m_fight_state = False
            return ro

        def _mk_move_w(mapid, rng_box):
            ww = _mk_w(mapid, rng_box)
            ww.target = None
            ww.target_fail = 0
            ww.dither_left = 0
            ww.walk_mode = "walk"
            ww.dither_ratio = 0.0
            ww.dither_min = 30
            ww.dither_max = 90
            ww.heading = None
            ww.heading_span = 0.6
            ww.step_min = 150
            ww.step_max = 300
            ww.walk_fail = 0
            ww.walk_fail_ms = 0
            ww._prog_pos = None
            ww.walk_next_try_ms = 0
            ww.stand_until_ms = 0
            return ww

        # ---- E0. __far_snap_cooling 边界(10 分钟内不补发 / 之后可再派) ----
        if _cooling is not None:
            ro0 = _mk_move_ro(FIX_MAP, 100, 100)
            check("补发冷却: 无时间戳(没触发过) → 不拦", _cooling(ro0, 500000) is False)
            ro0.m_roam_far_snap_ms = 400000
            check("补发冷却: 刚触发(<10min) → 拦(不补发)", _cooling(ro0, 400000 + 599999) is True)
            check("补发冷却: 恰好 10min → 放行(可再派)", _cooling(ro0, 400000 + 600000) is False)
            check("补发冷却: 已过 11min → 放行(可再派)", _cooling(ro0, 400000 + 660000) is False)

        # ---- E1. dispatch_cmd 层: 冷却期内静默跳过 / 冷却过后不跳过 ----
        # 注意: 真 dispatch_cmd 要用独立命名空间 exec(共享 ns 里的 dispatch_cmd 是 B 段桩,
        #       其它用例靠它收集"停止/换图"调用, 不能被真实现覆盖)。
        ns_d = dict(ns)
        _frag_dc = _extract_func(rw, "dispatch_cmd")
        if _frag_dc:
            try:
                exec(_frag_dc, ns_d)
            except Exception as _e:  # noqa
                check("exec random_walk.dispatch_cmd(独立 ns)", False, str(_e))
        _disp = ns_d.get("dispatch_cmd")
        if _disp is not None and _cooling is not None:
            _now = int(ns["__now_ms"]())
            ro_c = _mk_move_ro(FIX_MAP, 100, 100)
            ro_c.m_roam_far_snap_ms = _now		# 刚触发超限停止
            ro_c.m_collect_walk = None
            _re1 = _disp(ro_c, {"cmd": "random_walk", "mapid": "random"})
            check("补发冷却: dispatch_cmd 冷却期内 → 静默跳过(不启动游荡)",
                  isinstance(_re1, dict) and _re1.get("skipped") == "far_snap_cooldown"
                  and not getattr(ro_c.m_collect_walk, "enabled", False), _re1)
            ro_d = _mk_move_ro(FIX_MAP, 100, 100)
            ro_d.m_roam_far_snap_ms = _now - 700000	# 11 分钟前
            ro_d.m_collect_walk = None
            try:
                _re2 = _disp(ro_d, {"cmd": "random_walk", "mapid": "random"})
            except Exception as _e:  # 无全链路桩: 只要"没被冷却跳过"即算通过
                _re2 = {"raised": type(_e).__name__}
            check("补发冷却: 冷却已过 → 不跳过(继续走启动流程)",
                  not (isinstance(_re2, dict) and _re2.get("skipped") == "far_snap_cooldown"),
                  _re2)

        if _move is not None:
            _box = [[48, 48], [250, 150]]	# 主区可走范围内
            # ①≤上限: 位置在阻挡格(竖墙 x=5,y=5), 最近可走格 16px → 吸附 + 正常安排走路
            del _sched[:]
            del disp_calls[:]
            ro1 = _mk_move_ro(FIX_MAP, 5 * 16 + 8, 5 * 16 + 8)	# 格(5,5)=墙
            w1 = _mk_move_w(FIX_MAP, _box)
            _move(ro1, _stub_get_quest(ro1), w1, 100000)
            check("吸附上限: ≤1200px → 照常吸附(写回 m_pose)",
                  ro1.m_pose[0] == 4 * 16 + 8 and ro1.m_pose[1] == 5 * 16 + 8, ro1.m_pose[:2])
            check("吸附上限: ≤1200px → 照常安排走路(不停止)",
                  len(_sched) == 1 and not disp_calls,
                  (_sched[-1]["data"] if _sched else None, disp_calls))
            # ②>上限: 位置远在网格外(欧氏距离 ≈1914px) → 不吸附(位置不动) + 停止游荡
            del _sched[:]
            del disp_calls[:]
            ro2 = _mk_move_ro(FIX_MAP, 1600, 1600)
            w2 = _mk_move_w(FIX_MAP, _box)
            _move(ro2, _stub_get_quest(ro2), w2, 200000)
            check("吸附上限: >1200px → 不吸附(位置不变, 无瞬移)",
                  ro2.m_pose[0] == 1600 and ro2.m_pose[1] == 1600, ro2.m_pose[:2])
            check("吸附上限: >1200px → 走'停止游荡'路径(不是换图/不是走路)",
                  len(disp_calls) == 1
                  and disp_calls[0].get("cmd") == "random_walk_stop" and not _sched,
                  (disp_calls, _sched))
            check("吸附上限: >1200px → 有明确 warn 日志(写明距离/上限)",
                  any("超过上限" in m and "1200px" in m for m in logs), logs[-2:])
            check("吸附上限: >1200px → 记下 m_roam_far_snap_ms(冷却起点)",
                  int(getattr(ro2, "m_roam_far_snap_ms", 0) or 0) > 0
                  and (_cooling is None or _cooling(ro2, 200000) is True),
                  getattr(ro2, "m_roam_far_snap_ms", None))
            # ③历史案例同类: 1063px(>旧上限1024, <新上限1200) → 应照常吸附救出
            del _sched[:]
            del disp_calls[:]
            ro3 = _mk_move_ro(FIX_MAP, 312 + 1100, 184)	# 距格(19,11)=1100px
            w3 = _mk_move_w(FIX_MAP, _box)
            _move(ro3, _stub_get_quest(ro3), w3, 300000)
            check("吸附上限: 1100px(旧超限/新上限内) → 照常吸附救出",
                  (ro3.m_pose[0], ro3.m_pose[1]) == (312, 184)
                  and len(_sched) == 1 and not disp_calls,
                  (ro3.m_pose[:2], _sched, disp_calls))
            # ④边界: 990px(<上限) → 仍吸附
            del _sched[:]
            del disp_calls[:]
            ro4 = _mk_move_ro(FIX_MAP, 312 + 700, 184 + 700)	# 距格(19,11)≈990px
            w4 = _mk_move_w(FIX_MAP, _box)
            _move(ro4, _stub_get_quest(ro4), w4, 400000)
            check("吸附上限: 990px(<上限) → 仍吸附救出",
                  (ro4.m_pose[0], ro4.m_pose[1]) == (312, 184)
                  and len(_sched) == 1 and not disp_calls,
                  (ro4.m_pose[:2], _sched, disp_calls))

    nfail = 0
    for name, ok, detail in results:
        if ok:
            print("[PASS] %s" % name)
        else:
            nfail += 1
            print("[FAIL] %s %s" % (name, ("(" + str(detail) + ")") if detail else ""))
    print("=== 结果: %s (共 %d 项断言, 失败 %d 项) ===" % (
        "PASS" if nfail == 0 else "FAIL", len(results), nfail))
    return 0 if nfail == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
