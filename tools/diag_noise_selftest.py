# -*- coding: utf-8 -*-
"""diag.log 降噪 自检（2026-09-23）。

背景：diag.log 一天 200MB×3 归档。抽样（尾部 3MB）统计：`process_msg` 的逐包跟踪
（每包 4 行：>>> recv msgid= / handler for / calling handler / handler returned OK）
占行数 ~89%；其中高频"广播/移动"包（role_move 90341 / npc_stop 90381 /
npc_move 90060 / add_npc 90107 / fighter_ready 90216）≈ 全包的 92%。

修复（client.py `_DIAG_MUTE_MSGIDS`）：静默这 5 类包的逐包跟踪，其余包照记。
**不影响**：异常分支（handler RAISED / traceback）、活性打点（m_last_srv_msg_ms）、
proto_log（关键协议回包日志）。

动态用例：提取真实 `process_msg` 源码执行（mock 依赖），验证
  静默包 0 行 / 业务包 4 行 / 静默+异常仍打 RAISED / 静默不影响活性打点。

用法：python tools/diag_noise_selftest.py [script_dir]
"""
import io
import os
import re
import sys
import types
from unittest.mock import MagicMock

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_DIR = os.path.normpath(os.path.join(
    HERE, "..", "deploy", "zones", "prod-240-2300", "script"))
script_dir = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_DIR
cl = open(os.path.join(script_dir, "client.py"), encoding="utf-8").read()

fails = 0
total = 0


def check(name, ok, detail=""):
    global fails, total
    total += 1
    fails += 0 if ok else 1
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  " + detail) if detail else ""))


# ================================================================ 静态
check("S1 静默集合 _DIAG_MUTE_MSGIDS 存在",
      "_DIAG_MUTE_MSGIDS = frozenset(" in cl)
for mid in (90341, 90381, 90060, 90107, 90216):
    check("S2 含高频包 %d" % mid, str(mid) in cl.split("_DIAG_MUTE_MSGIDS = frozenset(")[1].split(")")[0])
check("S3 四处逐包跟踪都受 _quiet 控制",
      cl.count("if not _quiet:") == 4)
check("S4 异常分支不静默（RAISED 仍打）",
      "import traceback" in cl and '_diag("    handler RAISED: ' in cl)

# ================================================================ 动态：提取 process_msg 执行
m = re.search(r"(?ms)^def process_msg\(fd, msgid, data\):.*?(?=^def )", cl)
if not m:
    check("D0 提取 process_msg", False, "未匹配")
else:
    mute = frozenset((90341, 90381, 90060, 90107, 90216))
    calls = []
    ro = types.SimpleNamespace(m_last_srv_msg_ms=0)
    handlers = {}

    class _G:
        m_robots = {7: ro}

    ns = {
        "_DIAG_MUTE_MSGIDS": mute,
        "_diag": lambda msg: calls.append(msg),
        "robot_mgr": types.SimpleNamespace(g_mgr=_G()),
        "time": __import__("time"),
        "proto_log": MagicMock(),
        "protocol3": types.SimpleNamespace(g_handle_map=handlers),
        "msghandle": types.SimpleNamespace(default_handle=lambda fd, data: None),
        "error_handle": lambda: None,
    }
    sys.modules.setdefault("proto_log", ns["proto_log"])
    ns["proto_log"] = ns["proto_log"]
    exec(compile(m.group(0), "<process_msg>", "exec"), ns)  # noqa: S102 —— 自检专用
    fn = ns["process_msg"]

    # D1 静默包（role_move 90341）→ 0 行
    calls.clear()
    handlers[90341] = lambda fd, data: None
    fn(7, 90341, [])
    check("D1 静默包 90341 → _diag 0 行", len(calls) == 0, "calls=%d" % len(calls))

    # D2 静默包（fighter_ready 90216）→ 0 行
    calls.clear()
    handlers[90216] = lambda fd, data: None
    fn(7, 90216, [])
    check("D2 静默包 90216 → _diag 0 行", len(calls) == 0)

    # D3 业务包（如 90203 fight_end）→ 4 行（recv/handler/calling/returned）
    calls.clear()
    handlers[90203] = lambda fd, data: None
    fn(7, 90203, [])
    check("D3 业务包 90203 → 4 行跟踪", len(calls) == 4,
          "lines=%s" % [c.strip()[:24] for c in calls])

    # D4 静默包 + handler 抛异常 → RAISED 仍打出
    calls.clear()
    def _boom(fd, data):
        raise ValueError("boom")
    handlers[90341] = _boom
    fn(7, 90341, [])
    check("D4 静默包异常仍打 RAISED",
          any("RAISED" in c for c in calls), "calls=%d" % len(calls))

    # D5 静默包不影响活性打点
    ro.m_last_srv_msg_ms = 0
    handlers[90341] = lambda fd, data: None
    fn(7, 90341, [])
    check("D5 静默包仍更新 m_last_srv_msg_ms（活性打点）",
          ro.m_last_srv_msg_ms > 0)

# ================================================================ 汇总
print("\n自检目标: %s" % script_dir)
print("结果：%d 项，失败 %d 项" % (total, fails))
sys.exit(1 if fails else 0)
