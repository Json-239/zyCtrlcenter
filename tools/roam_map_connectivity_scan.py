# -*- coding: utf-8 -*-
"""游荡图"进得去出不来"诊断（只读）—— 2026-09-23

背景（现场采样）：map45 里困着 23 个在线号 —— 它们全落在网格的一个 2729 格连通分量里，
而**出图跳转点 (152,246) 在另一个 506 格分量**里 → 网格层面走不到出口 → 每次跨图游荡
补发都"无可行路径"刷几条后放弃就地游荡；号只进不出，逐渐堆积。
根因类别：`map_grids`(阻挡网格) 的连通域 与 `dijkstra` 跳转点坐标不一致。
为什么抽签/roampool 拦不住：它们只做**图级**可达性校验，查不到"走到第一个跳转点"这一步。

本脚本做三件事（全部只读）：
  1) 对每张图做连通域分解（与 robot_path.GridPathFinder 同规则: 8 邻接 + 禁止斜穿墙角）；
  2) 把该图所有跳转点(map_skip / npc_jumper 的出边 x,y)映射到连通域，标出"不在主连通域"的；
  3) 可选(--online)：拉中控 /api/status，列出**当前在线号**落在非主连通域的名单
     （即"此刻困在某张图里出不来"的号）。

用法：
  python tools/roam_map_connectivity_scan.py [--chain data/chains/newbie_full.json]
        [--config 脚本目录/config.py]  # 读 robot_roam_world_maps(世界游荡白名单)
        [--all]                        # 不按白名单, 扫全部有网格的图
        [--online] [--api http://127.0.0.1:28082] [--top 20]
退出码: 0=无问题; 1=存在"跳转点非主域"的图（便于自动化巡检）；2=参数/数据问题。
"""
import argparse
import collections
import json
import os
import re
import sys

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass


def load_grid_module(script_dir):
    sys.path.insert(0, script_dir)
    import robot_path
    return robot_path


def components(grid):
    """连通域分解: 返回 (comp_id dict, sizes list)。规则同 GridPathFinder._astar。"""
    comp_id = {}
    sizes = []
    for gy in range(grid.h):
        for gx in range(grid.w):
            if grid.blocked(gx, gy) or (gx, gy) in comp_id:
                continue
            cid = len(sizes)
            n = 0
            dq = collections.deque([(gx, gy)])
            comp_id[(gx, gy)] = cid
            while dq:
                cx, cy = dq.popleft()
                n += 1
                for dx in (-1, 0, 1):
                    for dy in (-1, 0, 1):
                        if dx == 0 and dy == 0:
                            continue
                        nx, ny = cx + dx, cy + dy
                        if (nx, ny) in comp_id or grid.blocked(nx, ny):
                            continue
                        if dx and dy and (grid.blocked(cx + dx, cy) or grid.blocked(cx, cy + dy)):
                            continue
                        comp_id[(nx, ny)] = cid
                        dq.append((nx, ny))
            sizes.append(n)
    return comp_id, sizes


def world_maps_from_config(path):
    try:
        src = open(path, encoding="utf-8", errors="replace").read()
    except Exception:
        return []
    m = re.search(r"robot_roam_world_maps\s*=\s*\[([^\]]*)\]", src)
    if not m:
        return []
    return [int(x) for x in re.findall(r"\d+", m.group(1))]


def main():
    here = os.path.dirname(os.path.abspath(__file__))
    ap = argparse.ArgumentParser()
    ap.add_argument("--chain", default=os.path.join("data", "chains", "newbie_full.json"))
    ap.add_argument("--script-dir", default=os.path.join(
        here, "..", "deploy", "zones", "prod-240-2300", "script"))
    ap.add_argument("--config", default="")
    ap.add_argument("--all", action="store_true")
    ap.add_argument("--online", action="store_true")
    ap.add_argument("--api", default="http://127.0.0.1:28082")
    ap.add_argument("--top", type=int, default=20)
    args = ap.parse_args()

    if not os.path.exists(args.chain):
        print("[FAIL] 找不到链数据: %s" % args.chain)
        return 2
    data = json.load(open(args.chain, encoding="utf-8", errors="replace"))
    grids = data.get("map_grids") or {}
    dijk = data.get("dijkstra") or {}
    if not grids:
        print("[FAIL] 链数据没有 map_grids")
        return 2
    try:
        rp = load_grid_module(os.path.abspath(args.script_dir))
    except Exception as e:  # noqa
        print("[FAIL] 无法导入 robot_path(%s): %s" % (args.script_dir, e))
        return 2

    cfg_path = args.config or os.path.join(args.script_dir, "config.py")
    world = [] if args.all else world_maps_from_config(cfg_path)
    maps = [int(k) for k in grids.keys() if str(k).isdigit()]
    if world:
        maps = [m for m in maps if m in world]
    maps.sort()
    print("=== 游荡图连通域/跳转点诊断(%s, %d 张图) ===" % (
        "全部网格图" if args.all else "世界白名单", len(maps)))

    problems = []
    comp_cache = {}
    for mid in maps:
        gd = grids.get(str(mid))
        if not gd:
            continue
        g = rp.MapGrid(mid, gd)
        comp_id, sizes = components(g)
        comp_cache[mid] = (g, comp_id, sizes)
        main = sizes.index(max(sizes)) if sizes else -1
        hops = []
        for e in (dijk.get(str(mid)) or []):
            try:
                x, y = int(e.get("x") or 0), int(e.get("y") or 0)
            except Exception:
                continue
            if x <= 0 and y <= 0:
                continue
            cell = g.to_grid(x, y)
            cid = comp_id.get(cell)
            if cid is None:
                # 跳转点在阻挡格: 生产走 __do_walk 的"就近可走格"校正(nearest_walkable,
                # 半径 8) —— 用校正后的格判域, 才是"号能不能走到/触发"的真实口径。
                near = rp.nearest_walkable(g, x, y, max_radius=8)
                if near is not None:
                    cid = comp_id.get(near)
                    cell = near
            hops.append((e.get("target_map"), cid, x, y,
                         (cell[0] * 16 + 8, cell[1] * 16 + 8) if cell else None))
        off = [h for h in hops if h[1] is not None and h[1] != main]
        none_hops = [h for h in hops if h[1] is None]
        if off or none_hops:
            problems.append((mid, len(sizes), max(sizes) if sizes else 0, hops, off, none_hops))
    if not problems:
        print("结论: 未发现'跳转点不在主连通域'的图 ✔")
    else:
        print("结论: %d 张图存在'跳转点不在主连通域/无就近可走格'(号可能进得去出不来):" % len(problems))
        for mid, ncomp, msize, hops, off, none_hops in problems:
            print("  map%-4s 分量数=%-3d 主域=%-5d 跳转点=%d 非主域=%d 无就近可走格=%d" % (
                mid, ncomp, msize, len(hops), len(off), len(none_hops)))
            for tgt, cid, x, y, eff in off[:6]:
                csize = comp_cache[mid][2][cid] if cid is not None else 0
                print("      → 目标图 %-5s 跳转点(%d,%d) 在 域%s(%d 格, 非主域)" % (
                    tgt, x, y, cid, csize))
            for tgt, cid, x, y, eff in none_hops[:3]:
                print("      → 目标图 %-5s 跳转点(%d,%d) 半径 8 格内无可走格(校正也到不了)" % (
                    tgt, x, y))

    if args.online:
        try:
            import urllib.request
            st = json.load(urllib.request.urlopen(args.api + "/api/status", timeout=10))
        except Exception as e:  # noqa
            print("[SKIP] 拉取 /api/status 失败: %s" % e)
            st = None
        if st is not None:
            # "困住"的准确口径: 该号所在连通域**没有任何出口跳转点** → 网格层面出不去。
            # (若所在域有出口, 只是"某条路线选的跳转点不可达"——属于路线选择问题,
            #  本工具不判, 避免把正常号误报成困住。)
            exit_comps = {}
            for mid, (g, comp_id, sizes) in comp_cache.items():
                s = set()
                for e in (dijk.get(str(mid)) or []):
                    try:
                        x, y = int(e.get("x") or 0), int(e.get("y") or 0)
                    except Exception:
                        continue
                    if x <= 0 and y <= 0:
                        continue
                    cell = g.to_grid(x, y)
                    if comp_id.get(cell) is None:
                        near = rp.nearest_walkable(g, x, y, max_radius=8)
                        if near is None:
                            continue
                        cell = near
                    cid = comp_id.get(cell)
                    if cid is not None:
                        s.add(cid)
                exit_comps[mid] = s
            stuck = collections.defaultdict(list)
            for r in st.get("robots") or []:
                if not r.get("online"):
                    continue
                mid = int(r.get("mapid") or 0)
                pos = r.get("pos") or []
                if len(pos) < 2 or mid not in comp_cache:
                    continue
                g, comp_id, sizes = comp_cache[mid]
                if not sizes:
                    continue
                cid = comp_id.get(g.to_grid(pos[0], pos[1]))
                if cid is None or cid in exit_comps.get(mid, set()):
                    continue
                w = r.get("walk") or {}
                stuck[mid].append((r.get("account"), pos, "roam" if w.get("enabled") else
                                   (r.get("state") or "-"), cid, sizes[cid]))
            print("\n在线号所在'无任何出口跳转点'连通域的分布(此刻真正出不去):")
            if not stuck:
                print("  (无)")
            for mid in sorted(stuck, key=lambda m: -len(stuck[m]))[:args.top]:
                lst = stuck[mid]
                print("  map%-4s %d 个号:" % (mid, len(lst)))
                for acc, pos, tag, cid, csize in lst[:8]:
                    print("      %-24s pos=%-14s %-10s 域%d(%d 格, 无出口)" % (acc, pos, tag, cid, csize))
                if len(lst) > 8:
                    print("      ...(还有 %d 个)" % (len(lst) - 8))

    print("\n说明: 只读诊断, 不改任何数据/配置; 数据修复后可用同一脚本复验。")
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
