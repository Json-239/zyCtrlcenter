# -*- coding: utf-8 -*-
"""药品自动回补(通用)+低血无药告警+robot_state 补血包+禁丢名单 自检（2026-09-29 实施批）。

覆盖（team-lead 实施批 ①②③④）：
  ① auto_summon 通用药品回补（低血/低蓝+存量不足 → shop_errand(owner=drug)）：
     触发/药够不触发/血满不触发/冷却内不触发/在途防呆(含抓鬼结果不碰)/share_daily
     先停会话/收尾(done/failed/超时)/no_money 冷却。
  ② quest_engine.__emit_state 补 hp/mp/bag（提取函数体真执行 + 源码锚点）。
  ③ bag_ops.DROP_PROTECT_INDEXES 追加渡劫造化丹/守护经验丹家族（行为+源码）。
  ④ 低血无药持续告警（首次 5 分钟、节流 10 分钟、恢复清零）。
  S 坏版灵敏度：对 `.bak_20260929_healbuy` 跑同断言 → 核心用例必 FAIL。

用法: python tools/heal_buy_selftest.py [script_dir]
"""
import contextlib
import importlib.machinery
import importlib.util
import io
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
DEFAULT_DIR = os.path.normpath(os.path.join(
    HERE, "..", "deploy", "zones", "prod-240-2300", "script"))
PROD_COPY = "F:/ZyBin/xm/2d-xiyou-server/robot/deploy/single_robot_zy/script"
SCRIPT_DIR = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_DIR
BAK = os.path.join(SCRIPT_DIR, "auto_summon.py.bak_20260929_healbuy")

fails = 0
total = 0


def check(name, ok, detail=""):
    global fails, total
    total += 1
    fails += 0 if ok else 1
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  " + detail) if detail else ""))


def _read_src(p):
    return io.open(p, encoding="utf-8", errors="replace").read()


# ================================================================
# stub 环境
# ================================================================
_EMIT = []
_SE = {"start": [], "consume": [], "stop": [], "status": {"active": False, "result": None,
                                                            "reason": "", "owner": "", "item": None}}


class FakeShopErrand(object):
    pass


def _install_stubs():
    config = types.ModuleType("config")
    config.robot_auto_heal = True
    config.robot_auto_heal_buy = True
    config.robot_heal_hp_ratio = 0.5
    config.robot_heal_mp_ratio = 0.5
    config.robot_auto_summon = True
    config.robot_auto_role_alloc = False
    config.robot_auto_summon_alloc = False
    config.robot_auto_summon_feed = False
    config.robot_bag_cleanup = False
    sys.modules["config"] = config

    diag = types.ModuleType("diag")
    diag.log = lambda *a, **k: None
    sys.modules["diag"] = diag

    cc = types.ModuleType("ctrl_client")
    cc.emit = lambda ev: _EMIT.append(ev)
    sys.modules["ctrl_client"] = cc

    p3 = types.ModuleType("protocol3")
    p3.C2S_USEITEM = 80312
    p3.C2S_SUMMON_USE_ITEM = 80098
    sys.modules["protocol3"] = p3

    err = types.ModuleType("error")
    err.ROBOT_DELETED = -101
    err.NET_CLOSED = -100
    sys.modules["error"] = err

    se = types.ModuleType("shop_errand")

    def _start(ro, item_index, count=None, npc=None, owner="errand", force=False):
        _SE["start"].append({"item": item_index, "count": count, "owner": owner, "npc": npc})
        _SE["status"] = {"active": True, "result": None, "reason": "", "owner": owner,
                         "item": item_index, "count": count}
        return {"ok": True, "result": "ok", "count": count}

    def _status(ro):
        return dict(_SE["status"])

    def _consume(ro, owner=None):
        _SE["consume"].append(owner)
        _SE["status"] = {"active": False, "result": None, "reason": "", "owner": "",
                         "item": None, "count": None}

    def _stop(ro, reason=""):
        _SE["stop"].append(reason)
        # 对齐真实 shop_errand.stop: 置 failed 后**立即 release**(锁清) → status 恢复空
        _SE["status"] = {"active": False, "result": None, "reason": "", "owner": "",
                         "item": None, "count": None}

    se.start = _start
    se.status = _status
    se.consume = _consume
    se.stop = _stop
    se.compute_need = lambda ro, item, target: 30   # 缺口 30(<上限)
    se.bag_count = lambda ro, item: 0
    sys.modules["shop_errand"] = se

    sd = types.ModuleType("share_daily")
    sd.STOP = []

    def _sd_dispatch(ro, cmd):
        sd.STOP.append(cmd)
        g = getattr(ro, "m_share_daily", None)
        if g is not None:
            g.enabled = False
        return {"cmd": cmd.get("cmd"), "result": "ok"}

    sd.dispatch_cmd = _sd_dispatch
    sys.modules["share_daily"] = sd

    # daily_ghost stub（② __emit_state 提取执行用: 调 __get_bag_summary）
    dg = types.ModuleType("daily_ghost")
    dg.__get_bag_summary = lambda ro: [{"name": "金创药", "count": 5, "pos": 8192}]
    sys.modules.setdefault("daily_ghost", dg)

    # quest_state stub（P1 __report_stuck 提取执行用）
    qs = types.ModuleType("quest_state")
    for _k in ("ST_IDLE", "ST_NAV", "ST_ERROR", "ST_DONE", "ST_WAIT_NEXT"):
        setattr(qs, _k, _k.replace("ST_", ""))
    sys.modules.setdefault("quest_state", qs)

    # random_walk / robot_path 等被 stop_collect_walk / 其它路径惰性 import 的模块
    rw = types.ModuleType("random_walk")
    sys.modules.setdefault("random_walk", rw)
    rp = types.ModuleType("robot_path")
    sys.modules.setdefault("robot_path", rp)


def _load_module(name, path):
    loader = importlib.machinery.SourceFileLoader(name, path)
    spec = importlib.util.spec_from_loader(name, loader)
    mod = importlib.util.module_from_spec(spec)
    sys.modules[name] = mod
    with contextlib.redirect_stdout(io.StringIO()):
        loader.exec_module(mod)
    return mod


class FakeQuest(object):
    def __init__(self, bag=None):
        self.bag = bag or {}
        self.shop_ctx = None
        self.active = False
        self.state = "IDLE"
        self.active_task_index = 0
        self.last_error = None

    def error_fields(self):
        return ("", 0)

    def mark_error(self, code, msg=""):
        self.last_error = (code, msg)

    def set_state(self, st):
        self.state = st


class FakeRobot(object):
    def __init__(self):
        self.m_account = ("t@x", "p")
        self.m_mapid = 12
        self.m_pose = (100, 100)
        self.m_fight_state = False
        self.m_cur_hp = 1000
        self.m_max_hp = 10000
        self.m_cur_mp = 9000
        self.m_max_mp = 10000
        self.m_bag_cache = {102007: [11111, 0]}     # 无金创药
        self.m_auto_summon = None
        self.m_quest = FakeQuest()
        self.m_share_daily = None
        self.m_collect_walk = None
        self.m_summon_list = {}
        self.m_level = 40

    def send_message(self, mid, data=None):
        return 0


_install_stubs()
try:
    A = _load_module("auto_summon", os.path.join(SCRIPT_DIR, "auto_summon.py"))
    check("import auto_summon（真实 import，stub 运行时）", True)
except Exception as e:  # noqa: BLE001
    import traceback
    check("import auto_summon（真实 import，stub 运行时）", False, "%s: %s" % (type(e).__name__, e))
    print(traceback.format_exc()[:1200])
    sys.exit(1)

BAD = None
if os.path.isfile(BAK):
    try:
        BAD = _load_module("auto_summon_bad", BAK)
        check("坏版(auto_summon.py.bak_20260929_healbuy)可加载", True)
    except Exception as e:  # noqa: BLE001
        check("坏版(auto_summon.py.bak_20260929_healbuy)可加载", False, str(e)[:200])
else:
    check("坏版备份存在（灵敏度对照）", False, BAK)


def new_robot(hp=1000, mhp=10000, mp=9000, mmp=10000, jc_count=0, th_count=50):
    r = FakeRobot()
    r.m_cur_hp, r.m_max_hp, r.m_cur_mp, r.m_max_mp = hp, mhp, mp, mmp
    r.m_bag_cache = {102007: [11111, jc_count], 102010: [22222, th_count]}
    return r


def reset_se():
    _SE["start"] = []
    _SE["consume"] = []
    _SE["stop"] = []
    _SE["status"] = {"active": False, "result": None, "reason": "", "owner": "", "item": None}


def now_ms():
    return int(time.time() * 1000)


# ================================================================
# ① 药品回补：触发/防呆/收尾
# ================================================================
# H1 低血+无药 → 触发采购(金创药)
r = new_robot(hp=1000, jc_count=0)
st = A.get_state(r)
reset_se()
_t = A._heal_buy_tick(r, st, now_ms())
check("①H1 低血(10%)且金创药=0 → 触发采购 item=102007",
      _t is True and len(_SE["start"]) == 1 and _SE["start"][0]["item"] == 102007
      and _SE["start"][0]["owner"] == "drug" and st.get("heal_buy_active") is True,
      str(_SE["start"]))
check("①H1b 采购量受上限约束(compute_need=30 → 下单 30)",
      _SE["start"] and _SE["start"][0]["count"] == 30, str(_SE["start"][:1]))

# H2 低血但药够(50≥20) → 不触发
r = new_robot(hp=1000, jc_count=50)
st = A.get_state(r)
reset_se()
check("①H2 低血但存量 50(≥低水位20) → 不触发", A._heal_buy_tick(r, st, now_ms()) is False
      and len(_SE["start"]) == 0)

# H3 血满+无药 → 不触发
r = new_robot(hp=10000, jc_count=0)
st = A.get_state(r)
reset_se()
check("①H3 血满且无药 → 不触发(按需补货, 不囤药)", A._heal_buy_tick(r, st, now_ms()) is False
      and len(_SE["start"]) == 0)

# H3b 低蓝+无昙花霜 → 触发 102010
r = new_robot(hp=10000, mp=1000, th_count=0)
st = A.get_state(r)
reset_se()
A._heal_buy_tick(r, st, now_ms())
check("①H3b 低蓝且昙花霜=0 → 触发采购 item=102010",
      len(_SE["start"]) == 1 and _SE["start"][0]["item"] == 102010, str(_SE["start"]))

# H4 冷却内不触发(120s)
r = new_robot(hp=1000, jc_count=0)
st = A.get_state(r)
reset_se()
st["last_buy_heal_ms"] = now_ms() - 10 * 1000
check("①H4 冷却内(10s<120s) → 不触发", A._heal_buy_tick(r, st, now_ms()) is False
      and len(_SE["start"]) == 0)

# H5 在途会话(抓鬼 active) → 不抢
r = new_robot(hp=1000, jc_count=0)
st = A.get_state(r)
reset_se()
_SE["status"] = {"active": True, "result": None, "reason": "", "owner": "drug",
                 "item": 102007, "count": 200}
check("①H5 抓鬼买药在途(active) → 不抢", A._heal_buy_tick(r, st, now_ms()) is False
      and len(_SE["start"]) == 0)

# H10 反例: 抓鬼的"未消费结果" → 不触发且绝不 consume(防抢抓鬼结果)
r = new_robot(hp=1000, jc_count=0)
st = A.get_state(r)
reset_se()
_SE["status"] = {"active": False, "result": "done", "reason": "", "owner": "drug",
                 "item": 102007, "count": 200}
_t = A._heal_buy_tick(r, st, now_ms())
check("①H10 反例: 他方(result未消费)在途 → 不触发且不 consume",
      _t is False and len(_SE["start"]) == 0 and len(_SE["consume"]) == 0)

# H6 share_daily 会话在跑 → 先停会话再采购
r = new_robot(hp=1000, jc_count=0)
st = A.get_state(r)
r.m_share_daily = types.SimpleNamespace(enabled=True, share_key="share_daily_宫廷10")
reset_se()
A._heal_buy_tick(r, st, now_ms())
_sdmod = sys.modules["share_daily"]
check("①H6 share_daily 在跑 → 先 share_daily_stop 再采购(避免抢导航)",
      _sdmod.STOP and _sdmod.STOP[0].get("cmd") == "share_daily_stop"
      and len(_SE["start"]) == 1 and r.m_share_daily.enabled is False,
      "stop=%s start=%s" % (_sdmod.STOP[:1], _SE["start"][:1]))
_sdmod.STOP = []

# H7 收尾 done → consume + 计数归零
r = new_robot(hp=1000, jc_count=0)
st = A.get_state(r)
reset_se()
st["heal_buy_active"] = True
st["heal_buy_start_ms"] = now_ms() - 5000
st["heal_buy_item"] = 102007
st["heal_buy_fails"] = 2
_SE["status"] = {"active": False, "result": "done", "reason": "", "owner": "drug",
                 "item": 102007, "count": 20}
_t = A._heal_buy_tick(r, st, now_ms())
check("①H7 收尾 done → consume(drug)+active 清+fails 归零",
      _t is True and _SE["consume"] == ["drug"] and st["heal_buy_active"] is False
      and st["heal_buy_fails"] == 0)

# H8 收尾 failed → consume + 连败冷却 60s
r = new_robot(hp=1000, jc_count=0)
st = A.get_state(r)
reset_se()
st["heal_buy_active"] = True
st["heal_buy_start_ms"] = now_ms() - 5000
_SE["status"] = {"active": False, "result": "failed", "reason": "no_money x", "owner": "drug",
                 "item": 102007, "count": 0}
A._heal_buy_tick(r, st, now_ms())
check("①H8 收尾 failed → consume + fails=1 + 冷却 60s(2^0)",
      st["heal_buy_fails"] == 1 and A._heal_buy_cooldown_ms(st) == 60 * 1000,
      "fails=%s cd=%s" % (st.get("heal_buy_fails"), A._heal_buy_cooldown_ms(st)))

# H9 会话超时 90s → stop + 冷却
r = new_robot(hp=1000, jc_count=0)
st = A.get_state(r)
reset_se()
st["heal_buy_active"] = True
st["heal_buy_start_ms"] = now_ms() - 91 * 1000
_SE["status"] = {"active": True, "result": None, "reason": "", "owner": "drug",
                 "item": 102007, "count": 200}
A._heal_buy_tick(r, st, now_ms())
check("①H9 会话 90s 未完成 → stop+冷却", _SE["stop"] and st["heal_buy_active"] is False
      and st["heal_buy_fails"] == 1, "stop=%s" % _SE["stop"][:1])

# H11 no_money → 冷却
r = new_robot(hp=1000, jc_count=0)
st = A.get_state(r)
reset_se()

def _start_no_money(ro, item_index, count=None, npc=None, owner="errand", force=False):
    return {"ok": False, "result": "no_money", "msg": "储备金不足"}

sys.modules["shop_errand"].start = _start_no_money
A._heal_buy_tick(r, st, now_ms())
check("①H11 no_money → 不启动 + fails=1 + 冷却生效",
      st["heal_buy_fails"] == 1 and st.get("heal_buy_active") in (False, None)
      and A._heal_buy_cooldown_ms(st) == 60 * 1000)

# 恢复 start stub
def _start_ok(ro, item_index, count=None, npc=None, owner="errand", force=False):
    _SE["start"].append({"item": item_index, "count": count, "owner": owner, "npc": npc})
    _SE["status"] = {"active": True, "result": None, "reason": "", "owner": owner,
                     "item": item_index, "count": count}
    return {"ok": True, "result": "ok", "count": count}

sys.modules["shop_errand"].start = _start_ok

# ================================================================
# ④ 低血无药告警
# ================================================================
r = new_robot(hp=1000, jc_count=0)
st = A.get_state(r)
del _EMIT[:]
_t0 = now_ms()
A._check_heal_starve(r, st, _t0)      # 首次: 记 since, 不告警
_after1 = len(_EMIT)
A._check_heal_starve(r, st, _t0 + 4 * 60 * 1000)   # 4min: 未到 5min
_after2 = len(_EMIT)
A._check_heal_starve(r, st, _t0 + 6 * 60 * 1000)   # 6min: 告警
_warns = [e for e in _EMIT if "低血/低蓝无药持续" in (e.get("msg") or "")]
check("④W1 持续≥5min → 告警一条(首次不告)", _after1 == 0 and _after2 == 0 and len(_warns) == 1,
      "warns=%d" % len(_warns))
A._check_heal_starve(r, st, _t0 + 7 * 60 * 1000)   # +1min(<10min 节流): 不再发
_warns2 = [e for e in _EMIT if "低血/低蓝无药持续" in (e.get("msg") or "")]
check("④W2 节流: 1 分钟后不再重复", len(_warns2) == 1)
A._check_heal_starve(r, st, _t0 + 17 * 60 * 1000)  # +11min: 再发一条
_warns3 = [e for e in _EMIT if "低血/低蓝无药持续" in (e.get("msg") or "")]
check("④W3 10 分钟节流到点 → 再发一条", len(_warns3) == 2, "warns=%d" % len(_warns3))
# 恢复有药 → 清零
r.m_bag_cache = {102007: [11111, 5]}
A._check_heal_starve(r, st, _t0 + 18 * 60 * 1000)
check("④W4 有药恢复 → starve 计时清零", int(st.get("heal_starve_since", 0) or 0) == 0)

# ================================================================
# P1(2026-09-30): 收尾与 tick 解耦 —— heal_buy_abort 外部收尾入口
# ================================================================
# H15 在途会话 → abort: 释放锁 + 清标记 + 落退避
r = new_robot(hp=1000, jc_count=0)
st = A.get_state(r)
reset_se()
st["heal_buy_active"] = True
st["heal_buy_start_ms"] = now_ms() - 30 * 1000
st["heal_buy_item"] = 102007
_t = A.heal_buy_abort(r, "stuck: 任务 0 换图推送迟迟未到")
check("①P1-H15 在途会话 → abort: stop(释放锁)+清标记+落退避(第1次)",
      _t is True and st["heal_buy_active"] is False and st["heal_buy_fails"] == 1
      and any("heal_buy aborted" in (x or "") for x in _SE["stop"]),
      "stop=%s fails=%s" % (_SE["stop"][:2], st.get("heal_buy_fails")))
# H16 幂等: 二连调无副作用
_cd_before = A._heal_buy_cooldown_ms(st)
_t2 = A.heal_buy_abort(r, "again")
check("①P1-H16 幂等: 二次调用 False 且 stop 不重复/fails 不翻倍",
      _t2 is False and len(_SE["stop"]) == 1 and A._heal_buy_cooldown_ms(st) == _cd_before,
      "stop_n=%d" % len(_SE["stop"]))
# H17 不碰他人(抓鬼会话不置本标记)
r = new_robot(hp=1000, jc_count=0)
st = A.get_state(r)
reset_se()
_t3 = A.heal_buy_abort(r, "x")
check("①P1-H17 无本模块会话(如抓鬼买药) → 不碰(False, stop 未调)",
      _t3 is False and len(_SE["stop"]) == 0)
# H18 端到端: quest_engine.__report_stuck 出口必触达 abort(提取真执行)
_qe_src = _read_src(os.path.join(SCRIPT_DIR, "quest_engine.py"))
_mr = re.search(r"(def __report_stuck\(robot_object, quest, reason\):.*?)\n(?=\n\n|\ndef |\nclass )",
                _qe_src, re.S)
if _mr:
    r = new_robot(hp=1000, jc_count=0)
    st = A.get_state(r)
    st["heal_buy_active"] = True
    r.m_quest = FakeQuest()
    reset_se()
    _ns2 = {"__emit": lambda ro, ev: None, "quest_state": sys.modules["quest_state"]}
    exec(compile(_mr.group(1), "<qe_report_stuck>", "exec"), _ns2)
    _ns2["__report_stuck"](r, r.m_quest, "换图推送迟迟未到, 无法走向跳转点")
    check("①P1-H18 端到端: __report_stuck 出口 → heal_buy_abort 被触达(锁释放)",
          any("heal_buy aborted" in (x or "") for x in _SE["stop"])
          and st["heal_buy_active"] is False,
          "stops=%s" % _SE["stop"][:2])
else:
    check("①P1-H18 提取 __report_stuck", False, "未定位")
# H19 abort 后冷却外可再次触发(锁已释放, 号可恢复)
r = new_robot(hp=1000, jc_count=0)
st = A.get_state(r)
reset_se()
st["heal_buy_active"] = True
A.heal_buy_abort(r, "stuck")
st["last_buy_heal_ms"] = now_ms() - 3600 * 1000   # 冷却外
_t4 = A._heal_buy_tick(r, st, now_ms())
check("①P1-H19 abort 后冷却外可再次触发(锁已释放, 号可恢复)",
      _t4 is True and len(_SE["start"]) == 1, "start=%s stop=%s" % (_SE["start"][:1], _SE["stop"][:1]))
# S4 坏版灵敏度: 修复前(healbuyfix 备份)无 heal_buy_abort / __report_stuck 无钩子
_bak2 = os.path.join(SCRIPT_DIR, "auto_summon.py.bak_20260930_healbuyfix")
if os.path.isfile(_bak2):
    _bad2 = _load_module("auto_summon_bad2", _bak2)
    check("①P1-S4a 坏版灵敏度: 修复前无 heal_buy_abort（H15 必 FAIL）",
          not hasattr(_bad2, "heal_buy_abort"))
    _qb2 = os.path.join(SCRIPT_DIR, "quest_engine.py.bak_20260930_healbuyfix")
    _qb_src = _read_src(_qb2) if os.path.isfile(_qb2) else ""
    check("①P1-S4b 坏版灵敏度: 修复前 __report_stuck 无 heal_buy_abort 钩子（H18 必 FAIL）",
          "heal_buy_abort" not in _qb_src and "heal_buy_abort" in _qe_src)
else:
    check("①P1-S4 坏版备份存在（灵敏度）", False, _bak2)

# ================================================================
# ② quest_engine.__emit_state 补 hp/mp/bag
# ================================================================
_qe_src = _read_src(os.path.join(SCRIPT_DIR, "quest_engine.py"))
_m = re.search(r"(def __emit_state\(robot_object, quest\):.*?)\n(?=\n\n|\ndef |\nclass )",
               _qe_src, re.S)
if _m:
    _fn_src = _m.group(1)
    _out = []
    _ns = {"__emit": lambda ro, ev: _out.append(ev),
           "__done_total": lambda q: 0,
           "getattr": getattr}
    exec(compile(_fn_src, "<qe_emit_state>", "exec"), _ns)
    _r2 = FakeRobot()
    _q2 = FakeQuest()
    _ns["__emit_state"](_r2, _q2)
    _ev = _out[0] if _out else {}
    check("②E1 提取 __emit_state 真执行: payload 含 hp/mp/bag",
          isinstance(_ev, dict) and _ev.get("hp") == [1000, 10000]
          and _ev.get("mp") == [9000, 10000] and "bag" in _ev,
          "keys=%s" % sorted(_ev.keys()))
    check("②E2 源码锚点: 对齐 daily_ghost 口径(注释含 '对齐 daily_ghost')",
          "对齐 daily_ghost.__emit_state 口径" in _qe_src
          and "daily_ghost" in _fn_src)
else:
    check("②E0 提取 __emit_state", False, "未定位到函数")

# ================================================================
# ③ bag_ops 禁丢名单
# ================================================================
_bo_src = _read_src(os.path.join(SCRIPT_DIR, "bag_ops.py"))
_bo_m = re.search(r"DROP_PROTECT_INDEXES\s*=\s*\(([^)]*)\)", _bo_src)
_ids = tuple(int(x) for x in re.findall(r"\d+", _bo_m.group(1))) if _bo_m else ()
check("③B1 DROP_PROTECT_INDEXES = 190146/102007/102010+110097/101373/101375/190034",
      _ids == (190146, 102007, 102010, 110097, 101373, 101375, 190034), str(_ids))
try:
    _bo = _load_module("bag_ops", os.path.join(SCRIPT_DIR, "bag_ops.py"))
    check("③B2 行为: is_protected_item(110097) is True（渡劫造化丹不丢）",
          _bo.is_protected_item(110097) is True
          and _bo.is_protected_item(101373) is True
          and _bo.is_protected_item(190034) is True)
except Exception as e:  # noqa: BLE001
    check("③B2 行为: is_protected_item（import 失败退源码断言）", False, str(e)[:150])

# ================================================================
# ①-补齐: config 双副本开关
# ================================================================
_cfg1 = _read_src(os.path.join(SCRIPT_DIR, "config.py"))
_cfg2 = _read_src(os.path.join(PROD_COPY, "config.py")) if os.path.isfile(
    os.path.join(PROD_COPY, "config.py")) else ""
check("①C1 config(当前目录) 含 robot_auto_heal_buy = True",
      "robot_auto_heal_buy = True" in _cfg1)
if _cfg2:
    check("①C2 config(生产副本) 也含（两份同步）",
          "robot_auto_heal_buy = True" in _cfg2)
else:
    print("[SKIP] ①C2 生产副本不可访问")

# ================================================================
# S 坏版灵敏度：修复前 auto_summon 无 _heal_buy_tick（H1 必 FAIL）
# ================================================================
if BAD is not None:
    _has_new = hasattr(BAD, "_heal_buy_tick") and hasattr(BAD, "_check_heal_starve")
    check("S1 坏版灵敏度: 修复前无 _heal_buy_tick/_check_heal_starve（H1/W1 必 FAIL）",
          _has_new is False, "bad_has=%s" % _has_new)
    _bad_src = _read_src(BAK)
    check("S2 坏版灵敏度: 修复前 heal_tick/tick 不接回补调用",
          "_heal_buy_tick(robot_object, st, now_ms)" not in _bad_src)
    # 行为对照: 坏版跑同场景 → 不会产生任何采购
    _rb = new_robot(hp=1000, jc_count=0)
    _stb = BAD.get_state(_rb)
    reset_se()
    try:
        BAD._try_role_heal(_rb, _stb, now_ms())   # 坏版里"无药 return False"
    except Exception:
        pass
    check("S3 坏版灵敏度: 修复前同场景零采购（①H1 断言必 FAIL）",
          len(_SE["start"]) == 0, "starts=%d" % len(_SE["start"]))

# ================================================================
# F 双副本一致（auto_summon / quest_engine / bag_ops / config）
# ================================================================
if os.path.isdir(PROD_COPY) and os.path.normpath(PROD_COPY) != os.path.normpath(SCRIPT_DIR):
    import filecmp
    for _fn in ("auto_summon.py", "quest_engine.py", "bag_ops.py", "config.py"):
        _a = os.path.join(SCRIPT_DIR, _fn)
        _b = os.path.join(PROD_COPY, _fn)
        if os.path.isfile(_a) and os.path.isfile(_b):
            check("F1 双副本一致: %s" % _fn, filecmp.cmp(_a, _b, shallow=False))
        else:
            check("F1 双副本一致: %s（文件缺失）" % _fn, False)
else:
    print("[SKIP] F1 双副本一致（生产目录不可访问或即当前目录）")

print()
print("自检目标: %s" % SCRIPT_DIR)
print("结果：%d 项，失败 %d 项" % (total, fails))
sys.exit(1 if fails else 0)
