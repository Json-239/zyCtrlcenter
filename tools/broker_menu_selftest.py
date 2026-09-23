# -*- coding: utf-8 -*-
"""钟馗"已有任务菜单"3 秒级死循环修复自检（2026-09-23）。

生产现象（2026-09-23 全天，`data/bot_logs/*/runs_20260923.log` 全量 472 个号）：
  - `type=error` 里 GHOST_DIALOG_STUCK 165 次 / 103 号，其中
    **"钟馗已有任务菜单连续 3 次无进展" 93 次 / 83 号**（00:00~18:00 共 90 次，
    18:00 后付费降量补丁上线后仅 3 次）。
  - 菜单命中（"钟馗弹已有任务菜单(放弃捉鬼)..."）全天 **581 次 / 208 号**，
    100% 是 `_local_has=True`；任务组合清一色是"1 个打鬼任务(2019501~2019510)
    + 7000101/2015915 等非抓鬼任务"，**没有一次含交付任务 2019511**
    （即注释里"2019511 与打鬼任务并存"的老场景当天 0 发生）。

根因链（代码 + 日志时间线）：
  1. 等刷鬼 6 轮无鬼 → `WAIT_GHOST` 置 `g.abandon_task = True` → 回 24 点钟馗
     （daily_ghost.py:3460 附近）。
  2. 服务端弹"已有任务菜单" → 选"放弃捉鬼任务"(index 2)，但放弃需扣 500 银两、
     号上不足 → 服务端**不回 S2C_DROP_TASK**，且**不关对话**。
  3. `对话超 30 秒未关闭` 看门狗（DIALOG_STUCK_MS）随即强制关闭对话 —— 旧实现
     **无条件 `__goto(钟馗)`**（"回 READY 重新接取"），机器人又走回钟馗重新点 NPC。
  4. 再弹同一菜单，此时 `abandon_task` 已被清（点放弃前就清）→ 落到"本地确实有任务
     → 关闭对话回 READY"分支 → `broker_menu_streak += 1`。
  5. 3 次即 `BROKER_MENU_STREAK_LIMIT` → `__need_relogin` 重登。

  现场 robot0001116（20:33:24 菜单 → 20:33:25 点放弃 → 20:33:25 强制关闭并回钟馗
  → 20:33:28 再弹菜单 3/3 → 20:33:28 熔断重登，全程 4 秒）；robot0005071
  （04:24~04:32 同样的 4 分钟一轮 ×3 → 熔断）。24 小时里 6594 次"强制关闭"有 6318 次
  （96%）发生在对话打开后 ≤2 秒（计时基准取的是陈旧的 `state_since_ms`），
  其中 502 次在 8 秒内又弹回同一菜单 —— 即这条循环是主力来源。

修复（daily_ghost.py，最小改动、两处）：
  ① 主路径（93 次里 78 次有该前因、589 次菜单命中 86% 有该前因）：
     看门狗强制关闭后**不再无条件回钟馗**，只回 READY，由 READY 既有分流决定去哪：
       · 本地有打鬼任务 → WAIT_GHOST 继续追鬼/等刷鬼（死循环断开）；
       · 本地无任务 / 只有交付任务 2019511 → READY 自己会 `__goto(钟馗)`（行为不变）。
     这样"点放弃被拒 → 强制关闭 → 回钟馗 → 再弹菜单"这条路不复存在，
     `broker_menu_streak` 不再被刷满，"本地无记录仍点放弃"、"交付照常回钟馗"
     两条既有语义都保留。
  ② 残余路径（93 次里 15 次：无强制关闭、纯 SUBMIT 驱动）：
     服务端弹本菜单 = 明确拒绝交付（有效交付 12386 次 100% 走"0 选项、任务 2019511"
     的交付对话）。菜单命中时就地清掉打鬼任务的陈旧"可交"标记（t[3]→0；只清标记、
     不删任务、不碰 2019511），斩断 READY→WAIT_GHOST→"任务可交"→SUBMIT→点钟馗→
     再弹菜单的 20 秒一轮空转；服务端仍判可交会重下发状态，正常交付不受影响。

本脚本为源码级自检（不 import daily_ghost：依赖机器人运行时）：
  A) 静态断言：修复点形状/顺序（含①②两处），基线四处逻辑（pay-reduce / 抓鬼计数 1:1 /
     交付无进展修复 / 跨图无回执看门狗）未被破坏。
  B) 动态断言：提取**真实源码块** + fake 注入执行：
     ① 看门狗块 —— 有打鬼任务时回 READY 且**不 goto 钟馗**；rounds>6 仍走重登兜底。
     ② READY 分流块 —— 打鬼任务 → WAIT_GHOST（不回钟馗）；
        交付任务 2019511 → SUBMIT + 回钟馗（交付不受影响）。
     ②b WAIT_GHOST"任务可交"块 —— t[3]==1 → SUBMIT（既有入口）；标记清掉 → 不再 SUBMIT；
        2019511 按 id 仍 → SUBMIT（交付路径与标记清理解耦）。
     ③ 菜单处理器块 —— 复现"点放弃被拒"整轮：链路里不再出现回钟馗 → 不熔断；
        残余复现：陈旧 t[3]==1 被就地清掉、链尾不再误进 SUBMIT → 不熔断重登。
     ④ 反例 —— 本地无记录（_local_has=False）仍点"放弃捉鬼任务"重接；
        _local_has=True 且非显式放弃仍走"关对话回 READY"计数，3 次仍熔断（兜底没丢）；
        非抓鬼任务的 done 标记不误清；2019511 的标记不清理。

用法:
  python tools/broker_menu_selftest.py [script 目录 | daily_ghost.py 路径]
  不带参数默认校验仓库副本；传生产 script 目录可再验一次线上文件。
"""
import os
import re
import sys

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_DIR = os.path.normpath(os.path.join(
    HERE, "..", "deploy", "zones", "prod-240-2300", "script"))


def _resolve(path):
    return path if os.path.isdir(path) else os.path.dirname(path)


RESULT = []


def check(name, ok, detail=""):
    RESULT.append((name, bool(ok), detail))
    return bool(ok)


def _slice(text, start, end):
    """取 [start, end) 片段; 找不到返回 None。"""
    i = text.find(start)
    if i < 0:
        return None
    j = text.find(end, i + len(start))
    if j < 0:
        return None
    return text[i:j]


def _slice_keep_end(text, start, end):
    """取 [start, end+len(end)) 片段（end 作为结束锚点并保留）。"""
    i = text.find(start)
    if i < 0:
        return None
    j = text.find(end, i + len(start))
    if j < 0:
        return None
    return text[i:j + len(end)]


def _dedent1(text):
    """每行去掉一个前导 tab（把 2 tab 缩进的块提到 1 tab，供 def 包壳）。"""
    out = []
    for ln in text.split("\n"):
        if ln.startswith("\t"):
            out.append(ln[1:])
        else:
            out.append(ln)
    # 去掉尾随空行（包壳后可能落在 def 外）
    while out and not out[-1].strip():
        out.pop()
    return "\n".join(out)


class _G(object):
    """GhostState 最简替身（默认值覆盖本自检用到的字段）。"""

    DEFAULTS = {
        "state": "READY", "state_since_ms": 0, "rounds": 0,
        "task_index": 0, "done_count": 0,
        "token_shop_ctx": None, "token_fail": 0,
        "dlg_close_tries": 0, "dlg_count_ms": 0,
        "abandon_task": False, "abandon_tries": 0, "abandon_click_ms": 0,
        "broker_menu_streak": 0, "broker_menu_fp": None, "last_task": None,
    }

    def __init__(self, **kw):
        for k, v in self.DEFAULTS.items():
            setattr(self, k, v)
        for k, v in kw.items():
            setattr(self, k, v)


class _Quest(object):
    """quest 最简替身（只要被源码块读到的字段）。"""

    def __init__(self, tasks=None, dialog_open=False):
        self.tasks = dict(tasks or {})
        self.dialog = {"opts": []} if dialog_open else None
        self.dialog_open = dialog_open
        self.pending = "PENDING"
        self.walk_target = "WALK"
        self.dijkstra_route = ["R"]
        self.chain = None


class _Engine(object):
    """quest_engine 最简替身: 只记 __schedule 的动作。

    注意: `__schedule`/`__human_delay` 在类体里定义会被 Python 名字改编
    （_Engine__schedule），而源码是按字面名 `quest_engine.__schedule` 调用的，
    所以这里用 setattr 设字面名。
    """

    def __init__(self, actions):
        self.actions = actions
        setattr(self, "__schedule", lambda quest, act: actions.append(act))
        setattr(self, "__human_delay", lambda ms: ms)


def _is_ghost_task(ti):
    try:
        return ti is not None and 2019501 <= int(ti) <= 2019513
    except Exception:
        return False


def main():
    if len(sys.argv) < 2:
        script_dir = DEFAULT_DIR
    else:
        script_dir = _resolve(sys.argv[1])
    ghost_path = os.path.join(script_dir, "daily_ghost.py")
    if not os.path.exists(ghost_path):
        print("[FAIL] 找不到 %s" % ghost_path)
        return 2
    src = open(ghost_path, encoding="utf-8").read()

    # ============================================================ A. 源码形状
    check("常量 BROKER_MENU_STREAK_LIMIT = 3 仍在(熔断兜底未改口径)",
          "BROKER_MENU_STREAK_LIMIT = 3" in src)
    check("常量 DIALOG_STUCK_MS = 30000 仍在(看门狗阈值未动)",
          "DIALOG_STUCK_MS = 30000" in src)

    # 看门狗块提取（起始锚带缩进: 供 _dedent1 包壳成 def 体）
    wd = _slice_keep_end(src, "\t\tif _dlg_elapsed > DIALOG_STUCK_MS:",
                         "\n\t\t\treturn 0\n\t\treturn 0")
    check("提取到对话看门狗块(if _dlg_elapsed > DIALOG_STUCK_MS)", wd is not None)
    if wd is None:
        print("\n提取失败, 后续动态用例跳过")
        wd = ""

    check("看门狗块内不再无条件回钟馗(无 __goto 调用)",
          wd != "" and "__goto(" not in wd,
          "块内出现 __goto" if "__goto(" in wd else "")
    check("看门狗块内不出现 BROKER_NPC_ID(不指向钟馗)",
          wd != "" and "BROKER_NPC_ID" not in wd)
    check("看门狗块保留 __set_state(g, \"READY\", now_ms)(只回 READY)",
          wd != "" and '__set_state(g, "READY", now_ms)' in wd)
    check("看门狗块保留 rounds>6 重登兜底",
          wd != "" and "if g.rounds > 6:" in wd and "__need_relogin(" in wd)
    check("看门狗日志改为'按本地任务分流'(不再宣称重新接取)",
          wd != "" and "强制关闭回 READY 按本地任务分流" in wd)

    # READY 分流块提取（修复后看门狗的唯一去向）
    rd = _slice(src, "\t\t# 有未完成的打鬼任务? 先打; 否则接取",
                "\n\t\t# 没有进行中任务: 找钟馗接取")
    check("提取到 READY 任务分流块(有打鬼任务先打, 否则接取)", rd is not None)
    rd = rd or ""
    check("READY 分流: 非交付抓鬼任务 → WAIT_GHOST(不回钟馗)",
          'g.task_index = ti' in rd and '__set_state(g, "WAIT_GHOST", now_ms)' in rd
          and rd.find('else:') < rd.find('"WAIT_GHOST"'))
    check("READY 分流: 交付任务 2019511 → SUBMIT + 回钟馗(交付路径不变)",
          "if ti == GHOST_SUBMIT_TASK:" in rd and '__set_state(g, "SUBMIT", now_ms)' in rd
          and "BROKER_NPC_ID" in rd)

    # 菜单处理器块提取（宿主 on_show_dialog 内, 1 tab 缩进; 结束锚带 tab）
    mn = _slice(src, "\tif has_abandon and npc_id == BROKER_NPC_ID:",
                "\n\t# 2026-09-22 删除")
    check("提取到钟馗已有任务菜单处理块", mn is not None)
    mn = mn or ""
    check("菜单块: 保留 _local_has 判定(本地任务在场语义)",
          "_local_has = any(is_ghost_task(ti) for ti in (quest.tasks or {}))" in mn)
    check("菜单块: 保留'本地无记录 → 点放弃捉鬼任务重接'(反例路径)",
          "钟馗已有任务但本地无记录" in mn and "选放弃捉鬼任务重接(第 %d 次)" in mn)
    check("菜单块: 保留 broker_menu_streak 指纹计数 + 达限重登(兜底)",
          "broker_menu_fp" in mn and "BROKER_MENU_STREAK_LIMIT" in mn
          and "__need_relogin(" in mn)
    check("菜单块: 保留缓存恢复(免金币原地续打)",
          "__recover_task_from_cache(robot_object, g, quest)" in mn)
    # 2026-09-23 残余路径修复(按证据清理陈旧"可交"标记)
    check("菜单块[残余修复]: 含'可交'标记就地清理(t[3]→0, 指纹顺序不变)",
          "的'可交'标记与服务端不一致(交付被拒), 就地清掉继续追鬼" in mn
          and "_nt[3] = 0" in mn and "quest.tasks[_ti] = _nt" in mn)
    check("菜单块[残余修复]: 只清打鬼任务标记, 不碰 2019511, 不删任务",
          "int(_ti) != GHOST_SUBMIT_TASK" in mn and "quest.tasks.pop" not in mn)

    # 基线四处逻辑（别人的修复）不得回退
    check("基线①pay-reduce: __should_skip_broker_abandon 降频闸在位",
          "def __should_skip_broker_abandon(g):" in src
          and "if __should_skip_broker_abandon(g):" in src)
    check("基线②抓鬼计数 1:1: 只对非交付任务计数在位",
          "if int(ti) != GHOST_SUBMIT_TASK:" in src and "g.done_count += 1" in src)
    check("基线③交付无进展修复: submit_rounds + 导航豁免在位",
          "submit_rounds" in src and "__submit_nav_busy(quest)" in src
          and "SUBMIT_NAV_LIMIT_MS" in src)
    check("基线④跨图无回执看门狗仍接入 __consume_nav",
          "__check_hop_noack(robot_object, quest, now_ms)" in src)

    # WAIT_GHOST "任务可交"重定向块提取（残余路径的驱动点: t[3]==1 → SUBMIT 点钟馗）
    wg = _slice(src,
                "\t\t# 2026-08-24 没找到可打的鬼: 先看任务是否已可交(打完/服务端判定)",
                "\n\t\t# 2026-09-15 目标坐标兜底")
    check("提取到 WAIT_GHOST '任务可交'重定向块", wg is not None)
    wg = wg or ""

    # ============================================================ B. 动态用例
    calls = []
    logs = []
    actions = []
    NOW = {"v": 1000000}

    def _mk_ns(recover_ret=None, must_buy=False, buy_opt=None):
        ns = {
            "BROKER_NPC_ID": 10146,
            "GHOST_SUBMIT_TASK": 2019511,
            "ABANDON_FAIL_LIMIT": 2,
            "BROKER_MENU_STREAK_LIMIT": 3,
            "DIALOG_STUCK_MS": 30000,
            "TOKEN_FAIL_MAX": 3,
            "is_ghost_task": _is_ghost_task,
            "quest_engine": _Engine(actions),
            "__log": lambda ro, lvl, msg: logs.append((lvl, msg)),
            "__now_ms": lambda: NOW["v"],
            "__emit_state": lambda ro, g: None,
            "__emit": lambda ro, d: None,
            "__clear_broker_empty_backoff": lambda g: None,
            "__release_token_lock": lambda ro: None,
            "__must_buy_token": lambda ro, quest, g: bool(must_buy),
            "__pick_token_opt": lambda opts, npc: (buy_opt if must_buy else None),
            "__begin_token_buy": lambda *a: None,
            "__recover_task_from_cache":
                lambda ro, g, quest, force_submit=False: recover_ret,
            "__task_fingerprint": lambda quest, g: (
                tuple(sorted(str(ti) for ti in (quest.tasks or {})
                             if _is_ghost_task(ti))),
                str(getattr(g, "task_index", 0)),
                int(getattr(g, "done_count", 0) or 0)),
        }

        def _goto(ro, g, npc_id, npc_id2, x, wait):
            calls.append(("goto", npc_id))
            return True

        def _set_state(g, state, now_ms=None):
            if g.state != state:
                g.state = state
                g.state_since_ms = now_ms if now_ms is not None else NOW["v"]

        def _relogin(ro, g, reason):
            calls.append(("relogin", reason))
            return True

        ns["__goto"] = _goto
        ns["__set_state"] = _set_state
        ns["__need_relogin"] = _relogin
        return ns

    ns = _mk_ns()
    frag = _extract_def(src, "on_show_dialog")
    check("提取 on_show_dialog(菜单块宿主)", frag is not None)

    # ---- 动态 ①: 看门狗块
    wd_ns = _mk_ns()
    wd_fn = None
    if wd:
        code = ("def _watchdog(robot_object, g, quest, now_ms,"
                " _dlg_elapsed=99999, _dlg_ms=12345):\n" + _dedent1(wd))
        try:
            exec(code, wd_ns)
            wd_fn = wd_ns.get("_watchdog")
            check("exec 看门狗块(源码级)", wd_fn is not None)
        except Exception as e:  # noqa
            check("exec 看门狗块(源码级)", False, str(e))
    if wd_fn is not None:
        # ①-1 本地有打鬼任务: 只回 READY, 不回钟馗
        calls[:] = []
        logs[:] = []
        g = _G(state="DIALOG", rounds=1, broker_menu_streak=0)
        q = _Quest(tasks={2019506: [2019506, 0, 0, 0, 0, []]}, dialog_open=True)
        wd_fn(None, g, q, NOW["v"])
        check("看门狗(有打鬼任务): 状态回 READY", g.state == "READY")
        check("看门狗(有打鬼任务): 关闭对话/清 pending/walk/路由",
              q.dialog_open is False and q.dialog is None and q.pending is None
              and q.walk_target is None and q.dijkstra_route == [])
        check("看门狗(有打鬼任务): 全程无回钟馗导航(goto 未被调用)",
              not [c for c in calls if c[0] == "goto"])
        check("看门狗(有打鬼任务): 未误触发重登",
              not [c for c in calls if c[0] == "relogin"])
        check("看门狗(有打鬼任务): 日志含'按本地任务分流'",
              any("按本地任务分流" in m for _l, m in logs))
        # ①-2 本地无任务: 同样只回 READY（回钟馗交给 READY 自己）
        calls[:] = []
        g2 = _G(state="DIALOG", rounds=1)
        q2 = _Quest(tasks={}, dialog_open=True)
        wd_fn(None, g2, q2, NOW["v"])
        check("看门狗(本地无任务): 回 READY 且不直接回钟馗(goto 零调用)",
              g2.state == "READY" and not [c for c in calls if c[0] == "goto"])
        # ①-3 rounds>6: 仍走重登兜底（原有语义保留）
        calls[:] = []
        g3 = _G(state="DIALOG", rounds=7)
        q3 = _Quest(tasks={2019506: [2019506, 0, 0, 0, 0, []]}, dialog_open=True)
        wd_fn(None, g3, q3, NOW["v"])
        check("看门狗(rounds>6): 仍走 __need_relogin(对话卡死兜底未丢)",
              any(c[0] == "relogin" for c in calls) and g3.state == "DIALOG")

    # ---- 动态 ②: READY 分流块
    rd_ns = _mk_ns()
    rd_fn = None
    if rd:
        code = "def _ready_route(robot_object, g, quest, now_ms):\n" + _dedent1(rd) \
               + "\n\treturn None\n"
        try:
            exec(code, rd_ns)
            rd_fn = rd_ns.get("_ready_route")
            check("exec READY 分流块(源码级)", rd_fn is not None)
        except Exception as e:  # noqa
            check("exec READY 分流块(源码级)", False, str(e))
    if rd_fn is not None:
        # ②-1 打鬼任务 → WAIT_GHOST, 不回钟馗
        calls[:] = []
        g = _G(state="READY")
        q = _Quest(tasks={2019506: [2019506, 0, 0, 0, 0, []]})
        rd_fn(None, g, q, NOW["v"])
        check("READY 分流(打鬼任务): → WAIT_GHOST", g.state == "WAIT_GHOST")
        check("READY 分流(打鬼任务): task_index 记录为任务号", g.task_index == 2019506)
        check("READY 分流(打鬼任务): 不产生回钟馗导航",
              not [c for c in calls if c[0] == "goto"])
        # ②-2 交付任务 → SUBMIT + 回钟馗
        calls[:] = []
        g = _G(state="READY")
        q = _Quest(tasks={2019511: [2019511, 0, 0, 0, 0, []]})
        rd_fn(None, g, q, NOW["v"])
        check("READY 分流(交付任务 2019511): → SUBMIT", g.state == "SUBMIT")
        check("READY 分流(交付任务 2019511): 回钟馗交付(交付路径不受影响)",
              [c for c in calls if c[0] == "goto" and c[1] == 10146])
        # ②-3 无抓鬼任务 → 不拦(落到接取路径)
        calls[:] = []
        g = _G(state="READY")
        q = _Quest(tasks={7000101: [7000101, 0, 0, 0, 0, []]})
        rd_fn(None, g, q, NOW["v"])
        check("READY 分流(无抓鬼任务): 不设状态(交由下方'找钟馗接取')",
              g.state == "READY" and not calls)

    # ---- 动态 ②b: WAIT_GHOST "任务可交"重定向块（残余路径驱动点）
    wg_ns = _mk_ns()
    wg_fn = None
    if wg:
        code = "def _wait_deliver_check(robot_object, g, quest, now_ms, ti=None):\n" \
               + _dedent1(wg) + "\n\treturn None\n"
        try:
            exec(code, wg_ns)
            wg_fn = wg_ns.get("_wait_deliver_check")
            check("exec WAIT_GHOST '任务可交'块(源码级)", wg_fn is not None)
        except Exception as e:  # noqa
            check("exec WAIT_GHOST '任务可交'块(源码级)", False, str(e))
    if wg_fn is not None:
        # ②b-1 打鬼任务 t[3]==1 → SUBMIT 点钟馗(既有行为, 残余路径的入口)
        calls[:] = []
        g = _G(state="WAIT_GHOST")
        q = _Quest(tasks={2019504: [2019504, 0, 0, 1, 0, []]})
        wg_fn(None, g, q, NOW["v"])
        check("WAIT_GHOST 可交块(打鬼任务 t[3]==1): → SUBMIT + 回钟馗(既有行为)",
              g.state == "SUBMIT"
              and [c for c in calls if c[0] == "goto" and c[1] == 10146])
        # ②b-2 标记被就地清掉(t[3]=0)后 → 不再误进 SUBMIT
        calls[:] = []
        g = _G(state="WAIT_GHOST")
        q = _Quest(tasks={2019504: [2019504, 0, 0, 0, 0, []]})
        wg_fn(None, g, q, NOW["v"])
        check("WAIT_GHOST 可交块(标记已清 t[3]=0): 不再 SUBMIT/不回钟馗",
              g.state == "WAIT_GHOST" and not calls)
        # ②b-3 交付任务 2019511 仍按 id 判定 → SUBMIT(交付路径不受标记清理影响)
        calls[:] = []
        g = _G(state="WAIT_GHOST")
        q = _Quest(tasks={2019511: [2019511, 0, 0, 0, 0, []]})
        wg_fn(None, g, q, NOW["v"])
        check("WAIT_GHOST 可交块(交付任务 2019511): 仍 → SUBMIT + 回钟馗",
              g.state == "SUBMIT" and [c for c in calls if c[0] == "goto"])

    # ---- 动态 ③/④: 菜单处理器块
    mn_ns = _mk_ns()
    mn_fn = None
    if mn:
        code = "def _menu_broker(robot_object, g, quest, option_list, npc_id," \
               " has_abandon, _local_has):\n" + mn
        try:
            exec(code, mn_ns)
            mn_fn = mn_ns.get("_menu_broker")
            check("exec 菜单处理块(源码级)", mn_fn is not None)
        except Exception as e:  # noqa
            check("exec 菜单处理块(源码级)", False, str(e))

    OPTS = [["【捉鬼任务】活动说明", 0], ["领取助战令", 0],
            ["我今天身体不舒服。（放弃捉鬼任务需扣除金", 0], ["离开", 1]]

    if mn_fn is not None:
        # ③ 复现整轮: 显式放弃 + 本地有任务 → 点放弃; 随后看门狗+READY 分流不再回钟馗
        calls[:] = []
        logs[:] = []
        actions[:] = []
        g = _G(state="READY", abandon_task=True, abandon_tries=0,
               abandon_click_ms=0, broker_menu_streak=0, broker_menu_fp=None,
               task_index=2019506, done_count=14)
        q = _Quest(tasks={2019506: [2019506, 0, 0, 0, 0, []],
                          7000101: [7000101, 0, 0, 0, 0, []],
                          2015915: [2015915, 0, 0, 0, 0, []]})
        ret_m = mn_fn(None, g, q, OPTS, 10146, True, True)
        check("复现①菜单(显式放弃+本地有任务): 消化=True", ret_m is True)
        check("复现①: 点了'放弃捉鬼任务'(option_index=2)",
              actions and actions[0].get("data", {}).get("option_index") == 2)
        check("复现①: 记 abandon_click_ms(在途, 供被拒判定)",
              int(getattr(g, "abandon_click_ms", 0) or 0) > 0)
        check("复现①: 本步未回钟馗(goto 零调用, 不刷新菜单命中)",
              not [c for c in calls if c[0] == "goto"])
        # 服务端静默拒绝(不回 DROP_TASK) → 看门狗强制关闭
        if wd_fn is not None:
            calls[:] = []
            q.dialog_open = True
            q.dialog = {"opts": OPTS}
            g.state = "DIALOG"
            g.rounds = 1
            wd_fn(None, g, q, NOW["v"])
            check("复现②看门狗: 回 READY(旧实现此处回钟馗)", g.state == "READY"
                  and not [c for c in calls if c[0] == "goto"])
            if rd_fn is not None:
                calls[:] = []
                rd_fn(None, g, q, NOW["v"])
                check("复现③READY 分流: 回 WAIT_GHOST 继续打鬼",
                      g.state == "WAIT_GHOST")
                check("复现③: 整条链无回钟馗导航 → 不会再弹同一菜单 → 不会熔断",
                      not [c for c in calls if c[0] == "goto"]
                      and g.broker_menu_streak == 0)
        # ③-b 残余路径复现: 本地打鬼任务带陈旧 t[3]==1 → 菜单命中时就地清标记,
        #      链尾 WAIT_GHOST 不再误进 SUBMIT(整条链收敛, 不靠重登)
        calls[:] = []
        logs[:] = []
        actions[:] = []
        g = _G(state="READY", abandon_task=False, abandon_tries=0,
               broker_menu_streak=0, broker_menu_fp=None,
               task_index=2019504, done_count=46)
        q = _Quest(tasks={2019504: [2019504, 0, 0, 1, 0, []],
                          7000101: [7000101, 0, 0, 1, 0, []],
                          2015915: [2015915, 0, 0, 0, 0, []],
                          2019502: [2019502, 0, 0, 0, 0, []]})
        ret_m = mn_fn(None, g, q, OPTS, 10146, True, True)
        check("残余复现①菜单(打鬼任务 t[3]==1): 消化=True 且回 READY",
              ret_m is True and g.state == "READY")
        check("残余复现②陈旧'可交'标记被就地清掉(2019504 → t[3]=0)",
              q.tasks[2019504][3] == 0 and list(q.tasks[2019504][:3]) == [2019504, 0, 0])
        check("残余复现②: 日志写明'交付被拒/清掉继续追鬼'",
              any("交付被拒" in m and "2019504" in m for _l, m in logs))
        check("残余复现②: 非抓鬼任务的 done 标记不误清(7000101 保持 1)",
              q.tasks[7000101][3] == 1 and q.tasks[2019502][3] == 0)
        if wg_fn is not None:
            calls[:] = []
            g2 = _G(state="WAIT_GHOST")
            wg_fn(None, g2, q, NOW["v"])
            check("残余复现③链尾 WAIT_GHOST: 不再误进 SUBMIT(不熔断重登)",
                  g2.state == "WAIT_GHOST"
                  and not [c for c in calls if c[0] == "goto"])
        # ③-c 反例: 标记清理不碰交付任务 2019511(交付路径仍按 id 走 SUBMIT)
        calls[:] = []
        logs[:] = []
        g = _G(state="READY", broker_menu_streak=0, broker_menu_fp=None,
               task_index=2019511)
        q = _Quest(tasks={2019511: [2019511, 0, 0, 1, 0, []]})
        ret_m = mn_fn(None, g, q, OPTS, 10146, True, True)
        check("反例③交付任务 2019511 的标记不被清理(交付语义保留)",
              q.tasks[2019511][3] == 1 and ret_m is True)
        # ④-1 反例: 本地无记录(_local_has=False) → 仍走"点放弃重接"
        calls[:] = []
        logs[:] = []
        actions[:] = []
        g = _G(state="READY", abandon_task=False, abandon_tries=0,
               abandon_click_ms=0, broker_menu_streak=0, task_index=0,
               done_count=0, last_task=None)
        q = _Quest(tasks={})
        ret_m = mn_fn(None, g, q, OPTS, 10146, True, False)
        check("反例①本地无记录: 仍选'放弃捉鬼任务'重接(index=2)",
              ret_m is True and actions
              and actions[0].get("data", {}).get("option_index") == 2)
        check("反例①本地无记录: 日志含'第 1 次'(计数递增)",
              any("本地无记录" in m and "第 1 次" in m for _l, m in logs)
              and g.abandon_tries == 1)
        # ④-2 反例: _local_has=True 且非显式放弃 → 走"关对话回 READY"计数(兜底保留)
        calls[:] = []
        logs[:] = []
        actions[:] = []
        g = _G(state="READY", abandon_task=False, abandon_tries=0,
               broker_menu_streak=0, broker_menu_fp=None, task_index=2019506,
               done_count=14)
        q = _Quest(tasks={2019506: [2019506, 0, 0, 0, 0, []]})
        for i in (1, 2, 3):
            calls[:] = []
            logs[:] = []
            actions[:] = []
            g.state = "READY"
            ret_m = mn_fn(None, g, q, OPTS, 10146, True, True)
            if i < 3:
                check("反例②菜单(有任务+非显式放弃) 第 %d 次: 关对话回 READY 计数 %d/3"
                      % (i, i), ret_m is True and g.broker_menu_streak == i
                      and not actions
                      and not [c for c in calls if c[0] == "goto"])
            else:
                check("反例②第 3 次: 达 BROKER_MENU_STREAK_LIMIT → 重登兜底仍生效",
                      any(c[0] == "relogin" for c in calls)
                      and not [c for c in calls if c[0] == "goto"])

    # ============================================================ C. 汇总
    passed = sum(1 for _n, ok, _d in RESULT if ok)
    failed = [(n, d) for n, ok, d in RESULT if not ok]
    for n, _ok, d in RESULT:
        if not _ok:
            print("[FAIL] %s%s" % (n, ("| " + str(d)) if d else ""))
    print("-" * 68)
    print("broker_menu_selftest: %d/%d PASS" % (passed, len(RESULT)))
    if failed:
        print("FAILED %d 项:" % len(failed))
        for n, _d in failed:
            print("  - %s" % n)
        return 1
    return 0


def _extract_def(src, name):
    pat = r"(?ms)^def %s\(.*?(?=^def |\Z)" % re.escape(name)
    m = re.search(pat, src)
    return m.group(0) if m else None


if __name__ == "__main__":
    sys.exit(main())
