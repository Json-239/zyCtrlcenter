# -*- coding: utf-8 -*-
"""游荡"图内死点"复查工具（只读）—— 2026-09-23

背景：少数游荡号卡在目标图死点，反复刷
  `地图 X 无可行路径 (x,y)→(x,y), 取消行走`（quest_engine.__do_walk 的 NO_LEGAL_ROUTE），
  位置一动不动；游荡号没有卡死上报机制 → 不会自愈。
修复（random_walk.py 2026-09-23b）后本工具用于**同一个口径**量化对比：
  · 刷屏号数 / 总条数（近 N 分钟，按账号分组）
  · 修复日志命中数：`吸附到可走格`（吸附）/ `换图 →`（连续无进展换图）/ `停止游荡`
用法：
  python tools/roam_deadpoint_scan.py [--minutes 30] [--dir data/bot_logs] [--top 20]
                                      [--date YYYYMMDD] [--min-hits 1]
日志来源：data/bot_logs/<账号>/runs_<YYYYMMDD>.log（JSON Lines, 字段 ts/msg/level）。
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
SNAP = "吸附到可走格"
SWITCH = "换图 →"
STOP = "停止游荡"


def scan_file(path, since_ts, patterns):
    """返回 {pattern: [(ts, msg), ...]}（只收 since_ts 之后的记录）。"""
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
    ap.add_argument("--minutes", type=int, default=30, help="只看最近 N 分钟（0=全部）")
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
    patterns = (SPAM, SNAP, SWITCH, STOP)
    per_acc = {}
    total = dict((p, 0) for p in patterns)
    for fp in files:
        acc = os.path.basename(os.path.dirname(fp))
        r = scan_file(fp, since, patterns)
        n_spam = len(r[SPAM])
        if any(r[p] for p in patterns):
            per_acc[acc] = {"spam": n_spam, "snap": len(r[SNAP]),
                            "switch": len(r[SWITCH]), "stop": len(r[STOP]),
                            "last": (max([t for t, _ in r[SPAM]]) if r[SPAM] else 0)}
        for p in patterns:
            total[p] += len(r[p])
    spam_accs = [(a, v) for a, v in per_acc.items() if v["spam"] >= args.min_hits]
    spam_accs.sort(key=lambda kv: -kv[1]["spam"])
    print("=== 游荡死点复查(近 %s 分钟, %s, %d 个账号日志) ===" % (
        args.minutes if args.minutes > 0 else "全部", date, len(files)))
    print("刷屏(%s): %d 个号 / %d 条 | 吸附(%s): %d 条 | 换图(%s): %d 条 | 停止(%s): %d 条" % (
        SPAM, len(spam_accs), total[SPAM], SNAP, total[SNAP], SWITCH, total[SWITCH],
        STOP, total[STOP]))
    for acc, v in spam_accs[:args.top]:
        print("  %-26s 刷屏=%-5d 吸附=%-3d 换图=%-3d 停止=%-2d" % (
            acc, v["spam"], v["snap"], v["switch"], v["stop"]))
    if len(spam_accs) > args.top:
        print("  ...(还有 %d 个号)" % (len(spam_accs) - args.top))
    return 0


if __name__ == "__main__":
    sys.exit(main())
