# -*- coding: utf-8 -*-
"""map45（瑶池回廊）域拓扑 + 救援处方（只读）—— 2026-09-30

背景（见 docs/04-测试/分析-20260930-map45死域与ERROR处置.md §3/§7）：
  45 网格 7 个连通域；5 个"传送 NPC"（灵儿13338/琴儿13339/兰儿13340/梦儿13341/虹儿13342）
  与 5 个落脚点分布在 域2/域3/域5；跳转边(map_skip)落在 域2/域3/域5；**域6 无 NPC/无落脚/无出口**。
  被卡在 域2/域5 的号：最近 NPC 可达 → 对话传送（落脚主域）→ 可继续行程；
  被卡在 域6 的号：无任何可走边 → 只能服务端拉出。

本工具：
  1) 域拓扑表：每个域格数 / 域内 NPC / 域内落脚点 / 域内跳转边起点；
  2) 逃逸矩阵：每个"有 NPC 的域" → 每个 NPC 的选项（spot_alias）→ 落脚域 → 是否桥到"有出边的域";
  3) --online：拉 /api/status，对 45 上的号逐号给"救援处方"（走哪个 NPC、选哪个选项；
     域6=需服务端拉出），并对"号→NPC"做 A* 可达性实证。

用法:
  python tools/map45_rescue_plan.py [--script-dir <脚本目录>] [--chain <链 json>]
        [--online] [--api http://127.0.0.1:28082]
退出码: 0=所有在线号(或离线模式)都有处方; 1=存在无处方号(域6 类); 2=参数/数据问题。
只读：不改任何文件/配置。
"""
import argparse
import collections
import json
import os
import sys

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)

# 落脚点坐标（来源: 服务端 config/map_spot.csv 459-468 行区 JUMP_TO_YCHL01-05，2026-09-30 核对）
LANDING = {
    "跳到南天门入口": (2458, 1640),
    "跳到瑶池回廊中心": (1074, 903),
    "跳到凌霄殿入口": (374, 468),
    "跳到兜率宫入口": (1724, 339),
    "跳到月宫入口": (677, 1582),
}


def _load_components(script_dir, chain_path):
    sys.path.insert(0, HERE)
    import roam_map_connectivity_scan as scan
    rp = scan.load_grid_module(os.path.abspath(script_dir))
    data = json.load(open(chain_path, encoding="utf-8"))
    grids, dijk = data.get("map_grids") or {}, (data.get("dijkstra") or {})
    if "45" not in grids:
        print("[FAIL] 链数据没有 map45 网格")
        return None
    g = rp.MapGrid(45, grids["45"])
    comp_id, sizes = scan.components(g)
    main = sizes.index(max(sizes)) if sizes else -1
    return scan, rp, dijk, g, comp_id, sizes, main


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--script-dir", default=os.path.join(ROOT, "deploy", "zones", "prod-240-2300", "script"))
    ap.add_argument("--chain", default=os.path.join(ROOT, "data", "chains", "newbie_full.json"))
    ap.add_argument("--online", action="store_true")
    ap.add_argument("--api", default="http://127.0.0.1:28082")
    args = ap.parse_args()

    pack = _load_components(args.script_dir, args.chain)
    if pack is None:
        return 2
    scan, rp, dijk, g, comp_id, sizes, main = pack

    def dom(x, y):
        cell = g.to_grid(x, y)
        cid = comp_id.get(cell)
        if cid is None:
            near = rp.nearest_walkable(g, x, y, 8)
            if near:
                cell, cid = near, comp_id.get(near)
        return cid

    edges45 = dijk.get("45") or []
    npcs, landings, exits = collections.defaultdict(list), collections.defaultdict(list), collections.defaultdict(list)
    npc_opts = collections.defaultdict(list)   # npc_index -> [(spot, landing_xy, landing_dom)]
    for e in edges45:
        cid = dom(int(e.get("x") or 0), int(e.get("y") or 0))
        if e.get("kind") == "npc_jumper" and int(e.get("target_map") or 0) == 45:
            npc_opts[int(e.get("npc_index") or 0)].append(
                (e.get("spot_alias") or "", LANDING.get(e.get("spot_alias") or ""), cid))
            npcs[cid].append((int(e.get("npc_index") or 0), int(e.get("x") or 0), int(e.get("y") or 0)))
        elif e.get("kind") == "map_skip":
            exits[cid].append((int(e.get("target_map") or 0), int(e.get("x") or 0), int(e.get("y") or 0)))
    for spot, xy in LANDING.items():
        landings[dom(*xy)].append((spot, xy))

    # 落脚域预解析（用于选项行展示）
    opt_view = {}
    for nid, opts in npc_opts.items():
        lst = []
        for spot, xy, _cd in opts:
            ld = dom(*xy) if xy else None
            lst.append((spot, xy, ld))
        # 去重（同 NPC 同存多条自环重复项）
        seen, uniq = set(), []
        for spot, xy, ld in lst:
            if spot in seen:
                continue
            seen.add(spot)
            uniq.append((spot, xy, ld))
        opt_view[nid] = uniq

    print("=== map45 域拓扑（主域=域%d/%d 格）===" % (main, sizes[main]))
    for cid in range(len(sizes)):
        tags = []
        nset = sorted(set(n for n, _x, _y in npcs.get(cid, [])))
        if nset:
            tags.append("NPC=%s" % nset)
        if landings.get(cid):
            tags.append("落脚=%s" % [s.replace("跳到", "") for s, _ in landings[cid]])
        if exits.get(cid):
            tags.append("出边→%s" % sorted(set(t for t, _x, _y in exits[cid])))
        print("  域%-2d %-5d 格 %s%s" % (
            cid, sizes[cid], " ★主域" if cid == main else "", ("  " + " | ".join(tags)) if tags else "  (无 NPC/无落脚/无出边)"))

    print("\n=== 逃逸矩阵（每域可达 NPC 的选项 → 落脚域）===")
    for cid in range(len(sizes)):
        nset = sorted(set(n for n, _x, _y in npcs.get(cid, [])))
        if not nset:
            continue
        for nid in nset:
            name = {13338: "灵儿", 13339: "琴儿", 13340: "兰儿", 13341: "梦儿", 13342: "虹儿"}.get(nid, str(nid))
            print("  域%-2d → %s(%d):" % (cid, name, nid))
            for spot, xy, ld in opt_view.get(nid, []):
                bridge = "★桥到主域" if ld == main else ("→域%s" % ld)
                print("        %-12s 落(%s) %s" % (spot, xy, bridge))

    # ---------------- 在线处方 ----------------
    bad = 0
    if args.online:
        import urllib.request
        try:
            st = json.load(urllib.request.urlopen(args.api + "/api/status", timeout=10))
        except Exception as e:  # noqa
            print("[SKIP] 拉取 /api/status 失败: %s" % e)
            return 2
        rows = [r for r in (st.get("robots") or [])
                if int(r.get("mapid") or 0) == 45 and r.get("online")]
        print("\n=== 在线处方（45 上 %d 个号）===" % len(rows))
        fp = rp.GridPathFinder(g)
        for r in sorted(rows, key=lambda x: str(x.get("account"))):
            acc = r.get("account")
            pos = r.get("pos") or []
            cid = dom(pos[0], pos[1]) if len(pos) >= 2 else None
            nset = sorted(set(n for n, _x, _y in npcs.get(cid, []))) if cid is not None else []
            if not nset:
                bad += 1
                print("  %-24s 域%s 状态=%s → **无 NPC 可达, 需服务端拉出**" % (
                    acc, cid, r.get("state")))
                continue
            for nid in nset:
                npos = [(n, x, y) for n, x, y in npcs[cid] if n == nid][0]
                ok = fp.find_path(pos[0], pos[1], npos[1], npos[2]) is not None \
                    if len(pos) >= 2 else False
                picks = [s for s, xy, ld in opt_view.get(nid, []) if ld == main]
                rec = picks[0] if picks else (opt_view.get(nid, [[""]])[0][0] if opt_view.get(nid) else "")
                print("  %-24s 域%s 状态=%-8s → 走 %s(%d)@(%d,%d) [A* %s] 选「%s」(落主域)" % (
                    acc, cid, r.get("state"),
                    {13338: "灵儿", 13339: "琴儿", 13340: "兰儿", 13341: "梦儿", 13342: "虹儿"}.get(nid, nid),
                    nid, npos[1], npos[2], "可达" if ok else "不可达", rec))
    print("\n说明: 只读诊断; 域6 无任何出口/NPC → 仅服务端拉出可解。")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
