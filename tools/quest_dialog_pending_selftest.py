# -*- coding: utf-8 -*-
"""quest_engine.show_dialog「零选项空对话」清理顺序自检（2026-09-21r2 修复）。

生产现象（48/101 个号）：重登后卡 DIALOG 一动不动，日志停在
    "对话打开: 任务 0, npc=0, 选项 0 个 []"
根因：服务端推"零选项空对话"(npc=0, 选项 0 个 []) 时的清态逻辑只写在
__handle_dialog 内（npc_id==0 块），而 show_dialog 分支的
`if quest.pending == None` 守卫使 pending 非空时永远不调用它 →
dialog_open 挂着 → 主循环 `if quest.dialog_open: return 0` 把号钉死。
现场路径：领双引导对话点"直接领取(双倍)"后 __schedule(dialog_click, +400ms)
使 pending 非空，同一秒服务端又推零选项空对话 → 守卫挡住清理。

本脚本为**源码级自检**（不 import quest_engine：它依赖机器人运行时 cnetwork 等）：
  1) 静态断言 —— 零选项清理已前置到 pending 守卫之前；清理动作与 __handle_dialog
     内原修复逐行一致（不引入新语义）；原清理块仍保留（等价路径）。
  2) 动态断言 —— 提取 show_dialog 分支与 __handle_dialog 的真实源码直接执行：
     4 条用例（pending 非空/为空 × 零选项/有选项）+ 1 条反例（删掉前置块 → 复现
     修复前的卡死）。

用法：python quest_dialog_pending_selftest.py [quest_engine.py 路径]
      不带参数默认校验仓库副本；传线上副本路径可再验一次线上文件。
"""
import hashlib
import os
import re
import sys

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_ENGINE = os.path.normpath(os.path.join(
    HERE, "..", "deploy", "zones", "prod-240-2300", "script", "quest_engine.py"))

path = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_ENGINE
src = open(path, encoding="utf-8").read()

SCRIPT_DIR = os.path.dirname(os.path.abspath(path))
sys.path.insert(0, SCRIPT_DIR)
import quest_state  # noqa: E402  纯枚举模块(只 import time), 离线可用

fails = 0


def check(name, ok, detail=""):
    global fails
    fails += 0 if ok else 1
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  " + detail) if detail else ""))


# ---------------------------------------------------------------- 源码提取
m_block = re.search(
    r'(?ms)^\telif event_name == "show_dialog":\n(.*?)(?=^\telif event_name == )', src)
assert m_block, "未找到 show_dialog 分支（缩进/写法变了?）"
block = m_block.group(1)

m_hdl = re.search(r'(?ms)^def __handle_dialog\(robot_object, quest, data\):\n.*?(?=^\S)', src)
assert m_hdl, "未找到 __handle_dialog 函数"
hdl_src = m_hdl.group(0)

FIX_COND = "if data[2] == 0 and len(data[5]) == 0:"
GUARD = "if quest.pending == None:"
ORIG_COND = "if len(option_list) == 0:"

# ---------------------------------------------------------------- 静态断言
# S1: 前置清理在 pending 守卫之前（修复的核心顺序）
i_fix, i_guard = block.find(FIX_COND), block.find(GUARD)
check("S1 前置清理在 pending 守卫之前",
      i_fix != -1 and i_guard != -1 and i_fix < i_guard,
      "fix@%s guard@%s" % (i_fix, i_guard))

# S2: 提取两处清理块，逐行比对"清态三件套"（动作必须完全一致）
m_fix_blk = re.search(
    r'(?ms)^\t\tif data\[2\] == 0 and len\(data\[5\]\) == 0:\n.*?^\t\t\treturn\n', block)
m_org_blk = re.search(r'(?ms)^\t\tif len\(option_list\) == 0:\n.*?^\t\t\treturn\n', hdl_src)
assert m_fix_blk, "未提取到前置清理块"
assert m_org_blk, "未提取到 __handle_dialog 内原清理块"
CLEAN_KEYS = ("quest.dialog_open = False", "quest.dialog = None",
              "quest.set_state(", "else quest_state.ST_IDLE)")


def clean_lines(text):
    return [l.strip() for l in text.splitlines()
            if l.strip().startswith(CLEAN_KEYS)]


fix_clean, org_clean = clean_lines(m_fix_blk.group(0)), clean_lines(m_org_blk.group(0))
check("S2 前置清理动作与原 __handle_dialog 清理逐行一致",
      fix_clean == org_clean and len(fix_clean) == 4,
      "前置=%s 原=%s" % (fix_clean, org_clean))

# S3: 原清理块未被删除（pending==None 时仍为等价路径）
check("S3 __handle_dialog 内原零选项清理块保留", ORIG_COND in hdl_src)


# ---------------------------------------------------------------- 动态执行
def build_branch_fn(body_text):
    """把 show_dialog 块体包成可执行函数（`return` 需在函数内）。"""
    lines = ["    " + l.replace("\t", "    ") if l.strip() else ""
             for l in body_text.splitlines()]
    code = ("def _branch(quest, robot_object, data, quest_state, "
            "__handle_dialog, __emit, __emit_state):\n" + "\n".join(lines) + "\n")
    ns = {}
    exec(compile(code, "<show_dialog branch>", "exec"), ns)
    return ns["_branch"]


branch = build_branch_fn(block)

# 反例用：删掉前置清理块，还原修复前的行为
old_block = re.sub(
    r'(?ms)^\t\tif data\[2\] == 0 and len\(data\[5\]\) == 0:\n.*?^\t\t\treturn\n',
    "", block)
old_branch = build_branch_fn(old_block)

ns_hdl = {"quest_state": quest_state}


class FakeQuest(object):
    def __init__(self, pending=None, active=True):
        self.dialog = None
        self.dialog_open = False
        self.npc_cand_npc_id = 7
        self.npc_cand_positions = ["stale"]
        self.npc_cand_idx = 3
        self.pending = pending
        self.active = active
        self.state = quest_state.ST_IDLE

    def set_state(self, state):
        self.state = state


def run_case(branch_fn, pending, active, data, real_handle=None):
    q = FakeQuest(pending=pending, active=active)
    calls = {"handle": 0, "events": 0, "states": 0}

    def handle(*a, **k):
        calls["handle"] += 1

    def emit(*a, **k):
        calls["events"] += 1

    def emit_state(*a, **k):
        calls["states"] += 1

    branch_fn(q, "robot", data, quest_state,
              real_handle if real_handle != None else handle, emit, emit_state)
    return q, calls


EMPTY = [0, 0, 0, "", 0, []]                                # 现场卡死那条
WITH_OPTS = [0, 0, 0, "", 0, [["直接领取（双倍）", 0], ["我什么都不想做", 0]]]
PENDING = {"type": "dialog_click", "at_ms": 1, "data": {"option_index": 0}}

# C1: pending 非空 + 零选项 npc=0 → 前置清态，且不调用 __handle_dialog
q, calls = run_case(branch, PENDING, True, EMPTY)
ok = (q.dialog_open is False and q.dialog is None
      and q.state == quest_state.ST_WAIT_NEXT and calls["handle"] == 0
      and calls["states"] >= 2)
check("C1 pending非空+零选项 → 清态(WAIT_NEXT)不调 __handle_dialog", ok,
      "open=%s dialog=%s state=%s handle=%d" % (q.dialog_open, q.dialog, q.state, calls["handle"]))
q2, _ = run_case(branch, PENDING, False, EMPTY)
check("C1b pending非空+零选项(未激活) → 清到 IDLE",
      q2.dialog_open is False and q2.state == quest_state.ST_IDLE,
      "state=%s" % q2.state)

# C2: pending 非空 + 有选项 → 仍走原守卫（不误清）
q, calls = run_case(branch, PENDING, True, WITH_OPTS)
ok = (q.dialog_open is True and q.dialog is WITH_OPTS
      and q.state == quest_state.ST_DIALOG and calls["handle"] == 0)
check("C2 pending非空+有选项 → 不误清(仍 DIALOG, 守卫生效)", ok,
      "open=%s state=%s handle=%d" % (q.dialog_open, q.state, calls["handle"]))

# C3: pending 为 None + 零选项 → 结果与 __handle_dialog 原路径完全等价
exec(compile(hdl_src, "<__handle_dialog>", "exec"), ns_hdl)
ns_hdl["__emit"] = lambda *a, **k: None
ns_hdl["__emit_state"] = lambda *a, **k: None
real_handle = ns_hdl["__handle_dialog"]

qa, ca = run_case(branch, None, True, EMPTY)
qb = FakeQuest(pending=None, active=True)
qb.dialog_open = True
qb.state = quest_state.ST_DIALOG
real_handle("robot", qb, EMPTY)
ok = (qa.dialog_open is False and qa.dialog is None
      and qa.state == qb.state == quest_state.ST_WAIT_NEXT)
check("C3 pending为None+零选项 → 与原 __handle_dialog 清理结果等价", ok,
      "新:open=%s state=%s 原:open=%s state=%s" % (
          qa.dialog_open, qa.state, qb.dialog_open, qb.state))

# C4: pending 为 None + 有选项 → 原守卫放行，仍调用 __handle_dialog
q, calls = run_case(branch, None, True, WITH_OPTS)
check("C4 pending为None+有选项 → 原守卫放行调用 __handle_dialog",
      q.dialog_open is True and calls["handle"] == 1,
      "open=%s handle=%d" % (q.dialog_open, calls["handle"]))

# C5(反例): 删掉前置块 → 复现修复前卡死（pending 非空 + 零选项无人清）
q, calls = run_case(old_branch, PENDING, True, EMPTY)
check("C5 反例: 无前置块时 pending非空+零选项 → dialog_open 挂着(复现卡死)",
      q.dialog_open is True and q.dialog is EMPTY and calls["handle"] == 0,
      "open=%s handle=%d" % (q.dialog_open, calls["handle"]))

# ---------------------------------------------------------------- 结果
print("\n自检目标: %s" % path)
print("sha1=%s" % hashlib.sha1(src.encode("utf-8")).hexdigest()[:12])
total = 3 + 6
print("结果：%d 项，失败 %d 项" % (total, fails))
sys.exit(1 if fails else 0)
