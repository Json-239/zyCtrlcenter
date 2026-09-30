# -*- coding: utf-8 -*-
"""镖行天下"主票实验"只读取证脚本（2026-09-30，general-purpose-12 复核配套）。

数据源（全只读）：
  - <logdir>/<account>/runs_<YYYYMMDD>.log（机器人运行日志，JSON Lines；主源）
  - 可选 diag：<script 目录>/diag.log*（尽力补充，缺省不读）
用法：
  python tools/biaoxing_live_evidence.py                     # 默认 robot0005240@xy3.com / 今天
  python tools/biaoxing_live_evidence.py --accounts 5240,5301,5348 --date 20260930
  python tools/biaoxing_live_evidence.py --accounts 5240 --json out.json
账号可用短号（5240 → robot0005240@xy3.com）。

按轮次汇总（a-f）：
  a. 轮次边界与版本抽签（启动行/接票序列/押金版 01-06 vs 抵押品版 07→08-13）
  b. 钱包类可读行（预检/BALANCE_LOW/商店扣储备金；银两只在服务端不可见 → 注明）
  c. 打劫战（战斗开始↔打劫战斗结束配对：场次/时长/首触发延迟/存活=有结束无掉任务）
  d. 交付（FINISH 行 + 交付对话）
  e. done 计数（state 采样 + 启动行"今日已完成"；FINISH 与 done 不同步 → 标记 gap）
  f. 限时余量（接票→FINISH 用时 vs 30min buff 口径）
"""
import argparse
import datetime as _dt
import io
import json
import os
import re
import sys

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DEFAULT_LOGDIR = os.path.join(ROOT, "data", "bot_logs")
BIAOXING_KEYS = ("share_daily_镖行天下", "share_daily_镖局嘱托")  # 主 key + 嘱托 key


def norm_account(a):
    a = a.strip()
    if a.startswith("robot") and "@" in a:
        return a
    if a.isdigit():
        return "robot%07d@xy3.com" % int(a)
    return a


def fmt_ts(ts):
    try:
        return _dt.datetime.fromtimestamp(int(ts)).strftime("%H:%M:%S")
    except Exception:
        return "?"


def load_events(path):
    out = []
    if not os.path.exists(path):
        return out
    for ln in io.open(path, encoding="utf-8", errors="replace"):
        ln = ln.strip()
        if not ln:
            continue
        try:
            out.append(json.loads(ln))
        except Exception:
            continue
    return out


def analyze(account, date_str, main_only=True):
    """返回该账号当日的轮次档案（dict）。main_only=False 时也分析嘱托轮。"""
    path = os.path.join(DEFAULT_LOGDIR, account, "runs_%s.log" % date_str)
    evs = load_events(path)
    rounds = []
    cur = None
    prev_done_state = None

    def new_round(ts, limit, done_hint, kind_key):
        return {"key": kind_key, "start_ts": ts, "daily_limit": limit, "done_at_start": done_hint,
                "accepts": [], "dialog": [], "fights": [], "fight_open": None, "finishes": [],
                "wallet_lines": [], "done_samples": [], "stop": None, "end_ts": ts}

    for d in evs:
        ts = d.get("ts") or 0
        t = d.get("type")
        msg = str(d.get("msg") or "")
        # a. 轮次边界：启动行（主 key 或嘱托 key）
        m = re.search(r"分享日常启动: (share_daily_\S+?), 日限 (\d+), 今日已完成 (\d+)", msg)
        if m:
            key = m.group(1).rstrip(",")
            if key in BIAOXING_KEYS and (main_only is False or key == "share_daily_镖行天下"):
                if cur is not None:
                    cur["end_ts"] = ts
                    rounds.append(cur)
                cur = new_round(ts, int(m.group(2)), int(m.group(3)), key)
                prev_done_state = None
                continue
        if cur is None or cur["key"] != "share_daily_镖行天下":
            # 嘱托轮（main_only=True 时）单独收集为简易轮
            if m and m.group(1).rstrip(",") == "share_daily_镖局嘱托" and not main_only:
                if cur is not None:
                    cur["end_ts"] = ts
                    rounds.append(cur)
                cur = new_round(ts, int(m.group(2)), int(m.group(3)), "share_daily_镖局嘱托")
            continue
        # 以下均为主 key 轮内
        if t == "log":
            if msg.startswith("[神捕] 接到任务"):
                mm = re.search(r"接到任务 (\d+) \(catcher=(\d+)\)", msg)
                if mm:
                    cur["accepts"].append({"ts": ts, "task": int(mm.group(1)), "catcher": mm.group(2)})
            elif "接取对话: 选 #" in msg or "对话选项: #" in msg or "商店" in msg or "回收提交" in msg:
                cur["dialog"].append({"ts": ts, "msg": msg[:140]})
            elif msg.startswith("[神捕] 战斗开始"):
                cur["fight_open"] = ts
            elif "打劫战斗结束" in msg:
                dur = None
                if cur["fight_open"]:
                    dur = ts - cur["fight_open"]
                    cur["fight_open"] = None
                cur["fights"].append({"end_ts": ts, "dur_s": dur,
                                      "pre_accept": not cur["accepts"]})
            elif "被服务端放弃(掉任务)" in msg or "被删" in msg:
                cur.setdefault("drops", []).append({"ts": ts, "msg": msg[:140]})
            elif msg.startswith("[神捕] 任务 ") and "完成(FINISH)" in msg:
                mm = re.search(r"任务 (\d+) 完成\(FINISH\)", msg)
                cur["finishes"].append({"ts": ts, "task": int(mm.group(1)) if mm else 0})
            elif msg.startswith("[神捕] 停止"):
                cur["stop"] = {"ts": ts, "msg": msg[:120]}
            elif ("押金" in msg) or ("储备金" in msg) or ("现金" in msg) or ("购买" in msg):
                cur["wallet_lines"].append({"ts": ts, "msg": msg[:160]})
        elif t == "share_daily_state":
            done, limit = d.get("done"), d.get("limit")
            key = (d.get("state"), done, limit, d.get("task_index"))
            if (not cur.get("_last_state_key")) or cur["_last_state_key"] != key:
                cur["_last_state_key"] = key
                cur["done_samples"].append({"ts": ts, "state": d.get("state"), "done": done,
                                            "limit": limit, "task_index": d.get("task_index")})
            sk = d.get("share_key")
            if sk:
                cur.setdefault("state_keys", [])
                if not cur["state_keys"] or cur["state_keys"][-1] != sk:
                    cur["state_keys"].append(sk)
        elif t == "error":
            cur.setdefault("errors", []).append({"ts": ts, "code": d.get("code"), "msg": msg[:140]})
    if cur is not None:
        cur["end_ts"] = evs[-1].get("ts", 0) if evs else 0
        rounds.append(cur)

    # 后处理：版本/完整性/done gap
    for r in rounds:
        tasks = [a["task"] for a in r["accepts"]]
        veh = None
        for tk in tasks:
            if 2001101 <= tk <= 2001106:
                veh = "deposit(押金版 #%d)" % (tk - 2001100)
            elif tk == 2001107:
                veh = "collateral(抵押品版入口)"
            elif 2001108 <= tk <= 2001113:
                veh = "collateral(抵押品版 #%d)" % (tk - 2001100)
        r["version"] = veh or "(未接票)"
        r["finish_tasks"] = sorted(set(f["task"] for f in r["finishes"]))
        # 完整性：末次接票无对应 FINISH → 未完成（重启/掉线切断或进行中）
        if tasks and tasks[-1] not in r["finish_tasks"]:
            r["incomplete_reason"] = "末次接票 %d 未见 FINISH（重启/掉线切断或仍在进行）" % tasks[-1]
        # e. done-finish 同步检查：FINISH 主变体（01-06/08-13）后 done 是否增加
        gaps = []
        for f in r["finishes"]:
            base = [s for s in r["done_samples"] if s["ts"] >= f["ts"]]
            if f["task"] in list(range(2001101, 2001107)) + list(range(2001108, 2001114)):
                if base and base[0]["done"] is not None and r["done_samples"]:
                    before = [s for s in r["done_samples"] if s["ts"] <= f["ts"]]
                    b = before[-1]["done"] if before else None
                    a_ = base[0]["done"]
                    if b is not None and a_ is not None and a_ <= b:
                        gaps.append({"finish_task": f["task"], "finish_ts": f["ts"],
                                     "done_before": b, "done_after": a_})
        r["done_finish_gap"] = gaps
        # f. 限时余量：首接票 → 末次 FINISH
        if tasks and r["finishes"]:
            first_acc = min(a["ts"] for a in r["accepts"])
            last_fin = max(f["ts"] for f in r["finishes"])
            used = last_fin - first_acc
            r["time_used_s"] = used
            r["time_margin_s"] = 1800 - used
        # c. 首触发延迟：接票后的首场战斗（排除未接票时段的历史归因行）
        post = [f for f in r["fights"] if not f.get("pre_accept")]
        if tasks and post:
            r["first_fight_after_accept_s"] = post[0]["end_ts"] - min(a["ts"] for a in r["accepts"])
        r["drops_n"] = len(r.get("drops") or [])
    return {"account": account, "date": date_str, "runs_log": path, "rounds": rounds,
            "events_total": len(evs)}


def print_md(rep):
    print("# 镖行天下主票实验取证 —— %s（%s）" % (rep["account"], rep["date"]))
    print("- 数据源：`%s`（%d 条事件）" % (rep["runs_log"], rep["events_total"]))
    print("- 轮数：%d\n" % len(rep["rounds"]))
    for i, r in enumerate(rep["rounds"], 1):
        print("## 轮 %d [%s] %s → %s" % (i, r["version"], fmt_ts(r["start_ts"]), fmt_ts(r["end_ts"])))
        print("- 日限 %s / 启动时已完成 %s；接票 %s；FINISH %s" % (
            r["daily_limit"], r["done_at_start"],
            [t["task"] for t in r["accepts"]] or "-", r["finish_tasks"] or "-"))
        if r.get("incomplete_reason"):
            print("- **未完成轮**：%s" % r["incomplete_reason"])
        if r.get("stop"):
            print("- 停止：`%s`（%s）" % (r["stop"]["msg"], fmt_ts(r["stop"]["ts"])))
        if r["fights"]:
            pre = [f for f in r["fights"] if f.get("pre_accept")]
            post = [f for f in r["fights"] if not f.get("pre_accept")]
            print("- 打劫战 %d 场（接票后 %d + 未接票时段 %d）：" % (len(r["fights"]), len(post), len(pre)))
            for f in r["fights"]:
                tag = " ⚠未接票时段(bxtune 前历史归因)" if f.get("pre_accept") else ""
                print("  - %s 结束（时长 %ss）%s" % (fmt_ts(f["end_ts"]), f["dur_s"], tag))
            if r.get("first_fight_after_accept_s") is not None:
                print("  - 首触发延迟（接票→首场结束）：%ss" % r["first_fight_after_accept_s"])
            print("  - 掉任务/被删：%d 条%s" % (r["drops_n"], "（存活 ✓）" if r["drops_n"] == 0 else " ⚠"))
        if r.get("state_keys"):
            print("- state share_key 序列：%s" % " → ".join(r["state_keys"]))
        if r.get("time_used_s") is not None:
            print("- 限时：接票→末次 FINISH 用时 %ss（30min 余量 %ss）" % (
                r["time_used_s"], r["time_margin_s"]))
        if r["done_finish_gap"]:
            print("- **done 计数疑点**（FINISH 后 done 未增）：%s" % r["done_finish_gap"])
        for w in r["wallet_lines"][:6]:
            print("- 钱包行 %s：%s" % (fmt_ts(w["ts"]), w["msg"]))
        print()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--accounts", default="5240", help="逗号分隔（短号或全称）")
    ap.add_argument("--date", default=_dt.date.today().strftime("%Y%m%d"))
    ap.add_argument("--json", default="", help="落盘 JSON 路径（可选）")
    a = ap.parse_args()
    reports = []
    for acc in [x for x in a.accounts.split(",") if x.strip()]:
        rep = analyze(norm_account(acc), a.date)
        reports.append(rep)
        print_md(rep)
    if a.json:
        io.open(a.json, "w", encoding="utf-8", newline="\n").write(
            json.dumps(reports, ensure_ascii=False, indent=1))
        print("JSON 已写：%s" % a.json)
    return 0


if __name__ == "__main__":
    sys.exit(main())
