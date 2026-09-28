# -*- coding: utf-8 -*-
"""找鬼追踪（等刷鬼优化）自检 —— 2026-09-24 现场缺陷（data/bot_logs 生产取证）:
  现象（robot0001000@xy3.com 2026-09-24 14:42~15:02，另见全场 411 号统计）:
    ① 号按 12054 目标坐标 [26,2144,2736] 走到位（实测 (2152,2744) 距目标 11px）后
       dynamic_npcs 无鬼 → "等刷鬼超时, 附近巡逻" → 3 轮无鬼；
    ② "换图"选到**别的刷鬼图**（26→9，__pick_ghost_map 优先 map_skip 直达）→ 下一
       tick 等鬼分支的 kill_area 导航又把号拉回 26 → 26↔9 往返 5 次（每次 ~80 秒，
       途经付费跳转 13255）→ 12 轮 → 残留出口停摆；
    ③ daily_ghost 只在接取/登录/缓存恢复时解析一次 12054 —— g.kill_area 是"接取
       瞬间的快照"，即使服务端 TASK_UPDATE 重推了新坐标（quest_engine.on_event 会
       刷 quest.tasks[ti]，daily_ghost 无任何消费点）也不会追。
  量化（2026-09-24 全场 411 号，工具 tools 外的一次性分析）:
    · "前往图X"跨图导航 30942 次，其中 11160 次目标图 ≠ 任务图（无用换图）；
    · 7112 次"鬼 X 在图N(跨图)"里 7104 次 N 就是 kill_area 图（99.94%，鬼只在
      任务图刷）；90351 推送与号所在图无关（号在图 24 也收到"鬼在图 26"）；
    · 等刷鬼超时 6694 次、"等刷鬼 N 轮"12483 次、清鬼 1475 次/311 号、残留停止
      639 次/167 号。

修复（daily_ghost.py，两处，均在 WAIT_GHOST 的"无鬼"分支内，最小侵入）:
  A 追新坐标 `__refresh_hunt_target`（节流 GHOST_HUNT_REREAD_MS=3000ms，上限
    GHOST_HUNT_MAX_FOLLOW=4 次/任务）: 重读 quest.tasks[ti] 最新 12054 → 坐标变化则
    更新 g.kill_area/target_locs，交给既有导航（跨图 __goto_xy / 同图 walk）追过去；
    坐标未变/解析失败/任务缺失/超限 → 零副作用（一切保持原样）。
  B 换图不越任务图: `g.rounds >= 3` 的换图分支加 `not g.kill_area` —— 有任务目标
    坐标时留在任务图等/巡逻（鬼 99.94% 在任务图，换图对发现鬼零帮助且与 kill_area
    导航互相拉锯）；kill_area 缺失时与修复前逐字节一致（换图碰运气）。

本脚本四段:
  A 静态: 源码形状（常量/函数/调用点唯一且在 WAIT_GHOST 无鬼分支、换图条件、既有
          路径保留: 发现即打/残留出口/计数 1:1/付费降量/蛋保护/巡逻）；
  B 动态: 真执行 `__refresh_hunt_target` —— 坐标变化→追新（更新 kill_area）；
          坐标未变/节流内/达上限/任务缺失/解析异常→返回 False 且不改任何状态；
          换任务号→计数重置；只有 target_locs（无 12053）→ 按当前图选点；
  C 回放: 用**真实生产日志**断言缺陷证据（换图↔kill_area 拉锯、"鬼在任务图"高
          比例）——修复针对的现象真实存在；日志缺失时跳过；
  D 兼容: pay_reduce/leftover 原文保留、不动协议面、不加 config 开关（改动只影响
          "WAIT_GHOST 无鬼 + kill_area 存在"窄分支）。

用法: python tools/ghost_hunt_selftest.py [script_dir] [log_dir]
"""
import io
import os
import re
import sys

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
    print("[%s] %s%s" % ("PASS" if cond else "FAIL", name,
        (" | %s" % detail) if (detail and not cond) else ""))


def _extract_func(src, name):
    lines = src.split("\n")
    for i, ln in enumerate(lines):
        if ln.startswith("def %s(" % name):
            out = [ln]
            for ln2 in lines[i + 1:]:
                if ln2.strip() and not ln2[0].isspace():
                    break
                out.append(ln2)
            return "\n".join(out)
    return None


def _strip_comments(frag):
    # 粗暴剥"#"后注释(本片段内无含 "#" 的字符串字面量): 供"代码里不得出现 X"类断言
    out = []
    for ln in frag.split("\n"):
        i = ln.find("#")
        out.append(ln[:i] if i >= 0 else ln)
    return "\n".join(out)


# ======================================================================
# A. 静态: 源码形状
# ======================================================================
def part_a(dh, cfg):
    check("A1 常量 GHOST_HUNT_REREAD_MS = 3000(节流)",
          re.search(r"^GHOST_HUNT_REREAD_MS\s*=\s*3000", dh, re.M) is not None)
    check("A2 常量 GHOST_HUNT_MAX_FOLLOW = 4(追新上限)",
          re.search(r"^GHOST_HUNT_MAX_FOLLOW\s*=\s*4", dh, re.M) is not None)
    check("A3 定义 __refresh_hunt_target(重读入口)", "def __refresh_hunt_target(" in dh)

    # 调用点: 唯一, 且在 WAIT_GHOST 分支内、发现鬼分支之后(有鬼优先顺序不变)
    call = "\t\t__refresh_hunt_target(robot_object, g, quest, now_ms)"
    check("A4 调用点唯一(全文件 1 处, 带缩进调用)",
          dh.count(call) == 1, "count=%d" % dh.count(call))
    i_wait = dh.find('elif g.state == "WAIT_GHOST":')
    i_submit = dh.find('elif g.state == "SUBMIT":', i_wait)   # WAIT_GHOST 之后的下一个状态分支
    i_call = dh.find(call)
    i_find = dh.find("gid = __find_ghost_npc(robot_object)")
    check("A5 调用点在 WAIT_GHOST 分支区间内",
          i_wait >= 0 and i_submit > i_wait and i_wait < i_call < i_submit,
          "wait=%d call=%d submit=%d" % (i_wait, i_call, i_submit))
    check("A6 调用点在「发现鬼→NAV」之后(有鬼时永不走到新逻辑)",
          i_find >= 0 and i_find < i_call, "find=%d call=%d" % (i_find, i_call))

    # 换图条件: 有 kill_area 时不换
    check("A7 换图条件含 not g.kill_area(有任务目标坐标不换图)",
          "g.ghost_maps and robot_object.m_mapid in g.ghost_maps and not g.kill_area" in dh)
    check("A8 既有 2 处 __pick_ghost_map 调用都在(未删旧兜底, 仅 3 轮档受保护)",
          dh.count("= __pick_ghost_map(robot_object, quest, g)") == 2,
          "count=%d" % dh.count("= __pick_ghost_map(robot_object, quest, g)"))
    check("A9 新增日志文案(不换图可观测)",
          "任务图内继续等/巡逻(保留任务, 不换图)" in dh)
    check("A10 旧日志文案保留(kill_area 缺失路径与修复前一致, 统计口径不破)",
          "等刷鬼 %d 轮无鬼, 当前图继续等/巡逻(保留任务)" in dh)

    # 既有行为保留(源码形状)
    check("A11 发现鬼→NAV 路径原文保留",
          "gid = __find_ghost_npc(robot_object)" in dh
          and '__set_state(g, "NAV", now_ms)' in dh)
    check("A12 残留计数累加原文保留(no_ghost_rounds)",
          'g.no_ghost_rounds = int(getattr(g, "no_ghost_rounds", 0)) + 1' in dh)
    check("A13 残留出口判据/动作原文保留",
          "def __should_leftover_stop(" in dh
          and "def __enter_leftover_stop(" in dh
          and "if __should_leftover_stop(g):" in dh
          and "if __enter_leftover_stop(robot_object, g, quest, now_ms):" in dh)
    check("A14 计数 1:1(只数打鬼 FINISH)原文保留",
          re.search(r"if int\(ti\) != GHOST_SUBMIT_TASK:\s*\n\t\tg\.done_count \+= 1", dh) is not None)
    check("A15 付费降量闸原文保留(__should_skip_broker_abandon)",
          "def __should_skip_broker_abandon(" in dh
          and "if __should_skip_broker_abandon(g):" in dh
          and "原地换图再等(第 %d/%d 次)" in dh)
    check("A16 蛋保护原文保留(整理跳过蛋)",
          "孵化蛋(坐骑蛋/元气蛋)不参与整理" in dh and "bag_ops.egg_kind" in dh)
    check("A17 巡逻/基准原文保留(__patrol 同图 kill_area 基准)",
          "if g.kill_area and int(g.kill_area[0]) == int(robot_object.m_mapid):" in dh)
    check("A18 发现鬼选鬼实现原文保留(__find_ghost_npc 主鬼匹配)",
          "if npc_id in g.ghost_names:" in dh
          and "main_npc_index = int(t[1])" in dh)

    # 新逻辑边界: 新函数只读 quest.tasks(不写), 不发包, 不加协议面
    frag = _extract_func(dh, "__refresh_hunt_target")
    check("A19 可抽取 __refresh_hunt_target 函数体", frag is not None)
    if frag:
        frag_code = _strip_comments(frag)
        check("A20 新函数不写 quest.tasks(只读镜像, 不改任务数据)",
              re.search(r"quest\.tasks\[[^\]]*\]\s*=", frag_code) is None)
        check("A21 新函数不发包/不碰协议(send_message/click 零调用)",
              "send_message" not in frag_code and "__teleport_click" not in frag_code
              and "__goto_xy" not in frag_code)
        check("A22 新函数不动 rounds(不干扰看门狗/残留计数语义)",
              "g.rounds" not in frag_code)
        check("A23 新函数有零副作用边界(坐标未变 → 不追、return False)",
              "if _new_ka == _old_ka and _new_locs == _old_locs:" in frag_code)
        check("A24 新函数有追新上限闸(GHOST_HUNT_MAX_FOLLOW)",
              "GHOST_HUNT_MAX_FOLLOW" in frag_code)
    # 不加 config 开关(部署面最小化): 新常量只在 daily_ghost 内
    check("A25 config.py 无 GHOST_HUNT 开关(纯逻辑, 不扩部署面)",
          ("GHOST_HUNT" not in cfg) if cfg is not None else True)

    # ---- 病灶1: NAV 追鬼误清鬼(独立 nav 计数) 的源码形状 ----
    check("A26 常量 NAV_FOLLOW_MAX = 3(本状态真实超时上限)",
          re.search(r"^NAV_FOLLOW_MAX\s*=\s*3", dh, re.M) is not None)
    check("A27 定义 __nav_ghost_follow_ok(清鬼判据入口)",
          "def __nav_ghost_follow_ok(" in dh)
    check("A28 清鬼判据改用本状态计数(共享 rounds<3 判据已不存在)",
          "__nav_ghost_follow_ok(g):" in dh
          and re.search(r"and g\.rounds < 3", dh) is None)
    check("A29 NAV 真实超时累加独立计数(nav_rounds)",
          "g.nav_rounds = _nav_r" in dh
          and '_nav_r = int(getattr(g, "nav_rounds", 0) or 0)' in dh)
    check("A30 进入 NAV 清零(仅进入; 内部重推不清)",
          re.search(r'if state == "NAV":\s*\n\t\t\tg\.nav_rounds = 0', dh) is not None)
    check("A31 清鬼后本状态计数归零",
          "g.nav_rounds = 0\t# 2026-09-24 找鬼追踪: 本轮追鬼结束" in dh)
    check("A32 共享 rounds 其他语义原样(累加/非 NAV 日志; NAV stuck 判据已按 09-28 P1 改用本状态计数)",
          '_over_r = _nav_r if g.state == "NAV" else g.rounds' in dh
          and "if _over_r > 4:" in dh
          and "if g.rounds > 4:" not in dh
          and '__stuck(robot_object, g, "状态 %s 重试 %d 次仍无进展"' in dh
          and '"抓鬼状态 %s 超时, 第 %d 次重试" % (g.state, g.rounds)' in dh)
    check("A33 nav_rounds 字段在 __init__ 与 reset 都初始化",
          dh.count("self.nav_rounds = 0") >= 2)


# ======================================================================
# B. 动态: 真执行 __refresh_hunt_target
# ======================================================================
class _RO(object):
    def __init__(self, mapid=26):
        self.m_mapid = mapid


class _G(object):
    def __init__(self):
        self.task_index = 0
        self.kill_area = None
        self.target_locs = []
        self.hunt_reread_ms = 0
        self.hunt_follow_ti = 0
        self.hunt_follow_count = 0


class _Quest(object):
    def __init__(self):
        self.tasks = {}


def part_b(dh):
    frag_rt = _extract_func(dh, "__refresh_hunt_target")
    frag_pk = _extract_func(dh, "__pick_loc_for_map")
    if not frag_rt or not frag_pk:
        check("B0 抽取函数(入口+选点)", False, "frag_rt=%s frag_pk=%s" % (bool(frag_rt), bool(frag_pk)))
        return

    logs = []
    parse_state = {"ka": None, "locs": [], "boom": False}

    def _stub_parse(dl):
        if parse_state["boom"]:
            raise RuntimeError("marshal broken")
        return (parse_state["ka"], list(parse_state["locs"]), None, None, "dict")

    ns = {
        "__parse_task_attr": _stub_parse,
        "__log": lambda ro, lvl, msg: logs.append((lvl, msg)),
        "GHOST_HUNT_REREAD_MS": 3000,
        "GHOST_HUNT_MAX_FOLLOW": 4,
    }
    try:
        exec(frag_pk, ns)
        exec(frag_rt, ns)
    except Exception as e:  # noqa
        check("B0 抽取函数(入口+选点)", False, "exec: %s" % e)
        return
    rt = ns["__refresh_hunt_target"]

    def _mk(ti=2019502, ka=None, locs=None):
        q = _Quest()
        q.tasks[ti] = [ti, 101, 0, 0, 0, [], b""]
        parse_state["ka"] = ka
        parse_state["locs"] = list(locs or [])
        parse_state["boom"] = False
        return q

    NOW = 1790233000000.0

    # B1 首读: 无 kill_area → 新坐标(像缓存恢复/重登)
    g = _G(); g.task_index = 2019502
    q = _mk(ka=[26, 2144, 2736], locs=[(26, 2144, 2736)])
    r = rt(_RO(26), g, q, NOW)
    check("B1 首读: 追新生效(kill_area 更新 + 返回 True)",
          r is True and g.kill_area == [26, 2144, 2736]
          and g.target_locs == [(26, 2144, 2736)] and g.hunt_follow_count == 1,
          "r=%s ka=%s cnt=%s" % (r, g.kill_area, g.hunt_follow_count))
    check("B1b 首读有观测日志", any("任务目标坐标更新" in m for _, m in logs))

    # B2 坐标未变 → 不触发、状态原样
    logs.clear()
    r = rt(_RO(26), g, q, NOW + 5000)
    check("B2 坐标未变: 返回 False 且不改状态(等价修复前)",
          r is False and g.kill_area == [26, 2144, 2736] and g.hunt_follow_count == 1
          and not logs, "r=%s cnt=%s logs=%s" % (r, g.hunt_follow_count, logs))

    # B3 坐标变化(鬼移动/服务端刷新) → 追新
    parse_state["ka"] = [26, 3664, 1312]
    parse_state["locs"] = [(26, 3664, 1312)]
    r = rt(_RO(26), g, q, NOW + 10000)
    check("B3 坐标变化: 追新(kill_area → 新点, 计数 +1)",
          r is True and g.kill_area == [26, 3664, 1312] and g.hunt_follow_count == 2,
          "r=%s ka=%s cnt=%s" % (r, g.kill_area, g.hunt_follow_count))

    # B4 节流: 3 秒内不重读(即使坐标已变)
    parse_state["ka"] = [26, 1776, 416]
    parse_state["locs"] = [(26, 1776, 416)]
    r = rt(_RO(26), g, q, NOW + 11000)   # 距上次(10000) 仅 1 秒
    check("B4 节流内: 返回 False 且 kill_area 保持旧值",
          r is False and g.kill_area == [26, 3664, 1312] and g.hunt_follow_count == 2,
          "r=%s ka=%s" % (r, g.kill_area))

    # B5 节流外: 追新
    r = rt(_RO(26), g, q, NOW + 14000)
    check("B5 节流外: 追新(kill_area → 新点)",
          r is True and g.kill_area == [26, 1776, 416] and g.hunt_follow_count == 3,
          "r=%s ka=%s cnt=%s" % (r, g.kill_area, g.hunt_follow_count))

    # B6 达上限: 不再追(保持旧值), 防无底洞
    g.hunt_follow_count = 4
    parse_state["ka"] = [26, 999, 999]
    parse_state["locs"] = [(26, 999, 999)]
    r = rt(_RO(26), g, q, NOW + 20000)
    check("B6 达上限(4): 返回 False 且 kill_area 保持(退回现状, 不会更坏)",
          r is False and g.kill_area == [26, 1776, 416] and g.hunt_follow_count == 4,
          "r=%s ka=%s" % (r, g.kill_area))

    # B7 换任务号: 追新计数重置
    g2 = _G(); g2.task_index = 2019503; g2.hunt_follow_ti = 2019502; g2.hunt_follow_count = 4
    g2.kill_area = [11, 100, 100]; g2.target_locs = [(11, 100, 100)]
    q2 = _mk(ti=2019503, ka=[11, 2000, 2000], locs=[(11, 2000, 2000)])
    r = rt(_RO(11), g2, q2, NOW)
    check("B7 换任务号: 计数重置后可追新(第 1 次)",
          r is True and g2.kill_area == [11, 2000, 2000]
          and g2.hunt_follow_ti == 2019503 and g2.hunt_follow_count == 1,
          "r=%s cnt=%s" % (r, g2.hunt_follow_count))

    # B8 无任务号 → False
    g3 = _G()
    r = rt(_RO(26), g3, _mk(ka=[26, 1, 1], locs=[(26, 1, 1)]), NOW)
    check("B8 无当前任务号: 返回 False(不动作)", r is False and g3.kill_area is None)

    # B9 quest 为 None → False(不抛)
    r = rt(_RO(26), g3, None, NOW)
    check("B9 quest 缺失: 返回 False(不抛异常)", r is False)

    # B10 解析异常 → False(不抛, 不改变)
    g4 = _G(); g4.task_index = 2019502
    q4 = _mk(ka=[26, 5, 5], locs=[(26, 5, 5)])
    parse_state["boom"] = True
    r = rt(_RO(26), g4, q4, NOW)
    check("B10 解析异常: 吞掉异常返回 False(不把号带崩)", r is False and g4.kill_area is None)
    parse_state["boom"] = False

    # B11 无坐标(12053/12054 都空) → False(保持旧行为)
    g5 = _G(); g5.task_index = 2019502
    r = rt(_RO(26), g5, _mk(ka=None, locs=[]), NOW)
    check("B11 任务无坐标: 返回 False(与修复前一致)", r is False and g5.kill_area is None)

    # B12 只有 target_locs(无 12053): 按当前图优先选点(与 __set_task_locs 同策略)
    g6 = _G(); g6.task_index = 2019502
    q6 = _mk(ka=None, locs=[(9, 100, 100), (26, 700, 700)])
    r = rt(_RO(26), g6, q6, NOW)
    check("B12 仅 target_locs: 选当前图(26)的点",
          r is True and g6.kill_area == [26, 700, 700],
          "r=%s ka=%s" % (r, g6.kill_area))

    # B13 跨图坐标变化: kill_area 图变化也照样更新(交给既有导航跨图)
    g7 = _G(); g7.task_index = 2019502
    g7.kill_area = [26, 100, 100]; g7.target_locs = [(26, 100, 100)]
    q7 = _mk(ka=[10, 500, 500], locs=[(10, 500, 500)])
    r = rt(_RO(26), g7, q7, NOW)
    check("B13 跨图变化: kill_area 图/坐标都更新(跨图导航由既有逻辑负责)",
          r is True and g7.kill_area == [10, 500, 500], "ka=%s" % (g7.kill_area,))

    # B14 返回值 False 时不上报日志(不刷屏)
    logs.clear()
    rt(_RO(26), g7, q7, NOW + 9000)   # 坐标未变
    check("B14 未变化不刷日志", not logs, "logs=%s" % logs)

    # ================= 病灶1: 误清鬼回归(NAV 独立计数) =================
    class _G2(object):
        pass

    frag_nf = _extract_func(dh, "__nav_ghost_follow_ok")
    check("B15 可抽取 __nav_ghost_follow_ok", frag_nf is not None)
    if frag_nf:
        ns_nf = {"NAV_FOLLOW_MAX": 3}
        exec(frag_nf, ns_nf)
        nf = ns_nf["__nav_ghost_follow_ok"]
        g9 = _G2()
        g9.nav_rounds = 0
        g9.rounds = 3       # 其他状态残留(现场"第 3 次"的来源)
        check("B15a 误清鬼回归: rounds=3 残留 + 本状态 0 次 → 继续追(不清鬼)",
              nf(g9) is True)
        g9.nav_rounds = 2
        check("B15b 本状态真实 2 次 → 继续追", nf(g9) is True)
        g9.nav_rounds = 3
        check("B15c 反向: 本状态真实 3 次 → 达上限, 按原设计清鬼换新", nf(g9) is False)
        g9.nav_rounds = 4
        check("B15d 超过上限仍为 False(清鬼)", nf(g9) is False)
        check("B15e 旧对象无 nav_rounds 字段 → 默认 0 → 继续追(兼容)",
              nf(_G2()) is True)
        # B15f 判别力自证: 同一输入下 旧判据(g.rounds<3) 与 新判据 结论相反
        #   —— 若把本用例跑在修复前的代码上(判据=rounds<3), B15a 必 FAIL。
        g9.nav_rounds = 0
        g9.rounds = 3
        _old_ok = (g9.rounds < 3)      # 修复前判据: 同一输入 → False(会清鬼)
        _new_ok = nf(g9)               # 修复后判据: → True(继续追)
        check("B15f 判别力: 同输入 旧判据=False(必清鬼) vs 新判据=True(不清) "
              "→ 修复前本用例必 FAIL(非恒真用例)",
              _old_ok is False and _new_ok is True)

    frag_ss = _extract_func(dh, "__set_state")
    check("B16 可抽取 __set_state", frag_ss is not None)
    if frag_ss:
        ns_ss = {"__now_ms": lambda: 0}
        exec(frag_ss, ns_ss)
        ss = ns_ss["__set_state"]
        g11 = _G2(); g11.state = "WAIT_GHOST"; g11.state_since_ms = 0; g11.nav_rounds = 3
        ss(g11, "NAV", 111)
        check("B16 进入 NAV: nav_rounds 清零",
              g11.state == "NAV" and g11.nav_rounds == 0,
              "state=%s nav=%s" % (g11.state, g11.nav_rounds))
        g11.nav_rounds = 2
        ss(g11, "NAV", 222)
        check("B17 NAV 内部重复设置(超时重推): 不清零(要累计连续次数)",
              g11.nav_rounds == 2 and g11.state_since_ms == 111,
              "nav=%s since=%s" % (g11.nav_rounds, g11.state_since_ms))
        ss(g11, "WAIT_GHOST", 333)
        check("B18 离开 NAV: 不动 nav_rounds(其他状态语义不变)",
              g11.nav_rounds == 2)


# ======================================================================
# C. 回放: 真实生产日志证据(修复针对的现象真实存在)
# ======================================================================
def part_c(log_dir):
    if not log_dir or not os.path.isdir(log_dir):
        check("C0 日志目录存在(缺失则跳过回放)", False, log_dir or "(none)")
        return False
    acct = "robot0001000@xy3.com"
    p = os.path.join(log_dir, acct, "runs_20260924.log")
    if not os.path.exists(p):
        check("C0 样本日志存在(缺失则跳过回放)", False, p)
        return False
    diag_ka = None
    conflict = 0          # 换图目标 ≠ 任务图 的"前往图X"次数
    back_to_ka = 0        # 被 kill_area 拉回任务图的导航次数
    ghost_map_same = 0    # "鬼 X 在图N(跨图)" 且 N == 任务图
    ghost_map_diff = 0
    wait3 = 0
    ts_diag = None       # 最近一次"接到捉鬼任务诊断"的时间戳
    nav3_close = False   # 接任务后 ≤20s 出现"NAV 超时, 第 3 次重试"(残留铁证)
    pat_diag = re.compile(r"接到捉鬼任务 (\d+) 诊断.*?kill_area=(\[(.*?)\])")
    pat_go = re.compile(r"前往图(\d+)\(")
    pat_gm = re.compile(r"鬼 (\d+) 在图(\d+)\(跨图\)")
    with io.open(p, encoding="utf-8", errors="ignore") as f:
        for line in f:
            _mts = re.search(r'"ts":(\d+)', line)
            _ts = int(_mts.group(1)) if _mts else 0
            if "接到捉鬼任务" in line and "诊断" in line:
                ts_diag = _ts
            if ts_diag and "NAV 超时, 第 3 次重试" in line and 0 < _ts - ts_diag <= 20:
                nav3_close = True
            m = pat_diag.search(line)
            if m:
                try:
                    diag_ka = int(m.group(2).strip("[]").split(",")[0])
                except Exception:
                    diag_ka = None
                continue
            mg = pat_go.search(line)
            if mg and diag_ka is not None:
                tgt = int(mg.group(1))
                if tgt != diag_ka:
                    conflict += 1
                else:
                    back_to_ka += 1
            mm = pat_gm.search(line)
            if mm and diag_ka is not None:
                if int(mm.group(2)) == diag_ka:
                    ghost_map_same += 1
                else:
                    ghost_map_diff += 1
            if "等刷鬼 3 轮无鬼" in line:
                wait3 += 1
    total_gm = ghost_map_same + ghost_map_diff
    check("C1 现场样本存在「换图目标 ≠ 任务图」的无用跨图(缺陷证据)",
          conflict >= 1, "conflict=%d" % conflict)
    check("C2 现场样本存在「拉回任务图」导航(换图↔kill_area 拉锯证据)",
          back_to_ka >= 1, "back=%d" % back_to_ka)
    check("C3 现场样本出现「等刷鬼 3 轮」换图档",
          wait3 >= 1, "wait3=%d" % wait3)
    check("C4 样本「鬼所在图 == 任务图」占绝对多数(不换图的决策依据)",
          total_gm >= 10 and ghost_map_same * 100 >= total_gm * 90,
          "same=%d diff=%d" % (ghost_map_same, ghost_map_diff))
    check("C5 现场铁证: 接任务后 ≤20s 出现 NAV「第 3 次重试」"
          "(rounds 残留, 非本状态真实 3 次 → 修复前必误清鬼)",
          nav3_close is True)
    return True


# ======================================================================
# D. 兼容/回归提示
# ======================================================================
def part_d(dh):
    # 新逻辑不改既有状态机顺序: 残留出口仍在 rounds>=6 分支(先于降频/放弃)
    i_has = dh.find("if _has_hunt:")
    i_left = dh.find("if __should_leftover_stop(g):")
    i_skip = dh.find("if __should_skip_broker_abandon(g):")
    check("D1 出口顺序不变(残留出口 先于 降频闸)",
          i_has >= 0 and i_left > i_has and i_skip > i_left)
    # 本次改动不引入新协议/新协议注册(历史注释里出现 FORMAT_MS 字样不算, 看真实调用)
    check("D2 不新增协议面(无协议注册调用)",
          "set_format_dict(" not in dh and ".register(" not in dh and "cnet." not in dh)
    # 追新只影响镜像字段(不触碰 last_task 缓存写入 -> 缓存口径不变)
    frag = _extract_func(dh, "__refresh_hunt_target") or ""
    check("D3 不动任务缓存 __cache_task/last_task(缓存口径不变)",
          "__cache_task" not in frag and "last_task" not in frag)


def main():
    script_dir = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_DIR
    log_dir = sys.argv[2] if len(sys.argv) > 2 else DEFAULT_LOG_DIR
    if os.path.isfile(script_dir):
        script_dir = os.path.dirname(script_dir)
    dh_path = os.path.join(script_dir, "daily_ghost.py")
    cfg_path = os.path.join(script_dir, "config.py")
    if not os.path.exists(dh_path):
        print("[FAIL] 找不到 %s" % dh_path)
        return 2
    dh = io.open(dh_path, encoding="utf-8", errors="replace").read()
    cfg = io.open(cfg_path, encoding="utf-8", errors="replace").read() if os.path.exists(cfg_path) else None

    print("自检目标: %s" % script_dir)
    part_a(dh, cfg)
    part_b(dh)
    try:
        part_c(log_dir)
    except Exception as e:  # noqa
        check("C 回放段异常", False, str(e))
    part_d(dh)

    n_fail = sum(1 for _, ok, _ in results if not ok)
    print("=" * 60)
    print("结果：%d 项，失败 %d 项" % (len(results), n_fail))
    if n_fail:
        for name, ok, detail in results:
            if not ok:
                print("  FAIL: %s%s" % (name, (" | %s" % detail) if detail else ""))
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
