# -*- coding: utf-8 -*-
"""摆摊可摆点候选生成（2026-09-24 实测口径）。

背景（见 docs/04-测试/实测-20260924-摆摊1191定位与可摆点.md）:
  · 服务端开摊校验 = cpy_server.get_booth_colour(map_x/10, map_y/10) 查摆摊色块图（如 map11 的
    config/map_file/boothfile/cad_booth.bmp）：**像素非白=可摆、白=禁止(回 1191 "此地禁止摆摊")**；
  · 机器人走位 A* 的落点 = **格中心**（gx*16+8, gy*16+8），所以候选点直接按格中心遍历、
    用格中心像素判色，并留边距（默认 2px=20 游戏单位）吸收落点/坐标抖动；
  · 实测验证点（2026-09-24）：(1385,1545)、(3155,1555) 成功；(3864,2647)、(1525,1425) → 1191。

用法:
  python tools/booth_candidates.py --bmp "<cad_booth.bmp 路径>" --margin 2 --limit 30
  python tools/booth_candidates.py --bmp <...> --grid-json <grid.json>   # 只输出可走格(推荐)
  # grid.json 来自中控: GET http://127.0.0.1:28082/api/map/grid?mapid=11
  #   {"grid":{"w":..,"h":..,"rows":["0110..", ...]}}  rows[y][x]=='0' 即可走

输出: 每行一个候选点的**计划 cell 坐标（=格中心）**，按边距从大到小；含像素坐标与颜色供核对。
"""
import argparse
import json
import sys

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

CELL = 16  # 寻路网格格大小（与 quest_engine/robot_path 一致）


def load_bmp(path):
    from PIL import Image
    im = Image.open(path).convert("P")
    return im.load(), im.size


def dist_white(px, W, H, x, y, cap):
    """该像素到最近白像素的切比雪夫距离（上限 cap；0=像素本身是白）。"""
    if not (0 <= x < W and 0 <= y < H) or px[x, y] == 0:
        return 0
    for r in range(1, cap + 1):
        for dx in range(-r, r + 1):
            for dy in range(-r, r + 1):
                if max(abs(dx), abs(dy)) != r:
                    continue
                nx, ny = x + dx, y + dy
                if not (0 <= nx < W and 0 <= ny < H) or px[nx, ny] == 0:
                    return r - 1
    return cap


def main():
    ap = argparse.ArgumentParser(description="摆摊可摆点候选（格心落点 + 非白 + 边距）")
    ap.add_argument("--bmp", required=True, help="摆摊色块图，如 config/map_file/boothfile/cad_booth.bmp")
    ap.add_argument("--margin", type=int, default=2, help="要求的最小边距（像素；默认 2=20 游戏单位）")
    ap.add_argument("--limit", type=int, default=30, help="输出上限（按边距降序）")
    ap.add_argument("--grid-json", default="", help="可选：中控 /api/map/grid 的 JSON 文件（只保留可走格）")
    ap.add_argument("--exclude-near", default="", help="可选：x,y 与半径，排除其附近点。如 3864,2647,600")
    args = ap.parse_args()

    px, (W, H) = load_bmp(args.bmp)
    print("# 摆摊图 %s 尺寸 %dx%d（像素=游戏坐标/10，顶左原点）" % (args.bmp, W, H))

    walk_ok = None
    if args.grid_json:
        with open(args.grid_json, "r", encoding="utf-8") as f:
            g = json.load(f)["grid"]
        rows = g["rows"]
        walk_ok = lambda gx, gy: 0 <= gy < len(rows) and 0 <= gx < len(rows[gy]) and rows[gy][gx] == "0"
        print("# 已加载网格 %dx%d（rows[y][x]=='0' 可走）" % (g["w"], g["h"]))

    ex = None
    if args.exclude_near:
        try:
            exs = [float(v) for v in args.exclude_near.split(",")]
            ex = (exs[0], exs[1], exs[2])
        except Exception:
            print("!! --exclude-near 解析失败，忽略")

    out = []
    # 注意单位: BMP 每像素=10 游戏单位, 而寻路格=16 游戏单位（格心 = gx*16+8）。
    gx_max, gy_max = (W * 10) // CELL, (H * 10) // CELL
    for gy in range(gy_max):
        for gx in range(gx_max):
            cx, cy = gx * CELL + CELL // 2, gy * CELL + CELL // 2
            if walk_ok is not None and not walk_ok(gx, gy):
                continue
            if ex is not None and ((cx - ex[0]) ** 2 + (cy - ex[1]) ** 2) ** 0.5 < ex[2]:
                continue
            d = dist_white(px, W, H, cx // 10, cy // 10, args.margin + 1)
            if d >= args.margin:
                out.append((d, gx, gy, cx, cy, px[cx // 10, cy // 10]))

    out.sort(key=lambda t: (-t[0], t[2], t[1]))
    print("# 候选数（margin>=%d）: %d；输出前 %d：" % (args.margin, len(out), min(args.limit, len(out))))
    print("# plan占用cell(x,y)  格(gx,gy)  像素色  边距")
    for d, gx, gy, cx, cy, col in out[: args.limit]:
        print("%d,%d  (%d,%d)  color=%d  margin=%d" % (cx, cy, gx, gy, col, d))
    print("# 用法提示: plan 的 cell 直接填上面的坐标（格心）；同一格心建议整套坐标 (x,y) 原样用。")


if __name__ == "__main__":
    main()
