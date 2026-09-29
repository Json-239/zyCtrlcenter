# -*- coding: utf-8 -*-
"""神捕/烽火两池修复包（2026-09-29）机器人端自检。

对应改动（分析-20260929-烽火大唐任务链.md §4.1/§4.2/§4.4/§6、分析-20260929-大唐神捕任务链.md
§2.3/§六 P0-3）：
  A 停止=收工：__request_stop 清 enabled + quest 复位 IDLE（治"冻态死号"：心跳 STOPPED
    残留 / random_walk 拦游荡 / 恢复引擎不补发）；__cmd_start 幂等分支不回归。
  B 跳转热循环守卫：同跳被拒 ≥2 → 硬黑名单(hop_black_hard) + 清导航残留 + 静默 60s
    （opt_loop_protect_ms 联动）；反例：单次不拉黑 / 不同跳独立 / TTL 过期可再触发 /
    只报一次聚合错误。
  C BAG_FULL 治理：包满通知(360/415/418/528/549/839)/交付包满/接取判决包满/采购被拒
    "空格不足" → 首次本地清理(daily_ghost.__tidy_bag)重试一轮；再失败才停止并上报
    "需要清包"。反例：非包满拒绝（任务栏已满/次数用完/背包"数量"不足）不误分流。
  D 心跳 state：client.py 分享日常运行中 → st["state"] 覆盖（同 ghost/booth 口径），
    消"中控把在跑的日常看成 IDLE → RESTORE 反复补发"。STOPPED/未启用不覆盖。
  G bag_full_age_ms（任务 #13 机器人端）：client.py 心跳字段（0=从未/无信号；有记录≥1ms；
    来源取新 ro.m_bag_full_ms > m_share_daily.bag_full_ms）。
  H msghandle.ghost_notice 全站包满记录（任务 #13 / 拍板 A）：包满码→ro.m_bag_full_ms；
    非包满码不写/不覆盖；无 robot 安全返回；与 G 组成跨文件端到端。
  E 双副本一致（可选：两目录都可访问时对比 share_daily.py / client.py / msghandle.py）。
  S 坏版灵敏度：对备份（share_daily.py.bak_20260929_2poolfix / client.py.bak_20260929_bagage /
    msghandle.py.bak_20260929_bagnotice）跑同一断言 → 核心用例必 FAIL（证明测的是真差异）。

用法: python tools/share_daily_2pool_selftest.py [script_dir]
  不带参数默认校验仓库副本 deploy/zones/prod-240-2300/script；
  传生产目录 F:/ZyBin/xm/2d-xiyou-server/robot/deploy/single_robot_zy/script 可再验一次。

实现方式：stub 注入（config/diag/error/protocol3/quest_engine/quest_state/keys/robot_path/
random_walk/pre_daily/daily_ghost/shop_errand/auto_summon）后真实 import share_daily；
坏版用 SourceFileLoader 显式加载 .bak 文件。
"""
import contextlib
import importlib.machinery
import importlib.util
import io
import os
import re
import sys
import tempfile
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
BAD_BAK = os.path.join(SCRIPT_DIR, "share_daily.py.bak_20260929_2poolfix")

# 运行期状态落盘重定向（防污染生产 state/ 目录；必须在 get_config/写计数前生效）
os.environ["ZCC_SHARE_DAILY_STATE_DIR"] = tempfile.mkdtemp(prefix="sd2pool_selftest_")

fails = 0
total = 0


def check(name, ok, detail=""):
    global fails, total
    total += 1
    fails += 0 if ok else 1
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  " + detail) if detail else ""))


# ================================================================
# 1) stub 环境（与 share_daily_selftest.py 同款，追加本包所需钩子）
# ================================================================
class FakeQuest(object):
    def __init__(self):
        self.tasks = {}
        self.dialog = None
        self.dialog_open = False
        self.dialog_open_ms = 0
        self.pending = None
        self.walk_target = None
        self.walk_end_ms = 0
        self.walk_pending_hop = None
        self.walk_pending_click = None
        self.timeout_override_ms = 0
        self.path_points = []
        self.dijkstra_route = []
        self.dijkstra_waiting = False
        self.dijkstra_jumper = None
        self.dijkstra_final = None
        self.dijkstra_goal_map = 0
        self.dynamic_npcs = {}
        self.dyn_npc_meta = {}
        self.fight_ctx = None
        self.fight_done = {}
        self.accept_ctx = None
        self.npc_cand_npc_id = 0
        self.npc_cand_positions = []
        self.npc_cand_idx = 0
        self.bag = {}
        self.chain = None
        self.shop_ctx = None
        self.chain_id = ""
        self.chain_task_set = None
        self.chain_done = False
        self.active = False
        self.active_task_index = 0
        self.state = "IDLE"
        self.completed = []

    def set_state(self, st):
        self.state = st

    def get_npc_candidates(self, npc_id, npc_index=None):
        return []


class FakeRobot(object):
    def __init__(self):
        self.m_account = ("test_account", "pwd")
        self.m_logined = True
        self.m_mapid = 12
        self.m_pose = (2265, 1846)
        self.m_fight_state = False
        self.m_quest = FakeQuest()
        self.m_share_daily = None
        self.m_ghost = None
        self.m_collect_walk = None
        self.m_team = None
        self.m_attrs = {}
        self.m_task_limited = {}
        self.m_task_limited_loaded_date = time.strftime("%Y%m%d")
        self.m_npc_positions = {}
        self.m_fight_kind = ""
        self.m_level = 43


_EMITTED = []
_QE_LOG = {"dijkstra": 0, "teleport_click": [], "schedule": [], "start_next_hop": 0}
_DG_LOG = {"tidy": 0}


def _install_stubs():
    config = types.ModuleType("config")
    config.robot_task_tester = False
    config.robot_spike_tester = False
    config.quest_auto_accept = True
    config.quest_max_retry = 3
    sys.modules["config"] = config

    diag = types.ModuleType("diag")
    diag.log = lambda *a, **k: None
    sys.modules["diag"] = diag
    error = types.ModuleType("error")
    error.NET_CLOSED = -100
    error.ROBOT_DELETED = -101
    sys.modules["error"] = error
    protocol3 = types.ModuleType("protocol3")
    protocol3.C2S_CLICKNPC = 80305
    protocol3.C2S_CLICK_DIALOG = 80139
    protocol3.C2S_USEITEM = 80312
    protocol3.C2S_DELITEM = 80313
    protocol3.C2S_DROP_ITEM = 80314
    sys.modules["protocol3"] = protocol3

    qs = types.ModuleType("quest_state")
    for k in ("ST_IDLE", "ST_WAIT_TASK", "ST_NAV", "ST_CLICK", "ST_DIALOG",
              "ST_FIGHT", "ST_SHOP", "ST_RECYCLE", "ST_ALLOC", "ST_WAIT_NEXT",
              "ST_DONE", "ST_ERROR"):
        setattr(qs, k, k.replace("ST_", ""))
    qs.QuestState = FakeQuest
    qs.ERROR_TTL_MS = 180000
    sys.modules["quest_state"] = qs

    keys = types.ModuleType("keys")
    kv = {
        "NOTICE_ADD_TASK": 21,
        "NOTICE_ROLE_TASK_CONDITION": 58,
        "NOTICE_ITEM_SPACE_CONDITION": 63,
        "NOTICE_NOT_ACCEPT_TASK_SHARE_DIALY_KEY_CONDITION": 60,
        "NOTICE_CAPTURE_MONSTER_FULL_BAG": 154,
        "NOTICE_TAKE_OUT_MOUNT_FULL_BAG": 155,
        "NOTICE_NO_SPACE_TO_GIVE": 360,
        "NOTICE_CAN_NOT_GET_BY_FULL_BAG": 415,
        "NOTICE_HAVE_NO_SPACE_IN_BAG": 418,
        "NOTICE_BAG_HAS_NO_SPACE_TO_OPERATOR": 528,
        "NOTICE_BAG_HAS_NO_SPACE_TO_COVER": 549,
        "NOTICE_NPC_ACCEPT_TASK_FREEZE": 666,
        "NOTICE_ADD_TASK_REACH_DAILY_LIMIT": 715,
        "NOTICE_ADD_TASK_REACH_WEEKLY_LIMIT": 716,
        "NOTICE_POCKET_NO_ENOUGH": 836,
        "NOTICE_BAG_FULL_TO_PICK": 839,
        "NOTICE_NO_SPACE_FOR_GOOD": 1207,
        "NOTICE_TROUGH_FULL": 1290,
        "NOTICE_ITEM_NOFREEPOINT": 1295,
        "NOTICE_ACCEPTED_TASK_REACH_MAX_COUNT": 61,
        "NOTICE_ROLE_FIXED_TEAM_CONDITION": 62,
        "NOTICE_MEMBER_CANT_SHARE": 71,
        "NOTICE_TEAM_CONDITION1": 72,
        "NOTICE_TEAM_CONDITION2": 73,
        "NOTICE_TEAM_CONDITION3": 74,
        "NOTICE_TEAM_LEVEL_CONDITION1": 75,
        "NOTICE_TEAM_LEVEL_CONDITION2": 76,
        "NOTICE_LEVEL_CONDITION1": 77,
        "NOTICE_LEVEL_CONDITION2": 78,
        "NOTICE_ROLE_RACE_CONDITION": 79,
        "NOTICE_ROLE_GENDER_CONDITION": 80,
        "NOTICE_FINISH_TASK_CONDITION": 59,
        "KILLING_DEMAND_COUNTER": 11883,
        "CAPTURE_DEMAND_COUNTER": 11884,
        "EQUIPPED_DEMAND_COUNTER": 11885,
        "COLLECTION_DEMAND_COUNTER": 11886,
        "PURCHASE_DEMAND_COUNTER": 11887,
        "DEMAND_LOCATION": 12053,
        "TASK_TARGET_LOCATION_LIST": 12054,
        "TASK_ITEM_INDEX_LIST": 12055,
    }
    for k, v in kv.items():
        setattr(keys, k, v)
    sys.modules["keys"] = keys

    qe = types.ModuleType("quest_engine")
    qe.CALL_LOG = _QE_LOG

    def _teleport_click(robot_object, quest, npc_id, npc_index, click_type, delay_ms, **kw):
        _QE_LOG["teleport_click"].append((npc_id, npc_index, click_type))
        return True

    def _schedule(quest, p):
        _QE_LOG["schedule"].append(p)

    def _emit(robot_object, event):
        _EMITTED.append(event)

    def _find_dijkstra_route(q, fm, tm, **kw):
        _QE_LOG["dijkstra"] += 1
        return [{"from_map": fm, "target_map": tm, "kind": "map_skip",
                 "x": 100, "y": 100, "destination_index": 1, "_from_map": fm}]

    def _start_next_hop(robot_object, quest, ms):
        _QE_LOG["start_next_hop"] += 1
        return True

    qe.__now_ms = lambda: time.time() * 1000
    qe.__human_delay = lambda ms: ms
    qe.__teleport_click = _teleport_click
    qe.__schedule = _schedule
    qe.__emit = _emit
    qe.__tick_walk = lambda ro, q, now: False
    qe.__start_next_hop = _start_next_hop
    qe.__do_action = lambda ro, q, p, now: 0
    qe.__handle_dialog = lambda ro, q, data: None
    qe.__recover = lambda ro, q: None
    qe.__check_hop_noack = lambda ro, q, now: False
    qe.__npc_live_pos = lambda ro, npc_id: None
    qe.__find_dijkstra_route = _find_dijkstra_route
    qe.__loads_blob = lambda blob: None
    qe.dispatch_cmd = lambda ro, cmd: {"cmd": cmd.get("cmd"), "result": "ok"}
    qe.get_quest = lambda ro, create=False: ro.m_quest
    qe.set_quest_chain = lambda q, chain: setattr(q, "chain", chain)
    qe._clear_recent_error = lambda q: None
    qe.__chain_grid_for = lambda q, m: None
    sys.modules["quest_engine"] = qe

    rp = types.ModuleType("robot_path")

    class MapGrid(object):
        def __init__(self, mapid, gd):
            self.mapid = mapid
            self.w = 1000
            self.h = 1000

        def to_grid(self, x, y):
            return (int(x) // 64, int(y) // 64)

        def blocked(self, gx, gy):
            return False

    rp.MapGrid = MapGrid
    sys.modules["robot_path"] = rp
    rw = types.ModuleType("random_walk")
    rw.walk_point_connected = lambda quest, mapid, a, b: True
    rw.snap_walkable = lambda grid, x, y: (int(x), int(y))
    rw.ROAM_SNAP_MAX_DIST_PX = 1024
    sys.modules["random_walk"] = rw
    pd = types.ModuleType("pre_daily")
    pd.is_active = lambda ro: False
    pd.double_claim_date = lambda ro: ""
    sys.modules["pre_daily"] = pd

    dg = types.ModuleType("daily_ghost")
    dg.SCRIPT_VERSION = "stub"
    dg.dispatch_cmd = lambda ro, cmd: {"cmd": cmd.get("cmd"), "result": "ok"}
    dg.on_task_limited = lambda ro, dl, is_load=None: None

    def _tidy_bag(ro):
        _DG_LOG["tidy"] += 1
        return 1

    setattr(dg, "__tidy_bag", _tidy_bag)
    sys.modules["daily_ghost"] = dg

    asm = types.ModuleType("auto_summon")
    asm.heal_tick = lambda ro, now_ms=None: 0
    sys.modules["auto_summon"] = asm

    se = types.ModuleType("shop_errand")
    se.LOCK = {"holder": "", "owner": "", "result": "", "reason": "", "skipped": False}
    se.SHOP_NPCS = {}

    def _se_acquire(ro, owner, item, need):
        se.LOCK["holder"] = owner
        se.LOCK["owner"] = owner
        return True, ""

    def _se_release(ro, owner):
        se.LOCK.update({"holder": "", "owner": "", "result": "", "reason": "", "skipped": False})

    se.acquire = _se_acquire
    se.release = _se_release
    se.status = lambda ro: dict(se.LOCK)
    se.notify_failed = lambda ro, msg: None
    sys.modules["shop_errand"] = se


def _read_src(path):
    return io.open(path, encoding="utf-8", errors="replace").read()


def _load_module(name, path):
    """显式 SourceFileLoader 加载（兼容 .bak_* 非 .py 后缀）。"""
    loader = importlib.machinery.SourceFileLoader(name, path)
    spec = importlib.util.spec_from_loader(name, loader)
    mod = importlib.util.module_from_spec(spec)
    sys.modules[name] = mod
    with contextlib.redirect_stdout(io.StringIO()):
        loader.exec_module(mod)
    return mod


_install_stubs()
try:
    S = _load_module("share_daily", os.path.join(SCRIPT_DIR, "share_daily.py"))
    check("import share_daily（真实 import，stub 运行时）", True)
    S.get_config(force_reload=True)
except Exception as e:  # noqa: BLE001
    import traceback
    check("import share_daily（真实 import，stub 运行时）", False,
          "%s: %s" % (type(e).__name__, str(e)[:200]))
    print(traceback.format_exc()[:1500])
    sys.exit(1)

BAD = None
if os.path.isfile(BAD_BAK):
    try:
        BAD = _load_module("share_daily_bad", BAD_BAK)
        check("坏版(.bak_20260929_2poolfix)可加载（灵敏度对照）", True)
    except Exception as e:  # noqa: BLE001
        check("坏版(.bak_20260929_2poolfix)可加载（灵敏度对照）", False,
              "%s: %s" % (type(e).__name__, str(e)[:200]))
else:
    check("坏版备份存在（灵敏度对照）", False, BAD_BAK)


def fresh_robot():
    return FakeRobot()


def new_state(mod=None, share_key="share_daily_大唐神捕", limit=10):
    mod = mod or S
    g = mod.ShareDailyState()
    g.enabled = True
    g.share_key = share_key
    g.daily_limit = limit
    g.chain = {"task_order": [
        {"task_index": 2028301, "thrower_npc": 13297, "catcher_npc": 13297,
         "kill_npc": [18140, 18141, 18142]},
        {"task_index": 2028302, "thrower_npc": 13297, "catcher_npc": 13297},
        {"task_index": 2028311, "thrower_npc": 0, "catcher_npc": 13597, "kill_npc": [13597]},
        {"task_index": 2028399, "thrower_npc": 0, "catcher_npc": 13297},
    ]}
    return g


def _clear_emitted():
    del _EMITTED[:]


def _emitted_of(code):
    return [e for e in _EMITTED if isinstance(e, dict) and e.get("code") == code]


# ================================================================
# A · 停止=收工（__request_stop 清 enabled + quest 复位 IDLE）
# ================================================================
def probe_stop_enabled(mod):
    r = fresh_robot()
    g = new_state(mod)
    r.m_share_daily = g
    getattr(mod, "__request_stop")(r, g, "HANDIN_STUCK", "t")
    return bool(g.enabled)


def probe_stop_quest_idle(mod):
    r = fresh_robot()
    g = new_state(mod)
    r.m_share_daily = g
    q = r.m_quest
    q.state = "CLICK"
    q.dijkstra_route = [{"kind": "map_skip"}]
    q.pending = {"type": "walk", "at_ms": 0, "data": {}}
    q.walk_target = (1, 2)
    getattr(mod, "__request_stop")(r, g, "BAG_FULL", "t")
    return (q.state, list(q.dijkstra_route), q.pending, q.walk_target, q.dijkstra_waiting)


r = fresh_robot()
g = new_state()
r.m_share_daily = g
getattr(S, "__request_stop")(r, g, "HANDIN_STUCK", "停止测试")
check("A1 停止后 enabled=False（心跳不再带 STOPPED 条目；游荡守卫放行）",
      g.enabled is False, "enabled=%s" % g.enabled)
check("A1b 停止后 STOPPED 状态/原因保留（可观测）",
      g.state == "STOPPED" and g.stop_code == "HANDIN_STUCK" and g.stop_reason == "停止测试")
check("A1c 停止事件仍上报 SHARE_DAILY_HANDIN_STUCK",
      bool(_emitted_of("SHARE_DAILY_HANDIN_STUCK")))

r2 = fresh_robot()
g2 = new_state()
r2.m_share_daily = g2
q2 = r2.m_quest
q2.state = "CLICK"
q2.dijkstra_route = [{"kind": "map_skip"}]
q2.pending = {"type": "walk", "at_ms": 0, "data": {}}
q2.walk_target = (1, 2)
q2.dijkstra_waiting = True
getattr(S, "__request_stop")(r2, g2, "BAG_FULL", "包满测试")
check("A2 停止后 quest 复位 IDLE（恢复引擎 needsRestore 视为可补发）",
      q2.state == "IDLE", "state=%s" % q2.state)
check("A3 停止后导航残留清空（route/pending/walk_target/waiting）",
      q2.dijkstra_route == [] and q2.pending is None and q2.walk_target is None
      and q2.dijkstra_waiting is False)

# 任务链活跃（quest.active=True）→ 不动 quest 状态（保守）
r3 = fresh_robot()
g3 = new_state()
r3.m_share_daily = g3
q3 = r3.m_quest
q3.active = True
q3.state = "NAV"
getattr(S, "__request_stop")(r3, g3, "BAG_FULL", "x")
check("A4 quest.active 时不动任务链状态（保守口径）",
      q3.state == "NAV" and g3.enabled is False)

# 幂等不破坏：运行中（enabled + KILL）→ already_running；停止后（enabled 已清）→ 完整启动
r4 = fresh_robot()
g4 = new_state()
g4.state = "KILL"
r4.m_share_daily = g4
rep = S.dispatch_cmd(r4, {"cmd": "share_daily_start", "share_key": "share_daily_大唐神捕",
                          "daily_limit": 10})
check("A5 运行中重复启动 → already_running（幂等分支保留）",
      rep.get("result") == "already_running", str(rep))

r5 = fresh_robot()
g5 = new_state()
r5.m_share_daily = g5
getattr(S, "__request_stop")(r5, g5, "HANDIN_STUCK", "x")
check("A6 停止后 enabled=False 且 state=STOPPED", g5.enabled is False and g5.state == "STOPPED")
# 污染"包满清理/跳转守卫"痕迹 → 重派应复位（新一轮 run）
g5.bag_cleanup_tried = True
g5.bag_cleanup_ms = int(time.time() * 1000)
g5.hop_guard_until_ms = int(time.time() * 1000) + 60000
g5.hop_guard_stops = 3
r5.m_quest.opt_loop_protect_ms = int(time.time() * 1000) + 60000   # quest 侧 R-1 节流也污染
rep5 = S.dispatch_cmd(r5, {"cmd": "share_daily_start", "share_key": "share_daily_大唐神捕",
                           "daily_limit": 10})
check("A7 停止后重派 → 完整启动（非 already_running，重新 enabled）",
      rep5.get("result") != "already_running" and g5.enabled is True,
      "result=%s enabled=%s" % (rep5.get("result"), g5.enabled))
check("A7b 新一轮 run 复位包满清理标记与跳转守卫窗口",
      g5.bag_cleanup_tried is False
      and int(g5.hop_guard_until_ms or 0) == 0 and int(g5.hop_guard_stops or 0) == 0)
check("A7c 重派即恢复：quest 侧 opt_loop_protect_ms 清零（静默不继承给新 run）",
      int(getattr(r5.m_quest, "opt_loop_protect_ms", 0) or 0) == 0,
      "opt=%s" % getattr(r5.m_quest, "opt_loop_protect_ms", None))

# random_walk / Go 恢复引擎判据锚点（源码级；跨语言只做存在性断言）
_rw_src = _read_src(os.path.join(SCRIPT_DIR, "random_walk.py"))
check("A8 源码锚点: random_walk 游荡守卫仍读 share_daily.enabled（清 enabled 即放行）",
      re.search(r"_sg\s*=\s*getattr\(robot_object,\s*\"m_share_daily\",\s*None\).*?"
                r"getattr\(_sg,\s*\"enabled\",\s*False\)", _rw_src, re.S) is not None)
_go_restorer = os.path.normpath(os.path.join(HERE, "..", "internal", "services", "restorer", "restorer.go"))
if os.path.isfile(_go_restorer):
    _rs = _read_src(_go_restorer)
    check("A9 源码锚点: Go needsRestore 对 IDLE/\"\"/ONLINE 判可补发（与 quest 复位配合）",
          'case "IDLE", "", "ONLINE":' in _rs)
else:
    check("A9 Go restorer.go 存在（needsRestore 判据对照）", False, _go_restorer)

# 坏版灵敏度：修复前 __request_stop 不清 enabled → A1 必 FAIL
if BAD is not None:
    _bad_en = probe_stop_enabled(BAD)
    check("A-S1 坏版灵敏度: 修复前停止后 enabled 仍 True（A1 必 FAIL）",
          _bad_en is True, "bad enabled=%s" % _bad_en)
    _bad_q = probe_stop_quest_idle(BAD)
    check("A-S2 坏版灵敏度: 修复前 quest 状态不清（A2/A3 必 FAIL）",
          _bad_q[0] == "CLICK" and len(_bad_q[1]) == 1,
          "bad state=%s route=%s" % (_bad_q[0], len(_bad_q[1])))

# ================================================================
# B · 跳转热循环守卫（同跳被拒≥2 → 硬黑名单 + 清态 + 静默 60s）
# ================================================================
def _mk_hop(fm=21, dest=30, fail=2, tm=17):
    return {"kind": "map_skip", "from_map": fm, "target_map": tm,
            "destination_index": dest, "x": 100, "y": 100,
            "_from_map": fm, "_fail": fail}


r = fresh_robot()
g = new_state()
r.m_share_daily = g
q = r.m_quest
hop = _mk_hop()
q.dijkstra_route = [hop]
q.walk_target = (100, 100)
q.walk_pending_hop = hop
now = int(time.time() * 1000)
_clear_emitted()
_ret = getattr(S, "__consume_nav")(r, g, q, now)
_hard = getattr(q, "hop_black_hard", None) or {}
_bl = getattr(q, "hop_black", None) or {}
_key = (21, 30)
check("B1 同跳被拒≥2 → 写入硬黑名单 hop_black_hard（BFS 不再回退选它）",
      _key in _hard and _hard[_key] >= now + 9 * 60 * 1000,
      "hard=%s" % {k: int(v - now) for k, v in _hard.items()})
check("B1b 同步写软黑名单 hop_black（一致性）", _key in _bl and _bl[_key] >= now + 9 * 60 * 1000)
check("B1c 该跳 _fail 计数复位（TTL 过期后需再拒 2 次才再拉黑）",
      int(hop.get("_fail", -1)) == 0, "_fail=%s" % hop.get("_fail"))
check("B2 触发后清导航残留（route/waiting/jumper/final/pending/walk_target）",
      q.dijkstra_route == [] and q.dijkstra_waiting is False and q.dijkstra_jumper is None
      and q.dijkstra_final is None and q.pending is None and q.walk_target is None
      and q.walk_pending_hop is None, "route=%s" % q.dijkstra_route)
check("B2b 触发后置免费优先（同 __replan_after_bad_hop：换路走下一条免费边组合）",
      getattr(q, "prefer_free", False) is True)
check("B3 静默窗 g.hop_guard_until_ms = now+60s；opt_loop_protect_ms 联动（quest_engine 重规划静默）",
      int(g.hop_guard_until_ms) >= now + 59 * 1000
      and int(getattr(q, "opt_loop_protect_ms", 0) or 0) >= now + 59 * 1000,
      "guard=%s opt=%s" % (g.hop_guard_until_ms, getattr(q, "opt_loop_protect_ms", 0)))
check("B8 守卫只报一次聚合错误（SHARE_DAILY_HOP_LOOP_STOP）",
      len(_emitted_of("SHARE_DAILY_HOP_LOOP_STOP")) == 1
      and int(getattr(g, "hop_guard_stops", 0)) == 1)
check("B3b 本帧已接管（__consume_nav 返回 0 早退）", _ret == 0, "ret=%s" % _ret)

# 静默期内：再进 __consume_nav 仍早退、不重规划、不重复报错
q.dijkstra_route = []
_ret2 = getattr(S, "__consume_nav")(r, g, q, now + 5000)
check("B3c 静默期内持续早退（第 2 帧）", _ret2 == 0)
check("B3d 静默期内不重复报 error", len(_emitted_of("SHARE_DAILY_HOP_LOOP_STOP")) == 1)
_dj0 = _QE_LOG["dijkstra"]
_ok_nav = getattr(S, "__nav_cross_to")(r, g, q, 17, 1, 2, now + 5000)
check("B3e 静默期内 __nav_cross_to 拒绝重规划（不发包不规划）",
      _ok_nav is False and _QE_LOG["dijkstra"] == _dj0)

# 静默过期 + opt_loop_protect 过期 → 恢复重规划（低频重试，不永久死锁）
g.hop_guard_until_ms = now - 1
q.opt_loop_protect_ms = now - 1
_ok_nav2 = getattr(S, "__nav_cross_to")(r, g, q, 17, 1, 2, now + 120000)
check("B6 静默/节流过期后恢复重规划（__find_dijkstra_route 被调用且导航接管）",
      _QE_LOG["dijkstra"] == _dj0 + 1 and _ok_nav2 is True,
      "dijkstra_calls=%s ok=%s" % (_QE_LOG["dijkstra"], _ok_nav2))

# 反例 1：单次被拒（_fail=1）不拉黑、不清态
r = fresh_robot()
g = new_state()
r.m_share_daily = g
q = r.m_quest
hop1 = _mk_hop(dest=31, fail=1)
q.dijkstra_route = [hop1]
triggered = getattr(S, "__hop_guard_tick")(r, g, q, int(time.time() * 1000))
_hb4 = getattr(q, "hop_black_hard", None) or {}
check("B4 反例: 单次被拒(_fail=1)不触发（不拉黑不清态）",
      triggered is False and (21, 31) not in _hb4 and len(q.dijkstra_route) == 1,
      "triggered=%s route=%d hard=%s" % (triggered, len(q.dijkstra_route), list(_hb4.keys())))
check("B4b 反例: 单次被拒不进入静默窗", int(g.hop_guard_until_ms or 0) == 0)

# 反例 2：不同跳独立计数（另一条 key 不受影响）
r = fresh_robot()
g = new_state()
r.m_share_daily = g
q = r.m_quest
hopA = _mk_hop(fm=21, dest=30, fail=2)
q.dijkstra_route = [hopA]
getattr(S, "__hop_guard_tick")(r, g, q, int(time.time() * 1000))
_hardA = dict(getattr(q, "hop_black_hard", None) or {})
check("B5 反例: 不同跳各自独立（仅命中 key 被拉黑）",
      (21, 30) in _hardA and (21, 31) not in _hardA and (25, 30) not in _hardA)

# TTL/静默过期后可再次触发（刷新窗口，不永久死锁）；第二次只 warn 不再抱 error
r = fresh_robot()
g = new_state()
r.m_share_daily = g
q = r.m_quest
now2 = int(time.time() * 1000)
hopB = _mk_hop(dest=32, fail=2)
q.dijkstra_route = [hopB]
_clear_emitted()
getattr(S, "__hop_guard_tick")(r, g, q, now2)
_err_first = len(_emitted_of("SHARE_DAILY_HOP_LOOP_STOP"))
g.hop_guard_until_ms = now2 - 1          # 模拟静默窗已过（低频重试窗口）
hopB2 = _mk_hop(dest=32, fail=2)
q.dijkstra_route = [hopB2]
getattr(S, "__hop_guard_tick")(r, g, q, now2 + 1000)
_hb7 = getattr(q, "hop_black_hard", None) or {}
check("B7 静默过期后再次连拒仍可拉黑（刷新窗口；不永久死锁）",
      int(getattr(g, "hop_guard_stops", 0) or 0) == 2
      and int(_hb7.get((21, 32), 0) or 0) >= now2 + 1000 + 9 * 60 * 1000
      and len(q.dijkstra_route) == 0,
      "stops=%s hard=%s" % (getattr(g, "hop_guard_stops", 0),
                            int(_hb7.get((21, 32), 0) or 0) - now2))
check("B8b 第二次触发不再抱 error（聚合只报一次）",
      _err_first == 1 and len(_emitted_of("SHARE_DAILY_HOP_LOOP_STOP")) == 1,
      "err1=%s err2=%s" % (_err_first, len(_emitted_of("SHARE_DAILY_HOP_LOOP_STOP"))))

# 源码锚点：__consume_nav 顶部接入 / __nav_cross_to 静默闸
_sd_src = _read_src(os.path.join(SCRIPT_DIR, "share_daily.py"))
check("B9 源码锚点: __consume_nav 顶部接入 __hop_guard_tick",
      re.search(r"def __consume_nav\(.*?__hop_guard_tick\(robot_object, g, quest, now_ms\)",
                _sd_src, re.S) is not None)
check("B9b 源码锚点: __nav_cross_to 静默闸（hop_guard_until_ms + opt_loop_protect_ms）",
      re.search(r"def __nav_cross_to\(.*?hop_guard_until_ms.*?opt_loop_protect_ms", _sd_src, re.S)
      is not None)

# 坏版灵敏度：修复前无守卫 → B1 必 FAIL
if BAD is not None:
    _bad_has_guard = getattr(BAD, "__hop_guard_tick", None) is not None
    check("B-S1 坏版灵敏度: 修复前无 __hop_guard_tick（B1/B2/B3 必 FAIL）",
          _bad_has_guard is False, "bad_has_guard=%s" % _bad_has_guard)
    _rb = fresh_robot()
    _gb = new_state(BAD)
    _qb = _rb.m_quest
    _hb = _mk_hop()
    _qb.dijkstra_route = [_hb]
    try:
        getattr(BAD, "__consume_nav")(_rb, _gb, _qb, int(time.time() * 1000))
        _bad_hard = getattr(_qb, "hop_black_hard", None) or {}
        _bad_blocked = (21, 30) in _bad_hard
    except Exception:
        _bad_blocked = False
    check("B-S2 坏版灵敏度: 修复前同跳连拒不写硬黑名单（B1 必 FAIL）",
          _bad_blocked is False)

# ================================================================
# C · BAG_FULL 治理（清理一次 → 重试一轮 → 再失败才停"需要清包"）
# ================================================================
# C1/C2: 交付/接取判决超时包满（经 __on_accept 判决路径）
r = fresh_robot()
g = new_state()
r.m_share_daily = g
S.on_notice(r, [528, "x"])
g.state = "ACCEPT"
g.accept_start_ms = (time.time() - 4.0) * 1000
_t0 = _DG_LOG["tidy"]
getattr(S, "__on_accept")(r, g, r.m_quest, time.time() * 1000)
check("C1 首次包满 → 调用本地清理一次且不停止",
      _DG_LOG["tidy"] == _t0 + 1 and g.state != "STOPPED"
      and bool(getattr(g, "bag_cleanup_tried", False)),
      "tidy=%s state=%s" % (_DG_LOG["tidy"], g.state))
check("C1b 清理后重置包满通知窗口（让重试一轮不被旧通知立即判停）",
      int(g.bag_full_ms or 0) == 0)
g.bag_full_ms = time.time() * 1000        # 重试后再次被判包满（新通知/新判定）
g.accept_start_ms = (time.time() - 4.0) * 1000
_t1 = _DG_LOG["tidy"]
getattr(S, "__on_accept")(r, g, r.m_quest, time.time() * 1000)
check("C2 再失败 → 停止 BAG_FULL 且上报'需要清包'（不再清理）",
      g.state == "STOPPED" and g.stop_code == "BAG_FULL"
      and "需要清包" in (g.stop_reason or "") and _DG_LOG["tidy"] == _t1,
      "%s / %s" % (g.stop_code, g.stop_reason))

# C3: on_notice 549/839（ACCEPT_FATAL 包满码；528 会先被 BAG_FULL_NOTICE 分支截胡——
#     该链路的停止在"3s 判决超时"分支，已由 C1/C2 覆盖）→ 首次清理不停止；再次才停
r = fresh_robot()
g = new_state()
r.m_share_daily = g
g.state = "ACCEPT"
_t0 = _DG_LOG["tidy"]
S.on_notice(r, [549, "x"])
check("C3 接取被拒(549)首次 → 清理一次不停止",
      g.state != "STOPPED" and _DG_LOG["tidy"] == _t0 + 1, "state=%s" % g.state)
S.on_notice(r, [839, "x"])   # 再次被拒（另一包满码，同语义）
check("C3b 接取被拒再失败 → 停止 ACCEPT_REFUSED + '需要清包'",
      g.state == "STOPPED" and g.stop_code == "ACCEPT_REFUSED"
      and "需要清包" in (g.stop_reason or ""),
      "%s / %s" % (g.stop_code, g.stop_reason))

# C4 反例: 非包满拒绝（716 本周次数已用完）→ 直接停、不清理
r = fresh_robot()
g = new_state()
r.m_share_daily = g
g.state = "ACCEPT"
_t0 = _DG_LOG["tidy"]
S.on_notice(r, [716, "x"])
check("C4 反例: 非包满拒绝(716)直接停止且不清理",
      g.state == "STOPPED" and g.stop_code == "ACCEPT_REFUSED" and _DG_LOG["tidy"] == _t0,
      "%s/%s" % (g.state, g.stop_code))

# C5: 采购被拒"背包空格不足" → 清理 + 重开会话重试；再失败才停
r = fresh_robot()
g = new_state()
r.m_share_daily = g
g.shop_item = 102031
g.shop_need = 1
g.shop_npc = 13011
_t0 = _DG_LOG["tidy"]
getattr(S, "__shop_fail")(r, g, r.m_quest, "购买被拒: 背包空格不足")
check("C5 采购被拒'背包空格不足'首次 → 清理一次 + 重开会话重试（不停止）",
      g.state != "STOPPED" and _DG_LOG["tidy"] == _t0 + 1
      and int(g.shop_wait_until or 0) > 0,
      "state=%s tidy=%s wait=%s" % (g.state, _DG_LOG["tidy"], g.shop_wait_until))
getattr(S, "__shop_fail")(r, g, r.m_quest, "购买被拒: 背包空格不足")
check("C5b 采购被拒再失败 → 停止 SHOP_FAILED（不清理）",
      g.state == "STOPPED" and g.stop_code == "SHOP_FAILED" and _DG_LOG["tidy"] == _t0 + 1,
      "%s" % g.stop_code)

# C6 反例: "背包数量不足"（数量语义，非空格）不误分流
r = fresh_robot()
g = new_state()
r.m_share_daily = g
g.shop_item = 102031
g.shop_need = 1
g.shop_npc = 13011
_t0 = _DG_LOG["tidy"]
getattr(S, "__shop_fail")(r, g, r.m_quest, "商店回执'已满足但未购买'且背包数量不足(0)")
check("C6 反例: '背包数量不足'不触发清理（防误伤，直接停）",
      g.state == "STOPPED" and _DG_LOG["tidy"] == _t0,
      "%s/%s" % (g.state, g.stop_code))

# C7 源码锚点: 四个触发点均接清理闸 + 停止文案含"需要清包"
check("C7 源码锚点: __on_submit/__on_accept/on_notice/__shop_fail 均接 __bag_cleanup_retry",
      len(re.findall(r"__bag_cleanup_retry\(robot_object, g\)", _sd_src)) >= 4,
      "hits=%d" % len(re.findall(r"__bag_cleanup_retry\(robot_object, g\)", _sd_src)))
check("C7b 源码锚点: 停止文案含'需要清包'（可观测）",
      _sd_src.count("需要清包") >= 3)

# 坏版灵敏度：修复前 549（ACCEPT_FATAL 包满码）直接停止（无清理）
if BAD is not None:
    _rb = fresh_robot()
    _gb = new_state(BAD)
    _rb.m_share_daily = _gb
    _gb.state = "ACCEPT"
    _tb = _DG_LOG["tidy"]
    _bf = getattr(BAD, "__is_bag_full_reason", None)
    try:
        BAD.on_notice(_rb, [549, "x"])
    except Exception:
        pass
    check("C-S1 坏版灵敏度: 修复前 549 直接停止（C3 必 FAIL）",
          _gb.state == "STOPPED" and _bf is None
          and _DG_LOG["tidy"] == _tb,
          "bad state=%s tidy_delta=%s" % (_gb.state, _DG_LOG["tidy"] - _tb))

# ================================================================
# D · 心跳 state（client.py）
# ================================================================
_client_path = os.path.join(SCRIPT_DIR, "client.py")
check("D0 client.py 存在", os.path.isfile(_client_path), _client_path)
_client_src = _read_src(_client_path) if os.path.isfile(_client_path) else ""
check("D1 源码锚点: 分享日常 state 覆盖块存在（enabled + 非 STOPPED + st['state']）",
      ("m_share_daily" in _client_src)
      and re.search(r"_sd_state\s*!=\s*\"STOPPED\".*?st\[\"state\"\]\s*=\s*_sd_state",
                    _client_src, re.S) is not None
      and '_sd_state = str(getattr(_sd, "state", "") or "")' in _client_src)
check("D1b 覆盖块位于 ghost/booth 覆盖之后（同区、格式一致）",
      _client_src.find('st["booth"]') < _client_src.find("_sd_state != \"STOPPED\""))

# D2 行为级：抽取覆盖块源码，受控执行
_m = re.search(r"(\t{3}# 2026-09-29 2 池修复包: 分享日常.*?)\n\t{3}st\[\"account\"\]",
               _client_src, re.S)
if _m:
    _block = _m.group(1)
    # 缩进归一（3 tab → 0）
    _lines = [(ln[3:] if ln.startswith("\t\t\t") else ln) for ln in _block.split("\n")]
    _block_src = "\n".join(_lines)

    def _run_heartbeat_state(sd):
        ro = types.SimpleNamespace(m_share_daily=sd)
        st = {"state": "IDLE"}
        exec(compile(_block_src, "<client_state_block>", "exec"),
             {"ro": ro, "st": st, "getattr": getattr, "int": int, "str": str})
        return st

    st_kill = _run_heartbeat_state(
        types.SimpleNamespace(enabled=True, state="KILL", task_index=2028302))
    check("D2 行为: 日常运行中(KILL) → 心跳 state 覆盖为 KILL（不再报 IDLE）",
          st_kill.get("state") == "KILL" and st_kill.get("task_index") == 2028302,
          str(st_kill))
    st_sub = _run_heartbeat_state(
        types.SimpleNamespace(enabled=True, state="SUBMIT", task_index=0))
    check("D2b 行为: SUBMIT → 心跳 state=SUBMIT", st_sub.get("state") == "SUBMIT")
    st_stop = _run_heartbeat_state(
        types.SimpleNamespace(enabled=True, state="STOPPED", task_index=0))
    check("D2c 行为: STOPPED 不覆盖（保持原 state 交恢复引擎）",
          st_stop.get("state") == "IDLE")
    st_off = _run_heartbeat_state(
        types.SimpleNamespace(enabled=False, state="KILL", task_index=0))
    check("D2d 行为: 未启用不覆盖（保持原 state）", st_off.get("state") == "IDLE")
else:
    check("D2 行为: 抽取覆盖块源码", False, "未定位到覆盖块")

if BAD is not None:
    _bclient = os.path.join(SCRIPT_DIR, "client.py.bak_20260929_2poolfix")
    _bsrc = _read_src(_bclient) if os.path.isfile(_bclient) else ""
    check("D-S1 坏版灵敏度: 修复前 client.py 无分享日常 state 覆盖块（D1 必 FAIL）",
          "_sd_state" not in _bsrc)

# ================================================================
# E · 热更兼容（旧实例缺新字段 → 首帧补齐）
# ================================================================
r = fresh_robot()
g = new_state()
r.m_share_daily = g
for _f in ("bag_cleanup_tried", "bag_cleanup_ms", "hop_guard_until_ms", "hop_guard_stops"):
    try:
        delattr(g, _f)
    except Exception:
        pass
getattr(S, "__check_watchdog")(r, g, r.m_quest, int(time.time() * 1000))
check("E1 热更兼容: 旧实例缺新字段 → __check_watchdog 补齐",
      hasattr(g, "bag_cleanup_tried") and hasattr(g, "hop_guard_until_ms")
      and hasattr(g, "hop_guard_stops") and hasattr(g, "bag_cleanup_ms"))

# ================================================================
# G · bag_full_age_ms（任务 #13 机器人端；心跳字段）
# ================================================================
_bam = re.search(r"(\t{3}# 2026-09-29 bag_full_age_ms.*?)\n\t{3}# 2026-08-31 守护数据上报",
                 _client_src, re.S)
if _bam:
    _blines = [(ln[3:] if ln.startswith("\t\t\t") else ln) for ln in _bam.group(1).split("\n")]
    _bsrc = "\n".join(_blines)

    def _run_bag_age(ro, st=None):
        st = st if st is not None else {}
        exec(compile(_bsrc, "<client_bag_age_block>", "exec"),
             {"ro": ro, "st": st, "getattr": getattr, "int": int,
              "max": max, "time": time})
        return st.get("bag_full_age_ms")

    _now = int(time.time() * 1000)
    _age0 = _run_bag_age(types.SimpleNamespace(m_share_daily=None))
    check("G1 无任何包满通知 → bag_full_age_ms=0（0=无信号/从未）", _age0 == 0, "age=%s" % _age0)
    _age1 = _run_bag_age(types.SimpleNamespace(m_bag_full_ms=_now - 5000, m_share_daily=None))
    check("G2 有全站记录（5s 前）→ age≈5000（通知→字段输出）",
          _age1 is not None and 4000 <= _age1 <= 20000, "age=%s" % _age1)
    _sd_new = types.SimpleNamespace(bag_full_ms=_now - 2000)
    _age2 = _run_bag_age(types.SimpleNamespace(m_bag_full_ms=_now - 600000,
                                               m_share_daily=_sd_new))
    check("G3 两来源取新（旧全站 10min vs 新玩法 2s → age≈2000）",
          _age2 is not None and _age2 <= 10000, "age=%s" % _age2)
    _sd_old = types.SimpleNamespace(bag_full_ms=_now - 600000)
    _age3 = _run_bag_age(types.SimpleNamespace(m_bag_full_ms=_now - 1500,
                                               m_share_daily=_sd_old))
    check("G3b 反向取新（新全站 1.5s vs 旧玩法 10min → age≈1500）",
          _age3 is not None and _age3 <= 10000, "age=%s" % _age3)
    # 端到端（share_daily 语境真链路）：on_notice(528) → g.bag_full_ms → 心跳字段
    _r9 = fresh_robot()
    _g9 = new_state()
    _r9.m_share_daily = _g9
    S.on_notice(_r9, [528, "x"])
    _age4 = _run_bag_age(_r9)
    check("G4 端到端: on_notice(528)→g.bag_full_ms→心跳 age>0 且很小",
          _age4 is not None and 0 < _age4 <= 5000,
          "age=%s g.bag_full_ms=%s" % (_age4, getattr(_g9, "bag_full_ms", None)))
    check("G5 源码锚点: 字段名/0 语义（0=从未/无信号）在实现中声明",
          "bag_full_age_ms" in _client_src and "0=从未发生/未知" in _client_src)
else:
    check("G0 提取 client.py bag_full_age_ms 块", False, "未定位到字段块")

_bclient2 = os.path.join(SCRIPT_DIR, "client.py.bak_20260929_bagage")
_bsrc2 = _read_src(_bclient2) if os.path.isfile(_bclient2) else None
check("G-S1 坏版灵敏度: 本项修复前(client.py.bak_20260929_bagage)无 bag_full_age_ms（G1/G2 必 FAIL）",
      _bsrc2 is not None and "bag_full_age_ms" not in _bsrc2,
      "bak=%s" % ("缺失" if _bsrc2 is None else "ok"))

# ================================================================
# H · msghandle.ghost_notice 全站包满记录（任务 #13 / team-lead 拍板 A）
# ================================================================
_MS_ROBOTS = {}
_MS_MGR = types.SimpleNamespace(g_mgr=types.SimpleNamespace(
    get_robot_object_by_fd=lambda fd: _MS_ROBOTS.get(fd)))

_ms_src = _read_src(os.path.join(SCRIPT_DIR, "msghandle.py"))
_ms_lines = _ms_src.split("\n")


def _ms_find(lines):
    """定位 [_bag_full_notice_ids 定义 .. ghost_notice 函数结束) 行区间。"""
    i0 = i1 = i2 = None
    for _k in range(len(lines)):
        if i0 is None and lines[_k].startswith("def _bag_full_notice_ids()"):
            i0 = _k
        if i0 is not None and i1 is None and lines[_k].startswith("def ghost_notice("):
            i1 = _k
            _j = _k + 1
            while _j < len(lines):
                if lines[_j].startswith("def ") or lines[_j].startswith("class "):
                    break
                _j += 1
            i2 = _j
            break
    return i0, i1, i2


def _ms_load(lines, lo, hi, name):
    _ns = {"keys": sys.modules.get("keys"), "time": time, "robot_mgr": _MS_MGR,
           "daily_ghost": sys.modules.get("daily_ghost"), "share_daily": S}
    exec(compile("\n".join(lines[lo:hi]), "<%s>" % name, "exec"), _ns)
    return _ns


_i_bag0, _i_ghost, _i_end = _ms_find(_ms_lines)
if _i_bag0 is not None and _i_ghost is not None and _i_end is not None:
    _ms_ns = _ms_load(_ms_lines, _i_bag0, _i_end, "ms_ghost_new")
    _ids = _ms_ns.get("_BAG_FULL_NOTICE_IDS")
    check("H4 包满码集 = 13 码（服务端实证扩集；与 client/share_daily 单一来源）",
          tuple(_ids or ()) == (63, 154, 155, 360, 415, 418, 528, 549, 836, 839, 1207, 1290, 1295),
          str(_ids))

    _r11 = fresh_robot()
    _MS_ROBOTS[11] = _r11
    _t0 = int(time.time() * 1000)
    _ms_ns["ghost_notice"](11, [528, "x"])
    _v1 = int(getattr(_r11, "m_bag_full_ms", 0) or 0)
    check("H1 收到 528 → ro.m_bag_full_ms 更新（全站记录点，不要求 enabled）",
          _t0 - 50 <= _v1 <= int(time.time() * 1000) + 50, "v=%s" % _v1)

    _r12 = fresh_robot()
    _MS_ROBOTS[12] = _r12
    _ms_ns["ghost_notice"](12, [21, "x"])       # NOTICE_ADD_TASK：非包满
    check("H2a 非包满码(21)不写字段（不产生假信号）",
          int(getattr(_r12, "m_bag_full_ms", 0) or 0) == 0)
    _r12.m_bag_full_ms = 11111
    _ms_ns["ghost_notice"](12, [716, "x"])      # 本周次数用完：非包满
    check("H2b 非包满码不覆盖既有值", int(_r12.m_bag_full_ms) == 11111)

    try:
        _ms_ns["ghost_notice"](99, [528, "x"])  # fd 无 robot
        _ok3 = True
    except Exception:
        _ok3 = False
    check("H3 无 robot_object → 安全返回（不抛）", _ok3)

    time.sleep(0.03)
    _ms_ns["ghost_notice"](11, [549, "x"])      # 再次（另一包满码）
    _v2 = int(getattr(_r11, "m_bag_full_ms", 0) or 0)
    check("H5 重复收到 → 取最新（时间戳前进）", _v2 > _v1, "%s → %s" % (_v1, _v2))

    # 扩集码验证（2026-09-29 服务端实证补：63/1290 为最高频"操作被拒"包满码）
    _r13 = fresh_robot()
    _MS_ROBOTS[13] = _r13
    _ms_ns["ghost_notice"](13, [63, "x"])       # 背包空格条件
    check("H7 新码 63（背包空格条件）→ 写入",
          int(getattr(_r13, "m_bag_full_ms", 0) or 0) > 0)
    _r14 = fresh_robot()
    _MS_ROBOTS[14] = _r14
    _ms_ns["ghost_notice"](14, [1290, "x"])     # 背包数量限制
    check("H8 新码 1290（背包数量限制）→ 写入",
          int(getattr(_r14, "m_bag_full_ms", 0) or 0) > 0)
    _r15 = fresh_robot()
    _MS_ROBOTS[15] = _r15
    _ms_ns["ghost_notice"](15, [838, "x"])      # 临时拾取栏位置不够：非背包满
    check("H8b 反例: 838（临时拾取栏位置不够）不写（防误扩到非背包满场景）",
          int(getattr(_r15, "m_bag_full_ms", 0) or 0) == 0)

    # H6 端到端：msghandle 记录 → client.py 心跳字段（跨文件链路）
    if _bam:
        _age5 = _run_bag_age(_r11)
        check("H6 端到端: ghost_notice(528)→ro.m_bag_full_ms→心跳 age>0 且很小",
              _age5 is not None and 0 < _age5 <= 5000, "age=%s" % _age5)
else:
    check("H0 提取 msghandle 记录块", False,
          "bag0=%s ghost=%s end=%s" % (_i_bag0, _i_ghost, _i_end))

# 坏版灵敏度：本项修复前(msghandle.bak_20260929_bagnotice)同调用不写字段（H1 必 FAIL）
_mbak = os.path.join(SCRIPT_DIR, "msghandle.py.bak_20260929_bagnotice")
_mbak_src = _read_src(_mbak) if os.path.isfile(_mbak) else None
if _mbak_src is not None:
    _ml2 = _mbak_src.split("\n")
    _b0 = _b1 = _b2 = None
    for _k in range(len(_ml2)):
        if _b0 is None and _ml2[_k].startswith("def ghost_notice("):
            _b0 = _k
            _j = _k + 1
            while _j < len(_ml2):
                if _ml2[_j].startswith("def ") or _ml2[_j].startswith("class "):
                    break
                _j += 1
            _b2 = _j
            break
    _rbad = fresh_robot()
    _MS_ROBOTS[21] = _rbad
    if _b0 is not None and _b2 is not None:
        _nsbad = _ms_load(_ml2, _b0, _b2, "ms_ghost_bad")
        _nsbad["ghost_notice"](21, [528, "x"])
    check("H-S1 坏版灵敏度: 修复前 ghost_notice 不写 m_bag_full_ms（H1 必 FAIL）",
          _b0 is not None and int(getattr(_rbad, "m_bag_full_ms", 0) or 0) == 0,
          "bad0=%s v=%s" % (_b0, getattr(_rbad, "m_bag_full_ms", None)))
else:
    check("H-S1 坏版灵敏度: 备份 msghandle.py.bak_20260929_bagnotice 存在", False, _mbak)

# ================================================================
# F · 双副本一致（若生产目录可访问）
# ================================================================
if os.path.isdir(PROD_COPY) and os.path.normpath(PROD_COPY) != os.path.normpath(SCRIPT_DIR):
    import filecmp
    for _fn in ("share_daily.py", "client.py", "msghandle.py"):
        _a = os.path.join(SCRIPT_DIR, _fn)
        _b = os.path.join(PROD_COPY, _fn)
        if os.path.isfile(_a) and os.path.isfile(_b):
            check("F1 双副本一致: %s" % _fn, filecmp.cmp(_a, _b, shallow=False))
        else:
            check("F1 双副本一致: %s（文件缺失）" % _fn, False, "%s / %s" % (_a, _b))
else:
    print("[SKIP] F1 双副本一致（生产目录不可访问或即当前目录：%s）" % PROD_COPY)

print()
print("自检目标: %s" % SCRIPT_DIR)
print("结果：%d 项，失败 %d 项" % (total, fails))
sys.exit(1 if fails else 0)
