# -*- coding: utf-8 -*-
"""跨夜误判满额修复 自检（2026-09-23）。

现场：139 个号在 00:00~06:28 被判"抓鬼满额 → 转野外游荡"，其实服务端 0 点已切日
清零（daily_timer 每天 00:00 更新日历；TaskLimited 读取时惰性清零），它们本可继续抓。

机制（服务端权威）：
  · 单人完成任务后服务端**不推** S2C_UPDATE_TASK_LIMITED（只在登录推全量 90152）→
    机器人手里的 m_task_limited 是登录时的快照；
  · 中控的 ghost.done 是机器人上报的透传值，跨夜未清；
  · 于是"本地计数/中控 done/服务端旧快照"三者都可能过期 → 误判满额。

修复（本脚本守护 9 处）：
  机器人端 daily_ghost.py：
    S1 __today_key() 定义
    S2 GhostState.count_date 初始化
    S3 完成计数跨夜检测（done_count 归零 + 日期更新）
    S4 启动三态规则（同日→中控优先 / fresh 服务端 / 跨夜旧缓存→0）
    S5 on_task_limited 全量时记录 m_task_limited_loaded_date
  机器人端 client.py：
    S6 心跳 ghost 上报带 count_date（顺带：写死 2019510 → 扫 2019501~2019513 取最大）
  中控 Go：
    S7 autotask.go ghostDailyFull：旧快照（count_date 非今天）不判满
    S8 handlers.go ghostDoneMap：旧快照的 done 不下发

动态：提取 S3/S4 源码块真执行（mock __log/__srv_ghost_done/__today_key）——
  覆盖"同日 / 跨夜持续在线 / 跨夜重开会话 / 进程重启"四类场景。

用法：python tools/ghost_crossday_selftest.py [script_dir]
"""
import io
import os
import re
import sys
import types

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.normpath(os.path.join(HERE, ".."))
DEFAULT_DIR = os.path.join(ROOT, "deploy", "zones", "prod-240-2300", "script")
script_dir = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_DIR

dh = open(os.path.join(script_dir, "daily_ghost.py"), encoding="utf-8").read()
cl = open(os.path.join(script_dir, "client.py"), encoding="utf-8").read()
at = open(os.path.join(ROOT, "internal", "api", "autotask.go"), encoding="utf-8").read()
hd = open(os.path.join(ROOT, "internal", "api", "handlers.go"), encoding="utf-8").read()

fails = 0
total = 0


def check(name, ok, detail=""):
    global fails, total
    total += 1
    fails += 0 if ok else 1
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  " + detail) if detail else ""))


TODAY = "20260923"
YDAY = "20260922"

# ================================================================ 静态
check("S1 daily_ghost: __today_key() 定义", "def __today_key():" in dh)
check("S2 GhostState.count_date 初始化", 'self.count_date = ""' in dh)
check("S3 完成计数跨夜检测块", "# 2026-09-23 跨夜检测" in dh and "跨夜重置抓鬼计数" in dh)
check("S4 启动三态规则（_fresh/loaded_date）",
      "m_task_limited_loaded_date" in dh and "_cd == _today" in dh and "_fresh" in dh)
check("S5 on_task_limited 记录加载日期",
      "m_task_limited_loaded_date = __today_key()" in dh)
check("S6 client.py 上报带 count_date",
      '"count_date": getattr(g, "count_date", "")' in cl
      and "range(2019501, 2019514)" in cl)
check("S7 中控 ghostDailyFull 旧快照不判满",
      'toStr(r.Ghost["count_date"])' in at and "func todayKey() string" in at)
check("S8 中控 ghostDoneMap 旧快照不下发",
      'toStr(r.Ghost["count_date"])' in hd)

# ================================================================ 动态：S3 跨夜检测
# 2026-09-23 计数口径修复后: `g.done_count += 1` 被收进 `if int(ti) != GHOST_SUBMIT_TASK:`
# （只数打鬼 FINISH）→ 这里按新形状提取, 并给 _f 补 ti 形参（打鬼 ti, 跨夜语义不变）。
m3 = re.search(
    r"(?ms)^\t# 2026-09-23 跨夜检测.*?^\t\tg\.done_count \+= 1\n"
    r"^\tg\.submit_fail = 0\t\t# 2026-09-15[^\n]*$", dh)
if not m3:
    check("D-S3 提取跨夜检测块", False, "未匹配")
else:
    body = m3.group(0)
    code = ("def _f(g, robot_object, __today_key, __log, ti, GHOST_SUBMIT_TASK):\n"
            + body + "\n")
    ns = {}
    exec(compile(code, "<crossday3>", "exec"), ns)  # noqa: S102 —— 自检专用
    fn = ns["_f"]
    logs = []
    SUBMIT = 2019511
    mk = lambda done, cd: types.SimpleNamespace(done_count=done, count_date=cd, submit_fail=9)

    g = mk(49, YDAY)
    fn(g, None, lambda: TODAY, lambda *a: logs.append(a), 2019509, SUBMIT)
    check("D1 跨夜后 +1: 旧计数 49 → 1（并打重置日志）",
          g.done_count == 1 and g.count_date == TODAY and g.submit_fail == 0
          and any("跨夜重置" in str(x) for x in logs),
          "done=%s cd=%s logs=%d" % (g.done_count, g.count_date, len(logs)))

    g = mk(49, TODAY)
    logs2 = []
    fn(g, None, lambda: TODAY, lambda *a: logs2.append(a), 2019509, SUBMIT)
    check("D2 同日 +1: 49 → 50（无重置日志）",
          g.done_count == 50 and not any("跨夜重置" in str(x) for x in logs2),
          "done=%s" % g.done_count)

    g = mk(0, YDAY)
    fn(g, None, lambda: TODAY, lambda *a: None, 2019509, SUBMIT)
    check("D3 跨夜且旧计数为 0: → 1", g.done_count == 1 and g.count_date == TODAY)

    # D10: 计数口径 —— 交付任务(2019511) 不再累加（2026-09-23 修复）
    g = mk(7, TODAY)
    fn(g, None, lambda: TODAY, lambda *a: None, SUBMIT, SUBMIT)
    check("D10 交付 FINISH(2019511) 不计数: 7 → 7", g.done_count == 7,
          "done=%s" % g.done_count)

# ================================================================ 动态：S4 启动三态
m4 = re.search(r"(?ms)^\t\t# 2026-08-24 已完成数.*?^\t\tg\.count_date = _today\n", dh)
if not m4:
    check("D-S4 提取启动三态块", False, "未匹配")
else:
    body = "\n".join((ln[1:] if ln.startswith("\t") else ln) for ln in m4.group(0).splitlines())
    code = ("def _f(g, robot_object, cmd, __today_key, __srv_ghost_done, __log,\n"
            "        GHOST_TASK_MIN, GHOST_TASK_MAX):\n" + body + "\n")
    ns = {}
    exec(compile(code, "<crossday4>", "exec"), ns)  # noqa: S102
    fn = ns["_f"]
    ACC = "robot0001@xy3.com"

    def run(g_done, g_cd, mid, loaded, srv):
        g = types.SimpleNamespace(done_count=g_done, count_date=g_cd, daily_limit=50)
        ro = types.SimpleNamespace(m_account=[ACC], m_task_limited={},
                                   m_task_limited_loaded_date=loaded)
        cmd = {"done": {ACC: mid}} if mid else {}
        logs = []
        fn(g, ro, cmd, lambda: TODAY, lambda store: srv, lambda *a: logs.append(a),
           2019501, 2019513)
        return g, logs

    g, logs = run(0, TODAY, 30, "", None)
    check("D4 同日+中控 30 → 30", g.done_count == 30 and g.count_date == TODAY,
          "done=%s" % g.done_count)

    g, logs = run(0, TODAY, 0, TODAY, 12)
    check("D5 同日+中控无值+fresh 服务端 12 → 12", g.done_count == 12)

    g, logs = run(50, YDAY, 50, TODAY, 5)
    check("D6 跨夜+fresh 服务端 5 → 5（且打校准日志）",
          g.done_count == 5 and any("校准" in str(x) for x in logs),
          "done=%s" % g.done_count)

    g, logs = run(50, YDAY, 50, YDAY, 50)
    check("D7 跨夜+旧缓存+中控 50 → 0（且打跨夜重置日志）",
          g.done_count == 0 and any("跨夜重置" in str(x) for x in logs),
          "done=%s" % g.done_count)

    g, logs = run(0, "", 0, TODAY, 3)
    check("D8 进程重启（count_date 空）+fresh 服务端 3 → 3", g.done_count == 3)

    g, logs = run(50, TODAY, 0, "", None)
    check("D9 同日+全无数据 → 保留本地 50", g.done_count == 50)

# ================================================================ 汇总
print("\n自检目标: %s" % script_dir)
print("结果：%d 项，失败 %d 项" % (total, fails))
sys.exit(1 if fails else 0)
