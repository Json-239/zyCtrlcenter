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

    # 结构: 连通筛选必须是"就近可走格校正"的**独立一层**(与 >8 门槛同级) —— 否则
    #   "目标格可走但不可达"(现场 map45 (1074,907)→(152,246)、map24 钟馗
    #   (4712,2776)→(1672,1080)) 这类连门槛都进不去, 筛选形同虚设。
    def _indent(s):
        return len(s) - len(s.lstrip("\t"))

    _abs_line = None
    _guard_line = None
    for _ln in qe.splitlines():
        if _abs_line is None and "if abs(_nx - to_x) + abs(_ny - to_y) > 8:" in _ln:
            _abs_line = _ln
        if _guard_line is None and _ln.strip().startswith("if path2 is None"):
            _guard_line = _ln
    check("连通筛选是独立一层(与 >8 门槛同级, 目标格可走也能进)",
          _abs_line is not None and _guard_line is not None
          and _indent(_abs_line) == _indent(_guard_line),
          (_abs_line, _guard_line))
    check("连通筛选只在非任务链启用(quest.active=False; 游荡/抓鬼/商店)",
          'if path2 is None and not bool(getattr(quest, "active", False)):' in qe)
    check("链任务失败语义保留(active=True → 原 NO_LEGAL_ROUTE 停链报错)",
          "if quest.active:" in qe and "quest.set_state(quest_state.ST_ERROR)" in qe)
    check("path2 进入门槛前先置 None(门槛不进入时不 NameError)",
          "\t\t\t\t\tpath2 = None" in qe)

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
