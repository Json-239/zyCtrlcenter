# -*- coding: utf-8 -*-
"""给链数据补"牢房出口边"（653 天牢 / 654 地牢 / 655 水牢 → npc_jumper 牢头）。

背景（2026-09-23 现场取证）:
  robot0001108 / robot0001176 被"随机游荡"抽进 654(地牢) 后出不来 —— 链数据
  dijkstra 里 653/654/655 只有入边(其它图→牢房 map_skip)、**没有任何出边**，
  机器人 BFS 算不出离开路径，日志刷 "当前图无可达游荡图（链数据里从图 654 走不出去）"。

出口机制（服务端 config/npc/turnkey_ground.xml 等）:
  三张牢房各有一个"牢头"NPC，对话选项「离开这个是非之地」（非囚禁/非出狱状态可用）
  → action_jumper 到固定的"牢房→外界跳转点"：
    653 天牢牢头(13460) @(896,1424)  → 48  齐天桥   (config/map_spot.csv: PRISON_QITIAN_SPOT)
    654 地牢牢头(13459) @(1744,1264) → 609 地藏王殿 (PRISON_DIFU_SPOT)
    655 水牢牢头(13451) @(368,928)   → 34  通天河   (PRISON_TONGTIAN_SPOT)

机器人端用法（quest_engine py 参考）:
  kind="npc_jumper" → 走到 (x,y) 点 npc_index 开对话，按 spot_alias 匹配选项文本
  （本补丁把 spot_alias 设为对话原文「离开这个是非之地」——该文本不含目标图名，
   是唯一能稳定匹配的关键词）。destination_index 仅供黑名单/调试定位，需全局唯一。

本工具**只改我们这份链数据**（data/chains/*.json，Go 中控按 size:mtime 热重载），
不动共享的 config/dijkstra.xml 源数据（那是 py 参考侧接口，改不改等用户口径）。

用法:
  python tools/patch_jail_exit_edges.py --check    # 只检查（不写盘），退出码 0=已补/1=缺
  python tools/patch_jail_exit_edges.py --apply    # 补边（幂等；首次写盘前存 .bak_20260923_jail）
"""
import argparse
import json
import os
import shutil
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CHAIN_DIR = os.path.join(ROOT, "data", "chains")
FILES = ("newbie_full.json", "zhongkui_nav.json")
BAK_SUFFIX = ".bak_20260923_jail"

# (from_map, target_map, npc_index, npc_name, x, y, target_name)
JAIL_EXITS = [
    (653, 48, 13460, "天牢牢头", 896, 1424, "齐天桥"),
    (654, 609, 13459, "地牢牢头", 1744, 1264, "地藏王殿"),
    (655, 34, 13451, "水牢牢头", 368, 928, "通天河"),
]
SPOT_ALIAS = "离开这个是非之地"
# 对话原文（服务端 turnkey_*.xml option_step=3 的 option_text）：
#   三张牢房完全一致，非"囚禁状态/出狱状态"即可见 —— 误入牢房的号就是这个状态。


def make_edge(dest_index, from_map, target_map, npc_index, npc_name, x, y, target_name):
    return {
        "destination_index": dest_index,
        "from_map": from_map,
        "target_map": target_map,
        "kind": "npc_jumper",
        "x": x,
        "y": y,
        "target_name": target_name,
        "npc_index": npc_index,
        "npc_name": npc_name,
        "cost_money": 0,
        "spot_alias": SPOT_ALIAS,
        "dest_names": [],
        "match_name": "",
    }


def dump_compact(obj):
    # 与现存文件同格式：紧凑分隔符、中文不转义（机器人/Go 只按 JSON 语义解析）
    return json.dumps(obj, ensure_ascii=False, separators=(",", ":"))


def edge_exists(edges, from_map, target_map, npc_index):
    for e in edges:
        if (e.get("from_map") == from_map and e.get("target_map") == target_map
                and e.get("npc_index") == npc_index and e.get("kind") == "npc_jumper"):
            return True
    return False


def load(path):
    with open(path, "r", encoding="utf-8") as f:
        return json.load(f)


def next_indices(data, n):
    used = set()
    for _src, edges in (data.get("dijkstra") or {}).items():
        for e in edges:
            try:
                used.add(int(e.get("destination_index")))
            except Exception:
                pass
    out = []
    cur = max(used) if used else 0
    while len(out) < n:
        cur += 1
        if cur not in used:
            out.append(cur)
    return out


def check_file(path):
    data = load(path)
    dj = data.get("dijkstra") or {}
    missing = []
    for from_map, target_map, npc_index, npc_name, x, y, target_name in JAIL_EXITS:
        if not edge_exists(dj.get(str(from_map)) or [], from_map, target_map, npc_index):
            missing.append((from_map, target_map, npc_name))
    return missing, dj


def apply_file(path, dry=False):
    data = load(path)
    dj = data.setdefault("dijkstra", {})
    missing = []
    for from_map, target_map, npc_index, npc_name, x, y, target_name in JAIL_EXITS:
        if not edge_exists(dj.get(str(from_map)) or [], from_map, target_map, npc_index):
            missing.append((from_map, target_map, npc_index, npc_name, x, y, target_name))
    if not missing:
        return 0
    idxs = next_indices(data, len(missing))
    for (from_map, target_map, npc_index, npc_name, x, y, target_name), di in zip(missing, idxs):
        edge = make_edge(di, from_map, target_map, npc_index, npc_name, x, y, target_name)
        dj.setdefault(str(from_map), []).append(edge)
        print("  + %s → %s via %s(%d)@(%d,%d) spot=%r dst_index=%d" % (
            from_map, target_map, npc_name, npc_index, x, y, SPOT_ALIAS, di))
    if dry:
        return len(missing)
    bak = path + BAK_SUFFIX
    if not os.path.exists(bak):
        shutil.copy2(path, bak)
        print("  备份: %s" % bak)
    tmp = path + ".tmp_patch"
    with open(tmp, "w", encoding="utf-8") as f:
        f.write(dump_compact(data))  # 与原文件同格式：紧凑、无结尾换行（字节级往返已核对）
    os.replace(tmp, path)
    print("  已写入: %s" % path)
    return len(missing)


def main():
    ap = argparse.ArgumentParser()
    g = ap.add_mutually_exclusive_group(required=True)
    g.add_argument("--check", action="store_true", help="只检查是否已补")
    g.add_argument("--apply", action="store_true", help="补边（幂等）")
    args = ap.parse_args()

    total_missing = 0
    for name in FILES:
        path = os.path.join(CHAIN_DIR, name)
        if not os.path.exists(path):
            print("[跳过] 文件不存在: %s" % path)
            continue
        print("== %s" % name)
        if args.check:
            missing, dj = check_file(path)
            total_missing += len(missing)
            if missing:
                for fm, tm, nn in missing:
                    print("  缺: %s → %s (%s)" % (fm, tm, nn))
            else:
                print("  已补全（3 条牢房出口边俱在）")
        else:
            total_missing += apply_file(path)
    if args.check:
        print("结论: %s" % ("缺 %d 条，需要 --apply" % total_missing if total_missing else "全部已补"))
        return 1 if total_missing else 0
    print("完成: 本次补了 %d 条边" % total_missing)
    return 0


if __name__ == "__main__":
    sys.exit(main())
