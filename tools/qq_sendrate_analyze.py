# -*- coding: utf-8 -*-
"""发包/交互统计（2026-09-21 发包限流分析用，可复跑对比部署前后）。

统计 runs_<日期>.jsonl（中控事件流）：
  1) 小时级包类/事件计数（点击NPC/对话/PLAYERMOVE/走路段/destination/钟馗对话/空菜单/战斗）
  2) 同号相邻两次「点击NPC 10146」「跨图跳转被拒绝」的间隔分布（限流效果对比用）
  3) 空菜单按号分布（集中度）
  4) 位置包 QPS 估算（走路距离/速度 × 10Hz）

用法：
  python qq_sendrate_analyze.py [runs_jsonl 路径]
  缺省 = F:\\ZyBin\\xm\\2d-xiyou-server\\robot\\ctrlcenter\\data\\runs_20260921.jsonl
"""
import collections
import io
import json
import math
import re
import sys
import time

DEFAULT = "F:/ZyBin/xm/2d-xiyou-server/robot/ctrlcenter/data/runs_20260921.jsonl"
path = sys.argv[1] if len(sys.argv) > 1 else DEFAULT

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

PAT_WALK = re.compile(r"走路: \((\d+),(\d+)\) → \((\d+),(\d+)\) 距离 (\d+)")
PAT_WHOLE = re.compile(r"SEND_WHOLE pts=(\d+)")
SPEED_PX_S = 120.0          # quest_walk_speed*60
POS_HZ = 10.0               # 1 / quest_walk_report_interval_sec

c = collections.Counter()
hour = collections.defaultdict(collections.Counter)
walk_dist = 0.0
click_ts = collections.defaultdict(list)
rej_ts = collections.defaultdict(list)
empty_by = collections.Counter()
zk_by = collections.Counter()
ts_min = ts_max = None
n = 0

with io.open(path, encoding="utf-8", errors="replace") as f:
    for line in f:
        n += 1
        try:
            d = json.loads(line)
        except Exception:
            continue
        ts = d.get("ts") or 0
        msg = d.get("msg") or ""
        a = d.get("account")
        if ts:
            ts_min = ts if ts_min is None or ts < ts_min else ts_min
            ts_max = ts if ts_max is None or ts > ts_max else ts_max
        h = time.strftime("%H", time.localtime(ts)) if ts else "?"
        if not msg:
            continue
        m = PAT_WALK.match(msg)
        if m:
            c["walk_seg"] += 1
            hour[h]["walk_seg"] += 1
            walk_dist += float(m.group(5))
        if msg.startswith("SEND_WHOLE "):
            c["playermove"] += 1
            hour[h]["playermove"] += 1
        if msg.startswith("点击NPC "):
            c["click_npc"] += 1
            hour[h]["click_npc"] += 1
            if a and "点击NPC 10146" in msg:
                click_ts[a].append(ts)
        if msg.startswith("点击跳转NPC "):
            c["click_jumpnpc"] += 1
            hour[h]["click_jumpnpc"] += 1
        if msg.startswith("点击对话选项"):
            c["click_dialog"] += 1
            hour[h]["click_dialog"] += 1
        if "发 C2S_DIJKSTRA_DESTINATION" in msg:
            c["dijkstra"] += 1
            hour[h]["dijkstra"] += 1
        if "跨图跳转被拒绝" in msg:
            c["hop_reject"] += 1
            if a:
                rej_ts[a].append(ts)
        if msg.startswith("抓鬼对话: NPC 10146"):
            c["zk_dialog"] += 1
            hour[h]["zk_dialog"] += 1
            if a:
                zk_by[a] += 1
            if "选项 0 个" in msg:
                c["zk_empty"] += 1
                hour[h]["zk_empty"] += 1
                if a:
                    empty_by[a] += 1
        if "抓鬼战斗开始" in msg:
            c["battle"] += 1
            hour[h]["battle"] += 1
        if "钟馗空菜单: 退避" in msg:
            c["backoff"] += 1
        if "重登恢复" in msg:
            c["relogin"] += 1


def gaps(store):
    out = []
    for a, tss in store.items():
        tss.sort()
        for i in range(1, len(tss)):
            out.append(tss[i] - tss[i - 1])
    return out


def pct(vals, thr):
    return sum(1 for v in vals if v <= thr) * 100.0 / len(vals) if vals else 0.0


print("文件=%s 行数=%d" % (path, n))
if ts_min:
    print("范围: %s -> %s" % (time.strftime('%m-%d %H:%M', time.localtime(ts_min)),
                              time.strftime('%m-%d %H:%M', time.localtime(ts_max))))
print("\n--- 事件计数 ---")
for k, v in c.most_common():
    print("%8d  %s" % (v, k))
walk_sec = walk_dist / SPEED_PX_S
span_s = max(1, (ts_max or 0) - (ts_min or 0))
print("\n走路: %d 段, 距离 %d px -> %.0f 秒" % (c["walk_seg"], walk_dist, walk_sec))
print("位置包估算: %.0f 个 (%.0f/s 平均, 含走路时段; 全局跨度 %.0f 秒)" % (
    walk_sec * POS_HZ, walk_sec * POS_HZ / span_s, span_s))

print("\n--- 每小时 ---")
keys = ["click_npc", "click_jumpnpc", "click_dialog", "playermove", "walk_seg",
        "dijkstra", "zk_dialog", "zk_empty", "battle"]
print("%-5s " % "h" + " ".join("%9s" % k for k in keys))
for h in sorted(hour):
    print("%-5s " % h + " ".join("%9d" % hour[h][k] for k in keys))

g = gaps(click_ts)
print("\n同号相邻两次「点击NPC 10146」间隔 (n=%d): <1s %.1f%% <2s %.1f%% <3s %.1f%%" % (
    len(g), pct(g, 1), pct(g, 2), pct(g, 3)))
g = gaps(rej_ts)
med = sorted(g)[len(g) // 2] if g else None
print("同号相邻两次「跨图跳转被拒绝」间隔 (n=%d): <2s %.1f%% <5s %.1f%% 中位数=%ss" % (
    len(g), pct(g, 2), pct(g, 5), med))

if empty_by:
    tot = sum(empty_by.values())
    vals = sorted(empty_by.values(), reverse=True)
    print("空菜单: 总 %d / 出现号 %d 个; top10 占比 %.0f%%; 高发号(>=20次) %d 个贡献 %.0f%%" % (
        tot, len(empty_by), sum(vals[:10]) * 100.0 / tot,
        sum(1 for v in vals if v >= 20), sum(v for v in vals if v >= 20) * 100.0 / tot))
    print("按号空菜单率(前 5):")
    for a, k in empty_by.most_common(5):
        print("  %s: %d/%d = %.1f%%" % (a, k, zk_by.get(a, 0), k * 100.0 / max(zk_by.get(a, 0), 1)))
