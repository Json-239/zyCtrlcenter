# -*- coding: utf-8 -*-
"""「面板误报卡住」修复自检：错误上报 TTL + 恢复即清（2026-09-21）。

生产现象（20:17 实测）：在线 48 个号中 5 个带 err_code（NO_LEGAL_ROUTE×4 / TASK_STUCK×1，
err_repeat 1~3），但 5 个全是活跃态（NAV/WAIT_GHOST）、ghost.enabled=true（在正常抓鬼）；
state=ERROR 的真卡住号 0 个 —— 面板"卡住"chip 对"err_code 非空"的号永久误报。

根因链条：
  1) 机器人端 quest_state.mark_error 记的 last_error 永不失效/永不清理；
  2) error_fields（心跳/状态上报口径）永远返回旧 err_code；
  3) 中控 event.go 只在"机器人不带错误且非 ERROR"时才清 → 机器人一直带 → 永远清不掉；
  4) 面板 Dashboard "卡住" chip = err_code 非空 → 永久误报。

修复（本文件校验的就是上线那份源码）：
  A) quest_state.ERROR_TTL_MS=3 分钟：error_fields 只在其后 TTL 内返回非空，超时返回
     ("", 0)（last_error 本体保留供本地诊断）；
  B) quest_state.clear_error()：明确恢复点恢复即清（不等 TTL 自然过期）；
  C) 恢复点取 3 处：quest_engine.finish_task、daily_ghost.on_add_task / on_finish_task；
     报错原地重试路径（看门狗 __recover / tester）不清，避免掩盖持续错误；
  D) 前端 Dashboard.vue："卡住" = err_code 且 state==='ERROR'，非 ERROR 态弱化"曾出错"。

本脚本为**源码级 + 动态**自检：
  - 动态：加载真实 quest_state 模块（时钟可注入），验证 TTL 边界 / clear_error / 兜底；
  - 源码：断言常量/方法/恢复点位置、且重试路径未被误清；
  - 兜底：提取 quest_engine._clear_recent_error 真实源码执行（旧类实例无 clear_error 不抛）。

用法：python err_ttl_selftest.py [script 目录]
      不带参数默认校验仓库副本；传线上副本目录可再验一次线上文件。
"""
import hashlib
import importlib.util
import os
import re
import sys
import time

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_DIR = os.path.normpath(os.path.join(
    HERE, "..", "deploy", "zones", "prod-240-2300", "script"))

script_dir = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_DIR
qs_path = os.path.join(script_dir, "quest_state.py")
qe_path = os.path.join(script_dir, "quest_engine.py")
dg_path = os.path.join(script_dir, "daily_ghost.py")

qs_src = open(qs_path, encoding="utf-8").read()
qe_src = open(qe_path, encoding="utf-8").read()
dg_src = open(dg_path, encoding="utf-8").read()

fails = 0


def check(name, ok, detail=""):
    global fails
    fails += 0 if ok else 1
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  " + detail) if detail else ""))


# ---------------------------------------------------------------- 模块加载
def load_module(path, name):
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


qs_real = load_module(qs_path, "quest_state_real")   # 真实时钟
qs_fake = load_module(qs_path, "quest_state_fake")   # 注入时钟(模拟流逝)

CLOCK = [10 ** 12]  # 可控毫秒时钟


class FakeTime(object):
    @staticmethod
    def time():
        return CLOCK[0] / 1000.0

    @staticmethod
    def sleep(_sec):
        pass


qs_fake.time = FakeTime

# ---------------------------------------------------------------- 源码级断言
# S1: TTL 常量存在且为 3 分钟
m_ttl = re.search(r'(?m)^ERROR_TTL_MS\s*=\s*(.+)$', qs_src)
ok_s1 = bool(m_ttl) and eval(m_ttl.group(1)) == 3 * 60 * 1000
check("S1 常量 ERROR_TTL_MS = 3 分钟", ok_s1,
      "value=%s" % (m_ttl.group(1) if m_ttl else "<缺失>"))

# S2: error_fields 走 TTL 判定（引用常量 + 真实时钟）
m_ef = re.search(r'(?ms)^\tdef error_fields\(self\):\n(.*?)(?=^\tdef |^\S)', qs_src)
ef_block = m_ef.group(1) if m_ef else ""
ok_s2 = "ERROR_TTL_MS" in ef_block and "time.time()" in ef_block
check("S2 error_fields 含 TTL 时效判定", ok_s2,
      "len(block)=%d" % len(ef_block))

# S3: clear_error 方法存在且置空 last_error
m_ce = re.search(r'(?ms)^\tdef clear_error\(self\):\n(.*?)(?=^\tdef |^\S)', qs_src)
ce_block = m_ce.group(1) if m_ce else ""
ok_s3 = bool(m_ce) and "self.last_error = None" in ce_block
check("S3 clear_error() 置空 last_error", ok_s3,
      "found=%s" % bool(m_ce))

# S4: 恢复点覆盖（3 处：quest_engine.finish_task / daily_ghost.on_add_task & on_finish_task）
m_ft = re.search(r'(?ms)^\telif event_name == "finish_task":\n(.*?)(?=^\telif |^\tif |^\S)',
                 qe_src)
ft_block = m_ft.group(1) if m_ft else ""
m_dg_add = re.search(r'(?ms)^def on_add_task\(.*?(?=^\S)', dg_src)
m_dg_fin = re.search(r'(?ms)^def on_finish_task\(.*?(?=^\S)', dg_src)
dg_add = m_dg_add.group(0) if m_dg_add else ""
dg_fin = m_dg_fin.group(0) if m_dg_fin else ""
n_hits = (ft_block.count("_clear_recent_error(quest)")
          + dg_add.count("quest_engine._clear_recent_error(quest)")
          + dg_fin.count("quest_engine._clear_recent_error(quest)"))
ok_s4 = (n_hits == 3
         and "_clear_recent_error(quest)" in ft_block
         and "quest_engine._clear_recent_error(quest)" in dg_add
         and "quest_engine._clear_recent_error(quest)" in dg_fin)
check("S4 恢复点 3 处调用 clear(完成/交付 + 抓鬼派发 + 抓鬼交付)", ok_s4,
      "hits=%d ft=%d add=%d fin=%d" % (
          n_hits, ft_block.count("_clear_recent_error(quest)"),
          dg_add.count("quest_engine._clear_recent_error(quest)"),
          dg_fin.count("quest_engine._clear_recent_error(quest)")))

# S5: helper 用 getattr 兜底（热更 reload 后旧实例无 clear_error）
m_hl = re.search(r'(?ms)^def _clear_recent_error\(quest\):\n(.*?)(?=^\S)', qe_src)
hl_block = m_hl.group(0) if m_hl else ""
ok_s5 = 'getattr(quest, "clear_error", None)' in hl_block
check("S5 _clear_recent_error 用 getattr 兜底旧实例", ok_s5,
      "found=%s" % bool(m_hl))

# S6(反例): 报错原地重试路径不清（否则会掩盖持续错误）
m_rec = re.search(r'(?ms)^def __recover\(robot_object, quest\):\n(.*?)(?=^\S)', qe_src)
m_tst = re.search(r'(?ms)^def tester\(robot_object, now\):\n(.*?)(?=^\S)', qe_src)
rec_block = m_rec.group(1) if m_rec else ""
tst_block = m_tst.group(1) if m_tst else ""
ok_s6 = ("_clear_recent_error" not in rec_block
         and "_clear_recent_error" not in tst_block)
check("S6 看门狗重试路径(__recover/tester)未误清", ok_s6,
      "recover_hit=%s tester_hit=%s" % (
          "_clear_recent_error" in rec_block, "_clear_recent_error" in tst_block))

# ---------------------------------------------------------------- 动态断言
# C1: 刚 mark_error → error_fields 非空且形状正确（真实时钟）
q1 = qs_real.QuestState()
before_ms = int(time.time() * 1000)
r1 = q1.mark_error("NO_LEGAL_ROUTE", "商店采购无跨图路径 24→57")
code1, ts1 = q1.error_fields()
ok_c1 = (code1 == "NO_LEGAL_ROUTE" and before_ms <= ts1 <= int(time.time() * 1000)
         and isinstance(ts1, int))
check("C1 刚 mark_error → error_fields 非空(code/ts 正确)", ok_c1,
      "code=%r ts=%r" % (code1, ts1))

# C2: TTL 边界（真实时钟 + 注入时间戳）: 未到期非空 / 到期即空 / 本体保留
q2 = qs_real.QuestState()
q2.mark_error("TASK_STUCK", "任务 7001001 停链等待处理")
now_ms = int(time.time() * 1000)
q2.last_error["ts"] = now_ms - qs_real.ERROR_TTL_MS + 5000   # 还差 5s 到期
c_in = q2.error_fields()
q2.last_error["ts"] = now_ms - qs_real.ERROR_TTL_MS - 1      # 刚过 1ms
c_out = q2.error_fields()
ok_c2 = (c_in[0] == "TASK_STUCK" and c_out == ("", 0) and q2.last_error is not None)
check("C2 TTL 边界: 未到期非空 / 超 1ms 即空 / last_error 本体保留", ok_c2,
      "in=%r out=%r kept=%s" % (c_in, c_out, q2.last_error is not None))

# C3: 完整生命周期（注入时钟）：活跃态号恢复后 3 分钟内心跳不再带错误
CLOCK[0] = 10 ** 12
q3 = qs_fake.QuestState()
q3.mark_error("NO_LEGAL_ROUTE", "无合法跨图路径")
q3.set_state(qs_fake.ST_NAV)      # 号已恢复继续跑(活跃态)
c0 = q3.error_fields()
CLOCK[0] += 179 * 1000            # +2:59 仍在时效内
c1 = q3.error_fields()
CLOCK[0] += 2 * 1000              # +3:01 超出时效
c2 = q3.error_fields()
ok_c3 = (c0[0] == "NO_LEGAL_ROUTE" and c1[0] == "NO_LEGAL_ROUTE"
         and c2 == ("", 0) and q3.last_error is not None and q3.state == "NAV")
check("C3 注入时钟: 出错→恢复(NAV)→3 分钟内带错→超时自动停报(本体保留)", ok_c3,
      "c0=%r c1=%r c2=%r state=%s" % (c0, c1, c2, q3.state))

# C4: clear_error 后立即空（不等 TTL）
CLOCK[0] = 10 ** 12
q4 = qs_fake.QuestState()
q4.mark_error("TASK_STUCK", "x")
ok_pre = q4.error_fields()[0] == "TASK_STUCK"
q4.clear_error()
ok_c4 = ok_pre and q4.error_fields() == ("", 0) and q4.last_error is None
check("C4 clear_error 后立即返回空(恢复即清)", ok_c4,
      "pre=%s post=%r" % (ok_pre, q4.error_fields()))

# C5: clear 后可再次 mark（新错误重新上报, TTL 从新 ts 起算）
CLOCK[0] += 10 * 1000
q4.mark_error("GHOST_STUCK", "抓鬼 NAV: 走路超时")
c5 = q4.error_fields()
ok_c5 = c5 == ("GHOST_STUCK", CLOCK[0])
check("C5 clear 后可再 mark: 新错误重新上报(ts=当前)", ok_c5,
      "fields=%r expect_ts=%d" % (c5, CLOCK[0]))

# C6: 异常输入兜底（None / 缺 ts / 非 dict 不抛）
q6 = qs_fake.QuestState()
bad = []
q6.last_error = None
bad.append(q6.error_fields() == ("", 0))
q6.last_error = {}
bad.append(q6.error_fields() == ("", 0))
q6.last_error = {"code": "X"}                  # 缺 ts → ts=0 → 早已过期 → 空
bad.append(q6.error_fields() == ("", 0))
q6.last_error = "not-a-dict"                   # 非 dict → 异常兜底 → 空
bad.append(q6.error_fields() == ("", 0))
q6.last_error = {"code": True, "ts": "bad"}    # 类型异常 → 兜底 → 空
bad.append(q6.error_fields() == ("", 0))
check("C6 兜底: None/缺 ts/非 dict/类型异常 一律返回 (\"\",0) 不抛", all(bad),
      "results=%s" % bad)

# C7: helper 动态兜底（提取真实源码执行）：旧类实例(无 clear_error)不抛；新实例被清
ns_exec = {}
exec(compile(hl_block + "\n", "<quest_engine excerpt>", "exec"), ns_exec)
clear_fn = ns_exec["_clear_recent_error"]


class FakeOldQuest(object):
    pass


class FakeNewQuest(object):
    def __init__(self):
        self.cleared = 0

    def clear_error(self):
        self.cleared += 1


raised = None
try:
    clear_fn(FakeOldQuest())   # 旧类实例: 无 clear_error → 静默跳过
    clear_fn(None)             # 连 quest 都没有 → 静默跳过
except Exception as e:
    raised = "%s: %s" % (type(e).__name__, e)
new_q = FakeNewQuest()
clear_fn(new_q)
ok_c7 = raised is None and new_q.cleared == 1
check("C7 helper 兜底: 旧实例/None 不抛; 新实例 clear_error 被调", ok_c7,
      "raised=%s cleared=%d" % (raised, new_q.cleared))

# ---------------------------------------------------------------- 结果
print("\n自检目录: %s" % script_dir)
for _p, _s in ((qs_path, qs_src), (qe_path, qe_src), (dg_path, dg_src)):
    _h = hashlib.sha1(_s.encode("utf-8")).hexdigest()[:12]
    print("  %s  sha1=%s" % (os.path.basename(_p), _h))
total = 6 + 7
print("结果：%d 项，失败 %d 项" % (total, fails))
sys.exit(1 if fails else 0)
