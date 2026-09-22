# -*- coding: utf-8 -*-
"""发包限流自检（2026-09-21）：钟馗点击节流 + 跨图被拒退避。

背景（当天全量数据实测，16:34-19:54 约 200 分钟 / 97 个抓鬼号）：
  1) 同号相邻两次「点击NPC 10146」间隔 6.9% <1 秒，存在同秒 8~13 连点簇 —— 既发
     无效重复包，又快速消耗 broker_nodlg 计数（同秒连点 8 次无对话即误判"服务端
     对话卡死"触发重登）。
  2) 同号相邻两次「跨图跳转被拒绝」97% 间隔 <2 秒（中位数 1 秒）—— 被拒后 300ms
     立即重走/重发，位置未变又被拒，1 秒 1 次连发 DIJKSTRA。
  3) 拉黑后"换跳转点重新规划"原为立即执行（10681 次/200 分钟），常见"拒-黑-换"循环。

修复（本文件校验的就是上线那份源码）：
  A. daily_ghost.__goto 钟馗分支：两次"发射钟馗点击"最小间隔
     BROKER_CLICK_MIN_GAP_MS(默认 1200ms)；被拦的点击不发包、不计 broker_nodlg。
  B. quest_engine：被拒重试/拉黑换路叠加递增退避 __hop_retry_delay_ms
     （第 1/2/3+ 次 = 1s/3s/6s）。

本脚本为**源码级自检**（不 import 运行时依赖）：正则抽取真实源码块直接执行。
用法：python qq_throttle_selftest.py [daily_ghost.py 路径 [quest_engine.py 路径]]
      不带参数默认校验仓库副本。
"""
import hashlib
import os
import re
import sys
import time

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT_DIR = os.path.normpath(os.path.join(HERE, "..", "deploy", "zones", "prod-240-2300", "script"))
GHOST_PATH = sys.argv[1] if len(sys.argv) > 1 else os.path.join(SCRIPT_DIR, "daily_ghost.py")
ENGINE_PATH = sys.argv[2] if len(sys.argv) > 2 else os.path.join(os.path.dirname(os.path.abspath(GHOST_PATH)), "quest_engine.py")

ghost_src = open(GHOST_PATH, encoding="utf-8").read()
engine_src = open(ENGINE_PATH, encoding="utf-8").read()

fails = 0


def check(name, ok, detail=""):
    global fails
    fails += 0 if ok else 1
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  " + detail) if detail else ""))


# ==================================================================
# A. daily_ghost: 钟馗点击节流
# ==================================================================
CONST_A = "BROKER_CLICK_MIN_GAP_MS"
m = re.search(r'(?m)^%s\s*=\s*(\d+)' % CONST_A, ghost_src)
assert m, "缺少常量 %s" % CONST_A
GAP_DEFAULT = int(m.group(1))

m_goto = re.search(r'(?ms)^def __goto\(.*?(?=^\S)', ghost_src)
assert m_goto, "未找到 __goto 函数"
goto_src = m_goto.group(0)

# 静态 A1: 节流检查必须位于 broker_nodlg 计数之前（被拦点击不能计入"无对话"）
i_throttle = goto_src.find("BROKER_CLICK_MIN_GAP_MS")
i_nodlg = goto_src.find("__check_broker_nodlg(robot_object, g)")
check("S1 __goto 节流检查位于 broker_nodlg 计数之前",
      0 <= i_throttle < i_nodlg, "throttle@%s nodlg@%s" % (i_throttle, i_nodlg))

# 静态 A2: 节流只作用于钟馗分支
check("S2 节流在 `npc_id == BROKER_NPC_ID` 分支内",
      goto_src.find("if npc_id == BROKER_NPC_ID") < i_throttle)

# ---- 动态：提取真实 __goto 执行
ns_g = {}
exec(compile(goto_src, "<__goto>", "exec"), ns_g)

CLOCK = [10 ** 12]
LOGS = []


def now_fn():
    return CLOCK[0]


def log_fn(robot_object, level, msg):
    LOGS.append((level, msg))


class FakeQuest(object):
    pass


class FakeRobotEngine(object):
    def __init__(self):
        self.tp = []

    def _teleport_click(self, robot_object, quest, npc_id, npc_index, click_type, delay_ms):
        self.tp.append((npc_id, npc_index, click_type, delay_ms))


FakeRobotEngine.__teleport_click = FakeRobotEngine._teleport_click


class FakeG(object):
    def __init__(self, **kw):
        self.enabled = True
        self.broker_click_ts = 0
        self.broker_nodlg = 0
        for k, v in kw.items():
            setattr(self, k, v)


NODLG_CALLS = [0]


def fake_nodlg(robot_object, g):
    NODLG_CALLS[0] += 1
    g.broker_nodlg = int(getattr(g, "broker_nodlg", 0)) + 1
    return False


ns_g["BROKER_NPC_ID"] = 10146
ns_g[CONST_A] = GAP_DEFAULT
ns_g["__now_ms"] = now_fn
ns_g["__log"] = log_fn
ns_g["__quest"] = lambda r: FakeQuest()
ns_g["__check_broker_nodlg"] = fake_nodlg
ENGINE = FakeRobotEngine()
ns_g["quest_engine"] = ENGINE
goto = ns_g["__goto"]
BROKER = ns_g["BROKER_NPC_ID"]


def run_goto(g, npc_id=None, at_offset_ms=-999999):
    if at_offset_ms != -999999:
        g.broker_click_ts = CLOCK[0] + at_offset_ms
    n0 = len(ENGINE.tp); c0 = NODLG_CALLS[0]
    goto("robot", g, BROKER if npc_id is None else npc_id, BROKER if npc_id is None else npc_id, 0, 500)
    return (len(ENGINE.tp) - n0), (NODLG_CALLS[0] - c0)


del LOGS[:]
g1 = FakeG(broker_click_ts=CLOCK[0] - 500)          # 0.5s 前刚点过 → 应拦截
sent, nodlg = run_goto(g1)
check("C1 距上次 0.5s(<%.1fs) → 节流: 不发点击、不计无对话" % (GAP_DEFAULT / 1000.0),
      sent == 0 and nodlg == 0 and any("节流" in m for _l, m in LOGS) and g1.broker_nodlg == 0,
      "sent=%d nodlg=%d log=%s" % (sent, nodlg, [m for _l, m in LOGS][-1:]))

g2 = FakeG(broker_click_ts=CLOCK[0] - 2000)         # 2s 前点过 → 放行
sent, nodlg = run_goto(g2)
check("C2 距上次 2.0s(>=%.1fs) → 放行: 发点击、计 1 次无对话、刷新时间戳" % (GAP_DEFAULT / 1000.0),
      sent == 1 and nodlg == 1 and g2.broker_click_ts == CLOCK[0] and g2.broker_nodlg == 1,
      "sent=%d nodlg=%d ts=%s" % (sent, nodlg, g2.broker_click_ts))

g3 = FakeG(broker_click_ts=0)                        # 首次(无记录) → 放行
sent, nodlg = run_goto(g3)
check("C3 首次点击(无时间戳) → 放行", sent == 1 and g3.broker_click_ts == CLOCK[0],
      "sent=%d" % sent)

g4 = FakeG(broker_click_ts=CLOCK[0] - 2000)          # 参数改 5000 → 2s 也应被拦
ns_g[CONST_A] = 5000
sent, nodlg = run_goto(g4)
ns_g[CONST_A] = GAP_DEFAULT
check("C4 间隔参数可配: 调大到 5000ms 后 2s 间隔被拦", sent == 0 and nodlg == 0,
      "sent=%d nodlg=%d" % (sent, nodlg))

g5 = FakeG(broker_click_ts=CLOCK[0] - 100)           # 非钟馗 NPC → 不节流(维持原行为)
sent, nodlg = run_goto(g5, npc_id=999)
check("C5 非钟馗 NPC 不受节流(维持原行为)", sent == 1 and nodlg == 0, "sent=%d" % sent)

# ==================================================================
# B. quest_engine: 跨图被拒退避
# ==================================================================
m_bk = re.search(r'(?m)^HOP_RETRY_BACKOFF_MS\s*=\s*\(([^)]*)\)', engine_src)
assert m_bk, "缺少常量 HOP_RETRY_BACKOFF_MS"
BACKOFF = tuple(int(x.strip()) for x in m_bk.group(1).split(",") if x.strip())

parts = []
for fn in ("__hop_retry_delay_ms", "__hop_key", "__hop_fail_note"):
    mm = re.search(r'(?ms)^def %s\(.*?(?=^\S)' % re.escape(fn), engine_src)
    assert mm, "缺少函数 %s" % fn
    parts.append(mm.group(0))
for cn in ("HOP_FAIL_LIMIT", "HOP_BLACKLIST_MS"):
    mm = re.search(r'(?m)^%s\s*=.*$' % cn, engine_src)
    assert mm, "缺少常量 %s" % cn
    parts.append(mm.group(0))

ns_e = {}
exec(compile("\n".join(parts), "<quest_engine excerpt>", "exec"), ns_e)
ns_e["HOP_RETRY_BACKOFF_MS"] = BACKOFF
ns_e["time"] = time          # __hop_fail_note 拉黑用 time.time()
ns_e["__emit"] = lambda *a, **k: None
ns_e["__report_stuck"] = lambda *a, **k: None
TP_CALLS = []


def fake_teleport(robot_object, quest, npc_id, npc_index, click_type, delay_ms, **kw):
    TP_CALLS.append(delay_ms)


ns_e["__teleport_click"] = fake_teleport

hop_delay = ns_e["__hop_retry_delay_ms"]

# 静态 B1: 被拒重试的 at_ms 叠加退避
check("S3 被拒重试 at_ms 含 __hop_retry_delay_ms(hop[\"retry\"])",
      '__human_delay(300) + __hop_retry_delay_ms(hop["retry"])' in engine_src)

# 静态 B2: 拉黑换路 delay 叠加退避
check("S4 拉黑换路 delay_ms 含 __hop_retry_delay_ms(_n)",
      '__hop_retry_delay_ms(_n)' in engine_src)

# 动态 B3: 退避序列 + 封顶
_ok_seq = (hop_delay(1) == BACKOFF[0] and hop_delay(2) == BACKOFF[1]
           and hop_delay(3) == BACKOFF[2] and hop_delay(9) == BACKOFF[-1]
           and hop_delay(None) == BACKOFF[0])
check("C6 退避序列 %s(第3+次封顶)" % (BACKOFF,), _ok_seq,
      "1→%s 2→%s 3→%s 9→%s None→%s" % (hop_delay(1), hop_delay(2), hop_delay(3),
                                        hop_delay(9), hop_delay(None)))

# 动态 B4: 参数可配(替换常量后生效)
ns_e["HOP_RETRY_BACKOFF_MS"] = (200, )
check("C7 退避参数可配: 替换为 (200,) 后 retry=1 → 200",
      hop_delay(1) == 200, "got=%s" % hop_delay(1))
ns_e["HOP_RETRY_BACKOFF_MS"] = BACKOFF

# 动态 B5: __hop_fail_note 拉黑时 teleport 的 delay = 原 delay + 退避
class FakeQ(object):
    def __init__(self):
        self.hop_black = None
        self.dijkstra_plan = {"npc_id": 1, "npc_index": 1, "click_type": 0, "delay_ms": 500}
        self.dijkstra_route = []
        self.dijkstra_waiting = False
        self.dijkstra_jumper = None
        self.dijkstra_final = None


class FakeR(object):
    m_mapid = 24


del TP_CALLS[:]
q = FakeQ()
hop = {"destination_index": 77, "x": 100, "y": 100}
r1 = ns_e["__hop_fail_note"](FakeR(), q, hop, 77)     # _fail=1 <3 → 不接管
r2 = ns_e["__hop_fail_note"](FakeR(), q, hop, 77)     # _fail=2 <3 → 不接管
n_before = len(TP_CALLS)
r3 = ns_e["__hop_fail_note"](FakeR(), q, hop, 77)     # _fail=3 → 拉黑 + 换路
ok_black = (r1 is False and r2 is False and r3 is True and n_before == 0
            and len(TP_CALLS) == 1 and TP_CALLS[-1] == 500 + BACKOFF[-1])
check("C8 连拒 3 次拉黑 → 换路 delay=500+%d(退避生效), 前两次不触发" % BACKOFF[-1],
      ok_black, "r=%s/%s/%s teleport_delay=%s" % (r1, r2, r3, TP_CALLS[-1:]))

# ---------------------------------------------------------------- 结果
print("\n自检目标: daily_ghost=%s" % GHOST_PATH)
print("          quest_engine=%s" % ENGINE_PATH)
print("sha1(daily_ghost)=%s sha1(quest_engine)=%s" % (
    hashlib.sha1(ghost_src.encode("utf-8")).hexdigest()[:12],
    hashlib.sha1(engine_src.encode("utf-8")).hexdigest()[:12]))
total = 4 + 8
print("结果：%d 项，失败 %d 项" % (total, fails))
sys.exit(1 if fails else 0)
