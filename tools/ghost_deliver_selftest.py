# -*- coding: utf-8 -*-
"""钟馗"交付对话被误判为空菜单"修复自检（2026-09-22）。

生产现象（2026-09-22 全天，`data/bot_logs/*/runs_20260922.log`）：
  - `抓鬼下线换号(NO_ACTION)` 954 次 / 513 个号；其中 **750 次(78.6%)** 下线前最后一条
    钟馗对话是 `任务 2019511 选项 0 个`（= 交付成功对话），**仅 206 次** 是真·3 项闲聊菜单。
  - 20:31~20:40 窗口 78 次下线的"会话内交付对话次数"分布：`{2 次: 76 个号}`，
    与 NO_ACTION_OFFLINE_LIMIT=2 完全吻合；76/78 在 3 秒内就收到带接取项的正常菜单
    （证明交付成功、号本可继续抓，却被判"下线换号"，中控还会标"当天不可用"）。
  - 样本 robot0001102：20:37:29 第 1 次交付 → 退避 5s 恢复继续抓；20:39:52 第 2 次交付 → 下线。

根因（三个缺陷叠加）：
  A. 服务端点钟馗会**自动交付**（npc_dispatcher.role_click_npc → execute_task_step），
     下发 0 选项 + task_index=2019511 的信息型对话；机器人把它当"钟馗空菜单" →
     `__note_no_action()` 累加 no_action_rounds。
  B. `no_action_rounds` 只在"抓鬼启动"清零 —— 拿到正常菜单/接到任务/交付成功都不清零，
     等于"整会话累计"；叠加 2026-09-22 把 NO_ACTION_OFFLINE_LIMIT 3→2 → 每个会话在
     第 2 次正常交付时必被判"下线换号"。
  C. 满额号校准键写死 `2019510`，而服务端 task_limited 的 episode_root 会漂移
     （当日首个打鬼任务号，实测 2019501~2019510）→ 误判"已抓 0" → 满额号仍去接 → 空菜单。
  另有死路自愈：C2S_DROP_TASK 对捉鬼全系列（task_dropable="否"）必然被服务端拒绝。

本脚本为源码级自检（不 import daily_ghost: 依赖机器人运行时）：
  1) 静态断言 —— 四个修复点存在且顺序正确（豁免在 __note_no_action 之前）。
  2) 动态断言 —— 提取**真实源码块** + fake 注入执行：
     ① 0 选项 + task_index=2019511 → 豁免: 不计空菜单, rounds 清 0, 提前 return
     ② 0 选项 + task_index=0（真空菜单）→ 不豁免, 仍走 __note_no_action（不误伤）
     ③ 两次交付场景 → 不再触发 __emit_ghost_offline；反例: 旧逻辑同输入必然下线
     ④ 拿到正常动作项 → no_action_rounds 清零（"连续"语义）
     ⑤ 满额校准: f 只有 2019508=50 → 判满额；反例: 旧实现按 2019510 查 → 判 0

用法：python ghost_deliver_selftest.py [script_dir]
      不带参数默认校验仓库副本；传线上 script 目录可再验一次线上文件。
"""
import hashlib
import os
import re
import sys
import types

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_DIR = os.path.normpath(os.path.join(
    HERE, "..", "deploy", "zones", "prod-240-2300", "script"))

script_dir = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_DIR
ghost_path = os.path.join(script_dir, "daily_ghost.py")
ghost_src = open(ghost_path, encoding="utf-8").read()

fails = 0


def check(name, ok, detail=""):
    global fails
    fails += 0 if ok else 1
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  " + detail) if detail else ""))


# ================================================================ 源码提取
# ① 交付豁免块（daily_ghost: if first is None and npc_id == BROKER_NPC_ID 块内的
#    _deliver_ti = 0 ... return True）
m_ex = re.search(
    r'(?ms)^\t\t\t_deliver_ti = 0\n.*?^\t\t\t\treturn True\n', ghost_src)
assert m_ex, "未提取到交付豁免块（_deliver_ti 开头; 缩进/写法变了?）"
ex_block = m_ex.group(0)

# ② 退避/计数清零函数（真实 def）
m_clear = re.search(
    r'(?ms)^def __clear_broker_empty_backoff\(g\):\n.*?(?=^def )', ghost_src)
assert m_clear, "未提取到 __clear_broker_empty_backoff"
clear_block = m_clear.group(0)

# ③ 无可动作项处理函数（真实 def）
m_note = re.search(
    r'(?ms)^def __note_no_action\(robot_object, g, need_token\):\n.*?(?=^def )', ghost_src)
assert m_note, "未提取到 __note_no_action"
note_block = m_note.group(0)

# ④ 服务端次数校准函数（真实 def）
m_done = re.search(
    r'(?ms)^def __srv_ghost_done\(store\):\n.*?(?=^def )', ghost_src)
assert m_done, "未提取到 __srv_ghost_done"
done_block = m_done.group(0)

# ⑤ first is not None 分支块（拿到可动作项）
#   注: 用 find 切片而非 `(?:.*\n)*?` 正则 —— DOTALL 下后者会灾难性回溯(实测挂死)。
i_first = ghost_src.find("\tif first is not None:\n")
assert i_first >= 0, "未提取到 first is not None 分支块"
_first_end_marker = '\t\t\t"data": {"option_index": first},\n\t\t})\n'
i_first_end = ghost_src.find(_first_end_marker, i_first)
assert i_first_end > 0, "未找到 first 分支结束锚点"
first_block = ghost_src[i_first:i_first_end + len(_first_end_marker)]

# ⑥ on_add_task（任务派发/接到捉鬼任务）里的清零调用点
m_add = re.search(r'(?ms)^def on_add_task\(robot_object, datalist\):\n.*?(?=^def )', ghost_src)
assert m_add, "未提取到 on_add_task"
add_block = m_add.group(0)

# ⑦ 2026-09-23 交付"无进展"误报修复相关块:
#    - SUBMIT 状态机内的活性判定/计数/超时自处理块(源码提取做动态用例)
i_sub = ghost_src.find("\t\tif __submit_nav_busy(quest):\n")
assert i_sub >= 0, "未提取到 SUBMIT 活性判定块(__submit_nav_busy 调用点; 缩进/写法变了?)"
_sub_end_marker = "\t\treturn 0\t# SUBMIT 分支终点"
i_sub_end = ghost_src.find(_sub_end_marker, i_sub)
assert i_sub_end > 0, "未找到 SUBMIT 分支终点锚点"
sub_block = ghost_src[i_sub:ghost_src.find("\n", i_sub_end) + 1]

#    - __submit_nav_busy 真实 def
m_nav = re.search(r'(?ms)^def __submit_nav_busy\(quest\):\n.*?(?=^def )', ghost_src)
assert m_nav, "未提取到 __submit_nav_busy"
nav_block = m_nav.group(0)

#    - __set_state 真实 def(进入 SUBMIT 记 submit_since_ms)
m_sst = re.search(r'(?ms)^def __set_state\(g, state, now_ms=None\):\n.*?(?=^def )', ghost_src)
assert m_sst, "未提取到 __set_state"
setstate_block = m_sst.group(0)

#    - on_finish_task / on_load_task(交付推进清零 submit_rounds)
m_fin = re.search(r'(?ms)^def on_finish_task\(robot_object, datalist\):\n.*?(?=^def )', ghost_src)
assert m_fin, "未提取到 on_finish_task"
fin_block = m_fin.group(0)
m_load = re.search(r'(?ms)^def on_load_task\(robot_object, datalist\):\n.*?(?=^def )', ghost_src)
assert m_load, "未提取到 on_load_task"
load_block = m_load.group(0)
#    - GhostState.__init__ / reset(新会话清零)
m_init = re.search(r'(?ms)^class GhostState\(object\):\n.*?def reset\(self\):', ghost_src)
assert m_init, "未提取到 GhostState.__init__"
init_block = m_init.group(0)
m_reset = re.search(r'(?ms)^\tdef reset\(self\):\n.*?(?=^\tdef )', ghost_src)
assert m_reset, "未提取到 GhostState.reset"
reset_block = m_reset.group(0)
#    - 通用看门狗块(确认 SUBMIT 分支不落入: 由 sub_block 终点 return 0 保证)
i_wd = ghost_src.find("\t# 看门狗超时 → 重试")
assert i_wd > 0, "未提取到通用看门狗块"
watchdog_block = ghost_src[i_wd:i_wd + 4000]


# ================================================================ 静态断言
# S1: 豁免判据 = 0 选项 + 捉鬼任务号（非 0）
check("S1 豁免判据=0 选项 + 捉鬼 task_index(is_ghost_task 且 >0)",
      "len(option_list) == 0" in ex_block
      and "is_ghost_task(int(task_index))" in ex_block
      and "int(task_index) > 0" in ex_block)

# S2: 豁免处置 = 清计数/退避 + 日志 + 提前 return
check("S2 豁免处置=__clear_broker_empty_backoff + info 日志 + return True",
      "__clear_broker_empty_backoff(g)" in ex_block
      and "视为交付成功, 不计空菜单" in ex_block
      and ex_block.rstrip().endswith("return True"))

# S3: 豁免块位于 __note_no_action 之前（否则先累加再豁免 = 无效）
i_ex = ghost_src.find(ex_block)
i_note_call = ghost_src.find("if __note_no_action(robot_object, g, need_token):", i_ex)
check("S3 豁免在 __note_no_action 调用之前（先判交付, 再判空菜单）",
      0 <= i_ex and i_ex < i_note_call, "ex@%s note@%s" % (i_ex, i_note_call))

# S4: 清零函数体内的 no_action_rounds 归零（"连续"语义的唯一咽喉点）
check("S4 __clear_broker_empty_backoff 内 g.no_action_rounds = 0",
      "g.no_action_rounds = 0" in clear_block)

# S5: 正常动作项分支显式清零
check("S5 first is not None 分支清零 no_action_rounds",
      "__clear_broker_empty_backoff(g)" in first_block
      and "g.no_action_rounds = 0" in first_block)

# S6: 任务正常派发路径（接到捉鬼任务）清零
check("S6 on_add_task(接到捉鬼任务) 走 __clear_broker_empty_backoff 清零",
      "__clear_broker_empty_backoff(g)" in add_block)

# S7: 删除盲试协议放弃（C2S_DROP_TASK 对 20195 系列必然被服务端拒）
check("S7 盲试协议放弃已删除（无 send_message(C2S_DROP_TASK) / drop_blind_idx）",
      "C2S_DROP_TASK" not in ghost_src.replace(
          '# 2026-09-22 删除"第三形态自愈: 协议放弃并重试接取"(C2S_DROP_TASK 盲试):', '')
      and "drop_blind_idx" not in ghost_src
      and "抓鬼自愈(盲试)" not in ghost_src)

# S8: 满额校准扫全段 + 不再写死键
check("S8 校准扫 GHOST_TASK_MIN~GHOST_TASK_MAX, 无写死 2019510 查表",
      "range(GHOST_TASK_MIN, GHOST_TASK_MAX + 1)" in done_block
      and '_tl.get("2019510")' not in ghost_src
      and "store.get(GHOST_DAILY_ROOT)" not in ghost_src
      and "GHOST_DAILY_ROOT" not in ghost_src)

# S9: 启动校准与增量日志都改用新函数
# 2026-09-23 跨夜修复后启动块改为三态规则, 调用形式变为内联 getattr(见
#   tools/ghost_crossday_selftest.py 的 D4~D9 动态用例), 这里只要求"确实在用它"。
check("S9 启动校准/增量日志改用 __srv_ghost_done",
      ghost_src.count("__srv_ghost_done(") >= 2
      and "__srv_ghost_done(getattr(robot_object" in ghost_src)

# ---------------- 2026-09-23 交付"无进展"误报修复 ----------------
# S10: 独立硬上限常量存在（与 NAV 跨图豁免同口径 120s）
check("S10 SUBMIT_NAV_LIMIT_MS = 120000 存在",
      "SUBMIT_NAV_LIMIT_MS = 120000" in ghost_src)

# S11: 无进展判定不再用跨状态共享的 rounds, 改用独立 submit_rounds
check("S11 SUBMIT 分支无进展判据改用 submit_rounds(不用 g.rounds >= 2)",
      "int(getattr(g, \"submit_rounds\", 0)) >= 2" in sub_block
      and "g.rounds >= 2" not in sub_block)

# S12: 导航活跃豁免在无进展判定之前(顺序: 先豁免, 再判无进展)
_i_nb = sub_block.find("if __submit_nav_busy(quest):")
_i_np = sub_block.find("submit_rounds\", 0)) >= 2")
check("S12 活性豁免块位于无进展判定之前",
      0 <= _i_nb < _i_np, "nav_busy@%s no_progress@%s" % (_i_nb, _i_np))

# S13: SUBMIT 分支自处理 20s 超时且以 return 0 收尾 —— 不再落入通用看门狗
#      (通用看门狗会 +rounds 并可能 __stuck, 且不会累计 submit_rounds)
check("S13 SUBMIT 分支自处理超时(__timeout 在本块) 且终点 return 0 不落入通用看门狗",
      "__timeout(g)" in sub_block
      and sub_block.rstrip().endswith("# SUBMIT 分支终点: 不落入通用看门狗(后者会动 rounds)"))

# S14: 交付推进(任务到达/交付完成/重登加载)清零 submit_rounds; __set_state 进入
#      SUBMIT **不得**清零(否则"两轮超时→无进展"跨 READY 往返永远攒不满)
check("S14 推进清零: on_add_task/on_finish_task/on_load_task 各清 submit_rounds",
      "g.submit_rounds = 0" in add_block
      and "g.submit_rounds = 0" in fin_block
      and "g.submit_rounds = 0" in load_block)
check("S14b __set_state 进入 SUBMIT: 记 submit_since_ms, 不清 submit_rounds",
      "g.submit_since_ms = g.state_since_ms" in setstate_block
      and "g.submit_rounds = 0" not in setstate_block)

# S15: 新会话(__init__/reset)初始化双双存在
check("S15 GhostState.__init__ / reset 均初始化 submit_rounds",
      "self.submit_rounds = 0" in init_block
      and "self.submit_rounds = 0" in reset_block)

# S16: 通用看门狗块不再需要 SUBMIT 特判(sub_block 已全部 return); 反向保护:
#      看门狗块不得出现 SUBMIT 的 rounds 判定(避免两处口径打架)
check("S16 通用看门狗块内无 SUBMIT 专有 rounds 判定",
      "SUBMIT" not in watchdog_block.replace("SUBMIT_FAIL_LIMIT", ""))


# ================================================================ 动态执行
def wrap_fn(name, body, params):
    """把 tab 缩进的代码块包成可执行函数（return 需在函数内）。"""
    lines = ["    " + l.replace("\t", "    ") if l.strip() else ""
             for l in body.splitlines()]
    code = "def %s(%s):\n%s\n" % (name, ", ".join(params), "\n".join(lines))
    return code


# fake config: 生产口径 robot_ghost_use_token=False（决定"空菜单 → NO_ACTION"分支）
_fake_cfg = types.ModuleType("config")
_fake_cfg.robot_ghost_use_token = False
sys.modules["config"] = _fake_cfg


class FakeQuest(object):
    def __init__(self):
        self.dialog_open = True
        self.dialog = None
        self.pending = None
        self.walk_target = None
        self.dijkstra_route = []
        self.tasks = {}


class FakeG(object):
    def __init__(self):
        self.no_action_rounds = 0
        self.no_action_retry = 0
        self.no_action_retry_at = 0
        self.state = "SUBMIT"
        self.done_count = 0
        self.daily_limit = 50
        self.broker_menu_streak = 0
        self.broker_menu_fp = None
        self.abandon_tries = 0
        self.submit_fail = 0
        self.token_no_money = False
        self.token_fail = 0
        self.token_selfheal_rounds = 0
        self.token_selfheal_ts = 0


CALLS = {"offline": 0, "log": [], "note": 0, "relogin": 0, "state": [], "goto": 0}


def build_ns():
    ns = {
        "NO_ACTION_OFFLINE_LIMIT": 2,
        "SELFHEAL_MAX": 2,
        "SELFHEAL_MIN_SEC": 300,
        "SELFHEAL_RETRY_DELAY_MS": 180000,
        "TOKEN_FAIL_MAX": 3,
        "__now_ms": lambda: 5000000,
        "__log": lambda ro, lv, msg, *a: CALLS["log"].append(msg % a if a else msg),
        "__emit": lambda *a, **k: None,
        "__set_state": lambda g, s, n=None: (CALLS["state"].append(s), setattr(g, "state", s)),
        "__goto": lambda *a, **k: CALLS.__setitem__("goto", CALLS["goto"] + 1),
        "__emit_ghost_offline": lambda *a, **k: CALLS.__setitem__("offline", CALLS["offline"] + 1),
        "__need_relogin": lambda *a, **k: CALLS.__setitem__("relogin", CALLS["relogin"] + 1),
        "welfare_tick": lambda *a, **k: None,
        "GHOST_TASK_MIN": 2019501,
        "GHOST_TASK_MAX": 2019513,
    }
    return ns


NS = build_ns()
exec(compile(clear_block, "<clear_backoff>", "exec"), NS)
exec(compile(note_block, "<note_no_action>", "exec"), NS)
exec(compile(done_block, "<srv_ghost_done>", "exec"), NS)
CLEAR = NS["__clear_broker_empty_backoff"]
NOTE = NS["__note_no_action"]
SRV_DONE = NS["__srv_ghost_done"]

# 豁免块函数（真实源码块; is_ghost_task 用源码同款判据实现）
NS["is_ghost_task"] = lambda ti: ti is not None and 2019501 <= int(ti) <= 2019513
exec(compile(wrap_fn("_exempt", ex_block, [
    "option_list", "task_index", "g", "robot_object",
    "is_ghost_task", "__clear_broker_empty_backoff", "__log"]), "<exempt>", "exec"), NS)
EXEMPT = NS["_exempt"]


def run_exempt(opts, ti, g=None):
    g = g or FakeG()
    CALLS["note"] = 0
    before_note = CALLS["note"]
    res = EXEMPT(opts, ti, g, "robot", NS["is_ghost_task"], CLEAR, NS["__log"])
    # 豁免返回后, 真实代码会走到 __note_no_action; 这里模拟"未豁免则记账"以验证分支
    if res is not True:
        CALLS["note"] += 1
    return res, g, CALLS["note"] != before_note


# ---------------- ① 交付成功对话豁免
CALLS["offline"] = 0
g = FakeG()
g.no_action_rounds = 2                       # 旧口径: 已是第 2 次 → 修复前直接下线
res, g, noted = run_exempt([], 2019511, g)
check("① 0选项+2019511 → 豁免(return True), rounds 清 0, 不记账, 不下线",
      res is True and g.no_action_rounds == 0 and noted is False
      and CALLS["offline"] == 0
      and any("视为交付成功" in m for m in CALLS["log"]),
      "res=%s rounds=%s noted=%s offline=%d" % (
          res, g.no_action_rounds, noted, CALLS["offline"]))

# 边界: 其它捉鬼任务号同样豁免; 非捉鬼号不豁免
res2, g2, _ = run_exempt([], 2019501)
res3, g3, _ = run_exempt([], 2019513)
res4, g4, noted4 = run_exempt([], 2019600)
check("①b 边界: 2019501/2019513 豁免; 2019600 不豁免",
      res2 is True and res3 is True and res4 is None and noted4 is True,
      "res=%s/%s/%s" % (res2, res3, res4))

# ---------------- ② 真空菜单（task_index=0）不误豁免
res5, g5, noted5 = run_exempt([], 0)
off_before = CALLS["offline"]
r5 = NOTE("robot", g5, False)                # 真实 __note_no_action: 第 1 次 → 记账不换号
check("② 0选项+task_index=0 → 不豁免, 仍走 __note_no_action(第 1 次, 不下线)",
      res5 is None and noted5 is True and g5.no_action_rounds == 1
      and CALLS["offline"] == off_before and r5 is False,
      "res=%s rounds=%s offline=%d" % (res5, g5.no_action_rounds, CALLS["offline"]))

# ②b 真·空菜单连续第 2 次 → 仍会下线（兜底未被削弱）
r6 = NOTE("robot", g5, False)
check("②b 真空菜单连续第 2 次 → __emit_ghost_offline 触发(兜底保留)",
      g5.no_action_rounds == 2 and CALLS["offline"] == off_before + 1,
      "rounds=%s offline=%d" % (g5.no_action_rounds, CALLS["offline"]))

# ---------------- ③ 两次交付场景（★核心反例）
# 修复后: 一个会话做两次鬼 + 两次交付 → 全程豁免, 不记账, 不下线
CALLS["offline"] = 0
g = FakeG()
for _ in range(2):
    run_exempt([], 2019511, g)               # 交付 #1 / #2
    g.no_action_rounds = g.no_action_rounds  # 无变化(豁免已清零)
check("③ 两次交付 → 不触发 __emit_ghost_offline(修复后实际行为)",
      g.no_action_rounds == 0 and CALLS["offline"] == 0,
      "rounds=%s offline=%d" % (g.no_action_rounds, CALLS["offline"]))

# 反例: 旧逻辑(交付对话直接进 __note_no_action)同输入必然下线
CALLS["offline"] = 0
g_old = FakeG()
NOTE("robot", g_old, False)                  # 第 1 次交付(旧: 记 1, 退避)
NOTE("robot", g_old, False)                  # 第 2 次交付(旧: 记 2 → 下线)
check("③-反例 旧逻辑两次交付 → 第 2 次即 __emit_ghost_offline（证明修复必要且可被回归捕获）",
      g_old.no_action_rounds == 2 and CALLS["offline"] == 1,
      "rounds=%s offline=%d" % (g_old.no_action_rounds, CALLS["offline"]))

# ---------------- ④ "连续"语义: 正常推进即清零
g4 = FakeG()
g4.no_action_rounds = 2
CLEAR(g4)
check("④ __clear_broker_empty_backoff 清零 no_action_rounds(2→0)",
      g4.no_action_rounds == 0 and g4.no_action_retry == 0 and g4.no_action_retry_at == 0)

# ④b 清零后再次空菜单 → 从第 1 次重新算(不再一碰就下线)
CALLS["offline"] = 0
r = NOTE("robot", g4, False)
check("④b 清零后遇真空菜单 → 重新从第 1 次算(不下线)",
      g4.no_action_rounds == 1 and r is False and CALLS["offline"] == 0,
      "rounds=%s offline=%d" % (g4.no_action_rounds, CALLS["offline"]))

# ---------------- ⑤ 满额校准(episode_root 漂移)
s_behind = {"2015915": 0, "2019508": 50, "2019511": 50}   # 现场 robot0001050 形态
s_mid = {"2019502": 46, "2019511": 46}                     # 现场 robot0001097 形态
check("⑤ 扫全段取最大: er=2019508/2019511 → 50(满额) / er=2019502 → 46",
      SRV_DONE(s_behind) == 50 and SRV_DONE(s_mid) == 46,
      "v=%s/%s" % (SRV_DONE(s_behind), SRV_DONE(s_mid)))

check("⑤b 容错: 空表→None; 脏值跳过; 取最大而非首个",
      SRV_DONE({}) is None and SRV_DONE(None) is None
      and SRV_DONE({"2019501": "x", "2019502": "7"}) == 7
      and SRV_DONE({"2019501": 3, "2019510": 9}) == 9,
      "v=%s" % SRV_DONE({"2019501": 3, "2019510": 9}))

# 反例: 旧实现写死 2019510 → 同输入判 0(满额号被当"已抓 0"重新拉起)
old_val = s_behind.get("2019510")
check("⑤-反例 旧实现(写死 2019510)同输入 → None/0(满额号被误判未抓) → 证明修复必要",
      old_val is None and SRV_DONE(s_behind) == 50)


# ================================================================ 2026-09-23 交付"无进展"动态用例
# 现场(2026-09-23): "SUBMIT 交付第 1 次无进展" 7709 次 / 402 号。根因:
#   旧判据 `g.rounds >= 2` 用的是跨状态共享看门狗计数 —— NAV 追鬼/等刷鬼/FIGHT
#   等把 rounds 带进 SUBMIT 后, 进入后下一 tick 立即误报(实测 98% 的"第 1 次无进展"
#   发生时 5 分钟内连一次 SUBMIT 超时都没有)。修复: 独立 submit_rounds +
#   导航活跃豁免(SUBMIT_NAV_LIMIT_MS 硬上限)。
class FG(object):
    def __init__(self):
        self.state = "SUBMIT"
        self.state_since_ms = 0
        self.submit_since_ms = 0
        self.submit_rounds = 0
        self.submit_fail = 0
        self.rounds = 0
        self.dynamic_npcs = {}


class FQ(object):
    def __init__(self):
        self.pending = None
        self.walk_target = None
        self.dijkstra_route = []
        self.dijkstra_final = None


NOW2 = {"v": 0}
LOG2 = []
CALL2 = {"relogin": 0, "goto": 0, "reason": ""}


def f_timeout2(g):
    return (NOW2["v"] - g.state_since_ms) > 20000


def f_set_state2(g, s, n=None):
    if g.state != s:
        g.state = s
        g.state_since_ms = n if n is not None else NOW2["v"]


def f_log2(ro, lv, msg, *a):
    LOG2.append(msg % a if a else msg)


def f_relogin2(ro, g, reason):
    CALL2["relogin"] += 1
    CALL2["reason"] = reason
    return True


def f_goto2(*a, **k):
    CALL2["goto"] += 1


NS2 = {
    "SUBMIT_NAV_LIMIT_MS": 120000,
    "SUBMIT_FAIL_LIMIT": 3,
    "BROKER_NPC_ID": 10146,
    "__now_ms": lambda: NOW2["v"],
    "__timeout": f_timeout2,
    "__log": f_log2,
    "__set_state": f_set_state2,
    "__goto": f_goto2,
    "__need_relogin": f_relogin2,
}
exec(compile(nav_block, "<nav_busy>", "exec"), NS2)
exec(compile(setstate_block, "<set_state>", "exec"), NS2)
exec(compile(wrap_fn("_submit_tick", sub_block, [
    "g", "quest", "robot_object", "now_ms", "SUBMIT_NAV_LIMIT_MS", "SUBMIT_FAIL_LIMIT",
    "BROKER_NPC_ID", "__timeout", "__submit_nav_busy", "__log", "__set_state",
    "__goto", "__need_relogin"]), "<submit_tick>", "exec"), NS2)
NAVBUSY2 = NS2["__submit_nav_busy"]
SETSTATE2 = NS2["__set_state"]
TICK2 = NS2["_submit_tick"]


def run_tick(g, q, now):
    NOW2["v"] = now
    LOG2[:] = []
    CALL2["relogin"] = 0
    CALL2["goto"] = 0
    CALL2["reason"] = ""
    return TICK2(g, q, "robot", now, NS2["SUBMIT_NAV_LIMIT_MS"],
                 NS2["SUBMIT_FAIL_LIMIT"], NS2["BROKER_NPC_ID"],
                 f_timeout2, NAVBUSY2, f_log2, f_set_state2, f_goto2, f_relogin2)


def has_np():
    return any("无进展" in m for m in LOG2)


# ---------------- D1 ★核心反例: rounds 残留(旧实现立即误报)
now = 1000000
g = FG()
g.rounds = 2                      # 模拟 NAV 追鬼两次超时带进来的脏值(现场 11:02 案例)
g.state_since_ms = now - 1000
g.submit_since_ms = now - 1000
r = run_tick(g, FQ(), now)
check("D1 rounds 残留(=2)进入 SUBMIT 首个 tick → 不误报无进展(修复后)",
      r == 0 and g.submit_fail == 0 and g.submit_rounds == 0 and not has_np(),
      "fail=%s rounds=%s log=%s" % (g.submit_fail, g.submit_rounds, LOG2[:1]))
check("D1-反例 旧判据(g.rounds >= 2)同输入 → 必报无进展(证明修复必要)",
      g.rounds >= 2)

# ---------------- D2 rounds 残留 + 20s 到点: 只计 1 轮, 回 READY 重推
now = 2000000
g = FG()
g.rounds = 2
g.state_since_ms = now - 25000
g.submit_since_ms = now - 25000
r = run_tick(g, FQ(), now)
check("D2 rounds 残留 + 20s 到点 → submit_rounds=1/回 READY/仍不报无进展",
      r == 0 and g.submit_rounds == 1 and g.state == "READY"
      and g.submit_fail == 0 and not has_np()
      and any("SUBMIT 超时, 第 1 次重试" in m for m in LOG2),
      "state=%s rounds=%s" % (g.state, g.submit_rounds))

# ---------------- D3 导航活跃豁免(走位途中不判无进展)
now = 3000000
g = FG()
g.submit_rounds = 2
g.state_since_ms = now - 30000
g.submit_since_ms = now - 30000
q = FQ()
q.walk_target = (100, 200)
r = run_tick(g, q, now)
check("D3 导航活跃(walk_target) + 已两轮 → 豁免: 不报无进展, 刷新看门狗计时",
      r == 0 and g.submit_fail == 0 and g.submit_rounds == 2
      and g.state_since_ms == now and not has_np())

# ---------------- D4 硬上限: 导航活跃超 120s → 回原无进展兜底(并清导航重推)
now = 4000000
g = FG()
g.submit_rounds = 2
g.state_since_ms = now - 30000
g.submit_since_ms = now - 130000
q = FQ()
q.walk_target = (100, 200)
r = run_tick(g, q, now)
check("D4 导航活跃但超 120s 硬上限 → 报第 1 次无进展, 清导航重推(goto)",
      r == 0 and g.submit_fail == 1 and g.submit_rounds == 0
      and q.walk_target is None and CALL2["goto"] == 1
      and any("SUBMIT 交付第 1 次无进展" in m for m in LOG2),
      "fail=%s goto=%d" % (g.submit_fail, CALL2["goto"]))

# ---------------- D5 真卡死(导航已停 + 两轮) → 兜底保留
now = 5000000
g = FG()
g.submit_rounds = 2
g.state_since_ms = now - 30000
g.submit_since_ms = now - 60000
r = run_tick(g, FQ(), now)
check("D5 导航已停 + 两轮计数 → 报第 1 次无进展(原兜底未被削弱)",
      g.submit_fail == 1 and any("第 1 次无进展" in m for m in LOG2))

# ---------------- D6 20s 超时自处理: submit_rounds+1 回 READY(原节奏)
now = 6000000
g = FG()
g.state_since_ms = now - 21000
g.submit_since_ms = now - 21000
r = run_tick(g, FQ(), now)
check("D6 20s 超时 → submit_rounds=1/回 READY/日志'第 1 次重试'/不报无进展",
      g.submit_rounds == 1 and g.state == "READY" and g.submit_fail == 0
      and not has_np()
      and any("抓鬼状态 SUBMIT 超时, 第 1 次重试" in m for m in LOG2))

# ---------------- D7 重登通道保留(submit_fail 达上限)
now = 7000000
g = FG()
g.submit_rounds = 2
g.submit_fail = 2
g.state_since_ms = now - 30000
g.submit_since_ms = now - 60000
r = run_tick(g, FQ(), now)
check("D7 submit_fail 达 SUBMIT_FAIL_LIMIT → __need_relogin(重登通道保留)",
      CALL2["relogin"] == 1 and g.submit_fail == 3
      and "连续 3 次无进展" in CALL2["reason"])

# ---------------- D8 __set_state 真行为: 记 submit_since_ms, 不清 submit_rounds
g = FG()
g.state = "READY"
g.state_since_ms = 0
g.submit_rounds = 1
NOW2["v"] = 8000000
SETSTATE2(g, "SUBMIT", 8000000)
check("D8 __set_state 进入 SUBMIT: submit_since_ms=进入时刻, submit_rounds 保留(不清)",
      g.submit_since_ms == 8000000 and g.submit_rounds == 1 and g.state == "SUBMIT")


# ---------------- D9 __submit_nav_busy 判据(含设计约束: pending 不算活跃)
def _nb(pending=None, walk=None, route=None, final=None):
    q = FQ()
    q.pending = pending
    q.walk_target = walk
    q.dijkstra_route = route or []
    q.dijkstra_final = final
    return NAVBUSY2(q)


check("D9 判据: walk_target/dijkstra_route/dijkstra_final 命中; pending 不命中(点后等对话严判); None→False",
      _nb(walk=(1, 2)) is True and _nb(route=[{"a": 1}]) is True
      and _nb(final={"npc_id": 10146}) is True
      and _nb(pending={"type": "click"}) is False
      and _nb() is False and NAVBUSY2(None) is False)


# ---------------- D10 跨 READY 往返两轮超时 → "第 1 次无进展"依然可达(计数不被抹)
now = 9000000
g = FG()
g.state_since_ms = now - 21000
g.submit_since_ms = now - 21000
q = FQ()
run_tick(g, q, now)                        # 第 1 轮超时 → submit_rounds=1, READY
NOW2["v"] = now + 100
SETSTATE2(g, "SUBMIT", now + 100)          # READY 立即回 SUBMIT(生产同序)
g.state_since_ms = now + 100
NOW2["v"] = now + 21100
run_tick(g, q, now + 21100)                # 第 2 轮超时 → submit_rounds=2, READY
NOW2["v"] = now + 21200
SETSTATE2(g, "SUBMIT", now + 21200)
g.state_since_ms = now + 21200
LOG2[:] = []
run_tick(g, q, now + 21200)                # 下一 tick → 报无进展
check("D10 跨 READY 往返两轮超时 → 第 1 次无进展可达(计数未被 __set_state 抹掉)",
      g.submit_fail == 1 and any("第 1 次无进展" in m for m in LOG2),
      "fail=%s log=%s" % (g.submit_fail, LOG2[:1]))

# ================================================================ 结果
print()
print("自检目标: %s" % script_dir)
print("  %s sha1=%s" % (os.path.basename(ghost_path),
                        hashlib.sha1(ghost_src.encode("utf-8")).hexdigest()[:12]))
total = 17 + 22
print("结果：%d 项，失败 %d 项" % (total, fails))
sys.exit(1 if fails else 0)
