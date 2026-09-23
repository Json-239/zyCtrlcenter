# -*- coding: utf-8 -*-
"""抓鬼"巡逻选点死点"复查工具（只读）—— 2026-09-23

背景: 抓鬼号 `daily_ghost.__patrol` 旧实现拿 g.kill_area（**别的打鬼目标图**的坐标）
当巡逻基准 → 号被钟馗对话/换图拉回当前图后，每轮选点都不可达 → 反复刷
  `地图 X 无可行路径 (x,y)→(x,y), 取消行走`（现场 8 个号 ≈44 条/270s）。
修复（daily_ghost 2026-09-23e）:
  · kill_area 图≠当前图 → 基准退回当前位置;
  · 候选点复用 random_walk.walk_point_connected 试算（≤3 次换点）;
  · 全不通 → 不安排走动 + `巡逻选点 N 次均与当前位置不连通, 本轮不巡逻`。

本工具用于**同口径前后对比**（近 N 分钟，按账号分组）:
  · 刷屏: `无可行路径` 条数（修复目标：抓鬼号应显著下降/归零）
  · 巡逻: `附近巡逻`（巡逻轮次，修复后应 ≥ 修复前，说明巡逻仍在发生）
  · 跳过: `本轮不巡逻`（新的"全不连通"兜底，少量=正常）
  · 退化: `无 walk_point_connected`（random_walk 未热更的提示，应为 0）

**链路归属列（2026-09-23 加，防误读）**: `无可行路径` 消息本身不含链路信息
（quest_engine 通用报错），只看消息内容会把**游荡跨图跳转**的刷屏误读成抓鬼/巡逻链路。
本工具逐行扫描、按账号记住"最近一条链路标记"（`集合走/游荡`=游荡, `等刷鬼/抓鬼/钟馗/
巡逻/发现鬼`=抓鬼, `商店/任务链/新手`=其它），把随后的刷屏归到对应链路并分列输出。
归属是**启发式**（下游兜底：报错消息紧跟在哪个链路的动作之后），列出来是为了
"粗筛 ≠ 归因"——给人看时先看 游荡/抓鬼 分列，再看具体消息。

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

# 链路上下文关键词(优先级: 游荡 → 抓鬼 → 其它; 同一行命中多个取靠前的那档)
ROAM_CTX = ("集合走", "游荡")
GHOST_CTX = ("等刷鬼", "抓鬼", "钟馗", "发现鬼", "巡逻")
OTHER_CTX = ("商店", "任务链", "新手")
_KEYS = (SPAM, PATROL, SKIP, UNAVAIL) + ROAM_CTX + GHOST_CTX + OTHER_CTX


def _classify(msg):
    for k in ROAM_CTX:
        if k in msg:
            return "roam"
    for k in GHOST_CTX:
        if k in msg:
            return "ghost"
    for k in OTHER_CTX:
        if k in msg:
            return "other"
    return None


def scan_file(path, since_ts):
    """单账号日志扫描 → 计数 dict（含链路归属: roam/ghost/other/unknown）。"""
    out = {"spam": 0, "roam": 0, "ghost": 0, "other": 0, "unknown": 0,
           "patrol": 0, "skip": 0, "unavail": 0}
    try:
        f = open(path, encoding="utf-8", errors="replace")
    except Exception:
        return out
    ctx = None
    with f:
        for line in f:
            if not any(k in line for k in _KEYS):
                continue
            try:
                o = json.loads(line)
            except Exception:
                continue
            msg = o.get("msg") or ""
            # ① 链路上下文: 全文件跟踪(不受时间窗限制) → 窗口首条刷屏也有归属
            c = _classify(msg)
            if c is not None:
                ctx = c
            ts = o.get("ts") or 0
            if since_ts and ts < since_ts:
                continue
            # ② 计数
            if SPAM in msg:
                out["spam"] += 1
                out[ctx or "unknown"] += 1
            if PATROL in msg:
                out["patrol"] += 1
            if SKIP in msg:
                out["skip"] += 1
            if UNAVAIL in msg:
                out["unavail"] += 1
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dir", default=os.path.join("data", "bot_logs"))
    ap.add_argument("--minutes", type=int, default=5, help="只看最近 N 分钟（0=全部）")
    ap.add_argument("--date", default="", help="日志日期 YYYYMMDD（默认今天）")
    ap.add_argument("--top", type=int, default=20)
    ap.add_argument("--min-hits", type=int, default=1, help="只列刷屏 >= N 条的账号")
    ap.add_argument("--roam-only", action="store_true", help="只列刷屏里有游荡归属的账号")
    args = ap.parse_args()

    date = args.date or time.strftime("%Y%m%d")
    since = int(time.time()) - args.minutes * 60 if args.minutes > 0 else 0
    files = glob.glob(os.path.join(args.dir, "*", "runs_%s.log" % date))
    if not files:
        print("[FAIL] 没找到日志: %s/*/runs_%s.log" % (args.dir, date))
        return 2
    per_acc = {}
    total = {"spam": 0, "roam": 0, "ghost": 0, "other": 0, "unknown": 0,
             "patrol": 0, "skip": 0, "unavail": 0}
    for fp in files:
        acc = os.path.basename(os.path.dirname(fp))
        v = scan_file(fp, since)
        if v["spam"] or v["skip"] or v["patrol"] or v["unavail"]:
            per_acc[acc] = v
        for k in total:
            total[k] += v[k]
    rows = [(a, v) for a, v in per_acc.items() if v["spam"] >= args.min_hits]
    if args.roam_only:
        rows = [(a, v) for a, v in rows if v["roam"] > 0]
    rows.sort(key=lambda kv: -kv[1]["spam"])
    print("=== 抓鬼巡逻死点复查(近 %s 分钟, %s, %d 个账号日志) ===" % (
        args.minutes if args.minutes > 0 else "全部", date, len(files)))
    print("刷屏(%s): %d 个号 / %d 条 —— 归属: 游荡=%d 抓鬼=%d 其它=%d 未归类=%d" % (
        SPAM, len(rows), total["spam"], total["roam"], total["ghost"],
        total["other"], total["unknown"]))
    print("巡逻轮次(%s): %d | 全不通跳过(%s): %d | 入口退化(%s): %d" % (
        PATROL, total["patrol"], SKIP, total["skip"], UNAVAIL, total["unavail"]))
    for acc, v in rows[:args.top]:
        print("  %-28s 刷屏=%-4d [游荡=%-4d 抓鬼=%-3d 其它=%-3d 未知=%-2d] 巡逻=%-3d 全不通=%-3d 退化=%d" % (
            acc, v["spam"], v["roam"], v["ghost"], v["other"], v["unknown"],
            v["patrol"], v["skip"], v["unavail"]))
    if len(rows) > args.top:
        print("  ...(还有 %d 个号)" % (len(rows) - args.top))
    return 0


if __name__ == "__main__":
    sys.exit(main())
