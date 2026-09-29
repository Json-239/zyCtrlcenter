# -*- coding: utf-8 -*-
"""残留抓鬼任务出口 自检 —— 2026-09-24

现场缺陷（生产取证，2026-09-23/24 两天 480 个号出现、单号最多 22 次被拒/天）:
  号手里压着**打鬼任务**(2019501~2019513)、目标图持续无鬼，想"放弃重接"却被服务端拒
  （config/task/20195.xml `action_ticket_drop_task.money_count=500`，机器人号银两只几十~几百）
  → 无限循环:
    等刷鬼 6 轮(约 3 分钟) → 回 24 点钟馗"放弃捉鬼任务" → 无 DROP_TASK(被拒) →
    关对话回 READY → 本地任务仍在 → 等刷鬼 6 轮 → 再被拒 …；
  被拒 2 次后 `abandon_tries >= ABANDON_FAIL_LIMIT` → 走**缓存恢复**把**同一个任务**
  装回本地（`__recover_task_from_cache`）→ 又等刷鬼 6 轮 → …（同一任务永不释放）。
  后果（样本 robot0005232@xy3.com 2026-09-23 19:04~19:19、robot0001131@xy3.com
  2026-09-24 00:08~00:30+ 实证）: 号全天空转（等鬼/换图/往返 24），
  且 21:24 意图切 newbie（等级 30 < 31）期间抓鬼循环不停 → 低等级号进不了新手链。

修复（daily_ghost.py，最小改动、幂等、有观测）:
  ① 判据 `__should_leftover_stop`:
       放弃被拒 ≥ GHOST_LEFTOVER_FAIL_MIN(2) 已成事实
       + 本会话连续无鬼 ≥ GHOST_LEFTOVER_ROUNDS(12 轮 ≈ 6 分钟) → 判该任务本会话不可完成。
  ② 出口 `__enter_leftover_stop`（幂等，只做一次）:
       · **保留**本地任务与 _TASK_CACHE（与服务端状态一致，不制造"本地无记录"新循环）；
       · 清动态导航/对话 → 置 DONE 停状态机推进（不再往返 24/换图巡逻）；
       · 上报 error `GHOST_LEFTOVER_TASK`（含 task_index/被拒次数/无鬼轮数）供中控处置
         （停链/换号/重登；<31 级号随之可被派新手链）；
       · 重登/重新下发 ghost_start 时 reset() 自然解除。
  ③ 计数与清零: `no_ghost_rounds` 在等刷鬼超时处 +1；发现鬼/换新任务/交付完成/放弃成功清零。

本脚本四段:
  A 静态: 源码形状（常量/字段/函数/调用点顺序/清零点/不动任务与缓存/既有路径保留）；
  B 动态: 真执行 `__should_leftover_stop` / `__enter_leftover_stop` ——
         复现样本（被拒2次+12轮无鬼）→ 收敛；反例（从没被拒/不够久）不触发；
         幂等（二次调用不重复清导航/不重复上报）；事件字段完整；
  C 回放: 用**真实生产日志**重放被拒/无鬼/进展事件，断言
         C1 样本号在日志覆盖期内**首次命中**（列出时刻）；
         C2 命中时刻之后生产日志仍在循环（= 旧逻辑无出口的证据，量化为浪费）；
         C3 正常号（放弃成功/无被拒）全程**从不命中**（反例）。
  D 兼容: pay_reduce 降频闸/回落路径原文保留；出口在降频闸之前（先拦）；不改协议面。

用法: python tools/ghost_leftover_selftest.py [script_dir] [log_dir]
"""
import io
import json
import os
import re
import sys
import time

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.normpath(os.path.join(HERE, ".."))
DEFAULT_DIR = os.path.join(ROOT, "deploy", "zones", "prod-240-2300", "script")
DEFAULT_LOG_DIR = os.path.join(ROOT, "data", "bot_logs")

results = []


def check(name, cond, detail=""):
    results.append((name, bool(cond), detail))


def _resolve(path):
    if os.path.isdir(path):
        return path
    return os.path.dirname(path)


def _extract_func(src, name):
    lines = src.splitlines()
    for i, ln in enumerate(lines):
        if ln.startswith("def %s(" % name):
            out = [ln]
            for ln2 in lines[i + 1:]:
                if ln2.strip() and not ln2[0].isspace():
                    break
                out.append(ln2)
            return "\n".join(out)
    return None


def _block_between(src, start_marker, end_marker):
    i = src.find(start_marker)
    if i < 0:
        return None
    j = src.find(end_marker, i)
    if j < 0:
        return None
    return src[i:j]


class _G(object):
    """GhostState 最简替身。"""

    def __init__(self, **kw):
        for k, v in kw.items():
            setattr(self, k, v)


class _Quest(object):
    """quest 最简替身: 只带出口会清的导航字段 + tasks(断言保留)。"""

    def __init__(self):
        self.dynamic_npcs = {"1297223": [25, 100, 100]}
        self.pending = {"type": "walk"}
        self.walk_target = (1, 2)
        self.dijkstra_route = [1, 2, 3]
        self.dijkstra_final = {"npc_id": 10146}
        self.dijkstra_waiting = True
        self.dijkstra_jumper = 1
        self.walk_pending_hop = (1, 2)
        self.walk_pending_click = {"npc_id": 10146}
        self.path_points = [(1, 2)]
        self.timeout_override_ms = 5000
        self.tasks = {2019501: [2019501, 0, 10146, 0, 0, []]}


class _Robot(object):
    def __init__(self, acct="robot0005232@xy3.com"):
        self.m_account = (acct,)


def _load_dynamic(dh):
    """抽取 2 个新函数 + 常量，注入 mock 依赖，返回可执行命名空间与调用记录。"""
    ns = {}
    for m in re.finditer(r"(?m)^(GHOST_LEFTOVER_(?:FAIL_MIN|ROUNDS))\s*=\s*(\d+)", dh):
        ns[m.group(1)] = int(m.group(2))
    m = re.search(r"(?m)^GHOST_LEFTOVER_CODE\s*=\s*\"([^\"]+)\"", dh)
    if m:
        ns["GHOST_LEFTOVER_CODE"] = m.group(1)

    calls = {"log": [], "emit": [], "state": [], "emit_state": 0}

    def _set_state(g, state, now_ms=None):
        g.state = state
        g.state_since_ms = now_ms if now_ms is not None else 0

    ns["__set_state"] = _set_state
    ns["__log"] = lambda ro, lvl, msg: calls["log"].append((lvl, msg))
    ns["__emit"] = lambda ro, ev: calls["emit"].append(ev)
    ns["__emit_state"] = lambda ro, g: calls.__setitem__("emit_state", calls["emit_state"] + 1)

    ok = True
    for _name in ("__should_leftover_stop", "__enter_leftover_stop"):
        frag = _extract_func(dh, _name)
        if not frag:
            ok = False
            continue
        try:
            exec(compile(frag, "<daily_ghost.%s>" % _name, "exec"), ns)
        except Exception as e:  # noqa
            check("exec daily_ghost.%s" % _name, False, str(e))
            ok = False
    return ns, calls, ok


# ------------------------------------------------------------------ 日志回放
_TS = re.compile(r"\"ts\":(\d+)")


def _iter_log(path):
    try:
        f = io.open(path, encoding="utf-8", errors="replace")
    except Exception:
        return
    for ln in f:
        ln = ln.strip()
        if not ln:
            continue
        try:
            d = json.loads(ln)
        except Exception:
            continue
        ts = d.get("ts")
        if not isinstance(ts, (int, float)):
            continue
        yield ts, (d.get("msg") or "")


def _replay(path, should_stop, rounds_min):
    """重放一个号的一天日志: 维护 fail_seen / no_ghost_rounds, 在每个"等刷鬼 6 轮"
    检查点上评估判据。返回 dict(checks=[(ts, fail, no, hit)], n_reject, n_loop,
    first_hit=(ts,fail,no) or None, ...)。

    事件语义与实现对齐:
      · "记被拒第 N 次"           → fail_seen = N
      · "等刷鬼超时, 附近巡逻(第 N 轮)" / "等刷鬼 N 轮无鬼..." → no += 1
        （no_ghost_rounds 在实现里是**独立累计**, 不随 g.rounds 回退/清零 ——
          日志里的 N 是 g.rounds, 会回退到 3, 不能直接当 no 用）
      · "等刷鬼 N 轮仍无鬼..."     → 检查点(此刻按 rounds>=min 判), no 按当前累计值
      · 发现鬼/抓鬼完成/接到捉鬼任务/任务已放弃/交付完成 → no = 0
      · "用缓存恢复..."            → **不清**(实现如此: 同一任务装回本地, 无进展)
    """
    g = _G(abandon_fail_seen=0, no_ghost_rounds=0, leftover_stop=False)
    checks = []
    n_reject = n_loop = 0
    first_hit = None
    last_ts = None
    for ts, msg in _iter_log(path):
        last_ts = ts
        m = re.search(r"记被拒第 (\d+) 次", msg)
        if m:
            g.abandon_fail_seen = int(m.group(1))
            n_reject += 1
            continue
        if re.search(r"等刷鬼超时, 附近巡逻\(第 \d+ 轮\)", msg):
            g.no_ghost_rounds += 1
            continue
        if re.search(r"等刷鬼 \d+ 轮无鬼, 当前图继续等/巡逻", msg):
            g.no_ghost_rounds += 1
            continue
        if "等刷鬼" in msg and "轮仍无鬼" in msg:
            n_loop += 1
            hit = bool(should_stop(g))
            checks.append((ts, g.abandon_fail_seen, g.no_ghost_rounds, hit))
            if hit and first_hit is None:
                first_hit = (ts, g.abandon_fail_seen, g.no_ghost_rounds)
            continue
        if ("发现鬼 NPC" in msg or "抓鬼完成" in msg or "接到捉鬼任务" in msg
                or "已放弃, 回 READY" in msg or "完成, 去钟馗交付" in msg):
            g.no_ghost_rounds = 0
            continue
    return {"checks": checks, "n_reject": n_reject, "n_loop": n_loop,
            "first_hit": first_hit, "last_ts": last_ts}


def main():
    script_dir = _resolve(sys.argv[1] if len(sys.argv) > 1 else DEFAULT_DIR)
    log_dir = sys.argv[2] if len(sys.argv) > 2 else DEFAULT_LOG_DIR
    dh_path = os.path.join(script_dir, "daily_ghost.py")
    if not os.path.exists(dh_path):
        print("[FAIL] 找不到 %s" % dh_path)
        return 2
    dh = io.open(dh_path, encoding="utf-8", errors="replace").read()

    # ============================================================ A. 源码形状
    check("A1 常量 GHOST_LEFTOVER_FAIL_MIN = 2",
          re.search(r"(?m)^GHOST_LEFTOVER_FAIL_MIN = 2\b", dh) is not None)
    check("A2 常量 GHOST_LEFTOVER_ROUNDS = 12",
          re.search(r"(?m)^GHOST_LEFTOVER_ROUNDS = 12\b", dh) is not None)
    check("A3 错误码 GHOST_LEFTOVER_CODE = \"GHOST_LEFTOVER_TASK\"",
          re.search(r"(?m)^GHOST_LEFTOVER_CODE = \"GHOST_LEFTOVER_TASK\"", dh) is not None)
    check("A4 注释含现场证据链(被拒 500 银两 / 缓存恢复 / 新手链)",
          "money_count" in dh and "缓存恢复" in dh and "新手链" in dh
          and "480" in dh)

    init_block = _block_between(dh, "class GhostState(object):", "\tdef reset(self):")
    check("A5 GhostState.__init__ 有 no_ghost_rounds 字段",
          init_block is not None and "self.no_ghost_rounds = 0" in init_block)
    check("A6 GhostState.__init__ 有 leftover_stop 字段",
          init_block is not None and "self.leftover_stop = False" in init_block)

    reset_block = _block_between(dh, "\tdef reset(self):", "\ndef __now_ms():")
    check("A7 reset() 清零 no_ghost_rounds(新会话重判)",
          reset_block is not None and "self.no_ghost_rounds = 0" in reset_block)
    check("A8 reset() 清零 leftover_stop(重登/重新下发即解除冻结)",
          reset_block is not None and "self.leftover_stop = False" in reset_block)

    check("A9 定义 __should_leftover_stop", "def __should_leftover_stop(g):" in dh)
    check("A10 定义 __enter_leftover_stop",
          "def __enter_leftover_stop(robot_object, g, quest, now_ms):" in dh)

    # 调用点: 在 WAIT_GHOST rounds>=6 的 _has_hunt 分支内、降频闸之前
    stop_pos = dh.find("if __should_leftover_stop(g):")
    skip_pos = dh.find("if __should_skip_broker_abandon(g):")
    abandon_true_pos = dh.find("g.abandon_task = True")
    has_hunt_pos = dh.find("if _has_hunt:")
    check("A11 出口调用位于 _has_hunt 分支内",
          stop_pos > 0 and has_hunt_pos > 0 and has_hunt_pos < stop_pos)
    check("A12 出口在降频闸之前(先拦: 不再白跑 24)",
          stop_pos > 0 and skip_pos > 0 and stop_pos < skip_pos)
    check("A13 出口在 g.abandon_task = True(回 24) 之前",
          stop_pos > 0 and abandon_true_pos > 0 and stop_pos < abandon_true_pos)
    stop_block = _block_between(dh, "if __should_leftover_stop(g):",
                                "# 2026-09-23 付费降量闸")
    check("A14 出口分支命中即 return(不落原回落路径)",
          stop_block is not None and "return 0" in stop_block)

    # 计数点: 等刷鬼超时处 +1, 且只此一处累加
    n_inc = len(re.findall(r"g\.no_ghost_rounds = int\(getattr\(g, \"no_ghost_rounds\", 0\)\) \+ 1", dh))
    check("A15 no_ghost_rounds 只有一处累加(等刷鬼超时处)", n_inc == 1, "count=%d" % n_inc)
    inc_block = _block_between(dh, "if __timeout(g):", "if g.rounds >= 6:")
    check("A16 累加在 WAIT_GHOST 超时块内(30s/轮)",
          inc_block is not None and "no_ghost_rounds" in inc_block)

    # 清零点: 发现鬼 / 换新任务 / 交付完成 / 放弃成功
    ghost_block = _block_between(dh, "gid = __find_ghost_npc(robot_object)",
                                 '__log(robot_object, "info", "发现鬼 NPC')
    check("A17 发现鬼清零(服务端仍在刷鬼 = 有进展)",
          ghost_block is not None and "g.no_ghost_rounds = 0" in ghost_block)
    add_block = _block_between(dh, "def on_add_task(", "def on_finish_task(")
    check("A18 on_add_task 换新打鬼任务/交付任务到达时清零",
          add_block is not None and add_block.count("g.no_ghost_rounds = 0") >= 2,
          "count=%d" % (add_block.count("g.no_ghost_rounds = 0") if add_block else -1))
    fin_block = _block_between(dh, "def on_finish_task(", "def on_drop_task(")
    check("A19 on_finish_task(抓到鬼/交付完成)清零",
          fin_block is not None and "g.no_ghost_rounds = 0" in fin_block)
    drop_block = _block_between(dh, "def on_drop_task(", "def on_show_dialog(")
    check("A20 on_drop_task(放弃成功)清零",
          drop_block is not None and "g.no_ghost_rounds = 0" in drop_block)
    rec_block = _block_between(dh, "def __recover_task_from_cache(",
                               "def __is_low_hp(")
    check("A21 缓存恢复**不**清零(同一任务装回 = 无进展, 否则出口永不触发)",
          rec_block is not None and "no_ghost_rounds" not in rec_block)

    # 出口动作: 保留任务与缓存 / 清导航 / 置 DONE / 上报一次
    enter_block = _block_between(dh, "def __enter_leftover_stop(",
                                 "def __task_fingerprint(")
    check("A22 出口不清 quest.tasks(与服务端状态一致)",
          enter_block is not None and "tasks.pop" not in enter_block and "tasks[" not in enter_block)
    enter_code = "\n".join(ln for ln in (enter_block or "").splitlines()
                           if not ln.strip().startswith("#"))
    check("A23 出口不碰 _TASK_CACHE(缓存留给重登后原样恢复)",
          "_TASK_CACHE" not in enter_code)
    check("A24 出口清动态导航(dynamic_npcs/pending/dijkstra/walk)",
          enter_block is not None and all(k in enter_block for k in
              ("quest.dynamic_npcs = {}", "quest.pending = None", "quest.walk_target = None",
               "quest.dijkstra_route = []", "quest.dijkstra_final = None")))
    check("A25 出口置 DONE 停状态机推进",
          enter_block is not None and '__set_state(g, "DONE", now_ms)' in enter_block)
    check("A26 出口上报 error + GHOST_LEFTOVER_CODE(带 task_index/被拒/无鬼轮数)",
          enter_block is not None and all(k in enter_block for k in
              ('"type": "error"', "GHOST_LEFTOVER_CODE", '"task_index"',
               '"abandon_fail_seen"', '"no_ghost_rounds"')))
    check("A27 出口幂等: 已冻结直接返回(不重复清/不重复报)",
          enter_block is not None and 'if bool(getattr(g, "leftover_stop", False)):' in enter_block
          and "return True" in enter_block)
    check("A28 出口有明确日志(含'停止空转')",
          enter_block is not None and "停止空转" in enter_block)

    # 既有路径保留
    check("A29 pay_reduce 降频闸原文保留",
          "def __should_skip_broker_abandon(g):" in dh
          and "GHOST_WAIT_EXTEND_MAX" in dh and "ghost_wait_extend" in dh)
    check("A30 原回落路径(有 hunt → 回钟馗放弃)保留",
          "g.abandon_task = True" in dh and "去钟馗放弃重接" in dh)
    check("A31 原被拒记忆清理(on_drop/on_finish)保留",
          drop_block is not None and "g.abandon_fail_seen = 0" in drop_block
          and fin_block is not None and "g.abandon_fail_seen = 0" in fin_block)
    check("A32 出口不改协议面(只用既有 __emit/__log/__emit_state)",
          enter_block is not None and "send_message" not in enter_block
          and "protocol" not in enter_block)
    check("A33 出口停用会话(g.enabled = False): 中控不再当'抓鬼在跑', 孵化/游荡池可接手",
          enter_block is not None and "g.enabled = False" in enter_block)

    # ============================================================ B. 行为测试
    ns, calls, ok = _load_dynamic(dh)
    should = ns.get("__should_leftover_stop")
    enter = ns.get("__enter_leftover_stop")
    check("B0 常量注入: GHOST_LEFTOVER_FAIL_MIN=2 / ROUNDS=12 / CODE",
          ok and ns.get("GHOST_LEFTOVER_FAIL_MIN") == 2
          and ns.get("GHOST_LEFTOVER_ROUNDS") == 12
          and ns.get("GHOST_LEFTOVER_CODE") == "GHOST_LEFTOVER_TASK")

    if should is not None:
        # 样本复现: 被拒 2 次 + 连续无鬼 12 轮 → 命中
        check("B1 复现样本: fail=2, no_ghost=12 → True(收敛)",
              should(_G(abandon_fail_seen=2, no_ghost_rounds=12, leftover_stop=False)) is True)
        check("B2 样本上游: fail=2, no_ghost=11 → False(还不够久)",
              should(_G(abandon_fail_seen=2, no_ghost_rounds=11, leftover_stop=False)) is False)
        check("B3 样本上游: fail=1, no_ghost=99 → False(被拒未成事实)",
              should(_G(abandon_fail_seen=1, no_ghost_rounds=99, leftover_stop=False)) is False)
        # 反例: 正常"等刷鬼"(从没被拒)不受影响
        check("B4 反例·正常等刷鬼: fail=0, no_ghost=60 → False",
              should(_G(abandon_fail_seen=0, no_ghost_rounds=60, leftover_stop=False)) is False)
        # 反例: 放弃成功路径(fail 被清)不受影响
        check("B5 反例·放弃成功: fail=0(已清), no_ghost=30 → False",
              should(_G(abandon_fail_seen=0, no_ghost_rounds=30, leftover_stop=False)) is False)
        # 幂等: 已冻结不再触发
        check("B6 幂等: leftover_stop=True → False(不再触发)",
              should(_G(abandon_fail_seen=9, no_ghost_rounds=99, leftover_stop=True)) is False)
        # 向后兼容: 旧状态对象缺字段
        check("B7 兼容: 缺字段(旧状态对象) → False",
              should(type("X", (), {})()) is False)

    if enter is not None:
        # 首次进入: 保留任务/缓存, 清导航, 置 DONE, 上报一次
        g = _G(abandon_fail_seen=2, no_ghost_rounds=12, leftover_stop=False,
               task_index=2019501, done_count=7, daily_limit=50, state="WAIT_GHOST",
               state_since_ms=0, ghost_npc_id=12345)
        q = _Quest()
        ro = _Robot()
        calls["log"][:] = []
        calls["emit"][:] = []
        calls["emit_state"] = 0
        r1 = enter(ro, g, q, 1790000000000)
        check("B8 首次进入返回 True", r1 is True)
        check("B9 置 leftover_stop=True", g.leftover_stop is True)
        check("B10 状态置 DONE(停状态机)", g.state == "DONE")
        check("B10b 停用会话: g.enabled is False(中控不再当'抓鬼在跑')",
              getattr(g, "enabled", True) is False)
        check("B11 任务表**保留**(与服务端一致)", 2019501 in q.tasks)
        check("B12 动态导航已清(不再巡逻/换图)",
              q.dynamic_npcs == {} and q.pending is None and q.walk_target is None
              and q.dijkstra_route == [] and q.dijkstra_final is None
              and q.dijkstra_waiting is False and q.walk_pending_hop is None
              and q.walk_pending_click is None and q.path_points == [])
        check("B13 上报恰一次", len(calls["emit"]) == 1)
        ev = calls["emit"][0] if calls["emit"] else {}
        check("B14 事件形状: type=error / code=GHOST_LEFTOVER_TASK",
              ev.get("type") == "error" and ev.get("code") == "GHOST_LEFTOVER_TASK")
        check("B15 事件带处置所需字段",
              ev.get("account") == "robot0005232@xy3.com" and ev.get("task_index") == 2019501
              and ev.get("abandon_fail_seen") == 2 and ev.get("no_ghost_rounds") == 12
              and ev.get("done") == 7 and ev.get("limit") == 50)
        check("B16 状态已上报(__emit_state 一次)", calls["emit_state"] == 1)
        check("B17 日志含'停止空转'与口径数字",
              any("停止空转" in m and "2" in m and "12" in m for _l, m in calls["log"]))
        # 二次进入: 幂等(不重复上报/不重复动作)
        q2 = _Quest()
        q2.dynamic_npcs = {}
        q2.pending = None
        q2.walk_target = None
        q2.dijkstra_route = []
        q2.dijkstra_final = None
        q2.dijkstra_waiting = False
        q2.walk_pending_hop = None
        q2.walk_pending_click = None
        q2.path_points = []
        g2 = _G(abandon_fail_seen=2, no_ghost_rounds=12, leftover_stop=True,
                task_index=2019501, done_count=7, daily_limit=50, state="DONE",
                state_since_ms=0, ghost_npc_id=0)
        calls["emit"][:] = []
        calls["emit_state"] = 0
        r2 = enter(ro, g2, q2, 1790009999000)
        check("B18 二次进入返回 True(幂等)", r2 is True)
        check("B19 二次进入不重复上报", len(calls["emit"]) == 0)
        check("B20 二次进入不重复上报状态", calls["emit_state"] == 0)
        # 反例: 不该触发的号不会进(直接断言判据 + 出口前置)
        g3 = _G(abandon_fail_seen=0, no_ghost_rounds=60, leftover_stop=False,
                task_index=2019501, done_count=7, daily_limit=50, state="WAIT_GHOST",
                state_since_ms=0, ghost_npc_id=0)
        check("B21 反例·正常等刷鬼: 判据 False → 出口不被调用",
              should(g3) is False)

    # ============================================================ C. 日志回放
    c1_path = os.path.join(log_dir, "robot0001131@xy3.com", "runs_20260924.log")
    c3_path = os.path.join(log_dir, "robot0001034@xy3.com", "runs_20260923.log")
    if should is not None and os.path.exists(c1_path):
        rp = _replay(c1_path, should, 6)
        hit = rp["first_hit"]
        tstr = time.strftime("%H:%M:%S", time.localtime(hit[0])) if hit else "-"
        check("C1 样本号(robot0001131 09-24)在日志覆盖期内首次命中",
              hit is not None,
              "first_hit=%s fail=%s no_ghost=%s" % (tstr, hit[1] if hit else "-",
                                                    hit[2] if hit else "-"))
        if hit:
            # 命中之后生产日志仍在循环 = 旧逻辑无出口的证据
            after = [c for c in rp["checks"] if c[0] > hit[0]]
            n_rej_after = sum(1 for t, m in _iter_log(c1_path)
                              if t > hit[0] and "记被拒第" in m)
            check("C2 命中发生在被拒第 3 次之前(收敛提前于生产现状)",
                  (hit[1] if hit else 0) <= 2, "fail_at_hit=%s" % (hit[1] if hit else "-"))
            check("C2b 命中后生产日志仍在空转(旧逻辑浪费证据)",
                  len(after) >= 3, "loop_checks_after_hit=%d" % len(after))
            check("C2c 命中后被拒次数(旧逻辑本可省下的往返)",
                  n_rej_after >= 5, "rejects_after_hit=%d" % n_rej_after)
            check("C2d 该号全天被拒次数 ≥ 10(量级核对)",
                  rp["n_reject"] >= 10, "n_reject=%d" % rp["n_reject"])
    else:
        check("C1/C2 样本号日志存在(robot0001131@xy3.com)", False, c1_path)

    if should is not None and os.path.exists(c3_path):
        rp2 = _replay(c3_path, should, 6)
        check("C3 反例·正常号(robot0001034 09-23, 放弃成功/无被拒)全程不命中",
              rp2["first_hit"] is None,
              "loops=%d rejects=%d" % (rp2["n_loop"], rp2["n_reject"]))
        check("C3b 反例号确实有抓鬼推进(检查点非空)",
              rp2["n_loop"] > 0, "loop_checks=%d" % rp2["n_loop"])

    # C4 多号口径: 另两个现场号也在第二圈收敛(命中早于第 3 次被拒)
    for acct, day in (("robot0005232@xy3.com", "runs_20260923.log"),
                      ("robot0001130@xy3.com", "runs_20260923.log")):
        p = os.path.join(log_dir, acct, day)
        if should is None or not os.path.exists(p):
            check("C4 现场号日志存在(%s %s)" % (acct, day), False, p)
            continue
        rp3 = _replay(p, should, 6)
        hit = rp3["first_hit"]
        tstr = time.strftime("%H:%M:%S", time.localtime(hit[0])) if hit else "-"
        check("C4 %s 命中早于第 3 次被拒(第 2 圈即收敛)" % acct,
              hit is not None and hit[1] <= 2,
              "first_hit=%s fail=%s no_ghost=%s rejects=%d" % (
                  tstr, hit[1] if hit else "-", hit[2] if hit else "-", rp3["n_reject"]))

    # ------- 证据块(与生产人工核对用; PASS 项也可见)
    for acct, day in (("robot0001131@xy3.com", "runs_20260924.log"),
                      ("robot0005232@xy3.com", "runs_20260923.log"),
                      ("robot0001130@xy3.com", "runs_20260923.log"),
                      ("robot0001034@xy3.com", "runs_20260923.log")):
        p = os.path.join(log_dir, acct, day)
        if should is None or not os.path.exists(p):
            continue
        rp = _replay(p, should, 6)
        hit = rp["first_hit"]
        if hit:
            tstr = time.strftime("%m-%d %H:%M:%S", time.localtime(hit[0]))
            print("[证据] %s %s: 首个收敛点 %s (被拒=%d 无鬼轮=%d) | 全天检查点=%d 被拒=%d" % (
                acct, day, tstr, hit[1], hit[2], rp["n_loop"], rp["n_reject"]))
        else:
            print("[证据] %s %s: 全程不收敛(反例/正常路径) | 检查点=%d 被拒=%d" % (
                acct, day, rp["n_loop"], rp["n_reject"]))

    # ============================================================ 汇总
    passed = sum(1 for _n, ok, _d in results if ok)
    failed = [(n, d) for n, ok, d in results if not ok]
    for n, _ok, d in results:
        if not _ok:
            print("[FAIL] %s %s" % (n, ("| " + str(d)) if d else ""))
    print("-" * 68)
    print("ghost_leftover_selftest: %d/%d PASS" % (passed, len(results)))
    if failed:
        print("FAILED %d 项:" % len(failed))
        for n, _d in failed:
            print("  - %s" % n)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
