# -*- coding: utf-8 -*-
"""pool_fix_verify.py — 只读：两池（神捕/烽火）修复包效果验证 + 部署前基线快照（2026-09-29）。

背景：2026-09-29 两池修复包（Go 侧 ba2b3e5+28e960b / 机器人端 share_daily+client）统一窗口
部署前后，需要同一天"窗口前后对比"验证。本工具只读、可复跑。

指标（数据源 = 中控 HTTP + 当日日志）：
  ① 两池池态        /api/autotask 的 shenbu/fenghuo：candidates / usable / running / deficit
  ② 两池派发数      中控日志“已下发大唐神捕/烽火大唐任务”行数 + “[RESTORE] 补发 share_daily_start”
                    + “[ROAMPOOL] 回收→大唐神捕/烽火大唐”（修法 B 新通路，部署后才有）
  ③ 热循环（5207/5275）  机器人日志 destination 30 / 跨图跳转被拒绝 / 重走跳转点 / TASK_STUCK /
                    SHARE_DAILY_HOP_LOOP_STOP（修复包验收锚点，部署后每号一条级）
  ④ 恢复引擎留痕   中控日志 “[RESTORE-SKIP]” 条数（P0-5 新留痕，部署后才有）
  ⑤ 包满信号      /api/status robots 里 bag_full_age_ms 字段：存在（新心跳）/非 0/≤30min
  ⑥ 两池进度      两种玩法 daily done 合计（robots[].daily）+ 在跑号数
  ⑦ 对照          ghost done 合计（今日心跳）

用法：
  python tools/pool_fix_verify.py --snapshot                          # 即时快照（HTTP）
  python tools/pool_fix_verify.py --baseline "13:50-14:20" --snapshot
  python tools/pool_fix_verify.py --baseline "13:50-14:20" --compare "15:00-15:30" --snapshot
  python tools/pool_fix_verify.py --snapshot --json tools/_tmp_scan/pool_fix_baseline.json
输出：控制台摘要 + JSON（--json；默认只打印不落盘）。
红线：只读（仅 HTTP GET 与读文件）；不写任何生产数据；utf-8/LF。
"""
import argparse
import io
import json
import os
import re
import sys
import time
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT_DEFAULT = os.path.join(ROOT, "tools", "_tmp_scan", "pool_fix_verify.json")

# 热循环观察对象（2026-09-29 报告 §4.1：两号占全场 error 76%）
HOT_ACCOUNTS = ["robot0005207@xy3.com", "robot0005275@xy3.com"]
# 玩法键（与中控配置一致；匹配时先用精确键，退化用子串）
SHENBU_KEY = "share_daily_大唐神捕"
FENGHUO_KEY = "share_daily_宫廷10"
BAG_FULL_WINDOW_MS = 30 * 60 * 1000  # 与 Go 侧 dailyBagFullAgeWindowMS 同口径

# 中控日志（文本行：2026-09-29 14:20:10 [XXX] ...）窗口内计数模式
CTRL_PATTERNS = {
    "dispatch_shenbu": "[AUTOTASK] 大唐神捕 已下发大唐神捕任务",
    "dispatch_fenghuo": "[AUTOTASK] 烽火大唐 已下发烽火大唐任务",
    "restore_share_daily": "补发 share_daily_start",
    "roampool_reclaim_daily": "回收→大唐神捕",
    "roampool_reclaim_daily_fenghuo": "回收→烽火大唐",
    "roampool_reclaim_fail": "回收→日常池失败",  # 724712d 修复后应趋零（配额满改走 noop）
    "restore_skip": "[RESTORE-SKIP]",
    "daily_full_stop": "满额停止",
}
# 机器人日志（JSON Lines，含 "ts":epoch秒）窗口内计数模式
BOT_PATTERNS = {
    "dest30": "destination 30",
    "dest30_refused": "跨图跳转被拒绝",
    "dest30_rewalk": "重走跳转点",
    "task_stuck": "TASK_STUCK",
    "hop_loop_stop": "SHARE_DAILY_HOP_LOOP_STOP",
    "handin_stuck": "HANDIN_STUCK",
}
GLOBAL_HOP_KEY = b"SHARE_DAILY_HOP_LOOP_STOP"  # 全站字节扫描（锚点计数）


def _reconf_stdout():
    try:
        sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    except Exception:
        pass


def http_get_json(url, timeout):
    try:
        req = urllib.request.Request(url, headers={"Accept": "application/json"})
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return json.loads(r.read().decode("utf-8", "replace")), ""
    except Exception as e:
        return None, "%s: %s" % (type(e).__name__, e)


def parse_window(text):
    """'HH:MM-HH:MM' → (start_sec, end_sec)（当日秒；end<start 视为跨夜 +24h）。"""
    m = re.match(r"^\s*(\d{1,2}):(\d{2})\s*-\s*(\d{1,2}):(\d{2})\s*$", text or "")
    if not m:
        raise ValueError("窗口格式应为 HH:MM-HH:MM，实际 %r" % text)
    h1, m1, h2, m2 = (int(x) for x in m.groups())
    start = h1 * 3600 + m1 * 60
    end = h2 * 3600 + m2 * 60
    if end <= start:
        end += 24 * 3600
    return start, end


def in_window(sec_of_day, win, day_offset=0):
    start, end = win
    s = sec_of_day + day_offset * 86400
    return start <= s < end


# ---------------------------------------------------------------- 中控日志

CTRL_TS_RE = re.compile(r"^(\d{4})-(\d{2})-(\d{2}) (\d{2}):(\d{2}):(\d{2})")


def scan_ctrl_log(path, date, windows):
    """一遍流式：全量日计数 + 各窗口计数。返回 (day_totals, per_window)。"""
    day = {}
    per = {label: {} for label, _ in windows}
    if not os.path.isfile(path):
        return day, per
    with io.open(path, "r", encoding="utf-8", errors="replace") as f:
        for line in f:
            m = CTRL_TS_RE.match(line)
            if not m:
                continue
            y, mo, d, hh, mm, ss = m.groups()
            if "%s%s%s" % (y, mo, d) != date:
                continue
            sod = int(hh) * 3600 + int(mm) * 60 + int(ss)
            hits = [k for k, pat in CTRL_PATTERNS.items() if pat in line]
            if not hits:
                continue
            for k in hits:
                day[k] = day.get(k, 0) + 1
            for label, win in windows:
                if in_window(sod, win):
                    for k in hits:
                        per[label][k] = per[label].get(k, 0) + 1
    return day, per


# ---------------------------------------------------------------- 机器人日志


def scan_bot_log(path, windows):
    """JSON Lines：全量日计数 + 各窗口计数（同一遍）。返回 (day_totals, per_window)。"""
    day = {}
    per = {label: {} for label, _ in windows}
    if not os.path.isfile(path):
        return day, per
    with io.open(path, "r", encoding="utf-8", errors="replace") as f:
        for line in f:
            if '"ts":' not in line:
                continue
            hits = [k for k, pat in BOT_PATTERNS.items() if pat in line]
            if not hits:
                continue
            for k in hits:
                day[k] = day.get(k, 0) + 1
            if not windows:
                continue
            try:
                ts = int(line[line.index('"ts":') + 5:].split(",", 1)[0].split("}", 1)[0])
            except Exception:
                continue
            lt = time.localtime(ts)
            sod = lt.tm_hour * 3600 + lt.tm_min * 60 + lt.tm_sec
            for label, win in windows:
                if in_window(sod, win):
                    for k in hits:
                        per[label][k] = per[label].get(k, 0) + 1
    return day, per


def global_hop_count(date):
    """全站字节扫描 SHARE_DAILY_HOP_LOOP_STOP（部署后锚点；每号一条级）。"""
    total = 0
    files = 0
    base = os.path.join(ROOT, "data", "bot_logs")
    try:
        names = os.listdir(base)
    except OSError:
        return 0, 0
    for name in names:
        p = os.path.join(base, name, "runs_%s.log" % date)
        if not os.path.isfile(p):
            continue
        try:
            with open(p, "rb") as f:
                total += f.read().count(GLOBAL_HOP_KEY)
            files += 1
        except OSError:
            continue
    return total, files


# ---------------------------------------------------------------- HTTP 快照


def _daily_entries(daily):
    """robots[].daily 兼容单对象/数组 → [{share_key,done,limit,state}]。"""
    out = []
    if isinstance(daily, dict):
        if daily.get("share_key"):
            out.append(daily)
    elif isinstance(daily, list):
        for it in daily:
            if isinstance(it, dict) and it.get("share_key"):
                out.append(it)
    return out


def snapshot(base, timeout):
    snap = {"errors": []}
    at, err = http_get_json(base + "/api/autotask", timeout)
    if err:
        snap["errors"].append("autotask: " + err)
    else:
        cands = at.get("candidates") or {}
        pools = at.get("pools") or {}
        snap["pools"] = {}
        for k in ("shenbu", "fenghuo", "ghost", "newbie"):
            p = dict(pools.get(k) or {})
            p["candidates"] = cands.get(k)
            snap["pools"][k] = p
    st, err = http_get_json(base + "/api/status", timeout)
    if err:
        snap["errors"].append("status: " + err)
        return snap
    robots = st.get("robots") or []
    today = time.strftime("%Y%m%d")
    agg = {
        "robots": len(robots),
        "online": 0,
        "shenbu_done": 0, "shenbu_running": 0,
        "fenghuo_done": 0, "fenghuo_running": 0,
        "ghost_done": 0,
        "bag_full_field_present": 0, "bag_full_nonzero": 0, "bag_full_in_window": 0,
    }
    for r in robots:
        if r.get("online"):
            agg["online"] += 1
        for e in _daily_entries(r.get("daily")):
            key = e.get("share_key") or ""
            done = int(e.get("done") or 0)
            is_stopped = str(e.get("state") or "").upper() == "STOPPED"
            if key == SHENBU_KEY or "神捕" in key:
                agg["shenbu_done"] += done
                if not is_stopped:
                    agg["shenbu_running"] += 1
            elif key == FENGHUO_KEY or "宫廷" in key:
                agg["fenghuo_done"] += done
                if not is_stopped:
                    agg["fenghuo_running"] += 1
        g = r.get("ghost") or {}
        if str(g.get("count_date") or "") == today:
            agg["ghost_done"] += int(g.get("done") or 0)
        if "bag_full_age_ms" in r:
            agg["bag_full_field_present"] += 1
            age = int(r.get("bag_full_age_ms") or 0)
            if age > 0:
                agg["bag_full_nonzero"] += 1
                if age <= BAG_FULL_WINDOW_MS:
                    agg["bag_full_in_window"] += 1
    snap["aggregate"] = agg
    return snap


# ---------------------------------------------------------------- 摘要


def fmt_delta(base_cnt, cmp_cnt, key):
    a = (base_cnt or {}).get(key, 0)
    b = (cmp_cnt or {}).get(key, 0)
    return "%s=%d→%d" % (key, a, b)


def main():
    _reconf_stdout()
    ap = argparse.ArgumentParser(description="两池修复效果验证（只读）")
    ap.add_argument("--baseline", default="", help='"HH:MM-HH:MM"（修复前窗口）')
    ap.add_argument("--compare", default="", help='"HH:MM-HH:MM"（修复后窗口）')
    ap.add_argument("--snapshot", action="store_true", help="附带即时 HTTP 快照")
    ap.add_argument("--date", default=time.strftime("%Y%m%d"), help="日志日期 YYYYMMDD（默认今天）")
    ap.add_argument("--base", default="http://127.0.0.1:28082", help="中控 API 基址")
    ap.add_argument("--timeout", type=float, default=8.0, help="HTTP 超时秒")
    ap.add_argument("--json", default="", help="JSON 输出路径（默认只打印）")
    ap.add_argument("--no-global-scan", action="store_true", help="跳过全站 HOP_LOOP_STOP 字节扫描")
    args = ap.parse_args()

    windows = []
    if args.baseline:
        windows.append((args.baseline, parse_window(args.baseline)))
    if args.compare:
        windows.append((args.compare, parse_window(args.compare)))

    out = {
        "tool": "pool_fix_verify.py",
        "generated_at": time.strftime("%Y-%m-%d %H:%M:%S"),
        "date": args.date,
        "http_base": args.base,
        "windows": [w for w, _ in windows],
    }

    # ---- 中控日志（当日）
    ctrl_path = os.path.join(ROOT, "logs", "ctrlcenter_%s.log" % args.date)
    ctrl_day, ctrl_win = scan_ctrl_log(ctrl_path, args.date, windows)
    out["ctrl_log"] = {"path": ctrl_path, "day_totals": ctrl_day,
                       "per_window": ctrl_win}

    # ---- 机器人日志（热循环两号逐号 + 全站锚点）
    bot = {}
    for acc in HOT_ACCOUNTS:
        p = os.path.join(ROOT, "data", "bot_logs", acc, "runs_%s.log" % args.date)
        day, per = scan_bot_log(p, windows)
        bot[acc] = {"path": p, "day_totals": day, "per_window": per}
    out["bot_hot"] = bot
    if not args.no_global_scan:
        hop_total, hop_files = global_hop_count(args.date)
        out["global"] = {"hop_loop_stop_total": hop_total, "bot_log_files_scanned": hop_files}

    # ---- HTTP 快照
    if args.snapshot:
        out["snapshot"] = snapshot(args.base, args.timeout)

    # ---- 控制台摘要
    print("== pool_fix_verify · %s · date=%s ==" % (out["generated_at"], args.date))
    if args.snapshot and "snapshot" in out:
        snap = out["snapshot"]
        pools = snap.get("pools") or {}
        for k in ("shenbu", "fenghuo", "ghost"):
            p = pools.get(k) or {}
            print("  [pools] %-8s cand=%s usable=%s running=%s target=%s deficit=%s" % (
                k, p.get("candidates"), p.get("usable"), p.get("running"),
                p.get("target"), p.get("deficit")))
        agg = snap.get("aggregate") or {}
        if agg:
            print("  [daily] shenbu done=%s running=%s | fenghuo done=%s running=%s | ghost done=%s" % (
                agg.get("shenbu_done"), agg.get("shenbu_running"),
                agg.get("fenghuo_done"), agg.get("fenghuo_running"), agg.get("ghost_done")))
            print("  [bag]   bag_full_age_ms 字段存在=%s 非0=%s ≤30min=%s（在线 %s/%s）" % (
                agg.get("bag_full_field_present"), agg.get("bag_full_nonzero"),
                agg.get("bag_full_in_window"), agg.get("online"), agg.get("robots")))
        for e in snap.get("errors") or []:
            print("  [snapshot][warn] %s" % e)
    print("  [ctrl] day: " + " ".join("%s=%d" % (k, v) for k, v in sorted(ctrl_day.items())))
    for label, _ in windows:
        w = ctrl_win.get(label) or {}
        print("  [ctrl] %s: %s" % (label, " ".join("%s=%d" % (k, v) for k, v in sorted(w.items())) or "(无)"))
    for acc in HOT_ACCOUNTS:
        d = (bot.get(acc) or {}).get("day_totals") or {}
        print("  [bot]  %s day: %s" % (acc, " ".join("%s=%d" % (k, v) for k, v in sorted(d.items()))))
        for label, _ in windows:
            w = ((bot.get(acc) or {}).get("per_window") or {}).get(label) or {}
            print("  [bot]  %s %s: %s" % (acc, label,
                                          " ".join("%s=%d" % (k, v) for k, v in sorted(w.items())) or "(无)"))
    if "global" in out:
        print("  [global] HOP_LOOP_STOP 全站=%s（扫 %s 个 bot 日志文件）" % (
            out["global"]["hop_loop_stop_total"], out["global"]["bot_log_files_scanned"]))

    if args.json:
        path = args.json
        os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
        with io.open(path, "w", encoding="utf-8", newline="\n") as f:
            f.write(json.dumps(out, ensure_ascii=False, indent=2))
            f.write("\n")
        print("  [saved] %s" % path)
    return 0


if __name__ == "__main__":
    sys.exit(main())
