# -*- coding: utf-8 -*-
"""分享日常通用驱动（P0 大唐神捕）自检（2026-09-23）。

覆盖 docs/04-测试/设计与改动-20260923-分享日常改造清单.md 的机器人端 P0 验收点：
  S1  15 列配置加载（加载行数/keywords/chain/kill_npc/命中与未命中/空列）
  S2  归属判定三级（chain / chain_task / 同 key / 无关日常）
  S3  resolve_share_key 反查父行（share_daily_20100 场景）
  S4  keep_playthrough_key（收尾环节保留启动 key）
  S5  次数判定（9/10 未满、10/10 满、limit=0 恒未满、与抓鬼 scan 同源）
  S6  tick 未启用零动作 / 命令回包结构 / 挂载点（源码级）
  S7  demand 分型决策树（交付 / KILLING→点击 / KILLING→巡逻 / COLLECTION / PURCHASE / 满而不可交等待）
  S8  接取三段（无判决→冷却等待 / NOTICE_ADD_TASK 确认 / 失败码停止 / 20s 到达超时）
  S9  交付不覆盖 catcher / can_finish 先等一拍 / 12 次上限停止
  S10 多点轮换（当前图优先 / 跳过已去过 / 耗尽清空）
  S11 重生等待（三点耗尽→退避，坐标不缓存）
  S12 巡逻选点（复用 walk_point_connected / 范围 = PATROL_RANGE / 基准点回退当前图）
  S16 接取白名单放宽（源码级：zhuaogui 与 share_daily_ 前缀 / allow_auto_accept 覆盖）
  S17 __roots_from_chain 列全四个任务号
  S18 parse_task_attr 与抓鬼实测样本回归一致 + 12055 可解析
  S19 点击怪先置 quest.fight_ctx（普攻闸）
  S20 单人校验（队员/有成员→拒绝，无队伍→放行）
  S21 包满 5 码（15s 窗口内→停止；过期不触发）
  S22 掉任务恢复（未满→不停止；满→走 S24；熔断跨周期计数；冷却期 30s 节流）
  S23 9 类停止条件 → STOPPED + 原因 + 上报字段；人工停止
  S24 次数满 ≠ 立刻收工（有在身→先跑完；无后继→等 2s→停；9/10→继续接取）

用法：
  python tools/share_daily_selftest.py [script 目录]
  不带参数默认校验仓库副本 deploy/zones/prod-240-2300/script；
  传生产目录 F:/ZyBin/xm/2d-xiyou-server/robot/deploy/single_robot_zy/script 可再验一次线上文件。

实现方式：stub 注入（config/diag/error/protocol3/quest_engine/quest_state/keys/robot_path/
random_walk/pre_daily）后**真实 import** share_daily —— 能抓模块级 NameError，并可直接
调用模块函数（含模块级私有函数：模块级 `def __x` 不触发名字改写）。
"""
import importlib.util
import io
import contextlib
import marshal
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

fails = 0
total = 0


def check(name, ok, detail=""):
    global fails, total
    total += 1
    fails += 0 if ok else 1
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  " + detail) if detail else ""))


# ================================================================
# 1) stub 环境 + 真实 import
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
        try:
            npcs = (self.chain or {}).get("npcs") or {}
            v = npcs.get(str(npc_id))
            if v is None and npc_index is not None:
                v = npcs.get(str(npc_index))
            if v:
                return [list(v)]
        except Exception:
            pass
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


_EMITTED = []
_QUEST_ENGINE_STUB = None


def _install_stubs():
    global _QUEST_ENGINE_STUB
    # config
    config = types.ModuleType("config")
    config.robot_task_tester = False
    config.robot_spike_tester = False
    config.quest_auto_accept = True
    config.quest_max_retry = 3
    sys.modules["config"] = config

    # diag / error / protocol3
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
    sys.modules["protocol3"] = protocol3

    # quest_state
    qs = types.ModuleType("quest_state")
    for k in ("ST_IDLE", "ST_WAIT_TASK", "ST_NAV", "ST_CLICK", "ST_DIALOG",
              "ST_FIGHT", "ST_SHOP", "ST_RECYCLE", "ST_ALLOC", "ST_WAIT_NEXT",
              "ST_DONE", "ST_ERROR"):
        setattr(qs, k, k.replace("ST_", ""))
    qs.QuestState = FakeQuest
    qs.ERROR_TTL_MS = 180000
    sys.modules["quest_state"] = qs

    # keys（notice / task 常量，值取 keys/*.py 实值）
    keys = types.ModuleType("keys")
    kv = {
        "NOTICE_ADD_TASK": 21,
        "NOTICE_ROLE_TASK_CONDITION": 58,
        "NOTICE_NOT_ACCEPT_TASK_SHARE_DIALY_KEY_CONDITION": 60,
        "NOTICE_NO_SPACE_TO_GIVE": 360,
        "NOTICE_CAN_NOT_GET_BY_FULL_BAG": 415,
        "NOTICE_HAVE_NO_SPACE_IN_BAG": 418,
        "NOTICE_BAG_HAS_NO_SPACE_TO_OPERATOR": 528,
        "NOTICE_BAG_HAS_NO_SPACE_TO_COVER": 549,
        "NOTICE_NPC_ACCEPT_TASK_FREEZE": 666,
        "NOTICE_ADD_TASK_REACH_DAILY_LIMIT": 715,
        "NOTICE_ADD_TASK_REACH_WEEKLY_LIMIT": 716,
        "NOTICE_BAG_FULL_TO_PICK": 839,
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

    # quest_engine
    qe = types.ModuleType("quest_engine")
    call_log = {"teleport_click": [], "schedule": [], "emit": []}
    qe.CALL_LOG = call_log

    def _loads_blob(blob):
        if blob is None:
            return None
        if isinstance(blob, str):
            blob = blob.encode("latin-1", "replace")
        return marshal.loads(bytes(blob))

    def _teleport_click(robot_object, quest, npc_id, npc_index, click_type, delay_ms, **kw):
        call_log["teleport_click"].append((npc_id, npc_index, click_type))
        return True

    def _schedule(quest, p):
        call_log["schedule"].append(p)

    def _emit(robot_object, event):
        call_log["emit"].append(event)
        _EMITTED.append(event)

    qe.__loads_blob = _loads_blob
    qe.__now_ms = lambda: time.time() * 1000
    qe.__human_delay = lambda ms: ms
    qe.__teleport_click = _teleport_click
    qe.__schedule = _schedule
    qe.__emit = _emit
    qe.__tick_walk = lambda ro, q, now: False
    qe.__start_next_hop = lambda ro, q, ms: True
    qe.__do_action = lambda ro, q, p, now: 0
    qe.__handle_dialog = lambda ro, q, data: None
    qe.__recover = lambda ro, q: None
    qe.__check_hop_noack = lambda ro, q, now: False
    qe.__npc_live_pos = lambda ro, npc_id: None
    qe.__find_dijkstra_route = lambda q, fm, tm, **kw: [{"from_map": fm, "target_map": tm,
                                                        "kind": "map_skip"}]
    qe.get_quest = lambda ro, create=False: ro.m_quest
    qe.set_quest_chain = lambda q, chain: setattr(q, "chain", chain)
    qe._clear_recent_error = lambda q: None
    qe.__chain_grid_for = lambda q, m: None
    sys.modules["quest_engine"] = qe
    _QUEST_ENGINE_STUB = qe

    # 函数内惰性 import 的模块
    rp = types.ModuleType("robot_path")

    class MapGrid(object):
        def __init__(self, mapid, gd):
            self.mapid = mapid

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
    sys.modules["daily_ghost"] = dg


def _load_share_daily():
    _install_stubs()
    path = os.path.join(SCRIPT_DIR, "share_daily.py")
    spec = importlib.util.spec_from_file_location("share_daily", path)
    mod = importlib.util.module_from_spec(spec)
    sys.modules["share_daily"] = mod
    with contextlib.redirect_stdout(io.StringIO()):
        spec.loader.exec_module(mod)
    return mod


try:
    S = _load_share_daily()
    check("import share_daily（真实 import，stub 运行时）", True)
except Exception as e:  # noqa: BLE001
    import traceback
    check("import share_daily（真实 import，stub 运行时）", False,
          "%s: %s" % (type(e).__name__, str(e)[:200]))
    print(traceback.format_exc()[:1500])
    sys.exit(1)

CFG = S.get_config(force_reload=True)


def fresh_robot(**kw):
    r = FakeRobot()
    for k, v in kw.items():
        setattr(r, k, v)
    return r


def new_state(share_key="share_daily_大唐神捕", limit=10):
    g = S.ShareDailyState()
    g.enabled = True
    g.share_key = share_key
    g.daily_limit = limit
    g.chain = {"task_order": [
        {"task_index": 2028301, "thrower_npc": 13297, "catcher_npc": 13297, "kill_npc": [18140, 18141, 18142]},
        {"task_index": 2028302, "thrower_npc": 13297, "catcher_npc": 13297},
        {"task_index": 2028311, "thrower_npc": 0, "catcher_npc": 13597, "kill_npc": [13597]},
        {"task_index": 2028399, "thrower_npc": 0, "catcher_npc": 13297},
    ]}
    return g


def mk_task(ti, npc_index=13297, npc_id=13297, can_finish=0, counters=None):
    return [ti, npc_index, npc_id, can_finish, 1, counters or [], None]


def counter(ctype, obj=0, cur=0, req=1):
    return [ctype, obj, cur, 0, req]


# ================================================================
# 2) S1 · 配置层
# ================================================================
check("S1 配置加载成功", CFG.loaded, CFG.load_error)
check("S1 加载行数 = CSV 数据行数(2)",
      sorted(CFG.keys_order) == sorted(["share_daily_捉鬼", "share_daily_大唐神捕"]),
      str(CFG.keys_order))
kw = CFG.get_keywords("share_daily_大唐神捕")
check("S1 keywords 拆分为 [大唐神捕, 回复张忍慎]",
      kw == ["大唐神捕", "回复张忍慎"], str(kw))
check("S1 chain=[share_daily_shenbu_reply]",
      CFG.get_chain_keys("share_daily_大唐神捕") == ["share_daily_shenbu_reply"],
      str(CFG.get_chain_keys("share_daily_大唐神捕")))
check("S1 kill_npc=[18140,18141,18142]",
      CFG.get_kill_npc_list("share_daily_大唐神捕") == [18140, 18141, 18142],
      str(CFG.get_kill_npc_list("share_daily_大唐神捕")))
check("S1 is_auto_executable 命中/未命中",
      CFG.is_auto_executable("share_daily_大唐神捕")
      and not CFG.is_auto_executable("share_daily_不存在"))
# 2026-09-24 事故回归（robot0005274 unknown_share_key）：配置表读取必须显式 encoding ——
# 中文 Windows 的内嵌 Python 缺省按 cp936 打开 UTF-8 表 → UnicodeDecodeError → 表为空 →
# 所有 share_key 被拒。开发机缺省 UTF-8 不复现，故用"源码必须写 encoding="兜底。
_src = open(os.path.join(SCRIPT_DIR, "share_daily.py"), "r", encoding="utf-8").read()
_all_opens = re.findall(r"open\([^)\n]*\)", _src)
check("S1 所有 open() 显式指定 encoding（防 cp936 环境解码失败）",
      bool(_all_opens) and all("encoding=" in _o for _o in _all_opens),
      str(_all_opens))
with open(os.path.join(SCRIPT_DIR, "share_daily_cfg.csv"), "r", encoding="utf-8-sig") as _f:
    _hdr = _f.readline().strip().split(",")[0]
check("S1 表头无 BOM 残留（utf-8-sig 读取）", _hdr == "share_daily_key", repr(_hdr))
# 空列不报错：构造仅 key 的行
c2 = S.ShareDailyConfig()
ok = True
try:
    c2._ShareDailyConfig__parse_line("share_daily_空列测试,,,,,,,,,,,,,,")
    c2._ShareDailyConfig__parse_line("share_daily_极短")
except Exception as e:
    ok = False
    c2.load_error = str(e)
check("S1 空列/短行不报错", ok and len(c2.keys_order) == 2, c2.load_error)

# ================================================================
# 3) S2 · 归属判定三级
# ================================================================
running = "share_daily_大唐神捕"
check("S2 ①经 chain: 2028399(reply key) 属本 run",
      S.belongs_to_run(CFG, running, 2028399, "", "share_daily_shenbu_reply"))
c3 = S.ShareDailyConfig()
c3.keys_order.append("share_daily_宫廷10")
c3.chains["share_daily_宫廷10"] = []
c3.chain_tasks["share_daily_宫廷10"] = [2002107]
check("S2 ②经 chain_task: 构造 2002107 属宫廷10",
      S.belongs_to_run(c3, "share_daily_宫廷10", 2002107))
check("S2 ②经 chain_task(显式注入) 命中", S.belongs_to_run(c3, "share_daily_宫廷10", 2002107))
check("S2 ④b 链任务号集合兜底命中（robot 端扩展）",
      S.belongs_to_run(CFG, running, 2028311, "", "", chain_tasks={2028301, 2028311}))
check("S2 ④c 链任务号集合之外 → False",
      not S.belongs_to_run(CFG, running, 2019501, "", "", chain_tasks={2028301, 2028311}))
check("S2 ③同 key 不同任务号 → True",
      S.belongs_to_run(CFG, running, 2028302, "", running))
check("S2 ④无关日常(捉鬼) → False",
      not S.belongs_to_run(CFG, running, 2019501, "", "share_daily_捉鬼"))
check("S2 ④b 无关任务且无 key/名字 → False",
      not S.belongs_to_run(CFG, running, 2019501, "", ""))

# ================================================================
# 4) S3 · resolve_share_key 反查父行
# ================================================================
c4 = S.ShareDailyConfig()
c4.keys_order.append("share_daily_20100")
all(c4.keywords.setdefault("share_daily_20100", []))
c4.chains["share_daily_20100"] = ["share_daily_201009"]
c4._ShareDailyConfig__chain_parents["share_daily_201009"] = "share_daily_20100"
check("S3 子 key 归一到父行", c4.resolve_share_key("share_daily_201009") == "share_daily_20100")
check("S3 未知 key 原样返回", c4.resolve_share_key("share_daily_无") == "share_daily_无")

# ================================================================
# 5) S4 · keep_playthrough_key
# ================================================================
k1 = S.keep_playthrough_key(CFG, running, 2028399, "share_daily_shenbu_reply")
check("S4 收尾环节保留启动 key", k1 == running, k1)
k2 = S.keep_playthrough_key(CFG, running, 2019501, "share_daily_捉鬼")
check("S4 切到无关任务 → 采用新 key", k2 == "share_daily_捉鬼", k2)

# ================================================================
# 6) S5 · 次数判定（同源校验）
# ================================================================
check("S5 (9,10) 未满", not S.daily_limit_reached(9, 10))
check("S5 (10,10) 满", S.daily_limit_reached(10, 10))
check("S5 limit=0 恒未满", not S.daily_limit_reached(999, 0))
_store = {"2019501": 3, "2019505": 7, "2028301": 4, "2028302": 4, "2028399": 4}

# daily_ghost.__srv_ghost_done 的真实实现（源码级同源：扫段取最大）
def _ghost_scan_max(store, lo, hi):
    mx = None
    for ti in range(lo, hi + 1):
        v = store.get(str(ti))
        if v is None:
            continue
        if mx is None or v > mx:
            mx = v
    return mx


check("S5 同源校验: 同一 store 两实现结果一致(捉鬼段)",
      S.scan_task_limited_max(_store, list(range(2019501, 2019514)))
      == _ghost_scan_max(_store, 2019501, 2019513))
check("S5 神捕段纯函数取最大(2028301/2028302 同 key 共享)",
      S.scan_task_limited_max(_store, [2028301, 2028302, 2028311, 2028399]) == 4)
check("S5 空 store → None", S.scan_task_limited_max({}, [2028301]) is None)
# 计数口径（关键）：只扫主 share_daily 的 episode_root = task_order 首任务号；
# reply 条目（2028311/2028399 各自为根、daily_limit=-1）每轮 +2，全链取最大会虚高一倍
_sb_chain = new_state().chain
_main_roots = getattr(S, "__main_count_roots")(_sb_chain)
check("S5 主键计数只扫 task_order 首任务号", _main_roots == [2028301], str(_main_roots))
_store2 = {"2028301": 3, "2028311": 6, "2028399": 6}
check("S5 reply 条目(6)不污染主键计数(3)",
      S.scan_task_limited_max(_store2, _main_roots) == 3)
r = fresh_robot()
g = new_state()
r.m_share_daily = g
r.m_task_limited = dict(_store2)
g.settle_at_ms = time.time() * 1000 - 1
g.settle_summary = True
getattr(S, "__check_settle")(r, g, time.time() * 1000)
check("S5 轮次结算按主键校准(3, 非 6)", g.done_count == 3, str(g.done_count))

# ================================================================
# 7) S6 · tick 未启用 / 命令回包 / 挂载点
# ================================================================
r = fresh_robot()
g = S.get_state(r, create=True)
g.enabled = False
before = (g.state, g.done_count)
ret = S.tick(r, time.time() * 1000)
check("S6 未启用 tick 返回 0 且不改状态",
      ret == 0 and (g.state, g.done_count) == before)
rep = S.dispatch_cmd(r, {"cmd": "share_daily_status"})
check("S6 命令回包含 {cmd,result}",
      isinstance(rep, dict) and rep.get("cmd") == "share_daily_status" and "result" in rep)
rep2 = S.dispatch_cmd(r, {"cmd": "share_daily_start", "share_key": "share_daily_不存在"})
check("S6 未配置 share_key 拒绝启动", rep2.get("result") == "unknown_share_key", str(rep2))
src_main = open(os.path.join(SCRIPT_DIR, "main_tester.py"), encoding="utf-8").read()
src_client = open(os.path.join(SCRIPT_DIR, "client.py"), encoding="utf-8").read()
src_msg = open(os.path.join(SCRIPT_DIR, "msghandle.py"), encoding="utf-8").read()
check("S6 main_tester 挂载 share_daily.tick + enabled 独占",
      "share_daily.tick(robot_object, now)" in src_main and "m_share_daily" in src_main)
check("S6 client 挂载（缓冲/在线命令/心跳 daily）",
      "share_daily.dispatch_cmd(robot_object, cmd)" in src_client
      and 'cmd_name in ("share_daily_start", "share_daily_stop", "share_daily_status")' in src_client
      and 'st["daily"]' in src_client)
check("S6 msghandle 挂载（事件钩子 + 通知钩子）",
      "share_daily.on_task_event" in src_msg and "share_daily.on_notice" in src_msg)
# 幂等 start：中控 restorer 会周期性补发，重复 start 不得打断正在跑的轮次
r = fresh_robot()
g = new_state()
g.enabled = True
g.state = "KILL"
g.kill_mode = "click"
r.m_share_daily = g
rep = S.dispatch_cmd(r, {"cmd": "share_daily_start", "share_key": "share_daily_大唐神捕",
                         "daily_limit": 10})
check("S6 重复 start 幂等（不打断状态机）",
      rep.get("result") == "already_running" and g.state == "KILL" and g.kill_mode == "click",
      "%s/%s" % (rep.get("result"), g.state))
# 接取 NPC 定位：链数据首环只有 catcher_npc（中控 shenbu_nav.json 现状）时可用
r = fresh_robot()
g = new_state()
r.m_share_daily = g
g.task_index = 0
g.chain = {"task_order": [{"task_index": 2028301, "catcher_npc": "13297"}]}
check("S6b 接取 NPC 走链数据首环 catcher_npc 兜底",
      getattr(S, "__accept_npc_index")(r, g) == 13297,
      str(getattr(S, "__accept_npc_index")(r, g)))
check("S6c 决策节流字段存在（照客户端 UPDATE_INTERVAL=1s）",
      hasattr(g, "decide_ms") and
      "g.decide_ms" in open(os.path.join(SCRIPT_DIR, "share_daily.py"), encoding="utf-8").read())

# ================================================================
# 8) S7 · demand 分型决策树
# ================================================================
def run_ready(g, r=None, tasks=None, counters=None, can_finish=0, ti=2028301):
    r = r or fresh_robot()
    r.m_share_daily = g
    g.share_key = g.share_key or "share_daily_大唐神捕"
    q = r.m_quest
    q.tasks.clear()
    if tasks is None:
        q.tasks[ti] = mk_task(ti, can_finish=can_finish, counters=counters or [])
    else:
        q.tasks.update(tasks)
    _QUEST_ENGINE_STUB.CALL_LOG["teleport_click"] = []
    _QUEST_ENGINE_STUB.CALL_LOG["schedule"] = []
    getattr(S, "__on_ready")(r, g, q, time.time() * 1000)
    return r, q


g7 = new_state()
run_ready(g7, counters=[counter(11883, 30126, 0, 1)], can_finish=1)   # 首次：环节切换
g7.finishable_seen_ms = time.time() * 1000 - 2000                     # 已等过一拍（S9 的 FINISHABLE_WAIT）
r7, q7 = run_ready(g7, counters=[counter(11883, 30126, 0, 1)], can_finish=1)
check("S7 ①可交付 → SUBMIT", g7.state == "SUBMIT", g7.state)
g7 = new_state()
r7, q7 = run_ready(g7, counters=[counter(11883, 30126, 0, 1)])
check("S7 ②KILLING(无坐标无实例) → KILL 或 PATROL",
      g7.state in ("KILL", "PATROL"), g7.state)
g7 = new_state()
r7 = fresh_robot()
q7 = r7.m_quest
ka = marshal.dumps({12053: (12, 1947, 2637)})
q7.tasks[2028302] = mk_task(2028302, counters=[counter(11883, 30124, 0, 1)])
q7.tasks[2028302][-1] = ka
r7.m_share_daily = g7
getattr(S, "__start_kill_path")(r7, g7, q7, q7.tasks[2028302],
                                {"counter_type": 11883, "object_index": 30124,
                                 "current_count": 0, "required_count": 1},
                                time.time() * 1000)
check("S7 ③KILLING + 12053 → 巡逻型 PATROL", g7.state == "PATROL", g7.state)
g7 = new_state()
r7, q7 = run_ready(g7, counters=[counter(11886, 101306, 0, 1)])
check("S7 ④COLLECTION 无掉落无库存 → 停止(BUY_FAILED, P1采购路径)",
      g7.state == "STOPPED" and g7.stop_code == "BUY_FAILED", "%s/%s" % (g7.state, g7.stop_code))
g7 = new_state()
r7, q7 = run_ready(g7, counters=[counter(11887, 102031, 0, 1)])
check("S7 ⑤PURCHASE → 停止(BUY_FAILED)",
      g7.state == "STOPPED" and g7.stop_code == "BUY_FAILED", "%s/%s" % (g7.state, g7.stop_code))
g7 = new_state()
r7, q7 = run_ready(g7, counters=[counter(11883, 30126, 1, 1)], can_finish=0)
check("S7 ⑥需求全满但不可交 → WAIT(原地等服务端)",
      g7.state == "WAIT", g7.state)

# ================================================================
# 9) S8 · 接取三段
# ================================================================
def accept_case(notice=None, bag_full=False, waited_s=0.0):
    r = fresh_robot()
    g = new_state()
    g.state = "ACCEPT"
    g.state_since_ms = time.time() * 1000
    r.m_share_daily = g
    if bag_full:
        g.bag_full_ms = time.time() * 1000
    if notice is not None:
        S.on_notice(r, [notice, "x"])
    # 模拟"首次点击后 N 秒"
    if waited_s:
        g.accept_start_ms = (time.time() - waited_s) * 1000
        getattr(S, "__on_accept")(r, g, r.m_quest, time.time() * 1000)
    return r, g


r, g = accept_case(notice=21)
check("S8 ①NOTICE_ADD_TASK 确认 → 计时清零", g.accept_start_ms == 0)
r, g = accept_case(waited_s=4.0)
check("S8 ②3s 无判决(日限未满) → 进冷却等待, 不判失败",
      g.accept_cd_start_ms > 0 and g.state != "STOPPED", "%s/%s" % (g.state, g.accept_cd_start_ms))
r, g = accept_case(notice=715)
check("S8 ③失败码(今日上限) → STOPPED + 原因",
      g.state == "STOPPED" and "今日次数已用完" in g.stop_reason, g.stop_reason)
# 20s 到达超时（位置未知、非冷却期）
r = fresh_robot()
g = new_state()
g.state = "ACCEPT"
g.accept_arrive_ms = (time.time() - 25.0) * 1000
r.m_share_daily = g
r.m_quest.chain = {"task_order": [{"task_index": 2028301, "thrower_npc": 13297}]}
r.m_npc_positions = {}
getattr(S, "__on_accept")(r, g, r.m_quest, time.time() * 1000)
check("S8 ④走不到 NPC 20s → ACCEPT_UNREACHABLE",
      g.state == "STOPPED" and g.stop_code == "ACCEPT_UNREACHABLE",
      "%s/%s" % (g.state, g.stop_code))

# ================================================================
# 10) S9 · 交付（不覆盖 catcher / 先等一拍 / 12 次上限）
# ================================================================
r = fresh_robot()
g = new_state()
r.m_share_daily = g
q = r.m_quest
q.chain = g.chain
# 2028311: catcher = 13597（B 续），配置 submit_npc=13297 不得覆盖
q.tasks[2028311] = mk_task(2028311, npc_index=13597, npc_id=13597)
q.chain["npcs"] = {"13597": [12, 2265, 1846]}
_QUEST_ENGINE_STUB.CALL_LOG["teleport_click"] = []
g.state = "SUBMIT"
getattr(S, "__on_submit")(r, g, q, time.time() * 1000)
clicks = _QUEST_ENGINE_STUB.CALL_LOG["teleport_click"]
check("S9 交付用任务自带 catcher 13597（不走去 13297）",
      bool(clicks) and clicks[0][0] == 13597 and clicks[0][1] == 13597, str(clicks[:2]))
# can_finish 翻真先等一拍
g9 = new_state()
r9 = fresh_robot()
r9.m_share_daily = g9
q9 = r9.m_quest
q9.tasks[2028301] = mk_task(2028301, can_finish=1, counters=[counter(11883, 30126, 1, 1)])
g9.finishable_seen_ms = time.time() * 1000
getattr(S, "__on_ready")(r9, g9, q9, time.time() * 1000)
check("S9 can_finish 后 1s 内先等一拍（不空跑 NPC）", g9.state != "SUBMIT", g9.state)
# 12 次上限
r12 = fresh_robot()
g12 = new_state()
r12.m_share_daily = g12
q12 = r12.m_quest
q12.tasks[2028301] = mk_task(2028301, can_finish=1, counters=[])
q12.chain = {"npcs": {"13297": [12, 2265, 1846]}}
g12.state = "SUBMIT"
g12.finish_try = 12
g12.finish_try_task = 2028301        # 同一环节已尝试 12 次（计数不因环节切换重置）
getattr(S, "__on_submit")(r12, g12, q12, time.time() * 1000)
check("S9 交付尝试超 12 次 → HANDIN_STUCK 停止",
      g12.state == "STOPPED" and g12.stop_code == "HANDIN_STUCK",
      "%s/%s" % (g12.state, g12.stop_code))

# ================================================================
# 11) S10 / S11 · 多点轮换 + 重生等待
# ================================================================
locs = [(12, 100, 100), (12, 200, 200), (10, 300, 300)]
t, tr = S.pick_target_location(locs, 12, set())
check("S10 当前图优先取第一点", t == (12, 100, 100), str(t))
t2, tr2 = S.pick_target_location(locs, 12, {t})
check("S10 跳过已去过", t2 == (12, 200, 200), str(t2))
t3, tr3 = S.pick_target_location(locs, 12, {locs[0], locs[1]})
check("S10 耗尽后清空重来", t3 == (12, 100, 100) and tr3 == set(), "%s/%s" % (t3, tr3))
# 无可点实例 → 选中"当前图可点"的那只（多目标怪）
r = fresh_robot()
q = r.m_quest
q.dyn_npc_meta = {1001: 18140, 1002: 18141}
q.dynamic_npcs = {1001: [12, 9999, 9999], 1002: [12, 2300, 1900]}
got = getattr(S, "__find_clickable_instance")(r, q, [18140, 18141, 18142])
check("S10 选可点实例而非列表首项", got is not None and got[0] == 1002, str(got))
# S11 三点耗尽 → 退避且不缓存旧坐标
r = fresh_robot()
g = new_state()
r.m_share_daily = g
q = r.m_quest
q.tasks[2028301] = mk_task(2028301, counters=[counter(11883, 30126, 0, 1)])
g.state = "KILL"
g.target_locs = [(12, 100, 100)]
g.tried_locations = set()
r.m_pose = (100, 100)
r.m_mapid = 12
getattr(S, "__on_kill")(r, g, q, time.time() * 1000)
check("S11 到点无可点怪 → 标记该点已去过", (12, 100, 100) in g.tried_locations)
g.kill_tries = 99
getattr(S, "__on_kill")(r, g, q, time.time() * 1000)
check("S11 尝试超限 → 进入 60s 重生退避（不缓存坐标）",
      g.respawn_until_ms > 0 and g.target_locs == [(12, 100, 100)],
      "respawn=%s" % (g.respawn_until_ms - time.time() * 1000))
check("S11 退避期间不动作（坐标每 tick 重读 dynamic_npcs）",
      q.dynamic_npcs == {})

# ================================================================
# 12) S12 · 巡逻
# ================================================================
r = fresh_robot()
g = new_state()
r.m_share_daily = g
q = r.m_quest
q.tasks[2028301] = mk_task(2028301, counters=[counter(11883, 30126, 0, 3)])
g.state = "PATROL"
g.patrol_center = (12, 2265, 1846)
_QUEST_ENGINE_STUB.CALL_LOG["schedule"] = []
getattr(S, "__on_patrol")(r, g, q, time.time() * 1000)
sched = _QUEST_ENGINE_STUB.CALL_LOG["schedule"]
walk = sched[-1] if sched else None
ok_scope = False
if walk and walk.get("type") == "walk":
    tx = int(walk["data"]["to_x"])
    ty = int(walk["data"]["to_y"])
    ok_scope = abs(tx - 2265) <= S.PATROL_RANGE and abs(ty - 1846) <= S.PATROL_RANGE
check("S12 巡逻游走在基准 ±PATROL_RANGE(400) 内", ok_scope, str(walk))
src_sd = open(os.path.join(SCRIPT_DIR, "share_daily.py"), encoding="utf-8").read()
check("S12 复用 random_walk.walk_point_connected（不复制实现）",
      "walk_point_connected" in src_sd and "walk_point_connected" in
      open(os.path.join(SCRIPT_DIR, "random_walk.py"), encoding="utf-8").read())
check("S12 巡逻参数照抄客户端(400/300)",
      S.PATROL_RANGE == 400 and S.PATROL_EDGE_MARGIN == 300)

# ================================================================
# 13) S16 · 接取白名单（源码级）
# ================================================================
src_qe = open(os.path.join(SCRIPT_DIR, "quest_engine.py"), encoding="utf-8").read()
check("S16 白名单含 zhuaogui + shenbu_nav + share_daily_ 前缀",
      'startswith("share_daily_")' in src_qe and '_chain_id != "zhuaogui"' in src_qe
      and '"shenbu_nav"' in src_qe)
check("S16 支持链数据 allow_auto_accept 覆盖", "allow_auto_accept" in src_qe)

# ================================================================
# 14) S17 · 链任务号集合
# ================================================================
roots = getattr(S, "__roots_from_chain")(new_state().chain)
check("S17 roots 列全 4 个任务号",
      set(roots) == {2028301, 2028302, 2028311, 2028399}, str(roots))
# 仓库链声明文件（中控侧 S17 产物）现状校验：若存在，task_order 必须列全 4 个任务号
_nav_path = os.path.normpath(os.path.join(HERE, "..", "data", "chains", "shenbu_nav.json"))
if os.path.exists(_nav_path):
    import json as _json
    _nav = _json.load(open(_nav_path, encoding="utf-8"))
    _tis = set()
    for _e in (_nav.get("task_order") or []):
        try:
            _tis.add(int(_e.get("task_index") or 0))
        except Exception:
            pass
    check("S17b 链声明文件 shenbu_nav.json task_order 列全 4 个任务号",
          {2028301, 2028302, 2028311, 2028399} <= _tis, str(sorted(_tis)))
    check("S17b 链声明 chain_id 与中控下发一致（shenbu_nav 白名单放行）",
          _nav.get("chain_id") == "shenbu_nav", str(_nav.get("chain_id")))
else:
    check("S17b 链声明文件 shenbu_nav.json 存在", False, _nav_path)

# ================================================================
# 15) S18 · 任务属性解析（回归 + 12055）
# ================================================================
sample = [0, 0, 0, 0, 0, [], marshal.dumps({12008: 38, 12010: 1, 12054: ((12, 3392, 1184),)})]
ka, locs, raw, ak, at = S.parse_task_attr(sample)
check("S18 抓鬼实测样本回归（kill_area=None, locs=[(12,3392,1184)]）",
      ka is None and locs == [(12, 3392, 1184)], "%s/%s" % (ka, locs))
sample2 = [0, 0, 0, 0, 0, [], marshal.dumps({12054: ((12, 100, 100),), 12055: [101306, 102031]})]
items = S.parse_task_item_list(sample2)
check("S18 12055 可解析", items == [101306, 102031], str(items))
check("S18 12053 单点解析", S.parse_task_attr(
    [0, 0, 0, 0, 0, [], marshal.dumps({12053: (12, 1947, 2637)})])[0] == [12, 1947, 2637])
src_dg = open(os.path.join(SCRIPT_DIR, "daily_ghost.py"), encoding="utf-8").read()
check("S18 daily_ghost 委托公共实现",
      "share_daily.parse_task_attr(datalist)" in src_dg)
keys_src = open(os.path.join(SCRIPT_DIR, "keys", "task_.py"), encoding="utf-8").read()
check("S18 keys/task_.py 补 12054/12055",
      "TASK_TARGET_LOCATION_LIST = 12054" in keys_src and "TASK_ITEM_INDEX_LIST = 12055" in keys_src)

# ================================================================
# 16) S19 · 普攻闸（点击怪前置 fight_ctx）
# ================================================================
r = fresh_robot()
g = new_state()
r.m_share_daily = g
q = r.m_quest
getattr(S, "__click_kill")(r, g, q, 1001, 18140, time.time() * 1000)
check("S19 点击怪前设置 quest.fight_ctx（剧情战闸→普攻）",
      isinstance(q.fight_ctx, dict) and q.fight_ctx.get("npc_index") == 18140,
      str(q.fight_ctx))

# ================================================================
# 17) S20 · 单人校验
# ================================================================
r = fresh_robot()
check("S20 无队伍 → 放行", S.team_block_reason(r) == "")
t = types.SimpleNamespace(role="member", confirmed_role_ids=[], captain_role_id=1)
r.m_team = t
check("S20 队员 → 拒绝", "成员" in S.team_block_reason(r), S.team_block_reason(r))
t = types.SimpleNamespace(role="captain", confirmed_role_ids=[123, 456], captain_role_id=0)
r.m_team = t
check("S20 队长带成员 → 拒绝", "其他成员" in S.team_block_reason(r), S.team_block_reason(r))

# ================================================================
# 18) S21 · 包满 5 码
# ================================================================
for code in (360, 415, 418, 528, 1295):
    r = fresh_robot()
    g = new_state()
    r.m_share_daily = g
    S.on_notice(r, [code, "x"])
    g.state = "ACCEPT"
    g.accept_start_ms = (time.time() - 4.0) * 1000
    getattr(S, "__on_accept")(r, g, r.m_quest, time.time() * 1000)
    check("S21 码 %d → 停止(包裹已满)" % code,
          g.state == "STOPPED" and g.stop_code == "BAG_FULL",
          "%s/%s" % (g.state, g.stop_code))
r = fresh_robot()
g = new_state()
r.m_share_daily = g
g.bag_full_ms = time.time() * 1000 - (S.BAG_FULL_NOTICE_WINDOW + 5) * 1000
check("S21 窗口过期不再触发", not getattr(S, "__bag_full_recently")(g))

# ================================================================
# 19) S22 · 掉任务恢复
# ================================================================
r = fresh_robot()
g = new_state()
g.enabled = True
r.m_share_daily = g
q = r.m_quest
q.tasks[2028301] = mk_task(2028301)
S.on_task_event(r, "drop_task", [2028301])
check("S22①a 掉任务(日限未满) → 回 READY/不 STOPPED",
      g.state != "STOPPED" and g.drop_streak == 1, "%s/%s" % (g.state, g.drop_streak))
check("S22①b 日限未满时进入接取流程（READY→ACCEPT 由 tick 推进）",
      g.state == "READY" and g.accept_cd_start_ms == 0, g.state)
check("S22①c 冷却期点击节流 ≥ BROKER_CLICK_MIN_GAP_MS",
      S.BROKER_CLICK_MIN_GAP_MS >= 1200 and S.ACCEPT_COOLDOWN_LOG_INTERVAL == 30.0,
      "%s/%s" % (S.BROKER_CLICK_MIN_GAP_MS, S.ACCEPT_COOLDOWN_LOG_INTERVAL))
# ② 日限满 + 有在身任务 → 不得 STOPPED
r = fresh_robot()
g = new_state(limit=10)
g.done_count = 10
r.m_share_daily = g
q = r.m_quest
q.tasks[2028399] = mk_task(2028399, can_finish=0, counters=[])
S.on_task_event(r, "drop_task", [2028301])
check("S22② 满额掉任务但有在身收尾 → 不 STOPPED",
      g.state != "STOPPED", g.state)
# ③ 熔断跨周期计数
r = fresh_robot()
g = new_state()
r.m_share_daily = g
q = r.m_quest
q.tasks[2028301] = mk_task(2028301)
S.on_task_event(r, "drop_task", [2028301])
g.drop_seen_ms = time.time() * 1000 - 700000    # 上一条已超冷却周期
q.tasks[2028302] = mk_task(2028302)
S.on_task_event(r, "drop_task", [2028302])
check("S22③ 跨冷却周期重新计数(不假熔断)", g.drop_streak == 1, str(g.drop_streak))
q.tasks[2028301] = mk_task(2028301)
S.on_task_event(r, "drop_task", [2028301])
q.tasks[2028301] = mk_task(2028301)
S.on_task_event(r, "drop_task", [2028301])
check("S22③ 同周期连掉 → 熔断停止",
      g.state == "STOPPED" and g.stop_code == "DROP_STREAK",
      "%s/%s/%s" % (g.state, g.stop_code, g.drop_streak))

# ================================================================
# 20) S23 · 停止条件 9 类 + 上报字段
# ================================================================
STOP_CASES = [
    ("接取被拒(码表)", "ACCEPT_REFUSED"),
    ("接取3s无判决→900s超时", "ACCEPT_NO_VERDICT"),
    ("走不到broker 20s", "ACCEPT_UNREACHABLE"),
    ("次数用尽", "DAILY_LIMIT"),
    ("购买失败(P1)", "BUY_FAILED"),
    ("回收3次未确认(P1, 机器人端复用交付上限)", "HANDIN_STUCK"),
    ("交付12次未确认", "HANDIN_STUCK"),
    ("包裹满", "BAG_FULL"),
    ("人工停止", "MANUAL"),
]
for label, code in STOP_CASES:
    r = fresh_robot()
    g = new_state()
    r.m_share_daily = g
    g.state = "READY"
    getattr(S, "__request_stop")(r, g, code, "%s 测试" % label)
    check("S23 停止条件[%s] → STOPPED + 原因非空" % label,
          g.state == "STOPPED" and bool(g.stop_reason), "%s/%s" % (g.state, g.stop_reason))
_EMITTED[:] = []
r = fresh_robot()
g = new_state()
r.m_share_daily = g
getattr(S, "__request_stop")(r, g, "DAILY_LIMIT", "x")
ev = _EMITTED[-1] if _EMITTED else {}
check("S23 上报事件含 state/reason/done/limit",
      all(k in ev for k in ("state", "reason", "done", "limit")), str(ev)[:120])
r = fresh_robot()
g = new_state()
r.m_share_daily = g
S.dispatch_cmd(r, {"cmd": "share_daily_stop"})
check("S23 人工停止 → STOPPED + MANUAL",
      g.state == "STOPPED" and g.stop_code == "MANUAL" and not g.enabled,
      "%s/%s" % (g.state, g.stop_code))

# ================================================================
# 21) S24 · 次数满 ≠ 立刻收工
# ================================================================
r = fresh_robot()
g = new_state(limit=10)
g.done_count = 10
r.m_share_daily = g
q = r.m_quest
q.tasks[2028399] = mk_task(2028399, can_finish=0, counters=[])
handled = getattr(S, "__stop_if_daily_limit_reached")(r, g, time.time() * 1000)
check("S24 ①10/10 + 手上有 2028399 → 不得 STOPPED, 先跑完",
      handled and g.state != "STOPPED", "%s/%s" % (handled, g.state))
# ② 无在身任务 → 等 2s → STOPPED
r = fresh_robot()
g = new_state(limit=10)
g.done_count = 10
r.m_share_daily = g
t0 = time.time() * 1000
handled = getattr(S, "__stop_if_daily_limit_reached")(r, g, t0)
check("S24 ②a 首次判定开等 2s 窗口(不立刻停)", handled and g.state != "STOPPED" and g.limit_wait_until > t0)
handled = getattr(S, "__stop_if_daily_limit_reached")(r, g, t0 + 2500)
check("S24 ②b 等满 2s 无后继 → STOPPED(今日次数已用完)",
      g.state == "STOPPED" and "今日次数已用完" in g.stop_reason, g.stop_reason)
# ③ 9/10 → 继续接取
r = fresh_robot()
g = new_state(limit=10)
g.done_count = 9
r.m_share_daily = g
handled = getattr(S, "__stop_if_daily_limit_reached")(r, g, time.time() * 1000)
check("S24 ③9/10 → 不触发停止（继续接取）", not handled and g.state != "STOPPED")
check("S24 DAILY_LIMIT_DELIVERY_WAIT=2s（照抄客户端）", S.DAILY_LIMIT_DELIVERY_WAIT == 2.0)

# ================================================================
# 21.5) tick 集成冒烟（抓 tick 内 NameError / 流程串联）
# ================================================================
r = fresh_robot()
g = new_state()
r.m_share_daily = g
g.state = "ACCEPT"
g.state_since_ms = time.time() * 1000
_err = None
try:
    S.tick(r, time.time() * 1000)
except Exception as e:
    _err = "%s: %s" % (type(e).__name__, e)
check("S0 tick 集成: 启用 + 无任务(ACCEPT) 不抛异常", _err is None, _err or "")
r = fresh_robot()
g = new_state()
r.m_share_daily = g
g.state = "READY"
g.state_since_ms = time.time() * 1000
q = r.m_quest
q.tasks[2028301] = mk_task(2028301, counters=[counter(11883, 30126, 0, 1)])
q.dyn_npc_meta = {777: 18140}
q.dynamic_npcs = {777: [12, 2300, 1900]}
r.m_mapid = 12
r.m_pose = (2300, 1900)
_QUEST_ENGINE_STUB.CALL_LOG["teleport_click"] = []
_err = None
try:
    S.tick(r, time.time() * 1000)
except Exception as e:
    _err = "%s: %s" % (type(e).__name__, e)
_clicks = _QUEST_ENGINE_STUB.CALL_LOG["teleport_click"]
check("S0 tick 集成: 有可点怪 → 点击开战 + 普攻闸 fight_ctx",
      _err is None and bool(_clicks) and _clicks[0][0] == 777
      and isinstance(q.fight_ctx, dict), "%s / %s" % (_err, str(_clicks[:2])))

# ================================================================
# 22) 汇总
# ================================================================
print("\n自检目标: %s" % SCRIPT_DIR)
print("结果：%d 项，失败 %d 项" % (total, fails))
sys.exit(1 if fails else 0)
