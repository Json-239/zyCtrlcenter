# -*- coding: utf-8 -*-
"""ghost_discover_funnel.py — 只读："发现鬼→后续"配对漏斗统计（#11 NAV 回归对照基线口径）。

口径（对齐 docs/04-测试/分析-20260928-抓鬼停摆深挖.md §3.3，可复跑）：
  事件   = 日志 "发现鬼 NPC <id>, 前往击杀"（type=log）
  窗口   = 该事件之后 40 行（同账号文件行序；事件行不计入窗口）
  项目   = 窗口内是否出现（布尔命中，逐事件计数）：
            cross_map   "(跨图)"                   （01: 99.3% 基线）
            nav_timeout "抓鬼状态 NAV 超时"          （02: 97.1% 基线）
            wait_ghost  "等刷鬼"                    （03: 93.0% 基线）
            fight       "战斗开始"                  （04: 57.3% 基线）
            broker      "前往钟馗"                  （05: 43.7% 基线；含"去钟馗"辅助列）
            done        "抓鬼完成"                  （06:  1.9% 基线）
  附加全窗独立计数（#11/P1 验证锚点，与事件窗口无关；NAV/非NAV 按 P1 口径拆分，勿并算）：
            re_nav      "重新导航追"                （坏版恒 0，修复后应 >0）
            np_nav      "仍无进展"且含 NAV           （P1 前 170~225/时；P1 后应 →0）
            np_other    "仍无进展"不含 NAV           （判据未动，预期零星）

用法：
  python tools/ghost_discover_funnel.py                              # 今天全量
  python tools/ghost_discover_funnel.py --since 17:30 --until 18:30  # 事件时间过滤（T1 对比）
  python tools/ghost_discover_funnel.py --date 20260928 --window-lines 40 --json out.json
输出：控制台表 + JSON（默认 tools/_tmp_scan/ghost_funnel.json）。
红线：只读；不修改生产文件；不发送任何指令。
"""
import argparse
import collections
import io
import json
import os
import re
import sys
import time

ROOT = "F:/ZyBin/zyCtrlcenter"
BOT_LOGS = os.path.join(ROOT, "data", "bot_logs")
OUT_DEFAULT = os.path.join(ROOT, "tools", "_tmp_scan", "ghost_funnel.json")
TS_RE = re.compile(r'"ts":(\d+)')

# (key, 子串, 中文标签, baseline 占比参考)
KW = [
    ("cross_map", "(跨图)", "鬼在图N(跨图)", "99.3%"),
    ("nav_timeout", "抓鬼状态 NAV 超时", "NAV 超时/重试", "97.1%"),
    ("wait_ghost", "等刷鬼", "等刷鬼", "93.0%"),
    ("fight", "战斗开始", "进战斗/战斗开始", "57.3%"),
    ("broker", "去钟馗", "去钟馗", "43.7%"),
    ("done", "抓鬼完成", "抓鬼完成", "1.9%"),
    ("re_nav", "重新导航追", "重新导航追(#11锚点)", "0(坏版)"),
    ("np_nav", None, "仍无进展-NAV(P1锚点)", "170~225/h(P1前)"),
    ("np_other", None, "仍无进展-非NAV", "零星(判据未动)"),
]
NKEY = len(KW)
EVENT_MARK = "发现鬼 NPC"


def _kw_hit(key, needle, line):
    """命中判断（needle=None 的复合条件按 key 特判；NAV/非NAV 拆分按 P1 口径）。"""
    if key == "np_nav":
        return "仍无进展" in line and "NAV" in line
    if key == "np_other":
        return "仍无进展" in line and "NAV" not in line
    return needle in line


def parse_hm(s, date_str):
    m = re.match(r"^\s*(\d{1,2}):(\d{2})\s*$", s or "")
    if not m:
        raise ValueError("时间格式应为 HH:MM，got: %r" % s)
    d = time.strptime(date_str, "%Y%m%d")
    base = time.mktime((d.tm_year, d.tm_mon, d.tm_mday, 0, 0, 0, 0, 0, -1))
    return int(base + int(m.group(1)) * 3600 + int(m.group(2)) * 60)


def scan_account(fp, w0, w1, window_lines, agg, samples):
    """流式扫描单账号：事件=发现鬼；窗口=其后 N 行；返回该账号事件数。"""
    pending = collections.deque()  # [expire_idx(不含), hits(list[bool]), ev_ts]
    n_events = 0
    idx = 0
    try:
        f = io.open(fp, "r", encoding="utf-8", errors="replace")
    except Exception:
        return 0
    with f:
        for line in f:
            while pending and pending[0][0] < idx:
                exp, hits, ev_ts = pending.popleft()
                agg["events"] += 1
                for i, (key, _, _, _) in enumerate(KW):
                    if hits[i]:
                        agg["hit"][key] += 1
                if hits[0]:  # cross_map
                    agg["cross_map_events"] += 1
            if EVENT_MARK not in line and not any(_kw_hit(k, n, line) for k, n, _, _ in KW):
                idx += 1
                continue
            m = TS_RE.search(line)
            ts = int(m.group(1)) if m else 0
            # 全局（时间窗内）附加计数
            if w0 <= ts < w1:
                for key, needle, _, _ in KW:
                    if key in ("re_nav", "np_nav", "np_other") and _kw_hit(key, needle, line):
                        agg["global"][key] += 1
                        smp = samples.setdefault(key, [])
                        if len(smp) < 2:
                            try:
                                d = json.loads(line)
                                smp.append(str(d.get("msg"))[:110])
                            except Exception:
                                smp.append(line.strip()[:110])
            # 并入窗口命中
            if pending:
                for i, (key, needle, _, _) in enumerate(KW):
                    if _kw_hit(key, needle, line):
                        for p in pending:
                            p[1][i] = True
            # 新事件
            if EVENT_MARK in line and w0 <= ts < w1:
                pending.append([idx + window_lines, [False] * NKEY, ts])
                n_events += 1
            idx += 1
        while pending:
            exp, hits, ev_ts = pending.popleft()
            agg["events"] += 1
            for i, (key, _, _, _) in enumerate(KW):
                if hits[i]:
                    agg["hit"][key] += 1
            if hits[0]:
                agg["cross_map_events"] += 1
    return n_events


def main():
    try:
        sys.stdout.reconfigure(errors="replace")
    except Exception:
        pass
    ap = argparse.ArgumentParser(description="发现鬼→后续配对漏斗（只读）")
    ap.add_argument("--date", default=time.strftime("%Y%m%d"))
    ap.add_argument("--logs-dir", default=BOT_LOGS)
    ap.add_argument("--since", default="", help="事件时间过滤起 HH:MM（默认全量）")
    ap.add_argument("--until", default="", help="事件时间过滤止 HH:MM")
    ap.add_argument("--window-lines", type=int, default=40)
    ap.add_argument("--max-accounts", type=int, default=0)
    ap.add_argument("--json", default=OUT_DEFAULT)
    args = ap.parse_args()

    if args.since or args.until:
        w0 = parse_hm(args.since or "00:00", args.date)
        w1 = parse_hm(args.until or "23:59", args.date)
    else:
        w0, w1 = 0, 1 << 62
    agg = {"events": 0, "cross_map_events": 0,
           "hit": collections.Counter(), "global": collections.Counter()}
    samples = {}
    suffix = "runs_%s.log" % args.date
    names = sorted(os.listdir(args.logs_dir))
    if args.max_accounts:
        names = names[:args.max_accounts]
    t0 = time.time()
    tot_ev = 0
    for i, acc in enumerate(names):
        fp = os.path.join(args.logs_dir, acc, suffix)
        if not os.path.isfile(fp):
            continue
        tot_ev += scan_account(fp, w0, w1, args.window_lines, agg, samples)
        if i and i % 150 == 0:
            print("[scan] %d/%d (%.0fs)" % (i, len(names), time.time() - t0))
    n = agg["events"]
    print("\n== 发现鬼→后续漏斗（date=%s, 事件窗 %s~%s, 后 %d 行）==" % (
        args.date, args.since or "00:00", args.until or "24:00", args.window_lines))
    print("事件总数 = %d（跨图 %d, %.1f%%）" % (
        n, agg["cross_map_events"],
        100.0 * agg["cross_map_events"] / n if n else 0.0))
    print("%-26s %8s %8s   %s" % ("窗口内出现", "次数", "占比", "baseline 参考"))
    for key, _, label, ref in KW:
        c = agg["hit"][key]
        print("%-26s %8d %7.1f%%   %s" % (label, c, 100.0 * c / n if n else 0.0, ref))
    print("\n[全局独立计数（事件时间窗内，不限窗口行）]")
    for key in ("re_nav", "np_nav", "np_other"):
        c = agg["global"][key]
        mins = max(1.0, (w1 - w0) / 60.0) if w1 < (1 << 62) else None
        rate = "（%.1f/时）" % (c * 60.0 / mins) if mins else ""
        print("  %s = %d %s" % (dict((k, lb) for k, _, lb, _ in KW)[key], c, rate))
        for s in samples.get(key, []):
            print("      例: %s" % s)
    outdir = os.path.dirname(args.json)
    if outdir and not os.path.isdir(outdir):
        os.makedirs(outdir)
    payload = {"_ts": int(time.time()), "date": args.date,
               "since": args.since, "until": args.until,
               "window_lines": args.window_lines,
               "events": n, "cross_map_events": agg["cross_map_events"],
               "hit": dict(agg["hit"]), "global": dict(agg["global"]),
               "samples": samples}
    with io.open(args.json, "w", encoding="utf-8", newline="\n") as f:
        f.write(json.dumps(payload, ensure_ascii=False, indent=1))
    print("\n[json] %s" % args.json)
    print("[scan] done in %.0fs" % (time.time() - t0))
    return 0


if __name__ == "__main__":
    sys.exit(main())
