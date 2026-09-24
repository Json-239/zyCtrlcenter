# -*- coding: utf-8 -*-
"""跳转点"与当前位置连通"筛选自检 —— 2026-09-23f

背景（现场，只读取证）:
  游荡跨图刷屏残余（patrol=0 的号）：`地图 X 无可行路径 (x,y)→(152,246), 取消行走`
  —— 目标(152,246)是**跳转点 start_x/start_y**。旧兜底（2026-08-31）只把目标点校正到
  "离目标最近的可走格"(robot_path.nearest_walkable) 再寻路；但当该可走格与当前位置
  **不在同一连通域**（目标贴墙 → 最近可走格在墙的另一侧；或出入口跳转点天生不在同一
  连通域，见 tools/roam_map_connectivity_scan.py 的 7/43 图）→ 校正后仍无解 → 每次补发
  都"跨图重试→失败→就地游荡"的空转 + 刷屏。

修复（quest_engine.py 2026-09-23f/g，最小改动）:
  - 新增模块级 `__nearest_connected_cell(grid, from_x, from_y, to_x, to_y, max_radius=40)`:
    ①从起点格泛洪(8 邻、不斜穿墙角，与 robot_path.GridPathFinder._astar 同判据)得可达集;
    ②从目标格向外扩环，取第一个"可走且在可达集里"的格 = 能走到的最接近目标的合法位置
    （服务端 detect_skip_range 有 scope，走到附近合法位置也可能触发跳转）。
  - `__do_walk`："A* 无解 → 就近可走格校正"之后加**总兜底层**（`path2 is None` 时跑，
    与 `>8` 门槛同级 —— 否则"目标格可走"的场景根本进不去，筛选形同虚设）：
    **只在非任务链启用**（`quest.active=False`：游荡/抓鬼/商店等导航载体），链任务
    （goto/交付，`active=True`）保持原"立即失败 → NO_LEGAL_ROUTE → 停链报错"语义；
    半径 40 格内找不到可达格 → 同样保持原报错路径（不为不可达目标长距离空走）。
  - 异常/数据缺失 → 原路径（不拦）。

用法:
  python tools/hop_connectivity_selftest.py <script 目录 或 quest_engine.py 路径>
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


# 20x12 格(320x192 px):
#   x=5 竖墙(y=2..9) —— 左右只能从 y=0/1/10/11 绕行;
#   右下封闭口袋(x=15..18, y=3..8, 口字环全阻挡) —— 与主区互不连通。
FIX_MAP = 777


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


FIX_GRID = _mk_fixture_grid()
MAIN_POSE = (40, 40)                  # 主区: 格(2,2)
WALL_POSE = (5 * 16 + 8, 5 * 16 + 8)  # 竖墙格(5,5) = (88,88)
SNAP_POSE = (4 * 16 + 8, 5 * 16 + 8)  # 起点校正期望: 最近可走格(4,5) = (72,88)
TARGET_MAIN = (10 * 16 + 8, 6 * 16 + 8)   # 主区可走目标 = (168,104)
POCKET_POSE = (17 * 16 + 8, 5 * 16 + 8)   # 口袋中心 = (280,88) 与主区不连通
POCKET_NEAR_MAIN = (13 * 16 + 8, 5 * 16 + 8)  # 口袋左外侧主区格(13,5)=(216,88)


def main():
    if len(sys.argv) < 2:
        print("用法: python tools/hop_connectivity_selftest.py <script 目录 或 quest_engine.py 路径>")
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
    check("quest_engine 定义 __nearest_connected_cell(跳转点连通筛选)",
          "def __nearest_connected_cell(" in qe)
    check("__do_walk 总兜底位：path2 无解(且非链)时调用连通筛选(不取代旧逻辑)",
          "_alt = __nearest_connected_cell(_g2, from_x, from_y, to_x, to_y)" in qe
          and 'if path2 is None and not bool(getattr(quest, "active", False)):' in qe)
    check("连通筛选命中后用新目标重试寻路并改写 _nx/_ny",
          "path2b = __build_path(quest, robot_object.m_mapid," in qe
          and "_nx, _ny = _ax, _ay" in qe)
    check("泛洪判据与 A* 同源(8 邻、斜向要求两正交格可走)",
          "grid.blocked(cx + _dx, cy) or grid.blocked(cx, cy + _dy)" in qe)
    check("目标扩环有上限(默认 max_radius=40, 防整图扫描)",
          "def __nearest_connected_cell(grid, from_x, from_y, to_x, to_y, max_radius=40):" in qe)
    check("原 NO_LEGAL_ROUTE 报错路径保留(找不到才走原路径)",
          '地图 %d 无可行路径 (%d,%d)→(%d,%d), 取消行走' in qe)

    # 结构: 连通筛选必须在**真正的"path is None 总兜底位"** —— 即与 `if _near is not None:`
    #   同级（2026-09-23h 提升）。否则:
    #   ① 目标格可走但不可达(map45/map24 钟馗) → 旧放置连门槛都进不去;
    #   ② 目标在**厚阻挡区**(8 格半径内无可走格 → _near=None, 现场 map654 钟馗走位点)
    #      → 嵌在 if _near 内会整个跳过, 一直刷"无可行路径"。
    def _indent(s):
        return len(s) - len(s.lstrip("\t"))

    _abs_line = None
    _near_line = None
    _guard_line = None
    _lines = qe.splitlines()
    for _i, _ln in enumerate(_lines):
        if _abs_line is None and "if abs(_nx - to_x) + abs(_ny - to_y) > 8:" in _ln:
            _abs_line = _ln
        # 只看 __do_walk 无解分支里紧跟 `nearest_walkable(_g2, ...)` 的那个 _near 判定
        #   (force_walk 小步分支里有同名的 _near, 缩进更深, 别抓错; 提升后 _near 判定与
        #    它的赋值之间隔着 _nx/_ny/path2 的 None 初始化, 所以往后看 4 行)
        if _near_line is None and "nearest_walkable(_g2, to_x, to_y)" in _ln:
            for _ln2 in _lines[_i + 1:_i + 5]:
                if _ln2.strip() == "if _near is not None:":
                    _near_line = _ln2
                    break
        if _guard_line is None and _ln.strip().startswith("if path2 is None"):
            _guard_line = _ln
    check("连通筛选在总兜底位(与 if _near is not None 同级, 厚阻挡区也能进)",
          _near_line is not None and _guard_line is not None
          and _indent(_near_line) == _indent(_guard_line),
          (_near_line, _guard_line))
    check("连通筛选只在非任务链启用(quest.active=False; 游荡/抓鬼/商店)",
          'if path2 is None and not bool(getattr(quest, "active", False)):' in qe)
    check("链任务失败语义保留(active=True → 原 NO_LEGAL_ROUTE 停链报错)",
          "if quest.active:" in qe and "quest.set_state(quest_state.ST_ERROR)" in qe)
    check("path2/_nx/_ny 先置 None(总兜底不依赖 _near 是否存在)",
          "\t\t\t\t_nx, _ny = None, None" in qe and "\t\t\t\tpath2 = None" in qe)

    # ============================================================ B. 执行(真实 robot_path)
    try:
        sys.path.insert(0, script_dir)
        import robot_path as _rp
        check("可导入 robot_path(真实寻路)", True)
    except Exception as _e:  # noqa
        _rp = None
        check("可导入 robot_path(真实寻路)", False, str(_e))

    if _rp is not None:
        grid = _rp.MapGrid(FIX_MAP, FIX_GRID)
        finder = _rp.GridPathFinder(grid)
        ns = {"robot_path": _rp}
        frag = _extract_func(qe, "__nearest_connected_cell")
        check("提取 quest_engine.__nearest_connected_cell", frag is not None)
        if frag:
            try:
                exec(frag, ns)
            except Exception as e:  # noqa
                check("exec quest_engine.__nearest_connected_cell", False, str(e))
        ncc = ns.get("__nearest_connected_cell")

        if ncc is not None:
            # B1. 目标可走且同连通域 → 就是目标格本身(r=0)
            r = ncc(grid, MAIN_POSE[0], MAIN_POSE[1], TARGET_MAIN[0], TARGET_MAIN[1])
            check("同域可走目标 → 返回目标格自身(不绕路)",
                  r == grid.to_grid(*TARGET_MAIN), r)

            # B2. 目标落在阻挡格(竖墙) → 返回墙这侧最近的可走格(旧逻辑会选墙另一侧)
            r = ncc(grid, MAIN_POSE[0], MAIN_POSE[1], WALL_POSE[0], WALL_POSE[1])
            check("目标在阻挡格 → 返回'可达且离目标最近'的格(墙这侧)",
                  r == (4, 5), r)
            check("B2 返回格确实与当前位置连通",
                  r is not None and finder.find_path(
                      MAIN_POSE[0], MAIN_POSE[1], r[0] * 16 + 8, r[1] * 16 + 8) is not None, r)

            # B3. 目标在断开的口袋里 → 返回"能走到的最接近目标的合法位置"
            #     (13,5) 与 (17,1) 到目标都是 64px(等距), 扫描序取 (17,1) —— 两者都在主区
            r = ncc(grid, MAIN_POSE[0], MAIN_POSE[1], POCKET_POSE[0], POCKET_POSE[1])
            check("目标在别的连通域 → 返回可达的最近格(与目标等距 64px 的 (17,1))",
                  r == (17, 1), r)
            check("B3 返回格确实与当前位置连通",
                  r is not None and finder.find_path(
                      MAIN_POSE[0], MAIN_POSE[1], r[0] * 16 + 8, r[1] * 16 + 8) is not None, r)

            # B4. 起点落阻挡格 → 先校正起点(nearest_walkable)再泛洪
            r = ncc(grid, WALL_POSE[0], WALL_POSE[1], TARGET_MAIN[0], TARGET_MAIN[1])
            check("起点阻挡 → 先校正到 (4,5)/(6,5) 再筛选(仍返回同域目标)",
                  r == grid.to_grid(*TARGET_MAIN), r)
            check("B4 返回格与校正后的起点连通",
                  finder.find_path(SNAP_POSE[0], SNAP_POSE[1],
                                   TARGET_MAIN[0], TARGET_MAIN[1]) is not None)

            # B5. 扩环用尽 → 2026-09-23g 起改为"扫全可达集取像素最近"(不再返回 None):
            #     扩环只是"找近点"的快路径; 用尽后扫一遍泛洪集 —— 保证不会因半径判定
            #     又把号钉死(现场 map16 真最近可达点切比雪夫 45 格 > max_radius=40)
            r = ncc(grid, MAIN_POSE[0], MAIN_POSE[1], POCKET_POSE[0], POCKET_POSE[1],
                    max_radius=2)
            check("扩环用尽(半径 2) → 扫全可达集取像素最近 (13,5)(不再 None)",
                  r == (13, 5), r)
            check("B5 返回格仍与当前位置连通",
                  r is not None and finder.find_path(
                      MAIN_POSE[0], MAIN_POSE[1], r[0] * 16 + 8, r[1] * 16 + 8) is not None, r)

            # B6. 网格外目标 → 同样扫全可达集, 取离它最近的可达格(网格右下角 (19,11))
            r = ncc(grid, MAIN_POSE[0], MAIN_POSE[1], 3000, 3000)
            check("目标在网格外 → 扫全可达集取像素最近 (19,11)", r == (19, 11), r)
            check("B6 返回格仍与当前位置连通",
                  r is not None and finder.find_path(
                      MAIN_POSE[0], MAIN_POSE[1], r[0] * 16 + 8, r[1] * 16 + 8) is not None, r)

            # B7. 整片不可走 → None
            _all_blocked = _rp.MapGrid(FIX_MAP, {"w": 6, "h": 6, "rows": ["1" * 6] * 6})
            r = ncc(_all_blocked, 40, 40, 40, 40)
            check("整片不可走 → None(调用方报错)", r is None, r)

            # B8. 真实生产网格抽查: map45 的跳转点(离线只读, 有链数据才测)
            try:
                import json
                _repo = os.path.abspath(os.path.join(script_dir, "..", "..", "..", ".."))
                chain_fp = os.path.join(_repo, "data", "chains", "zhongkui_nav.json")
                if os.path.exists(chain_fp):
                    _ch = json.load(open(chain_fp, encoding="utf-8"))
                    _g45 = (_ch.get("map_grids") or {}).get("45")
                    if _g45:
                        _grid45 = _rp.MapGrid(45, _g45)
                        # 现场: (1074,907) 走不到 (152,246)
                        _old = _rp.GridPathFinder(_grid45).find_path(1074, 907, 152, 246)
                        _c = ncc(_grid45, 1074, 907, 152, 246)
                        _new = None
                        if _c is not None:
                            _new = _rp.GridPathFinder(_grid45).find_path(
                                1074, 907, _c[0] * 16 + 8, _c[1] * 16 + 8)
                        check("真实 map45 现场: 原目标不可达 → 筛选给出'可达的最近格'",
                              _old is None and _c is not None and _new is not None,
                              (_old, _c, _new is not None))
                        # 现场 map16: 真最近可达点在半径(40)之外 → 扫全可达集仍给出可达点
                        _g16 = (_ch.get("map_grids") or {}).get("16")
                        if _g16:
                            _grid16 = _rp.MapGrid(16, _g16)
                            _old16 = _rp.GridPathFinder(_grid16).find_path(1766, 2436, 346, 257)
                            _c16 = ncc(_grid16, 1766, 2436, 346, 257)
                            _new16 = None
                            if _c16 is not None:
                                _new16 = _rp.GridPathFinder(_grid16).find_path(
                                    1766, 2436, _c16[0] * 16 + 8, _c16[1] * 16 + 8)
                            check("真实 map16 现场: 真最近可达点在半径外 → 仍给出可达点",
                                  _old16 is None and _c16 is not None and _new16 is not None,
                                  (_old16, _c16, _new16 is not None))
                    else:
                        check("真实 map45 现场: 链里有 map45 网格", False, "链数据无 map45")
                else:
                    check("真实网格抽查: 有 data/chains/zhongkui_nav.json(可选)", True,
                          "跳过(无链数据)")
            except Exception as _e2:
                check("真实网格抽查(可选)未抛异常", False, str(_e2))

    # ============================================================ B9. 起点校正增强(2026-09-23h)
    # 现场: map49 进图到达点 (1160,856) = 阻挡格, 距最近可走格 11 格 > nearest_walkable
    #   默认 radius=8 → 旧代码不校正/不写回 → A* 从阻挡起点直接 NO_LEGAL_ROUTE
    #   ("地图 49 无可行路径")。定稿修法 __walk_start_fix_cell:
    #   ①先按旧行为 radius=8 搜(行为不变); ②找不到→夹回网格再扩到 40 格(640px)搜(越界格
    #   恒=阻挡, 环形搜索命中不了; 与 random_walk.__snap_walkable 同法); ③校正距离超上限
    #   (同 ROAM_SNAP_MAX_DIST_PX 口径, 缺省 1024) → 不写回=不瞬移(map24 越界点夹回后
    #   最近可走格 2026px, 属位置本身异常, 交上层游荡吸附/抓鬼重登)。
    check("quest_engine 定义 __walk_start_fix_cell(夹回+40格+距离上限)",
          "def __walk_start_fix_cell(" in qe)
    check("__do_walk 起点校正块改调 helper(不再直调 nearest_walkable radius=40)",
          "_fix0 = __walk_start_fix_cell(_g0, from_x, from_y)" in qe
          and "nearest_walkable(_g0, from_x, from_y, max_radius=40)" not in qe)
    check("写回机制保持原样(helper 返回校正点才 set_pose; 起点未阻挡不触发)",
          "_fix0 = None" in qe
          and '__set_pose(robot_object, _nx0, _ny0, "walk_start_fix")' in qe)
    ns_fix = {"robot_path": _rp, "math": __import__("math")} if _rp is not None else {}
    frag_fix = _extract_func(qe, "__walk_start_fix_cell")
    check("提取 quest_engine.__walk_start_fix_cell", frag_fix is not None)
    if frag_fix:
        try:
            exec(frag_fix, ns_fix)
        except Exception as e:  # noqa
            check("exec quest_engine.__walk_start_fix_cell", False, str(e))
    fixc = ns_fix.get("__walk_start_fix_cell")
    if _rp is not None and fixc is not None:
        # 夹具网格(20x12): 夹回逻辑 + 旧 radius=8 行为两个用例
        _fg = _rp.MapGrid(FIX_MAP, FIX_GRID)
        # 夹回分支: (800,88) 格(50,5) 远在网格外 → 直接搜(radius=8)必然 None
        #   → 夹回(19,5)(口袋右墙, 阻挡) → 扩到 40 格 → 最近可走格 (18,5)=(296,88)
        r = fixc(_fg, 800, 88)
        check("越界远点: 直接搜不到 → 夹回后取最近可走格 (18,5)/(296,88)(网格内可走)",
              _rp.nearest_walkable(_fg, 800, 88) is None and r == (296, 88)
              and not _fg.blocked(*_fg.to_grid(r[0], r[1])), r)
        r = fixc(_fg, WALL_POSE[0], WALL_POSE[1])	# 墙格(5,5) → radius=8 就近(4,5)
        check("旧 radius=8 就近行为保留: 阻挡格(88,88) → (72,88)", r == (72, 88), r)
        # 真实网格用例(离线只读, 有链数据才测)
        try:
            import json as _json_h
            _repo_h = os.path.abspath(os.path.join(script_dir, "..", "..", "..", ".."))
            _cfp_h = os.path.join(_repo_h, "data", "chains", "zhongkui_nav.json")
            _ch = _json_h.load(open(_cfp_h, encoding="utf-8")) if os.path.exists(_cfp_h) else None
        except Exception:
            _ch = None
        _g49 = ((_ch or {}).get("map_grids") or {}).get("49")
        if _g49:
            _grid49 = _rp.MapGrid(49, _g49)
            check("map49 前提: 到达点(1160,856) radius=8 → None(旧代码不校正、直接刷屏)",
                  _rp.nearest_walkable(_grid49, 1160, 856, max_radius=8) is None)
            r = fixc(_grid49, 1160, 856)
            _d = None if r is None else int((((r[0] - 1160) ** 2 + (r[1] - 856) ** 2)) ** 0.5)
            check("map49 现场: helper → (1336,856)(176px ≤ 上限) 且到目标(1753,918)可达",
                  r == (1336, 856) and _d == 176
                  and _rp.GridPathFinder(_grid49).find_path(r[0], r[1], 1753, 918) is not None,
                  (r, _d))
            check("map49 现场: 上限压到 100px → 不校正(返回 None, 不写回)",
                  fixc(_grid49, 1160, 856, max_dist_px=100) is None)
        _g24 = ((_ch or {}).get("map_grids") or {}).get("24")
        if _g24:
            _grid24 = _rp.MapGrid(24, _g24)
            _gx24, _gy24 = _grid24.to_grid(4712, 2776)
            _cx24 = min(max(_gx24, 0), max(_grid24.w - 1, 0))
            _cy24 = min(max(_gy24, 0), max(_grid24.h - 1, 0))
            _px24, _py24 = _grid24.to_coord(_cx24, _cy24)
            _n24 = _rp.nearest_walkable(_grid24, _px24, _py24, max_radius=40)
            _d24 = None
            if _n24 is not None:
                _d24 = int((((_n24[0] * 16 + 8) - 4712) ** 2
                            + ((_n24[1] * 16 + 8) - 2776) ** 2) ** 0.5)
            check("map24 现场: 越界格(294,173)→夹回→最近可走格 %spx(>上限1024) → 不写回" % (
                _d24,),
                  _gx24 == 294 and _n24 is not None and _d24 is not None and _d24 > 1024
                  and fixc(_grid24, 4712, 2776) is None, (_gx24, _d24))
        if _ch is None:
            check("起点校正真实网格用例(可选): 有链数据", True, "跳过(无链数据)")

    # ============================================================ B10. 起点连通性校正(2026-09-23i 对称兜底)
    # 现场: map45 入口走廊孤立域 —— 号跳进 map45 落在"入口走廊"连通域(506 格, 含出口跳转点),
    #   目标(钟馗走位点 (1162,848))在另一连通域(2729 格):
    #   ① __walk_start_fix_cell 不触发(起点 (632,440) 可走, 不是落墙/越界);
    #   ② 目标连通兜底 ncc(from→to) 退化: 起点域里离目标最近的点就是起点自己(668px)
    #      → path2b 起终点同格必 None → 仍 NO_LEGAL_ROUTE(现场 robot0001116 18:53 连报 3 次)。
    #   修法 __walk_start_connect_fix_cell: 把 ncc 的"泛洪源/扫描点"对调(从**目标**泛洪、
    #   从**起点**扩环) → 校正到"与目标连通且离起点最近"的格 (648,696)(距起点 256px) → A* 通。
    #   保守口径: 校正点与起点同格(≤1px) / 距离超上限(同 ROAM_SNAP_MAX_DIST_PX 口径, 退 1024)
    #   → 返回 None(不写回=不瞬移); 调用方只在非任务链启用 + 写回前先试算路径存在。
    import math as _math_c
    check("quest_engine 定义 __walk_start_connect_fix_cell(起点连通性校正)",
          "def __walk_start_connect_fix_cell(" in qe)
    _i_alt = qe.find("_alt = __nearest_connected_cell(_g2, from_x, from_y, to_x, to_y)")
    _i_sf = qe.find("_sf0 = __walk_start_connect_fix_cell(_g2, from_x, from_y, to_x, to_y)")
    _i_fail = qe.find("地图 %d 无可行路径 (%d,%d)→(%d,%d), 取消行走")
    check("新校正在目标连通兜底之后、NO_LEGAL_ROUTE 报错之前(依次兜底不取代旧逻辑)",
          -1 < _i_alt < _i_sf < _i_fail, (_i_alt, _i_sf, _i_fail))
    check("新校正只在非任务链启用(与目标连通兜底同口径, 链任务保持立即失败停链)",
          qe.count('if path2 is None and not bool(getattr(quest, "active", False)):') >= 2)
    check("写回统一走 __set_pose(..., \"walk_start_fix\") + 中文 warn(含距 Npx)",
          '__set_pose(robot_object, _snx0, _sny0, "walk_start_fix")' in qe
          and "起点(%d,%d)与目标不连通, 校正到(%d,%d) 距 %dpx" in qe)
    check("写回前先试算(校正点→目标 路径存在才写回, 防搬了家仍走不通)",
          "path2s = __build_path(quest, robot_object.m_mapid," in qe
          and "path2 = path2s" in qe)
    check("保守口径: 同格/超上限 → None(不写回=不瞬移; 复用 ROAM_SNAP_MAX_DIST_PX)",
          "if _dist <= 1:" in qe and "if _dist > max_dist_px:" in qe
          and 'getattr(random_walk, "ROAM_SNAP_MAX_DIST_PX", max_dist_px)' in qe)

    try:
        _ns_base = ns
    except NameError:
        _ns_base = {}
    ns_conn = dict(_ns_base) if _rp is not None else {}
    ns_conn["math"] = _math_c
    frag_conn = _extract_func(qe, "__walk_start_connect_fix_cell")
    check("提取 quest_engine.__walk_start_connect_fix_cell", frag_conn is not None)
    if frag_conn:
        try:
            exec(frag_conn, ns_conn)
        except Exception as e:  # noqa
            check("exec quest_engine.__walk_start_connect_fix_cell", False, str(e))
    connc = ns_conn.get("__walk_start_connect_fix_cell")
    if _rp is not None and connc is not None:
        # ---- 夹具(20x12, 见 FIX_GRID 注释): 主区(40,40) 与右下口袋(280,88) 不连通 ----
        _cfg = _rp.MapGrid(FIX_MAP, FIX_GRID)
        _cfinder = _rp.GridPathFinder(_cfg)
        check("夹具前提: 主区(40,40)→口袋(280,88) A* 无解",
              _cfinder.find_path(40, 40, 280, 88) is None)
        r = connc(_cfg, 40, 40, 280, 88)
        _dc = None if r is None else int(_math_c.sqrt((r[0] - 40) ** 2 + (r[1] - 40) ** 2))
        check("夹具: 起点可走但与口袋不连通 → 校正到口袋域最近格 (248,56), 距 208px",
              r == (248, 56) and _dc == 208, (r, _dc))
        check("夹具: 校正点与目标连通(校正后 A* 通, 修复生效)",
              r is not None and _cfinder.find_path(r[0], r[1], 280, 88) is not None)
        check("夹具: 超限(max_dist_px=100) → None(不写回)",
              connc(_cfg, 40, 40, 280, 88, max_dist_px=100) is None)
        check("夹具: 起点与目标同域可通 → None(不该校正; ncc 返回起点自身格, 距离 0)",
              connc(_cfg, 40, 40, TARGET_MAIN[0], TARGET_MAIN[1]) is None)
        _all_blocked2 = _rp.MapGrid(FIX_MAP, {"w": 6, "h": 6, "rows": ["1" * 6] * 6})
        check("夹具: 整片不可走 → None(交调用方原报错)",
              connc(_all_blocked2, 40, 40, 40, 40) is None)

        # ---- 真实网格(离线只读, 有链数据才测) ----
        if _ch is None:
            check("起点连通真实网格用例(可选): 有链数据", True, "跳过(无链数据)")
        else:
            _g45c = (_ch.get("map_grids") or {}).get("45")
            if _g45c:
                _grid45c = _rp.MapGrid(45, _g45c)
                _f45c = _rp.GridPathFinder(_grid45c)
                _sgx, _sgy = _grid45c.to_grid(632, 440)
                _tgx, _tgy = _grid45c.to_grid(1162, 848)
                check("现场 map45 前提: 起点/目标都可走, 但 A* 无解(起点孤立域 506 格)",
                      not _grid45c.blocked(_sgx, _sgy) and not _grid45c.blocked(_tgx, _tgy)
                      and _f45c.find_path(632, 440, 1162, 848) is None
                      and _rp.nearest_walkable(_grid45c, 632, 440) == (_sgx, _sgy))
                r = connc(_grid45c, 632, 440, 1162, 848)
                _d45 = None if r is None else int(_math_c.sqrt((r[0] - 632) ** 2 + (r[1] - 440) ** 2))
                check("现场 map45: 校正到 (648,696)(距起点 256px, 与目标同域)",
                      r == (648, 696) and _d45 == 256, (r, _d45))
                check("现场 map45: 校正后 A* 到目标可达(修复生效)",
                      r is not None and _f45c.find_path(r[0], r[1], 1162, 848) is not None)
                r2 = connc(_grid45c, 632, 440, 1720, 1128)
                _nw45 = _rp.nearest_walkable(_grid45c, 1720, 1128)
                _ok45b = False
                if r2 is not None and _nw45 is not None:
                    _ok45b = _f45c.find_path(r2[0], r2[1],
                        _nw45[0] * 16 + 8, _nw45[1] * 16 + 8) is not None
                check("现场 map45: 目标(1720,1128)本身阻挡 → 校正到 (648,696) 且到就近可走格可达",
                      r2 == (648, 696) and _ok45b, (r2, _nw45))
                check("现场 map45: 上限压到 100px → None(超限不写回)",
                      connc(_grid45c, 632, 440, 1162, 848, max_dist_px=100) is None)
            # ---- 回归: map24 越界起点 仍"超限不写回" ----
            _g24c = (_ch.get("map_grids") or {}).get("24")
            if _g24c:
                _grid24c = _rp.MapGrid(24, _g24c)
                check("回归 map24: 越界起点(4712,2776) 最近可达面 2026px > 上限 → None(仍不写回)",
                      connc(_grid24c, 4712, 2776, 1768, 1064) is None)
                check("回归 map24: 闸门确为距离上限(放宽到 3000px 才给出候选 (2936,1800))",
                      connc(_grid24c, 4712, 2776, 1768, 1064, max_dist_px=3000) == (2936, 1800))
            # ---- 回归: map49 阻挡起点 仍走"阻挡校正"(B9), 且两机制对同一场景给同一校正点 ----
            _g49c = (_ch.get("map_grids") or {}).get("49")
            if _g49c:
                _grid49c = _rp.MapGrid(49, _g49c)
                check("回归 map49: 阻挡起点仍由 __walk_start_fix_cell 解决(新 helper 同点一致)",
                      fixc is not None and fixc(_grid49c, 1160, 856) == (1336, 856)
                      and connc(_grid49c, 1160, 856, 1753, 918) == (1336, 856))

    # ============================================================ C. "免费 hop 有效性"机制
    # (2026-09-23g, 用户口径: 免费跳点走不到 → 优先改走 NPC 跳转)
    #   A. hop_first_reach_ok: 首跳可达性预判(可达→免费优先; 不可达但就近点 ≤400px→仍走;
    #      不可达且偏太远→判用不了, 调用方改走含 npc_jumper 路线);
    #   B. 走不到也记 __hop_fail_note(report=False): 连 3 次 → 拉黑 10 分钟 →
    #      之后规划自动改走 npc_jumper 备选。
    if _rp is not None:
        check("quest_engine 定义 hop_first_reach_ok(首跳可达性公共入口)",
              "def hop_first_reach_ok(" in qe)
        check("quest_engine 定义 HOP_NEAR_STEP_PX(就近点偏差上限 400px)",
              "HOP_NEAR_STEP_PX = 400" in qe)
        check("__hop_fail_note 支持 why/report(走不到可只计数+拉黑, 不重规划不停链)",
              'def __hop_fail_note(robot_object, quest, hop, dest, why="被拒", report=True):' in qe)
        check("__do_walk 的 NO_LEGAL_ROUTE 分支: 带 hop 的走不到也记账",
              '__hop_fail_note(robot_object, quest, _hop, "walk", why="走不到", report=False)' in qe)
        rw_path = os.path.join(script_dir, "random_walk.py")
        rw = open(rw_path, encoding="utf-8", errors="replace").read() if os.path.exists(rw_path) else ""
        check("random_walk.__goto_map: 首跳可达性预判 + 改走 NPC 路线",
              "elif not quest_engine.hop_first_reach_ok(quest, robot_object, route):" in rw
              and "改走跳转NPC 路线" in rw)

        # ---- C1. hop_first_reach_ok 语义(自建紧凑网格, 不依赖 B 段 fixture 细节) ----
        # 12x8: x=6 竖墙(y=2..7, 上方 y=0/1 可绕) + 右下 1x2 封闭口袋(x=9..10,y=4..5)
        def _c_grid():
            rows = []
            for y in range(8):
                row = ["0"] * 12
                if 2 <= y <= 7:
                    row[6] = "1"
                rows.append(row)
            for y in (3, 6):
                for x in range(7, 12):
                    rows[y][x] = "1"
            for y in range(4, 6):
                rows[y][8] = "1"
                rows[y][11] = "1"
            return {"w": 12, "h": 8, "rows": ["".join(r) for r in rows]}

        C_GRID = _c_grid()
        _cgrid = _rp.MapGrid(FIX_MAP, C_GRID)
        _cfinder = _rp.GridPathFinder(_cgrid)
        _c_main = (40, 40)			# 格(2,2) 主区
        _c_pocket = (9 * 16 + 8, 4 * 16 + 8)	# 格(9,4) 口袋内(与主区不连通)

        cns = {"robot_path": _rp, "HOP_NEAR_STEP_PX": 400}
        import re as _re2
        for _m2 in _re2.finditer(r"^(HOP_NEAR_STEP_PX)\s*=\s*(\d+)", qe, _re2.M):
            cns[_m2.group(1)] = int(_m2.group(2))
        cns["__chain_grid_for"] = lambda quest, mapid: C_GRID

        def _c_build_path(quest, mapid, fx, fy, tx, ty):
            return _cfinder.find_path(fx, fy, tx, ty)

        cns["__build_path"] = _c_build_path
        _fr_ns = dict(cns)
        _frag_ncc = _extract_func(qe, "__nearest_connected_cell")
        if _frag_ncc:
            exec(_frag_ncc, _fr_ns)
        _frag_hfr = _extract_func(qe, "hop_first_reach_ok")
        check("提取 quest_engine.hop_first_reach_ok", _frag_hfr is not None)
        if _frag_hfr:
            try:
                exec(_frag_hfr, _fr_ns)
            except Exception as e:  # noqa
                check("exec quest_engine.hop_first_reach_ok", False, str(e))
        hfr = _fr_ns.get("hop_first_reach_ok")
        if hfr is not None:
            class _RO(object):
                pass
            ro = _RO()
            ro.m_mapid = FIX_MAP
            ro.m_pose = [_c_main[0], _c_main[1], 0]
            q = types.SimpleNamespace(chain={"map_grids": {str(FIX_MAP): C_GRID}})
            check("首跳可达性: 免费跳点可达 → True(免费优先, 行为不变)",
                  hfr(q, ro, [{"kind": "map_skip", "x": _c_main[0], "y": _c_main[1]}]) is True)
            check("首跳可达性: 不可达但就近可走点很近(≤400px) → True(走过去仍可能触发免费跳)",
                  hfr(q, ro, [{"kind": "map_skip", "x": _c_pocket[0], "y": _c_pocket[1]}]) is True)
            _far_pt = (FIX_MAP and 12 * 16 + 600, 4 * 16 + 8)	# 网格外 600px+, 就近点偏太远
            check("首跳可达性: 不可达且就近点偏太远(>400px) → False(改走 NPC 路线)",
                  hfr(q, ro, [{"kind": "map_skip", "x": _far_pt[0], "y": _far_pt[1]}]) is False)
            check("首跳可达性: 非 map_skip(npc_jumper) → True(不拦)",
                  hfr(q, ro, [{"kind": "npc_jumper", "x": 9999, "y": 9999}]) is True)
            check("首跳可达性: 跳点无坐标 → True(不可判不拦)",
                  hfr(q, ro, [{"kind": "map_skip"}]) is True)
            _fr_ns["__build_path"] = lambda *a, **kw: (_ for _ in ()).throw(RuntimeError("boom"))
            check("首跳可达性: 试算异常 → True(退回旧行为, 不钉死)",
                  hfr(q, ro, [{"kind": "map_skip", "x": _far_pt[0], "y": _far_pt[1]}]) is True)
            _fr_ns["__build_path"] = _c_build_path

        # ---- C2. __hop_fail_note(report=False) 计数 → 拉黑; 拉黑后规划改走 NPC ----
        ev_logs = []
        fns = {"HOP_FAIL_LIMIT": 3, "HOP_BLACKLIST_MS": 10 * 60 * 1000,
               "__hop_key": lambda fm, e: (int(fm or 0), e.get("destination_index")),
               "__emit": lambda ro, ev: ev_logs.append(ev.get("msg") or ""),
               "__report_stuck": lambda ro, q, why: ev_logs.append("STUCK:" + why),
               "__teleport_click": lambda *a, **kw: ev_logs.append("REPLAN")}
        import time as _t2
        fns["time"] = _t2
        for _m3 in _re2.finditer(r"^(HOP_FAIL_LIMIT|HOP_BLACKLIST_MS)\s*=\s*(\d+)", qe, _re2.M):
            fns[_m3.group(1)] = int(_m3.group(2))
        _frag_note = _extract_func(qe, "__hop_fail_note")
        check("提取 quest_engine.__hop_fail_note", _frag_note is not None)
        if _frag_note:
            try:
                exec(_frag_note, fns)
            except Exception as e:  # noqa
                check("exec quest_engine.__hop_fail_note", False, str(e))
        note = fns.get("__hop_fail_note")
        if note is not None:
            class _RO2(object):
                pass
            ro2 = _RO2()
            ro2.m_mapid = 11
            q2 = types.SimpleNamespace(chain=None, hop_black={})
            hop = {"kind": "map_skip", "destination_index": 61, "target_map": 45,
                   "x": 152, "y": 246, "_from_map": 11}
            r1 = note(ro2, q2, hop, "walk", why="走不到", report=False)
            r2 = note(ro2, q2, hop, "walk", why="走不到", report=False)
            check("走不到记账: 第 1/2 次未达阈值 → False(不拉黑)",
                  r1 is False and r2 is False and hop.get("_fail") == 2 and not q2.hop_black,
                  (r1, r2, hop.get("_fail")))
            r3 = note(ro2, q2, hop, "walk", why="走不到", report=False)
            check("走不到记账: 第 3 次 → True + 拉黑 10 分钟(键=(11,61))",
                  r3 is True and (11, 61) in q2.hop_black
                  and q2.hop_black[(11, 61)] > _t2.time() * 1000,
                  q2.hop_black)
            check("走不到记账: 日志写明'走不到'(not 被拒)",
                  any("走不到" in m and "拉黑 10 分钟" in m for m in ev_logs), ev_logs[-3:])
            check("走不到记账: report=False → 不重规划/不停链",
                  not any(m == "REPLAN" or m.startswith("STUCK:") for m in ev_logs), ev_logs[-3:])

            # 拉黑后: 同图对的免费 hop 被跳过, 改走 npc_jumper 备选(第二次规划自动换)
            gns = {"__hop_key": fns["__hop_key"], "time": _t2,
                   "g_chain_grid_cache": {}, "g_chain_dijkstra_cache": {}}
            for _m4 in _re2.finditer(r"^(BAD_JUMPERS|HOP_FAIL_LIMIT|HOP_BLACKLIST_MS)\s*=", qe, _re2.M):
                pass
            _frag_find = _extract_func(qe, "__find_dijkstra_route")
            check("提取 quest_engine.__find_dijkstra_route", _frag_find is not None)
            if _frag_find:
                try:
                    exec(_frag_find, gns)
                except Exception as e:  # noqa
                    check("exec quest_engine.__find_dijkstra_route", False, str(e))
            find_route = gns.get("__find_dijkstra_route")
            if find_route is not None:
                TABLE = {"11": [{"kind": "map_skip", "destination_index": 61, "target_map": 45,
                                 "x": 152, "y": 246, "_from_map": 11},
                                {"kind": "npc_jumper", "destination_index": 62, "target_map": 45,
                                 "x": 300, "y": 300, "npc_index": 1, "cost_money": 10,
                                 "_from_map": 11}]}
                qa = types.SimpleNamespace(chain={"dijkstra": dict(TABLE)}, hop_black={})
                _r_free = find_route(qa, 11, 45, no_npc_jumper=True)
                check("规划: 拉黑前免费路线可用(no_npc_jumper=True 选中 map_skip)",
                      _r_free is not None and _r_free[0].get("kind") == "map_skip", _r_free)
                qb = types.SimpleNamespace(chain={"dijkstra": dict(TABLE)},
                                           hop_black={(11, 61): _t2.time() * 1000 + 600000})
                _r_after = find_route(qb, 11, 45, no_npc_jumper=True)
                check("规划: 免费 hop 是唯一 pure 选项且被拉黑 → 忽略黑名单再找一次(不返回 None, 旧安全网保留)",
                      _r_after is not None, _r_after)
                _r_npc = find_route(qb, 11, 45, no_npc_jumper=False, skip_bad_jumpers=True)
                check("规划: 拉黑后改走 npc_jumper 备选(第二次规划自动换)✓",
                      _r_npc is not None and _r_npc[0].get("kind") == "npc_jumper"
                      and _r_npc[0].get("cost_money") == 10, _r_npc)

    # ============================================================ D. 新手链 7001115 卡死修复(2026-09-24)
    # 现场(robot0003001/robot0005017, 09-23/09-24): 反复 TASK_STUCK, 每轮 2-3 分钟永不自愈。
    #   链条一: 跨图 5→612 BFS 直选 5→11(付费 3银, dest 156)→612; 余额不足的号点付费传送
    #     被服务端 1116 拒, 但跳转NPC 流程已"乐观改图 11" → 走到 11 的跳转点发 dest 83 →
    #     服务端按(真实在 5)拒 → 回滚 11→5 后"重走跳转点"仍在源图上走向目标图坐标 →
    #     __do_walk 等换图推送 12 次 → TASK_STUCK 死循环。
    #   链条二: NPC 13520(交付对象, 图11)在机器人当前 quest.chain 的 npcs 表查不到 →
    #     "无坐标盲点"→ CLICK 重试 ×4 → STUCK_CLICK(3001 首轮会话)。
    # 修复面: 免费优先规划(prefer_free 两阶段) / 回滚后换路重规划(__replan_after_bad_hop) /
    #   __do_walk 双保险 / 全局导航链缓存坐标兜底(P4)。
    check("D0: __find_dijkstra_route 支持 _free_only 免费优先参数",
          "_ignore_blacklist=False, _free_only=None):" in qe)
    # 2026-09-24 更新锚点: 入口重构为 _pf 变量(同语义) + newbie_full 默认免费优先
    #   (见 tools/newbie_shop_conflict_selftest.py Q1; 付费边放开路径不变)。
    check("D1: 免费优先两阶段入口保留(不通再放开付费; 含 newbie_full 默认)",
          '_pf = bool(getattr(quest, "prefer_free", False))' in qe
          and 'if _pf and not _ignore_blacklist:' in qe
          and "_ignore_blacklist=False, _free_only=True)" in qe)
    check("D2: 免费阶段跳过付费边(cost_money>0)",
          'if _free_only and int(e.get("cost_money") or 0) > 0:' in qe)
    check("D3: 忽略黑名单再找时保持免费阶段(_free_only=_free_only)",
          "_ignore_blacklist=True,\n\t\t\t\t_free_only=_free_only)" in qe)
    check("D4: __teleport_click 全局导航链缓存坐标兜底(g_nav_chain, 只增不减)",
          'g_nav_chain.get("npcs")' in qe
          and "NPC %s 静态表缺失, 用全局导航链缓存坐标" in qe)
    check("D5: 跳转NPC主动完成这跳时记 last_hop_done(供回滚定位元凶跳)",
          "quest.last_hop_done = _done" in qe)
    check("D6: 被拒+回滚后图不匹配 → 拉黑可疑跳+换路重规划(__replan_after_bad_hop)",
          "def __replan_after_bad_hop(robot_object, quest, dest, note):" in qe
          and 'if int(hop.get("from_map", 0) or 0) != int(robot_object.m_mapid or 0):' in qe
          and "__replan_after_bad_hop(robot_object, quest, data[0], _note)" in qe)
    check("D7: __do_walk 等换图双保险(已拒 retry>0 且图不匹配 → 不再空转 12 次报 TASK_STUCK)",
          'if int(hop.get("retry", 0) or 0) > 0:' in qe
          and '"本跳已被拒 retry=%d"' in qe)
    check("D8: __reset_quest 重置 prefer_free(切链/重置不残留)",
          "quest.prefer_free = False" in qe)

    # ---- D9: 行为测试 —— 提取 __find_dijkstra_route(免费优先) ----
    _dn = __import__("types")
    fr = None
    try:
        _ns2 = {"__hop_key": lambda fm, e: (int(fm or 0), e.get("destination_index")),
                "time": __import__("time"), "g_chain_grid_cache": {}, "g_chain_dijkstra_cache": {}}
        _frag2 = _extract_func(qe, "__find_dijkstra_route")
        check("D9: 提取 __find_dijkstra_route(免费优先版)", _frag2 is not None)
        if _frag2:
            exec(_frag2, _ns2)
            fr = _ns2.get("__find_dijkstra_route")
    except Exception as _e9:  # noqa
        check("D9: 提取 __find_dijkstra_route(免费优先版)", False, str(_e9))
    if fr is not None:
        # 表: 5→11(付费300, dest156) 与 5→9(免费, dest155) → 9→10(dest9) → 10→11(dest12)
        #     → 11→612(dest83) —— 对应生产真实拓扑
        FREE_TB = {
            "5": [{"kind": "npc_jumper", "destination_index": 156, "target_map": 11,
                   "cost_money": 300, "npc_index": 13003, "x": 1792, "y": 351},
                  {"kind": "npc_jumper", "destination_index": 155, "target_map": 9,
                   "cost_money": 0, "npc_index": 13003, "x": 1792, "y": 351}],
            "9": [{"kind": "map_skip", "destination_index": 9, "target_map": 10, "x": 95, "y": 842}],
            "10": [{"kind": "map_skip", "destination_index": 12, "target_map": 11, "x": 347, "y": 315}],
            "11": [{"kind": "map_skip", "destination_index": 83, "target_map": 612, "x": 3900, "y": 649}],
        }
        _q_np = _dn.SimpleNamespace(chain={"dijkstra": dict(FREE_TB)}, hop_black={})
        _r1 = fr(_q_np, 5, 612)
        check("D9a: 默认(不免费优先)最短跳: 5→11(付费)→612 两跳",
              _r1 is not None and len(_r1) == 2 and _r1[0].get("destination_index") == 156, _r1)
        _q_pf = _dn.SimpleNamespace(chain={"dijkstra": dict(FREE_TB)}, hop_black={}, prefer_free=True)
        _r2 = fr(_q_pf, 5, 612)
        check("D9b: prefer_free → 全免费路线 5→9→10→11→612(4 跳, 全 cost 0)",
              _r2 is not None and len(_r2) == 4
              and [h.get("destination_index") for h in _r2] == [155, 9, 12, 83]
              and all(int(h.get("cost_money") or 0) == 0 for h in _r2), _r2)
        PAID_ONLY = {"5": [{"kind": "npc_jumper", "destination_index": 999, "target_map": 11,
                            "cost_money": 300, "npc_index": 13003, "x": 1, "y": 2}]}
        _q_po = _dn.SimpleNamespace(chain={"dijkstra": dict(PAID_ONLY)}, hop_black={}, prefer_free=True)
        _r3 = fr(_q_po, 5, 11)
        check("D9c: prefer_free 无免费路线 → 回退付费边(不返回 None)",
              _r3 is not None and _r3[0].get("destination_index") == 999, _r3)

    # ---- D10: 行为测试 —— 提取 __replan_after_bad_hop(回滚换路统一出口) ----
    _ev2 = []
    rp = None
    _frag3 = _extract_func(qe, "__replan_after_bad_hop")
    check("D10: 提取 __replan_after_bad_hop", _frag3 is not None)
    if _frag3:
        try:
            _ns3 = {"__emit": lambda ro, ev: _ev2.append(ev.get("msg") or ""),
                    "__report_stuck": lambda ro, q, why: _ev2.append("STUCK:" + why),
                    "__teleport_click": lambda ro, q, *a, **kw: _ev2.append("REPLAN:%s:%s" % (a, kw))}
            exec(_frag3, _ns3)
            rp = _ns3.get("__replan_after_bad_hop")
        except Exception as _e10:  # noqa
            check("D10: exec __replan_after_bad_hop", False, str(_e10))
    if rp is not None:
        class _RO3(object):
            pass
        _ro3 = _RO3()
        _q3 = _dn.SimpleNamespace(
            dijkstra_plan={"npc_id": 13021, "npc_index": 13021, "click_type": 0,
                           "delay_ms": 500, "no_npc_jumper": False, "pos": [612, 1150, 831]},
            prefer_free=False, dijkstra_route=[{"destination_index": 83}],
            dijkstra_waiting=True, dijkstra_jumper={"npc_id": 13003}, dijkstra_final={"x": 1})
        _ev2[:] = []
        _rok = rp(_ro3, _q3, 83, "回滚后图不匹配; 拉黑跳转点 156(地图 5)")
        check("D10a: 换路: 返回 True + 置 prefer_free + 清跨图状态",
              _rok is True and _q3.prefer_free is True and _q3.dijkstra_route == []
              and _q3.dijkstra_waiting is False and _q3.dijkstra_jumper is None
              and _q3.dijkstra_final is None, (_rok, dict(_q3.__dict__)))
        check("D10b: 换路: 有 plan → 调 __teleport_click 重规划(带 +300ms 退避)",
              any(str(m).startswith("REPLAN") for m in _ev2) and any("+300" in str(m) or "800" in str(m) for m in _ev2),
              _ev2)
        _q4 = _dn.SimpleNamespace(dijkstra_plan=None, prefer_free=False, dijkstra_route=[],
                                  dijkstra_waiting=False, dijkstra_jumper=None, dijkstra_final=None)
        _ev2[:] = []
        _rok2 = rp(_ro3, _q4, 83, "no plan")
        check("D10c: 换路: 无 plan → 停链报错(STUCK)不静默",
              _rok2 is True and any(str(m).startswith("STUCK:") for m in _ev2), _ev2)

    # ---- D11: 真实链数据回归(可选; 找不到数据文件跳过) ----
    _chain_fp = None
    for _cand in (
        os.path.join(script_dir, "..", "..", "..", "..", "data", "chains", "newbie_full.json"),
        os.path.join(script_dir, "..", "..", "..", "data", "chains", "newbie_full.json"),
    ):
        if os.path.exists(_cand):
            _chain_fp = _cand
            break
    if _chain_fp and fr is not None:
        try:
            import json as _json9
            _cd = _json9.load(open(_chain_fp, encoding="utf-8"))
            _q_r = _dn.SimpleNamespace(chain={"dijkstra": _cd.get("dijkstra", {})},
                                       hop_black={}, prefer_free=True)
            _rr = fr(_q_r, 5, 612)
            check("D11a: 真实链数据: prefer_free 5→612 全免费(末跳 dest 83)",
                  _rr is not None and all(int(h.get("cost_money") or 0) == 0 for h in _rr)
                  and _rr[-1].get("destination_index") == 83,
                  [(h.get("destination_index"), h.get("cost_money")) for h in (_rr or [])])
            _q_r2 = _dn.SimpleNamespace(chain={"dijkstra": _cd.get("dijkstra", {})},
                                        hop_black={}, prefer_free=False)
            _rr2 = fr(_q_r2, 5, 612)
            check("D11b: 真实链数据: 默认仍为最短跳(付费 156 在前, 行为不回退)",
                  _rr2 is not None and _rr2[0].get("destination_index") == 156,
                  [(h.get("destination_index"), h.get("cost_money")) for h in (_rr2 or [])])
        except Exception as _e11:  # noqa
            check("D11: 真实链数据回归", False, str(_e11))
    else:
        print("[SKIP] D11: 未找到 data/chains/newbie_full.json(或 D9 提取失败), 跳过真实数据回归")

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
