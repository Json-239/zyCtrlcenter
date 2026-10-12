# -*- coding: utf-8 -*-
"""跨天 ghost_start 双发·假满额修复自检（2026-10-10）。

背景（已实证 bug）：跨天 0 点后 RESTORE 补发与 AUTOTASK 直派在 1~6 秒内双发
ghost_start：
  · 第 1 条把 count_date 置为今天（跨夜重置 done=0）；
  · 第 2 条命中 `_cd==today` 分支，此时 `_mid=0`（RESTORE 不带 done）→ 旧逻辑
    用 __srv_ghost_done 读"昨天登录加载的陈旧缓存"（finish=50）当权威 →
    判满转游荡 → 该号当天 0 只（每天 8 个号受害）。
根因：on_task_limited 存缓存时丢掉了服务端条目里的 today_epoch（it[1]）。

修复（本脚本守护）：
  S1 __today_epoch() helper：本地 0 点 unix 秒（与 __today_key 同口径）
  S2 on_task_limited 保存 it[1] → m_task_limited_epoch（全量/增量都存）
  S3 ghost_start 三态校准：_srv_ok = _fresh or epoch==today；陈旧缓存 _srv=None
  S4 双发幂等兜底：10s 内重复 ghost_start → ok_dup（不重算 done/不判满）
  S5 正常出口记 m_ghost_start_ms（幂等基准）

动态：提取真实源码块 exec 真执行（mock 依赖），覆盖三场景：
  D1 昨天全量缓存 + 今天首条 → 判 0（跨夜重置），不判满
  D2 1 秒后第二条 → 幂等 ok_dup 返回 / 三态块 done 仍 0，绝不判满
  D3 今天全量缓存（finish=50）→ 正确判满 done_limit（防修坏正常语义）
坏版灵敏度：把 _srv_ok 变异回旧逻辑（_cd == _today or _fresh）→ D2 必须复现
  done_limit（假满额）；若坏版不复现，说明提取/断言失效（B1/B2）。

2026-10-12 扩展（5209 凌晨混龄缓存假满额）：
  现场：03:07 换图推送未到超时 → reghost 重登 → 03:08:09 三态再次判定
  "启动时计数已满 50/50" → 转游荡（当天 0 只）。根因：缓存混龄 —— 今日
  root=0 与昨日 root=50 并存，_srv_ok（fresh/今日 UPDATE）通过，但 MAX
  扫描把昨日 50 抬出来判满；重登全量若含昨日期条目，_fresh 捷径同样放行。
  守护：
  S6 on_task_limited 逐条存 m_task_limited_epochs[episode_root] = it[1]
  S7 __srv_ghost_done(only_epoch=...) 只统计今日条目；无今日条目 → None
     （不抬数/不覆盖，_cd==today 分支自然落回本地 _cur）
  S8 跨夜重置 else 分支清缓存非今日条目 + 复位 epoch/loaded_date(防混龄)
  动态新增（真试 __srv_ghost_done 源码 + 三态块组合）：
  N1 混龄缓存 → done=今日值 0，绝不 50；N2 全昨日 → None → 落 _cur；
  N2b epochs dict 缺失(热更窗口) → 同 N2(容错)；N3 跨夜清理保今日清昨日。
  坏版灵敏度：去掉 only_epoch 过滤（等价旧逻辑全 root MAX）→ N1/N2 必须
  复现假满额；若坏版不复现，说明提取/断言失效（B3/B4/B5）。

用法：python tools/ghost_crossday_dup_selftest.py [script_dir]
      默认 script_dir = deploy/zones/prod-240-2300/script
"""
import os
import re
import sys
import time
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

fails = 0
total = 0


def check(name, ok, detail=""):
    global fails, total
    total += 1
    fails += 0 if ok else 1
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  " + detail) if detail else ""))


TODAY_KEY = time.strftime("%Y%m%d")
TODAY_EPOCH = int(time.mktime(time.localtime()[:3] + (0, 0, 0, 0, 0, -1)))
YDAY_EPOCH = TODAY_EPOCH - 86400
YDAY_KEY = time.strftime("%Y%m%d", time.localtime(TODAY_EPOCH - 43200))
NOW_MS = 1759999990000  # 固定毫秒时钟（幂等块用）
ACC = "robot0001@xy3.com"

# ================================================================ 静态
check("S1 daily_ghost: __today_epoch() 定义", "def __today_epoch():" in dh)
check("S2 on_task_limited 保存 it[1](全量/增量共用循环)",
      "m_task_limited_epoch = int(it[1] or 0)" in dh)
check("S3 三态校准 _srv_ok 用 epoch 判今日",
      'm_task_limited_epoch", 0) or 0) == __today_epoch()' in dh
      and "_srv_ok = _fresh or" in dh)
check("S3b 旧条件(_cd==today or _fresh)已移除(防回退)",
      "if _cd == _today or _fresh:" not in dh and "if _srv_ok:" in dh)
check("S4 幂等 ok_dup + 10s 判定",
      '"ok_dup"' in dh
      and 'getattr(robot_object, "m_ghost_start_ms", 0) or 0) < 10000' in dh)
check("S5 正常出口记 m_ghost_start_ms",
      "m_ghost_start_ms = __now_ms()" in dh)
check("S6 on_task_limited 逐条存 per-root epoch",
      "m_task_limited_epochs = _epochs" in dh
      and "_epochs[str(er)] = int(it[1] or 0)" in dh)
check("S7 __srv_ghost_done 带 only_epoch/epochs 且过滤生效",
      "def __srv_ghost_done(store, only_epoch=None, epochs=None):" in dh
      and "if _e != only_epoch:" in dh)
check("S8 三态跨夜重置清非今日条目 + 复位标记",
      "_lim_epochs.items() if _e != _ep_today" in dh
      and "robot_object.m_task_limited_epoch = 0" in dh
      and 'robot_object.m_task_limited_loaded_date = ""' in dh)

# ================================================================ 提取源码块
m_a = re.search(
    r'(?ms)^\t\t_done = cmd\.get\("done", 0\)\n'
    r'.*?^\t\t\treturn \{"cmd": name, "result": "done_limit"\}\n', dh)
check("E1 提取三态校准块(含判满)", m_a is not None,
      "len=%d" % (len(m_a.group(0)) if m_a else 0))
m_b = re.search(
    r'(?ms)^\t\t_now_dup = __now_ms\(\)\n'
    r'.*?^\t\t\treturn \{"cmd": name, "result": "ok_dup"\}\n', dh)
check("E2 提取幂等块", m_b is not None,
      "len=%d" % (len(m_b.group(0)) if m_b else 0))
m_c = re.search(r'(?ms)^def __today_epoch\(\):.*?\n\n\n(?=def __srv_ghost_done)',
                dh)
check("E3 提取 __today_epoch 本体", m_c is not None,
      "len=%d" % (len(m_c.group(0)) if m_c else 0))
m_s = re.search(r'(?ms)^def __srv_ghost_done\(store.*?\n\treturn _max\n', dh)
check("E4 提取 __srv_ghost_done 本体(only_epoch 版)", m_s is not None,
      "len=%d" % (len(m_s.group(0)) if m_s else 0))

if not (m_a and m_b and m_c and m_s):
    print("\n结果：通过 %d/失败 %d（提取失败，后续动态用例跳过）" % (total - fails, fails))
    sys.exit(1)


def _dedent1(block):
    # 提取块以 2 个 tab 为函数体基准 → 逐行去 1 个 tab 变成"def 内一级"
    return "\n".join((ln[1:] if ln.startswith("\t") else ln)
                     for ln in block.splitlines())


A_WRAP = ("def _f(g, robot_object, cmd, name, __today_key, __today_epoch, "
          "__srv_ghost_done, __log, __set_state, __emit_ghost_done, "
          "GHOST_TASK_MIN, GHOST_TASK_MAX):\n")
B_WRAP = "def _f(g, robot_object, cmd, __now_ms, __today_key, __log, name):\n"


def _mk_fn(body, wrapper, tag):
    code = wrapper + body + "\n\treturn None\n"
    ns = {}
    exec(compile(code, "<%s>" % tag, "exec"), ns)  # noqa: S102 —— 自检专用
    return ns["_f"]


def _mk_srv(src, tag):
    ns = {"GHOST_TASK_MIN": 2019501, "GHOST_TASK_MAX": 2019513}
    exec(compile(src, "<%s>" % tag, "exec"), ns)  # noqa: S102 —— 自检专用
    return ns["__srv_ghost_done"]


body_a = _dedent1(m_a.group(0))
body_b = _dedent1(m_b.group(0))
fn_a = _mk_fn(body_a, A_WRAP, "three_state")
fn_b = _mk_fn(body_b, B_WRAP, "dup_idem")
SRV_REAL = _mk_srv(m_s.group(0), "srv_real")


def _srv_mock(store, only_epoch=None, epochs=None):
    # 既有 D 用例桩: 非空缓存 → 50(今日全量判满语义); 不解析 only_epoch
    #   (only_epoch 过滤语义由 N 用例注入 SRV_REAL 真源码直测)。
    return 50 if store else None


def run_a(fn, g_done, g_cd, loaded, epoch, limited, mid, logs,
          srv_fn=None, epochs=None):
    g = types.SimpleNamespace(done_count=g_done, count_date=g_cd, daily_limit=50)
    ro = types.SimpleNamespace(m_account=[ACC], m_task_limited=limited,
                               m_task_limited_loaded_date=loaded,
                               m_task_limited_epoch=epoch,
                               m_task_limited_epochs=epochs)
    cmd = {"done": {ACC: mid}} if mid else {}
    res = fn(g, ro, cmd, "ghost_start", lambda: TODAY_KEY, lambda: TODAY_EPOCH,
             srv_fn or _srv_mock,
             lambda *a: logs.append(a), lambda *a: None, lambda *a: None,
             2019501, 2019513)
    return g, res, ro


# ================================================================ D0 helper 本体
ns0 = {"time": time}
exec(compile(m_c.group(0), "<today_epoch>", "exec"), ns0)  # noqa: S102
_ep = ns0["__today_epoch"]()
_lt = time.localtime(_ep)
check("D0a __today_epoch = 本地今日 0 点",
      _lt.tm_hour == 0 and _lt.tm_min == 0 and _lt.tm_sec == 0,
      "ep=%s -> %s" % (_ep, time.strftime("%Y-%m-%d %H:%M:%S", _lt)))
check("D0b 与 __today_key 同口径(本地日期=今天)",
      time.strftime("%Y%m%d", _lt) == TODAY_KEY)

# ================================================================ D1 昨天缓存+今天首条
LOG1 = []
g, res, _ = run_a(fn_a, 50, YDAY_KEY, YDAY_KEY, YDAY_EPOCH,
               {"2019502": (2019502, YDAY_EPOCH, 4, 50)}, 0, LOG1)
check("D1 昨天全量缓存+今天首条: 判 0(跨夜重置), 不判满",
      g.done_count == 0 and g.count_date == TODAY_KEY
      and res is None
      and any("跨夜重置" in str(x) for x in LOG1),
      "done=%s cd=%s res=%s" % (g.done_count, g.count_date, res))

# ================================================================ D2 1 秒后第二条
# 2a 幂等块: 10s 内 + count_date==today → ok_dup, 仅刷新 limit/role
g2 = types.SimpleNamespace(count_date=TODAY_KEY, daily_limit=50, role="solo")
ro2 = types.SimpleNamespace(m_ghost_start_ms=NOW_MS - 1000)
LOG2A = []
res2a = fn_b(g2, ro2, {"daily_limit": 40, "role": "captain"},
             lambda: NOW_MS, lambda: TODAY_KEY,
             lambda *a: LOG2A.append(a), "ghost_start")
check("D2a 1 秒后第二条: 幂等 ok_dup(不重算 done)",
      isinstance(res2a, dict) and res2a.get("result") == "ok_dup"
      and g2.daily_limit == 40 and g2.role == "captain"
      and any("幂等忽略" in str(x) for x in LOG2A),
      "res=%s limit=%s role=%s" % (res2a, g2.daily_limit, g2.role))

# 2b 幂等边界: 超 10s 不拦 / 跨夜(count_date 非今天)不拦
g2b = types.SimpleNamespace(count_date=TODAY_KEY, daily_limit=50, role="solo")
res2b = fn_b(g2b, types.SimpleNamespace(m_ghost_start_ms=NOW_MS - 20000),
             {}, lambda: NOW_MS, lambda: TODAY_KEY, lambda *a: None,
             "ghost_start")
g2c = types.SimpleNamespace(count_date=YDAY_KEY, daily_limit=50, role="solo")
res2c = fn_b(g2c, types.SimpleNamespace(m_ghost_start_ms=NOW_MS - 1000),
             {}, lambda: NOW_MS, lambda: TODAY_KEY, lambda *a: None,
             "ghost_start")
check("D2b 幂等边界: 超 10s / 跨夜未启动 均不拦",
      res2b is None and res2c is None,
      "res_20s=%s res_yday=%s" % (res2b, res2c))

# 2c 三态块第二道防线: 第二条时状态(cd=今天, 缓存仍是昨天) → done 仍 0
LOG2C = []
g, res, _ = run_a(fn_a, 0, TODAY_KEY, YDAY_KEY, YDAY_EPOCH,
               {"2019502": (2019502, YDAY_EPOCH, 4, 50)}, 0, LOG2C)
check("D2c 第二条(缓存陈旧): done 仍 0, 绝不判满/done_limit",
      g.done_count == 0 and g.count_date == TODAY_KEY and res is None,
      "done=%s res=%s" % (g.done_count, res))

# ================================================================ D3 今天缓存→应判满
LOG3 = []
g, res, _ = run_a(fn_a, 0, "", TODAY_KEY, TODAY_EPOCH,
               {"2019502": (2019502, TODAY_EPOCH, 4, 50)}, 0, LOG3)
check("D3 今天全量缓存(epoch=今天,finish=50): 正确判满 done_limit",
      g.done_count == 50 and isinstance(res, dict)
      and res.get("result") == "done_limit",
      "done=%s res=%s" % (g.done_count, res))

LOG3B = []
g, res, _ = run_a(fn_a, 0, TODAY_KEY, TODAY_KEY, TODAY_EPOCH,
                  {"2019502": (2019502, TODAY_EPOCH, 4, 50)}, 0, LOG3B)
check("D3b 今天缓存+cd=今天+_mid=0: 仍判满(done=50)",
      g.done_count == 50 and isinstance(res, dict)
      and res.get("result") == "done_limit",
      "done=%s res=%s" % (g.done_count, res))

# ================================================================ N 混龄缓存(2026-10-12)
# 5209 凌晨根因: 重登后缓存混龄(今日 root=0 + 昨日 root=50), _srv_ok 通过
# (fresh/今日 UPDATE)时旧 MAX 扫描把昨日 50 抬出来 → 假满额转游荡(当天 0 只)。
LOG_N1 = []
g, res, _ = run_a(fn_a, 7, TODAY_KEY, TODAY_KEY, TODAY_EPOCH,
                  {"2019502": 0, "2019503": 50}, 0, LOG_N1,
                  srv_fn=SRV_REAL,
                  epochs={"2019502": TODAY_EPOCH, "2019503": YDAY_EPOCH})
check("N1 混龄缓存(今日 root=0+昨日 root=50): done=今日值 0, 绝不 50",
      g.done_count == 0 and res is None,
      "done=%s res=%s" % (g.done_count, res))

LOG_N2 = []
g, res, _ = run_a(fn_a, 7, TODAY_KEY, TODAY_KEY, TODAY_EPOCH,
                  {"2019503": 50}, 0, LOG_N2,
                  srv_fn=SRV_REAL, epochs={"2019503": YDAY_EPOCH})
check("N2 全昨日条目(无今日): _srv=None → 落 _cur=7, 不判满",
      g.done_count == 7 and res is None,
      "done=%s res=%s" % (g.done_count, res))

LOG_N2B = []
g, res, _ = run_a(fn_a, 7, TODAY_KEY, TODAY_KEY, TODAY_EPOCH,
                  {"2019502": 0}, 0, LOG_N2B,
                  srv_fn=SRV_REAL, epochs=None)
check("N2b epochs dict 缺失(热更窗口): None → 落 _cur=7(容错)",
      g.done_count == 7 and res is None,
      "done=%s res=%s" % (g.done_count, res))

LOG_N3 = []
g, res, ro = run_a(fn_a, 50, YDAY_KEY, YDAY_KEY, YDAY_EPOCH,
                   {"2019502": 50, "2019503": 0}, 0, LOG_N3,
                   epochs={"2019502": YDAY_EPOCH, "2019503": TODAY_EPOCH})
check("N3 跨夜重置清理: 清非今日/保今日/复位 epoch+loaded_date",
      g.done_count == 0 and res is None
      and ro.m_task_limited == {"2019503": 0}
      and ro.m_task_limited_epochs == {"2019503": TODAY_EPOCH}
      and ro.m_task_limited_epoch == 0
      and ro.m_task_limited_loaded_date == "",
      "done=%s cache=%s epochs=%s" % (g.done_count, ro.m_task_limited,
                                      ro.m_task_limited_epochs))

# ================================================================ 坏版灵敏度
# 把 _srv_ok 变异回旧逻辑(_cd == _today or _fresh): 场景 2 必须复现假满额。
_bs = chr(92)
bad_body, n_sub = re.subn(
    r"(?m)^\t_srv_ok = _fresh or " + re.escape(_bs) + r"\n"
    r"\t\t\(int\(getattr\(robot_object, \"m_task_limited_epoch\", 0\) or 0\)"
    r" == __today_epoch\(\)\)$",
    "\t_srv_ok = _cd == _today or _fresh", body_a)
check("B1 坏版变异成功(回退 _srv_ok 条件)", n_sub == 1,
      "subn=%d" % n_sub)
if n_sub == 1:
    bad_fn_a = _mk_fn(bad_body, A_WRAP, "three_state_bad")
    LOGB = []
    g, res, _ = run_a(bad_fn_a, 0, TODAY_KEY, YDAY_KEY, YDAY_EPOCH,
                   {"2019502": (2019502, YDAY_EPOCH, 4, 50)}, 0, LOGB)
    check("B2 坏版复现: 场景 2 假满额(旧逻辑 done=50→done_limit)",
          g.done_count == 50 and isinstance(res, dict)
          and res.get("result") == "done_limit",
          "done=%s res=%s" % (g.done_count, res))
else:
    check("B2 坏版复现: 场景 2 假满额(旧逻辑 done=50→done_limit)", False,
          "变异失败, 无法验证灵敏度")

# ---- 2026-10-12: only_epoch 过滤坏版(去掉过滤 = 等价旧逻辑全 root MAX) ----
bad_srv_src, n2 = re.subn(r"(?m)^\t\t\tif _e != only_epoch:$", "\t\t\tif False:",
                          m_s.group(0))
check("B3 坏版变异成功(去掉 only_epoch 过滤)", n2 == 1, "subn=%d" % n2)
if n2 == 1:
    bad_srv = _mk_srv(bad_srv_src, "srv_bad")
    g, res, _ = run_a(fn_a, 7, TODAY_KEY, TODAY_KEY, TODAY_EPOCH,
                      {"2019502": 0, "2019503": 50}, 0, [],
                      srv_fn=bad_srv,
                      epochs={"2019502": TODAY_EPOCH, "2019503": YDAY_EPOCH})
    check("B4 坏版复现: N1 混龄场景 → MAX 抬出 50 假满额(好版必 0)",
          g.done_count == 50 and isinstance(res, dict)
          and res.get("result") == "done_limit",
          "done=%s res=%s" % (g.done_count, res))

    g, res, _ = run_a(fn_a, 7, TODAY_KEY, TODAY_KEY, TODAY_EPOCH,
                      {"2019503": 50}, 0, [], srv_fn=bad_srv,
                      epochs={"2019503": YDAY_EPOCH})
    check("B5 坏版复现: N2 全昨日场景 → 50 假满额(好版落 _cur=7)",
          g.done_count == 50 and isinstance(res, dict)
          and res.get("result") == "done_limit",
          "done=%s res=%s" % (g.done_count, res))
else:
    check("B4 坏版复现: N1 混龄场景 → MAX 抬出 50 假满额(好版必 0)", False,
          "变异失败, 无法验证灵敏度")
    check("B5 坏版复现: N2 全昨日场景 → 50 假满额(好版落 _cur=7)", False,
          "变异失败, 无法验证灵敏度")

# ================================================================ 汇总
print("\n自检目标: %s" % script_dir)
print("结果：通过 %d/失败 %d" % (total - fails, fails))
sys.exit(1 if fails else 0)
