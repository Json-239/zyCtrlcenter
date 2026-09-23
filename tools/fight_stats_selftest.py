# -*- coding: utf-8 -*-
"""战斗统计上大屏 自检（2026-09-23）。

数据管道：机器人 fight_tester.stat_begin/stat_end 统计（今日总/野怪/抓鬼/耗时）
  → client.py 心跳上报 st["fight_stats"] → 中控 state.Robot.FightStats 透传
  → /api/status → 大屏 Dashboard「今日战斗」卡（fightStats 聚合）。

口径：
  · 开战：daily_ghost 判定"抓鬼战斗/野外遇敌"后调 stat_begin(kind)；
  · 结束：90203(handle_fight_end) / 90188(fight_state_stop) 双点调 stat_end —— 幂等
    （cur_start_ms==0 跳过，多条结束消息只结算一次）；
  · 跨日：stat_begin 按 date 归零（服务端 0 点切日，与抓鬼计数同口径）。

动态用例：提取真人源代码执行（mock 时间），验证 首战/结算/幂等/跨日/未开战 五类场景。

用法：python tools/fight_stats_selftest.py [script_dir]
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

ft = open(os.path.join(script_dir, "fight_tester.py"), encoding="utf-8").read()
dg = open(os.path.join(script_dir, "daily_ghost.py"), encoding="utf-8").read()
ro = open(os.path.join(script_dir, "robot_operator.py"), encoding="utf-8").read()
cl = open(os.path.join(script_dir, "client.py"), encoding="utf-8").read()
st_go = open(os.path.join(ROOT, "internal", "state", "state.go"), encoding="utf-8").read()
ev_go = open(os.path.join(ROOT, "internal", "services", "event", "event.go"), encoding="utf-8").read()
dash = open(os.path.join(ROOT, "web", "src", "components", "Dashboard.vue"), encoding="utf-8").read()

fails = 0
total = 0


def check(name, ok, detail=""):
    global fails, total
    total += 1
    fails += 0 if ok else 1
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  " + detail) if detail else ""))


# ================================================================ 静态
check("S1 fight_tester: stat_begin/stat_end 定义 + import time",
      "def stat_begin(" in ft and "def stat_end(" in ft and "\nimport time\n" in ft)
check("S2 daily_ghost: 开战分类点调用 stat_begin(ghost/wild)",
      'stat_begin(robot_object, "ghost" if g.fight_was_ghost else "wild")' in dg)
check("S3 robot_operator: handle_fight_end 调 stat_end",
      "fight_tester.stat_end(robot_object)" in ro)
check("S4 fight_tester: fight_state_stop 调 stat_end（双点保险）",
      ft.count("stat_end(robot_object)") >= 2)
check("S5 client.py: 心跳上报 fight_stats",
      'st["fight_stats"] = {"total"' in cl and '"in_fight"' in cl)
check("S6 中控 state.Robot.FightStats 字段",
      'FightStats map[string]any `json:"fight_stats,omitempty"`' in st_go)
check("S7 中控 event: fight_stats 赋值",
      'if v, ok := st["fight_stats"]; ok {' in ev_go)
check("S8 大屏 Dashboard: 聚合 + 统计卡",
      "const fightStats = computed(" in dash and "<Aim />" in dash
      and "今日战斗" in dash and "cols-5 stat-grid" in dash)

# ================================================================ 动态：提取 stat_begin/stat_end
def extract_top_fn(src, name):
    m = re.search(r"(?ms)^def %s\(.*?(?=^def |\Z)" % re.escape(name), src)
    return m.group(0) if m else None

sb = extract_top_fn(ft, "stat_begin")
se = extract_top_fn(ft, "stat_end")
if not sb or not se:
    check("D0 提取 stat_begin/stat_end", False, "未匹配")
else:
    FAKE_NOW = [1790100000.0]  # 可变时间（秒）

    class _T:
        @staticmethod
        def time():
            return FAKE_NOW[0]

        @staticmethod
        def strftime(fmt):
            import time as _rt
            return _rt.strftime(fmt, _rt.localtime(FAKE_NOW[0]))

    ns = {"time": _T}
    exec(compile(sb + "\n" + se, "<fight_stats>", "exec"), ns)  # noqa: S102 —— 自检专用
    begin, end = ns["stat_begin"], ns["stat_end"]
    mk = lambda: types.SimpleNamespace()

    # D1 首战（野怪）：total=1/wild=1，cur_start 置位
    r = mk()
    begin(r, "wild")
    st = r.m_fight_stats
    check("D1 首战野怪: total=1/wild=1/ghost=0",
          st["total"] == 1 and st["wild"] == 1 and st["ghost"] == 0
          and st["cur_start_ms"] > 0)

    # D2 结算：+30s → dur_ms=30000，cur_start 清 0
    FAKE_NOW[0] += 30
    end(r)
    check("D2 结算 30 秒: dur_ms=30000 且 cur_start 清 0",
          st["dur_ms"] == 30000 and st["cur_start_ms"] == 0 and st["last_dur_ms"] == 30000,
          "dur=%s" % st["dur_ms"])

    # D3 幂等：重复 end 不再累加
    FAKE_NOW[0] += 10
    end(r)
    check("D3 重复结算幂等: dur 不变", st["dur_ms"] == 30000)

    # D4 第二战（抓鬼）：计数累加 + 结算
    FAKE_NOW[0] += 5
    begin(r, "ghost")
    FAKE_NOW[0] += 20
    end(r)
    check("D4 第二战抓鬼: total=2/ghost=1/dur=50000",
          st["total"] == 2 and st["ghost"] == 1 and st["dur_ms"] == 50000,
          "t=%s g=%s dur=%s" % (st["total"], st["ghost"], st["dur_ms"]))

    # D5 跨日：date 变化 → 归零重计
    import time as _real
    today = _real.strftime("%Y%m%d", _real.localtime(FAKE_NOW[0]))
    st["date"] = "20000101"  # 伪造陈旧日期
    begin(r, "wild")
    st = r.m_fight_stats  # 跨日会重建字典 → 取新引用
    check("D5 跨日归零: total=1（重置 date=%s）" % today,
          st["total"] == 1 and st["wild"] == 1 and st["ghost"] == 0 and st["date"] == today)

    # D6 未开战就 end：不产生数据
    r2 = mk()
    end(r2)
    check("D6 未开战 end: 不写数据（幂等）",
          getattr(r2, "m_fight_stats", None) is None)

# ================================================================ 汇总
print("\n自检目标: %s" % script_dir)
print("结果：%d 项，失败 %d 项" % (total, fails))
sys.exit(1 if fails else 0)
