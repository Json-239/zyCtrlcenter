# -*- coding: utf-8 -*-
"""fix_effect_verify.py — 只读：修复包"前后窗口对比"验证工具（2026-09-28）。

背景：2026-09-28 买药/穷号/卡NPC修复包（P0-1/P0-2/P0-3/R1/G1-G3/摆摊防呆/乐观过图回滚）
落地后，需要在同一批账号、同一天的两个时间窗上做"修复前 baseline / 修复后 compare"对比。

指标（全部从机器人上报日志派生；diag 日志可选交叉）：
  ① buy_552        买药 552 循环次数 = 窗口内 "通知码 552" 条数
                     辅助：买药启动 / 买药超时(90s) / 扣款成功(1110)
  ② done_nobag     "买药完成但背包无药" 空转条数
  ③ leftover       GHOST_LEFTOVER_TASK 事件数（type=error&code 唯一计，避免 log+error 双计）
  ④ frozen_hard    硬卡号数：同号 (state, mapid, pos) 连续冻结 ≥ --freeze-min 分钟
                     附 active 子集（有动作预期状态 NAV/READY/DIALOG/HEAL/... 的冻结）
  ⑤ ghost_finish   抓鬼完成条数：只计打鬼任务（ti 2019501~2019513 且 ≠ 2019511 交付），
                     与 09-23 "每轮+2" 修复口径对齐；交付条数单列
  ⑥ wait_ghost     WAIT_GHOST 号数（窗口内曾出现 / 窗口末仍为 WAIT_GHOST）
修复包新日志锚点（预期 compare 窗口出现、baseline≈0；已按 fix-impl 实际文案校准）：
  nomoney("买不起"，P0-1) / buy_fail("买药失败") / streak("连败"，P0-3) /
  grid_rollback("越出本地图网格"，A5 网格相容性判据) /
  inconsistent("背包数据源不一致"，P0-0 实际文案) / activate_skip("ACTIVATE_SKIP")

数据源（全只读）：
  A. F:/ZyBin/zyCtrlcenter/data/bot_logs/<账号>/runs_<YYYYMMDD>.log（JSON Lines）
  B. --diag-dir 指定的 diag.log*（默认单机器人 script 目录；流式仅计数，不打印内容）
     · diag 里大量协议 dump 行无 ts，无法归窗——只统计含 ts 的行（QUEST_EVENT 等）；
     · 默认只扫"与窗口有交集的滚动文件"（首尾 ts 范围预判），--diag-all 才全扫。

用法：
  python tools/fix_effect_verify.py --baseline "13:57-16:00"
  python tools/fix_effect_verify.py --baseline "13:57-16:00" --compare "16:30-18:30"
  python tools/fix_effect_verify.py --date 20260928 --baseline ... --compare ...
  python tools/fix_effect_verify.py --baseline ... --no-diag        # 跳过 diag 交叉
  python tools/fix_effect_verify.py --baseline ... --freeze-min 8   # 硬卡阈值(默认10分钟)
输出：控制台对照表 + --json 明细（默认 tools/_tmp_scan/fix_effect_verify.json）。
红线：只读；不修改任何生产文件；不发送任何控制指令；diag 大文件流式限度。
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
OUT_DEFAULT = os.path.join(ROOT, "tools", "_tmp_scan", "fix_effect_verify.json")
DIAG_DIR_DEFAULT = "F:/ZyBin/xm/2d-xiyou-server/robot/deploy/single_robot_zy/script"

GHOST_TASK_MIN = 2019501
GHOST_TASK_MAX = 2019513
GHOST_SUBMIT_TASK = 2019511   # 交付任务：不计入 done_count（会双计）

ACTIVE_STATES = {"NAV", "READY", "DIALOG", "WAIT_MAP", "FIGHT", "HEAL",
                 "SUBMIT", "ACCEPT", "WAIT_TASK", "WAIT_NEXT"}

TS_RE = re.compile(r'"ts":(\d+)')
TS_RE_DIAG = re.compile(r"""['"]ts['"]\s*:\s*(\d+)""")
GHOST_DONE_RE = re.compile(r"抓鬼完成 (\d+)/(\d+) \(第 (\d+) 次\)")

# 关键词表（子串匹配原始行；order 稳定用于输出）
KW = [
    ("buy_start", "买药启动"),
    ("notice552", "通知码 552"),
    ("timeout90", "买药超时"),
    ("done_nobag", "买药完成但背包无药"),
    ("leftover_kw", "GHOST_LEFTOVER_TASK"),
    ("ghost_finish_log", "抓鬼完成"),
    ("nomoney", "买不起"),          # P0-1 新日志专用词（"储备金不足"会混入助战令文案，勿用）
    ("buy_fail", "买药失败"),
    ("streak", "连败"),
    ("rollback", "回滚"),
    ("grid_rollback", "越出本地图网格"),   # 2026-09-28 网格相容性判据回滚（5048 型）
    ("inconsistent", "背包数据源不一致"),  # P0-0（实际文案，非"两数据源不一致"）
    ("activate_skip", "ACTIVATE_SKIP"),
    ("paid1110", "商店购买已扣储备金"),
    ("satisfied", "已满足, 未购买"),
]
# 有号级归集的 key
KW_ACCT = {"notice552": "accounts_552", "done_nobag": "accounts_done_nobag",
           "nomoney": "accounts_nomoney", "buy_fail": "accounts_buyfail"}

# diag 交叉只取核心几项（避免大文件行数多时开销失控）
DIAG_KW = [("buy_start", "买药启动"), ("notice552", "通知码 552"),
           ("timeout90", "买药超时"), ("done_nobag", "买药完成但背包无药"),
           ("leftover_kw", "GHOST_LEFTOVER_TASK")]


def fmt_ts(ts):
    try:
        return time.strftime("%H:%M:%S", time.localtime(int(ts)))
    except Exception:
        return "?"


def parse_window(spec, date_str):
    """'13:57-16:00' → (start_ts, end_ts)（本地时区；end<=start 视为跨零点次日）。"""
    m = re.match(r"^\s*(\d{1,2}):(\d{2})\s*-\s*(\d{1,2}):(\d{2})\s*$", spec or "")
    if not m:
        raise ValueError("窗口格式应为 'HH:MM-HH:MM'，got: %r" % spec)
    h1, m1, h2, m2 = (int(x) for x in m.groups())
    if not (0 <= h1 < 24 and 0 <= h2 < 24 and 0 <= m1 < 60 and 0 <= m2 < 60):
        raise ValueError("窗口时间越界: %r" % spec)
    d = time.strptime(date_str, "%Y%m%d")
    base = time.mktime((d.tm_year, d.tm_mon, d.tm_mday, 0, 0, 0, 0, 0, -1))
    s = base + h1 * 3600 + m1 * 60
    e = base + h2 * 3600 + m2 * 60
    if e <= s:
        e += 86400
    return int(s), int(e)


def new_metrics():
    return {
        "cnt": collections.Counter(),
        "leftover_events": 0,
        "ghost_finish": 0,        # 打鬼完成条数
        "ghost_submit": 0,        # 交付(2019511)条数（单列，不计产出）
        "ghost_finish_by_acct": collections.Counter(),
        "robot_state_samples": 0,
        "accounts_552": set(),
        "accounts_done_nobag": set(),
        "accounts_leftover": set(),
        "accounts_nomoney": set(),
        "accounts_buyfail": set(),
        "wait_ghost_seen": set(),
        "last_state": {},         # acct -> 窗口内最后一条 robot_state 的 state
        "freeze_ctx": {},         # acct -> FreezeCtx
        "frozen": {},             # acct -> 最长 strict 冻结段
        "silent": {},             # acct -> 心跳静默段（不计入硬卡）
        "loiter": {},             # acct -> 最长原地徘徊段
        "samples": {},            # acct -> [(ts,x,y,state,mapid)]（供徘徊检测）
    }


class FreezeCtx(object):
    """同号 (state, mapid, pos) 连续段追踪。
    · strict 冻结：跨度 ≥ freeze_sec 且样本≥3 且段内最大采样间隔 ≤ gap_limit
      （样本≥3 与间隔约束用于排除"心跳静默号"：只有 2 个样本跨数小时 ≠ 卡住）；
    · silent 静默：跨度 ≥ 1h 且段内存在 ≥1h 空档（疑似心跳停/掉线，不计入硬卡）。
    """
    __slots__ = ("key", "since", "last_ts", "n", "max_gap",
                 "best", "sparse", "seg_count")

    def __init__(self):
        self.key = None
        self.since = 0
        self.last_ts = 0
        self.n = 0
        self.max_gap = 0
        self.best = None
        self.sparse = None
        self.seg_count = 0

    def feed(self, ts, state, mapid, pos, freeze_sec, gap_limit):
        k = (state, mapid, tuple(pos or []))
        if k == self.key:
            gap = ts - self.last_ts
            if gap > self.max_gap:
                self.max_gap = gap
            self.n += 1
            self.last_ts = ts
            return
        self._close(freeze_sec, gap_limit)
        self.key = k
        self.since = ts
        self.last_ts = ts
        self.n = 1
        self.max_gap = 0

    def _close(self, freeze_sec, gap_limit):
        if self.key is None or self.n < 2:
            return
        span = self.last_ts - self.since
        cur = {
            "state": self.key[0], "mapid": self.key[1],
            "pos": list(self.key[2]), "start": self.since, "end": self.last_ts,
            "span_s": span, "samples": self.n, "max_gap_s": self.max_gap,
            "active": self.key[0] in ACTIVE_STATES,
        }
        if span >= freeze_sec and self.n >= 3 and self.max_gap <= gap_limit:
            self.seg_count += 1
            if self.best is None or span > self.best["span_s"]:
                self.best = cur
        elif span >= 3600 and self.max_gap >= 3600:
            if self.sparse is None or span > self.sparse["span_s"]:
                self.sparse = cur

    def flush(self, freeze_sec, gap_limit):
        self._close(freeze_sec, gap_limit)


def scan_loiter(samples, freeze_sec, gap_limit, step_px):
    """原地徘徊检测：相邻样本位移 ≤ step_px 视为同链；
    链内跨度 ≥ freeze_sec 且样本≥3 且最大采样间隔 ≤ gap_limit → 命中。
    samples: [(ts, x, y, state, mapid), ...]。返回最长链 dict 或 None。"""
    best = None

    def consider(seg):
        nonlocal best
        if len(seg) < 3:
            return
        span = seg[-1][0] - seg[0][0]
        if span < freeze_sec:
            return
        mg = 0
        for k in range(1, len(seg)):
            g = seg[k][0] - seg[k - 1][0]
            if g > mg:
                mg = g
        if mg > gap_limit:
            return
        if best is not None and span <= best["span_s"]:
            return
        states = collections.Counter(s[3] for s in seg)
        st_main = states.most_common(1)[0][0]
        best = {
            "state": st_main, "states": dict(states),
            "mapid": seg[-1][4], "pos": [seg[0][1], seg[0][2]],
            "start": seg[0][0], "end": seg[-1][0],
            "span_s": span, "samples": len(seg), "max_gap_s": mg,
            "active": st_main in ACTIVE_STATES,
        }

    start = 0
    for j in range(1, len(samples)):
        dx = samples[j][1] - samples[j - 1][1]
        dy = samples[j][2] - samples[j - 1][2]
        if dx * dx + dy * dy > step_px * step_px:
            consider(samples[start:j])
            start = j
    consider(samples[start:])
    return best


def file_ts_range(fp, regex):
    """快速取文件首/尾 ts（各读头尾 8/16KB）——用于 diag 滚动文件与窗口的交集预判。"""
    first = last = 0
    try:
        with open(fp, "rb") as f:
            head = f.read(8192).decode("utf-8", "replace")
            m = regex.search(head)
            if m:
                first = int(m.group(1))
            f.seek(0, os.SEEK_END)
            size = f.tell()
            if size > 0:
                f.seek(max(0, size - 16384))
                tail = f.read().decode("utf-8", "replace")
                last_m = None
                for last_m in regex.finditer(tail):
                    pass
                if last_m:
                    last = int(last_m.group(1))
    except Exception:
        pass
    return first, last


def pick_window(ts, windows):
    for idx, (_, start, end) in enumerate(windows):
        if start <= ts < end:
            return idx
    return None


def scan_bot_logs(date_str, windows, freeze_sec, logs_dir=BOT_LOGS,
                  max_accounts=0, quiet=False, gap_limit=600, loiter_step_px=64):
    """流式扫全部账号日志（只解析落在窗口内的行）。返回 per-window metrics 列表。"""
    suffix = "runs_%s.log" % date_str
    try:
        names = sorted(os.listdir(logs_dir))
    except Exception as e:
        print("[err] 无法列目录 %s: %s" % (logs_dir, e), file=sys.stderr)
        return []
    wins = [new_metrics() for _ in windows]
    t0 = time.time()
    n_scanned = 0
    for i, acct in enumerate(names):
        fp = os.path.join(logs_dir, acct, suffix)
        if not os.path.isfile(fp):
            continue
        n_scanned += 1
        if max_accounts and n_scanned > max_accounts:
            break
        if not quiet and i and i % 100 == 0:
            print("[scan] %d/%d 账号 (%.0fs)" % (i, len(names), time.time() - t0))
        seen_win = set()
        try:
            f = io.open(fp, "r", encoding="utf-8", errors="replace")
        except Exception:
            continue
        with f:
            for line in f:
                m = TS_RE.search(line)
                if not m:
                    continue
                ts = int(m.group(1))
                wi = pick_window(ts, windows)
                if wi is None:
                    continue
                w = wins[wi]
                seen_win.add(wi)
                # 关键词（原始行子串，成本低）
                for key, needle in KW:
                    if needle in line:
                        w["cnt"][key] += 1
                        ak = KW_ACCT.get(key)
                        if ak:
                            w[ak].add(acct)
                # 结构化行（error/robot_state/抓鬼完成 需要 JSON 语义）
                d = None
                if ("GHOST_LEFTOVER_TASK" in line or "抓鬼完成" in line
                        or '"type":"robot_state"' in line):
                    try:
                        d = json.loads(line)
                    except Exception:
                        d = None
                if d is not None:
                    etype = d.get("type") or ""
                    msg = str(d.get("msg") or "")
                    if etype == "error" and str(d.get("code") or "") == "GHOST_LEFTOVER_TASK":
                        w["leftover_events"] += 1
                        w["accounts_leftover"].add(acct)
                    if msg.startswith("抓鬼完成") or "抓鬼完成" in msg:
                        gm = GHOST_DONE_RE.search(msg)
                        if gm:
                            ti = int(gm.group(3))
                            if GHOST_TASK_MIN <= ti <= GHOST_TASK_MAX:
                                if ti == GHOST_SUBMIT_TASK:
                                    w["ghost_submit"] += 1
                                else:
                                    w["ghost_finish"] += 1
                                    w["ghost_finish_by_acct"][acct] += 1
                    if etype == "robot_state":
                        w["robot_state_samples"] += 1
                        st = str(d.get("state") or "")
                        w["last_state"][acct] = st
                        if st == "WAIT_GHOST":
                            w["wait_ghost_seen"].add(acct)
                        ctx = w["freeze_ctx"].get(acct)
                        if ctx is None:
                            ctx = FreezeCtx()
                            w["freeze_ctx"][acct] = ctx
                        pos = d.get("pos") or []
                        ctx.feed(ts, st, d.get("mapid"), pos, freeze_sec, gap_limit)
                        x = pos[0] if len(pos) > 0 and isinstance(pos[0], int) else None
                        y = pos[1] if len(pos) > 1 and isinstance(pos[1], int) else None
                        if x is not None and y is not None:
                            w["samples"].setdefault(acct, []).append(
                                (ts, x, y, st, d.get("mapid")))
        # 该账号结束：结算严格冻结段 + 原地徘徊检测
        acct_samples = {}
        for wi in seen_win:
            w = wins[wi]
            ctx = w["freeze_ctx"].pop(acct, None)
            if ctx is not None:
                ctx.flush(freeze_sec, gap_limit)
                if ctx.best:
                    w["frozen"][acct] = ctx.best
                if ctx.sparse:
                    w["silent"][acct] = ctx.sparse
            acct_samples[wi] = w["samples"].pop(acct, [])
        for wi, arr in acct_samples.items():
            if len(arr) >= 3:
                hit = scan_loiter(arr, freeze_sec, gap_limit, loiter_step_px)
                if hit:
                    wins[wi]["loiter"][acct] = hit
    # 兜底 flush（理论上空）：未闭合段 + 未做徘徊检测的样本
    for wi in range(len(wins)):
        w = wins[wi]
        for acct, ctx in list(w["freeze_ctx"].items()):
            ctx.flush(freeze_sec, gap_limit)
            if ctx.best:
                w["frozen"][acct] = ctx.best
            if ctx.sparse:
                w["silent"][acct] = ctx.sparse
        w["freeze_ctx"] = {}
        for acct, arr in list(w["samples"].items()):
            if len(arr) >= 3:
                hit = scan_loiter(arr, freeze_sec, gap_limit, loiter_step_px)
                if hit:
                    w["loiter"][acct] = hit
        w["samples"] = {}
    print("[scan] 完成: %d 账号, %.0fs" % (n_scanned, time.time() - t0))
    return wins


def scan_diag(dirpath, windows, all_files=False, max_lines=0):
    """diag.log* 流式交叉计数（只统计含 ts 的行；滴漏 dump 行自然跳过）。"""
    out = [[] for _ in windows]
    if not dirpath or not os.path.isdir(dirpath):
        return None
    cand = ["diag.log", "diag.log.1", "diag.log.2", "diag.log.3"]
    files = [os.path.join(dirpath, n) for n in cand
             if os.path.isfile(os.path.join(dirpath, n))]
    if not all_files:
        keep = []
        for fp in files:
            first, last = file_ts_range(fp, TS_RE_DIAG)
            if first and last and any(s - 3600 <= last and e + 3600 >= first
                                      for _, s, e in windows):
                keep.append(fp)
        files = keep
    if not files:
        return None
    t0 = time.time()
    total_lines = 0
    for fp in files:
        try:
            f = io.open(fp, "r", encoding="utf-8", errors="replace")
        except Exception:
            continue
        with f:
            for line in f:
                if max_lines:
                    total_lines += 1
                    if total_lines > max_lines:
                        break
                if "通知码 552" not in line and "买药" not in line \
                        and "GHOST_LEFTOVER_TASK" not in line:
                    continue
                m = TS_RE_DIAG.search(line)
                if not m:
                    continue
                ts = int(m.group(1))
                wi = pick_window(ts, windows)
                if wi is None:
                    continue
                for key, needle in DIAG_KW:
                    if needle in line:
                        out[wi].append(key)
    res = []
    for arr in out:
        res.append(dict(collections.Counter(arr)))
    print("[diag] 交叉扫描 %d 文件, %.0fs" % (len(files), time.time() - t0))
    return res


def metric_value(w, path):
    if path.startswith("cnt."):
        return w["cnt"].get(path[4:], 0)
    if path == "leftover_events":
        return w["leftover_events"]
    if path == "ghost_finish":
        return w["ghost_finish"]
    if path == "ghost_submit":
        return w["ghost_submit"]
    if path == "frozen_all":
        return len(w["frozen"])
    if path == "frozen_active":
        return sum(1 for x in w["frozen"].values() if x.get("active"))
    if path == "loiter_all":
        return len(w["loiter"])
    if path == "loiter_active":
        return sum(1 for x in w["loiter"].values() if x.get("active"))
    if path == "silent":
        return len(w["silent"])
    if path == "wait_ghost_seen":
        return len(w["wait_ghost_seen"])
    if path == "wait_ghost_last":
        return sum(1 for st in w["last_state"].values() if st == "WAIT_GHOST")
    return w.get(path, 0)


ROWS = [
    ("① 买药552循环(通知码552)", "cnt.notice552", "down"),
    ("   买药启动", "cnt.buy_start", "down"),
    ("   买药超时(90s)", "cnt.timeout90", "down"),
    ("   扣款成功(1110)", "cnt.paid1110", "up"),
    ("② 买药完成但背包无药", "cnt.done_nobag", "down"),
    ("③ GHOST_LEFTOVER_TASK 事件", "leftover_events", "down"),
    ("④ 硬卡-严格冻结(active)", "frozen_active", "down"),
    ("     严格冻结(全状态含等待)", "frozen_all", "neutral"),
    ("     硬卡-原地徘徊(active)", "loiter_active", "down"),
    ("     原地徘徊(全状态)", "loiter_all", "neutral"),
    ("     心跳静默号(参考)", "silent", "neutral"),
    ("⑤ 抓鬼完成(打鬼)条数", "ghost_finish", "up"),
    ("     抓鬼交付(2019511)条数", "ghost_submit", "up"),
    ("⑥ WAIT_GHOST号数(曾出现)", "wait_ghost_seen", "neutral"),
    ("     WAIT_GHOST号数(窗口末)", "wait_ghost_last", "neutral"),
    ("＋ 储备金不足(P0-1新日志)", "cnt.nomoney", "up"),
    ("＋ 买药失败(P0-2/3)", "cnt.buy_fail", "neutral"),
    ("＋ 连败冷却(P0-3)", "cnt.streak", "up"),
    ("＋ 回滚(乐观过图)", "cnt.rollback", "neutral"),
    ("＋ 越出网格回滚(A5新)", "cnt.grid_rollback", "up"),
    ("＋ 背包数据源不一致", "cnt.inconsistent", "down"),
    ("＋ ACTIVATE_SKIP(摆摊/等级)", "cnt.activate_skip", "neutral"),
]


def render(label_specs, wins, diag_res, freeze_min):
    labels = [ls[0] for ls in label_specs]
    print("\n== fix_effect_verify 指标对照 ==")
    for (lab, spec, s, e) in label_specs:
        print("[窗口] %-8s %s  (%s ~ %s)" % (lab, spec, fmt_ts(s), fmt_ts(e)))
    head = "%-30s" % "指标"
    for lab in labels:
        head += " | %14s" % lab
    print("\n" + head)
    print("-" * len(head))
    for name, path, direction in ROWS:
        vals = [metric_value(w, path) for w in wins]
        line = "%-30s" % name
        for v in vals:
            line += " | %14s" % v
        if len(vals) == 2:
            a, b = vals
            if isinstance(a, int) and isinstance(b, int):
                d = b - a
                mark = ""
                if direction == "down" and d < 0:
                    mark = "↓好"
                elif direction == "up" and d > 0:
                    mark = "↑好"
                elif d != 0:
                    mark = "(%+d)" % d
                line += "  " + mark
        print(line)
    # 号级明细
    for lab, w in zip(labels, wins):
        print("\n-- %s 明细 --" % lab)
        fz = sorted(w["frozen"].items(), key=lambda kv: -kv[1]["span_s"])
        if fz:
            print("  严格冻结(≥%d分, 取样数≥3):" % freeze_min)
            for acct, x in fz[:20]:
                print("    %s %s map=%s pos=%s %s~%s 持续%.1f分(%d样本)%s" % (
                    acct.split("@")[0], x["state"], x["mapid"], x["pos"],
                    fmt_ts(x["start"]), fmt_ts(x["end"]),
                    x["span_s"] / 60.0, x["samples"],
                    " [active]" if x["active"] else ""))
        lo = sorted(w["loiter"].items(), key=lambda kv: -kv[1]["span_s"])
        if lo:
            print("  原地徘徊(相邻位移≤64px, ≥%d分):" % freeze_min)
            for acct, x in lo[:20]:
                print("    %s %s map=%s pos=%s %s~%s 持续%.1f分(%d样本)%s" % (
                    acct.split("@")[0], x["state"], x["mapid"], x["pos"],
                    fmt_ts(x["start"]), fmt_ts(x["end"]),
                    x["span_s"] / 60.0, x["samples"],
                    " [active]" if x["active"] else ""))
        si = sorted(w["silent"].items(), key=lambda kv: -kv[1]["span_s"])
        if si:
            print("  心跳静默(取样空档≥1h, 参考):")
            for acct, x in si[:10]:
                print("    %s %s map=%s pos=%s %s~%s 空档%.1f分" % (
                    acct.split("@")[0], x["state"], x["mapid"], x["pos"],
                    fmt_ts(x["start"]), fmt_ts(x["end"]), x["max_gap_s"] / 60.0))
        for key, tag in (("accounts_552", "552号"), ("accounts_done_nobag", "无药空转号"),
                         ("accounts_leftover", "残留任务号"), ("accounts_nomoney", "储备金不足号"),
                         ("accounts_buyfail", "买药失败号")):
            arr = sorted(w[key])
            if arr:
                print("  %s(%d): %s" % (tag, len(arr), ", ".join(a.split("@")[0] for a in arr[:40])))
        gb = w["ghost_finish_by_acct"].most_common(10)
        if gb:
            print("  抓鬼完成 top: %s" % ", ".join("%s=%d" % (a.split("@")[0], n) for a, n in gb))
    if diag_res is not None:
        print("\n-- diag 交叉(含 ts 行, 仅计数) --")
        for i, lab in enumerate(labels):
            print("  [%s] %s" % (lab, diag_res[i] if i < len(diag_res) else {}))


def to_jsonable(w):
    return {
        "cnt": dict(w["cnt"]),
        "leftover_events": w["leftover_events"],
        "ghost_finish": w["ghost_finish"],
        "ghost_submit": w["ghost_submit"],
        "ghost_finish_by_acct": dict(w["ghost_finish_by_acct"]),
        "robot_state_samples": w["robot_state_samples"],
        "accounts": {
            "552": sorted(w["accounts_552"]),
            "done_nobag": sorted(w["accounts_done_nobag"]),
            "leftover": sorted(w["accounts_leftover"]),
            "nomoney": sorted(w["accounts_nomoney"]),
            "buyfail": sorted(w["accounts_buyfail"]),
            "wait_ghost_seen": sorted(w["wait_ghost_seen"]),
        },
        "frozen": {a: x for a, x in w["frozen"].items()},
        "loiter": {a: x for a, x in w["loiter"].items()},
        "silent": {a: x for a, x in w["silent"].items()},
    }


def main():
    try:
        sys.stdout.reconfigure(errors="replace")
    except Exception:
        pass
    ap = argparse.ArgumentParser(description="修复包前后窗口对比（只读）")
    ap.add_argument("--date", default=time.strftime("%Y%m%d"), help="日志日期 YYYYMMDD（默认今天）")
    ap.add_argument("--baseline", required=True, help="修复前窗口 'HH:MM-HH:MM'（同日）")
    ap.add_argument("--compare", default="", help="修复后窗口 'HH:MM-HH:MM'（可选）")
    ap.add_argument("--logs-dir", default=BOT_LOGS)
    ap.add_argument("--freeze-min", type=float, default=10.0, help="硬卡冻结阈值（分钟，默认10）")
    ap.add_argument("--gap-s", type=int, default=600,
                    help="段内相邻样本最大间隔（秒，默认600；超过视为心跳静默不计硬卡）")
    ap.add_argument("--loiter-step-px", type=int, default=64,
                    help="原地徘徊：相邻样本位移阈值（像素，默认64=4格）")
    ap.add_argument("--diag-dir", default=DIAG_DIR_DEFAULT, help="diag.log* 目录（空=跳过）")
    ap.add_argument("--no-diag", action="store_true", help="跳过 diag 交叉扫描")
    ap.add_argument("--diag-all", action="store_true", help="diag 无视窗口交集，全文件扫")
    ap.add_argument("--max-accounts", type=int, default=0)
    ap.add_argument("--json", default=OUT_DEFAULT)
    ap.add_argument("--quiet", action="store_true")
    args = ap.parse_args()

    label_specs = [("baseline", args.baseline, 0, 0)]
    if args.compare:
        label_specs.append(("compare", args.compare, 0, 0))
    windows = []
    for i, (lab, spec, _, _) in enumerate(label_specs):
        s, e = parse_window(spec, args.date)
        label_specs[i] = (lab, spec, s, e)
        windows.append((lab, s, e))
    for i in range(len(windows)):
        for j in range(i + 1, len(windows)):
            if windows[i][1] < windows[j][2] and windows[j][1] < windows[i][2]:
                print("[warn] 窗口 %s 与 %s 重叠，重叠行将只计入先声明窗口" % (
                    windows[i][0], windows[j][0]))
    freeze_sec = int(args.freeze_min * 60)

    print("== fix_effect_verify date=%s freeze>=%.0fmin ==" % (args.date, args.freeze_min))
    wins = scan_bot_logs(args.date, windows, freeze_sec, logs_dir=args.logs_dir,
                         max_accounts=args.max_accounts, quiet=args.quiet,
                         gap_limit=args.gap_s, loiter_step_px=args.loiter_step_px)

    diag_res = None
    if not args.no_diag and args.diag_dir:
        diag_res = scan_diag(args.diag_dir, windows, all_files=args.diag_all)

    render(label_specs, wins, diag_res, args.freeze_min)

    payload = {
        "_ts": int(time.time()),
        "date": args.date,
        "freeze_min": args.freeze_min,
        "windows": {},
        "diag": diag_res,
    }
    for (lab, spec, s, e), w in zip(label_specs, wins):
        payload["windows"][lab] = {"spec": spec, "start_ts": s, "end_ts": e,
                                   **to_jsonable(w)}
    outdir = os.path.dirname(args.json)
    if outdir and not os.path.isdir(outdir):
        os.makedirs(outdir)
    with io.open(args.json, "w", encoding="utf-8", newline="\n") as f:
        f.write(json.dumps(payload, ensure_ascii=False, indent=2))
    print("\n[json] %s" % args.json)
    return 0


if __name__ == "__main__":
    sys.exit(main())
