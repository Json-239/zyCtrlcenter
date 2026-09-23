# -*- coding: utf-8 -*-
"""抓鬼"巡逻选点死点"复查工具（只读）—— 2026-09-23

背景: 抓鬼号 `daily_ghost.__patrol` 旧实现拿 g.kill_area（**别的打鬼目标图**的坐标）
当巡逻基准 → 号被钟馗对话/换图拉回当前图后，每轮选点都不可达 → 反复刷
  `地图 X 无可行路径 (x,y)→(x,y), 取消行走`（现场 8 个号 ≈44 条/270s）。
修复（daily_ghost 2026-09-23d）:
  · kill_area 图≠当前图 → 基准退回当前位置;
  · 候选点复用 random_walk.walk_point_connected 试算（≤3 次换点）;
  · 全不通 → 不安排走动 + `巡逻选点 N 次均与当前位置不连通, 本轮不巡逻`。
本工具用于**同口径前后对比**（近 N 分钟，按账号分组）:
  · 刷屏: `无可行路径` 条数（修复目标：抓鬼号应显著下降/归零）
  · 巡逻: `附近巡逻`（巡逻轮次，修复后应 ≥ 修复前，说明巡逻仍在发生）
  · 跳过: `本轮不巡逻`（新的"全不连通"兜底，少量=正常）
  · 退化: `无 walk_point_connected`（random_walk 未热更的提示，应为 0）
用法:
  python tools/ghost_patrol_deadpoint_scan.py [--minutes 5] [--dir data/bot_logs]
                                              [--date YYYYMMDD] [--top 20] [--min-hits 1]
日志来源: data/bot_logs/<账号>/runs_<YYYYMMDD>.log（JSON Lines, 字段 ts/msg/level）。
"""
import argparse
import glob
import json
import os
import sys
import time

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

SPAM = "无可行路径"
PATROL = "附近巡逻"
SKIP = "本轮不巡逻"
UNAVAIL = "无 walk_point_connected"


def scan_file(path, since_ts, patterns):
    out = dict((p, []) for p in patterns)
    try:
        f = open(path, encoding="utf-8", errors="replace")
    except Exception:
        return out
    with f:
        for line in f:
            if not any(p in line for p in patterns):
                continue
            try:
                o = json.loads(line)
            except Exception:
                continue
            ts = o.get("ts") or 0
            if since_ts and ts < since_ts:
                continue
            msg = o.get("msg") or ""
            for p in patterns:
                if p in msg:
                    out[p].append((ts, msg))
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dir", default=os.path.join("data", "bot_logs"))
    ap.add_argument("--minutes", type=int, default=5, help="只看最近 N 分钟（0=全部）")
    ap.add_argument("--date", default="", help="日志日期 YYYYMMDD（默认今天）")
    ap.add_argument("--top", type=int, default=20)
    ap.add_argument("--min-hits", type=int, default=1, help="只列刷屏 >= N 条的账号")
    args = ap.parse_args()

    date = args.date or time.strftime("%Y%m%d")
    since = int(time.time()) - args.minutes * 60 if args.minutes > 0 else 0
    files = glob.glob(os.path.join(args.dir, "*", "runs_%s.log" % date))
    if not files:
        print("[FAIL] 没找到日志: %s/*/runs_%s.log" % (args.dir, date))
        return 2
    patterns = (SPAM, PATROL, SKIP, UNAVAIL)
    per_acc = {}
    total = dict((p, 0) for p in patterns)
    for fp in files:
        acc = os.path.basename(os.path.dirname(fp))
        r = scan_file(fp, since, patterns)
        if any(r[p] for p in patterns):
            per_acc[acc] = dict((p, len(r[p])) for p in patterns)
        for p in patterns:
            total[p] += len(r[p])
    rows = [(a, v) for a, v in per_acc.items() if v[SPAM] >= args.min_hits or v[SKIP]]
    rows.sort(key=lambda kv: -kv[1][SPAM])
    print("=== 抓鬼巡逻死点复查(近 %s 分钟, %s, %d 个账号日志) ===" % (
        args.minutes if args.minutes > 0 else "全部", date, len(files)))
    print("刷屏(%s): %d 个号 / %d 条 | 巡逻轮次(%s): %d | 全不通跳过(%s): %d | 入口退化(%s): %d" % (
        SPAM, len([1 for _a, v in rows if v[SPAM] >= args.min_hits]), total[SPAM],
        PATROL, total[PATROL], SKIP, total[SKIP], UNAVAIL, total[UNAVAIL]))
    for acc, v in rows[:args.top]:
        print("  %-28s 刷屏=%-5d 巡逻=%-3d 全不通=%-3d 退化=%d" % (
            acc, v[SPAM], v[PATROL], v[SKIP], v[UNAVAIL]))
    if len(rows) > args.top:
        print("  ...(还有 %d 个号)" % (len(rows) - args.top))
    return 0


if __name__ == "__main__":
    sys.exit(main())
