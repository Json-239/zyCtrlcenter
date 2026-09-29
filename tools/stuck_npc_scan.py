# -*- coding: utf-8 -*-
"""stuck_npc_scan.py — 只读：生产机器人"卡住"扫描（2026-09-28 13:57:35 重启后）。

口径（本文件同时是口径文档）:
  数据源:
    A. F:/ZyBin/zyCtrlcenter/data/bot_logs/<账号>/runs_20260928.log（JSON Lines, 每账号一份）
    B. http://127.0.0.1:28082/api/status（只读快照；可传 --status-file 用离线快照）
    C. F:/ZyBin/xm/2d-xiyou-server/config/npc_position.xml（附近 NPC 标注, 只读）
  卡住判定（分类会重叠, 每号可多标签）:
    1) capped        : 中控熔断表 restore_capped 里有该号（cap_reason 原样）
    2) dst_reject    : 窗口内出现"跨图跳转被拒(destination N)"→ 停链/重登循环源
    3) task_stuck    : 窗口内出现"停链等待处理"（TASK_STUCK / 换图推送迟迟未到等）
    4) dialog_stuck  : 窗口内出现 GHOST_DIALOG_STUCK / "点钟馗连续 N 次无对话"
    5) silent        : 当前 state ∈ {NAV,READY,DIALOG,WAIT_MAP,FIGHT,HEAL} 且
                       最后一条日志距今 > SILENT_GAP_S（默认 360s）——"有动作预期但没动静"
    6) frozen        : 两次 /api/status 快照间 pos_grid 未变 且 间隔 > FROZEN_MIN_S
                       （--prev-status-file 提供上一快照时启用）
    7) booth_block   : state == WAIT_MAP（摆摊自检 WAIT_MAP 阻塞全部模块, 见 booth.py）
    8) wait_ghost    : state == WAIT_GHOST（正常等待, 单列不做"卡"）
    9) no_path       : 窗口内出现 "无可行路径/走不到/不连通/NO_LEGAL_ROUTE"
  自愈判据: 信号后窗口内仍有 "走路:/到达跳转点/集合走启动" 等活动 → 视为已恢复(自愈)。

用法:
  python tools/stuck_npc_scan.py                     # 用当前 API 快照, 扫全部账号
  python tools/stuck_npc_scan.py --status-file X.json --prev-status-file Y.json
  python tools/stuck_npc_scan.py --since 1790575055  # 默认=2026-09-28 13:57:35
输出: 控制台摘要 + --json 指定的 JSON 明细（默认 tools/_tmp_scan/stuck_scan.json）。
红线: 只读, 不修改任何生产文件, 不发送任何控制指令。
"""
import argparse
import collections
import json
import os
import re
import sys
import time

try:
    import urllib.request
except ImportError:
    urllib.request = None

DEFAULT_SINCE = 1790575055      # 2026-09-28 13:57:35 本地(=重启时刻)
SILENT_GAP_S = 360              # 静默阈值(秒)
FROZEN_MIN_S = 240              # 冻结判定最小快照间隔
ACTIVE_STATES = ("NAV", "READY", "DIALOG", "WAIT_MAP", "FIGHT", "HEAL")
NOW = int(time.time())

ROOT = "F:/ZyBin/zyCtrlcenter"
BOT_LOGS = os.path.join(ROOT, "data", "bot_logs")
NPC_POS_XML = "F:/ZyBin/xm/2d-xiyou-server/config/npc_position.xml"
API_URL = "http://127.0.0.1:28082/api/status"

# 信号模式: key -> 子串（msg 里出现即计数; 前两个是"错误号"型事件）
PATTERNS = [
    ("dst_reject", "跨图跳转被拒"),
    ("dst_reject_retry", "跨图跳转被拒绝"),
    ("task_stuck", "停链等待处理"),
    ("no_path", "无可行路径"),
    ("no_route", "走不到"),
    ("not_conn", "不连通"),
    ("hop_push_late", "换图推送迟迟未到"),
    ("dialog_stuck", "无对话"),
    ("ghost_dialog_stuck", "GHOST_DIALOG_STUCK"),
    ("ghost_leftover", "GHOST_LEFTOVER"),
    ("roam_navfail", "跨图导航连续"),
    ("roam_nofeasible", "无纯跳转点"),
    ("roam_switch", "连续"),
    ("far_snap", "不吸附"),
    ("dst_rollback", "回滚"),
    ("relogin_down", "账号下线"),
    ("relogin_up", "账号上线"),
    ("walk", "走路:"),
    ("arrive_hop", "到达跳转点"),
    ("roam_start", "集合走启动"),
    ("roam_stop", "集合走停止"),
    ("task_err", "任务出错"),
    ("chain_done", "链已完成"),
]


def fetch_status(url):
    if urllib.request is None:
        return None
    try:
        with urllib.request.urlopen(url, timeout=15) as r:
            return json.loads(r.read().decode("utf-8", "replace"))
    except Exception as e:
        print("[warn] API 读取失败: %s" % e, file=sys.stderr)
        return None


def load_npc_positions(path):
    """解析 npc_position.xml → {map_index: [(npc_index, npc_name, x, y), ...]}"""
    out = {}
    if not os.path.exists(path):
        return out
    try:
        import xml.etree.ElementTree as ET
        tree = ET.parse(path)
        for obj in tree.getroot().findall("npc_object"):
            idx = obj.get("npc_index") or ""
            name = obj.get("npc_name") or ""
            for mp in obj.iter("map_entry"):
                try:
                    m = int(mp.get("map_index"))
                    x = int(mp.get("map_x"))
                    y = int(mp.get("map_y"))
                except Exception:
                    continue
                out.setdefault(m, []).append((idx, name, x, y))
    except Exception as e:
        print("[warn] npc_position.xml 解析失败: %s" % e, file=sys.stderr)
    return out


def nearest_npcs(npc_map, mapid, x, y, max_d=800, k=2):
    arr = npc_map.get(int(mapid or 0)) or []
    ds = []
    for (idx, name, nx, ny) in arr:
        d2 = (nx - x) ** 2 + (ny - y) ** 2
        ds.append((d2, idx, name, nx, ny))
    ds.sort()
    out = []
    for d2, idx, name, nx, ny in ds[:k]:
        d = int(d2 ** 0.5)
        if d <= max_d:
            out.append("%s(%s) 距%dpx" % (name, idx, d))
    return out


def scan_account_log(path, since):
    """流式扫描单账号日志（只留 ts>=since 的事件）。返回 dict 指标。"""
    m = {
        "events": 0,
        "first_ts": 0, "last_ts": 0, "last_msg": "", "last_level": "",
        "last3": [],
        "counts": collections.Counter(),
        "last_walk_ts": 0, "walk_cnt": 0,
        "sig_samples": {},
        "roam_on": False, "roam_target": 0, "roam_last_ts": 0,
    }
    try:
        f = open(path, "r", encoding="utf-8", errors="replace")
    except Exception:
        return None
    with f:
        for line in f:
            i = line.find('"ts":')
            if i < 0:
                continue
            j = i + 5
            n = j
            while n < len(line) and (line[n].isdigit()):
                n += 1
            if n == j:
                continue
            try:
                ts = int(line[j:n])
            except Exception:
                continue
            if ts < since:
                continue
            try:
                d = json.loads(line)
            except Exception:
                continue
            msg = str(d.get("msg") or "")
            if not msg:
                # 有些行 msg 在 cmd/error 中, 拼一个可读串
                msg = str(d.get("cmd") or d.get("code") or "")
            lvl = str(d.get("level") or "")
            m["events"] += 1
            if not m["first_ts"]:
                m["first_ts"] = ts
            m["last_ts"] = ts
            m["last_msg"] = msg[:160]
            m["last_level"] = lvl
            m["last3"].append((ts, lvl, msg[:120]))
            if len(m["last3"]) > 3:
                m["last3"].pop(0)
            for key, pat in PATTERNS:
                if pat in msg:
                    m["counts"][key] += 1
                    if key not in m["sig_samples"]:
                        m["sig_samples"][key] = (ts, msg[:160])
            if "走路:" in msg:
                m["counts"]["walk"] = m["counts"].get("walk", 0)
                m["last_walk_ts"] = ts
                m["walk_cnt"] += 1
            if "集合走启动" in msg:
                m["roam_on"] = True
                m["roam_last_ts"] = ts
                mm = re.search(r"目标图 (\d+)", msg)
                if mm:
                    m["roam_target"] = int(mm.group(1))
            elif "集合走停止" in msg:
                m["roam_on"] = False
                m["roam_last_ts"] = ts
    return m


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--since", type=int, default=DEFAULT_SINCE)
    ap.add_argument("--status-file", default="")
    ap.add_argument("--prev-status-file", default="")
    ap.add_argument("--json", default=os.path.join(ROOT, "tools", "_tmp_scan", "stuck_scan.json"))
    ap.add_argument("--max-accounts", type=int, default=0)
    args = ap.parse_args()

    status = None
    if args.status_file:
        status = json.load(open(args.status_file, "r", encoding="utf-8"))
    else:
        status = fetch_status(API_URL)
    if not status:
        print("无法获得 /api/status, 退出", file=sys.stderr)
        return 2

    prev = None
    if args.prev_status_file and os.path.exists(args.prev_status_file):
        try:
            prev = json.load(open(args.prev_status_file, "r", encoding="utf-8"))
        except Exception:
            prev = None

    robots = status.get("robots") or []
    by_acct = {}
    for r in robots:
        by_acct[r.get("account")] = r
    prev_by = {}
    for r in (prev.get("robots") if prev else []) or []:
        prev_by[r.get("account")] = r

    capped = {}
    for c in status.get("restore_capped") or []:
        capped[c.get("account")] = c.get("cap_reason") or ""

    npc_map = load_npc_positions(NPC_POS_XML)

    accounts = []
    for name in sorted(os.listdir(BOT_LOGS)):
        p = os.path.join(BOT_LOGS, name, "runs_20260928.log")
        if os.path.exists(p):
            accounts.append((name, p))
    if args.max_accounts:
        accounts = accounts[:args.max_accounts]
    print("[scan] 账号数 %d, since=%d, now=%d" % (len(accounts), args.since, NOW))

    results = []
    t0 = time.time()
    for i, (acct_dir, path) in enumerate(accounts):
        if i % 100 == 0:
            print("[scan] %d/%d (%.0fs)" % (i, len(accounts), time.time() - t0))
        m = scan_account_log(path, args.since)
        if m is None:
            continue
        acct = acct_dir  # 目录名=账号
        api = by_acct.get(acct) or {}
        pv = prev_by.get(acct) or {}
        gap = (NOW - m["last_ts"]) if m["last_ts"] else None
        state = api.get("state") or ""
        tags = []
        if acct in capped:
            tags.append("capped")
        if m["counts"].get("dst_reject") or m["counts"].get("dst_reject_retry"):
            tags.append("dst_reject")
        if m["counts"].get("task_stuck"):
            tags.append("task_stuck")
        if m["counts"].get("ghost_dialog_stuck") or m["counts"].get("dialog_stuck"):
            tags.append("dialog_stuck")
        if m["counts"].get("no_path") or m["counts"].get("no_route") or m["counts"].get("not_conn"):
            tags.append("no_path")
        if state == "WAIT_MAP":
            tags.append("booth_block")
        if state in ACTIVE_STATES and gap is not None and gap > SILENT_GAP_S:
            tags.append("silent")
        if state == "WAIT_GHOST":
            tags.append("wait_ghost")
        frozen = False
        if prev and pv and api:
            try:
                dt = NOW - int(prev.get("_ts") or 0)
                same = (list(api.get("pos_grid") or []) == list(pv.get("pos_grid") or []))
                if dt >= FROZEN_MIN_S and same and api.get("online") and pv.get("online"):
                    frozen = True
                    tags.append("frozen")
            except Exception:
                pass
        row = {
            "account": acct,
            "online": api.get("online"),
            "state": state,
            "mapid": api.get("mapid"),
            "pos_grid": api.get("pos_grid"),
            "level": api.get("level"),
            "paused": api.get("paused"),
            "fight": api.get("fight"),
            "gap_s": gap,
            "last_ts": m["last_ts"],
            "last_msg": m["last_msg"],
            "events_after": m["events"],
            "walk_cnt": m["walk_cnt"],
            "last_walk_ts": m["last_walk_ts"],
            "roam_on": m["roam_on"],
            "roam_target": m["roam_target"],
            "counts": dict(m["counts"]),
            "tags": tags,
            "capped_reason": capped.get(acct, ""),
            "last3": m["last3"],
        }
        if api and api.get("pos_grid") and tags:
            row["near_npc"] = nearest_npcs(npc_map, api.get("mapid"),
                                           api.get("pos_grid")[0] * 16, api.get("pos_grid")[1] * 16)
        results.append(row)

    # ---- 汇总 ----
    tag_cnt = collections.Counter()
    for r in results:
        for t in r["tags"]:
            tag_cnt[t] += 1
    online = [r for r in results if r.get("online")]
    interesting = [r for r in results if r["tags"] and "wait_ghost" not in r["tags"]]
    print("\n== 账号 %d（在线 %d）; 标签分布: %s" % (len(results), len(online), dict(tag_cnt.most_common())))
    print("== 非 wait_ghost 的疑似卡住 %d 个 ==" % len(interesting))
    for r in sorted(interesting, key=lambda x: -(x["gap_s"] or 0)):
        print("%s state=%s map=%s grid=%s gap=%ss roam=%s/%s tags=%s near=%s | last=%s" % (
            r["account"], r["state"], r["mapid"], r["pos_grid"],
            r["gap_s"], r["roam_on"], r["roam_target"], ",".join(r["tags"]),
            "; ".join(r.get("near_npc") or []), r["last_msg"][:70]))

    os.makedirs(os.path.dirname(args.json), exist_ok=True)
    with open(args.json, "w", encoding="utf-8") as f:
        json.dump({"_ts": NOW, "since": args.since, "tag_cnt": dict(tag_cnt),
                   "results": results}, f, ensure_ascii=False, indent=1)
    print("\n[json] %s" % args.json)
    print("[scan] done in %.0fs" % (time.time() - t0))
    return 0


if __name__ == "__main__":
    sys.exit(main())
