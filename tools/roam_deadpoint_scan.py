# -*- coding: utf-8 -*-
"""游荡"图内死点"复查工具（只读）—— 2026-09-23

背景：少数游荡号卡在目标图死点，反复刷
  `地图 X 无可行路径 (x,y)→(x,y), 取消行走`（quest_engine.__do_walk 的 NO_LEGAL_ROUTE），
  位置一动不动；游荡号没有卡死上报机制 → 不会自愈。
修复（random_walk.py 2026-09-23b）后本工具用于**同一个口径**量化对比：
  · 刷屏号数 / 总条数（近 N 分钟，按账号分组）
  · 修复日志命中数：`吸附到可走格`（吸附）/ `换图 →`（连续无进展换图）/ `停止游荡`
  · `--money`：跨图成本口径（**必须按过图量归一，否则被 churn 骗**，2026-09-23 team-lead 定案）
      - 付费传送次数/银两 → 银/小时；过图次数（跳转成功/NPC过图）→ 次/分钟；
      - **归一化指标 = 付费次数 / 过图次数（次/过图）**：游荡跨图成本对比一律看它
        （实测：重启后原 银/小时 +44% 全是"全员重新跨图"的旅行波，归一后 0.23→0.24 持平）。
用法：
  python tools/roam_deadpoint_scan.py [--minutes 30] [--dir data/bot_logs] [--top 20]
                                      [--date YYYYMMDD] [--min-hits 1] [--money]
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


def money_stats(files, since_ts, now_ts):
    """跨图成本口径: 付费传送(次数/银两) vs 过图次数 → 归一化"次/过图"。

    付费判定 = 对话选项文本里的 "（N银）"(如"请送我去幽冥界（5银）"), 取 N 累计;
    过图判定 = "跨图跳转成功" / "跳转NPC过图成功"。
    返回 dict(paid, silver, cross, mins)。
    """
    import re
    paid = 0
    silver = 0
    cross = 0
    for fp in files:
        try:
            f = open(fp, encoding="utf-8", errors="replace")
        except Exception:
            continue
        with f:
            for line in f:
                if "银" not in line and "过图成功" not in line:
                    continue
                try:
                    o = json.loads(line)
                except Exception:
                    continue
                ts = o.get("ts") or 0
                if ts < since_ts or ts > now_ts:
                    continue
                msg = o.get("msg") or ""
                if "对话打开" in msg:
                    m = re.search(r"[（(](\d+)\s*银", msg)
                    if m:
                        paid += 1
                        silver += int(m.group(1))
                if "过图成功" in msg:
                    cross += 1
    return {"paid": paid, "silver": silver, "cross": cross,
            "mins": max(1.0, (now_ts - since_ts) / 60.0)}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dir", default=os.path.join("data", "bot_logs"))
    ap.add_argument("--minutes", type=int, default=30, help="只看最近 N 分钟（0=全部）")
    ap.add_argument("--date", default="", help="日志日期 YYYYMMDD（默认今天）")
    ap.add_argument("--top", type=int, default=20)
    ap.add_argument("--min-hits", type=int, default=1, help="只列刷屏 >= N 条的账号")
    ap.add_argument("--money", action="store_true",
                    help="附跨图成本口径(付费/过图 归一化; 别只看 银/小时, 会被 churn 骗)")
    args = ap.parse_args()

    date = args.date or time.strftime("%Y%m%d")
    now_ts = int(time.time())
    since = now_ts - args.minutes * 60 if args.minutes > 0 else 0
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
    if args.money:
        m = money_stats(files, since, now_ts)
        ratio = (m["paid"] / float(m["cross"])) if m["cross"] else 0.0
        print("成本(跨图): 付费 %d 次/%d 银 (%.0f 银/小时) | 过图 %d 次 (%.1f 次/分钟) | "
              "**归一 %d/%d = %.3f 次/过图**" % (
                  m["paid"], m["silver"], m["silver"] * 60.0 / m["mins"],
                  m["cross"], m["cross"] / m["mins"], m["paid"], m["cross"], ratio))
        print("  说明: 银/小时 会被过图量(churn)带偏; 跨图成本对比看'次/过图'(实测重启波: "
              "银/小时 +44% 但归一 0.23→0.24 持平)。")
    for acc, v in spam_accs[:args.top]:
        print("  %-26s 刷屏=%-5d 吸附=%-3d 换图=%-3d 停止=%-2d" % (
            acc, v["spam"], v["snap"], v["switch"], v["stop"]))
    if len(spam_accs) > args.top:
        print("  ...(还有 %d 个号)" % (len(spam_accs) - args.top))
    return 0


if __name__ == "__main__":
    sys.exit(main())
