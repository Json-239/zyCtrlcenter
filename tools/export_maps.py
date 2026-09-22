# -*- coding: utf-8 -*-
"""从游戏配置 `config/map.csv` 导出地图名表 → JSON（{map_index: 地图名}），供中控显示用。

用法:
    python tools/export_maps.py <map.csv> [out.json]

默认输出: <项目>/data/maps.json

说明：map.csv 前两行是表头（中文标题行 + 英文字段行），数据行形如
    1,彩荷清池,普通,...
只取第 1 列（map_index，必须为数字）与第 2 列（map_name）。
"""
import io
import json
import os
import sys

try:
    sys.stdout.reconfigure(encoding="utf-8")
except Exception:
    pass

BASE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        return 1
    src = sys.argv[1]
    out = sys.argv[2] if len(sys.argv) > 2 else os.path.join(BASE, "data", "maps.json")
    if not os.path.exists(src):
        print("找不到 map.csv: %s" % src)
        return 1

    maps = {}
    with io.open(src, encoding="utf-8", errors="replace") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            parts = [x.strip() for x in line.split(",")]
            if len(parts) < 2 or not parts[0].isdigit():
                continue
            maps[parts[0]] = parts[1]

    d = os.path.dirname(out)
    if d and not os.path.isdir(d):
        os.makedirs(d)
    with io.open(out, "w", encoding="utf-8", newline="\n") as f:
        f.write("{\n")
        f.write("  \"_comment\": \"地图名表（map_index -> 名称），由 tools/export_maps.py 从游戏 config/map.csv 导出；客户端坐标=像素/16\",\n")
        items = sorted(maps.items(), key=lambda kv: int(kv[0]))
        body = ",\n".join('  "%s": "%s"' % (k, v.replace('"', '\\"')) for k, v in items)
        f.write(body + "\n}\n")
    print("已导出 %d 张地图 → %s" % (len(maps), out))
    return 0


if __name__ == "__main__":
    sys.exit(main())
