# -*- coding: utf-8 -*-
"""抓鬼"巡逻/等刷鬼选点"图内死点修复自检 —— 2026-09-23

背景（现场取证）:
  random_walk 图内死点修复（连通试算 + 吸附）上线后，残余刷屏大头在**抓鬼链路**:
  daily_ghost.__patrol 巡逻选点只校验"目标格非阻挡"、不校验与当前位置连通 →
  选到隔断区的点（或**别的图的坐标**）→ 每轮刷
  `地图 X 无可行路径 (x,y)→(x,y), 取消行走`，号站着不动、鬼刷不出来。

  实测 robot0001034（2026-09-23 16:26~16:31）:
    任务目标 = [26, 3776, 1776]；钟馗对话把号拉回 map24 (1672,1112) 后，巡逻仍按
    g.kill_area（map26 的 3776,1776）当基准 → 选出 (3999,2085)/(3673,1584)/(3376,1938)
    全是 map26 坐标 → map24 网格里不可达 → 每轮 3 条刷屏（8 个号 ≈44 条/270s）。

修复（两处，均复用 random_walk 的单一实现，不复制两份）:
  ① random_walk 新增公共入口:
     - snap_walkable(grid, x, y)                            位置吸附（见 __snap_walkable）
     - walk_point_connected(quest, mapid, from_pos, to_pos) 连通试算（同 __build_path/
       同 finder 缓存）；__pick_walk_point 内的 __connected 改为委托它（行为不变）。
  ② daily_ghost.__patrol:
     - 基准点: g.kill_area[0] != 当前图 → 退回"当前位置"作基准（别的图坐标必刷屏）；
     - 当前位置在网格外/阻挡格 → snap_walkable 吸附（**只作试算起点，不回写 m_pose**）；
     - 候选点试算连通（≤PATROL_PICK_TRIES 次不通换点）；试算不可用/异常 → 退回旧行为
       （直接用候选点，不把号钉死）；全不通 → 本轮不安排走动（warn + 等下一轮）。

用法:
  python tools/daily_ghost_deadpoint_selftest.py <script 目录 或 daily_ghost.py 路径>
"""
import os
import re
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


# ---------------------------------------------------------------- fixture 网格
# 20x12 格(320x192 px):
#   x=5 竖墙(y=2..9) —— 左右只能从 y=0/1/10/11 绕行;
#   右下封闭口袋(x=15..18, y=3..8, 口字环全阻挡) —— 与主区**互不连通**。
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
WALL_POSE = (5 * 16 + 8, 5 * 16 + 8)  # 竖墙格(5,5) = (88,88) 阻挡
SNAP_POSE = (4 * 16 + 8, 5 * 16 + 8)  # 吸附期望: 最近可走格(4,5) = (72,88)
POCKET_POSE = (17 * 16 + 8, 5 * 16 + 8)  # 口袋中心 = (280,88) 与主区不连通
KA_SAME_MAP = (FIX_MAP, 150, 88)      # 同图 kill_area 基准(格 9,5 可走)
KA_MAP_SAME_MAP = 150
KA_OTHER_MAP = (26, 3700, 1900)       # 别的图坐标(现场: 任务目标在 map26)


class _ScriptRng(object):
    """自检用随机源: randint 按脚本序列返回（(dx,dy) 成对给，用尽后重复最后一个）。"""

    def __init__(self, seq):
        self.seq = list(seq)
        self.i = 0

    def randint(self, a, b):
        if self.i < len(self.seq):
            v = self.seq[self.i]
            self.i += 1
            return v
        return self.seq[-1] if self.seq else a


def main():
    if len(sys.argv) < 2:
        print("用法: python tools/daily_ghost_deadpoint_selftest.py <script 目录 或 daily_ghost.py 路径>")
        return 2
    script_dir = _resolve(sys.argv[1])
    dh_path = os.path.join(script_dir, "daily_ghost.py")
    rw_path = os.path.join(script_dir, "random_walk.py")
    for p in (dh_path, rw_path):
        if not os.path.exists(p):
            print("[FAIL] 找不到 %s" % p)
            return 2
    dh = open(dh_path, encoding="utf-8", errors="replace").read()
    rw = open(rw_path, encoding="utf-8", errors="replace").read()

    results = []

    def check(name, cond, detail=""):
        results.append((name, bool(cond), detail))

    # ============================================================ A. 源码形状
    check("random_walk 定义公共 snap_walkable(吸附入口)", "def snap_walkable(" in rw)
    check("random_walk 定义公共 walk_point_connected(连通试算入口)",
          "def walk_point_connected(" in rw)
    check("random_walk.snap_walkable 委托 __snap_walkable(单一实现, 不重写)",
          "return __snap_walkable(grid, x, y, max_radius=max_radius)" in rw)
    check("random_walk.walk_point_connected 走 quest_engine.__build_path(与真走同寻路)",
          "return quest_engine.__build_path(quest, mapid," in rw)
    check("random_walk.__pick_walk_point.__connected 委托公共入口(单一实现)",
          "return walk_point_connected(quest, w.mapid, from_pos, (px, py))" in rw)
    check("daily_ghost 定义 PATROL_PICK_TRIES = 3", "PATROL_PICK_TRIES = 3" in dh)
    check("daily_ghost 定义 __patrol_point_ok(试算入口封装)",
          "def __patrol_point_ok(" in dh)
    check("daily_ghost.__patrol_point_ok 复用 random_walk.walk_point_connected",
          'getattr(_rw, "walk_point_connected", None)' in dh
          and "return bool(_fn(quest, robot_object.m_mapid, from_pos, to_pos))" in dh)
    check("daily_ghost.__patrol 试算不可用时提示一次(不静默退化)",
          "_PATROL_CONN_UNAVAIL_WARNED" in dh)
    check("daily_ghost.__patrol 用 snap_walkable 吸附(只作试算起点)",
          'getattr(_rw, "snap_walkable", None)' in dh)
    check("daily_ghost.__patrol 吸附带 ROAM_SNAP_MAX_DIST_PX 上限(超限本轮不巡逻/待命)",
          "ROAM_SNAP_MAX_DIST_PX" in dh and "本轮不巡逻(待命)" in dh)
    check("daily_ghost.__patrol kill_area 图≠当前图 → 基准退回当前位置",
          "if g.kill_area and int(g.kill_area[0]) == int(robot_object.m_mapid):" in dh)
    check("daily_ghost.__patrol 候选点调 __patrol_point_ok(试算)",
          "__patrol_point_ok(quest, robot_object, _pose, (_px, _py))" in dh)
    check("daily_ghost.__patrol 全不通 → 不安排走动 + warn",
          "巡逻选点 %d 次均与当前位置不连通, 本轮不巡逻" in dh)
    check("daily_ghost 未复制寻路实现(无 quest_engine.__build_path 调用)",
          "quest_engine.__build_path(" not in dh)

    # ============================================================ 导入真实 robot_path
    try:
        sys.path.insert(0, script_dir)
        import robot_path as _rp
        check("可导入 robot_path(真实寻路)", True)
    except Exception as _e:  # noqa
        _rp = None
        check("可导入 robot_path(真实寻路)", False, str(_e))

    # ============================================================ B. 执行 walk_point_connected
    bp_calls = []

    if _rp is not None:
        _finder = _rp.GridPathFinder(_rp.MapGrid(FIX_MAP, FIX_GRID))
        qe_rw = types.ModuleType("quest_engine")

        def _stub_build_path(quest, mapid, fx, fy, tx, ty):
            # 与生产 __build_path 同实现(复用 robot_path.GridPathFinder)
            bp_calls.append((mapid, fx, fy, tx, ty))
            if int(mapid) != FIX_MAP:
                return None
            return _finder.find_path(fx, fy, tx, ty)

        qe_rw.__build_path = _stub_build_path
        ns_rw = {"quest_engine": qe_rw, "robot_path": _rp}
        for _m in re.finditer(r"^(ROAM_\w+)\s*=\s*(\d+)", rw, re.M):
            ns_rw[_m.group(1)] = int(_m.group(2))
        for _name in ("__snap_walkable", "snap_walkable", "walk_point_connected"):
            frag = _extract_func(rw, _name)
            check("提取 random_walk.%s" % _name, frag is not None)
            if frag:
                try:
                    exec(frag, ns_rw)
                except Exception as e:  # noqa
                    check("exec random_walk.%s" % _name, False, str(e))
        wpc = ns_rw.get("walk_point_connected")
        snap = ns_rw.get("snap_walkable")

        if wpc is not None:
            check("试算: 主区内两点连通 → True", wpc(None, FIX_MAP, MAIN_POSE, (160, 98)) is True)
            check("试算: 主区 → 封闭口袋(隔断) → False",
                  wpc(None, FIX_MAP, MAIN_POSE, POCKET_POSE) is False)
            check("试算: 口袋内部两点(同区) → True",
                  wpc(None, FIX_MAP, POCKET_POSE, (16 * 16 + 8, 6 * 16 + 8)) is True)
            check("试算: from_pos 为空(选落地点) → True(旧行为, 不拦)",
                  wpc(None, FIX_MAP, None, POCKET_POSE) is True)
            check("试算: to_pos 为空 → True(旧行为, 不拦)",
                  wpc(None, FIX_MAP, MAIN_POSE, None) is True)
            check("试算: 目标图无网格 → False(与真走同判据: 真走也 NO_LEGAL_ROUTE, 不安排刷屏)",
                  wpc(None, 26, MAIN_POSE, (160, 98)) is False)

            def _raise_bp(quest, mapid, fx, fy, tx, ty):
                raise RuntimeError("grid broken")

            _orig = qe_rw.__build_path
            qe_rw.__build_path = _raise_bp
            check("试算: 寻路异常 → True(退回旧行为, 不把号钉死)",
                  wpc(None, FIX_MAP, MAIN_POSE, POCKET_POSE) is True)
            qe_rw.__build_path = _orig

        if snap is not None:
            r = snap(_rp.MapGrid(FIX_MAP, FIX_GRID), WALL_POSE[0], WALL_POSE[1])
            check("吸附: 阻挡格(5,5) → 最近可走格(4,5)=(72,88)", r == SNAP_POSE, r)

    # ============================================================ C. 执行 __patrol 端到端
    logs = []
    sched = []
    qe_dh = types.ModuleType("quest_engine")

    def _stub_chain_grid(quest, mapid):
        return FIX_GRID if int(mapid) == FIX_MAP else None

    qe_dh.__chain_grid_for = _stub_chain_grid
    qe_dh.__schedule = lambda quest, item: sched.append(item)
    qe_dh.__human_delay = lambda ms: 0

    ns_dh = {
        "quest_engine": qe_dh,
        "random_path": None,
        "__log": lambda ro, lvl, msg: logs.append((lvl, msg)),
    }
    for _m in re.finditer(r"^(PATROL_\w+)\s*=\s*(\d+)", dh, re.M):
        ns_dh[_m.group(1)] = int(_m.group(2))
    ns_dh["_PATROL_CONN_UNAVAIL_WARNED"] = [False]
    frag = _extract_func(dh, "__patrol_point_ok")
    check("提取 daily_ghost.__patrol_point_ok", frag is not None)
    if frag:
        try:
            exec(frag, ns_dh)
        except Exception as e:  # noqa
            check("exec daily_ghost.__patrol_point_ok", False, str(e))
    frag = _extract_func(dh, "__patrol")
    check("提取 daily_ghost.__patrol", frag is not None)
    if frag:
        try:
            exec(frag, ns_dh)
        except Exception as e:  # noqa
            check("exec daily_ghost.__patrol", False, str(e))
    patrol = ns_dh.get("__patrol")
    patrol_ok = ns_dh.get("__patrol_point_ok")

    if patrol is not None and patrol_ok is not None and _rp is not None:
        _finder2 = _rp.GridPathFinder(_rp.MapGrid(FIX_MAP, FIX_GRID))
        rw_stub = types.ModuleType("random_walk")
        wpc_calls = []

        def _rw_wpc(quest, mapid, from_pos, to_pos):
            wpc_calls.append((from_pos, to_pos))
            if int(mapid) != FIX_MAP:
                return True
            return _finder2.find_path(from_pos[0], from_pos[1], to_pos[0], to_pos[1]) is not None

        rw_stub.walk_point_connected = _rw_wpc
        rw_stub.snap_walkable = snap
        rw_stub.ROAM_SNAP_MAX_DIST_PX = 1024   # 与 random_walk 同源口径(吸附距离上限)

        real_random = sys.modules.get("random")
        real_rw = sys.modules.get("random_walk")
        real_rp = sys.modules.get("robot_path")
        sys.modules["robot_path"] = _rp
        sys.modules["random_walk"] = rw_stub

        def _run_patrol(pose, kill_area, seq, with_wpc=True):
            """跑一轮 __patrol, 返回 (调度列表, 日志列表)。"""
            del sched[:]
            del logs[:]
            if with_wpc:
                sys.modules["random_walk"] = rw_stub
            else:
                _no_fn = types.ModuleType("random_walk")
                sys.modules["random_walk"] = _no_fn
            mod = types.ModuleType("random")
            mod.Random = lambda: _ScriptRng(seq)
            sys.modules["random"] = mod
            ro = types.SimpleNamespace(m_mapid=FIX_MAP, m_pose=[pose[0], pose[1]])
            quest = types.SimpleNamespace(pending="x", walk_target="y")
            g = types.SimpleNamespace(kill_area=list(kill_area) if kill_area else None)
            patrol(ro, quest, g, 1000)
            return ro, list(sched), list(logs)

        # ---- C1. 同图 kill_area: 基准=区域中心, 点连通 → 调度一次
        del wpc_calls[:]
        ro, sc, lg = _run_patrol(MAIN_POSE, KA_SAME_MAP,
                                 [10, 10, 20, 20, 30, 30])
        ok_target = bool(sc) and len(sc) == 1 and sc[0].get("type") == "walk"
        tx, ty = (sc[0]["data"]["to_x"], sc[0]["data"]["to_y"]) if ok_target else (0, 0)
        check("同图 kill_area: 调度一次 walk", ok_target, sc)
        check("同图 kill_area: 目标在 kill_area±400 内(旧选点口径保留)",
              ok_target and abs(tx - KA_MAP_SAME_MAP) <= 400 and abs(ty - 88) <= 400,
              (tx, ty))
        check("同图 kill_area: 试算起点=号当前位置(不是基准点)",
              bool(wpc_calls) and tuple(wpc_calls[0][0]) == MAIN_POSE, wpc_calls[:1])
        check("同图 kill_area: 目标与当前位置连通(试算生效)",
              ok_target and _finder2.find_path(MAIN_POSE[0], MAIN_POSE[1], tx, ty) is not None,
              (tx, ty))

        # ---- C2. kill_area 是**别的图**(现场根因): 基准退回当前位置
        ro, sc, lg = _run_patrol(MAIN_POSE, KA_OTHER_MAP, [10, 10])
        ok_target = bool(sc) and len(sc) == 1
        tx, ty = (sc[0]["data"]["to_x"], sc[0]["data"]["to_y"]) if ok_target else (0, 0)
        check("别的图 kill_area: 仍调度(退回当前位置基准, 能巡逻)",
              ok_target, sc)
        check("别的图 kill_area: 目标在**当前位置**±400 内(不用别的图坐标)",
              ok_target and abs(tx - MAIN_POSE[0]) <= 400 and abs(ty - MAIN_POSE[1]) <= 400
              and (abs(tx - KA_OTHER_MAP[1]) > 400 or abs(ty - KA_OTHER_MAP[2]) > 400),
              (tx, ty))
        check("别的图 kill_area: 目标与当前位置连通",
              ok_target and _finder2.find_path(MAIN_POSE[0], MAIN_POSE[1], tx, ty) is not None,
              (tx, ty))

        # ---- C3. 候选点全在封闭口袋(不连通) → 3 次试算后不安排走动 + warn
        ro, sc, lg = _run_patrol(MAIN_POSE, None,
                                 [POCKET_POSE[0] - MAIN_POSE[0], POCKET_POSE[1] - MAIN_POSE[1]] * 3)
        check("全不连通: 不安排走动(不再刷'无可行路径')", not sc, sc)
        check("全不连通: 有 warn 说明(本轮不巡逻)",
              any("本轮不巡逻" in m for _lv, m in lg), lg)

        # ---- C4. 试算不可用(旧版 random_walk 无入口) → 退回旧行为(仍调度) + 提示一次
        ro, sc, lg = _run_patrol(MAIN_POSE, None,
                                 [POCKET_POSE[0] - MAIN_POSE[0], POCKET_POSE[1] - MAIN_POSE[1]],
                                 with_wpc=False)
        check("试算不可用: 退回旧行为(直接调度候选点, 不把号钉死)",
              len(sc) == 1 and sc[0]["data"]["to_x"] == POCKET_POSE[0], sc)
        check("试算不可用: 有一次性提示(不静默退化)",
              any("walk_point_connected" in m for _lv, m in lg), lg)
        ro, sc, lg = _run_patrol(MAIN_POSE, None, [10, 10], with_wpc=False)
        check("试算不可用: 提示只报一次",
              not any("walk_point_connected" in m for _lv, m in lg), lg)

        # ---- C5. 当前位置落阻挡格 → 吸附(只作试算起点, 不回写 m_pose)
        del wpc_calls[:]
        ro, sc, lg = _run_patrol(WALL_POSE, None, [10, 10])
        check("位置阻挡: m_pose 不被回写(巡逻不引入瞬移)",
              list(ro.m_pose) == [WALL_POSE[0], WALL_POSE[1]], ro.m_pose)
        check("位置阻挡: 仍调度(吸附后试算通过)", len(sc) == 1, sc)
        check("位置阻挡: 试算起点=吸附点(4,5)=(72,88), 不是原始阻挡坐标",
              bool(wpc_calls) and tuple(wpc_calls[-1][0]) == SNAP_POSE, wpc_calls[-1:])

        # ---- C6. 候选点先落在阻挡格 → 40 次重选跳过, 最终选到可走点
        ro, sc, lg = _run_patrol(MAIN_POSE, None, [48, 48, 120, 58])
        # 48,48 → (88,88) 竖墙格 → continue; 120,58 → (160,98) 可走 → 采用
        check("阻挡格重选: 跳过阻挡候选, 采用下一个可走点",
              len(sc) == 1 and sc[0]["data"]["to_x"] == 160 and sc[0]["data"]["to_y"] == 98, sc)

        # ---- C6b. 吸附距离超上限(ROAM_SNAP_MAX_DIST_PX) → 本轮不巡逻(待命)
        # 位置 (3000,3000): 网格外, 夹回后最近可走格 (312,184), 距离 ~3892px > 1024
        ro, sc, lg = _run_patrol((3000, 3000), None, [10, 10])
        check("吸附超限: 不安排走动(不采用远处吸附点当试算起点)", not sc, sc)
        check("吸附超限: warn 写明距离/上限 + 本轮不巡逻",
              any("超上限" in m and "本轮不巡逻" in m for _lv, m in lg), lg)
        # 上限内(304px < 1024px) → 照常采用吸附点做试算起点(证明上限只拦"过远")
        del wpc_calls[:]
        ro, sc, lg = _run_patrol((600, 100), None, [10, 10])
        check("吸附上限内: 仍采用吸附点(296,104)做试算起点(上限只拦过远)",
              bool(wpc_calls) and tuple(wpc_calls[-1][0]) == (296, 104), wpc_calls[-1:])

        # ---- C7. 网格缺失 → 旧行为(不试算, 直接调度)
        _orig_grid = qe_dh.__chain_grid_for
        qe_dh.__chain_grid_for = lambda quest, mapid: None
        ro, sc, lg = _run_patrol(MAIN_POSE, None, [10, 10])
        qe_dh.__chain_grid_for = _orig_grid
        check("网格缺失: 退回旧行为(直接调度候选点, 不卡号)",
              len(sc) == 1 and sc[0]["data"]["to_x"] == 50, sc)

        # 还原 sys.modules
        for _k, _v in (("random", real_random), ("random_walk", real_rw), ("robot_path", real_rp)):
            if _v is None:
                sys.modules.pop(_k, None)
            else:
                sys.modules[_k] = _v

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
