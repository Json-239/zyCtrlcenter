# -*- coding: utf-8 -*-
"""抓鬼计数口径 自检（2026-09-23 修复固化）。

现场缺陷：机器人本地 done_count **每轮 +2**。
  一只鬼会推两条 S2C_FINISH_TASK —— 打鬼任务(2019501~2019510/2019512/2019513)一条、
  交付任务(2019511)一条，两条都过 is_ghost_task() 进 on_finish_task；旧实现无条件
  `g.done_count += 1` → 每只鬼 +2。

服务端权威口径（config/task/20195.xml，本次修复的判据）：
  · 打鬼各条共用 share_daily_key=share_daily_捉鬼 + daily_limit=50 → **1 只鬼 1 次**；
  · 交付 2019511 是**另一个** key(share_daily_交付捉鬼, 20195.xml:641-643) → 不计入
    "捉鬼任务次数统计"。
  运行期服务端**不推** 90300 增量（全队实测 0 条）→ 本地计数是唯一的运行期依据。

满额判定生产链路（都读本地值）：daily_ghost.py:1782/1565 → 心跳 ghost.done(client.py)
  → Go autotask.go:587/633-646 ghostDailyFull、autotask.go:153 GhostDoneToday、
  roampool.go:55 → 号在服务端还有额度时提前"满额"停抓鬼转游荡（现场 196/408 号、317 次）。

本脚本（三段）：
  A 静态：源码断言（守卫存在 / 只有一处累加 / 常量正确 / 注释证据链完整）；
  B 动态：真执行 on_finish_task（mock 依赖）—— 打鬼+1、交付+0、一轮=+1、满额只在打鬼触发、
          跨夜仍归零；并把守卫改回旧实现做**变异测试**（旧码必须被本脚本抓住）；
  C 回放：用**真实生产日志**(data/bot_logs/*/runs_20260923.log) 重放
          (ts, 20195xx FINISH) + 登录校准读数，断言
            C1 两次校准之间 新码本地增量 == 服务端增量（旧码必然多计 = 本区间轮数）
            C2 一轮两条 FINISH 的前提（打鬼数 == 交付数 ±1）与代数关系
            C3 旧码 2× 在真实日志里可见（变异可抓 + 现场影响量化）

用法：python tools/ghost_count_selftest.py [script_dir] [log_dir]
"""
import io
import json
import os
import re
import sys
import types
import glob

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.normpath(os.path.join(HERE, ".."))
DEFAULT_DIR = os.path.join(ROOT, "deploy", "zones", "prod-240-2300", "script")
DEFAULT_LOG_DIR = os.path.join(ROOT, "data", "bot_logs")
script_dir = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_DIR
log_dir = sys.argv[2] if len(sys.argv) > 2 else DEFAULT_LOG_DIR

dh = open(os.path.join(script_dir, "daily_ghost.py"), encoding="utf-8").read()

fails = 0
total = 0


def check(name, ok, detail=""):
    global fails, total
    total += 1
    fails += 0 if ok else 1
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  " + detail) if detail else ""))


SUBMIT = 2019511
TODAY = "20260923"

# ================================================================ A 静态
n_add = len(re.findall(r"g\.done_count \+= 1", dh))
check("A1 全文件只有一处 done_count 累加", n_add == 1, "count=%d" % n_add)
check("A2 累加点受 '!= GHOST_SUBMIT_TASK' 守卫",
      re.search(r"(?m)^\tif int\(ti\) != GHOST_SUBMIT_TASK:\n\t\tg\.done_count \+= 1$", dh)
      is not None)
check("A3 GHOST_SUBMIT_TASK == 2019511",
      re.search(r"(?m)^GHOST_SUBMIT_TASK = 2019511\b", dh) is not None)
check("A4 计数段注释保留证据链（2× / share_daily_捉鬼 / 交付另 key）",
      "每轮 +2 → +1" in dh and "share_daily_捉鬼" in dh and "share_daily_交付捉鬼" in dh)
check("A5 is_ghost_task 覆盖含交付任务的 2019501~2019513",
      re.search(r"(?m)^def is_ghost_task\(ti\):\n\treturn ti is not None and "
                r"GHOST_TASK_MIN <= int\(ti\) <= GHOST_TASK_MAX$", dh) is not None)
check("A6 满额出口仍在（计满 → __emit_ghost_done）",
      "if g.done_count >= g.daily_limit:" in dh and "__emit_ghost_done(robot_object, g," in dh)

# ================================================================ B 动态（真执行）
m = re.search(r"(?ms)^def on_finish_task\(.*?(?=^\S)", dh)
check("B0 提取 on_finish_task", bool(m), "len=%d" % (len(m.group(0)) if m else 0))
SRC = m.group(0) if m else ""

# 旧实现（变异）：守卫去掉、无条件累加
LEGACY = SRC.replace("\tif int(ti) != GHOST_SUBMIT_TASK:\n\t\tg.done_count += 1",
                     "\tg.done_count += 1")
MUT_OK = (LEGACY != SRC and "\tif int(ti) != GHOST_SUBMIT_TASK:" not in LEGACY)


def build(src):
    ns = {"GHOST_SUBMIT_TASK": SUBMIT,
          "quest_engine": types.SimpleNamespace(_clear_recent_error=lambda q: None)}
    logs = []
    emits = []
    dones = []

    def _is_ghost_task(ti):
        try:
            return ti is not None and 2019501 <= int(ti) <= 2019513
        except Exception:
            return False

    def _set_state(g, s, *a):
        g.state = s

    ns.update({
        "is_ghost_task": _is_ghost_task,
        "__today_key": lambda: TODAY,
        "__log": lambda ro, lv, msg: logs.append(msg),
        "__emit": lambda ro, ev: emits.append(ev),
        "__set_state": _set_state,
        "__emit_state": lambda ro, g: None,
        "__emit_ghost_done": lambda ro, g, why: dones.append(why),
        "__quest": lambda ro: ro.m_quest,
    })
    exec(compile(src, "<on_finish_task>", "exec"), ns)  # noqa: S102 —— 自检专用
    return ns["on_finish_task"], logs, emits, dones


def mk_env(done=0, limit=50, cd=TODAY):
    g = types.SimpleNamespace(enabled=True, done_count=done, count_date=cd, daily_limit=limit,
                              submit_fail=1, abandon_fail_seen=2, ghost_wait_extend=3,
                              broker_menu_streak=4, broker_menu_fp="fp", task_index=0,
                              ghost_npc_id=0, state="SUBMIT")
    qu = types.SimpleNamespace(tasks={}, dialog_open=True, pending=object())
    ro = types.SimpleNamespace(m_ghost=g, m_quest=qu, m_account=["acc"], m_task_limited={})
    return g, ro


KILLS = [2019501, 2019505, 2019509, 2019510, 2019512, 2019513]  # 打鬼（含高阶）

if SRC:
    check("B-MUT 变异构造成功（旧实现 = 无条件累加）", MUT_OK)
    fn, logs, emits, dones = build(SRC)

    # B1 打鬼 FINISH 每次 +1（含高阶 2019512/2019513）
    g, ro = mk_env()
    for ti in KILLS:
        ro.m_quest.tasks[ti] = [ti, 0, 0, 0]
        fn(ro, [ti])
    check("B1 打鬼 FINISH 6 次 → done=6（含高阶 2019512/2019513）",
          g.done_count == 6, "done=%s" % g.done_count)

    # B2 交付 FINISH 不计数
    g, ro = mk_env(done=6)
    ro.m_quest.tasks[SUBMIT] = [SUBMIT, 0, 0, 0]
    fn(ro, [SUBMIT])
    check("B2 交付 FINISH(2019511) → done 不变(6)", g.done_count == 6,
          "done=%s" % g.done_count)

    # B3 一轮（打鬼+交付）= +1
    g, ro = mk_env(done=10)
    ro.m_quest.tasks[2019503] = [2019503, 0, 0, 0]
    fn(ro, [2019503])
    ro.m_quest.tasks[SUBMIT] = [SUBMIT, 0, 0, 0]
    fn(ro, [SUBMIT])
    check("B3 一轮(打鬼+交付) → +1 (10 → 11)", g.done_count == 11,
          "done=%s" % g.done_count)

    # B4 满额只在打鬼条上触发（交付条不触发 DONE/ghost_done）
    g, ro = mk_env(done=49)
    ro.m_quest.tasks[SUBMIT] = [SUBMIT, 0, 0, 0]
    fn(ro, [SUBMIT])
    ok_sub = (g.done_count == 49 and not dones and g.state != "DONE")
    g, ro = mk_env(done=49)
    ro.m_quest.tasks[2019507] = [2019507, 0, 0, 0]
    fn(ro, [2019507])
    ok_kill = (g.done_count == 50 and len(dones) == 1 and g.state == "DONE")
    check("B4 满额只在打鬼条触发（交付 49→49 不停; 打鬼 49→50 停）",
          ok_sub and ok_kill, "sub_ok=%s kill_ok=%s done=%s" % (ok_sub, ok_kill, g.done_count))

    # B5 非抓鬼任务/未启用 不受影响
    g, ro = mk_env(done=7)
    fn(ro, [3005])
    g.enabled = False
    ro.m_quest.tasks[2019502] = [2019502, 0, 0, 0]
    fn(ro, [2019502])
    check("B5 非抓鬼任务/未启用 → 不计数", g.done_count == 7, "done=%s" % g.done_count)

    # B6 跨夜仍归零（先归零再按打鬼 +1）
    g, ro = mk_env(done=48, cd="20260922")
    ro.m_quest.tasks[2019504] = [2019504, 0, 0, 0]
    fn(ro, [2019504])
    check("B6 跨夜归零后 +1: 48 → 1（并打重置日志）",
          g.done_count == 1 and g.count_date == TODAY
          and any("跨夜重置" in x for x in logs), "done=%s" % g.done_count)

    # B7 变异测试：旧实现（无条件累加）必须被抓住 —— 一轮变 +2
    lfn, _l, _e, _d = build(LEGACY)
    g, ro = mk_env(done=10)
    ro.m_quest.tasks[2019503] = [2019503, 0, 0, 0]
    lfn(ro, [2019503])
    ro.m_quest.tasks[SUBMIT] = [SUBMIT, 0, 0, 0]
    lfn(ro, [SUBMIT])
    check("B7 变异测试: 旧实现一轮 → +2（自检能抓住 2× 回归）", g.done_count == 12,
          "done=%s" % g.done_count)

# ================================================================ C 回放（真实生产日志）
def load_log(path):
    """返回事件序列 [(ts, kind, val)]；kind: KILL/SUB=完成事件(val=当时本地 done 打印值),
    CALIB=登录校准读数(val=服务端 task_limited 值)。"""
    ev = []
    with io.open(path, encoding="utf-8", errors="replace") as f:
        for line in f:
            if "抓鬼完成" not in line and "今日抓鬼数校准" not in line:
                continue
            try:
                e = json.loads(line)
            except Exception:
                continue
            msg = e.get("msg") or ""
            ts = e.get("ts") or 0
            if msg.startswith("抓鬼完成"):
                mm = re.match(r"抓鬼完成\s+(\d+)/50\s*\(\s*第\s*(\d+)\s*次\)", msg)
                if mm:
                    ti = int(mm.group(2))
                    ev.append((ts, "SUB" if ti == SUBMIT else "KILL", int(mm.group(1))))
            elif msg.startswith("今日抓鬼数校准"):
                mm = re.search(r":\s*(\d+)/", msg)
                if mm:
                    ev.append((ts, "CALIB", int(mm.group(1))))
    ev.sort(key=lambda x: x[0])
    return ev


def replay(ev):
    """按事件序列推演**新码**本地 done（= 只数打鬼）。
    返回 intervals=[(Δsrv, Δlocal, Δkill, Δsub)], local_final, kills, subs, max_logged_local。"""
    intervals = []
    local = 0
    kills = subs = 0
    srv0 = None
    l0 = 0
    k0 = s0 = 0
    max_logged = 0
    for ts, kind, val in ev:
        if kind in ("KILL", "SUB"):
            kills += 1 if kind == "KILL" else 0
            subs += 1 if kind == "SUB" else 0
            if val > max_logged:
                max_logged = val
            if kind == "KILL":
                local += 1
        elif kind == "CALIB":
            if srv0 is not None:
                intervals.append((val - srv0, local - l0, kills - k0, subs - s0))
            srv0 = val
            local = val
            l0 = val
            k0 = kills
            s0 = subs
    return intervals, local, kills, subs, max_logged


def over_count_violations(intervals, mode):
    """同一批校准区间, 分别用新码/旧码的增量算法算"多计"区间数。
    不变量: 区间内本地增量不得超过服务端增量 +1（快照时机边界）。
      · 新码 dlocal = 打鬼数 → 0 违规；
      · 旧码 dlocal = 打鬼数 + 交付数 ≈ 2×轮数 → 必然违规（这就是变异可抓）。
    dsrv < 0 的区间（跨夜 0 点服务端归零）不参与。"""
    n = 0
    for dsrv, _dnew, dkill, dsub in intervals:
        if dsrv < 0:
            continue
        dlocal = (dkill + dsub) if mode == "old" else dkill
        if dlocal - dsrv > 1:
            n += 1
    return n


log_files = sorted(glob.glob(os.path.join(log_dir, "*", "runs_20260923.log")))
if not log_files:
    check("C0 生产日志存在", False, log_dir)
else:
    n_acct = 0
    i_tot = 0
    v_new = v_old = 0
    v_new_accts = []
    pairs_ok = pairs_bad = 0
    tot_k = tot_s = 0
    old_2x_accts = 0
    drift_all = []
    field_prem = []          # 现场"假满额"：旧码本地踩满，而下一次登录读数 <50
    for p in log_files:
        ev = load_log(p)
        if not any(k in ("KILL", "SUB") for _, k, _ in ev):
            continue
        n_acct += 1
        iv, _local_new, kills, subs, max_logged = replay(ev)
        i_tot += len(iv)
        nv = over_count_violations(iv, "new")
        v_new += nv
        v_old += over_count_violations(iv, "old")
        if nv:
            v_new_accts.append(os.path.basename(os.path.dirname(p)).split("@")[0])
        # C2 一轮两条 FINISH 的前提: 打鬼数 ≈ 交付数（号级 ±1 为主, 少数因重启漏记事件）
        tot_k += kills
        tot_s += subs
        if abs(kills - subs) <= 1:
            pairs_ok += 1
        else:
            pairs_bad += 1
        # C3 现场: 旧码日志里的本地最大值 明显超过服务端终值(2× 量级) → 日志侧也能看见缺陷
        srv_final = 0
        for ts, k, v in ev:
            if k == "CALIB":
                srv_final = v
        if max_logged >= 2 * max(srv_final - 1, 1) and max_logged >= 10:
            old_2x_accts += 1
        # C3b 现场"假满额"硬证据: 本地踩到 limit(50) 之后的下一次登录读数 < 50
        hit = [ts for ts, k, v in ev if k in ("KILL", "SUB") and v >= 50]
        for ts in hit:
            nxt = next((v for t, k, v in ev if k == "CALIB" and t > ts), None)
            if nxt is not None and nxt < 50:
                field_prem.append((os.path.basename(os.path.dirname(p)).split("@")[0], nxt))
                break
        # C4 现场漂移(信息项): 真日志"区间末本地打印值 - 该次登录校准值"
        #    修复前 = 本区间轮数(2×), 修复后新日志会自然归零 → 仅 INFO, 不做硬断言
        last_logged = None
        for ts, k, v in ev:
            if k in ("KILL", "SUB"):
                last_logged = v
            elif k == "CALIB" and last_logged is not None:
                drift_all.append(last_logged - v)

    drift_all.sort()
    med_drift = drift_all[len(drift_all) // 2] if drift_all else 0
    ratio = (float(tot_k) / tot_s) if tot_s else 0.0
    check("C0 生产日志可解析的号数 >0", n_acct > 0, "accounts=%d" % n_acct)
    # 容忍度 ≤0.5%: 极少数号的服务端计数不随本号轮数增长（实测 robot0005195 04:01:05
    #   完成 20 轮后登录读数 f=0/r=5 —— 队号/服务端记账异常，非本地多计）。
    #   旧码在同一条不变量下违规 1200+ 区间（1.5 个数量级差距）→ 判别力不受影响。
    check("C1 校准区间不变量(新码): 本地增量 <= 服务端增量+1（违规率<=0.5%）",
          i_tot > 0 and v_new <= 0.005 * i_tot,
          "区间 %d 个, 违规 %d 个(%.2f%%) %s" % (
              i_tot, v_new, 100.0 * v_new / max(i_tot, 1), v_new_accts[:3]))
    check("C1b 变异(旧码)在同一不变量下必然违规 → 旧码会被本自检抓住",
          v_old > 0, "旧码违规区间 %d 个" % v_old)
    check("C2 前提: 一轮两条 FINISH（号级 |打鬼-交付|<=1 占比 >=80%, 全队差 <=5%）",
          pairs_ok > 0 and pairs_bad <= 0.2 * (pairs_ok + pairs_bad) and abs(1 - ratio) <= 0.05,
          "±1 号 %d/%d; 全队 打鬼%d 交付%d 比值%.3f" % (
              pairs_ok, pairs_ok + pairs_bad, tot_k, tot_s, ratio))
    check("C3 现场日志侧可见旧码 2× 量级（变异可抓）", old_2x_accts > 0,
          "符合 2× 量级的号 %d 个" % old_2x_accts)
    check("C3b 现场硬证据: 存在'本地踩满 50 而下次登录读数 <50'的号（修复前假满额）",
          len(field_prem) > 0, "号数 %d, 例: %s" % (len(field_prem), field_prem[:3]))
    print("[INFO] C4 现场漂移(区间末本地值-校准值) 中位 %d, >=2 占 %.0f%%（修复前 = 每区间轮数;"
          " 修复后新日志应自然归零）" % (
              med_drift, 100.0 * sum(1 for d in drift_all if d >= 2) / max(len(drift_all), 1)))

# ================================================================ 汇总
print("\n自检目标: %s" % script_dir)
print("日志目录: %s" % log_dir)
print("结果：%d 项，失败 %d 项" % (total, fails))
sys.exit(1 if fails else 0)
