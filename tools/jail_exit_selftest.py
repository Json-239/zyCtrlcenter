# -*- coding: utf-8 -*-
"""牢房出口边自检 —— 2026-09-23

背景（现场取证）:
  robot0001108/robot0001176 被随机游荡抽进 654(地牢) 后出不来：链数据 dijkstra
  里 653/654/655 只有入边、没有出边 → 机器人 BFS 永远算不出离开路径。
  服务端三张牢房各有一个"牢头"，对话「离开这个是非之地」→ action_jumper 固定跳回:
    653 天牢 → 48  齐天桥   (天牢牢头 13460 @896,1424)
    654 地牢 → 609 地藏王殿 (地牢牢头 13459 @1744,1264)
    655 水牢 → 34  通天河   (水牢牢头 13451 @368,928)
  详见 tools/patch_jail_exit_edges.py。

本自检覆盖（不改任何文件）:
  1) 两份链数据（newbie_full / zhongkui_nav）都有 3 条牢房出口边，字段齐全且
     形状与既有 npc_jumper 一致（x/y=NPC 坐标、spot_alias=对话原文、无 free_race）；
  2) **用生产 quest_engine 的 BFS 本体**（从脚本源码抽取执行）验证：
     从牢房能算到外界图（默认口径 + 游荡回退口径 skip_bad_jumpers），且首跳就是
     牢头；同时确认普通图对（609→11）不会被引到牢房里绕路；
  3) 游荡可达性口径（roam_feasible_maps 本体）从 654 出发非空（否则游荡仍会报
     "当前图无可达游荡图"拒绝启动）；
  4) 机器人端对话选项匹配（复刻 __handle_dialog 的匹配分支）能选中
     「离开这个是非之地」且不会误选"劫狱"；
  5) 654 网格可达性：两个困图号的实际坐标都能走到牢头脚下（8 邻 BFS，
     walkable='0'）；655 网格里牢头所在格可走；653 无网格记录在案；
  6) 配置：机器人 config.py 两份 + Go config.go 默认值的游荡排除图都含
     653/654/655；且三张牢房都不在游荡世界图白名单里（防再被抽进）。

用法:
  python tools/jail_exit_selftest.py [zoneScriptDir]   # 默认 deploy/zones/prod-240-2300/script
"""
import json
import os
import re
import sys
import types

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DEFAULT_SCRIPT = os.path.join(ROOT, "deploy", "zones", "prod-240-2300", "script")
PROD_SCRIPT = r"F:\ZyBin\xm\2d-xiyou-server\robot\deploy\single_robot_zy\script"
CHAIN_DIR = os.path.join(ROOT, "data", "chains")

JAILS = [
    # (from_map, target_map, npc_index, npc_name, x, y, target_name)
    (653, 48, 13460, "天牢牢头", 896, 1424, "齐天桥"),
    (654, 609, 13459, "地牢牢头", 1744, 1264, "地藏王殿"),
    (655, 34, 13451, "水牢牢头", 368, 928, "通天河"),
]
DOPT = "离开这个是非之地"
SAMPLE_TARGET = 11          # 长安东市集：牢房出口后的常见去处
GRID_STARTS = [(1576, 1080), (1560, 1048), (1736, 1256)]  # 两个困图号实际走过的位置

PASS, FAIL = [], []


def ok(name, cond, detail=""):
    (PASS if cond else FAIL).append(name)
    print("  [%s] %s%s" % ("OK" if cond else "FAIL", name, (" —— " + detail) if detail else ""))


# ---------------- 从生产脚本抽取函数（与 hop_connectivity_selftest 同手法） ----------------

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


def load_quest_engine(script_dir):
    path = os.path.join(script_dir, "quest_engine.py")
    src = open(path, encoding="utf-8").read()
    mod = types.ModuleType("qe_extract")
    ns = mod.__dict__
    ns["time"] = __import__("time")
    ns["g_chain_dijkstra_cache"] = {}
    # __hop_key 被 __find_dijkstra_route 引用
    hk = _extract_func(src, "__hop_key")
    if hk:
        exec(compile(hk, path, "exec"), ns)
    for fn in ("__find_dijkstra_route", "reachable_maps", "roam_feasible_maps"):
        code = _extract_func(src, fn)
        if code is None:
            raise RuntimeError("quest_engine.py 里找不到 %s（脚本版本不匹配？）" % fn)
        exec(compile(code, path, "exec"), ns)
    return mod


class _Q(object):
    def __init__(self, chain):
        self.chain = chain
        self.hop_black = {}


# ---------------- 检查项 ----------------

def check_chain_files():
    print("== 1) 链数据（两份）")
    for name in ("newbie_full.json", "zhongkui_nav.json"):
        path = os.path.join(CHAIN_DIR, name)
        if not os.path.exists(path):
            ok("%s 存在" % name, False, "文件缺失")
            continue
        data = json.load(open(path, encoding="utf-8"))
        dj = data.get("dijkstra") or {}
        for fm, tm, npc, nn, x, y, tn in JAILS:
            edges = dj.get(str(fm)) or []
            hit = [e for e in edges
                   if e.get("kind") == "npc_jumper" and e.get("target_map") == tm
                   and e.get("npc_index") == npc]
            ok("%s %d→%d(%s) 存在" % (name, fm, tm, nn), bool(hit))
            if not hit:
                continue
            e = hit[0]
            ok("%s %d→%d 字段齐备" % (name, fm, tm),
               e.get("x") == x and e.get("y") == y and e.get("npc_name") == nn
               and e.get("target_name") == tn and e.get("spot_alias") == DOPT
               and int(e.get("cost_money", -1)) == 0 and not e.get("free_race")
               and e.get("dest_names") == [] and e.get("match_name") == "",
               json.dumps(e, ensure_ascii=False)[:180])
    # 索引唯一性（destination_index 用于跳转点黑名单定位）
    for name in ("newbie_full.json", "zhongkui_nav.json"):
        path = os.path.join(CHAIN_DIR, name)
        if not os.path.exists(path):
            continue
        data = json.load(open(path, encoding="utf-8"))
        seen, dup = set(), 0
        for _src, edges in (data.get("dijkstra") or {}).items():
            for e in edges:
                di = e.get("destination_index")
                if di in seen:
                    dup += 1
                seen.add(di)
        ok("%s destination_index 全局唯一" % name, dup == 0, "重复 %d 个" % dup)


def check_routes(qe, chain):
    print("== 2) 生产 BFS 验证（从牢房能出去）")
    q = _Q(chain)
    for fm, tm, npc, nn, x, y, tn in JAILS:
        route = qe.__find_dijkstra_route(q, fm, SAMPLE_TARGET)
        first = route[0] if route else {}
        ok("%d→%d 默认口径可达（首跳=牢头）" % (fm, SAMPLE_TARGET),
           bool(route) and first.get("kind") == "npc_jumper"
           and first.get("npc_index") == npc and first.get("target_map") == tm,
           "route=%s" % (json.dumps(route, ensure_ascii=False)[:200] if route else None))
        # 游荡口径：先纯跳转点（必为 None），再 allow npc_jumper + skip_bad
        r1 = qe.__find_dijkstra_route(q, fm, SAMPLE_TARGET, no_npc_jumper=True)
        ok("%d→%d 纯跳转点口径无路(符合预期)" % (fm, SAMPLE_TARGET), r1 is None)
        r2 = qe.__find_dijkstra_route(q, fm, SAMPLE_TARGET,
                                      no_npc_jumper=False, skip_bad_jumpers=True)
        ok("%d→%d 游荡回退口径可达" % (fm, SAMPLE_TARGET), bool(r2))
    # 游荡可达性（roam_feasible_maps 本体）
    cands = [1, 2, 5, 6, 9, 10, 11, 12, 17, 24, 25, 34, 48, 49, 609]
    feas = qe.roam_feasible_maps(q, 654, cands)
    ok("roam_feasible_maps(654) 非空", bool(feas), "可达=%s" % (sorted(feas) if feas else feas))
    feas3 = qe.roam_feasible_maps(q, 653, cands)
    ok("roam_feasible_maps(653) 非空", bool(feas3), "可达=%s" % (sorted(feas3) if feas3 else feas3))
    feas5 = qe.roam_feasible_maps(q, 655, cands)
    ok("roam_feasible_maps(655) 非空", bool(feas5), "可达=%s" % (sorted(feas5) if feas5 else feas5))
    # 普通图对不被引到牢房绕路
    r = qe.__find_dijkstra_route(q, 609, 11)
    ok("609→11 不经过牢房", bool(r) and all(e.get("target_map") not in (653, 654, 655) for e in r),
       json.dumps(r, ensure_ascii=False)[:160])


def match_option(option_list, target_name, spot_alias, match_name=""):
    """复刻 quest_engine.__handle_dialog 的目的地匹配分支（npc_jumper）。"""
    spot = spot_alias or ""
    spot_keys = []
    if spot:
        k = spot.replace("出生点", "").replace("热点", "").replace("别名", "").strip()
        if k:
            spot_keys.append(k)
    paid_re = re.compile(r"[（(]\s*\d+\s*(银|两|金)")
    best, paid_best = None, None
    for i in range(len(option_list)):
        if option_list[i][1] != 0:
            continue
        core = option_list[i][0]
        for pre in ("请送我去", "前往", "去"):
            if core.startswith(pre):
                core = core[len(pre):]
                break
        idx = core.find("（")
        if idx > 0:
            core = core[:idx]
        if match_name:
            matched = (match_name == core or match_name in core)
        else:
            matched = bool(target_name) and (target_name in option_list[i][0]
                                             or target_name in core or core in target_name)
        if not matched:
            for k in spot_keys:
                if k and (k in option_list[i][0] or k in core or core in k):
                    matched = True
                    break
        if matched:
            if paid_re.search(option_list[i][0]):
                if paid_best is None:
                    paid_best = i
                continue
            best = i
            break
    if best is None and paid_best is not None:
        best = paid_best
    return best


def check_dialog_match():
    print("== 3) 对话选项匹配（复刻机器人端分支）")
    normal = [["劫狱", 0], ["离开这个是非之地", 0], ["出狱与劫狱说明", 0], ["让我再想想吧", 1]]
    for fm, tm, npc, nn, x, y, tn in JAILS:
        idx = match_option(normal, tn, DOPT)
        ok("%s: 选中「离开这个是非之地」(#%s)" % (nn, idx), idx == 1)
    # 反例：不会误选"劫狱"（用不含该词的关键词模拟别的目的地）
    idx = match_option(normal, "齐天桥", "天牢至齐天桥跳转点")
    ok("spot 用跳转点名(不含选项文本)时选不中 → 反证 spot_alias 必须是对话原文",
       idx is None, "index=%s" % idx)


def _grid_walkable(grid, cx, cy):
    if cy < 0 or cy >= grid["h"] or cx < 0 or cx >= grid["w"]:
        return False
    return grid["rows"][cy][cx] == "0"


def _grid_bfs(grid, sx, sy):
    cell = int(grid.get("cell", 16))
    cx, cy = sx // cell, sy // cell
    if not _grid_walkable(grid, cx, cy):
        # 起点在障碍里：先找附近可走格（机器人端有 snap，这里 ±2 格内找）
        found = None
        for r in range(1, 3):
            for dx in range(-r, r + 1):
                for dy in range(-r, r + 1):
                    if _grid_walkable(grid, cx + dx, cy + dy):
                        found = (cx + dx, cy + dy)
                        break
                if found:
                    break
            if found:
                break
        if not found:
            return None
        cx, cy = found
    seen = {(cx, cy)}
    frontier = [(cx, cy)]
    while frontier:
        x, y = frontier.pop(0)
        for dx in (-1, 0, 1):
            for dy in (-1, 0, 1):
                if dx == 0 and dy == 0:
                    continue
                nx, ny = x + dx, y + dy
                if (nx, ny) in seen:
                    continue
                if _grid_walkable(grid, nx, ny):
                    # 不斜穿墙角：斜向时两个正交邻格都要可走
                    if dx != 0 and dy != 0:
                        if not (_grid_walkable(grid, x + dx, y) and _grid_walkable(grid, x, y + dy)):
                            continue
                    seen.add((nx, ny))
                    frontier.append((nx, ny))
    return seen


def check_grids(chain):
    print("== 4) 网格可达性（654 牢头脚下能走到）")
    grids = chain.get("map_grids") or {}
    g654 = grids.get("654")
    if not g654:
        ok("654 有网格", False)
        return
    reached = None
    for sx, sy in GRID_STARTS:
        rs = _grid_bfs(g654, sx, sy)
        if rs is None:
            ok("654 起点(%d,%d) 可走" % (sx, sy), False, "起点在障碍里且附近无可走格")
            continue
        npc_cell = (1744 // 16, 1264 // 16)
        if reached is None:
            reached = rs
            ok("654 牢头格可走", npc_cell in rs or _grid_walkable(g654, npc_cell[0], npc_cell[1]),
               "npc_cell=%s" % (npc_cell,))
        ok("654 (%d,%d)→牢头 可达" % (sx, sy), npc_cell in rs)
    g655 = grids.get("655")
    if g655:
        ok("655 牢头格可走", _grid_walkable(g655, 368 // 16, 928 // 16))
    else:
        ok("655 有网格", False, "水牢无网格（记录）")
    if "653" not in grids:
        print("  [记录] 653(天牢) 无寻路网格 —— 出口边仍然补齐，落点靠直线走（本就不该进）")


def _read_robot_excludes(path):
    src = open(path, encoding="utf-8").read()
    m = re.search(r"robot_roam_exclude_maps\s*=\s*\[([^\]]*)\]", src)
    if not m:
        return None
    return [int(x) for x in re.findall(r"\d+", m.group(1))]


def _read_robot_world(path):
    src = open(path, encoding="utf-8").read()
    m = re.search(r"robot_roam_world_maps\s*=\s*\[([^\]]*)\]", src)
    if not m:
        return None
    return [int(x) for x in re.findall(r"\d+", m.group(1))]


def check_configs(zone_script):
    print("== 5) 配置（游荡排除图 / 世界图白名单）")
    need = {24, 653, 654, 655}
    for tag, path in (("zone", os.path.join(zone_script, "config.py")),
                      ("prod", os.path.join(PROD_SCRIPT, "config.py"))):
        if not os.path.exists(path):
            ok("config.py[%s] 存在" % tag, False, path)
            continue
        ex = _read_robot_excludes(path)
        ok("config.py[%s] 排除图含 24/653/654/655" % tag,
           ex is not None and need.issubset(set(ex)), "exclude=%s" % ex)
        wm = _read_robot_world(path)
        ok("config.py[%s] 世界图不含牢房" % tag,
           wm is not None and not (set(wm) & {653, 654, 655}), "world∩jail=%s" %
           (sorted(set(wm or []) & {653, 654, 655})))
    # Go 侧默认值（env 未覆盖时生效；下次重建 exe 部署）
    go = os.path.join(ROOT, "internal", "config", "config.go")
    if os.path.exists(go):
        src = open(go, encoding="utf-8").read()
        m = re.search(r'envIntList\("CTRL_ROAM_EXCLUDE_MAPS",\s*\[\]int\{([^}]*)\}\)', src)
        vals = [int(x) for x in re.findall(r"\d+", m.group(1))] if m else []
        ok("Go config.go 默认排除图含 24/653/654/655",
           need.issubset(set(vals)), "vals=%s" % vals)
        m2 = re.search(r'envIntList\("CTRL_ROAM_WORLD_MAPS",\s*\[\]int\{([^}]*)\}\)', src)
        vals2 = [int(x) for x in re.findall(r"\d+", m2.group(1))] if m2 else []
        ok("Go 世界图不含牢房", not (set(vals2) & {653, 654, 655}))


def main():
    script_dir = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_SCRIPT
    print("脚本目录: %s" % script_dir)
    print("链数据目录: %s" % CHAIN_DIR)
    check_chain_files()
    try:
        qe = load_quest_engine(script_dir)
    except Exception as e:
        ok("抽取 quest_engine 函数", False, str(e))
        qe = None
    chain = json.load(open(os.path.join(CHAIN_DIR, "zhongkui_nav.json"), encoding="utf-8"))
    if qe is not None:
        check_routes(qe, chain)
    check_dialog_match()
    check_grids(chain)
    check_configs(script_dir)
    print("\n结果: PASS=%d FAIL=%d" % (len(PASS), len(FAIL)))
    if FAIL:
        print("失败项:")
        for f in FAIL:
            print("  - %s" % f)
    return 1 if FAIL else 0


if __name__ == "__main__":
    sys.exit(main())
