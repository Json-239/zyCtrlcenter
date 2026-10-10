# -*- coding: utf-8 -*-
"""2026-10-10 落地批的「跨天验证」脚本（建议 0 点切换后 5~30 分钟内运行一次）。

用法:
    python tools/crossday_verify_20261010.py                 # 默认今天
    python tools/crossday_verify_20261010.py --day 20261011 --window 60

检查项（对应《2026-10-10 全队瘫痪修复批》0 点验证清单）:
  1) 跨天假满额:  机器人日志 "启动时计数已满" 条数（修前每日 8 个号中招）
  2) 双发幂等:    "幂等忽略"/ok_dup 条数（RESTORE+AUTOTASK 双发被幂等吃掉＝修复生效）
  3) 跨夜重置:    "跨夜重置抓鬼计数" 出现（真实清零路径在工作）
  4) 连接风暴:    中控日志窗口内 "robot 已连接" 次数（修前事故为每秒级）
  5) 看门狗误拉:  watchdog.log 窗口内 "★" 次数（应 0；"观察中" 属正常防误判）
  6) 48 死岛:     /api/status 中 mapid∈{47,48} 的在线号数（应 0；RESCUE48 兜底）
  7) 抓鬼产出:    窗口内 "抓鬼完成" 条数 vs 前一天同一窗口（信息性对比）

只读脚本：不写任何文件、不发 POST。中控未启动时跳过接口项（标 WARN）。
"""
import argparse
import datetime
import glob
import os
import re
import sys
import time
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BOT_LOGS = os.path.join(ROOT, "data", "bot_logs")


def read_text(path):
    try:
        with open(path, "r", encoding="utf-8", errors="replace") as f:
            return f.read()
    except OSError:
        return ""


def day_start_epoch(day):
    return int(time.mktime(time.strptime(day, "%Y%m%d")))


def prev_day(day):
    d = datetime.datetime.strptime(day, "%Y%m%d") - datetime.timedelta(days=1)
    return d.strftime("%Y%m%d")


def count_conn_events_in_window(text, day, window_min):
    """中控日志窗口内（00:00 ~ +window）的连接/握手计数。"""
    pat = re.compile(r"^%s-%s-%s (\d{2}):(\d{2}):" % (day[:4], day[4:6], day[6:]))
    conn = hello = fatal = 0
    for line in text.splitlines():
        m = pat.match(line)
        if not m:
            continue
        mins = int(m.group(1)) * 60 + int(m.group(2))
        if mins > window_min:
            continue
        if "robot 已连接" in line:
            conn += 1
        if "机器人握手 hello" in line:
            hello += 1
        if "[FATAL]" in line:
            fatal += 1
    return conn, hello, fatal


def count_bot_msgs(day, window_min, substrings):
    """遍历当天机器人日志，统计 msg 含任一子串、且 ts 落在窗口内的行数。"""
    start = day_start_epoch(day)
    end = start + window_min * 60
    counts = {s: 0 for s in substrings}
    files = glob.glob(os.path.join(BOT_LOGS, "*", "runs_%s.log" % day))
    for fp in files:
        try:
            with open(fp, "r", encoding="utf-8", errors="replace") as f:
                for line in f:
                    if '"ts"' not in line:
                        continue
                    m = re.search(r'"ts":\s*(\d+)', line)
                    if not m:
                        continue
                    ts = int(m.group(1))
                    if ts < start or ts > end:
                        continue
                    for s in substrings:
                        if s in line:
                            counts[s] += 1
        except OSError:
            continue
    return counts, len(files)


def fetch_status():
    try:
        req = urllib.request.Request("http://127.0.0.1:28082/api/status")
        with urllib.request.urlopen(req, timeout=8) as r:
            import json
            return json.loads(r.read().decode("utf-8", "replace"))
    except Exception:
        return None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--day", default=datetime.datetime.now().strftime("%Y%m%d"))
    ap.add_argument("--window", type=int, default=60, help="0 点后观测窗口(分钟), 默认 60")
    a = ap.parse_args()
    day = a.day
    window = max(5, a.window)
    results = []

    def rec(level, name, detail):
        results.append((level, name, detail))

    # ---- 中控日志窗口（风暴/致命） ----
    ctrl = read_text(os.path.join(ROOT, "logs", "ctrlcenter_%s.log" % day))
    conn, hello, fatal = count_conn_events_in_window(ctrl, day, window)
    rec("PASS" if conn <= 20 else "FAIL", "连接事件数(窗口内 robot 已连接)", str(conn))
    rec("PASS" if hello <= 5 else "FAIL", "hello 次数(窗口内)", str(hello))
    rec("PASS" if fatal == 0 else "FAIL", "FATAL 条数(窗口内)", str(fatal))
    rescue = len(re.findall(r"\[RESCUE48\]", ctrl))
    rec("INFO", "RESCUE48 行数(当日文件)", str(rescue))

    # ---- 看门狗 ----
    wd = read_text(os.path.join(ROOT, "logs", "watchdog.log"))
    wd_pat = re.compile(r"^%s-%s-%s (\d{2}):(\d{2}):" % (day[:4], day[4:6], day[6:]))
    kicks = observes = 0
    for line in wd.splitlines():
        m = wd_pat.match(line)
        if not m:
            continue
        mins = int(m.group(1)) * 60 + int(m.group(2))
        if mins > window:
            continue
        if "★" in line:
            kicks += 1
        if "观察中" in line:
            observes += 1
    rec("PASS" if kicks == 0 else "FAIL", "看门狗误拉(窗口内 ★ 次数)", str(kicks))
    rec("INFO", "看门狗『观察中』次数(防误判生效痕迹)", str(observes))

    # ---- 机器人日志（假满额/幂等/跨夜/产出） ----
    counts, nfiles = count_bot_msgs(day, window, [
        "启动时计数已满", "幂等忽略", "ok_dup", "跨夜重置抓鬼计数", "抓鬼完成",
    ])
    fake_full = counts["启动时计数已满"]
    rec("PASS" if fake_full <= 2 else "FAIL",
        "跨天假满额『启动时计数已满』", "%d 条(修前基线: 0 点窗口 8 个号)" % fake_full)
    dup = counts["幂等忽略"] + counts["ok_dup"]
    rec("INFO", "双发幂等命中(幂等忽略+ok_dup)", "%d 条(修复生效的正向痕迹)" % dup)
    rec("PASS" if counts["跨夜重置抓鬼计数"] > 0 else "WARN",
        "跨夜重置抓鬼计数出现次数", str(counts["跨夜重置抓鬼计数"]))

    pday = prev_day(day)
    pc, _ = count_bot_msgs(pday, window, ["抓鬼完成"])
    done_now, done_prev = counts["抓鬼完成"], pc["抓鬼完成"]
    if pc and done_prev > 0:
        ratio = done_now / float(done_prev)
        rec("PASS" if ratio >= 0.3 else "WARN",
            "抓鬼完成(窗口内) vs 前一天同窗",
            "今日 %d / 昨日 %d (%.0f%%)" % (done_now, done_prev, ratio * 100))
    else:
        rec("INFO", "抓鬼完成(窗口内)", "%d 条(昨日同窗无数据可比)" % done_now)
    rec("INFO", "机器人日志文件数(当日)", str(nfiles))

    # ---- 接口项 ----
    st = fetch_status()
    if st is None:
        rec("WARN", "接口 /api/status", "不可达（中控未启动？）")
    else:
        rob = st.get("robots") or []
        dead = [r.get("account") for r in rob if r.get("mapid") in (47, 48)]
        rec("PASS" if not dead else "FAIL", "47/48 死岛残留号数",
            "%d %s" % (len(dead), dead[:10]))
        c = st.get("counts") or {}
        on = c.get("online", 0)
        rec("PASS" if on >= 200 else "WARN", "在线数", str(on))
        rec("INFO", "capped 熔断数", str(len(st.get("restore_capped") or [])))

    # ---- 输出 ----
    print("=" * 72)
    print("跨天验证报告  day=%s  窗口=0点后 %d 分钟  时间=%s" %
          (day, window, datetime.datetime.now().strftime("%Y-%m-%d %H:%M:%S")))
    print("=" * 72)
    nfail = 0
    for lv, name, detail in results:
        if lv == "FAIL":
            nfail += 1
        print("[%-4s] %-34s %s" % (lv, name, detail))
    print("-" * 72)
    print("FAIL 数 = %d（0 = 全部关键项通过）" % nfail)
    return 0


if __name__ == "__main__":
    sys.exit(main())
