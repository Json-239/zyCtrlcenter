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
  S16 接取白名单放宽（源码级：zhuaogui/shenbu_nav/fenghuo_nav 与 share_daily_ 前缀 / allow_auto_accept 覆盖）
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
    # 2026-09-29 2 池修复包：share_daily 包满清理复用 daily_ghost.__tidy_bag（stub 记录调用）
    dg.TIDY_LOG = {"calls": 0}
    def _dg_tidy_bag(ro):
        dg.TIDY_LOG["calls"] += 1
        return 1
    setattr(dg, "__tidy_bag", _dg_tidy_bag)
    sys.modules["daily_ghost"] = dg
    # 2026-09-24：会话内吃药接线（share_daily.tick → auto_summon.heal_tick）
    asm = types.ModuleType("auto_summon")
    asm.CALL_LOG = {"heal_tick": 0}
    def _asm_heal_tick(ro, now_ms=None):
        asm.CALL_LOG["heal_tick"] += 1
        return 0
    asm.heal_tick = _asm_heal_tick
    sys.modules["auto_summon"] = asm


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
check("S1 加载行数 = CSV 数据行数(3: 捉鬼/神捕/宫廷10)",
      sorted(CFG.keys_order) == sorted(["share_daily_捉鬼", "share_daily_大唐神捕",
                                        "share_daily_宫廷10"]),
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
# 计数口径（关键）：扫 task_order 前两条（A/B 主任务）——容器 root = 首个被接的主任务
# （A 先接→2028301；B 先接→2028302；2026-09-24 修正：只扫首条会漏 B 先接的号）；
# reply 条目（2028311/2028399 各自为根、daily_limit=-1）每轮 +2，全链取最大会虚高一倍。
# 2026-09-24 P1-g：__main_count_roots 增加 share_key（按配置 chain 列判断 reply 链 → 只取主任务；
#   烽火无 reply 链 → 排除 chain_task 收尾后全扫），下面同时覆盖神捕（回归）与烽火（新增）。
_sb_chain = new_state().chain
_main_roots = getattr(S, "__main_count_roots")(_sb_chain, "share_daily_大唐神捕")
check("S5 主键计数扫 task_order 前两条（A/B 主任务）", _main_roots == [2028301, 2028302], str(_main_roots))
_store2 = {"2028301": 3, "2028311": 6, "2028399": 6}
check("S5 reply 条目(6)不污染主键计数(3)",
      S.scan_task_limited_max(_store2, _main_roots) == 3)
_store2b = {"2028302": 2, "2028311": 4, "2028399": 4}  # B 先接：容器在 2028302
check("S5 B 先接的号也能扫到（2028302 容器=2）",
      S.scan_task_limited_max(_store2b, _main_roots) == 2)
r = fresh_robot()
g = new_state()
r.m_share_daily = g
r.m_task_limited = dict(_store2)
g.settle_at_ms = time.time() * 1000 - 1
g.settle_summary = True
getattr(S, "__check_settle")(r, g, time.time() * 1000)
check("S5 轮次结算按主键校准(3, 非 6)", g.done_count == 3, str(g.done_count))
# 2026-09-24：今日完成数持久化（服务端无记录 + 机器人重启不丢；现场 5275 跑过 1 轮重启回 0）
import json as _json
import tempfile as _tempfile
_tmpdir = _tempfile.mkdtemp(prefix="sd_ct_")
os.environ["ZCC_SHARE_DAILY_STATE_DIR"] = _tmpdir
r = fresh_robot()
g = new_state()
g.done_count = 1
check("S5b 计数落盘 ok", getattr(S, "__save_local_count")(r, g) is True)
check("S5b 计数读回 = 1", getattr(S, "__load_local_count")(r) == 1)
_p = getattr(S, "__ct_state_path")(r)
with open(_p, "w", encoding="utf-8") as _f:
    _json.dump({"date": "19700101", "done": 9}, _f)
check("S5b 旧日期记录不认", getattr(S, "__load_local_count")(r) == 0)
with open(_p, "w", encoding="utf-8") as _f:
    _f.write("{broken")
check("S5b 损坏文件按无记录", getattr(S, "__load_local_count")(r) == 0)
# 启动时本地计数恢复（无服务端记录场景）：预写今日 1 → start 后 done=1
with open(_p, "w", encoding="utf-8") as _f:
    _json.dump({"date": time.strftime("%Y%m%d"), "done": 1}, _f)
r = fresh_robot()
g = S.ShareDailyState()
r.m_share_daily = g
_rep = S.dispatch_cmd(r, {"cmd": "share_daily_start", "share_key": "share_daily_大唐神捕",
                          "daily_limit": 10,
                          "chain": {"task_order": [{"task_index": 2028301}, {"task_index": 2028302}]}})
check("S5b 启动时恢复本地计数（重启不丢：1 保持为 1）",
      g.done_count == 1, "done=%s rep=%s" % (g.done_count, str(_rep)[:60]))
# 2026-09-24 回归：下一轮"秒接"不再吞计数 —— 结算等待只认"本轮收尾（链尾）在身"；
# 在身=下一轮主任务 → 本轮照常结算（现场 5275 第 2 轮被吞）。
os.environ["ZCC_SHARE_DAILY_STATE_DIR"] = _tmpdir
r = fresh_robot()
g = new_state()
r.m_share_daily = g
getattr(S, "__save_local_count")(r, g)  # 清基线
q = r.m_quest
q.tasks[2028301] = mk_task(2028301)          # 在身=下一轮主任务（秒接场景）
g.settle_at_ms = time.time() * 1000 - 1
g.settle_summary = True
g.settle_last_ms = 0
_d0 = g.done_count
getattr(S, "__check_settle")(r, g, time.time() * 1000)
check("S5c 在身=下一轮主任务 → 照常结算(+1)", g.done_count == _d0 + 1, str(g.done_count))
r = fresh_robot()
g = new_state()
r.m_share_daily = g
q = r.m_quest
q.tasks[2028399] = mk_task(2028399)          # 在身=本轮收尾（链尾）→ 继续等
g.settle_at_ms = time.time() * 1000 - 1
g.settle_summary = True
g.settle_last_ms = 0
_d1 = g.done_count
getattr(S, "__check_settle")(r, g, time.time() * 1000)
check("S5c 在身=本轮收尾 → 仍等待（不结算）", g.done_count == _d1, str(g.done_count))
os.environ.pop("ZCC_SHARE_DAILY_STATE_DIR", None)

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
# 2026-09-24：商店采购进行中 → 拒绝启动（零副作用）——镜像 random_walk 的 shop_busy 拒绝，
# 下面会清 quest 导航态，采购编排层(shop_errand)会静默卡死。
r = fresh_robot()
g = S.ShareDailyState()  # 默认未启用
r.m_share_daily = g
r.m_quest.shop_ctx = {"lock": 1}
rep = S.dispatch_cmd(r, {"cmd": "share_daily_start", "share_key": "share_daily_大唐神捕",
                         "daily_limit": 10})
check("S6c 采购中拒绝启动（zero side-effect）",
      rep.get("result") == "shop_busy" and not g.enabled, str(rep))
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
# 2026-09-24 现场回归（robot0005207 跨图途中被误判 ACCEPT_UNREACHABLE）：照抄客户端语义 ——
# 寻路进行中不判超时；NPC 异图 → 重新寻路且清零计时；仅"同图走不到"才计 20s。
def _accept_nav_case(robot_map, npc_map, nav_busy, arrive_age_s):
    _r = fresh_robot()
    _g = new_state()
    _g.state = "ACCEPT"
    _r.m_share_daily = _g
    _r.m_mapid = robot_map
    _r.m_pose = (100, 100)
    _r.m_quest.chain = {"task_order": [{"task_index": 2028301, "thrower_npc": 13297}],
                        "npcs": {"13297": [npc_map, 2265, 1846]}}
    if nav_busy:
        _r.m_quest.dijkstra_route = [[npc_map, 2265, 1846]]
    if arrive_age_s:
        _g.accept_arrive_ms = (time.time() - arrive_age_s) * 1000
    _QUEST_ENGINE_STUB.CALL_LOG["teleport_click"] = []
    getattr(S, "__on_accept")(_r, _g, _r.m_quest, time.time() * 1000)
    return _r, _g


r, g = _accept_nav_case(37, 12, nav_busy=True, arrive_age_s=25.0)
check("S8 ⑤跨图寻路中(已 25s) → 不判 ACCEPT_UNREACHABLE（不打断寻路）",
      g.state != "STOPPED", "%s/%s" % (g.state, g.stop_code))
r, g = _accept_nav_case(37, 12, nav_busy=False, arrive_age_s=25.0)
check("S8 ⑥NPC 异图且未在寻路 → 重新寻路 + 计时清零，不判超时",
      g.state != "STOPPED" and g.accept_arrive_ms == 0
      and bool(_QUEST_ENGINE_STUB.CALL_LOG["teleport_click"]),
      "%s/%s clicks=%s" % (g.state, g.stop_code, _QUEST_ENGINE_STUB.CALL_LOG["teleport_click"][:1]))
r, g = _accept_nav_case(12, 12, nav_busy=False, arrive_age_s=25.0)
check("S8 ⑦同图但走不到 25s → 仍判 ACCEPT_UNREACHABLE（保留原语义）",
      g.state == "STOPPED" and g.stop_code == "ACCEPT_UNREACHABLE",
      "%s/%s" % (g.state, g.stop_code))
# 2026-09-24 现场回归（robot0005275 接取对话被误判"无关对话"）：g.accept_npc 此前从未赋值，
# __on_show_dialog 的"本 run 对话"判定恒 False → 接取对话全被退出 → 接取失败。
r = fresh_robot()
g = new_state()
g.state = "ACCEPT"
r.m_share_daily = g
r.m_quest.chain = {"task_order": [{"task_index": 2028301, "thrower_npc": 13297}]}
getattr(S, "__on_accept")(r, g, r.m_quest, time.time() * 1000)
check("S8 ⑧ __on_accept 写回 accept_npc（供对话归属判定）",
      g.accept_npc == 13297, str(g.accept_npc))
_QUEST_ENGINE_STUB.CALL_LOG["schedule"] = []
_dlg = [0, 0, 13297, "我奉命来调查朝廷重臣的行踪，请把你知道的事情告诉我吧。", 0,
        [("#i901#大唐神捕", 0), ("离开", 1)]]
getattr(S, "__on_show_dialog")(r, g, _dlg)
_sched = _QUEST_ENGINE_STUB.CALL_LOG["schedule"]
check("S8 ⑨ 接取对话（#i901#大唐神捕）→ 选接取项而非退出项",
      bool(_sched) and _sched[-1].get("data", {}).get("option_index") == 0,
      str(_sched[-1:]))
# 2026-09-24 现场回归（robot0005275 打匪首"开战"对话被误判无关而退出）：击杀实例的对话算本 run
r = fresh_robot()
g = new_state()
g.state = "KILL"
g.kill_npc_id = 1923723
r.m_share_daily = g
_QUEST_ENGINE_STUB.CALL_LOG["schedule"] = []
_dlg2 = [0, 0, 1923723, "不是吧，这么冒昧的和我说话，难道有什么企图？", 0,
         [("#i902#大唐神捕", 0), ("离开", 1)]]
getattr(S, "__on_show_dialog")(r, g, _dlg2)
_sched2 = _QUEST_ENGINE_STUB.CALL_LOG["schedule"]
check("S8 ⑩ 击杀实例的'开战'对话 → 选【大唐神捕】而非退出",
      bool(_sched2) and _sched2[-1].get("data", {}).get("option_index") == 0,
      str(_sched2[-1:]))
# 2026-09-24 现场回归（三号卡死 25→12 循环）：跳转NPC传送对话不放行关闭（交 quest_engine），
# 只有挑战/战斗类无关对话仍关闭 —— 照抄 daily_ghost 生产口径。
r = fresh_robot()
g = new_state()
g.state = "ACCEPT"
r.m_share_daily = g
_QUEST_ENGINE_STUB.CALL_LOG["schedule"] = []
_dlgj = [0, 0, 10147, "生老病死，六道轮回，谁都不能幸免。", 0,
         [("送我去东海村（免费）", 0), ("送我去长安城中观星台（2银）", 0), ("离开", 1)]]
_retj = getattr(S, "__on_show_dialog")(r, g, _dlgj)
check("S8 ⑪ 跳转NPC目的地菜单 → 不关闭、放行 quest_engine",
      _retj is False and not _QUEST_ENGINE_STUB.CALL_LOG["schedule"],
      "ret=%s sched=%s" % (_retj, _QUEST_ENGINE_STUB.CALL_LOG["schedule"]))
_QUEST_ENGINE_STUB.CALL_LOG["schedule"] = []
_dlgb = [0, 0, 999999, "来战吧！", 0, [("进行挑战", 0), ("离开", 1)]]
_retb = getattr(S, "__on_show_dialog")(r, g, _dlgb)
check("S8 ⑫ 挑战类无关对话 → 仍点关闭（不放行）",
      _retb is True and bool(_QUEST_ENGINE_STUB.CALL_LOG["schedule"]),
      "ret=%s sched=%s" % (_retb, _QUEST_ENGINE_STUB.CALL_LOG["schedule"][-1:]))

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
# 2026-09-24 现场回归（robot0005275 B 分支"目标点无可点怪"死循环）：任务自带目标 npc 必须
# 进击杀名单 —— 真实 shenbu_nav.json 的 2028311 声明未带 kill_npc，配置的 18140|41|42
# （A 分支匪徒）顶不上，导致匪首 13597 实例永远不被认。
g10 = new_state()
g10.chain = {"task_order": [{"task_index": 2028311, "catcher_npc": 13597}]}
task10 = mk_task(2028311, npc_index=13597, npc_id=1923723)
idx10 = getattr(S, "__kill_npc_indexes")(fresh_robot(), g10, task10)
check("S10 ⑦任务自带目标 npc 进击杀名单（链数据无 kill_npc 仍含 13597）",
      13597 in idx10, str(idx10))
g10b = new_state()
g10b.chain = {"task_order": [{"task_index": 2028311, "kill_npc": [13597]}]}
idx10b = getattr(S, "__kill_npc_indexes")(fresh_robot(), g10b, task10)
check("S10 ⑧链数据带 kill_npc 时不重复", idx10b.count(13597) == 1, str(idx10b))
r10 = fresh_robot()
r10.m_mapid = 11
r10.m_pose = (1800, 1896)
q10 = r10.m_quest
q10.dyn_npc_meta[1923723] = 13597
q10.dynamic_npcs[1923723] = [11, 1792, 1888]
inst10 = getattr(S, "__find_clickable_instance")(r10, q10, [18140, 18141, 18142, 13597])
check("S10 ⑨同图已注册的匪首实例可被找到", bool(inst10) and inst10[1] == 13597, str(inst10))
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
check("S16 白名单含 zhuaogui + shenbu_nav + fenghuo_nav + share_daily_ 前缀",
      'startswith("share_daily_")' in src_qe and '_chain_id != "zhuaogui"' in src_qe
      and '"shenbu_nav"' in src_qe and '"fenghuo_nav"' in src_qe)
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
# 18) S21 · 包满 5 码（2026-09-29 2 池修复包：首次本地清理重试 → 再失败才停）
# ================================================================
for code in (360, 415, 418, 528, 1295):
    r = fresh_robot()
    g = new_state()
    r.m_share_daily = g
    S.on_notice(r, [code, "x"])
    g.state = "ACCEPT"
    g.accept_start_ms = (time.time() - 4.0) * 1000
    getattr(S, "__on_accept")(r, g, r.m_quest, time.time() * 1000)
    check("S21 码 %d 首次 → 本地清理一次不停止" % code,
          g.state != "STOPPED" and bool(getattr(g, "bag_cleanup_tried", False)),
          "%s/%s" % (g.state, getattr(g, "bag_cleanup_tried", None)))
    # 重试一轮后再次被判包满（新通知/新判定）→ 才停止并上报"需要清包"
    g.bag_full_ms = time.time() * 1000
    g.accept_start_ms = (time.time() - 4.0) * 1000
    getattr(S, "__on_accept")(r, g, r.m_quest, time.time() * 1000)
    check("S21 码 %d 再失败 → 停止(BAG_FULL, 需要清包)" % code,
          g.state == "STOPPED" and g.stop_code == "BAG_FULL"
          and "需要清包" in (g.stop_reason or ""),
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
check("S23 上报事件含 share_key（中控按 key 记账，防落回神捕；P1 联调契约）",
      ev.get("share_key") == "share_daily_大唐神捕", str(ev)[:120])
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
# 2026-09-24：会话内吃药接线 —— daily_ghost 有自己的疗伤，share_daily 此前没有（低血硬扛
# 定制战斗打输掉任务）；现在 share_daily.tick 应调用 auto_summon.heal_tick。
import auto_summon as _asm_stub
_hb = _asm_stub.CALL_LOG["heal_tick"]
r_h = fresh_robot()
g_h = new_state()
r_h.m_share_daily = g_h
g_h.state = "READY"
g_h.state_since_ms = time.time() * 1000
try:
    S.tick(r_h, time.time() * 1000)
except Exception:
    pass
check("S0 tick 集成: 启用后调用 auto_summon.heal_tick（会话内吃药）",
      _asm_stub.CALL_LOG["heal_tick"] > _hb, str(_asm_stub.CALL_LOG))
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
# 22) P1 烽火大唐（share_daily_宫廷10）—— 规划-20260924-烽火大唐P1 §3 P1-a~h
#     对照客户端 script/auto_task/*.py 与 分析-20260923-烽火大唐.md
# ================================================================
# ---- 测试用桩：发协议记录 + shop_errand 锁/结果 ----
_ROBOT_SENT = []


def _fake_send_message(self, msg_id, data):
    _ROBOT_SENT.append((msg_id, data))
    return True


FakeRobot.send_message = _fake_send_message
try:
    sys.modules["protocol3"].C2S_ITEM_RECYCLE = 80117
except Exception:
    pass
_se = types.ModuleType("shop_errand")
_se.LOCK = {"holder": "", "result": None, "reason": "", "skipped": False, "owner": ""}


def _se_acquire(ro, owner, item=None, count=None):
    if _se.LOCK["holder"] and _se.LOCK["holder"] != owner:
        return False, _se.LOCK["holder"]
    _se.LOCK["holder"] = owner
    _se.LOCK["owner"] = owner
    _se.LOCK["result"] = None
    _se.LOCK["reason"] = ""
    _se.LOCK["skipped"] = False
    return True, owner


def _se_release(ro, owner=None):
    if owner is not None and _se.LOCK["holder"] != owner:
        return
    _se.LOCK["holder"] = ""


def _se_status(ro):
    return {"active": _se.LOCK["result"] is None, "result": _se.LOCK["result"],
            "reason": _se.LOCK["reason"], "skipped": _se.LOCK["skipped"],
            "owner": _se.LOCK["owner"], "item": None, "count": None, "npc": None}


_se.acquire = _se_acquire
_se.release = _se_release
_se.status = _se_status
_se.holder = lambda ro: _se.LOCK["holder"]
sys.modules["shop_errand"] = _se


def new_state_fh(limit=20):
    """烽火大唐运行态（6 环链数据：5 子任务 + 收尾 2002107）。"""
    g = S.ShareDailyState()
    g.enabled = True
    g.share_key = "share_daily_宫廷10"
    g.daily_limit = limit
    g.chain = {"task_order": [
        {"task_index": 2002101, "thrower_npc": 10149, "catcher_npc": 30029},
        {"task_index": 2002102, "thrower_npc": 10149, "catcher_npc": 10149},
        {"task_index": 2002103, "thrower_npc": 10149, "catcher_npc": 10149},
        {"task_index": 2002104, "thrower_npc": 10149, "catcher_npc": 30030},
        {"task_index": 2002105, "thrower_npc": 10149, "catcher_npc": 10149},
        {"task_index": 2002107, "thrower_npc": 0, "catcher_npc": 10149},
    ]}
    return g


def mk_task_fh(ti, npc_index=10149, npc_id=10149, can_finish=0, counters=None, attrs=None):
    blob = marshal.dumps(attrs) if attrs else None
    return [ti, npc_index, npc_id, can_finish, 1, counters or [], blob]


# ---- F1 · 配置行（P1-h / S13） ----
_fh = "share_daily_宫廷10"
check("F1 配置含宫廷10 行", CFG.has_key(_fh))
check("F1 keywords = [烽火大唐, 回复秦琼]",
      CFG.get_keywords(_fh) == ["烽火大唐", "回复秦琼"], str(CFG.get_keywords(_fh)))
check("F1 chain_task = [2002107]（无 share_key 的收尾点名）",
      CFG.get_chain_task_list(_fh) == [2002107], str(CFG.get_chain_task_list(_fh)))
check("F1 chain 列为空（无 reply 链）", CFG.get_chain_keys(_fh) == [])
check("F1 shop 四家店前缀解析",
      CFG.shops.get(_fh) == [(13021, ["101", "108"]), (13007, ["210"]),
                             (13006, ["220"]), (13011, ["102"])],
      str(CFG.shops.get(_fh)))
check("F1 shop @keyword 兜底（仅 13021 配 @金币）",
      CFG.get_shop_keyword(_fh, 13021) == "金币" and CFG.get_shop_keyword(_fh, 13011) == "",
      "%s/%s" % (CFG.get_shop_keyword(_fh, 13021), CFG.get_shop_keyword(_fh, 13011)))
check("F1 patrol_map 四图（12/5/9/11）",
      CFG.get_patrol_maps(_fh) == [(12, 2464, 3019), (5, 1810, 922),
                                   (9, 899, 813), (11, 1100, 1100)],
      str(CFG.get_patrol_maps(_fh)))
check("F1 前缀路由: 101008/108086→13021",
      CFG.find_shop_npc(_fh, 101008) == 13021 and CFG.find_shop_npc(_fh, 108086) == 13021)
check("F1 前缀路由: 210102→13007(武器)/220102→13006(服装)/102031→13011(药品)",
      CFG.find_shop_npc(_fh, 210102) == 13007 and CFG.find_shop_npc(_fh, 220102) == 13006
      and CFG.find_shop_npc(_fh, 102031) == 13011)
check("F1 最长前缀：13020 覆盖不了 101xxx（101008 仍归 13021）",
      CFG.find_shop_npc(_fh, 101008) == 13021)
check("F1 accept_ticket 未配（12 列行，票靠链数据 thrower）",
      CFG.get_accept_ticket(_fh) == 0)
check("F1 is_auto_executable(宫廷10)", CFG.is_auto_executable(_fh))

# ---- F2 · 归属与次数（P1-g） ----
check("F2 归属: 2002107 经 chain_task 属本 run",
      S.belongs_to_run(CFG, _fh, 2002107, "", ""))
check("F2 归属: 2002105 经链任务号集合命中",
      S.belongs_to_run(CFG, _fh, 2002105, "", "", chain_tasks={2002101, 2002105}))
check("F2 归属: 无关日常(捉鬼) → False",
      not S.belongs_to_run(CFG, _fh, 2019501, "", "share_daily_捉鬼"))
_fh_chain = new_state_fh().chain
_fh_roots = getattr(S, "__main_count_roots")(_fh_chain, _fh)
check("F2 主键计数扫 5 个主 root（不含收尾 2002107）",
      _fh_roots == [2002101, 2002102, 2002103, 2002104, 2002105], str(_fh_roots))
check("F2 收尾 2002107 不入扫描集", 2002107 not in _fh_roots)
_fh_store = {"2002101": 7, "2002103": 7, "2002107": 99}
check("F2 五子任务共用容器：取最大 = 今日轮数(7)；2002107 不污染",
      S.scan_task_limited_max(_fh_store, _fh_roots) == 7)
check("F2 B 型漂移：容器在 2002105 → 仍取到 4",
      S.scan_task_limited_max({"2002105": 4}, _fh_roots) == 4)
_sb_roots2 = getattr(S, "__main_count_roots")(new_state().chain, "share_daily_大唐神捕")
check("F2 神捕回归：仍只扫主任务前两条（reply 每轮 +2 不入集）",
      _sb_roots2 == [2028301, 2028302], str(_sb_roots2))

# ---- F3 · 需求分型（P1-b） ----
_cfg = CFG


def plan_of(ti, counters, attrs=None, npc_index=10149, bag=0):
    task = mk_task_fh(ti, npc_index=npc_index, counters=counters, attrs=attrs)
    dem = None
    ds = S.parse_demand_counters(task)
    if ds:
        dem = ds[0]
    return S.classify_demand(task, dem, _cfg, _fh, bag_count=bag)


_p, _i = plan_of(2002101, [], attrs={12054: ((12, 100, 100),)})
check("F3 2002101 押送（无 demand）→ PLAN_FINISH", _p == "finish", _p)
_p, _i = plan_of(2002102, [counter(11883, 30860, 0, 1)], attrs={12053: (12, 2464, 3019)})
check("F3 2002102 暗雷（KILLING + 12053）→ PLAN_PATROL", _p == "patrol", _p)
check("F3 2002102 巡逻点解析 = 12053", tuple(_i.get("patrol_spot")) == (12, 2464, 3019),
      str(_i.get("patrol_spot")))
_p, _i = plan_of(2002103, [counter(11886, 101008, 0, 1)], attrs={12055: [101008]})
check("F3 2002103 军需品（COLLECTION 无坐标无库存）→ PLAN_BUY", _p == "buy", _p)
check("F3 2002103 item 取自 demand.object_index", _i.get("item") == 101008)
_p, _i = plan_of(2002103, [counter(11886, 101008, 1, 1)], attrs={12055: [101008]}, bag=1)
check("F3 2002103 背包已有 → PLAN_STOCK", _p == "stock", _p)
_p, _i = plan_of(2002104, [counter(11883, 30861, 0, 1)],
                 attrs={12054: ((5, 1882, 1306),)}, npc_index=30030)
check("F3 2002104 刺将军（KILLING + 12054）→ PLAN_KILL_CLICK", _p == "kill_click", _p)
_p, _i = plan_of(2002105, [counter(11886, 110185, 0, 3)], attrs={12053: (11, 1100, 1100)})
check("F3 2002105 军机情报（COLLECTION + 12053）→ PLAN_PATROL", _p == "patrol", _p)
check("F3 2002105 need = 3 - 0 = 3", _i.get("need") == 3, str(_i.get("need")))
_p, _i = plan_of(2002103, [counter(11886, 0, 0, 1)], attrs={12055: [102031]})
check("F3 12055 兜底：demand.object_index=0 时取 12055[0]", _i.get("item") == 102031,
      str(_i.get("item")))
# F3b 自备宽限（P1-c 配套）：背包已够但服务端未标可交 → 先等服务端推送，超 3s 去 catcher 催一次
_r = fresh_robot()
_g = new_state_fh()
_g.task_index = 2002103
_g.state = "READY"
_r.m_share_daily = _g
_q = _r.m_quest
_q.chain = _g.chain
_q.bag = {101008: [1, 1]}
_q.tasks[2002103] = mk_task_fh(2002103, counters=[counter(11886, 101008, 0, 1)],
                               attrs={12055: [101008]})
_t0 = time.time() * 1000
getattr(S, "__on_ready")(_r, _g, _q, _t0)
check("F3b 自备(未标可交) → 先等服务端（不空跑 NPC、不进 SHOP）",
      _g.state == "READY" and _g.stock_wait_ms > 0, "%s/%s" % (_g.state, _g.stock_wait_ms))
_g.stock_wait_ms = _t0 - (S.STOCK_WAIT_BEFORE_SUBMIT_MS + 1000)
_QUEST_ENGINE_STUB.CALL_LOG["teleport_click"] = []
getattr(S, "__on_ready")(_r, _g, _q, _t0)
check("F3b 自备超 3s → 去 catcher 催一次（SUBMIT）",
      _g.state == "SUBMIT" and bool(_QUEST_ENGINE_STUB.CALL_LOG["teleport_click"]),
      "%s/%s" % (_g.state, _QUEST_ENGINE_STUB.CALL_LOG["teleport_click"][:1]))

# ---- F4 · 接取严格选择（P1-a，报告 R7） ----
_opt2 = [("接受烽火大唐任务", 0), ("还是算了吧", 0)]
_pk, _kind = S.pick_accept_dialog_option(_opt2, ["烽火大唐", "回复秦琼"], False)
check("F4 两段票第二段 → 点'接受烽火大唐任务'(kind=accept)",
      _pk == 0 and _kind == "accept", "%s/%s" % (_pk, _kind))
_opt1 = [("#iBM#烽火大唐", 0), ("离开", 1)]
_pk, _kind = S.pick_accept_dialog_option(_opt1, ["烽火大唐", "回复秦琼"], False)
check("F4 第一段票列表 → 点票项(kind=ticket)", _pk == 0 and _kind == "ticket", "%s/%s" % (_pk, _kind))
_optmix = [("#iBM#烽火大唐", 0), ("#i901#回复秦琼", 0), ("还是算了吧", 0)]
_pk, _kind = S.pick_accept_dialog_option(_optmix, ["烽火大唐", "回复秦琼"], False)
check("F4 票/交付混排 + 手上无任务 → 点票项", _pk == 0 and _kind == "ticket", "%s/%s" % (_pk, _kind))
_pk, _kind = S.pick_accept_dialog_option(_optmix, ["烽火大唐", "回复秦琼"], True)
check("F4 票/交付混排 + 手上有任务 → 点交付项(不点票)",
      _pk == 1 and _kind == "deliver", "%s/%s" % (_pk, _kind))
check("F4 绝不点'还是算了吧'",
      S.pick_accept_dialog_option([("还是算了吧", 0)], ["烽火大唐"], False)[0] is None)
_pk, _kind = S.pick_accept_dialog_option([("还没有找出奸细么？", 0), ("知道了", 0)],
                                         ["烽火大唐", "回复秦琼"], True)
check("F4 催促对话无关键词命中 → 不点(None)", _pk is None, "%s/%s" % (_pk, _kind))
# 集成：STATE_ACCEPT 秦琼两段票走 __on_show_dialog
_r = fresh_robot()
_g = new_state_fh()
_g.state = "ACCEPT"
_g.accept_npc = 10149
_r.m_share_daily = _g
_r.m_quest.chain = _g.chain
_QUEST_ENGINE_STUB.CALL_LOG["schedule"] = []
_dlg1 = [0, 0, 10149, "我大唐建国不久……", 0, [("#iBM#烽火大唐", 0), ("离开", 1)]]
_ret1 = S.on_task_event(_r, "show_dialog", _dlg1)
_s1 = _QUEST_ENGINE_STUB.CALL_LOG["schedule"]
check("F4 集成: 第一段票 → 点票项 #0",
      _ret1 is True and _s1 and _s1[-1]["data"]["option_index"] == 0, str(_s1[-1:]))
_QUEST_ENGINE_STUB.CALL_LOG["schedule"] = []
_dlg2 = [0, 0, 10149, "此任务为单人任务……", 0, [("接受烽火大唐任务", 0), ("还是算了吧", 0)]]
S.on_task_event(_r, "show_dialog", _dlg2)
_s2 = _QUEST_ENGINE_STUB.CALL_LOG["schedule"]
check("F4 集成: 第二段 → 点'接受烽火大唐任务' #0",
      _s2 and _s2[-1]["data"]["option_index"] == 0, str(_s2[-1:]))
# 回归：神捕接取对话（"好的（接受大唐神捕任务）"）仍点对
_r2 = fresh_robot()
_g2 = new_state()
_g2.state = "ACCEPT"
_g2.accept_npc = 13297
_r2.m_share_daily = _g2
_r2.m_quest.chain = _g2.chain
_QUEST_ENGINE_STUB.CALL_LOG["schedule"] = []
S.on_task_event(_r2, "show_dialog",
                [0, 0, 13297, "最近长安周边匪患严重……", 0,
                 [("好的（接受大唐神捕任务）", 0), ("还是算了吧。", 0)]])
_s3 = _QUEST_ENGINE_STUB.CALL_LOG["schedule"]
check("F4 回归: 神捕接取对话仍点'好的（接受…）' #0",
      _s3 and _s3[-1]["data"]["option_index"] == 0, str(_s3[-1:]))
# 接取 NPC 判定（P1-a）：真实链数据各环只有 catcher_npc，首环 2002101 的 catcher 是
# 动态军需官 30029 —— 不得把它当 broker（否则点它开交付对话、接不到票）
_rn = fresh_robot()
_gn = new_state_fh()
check("F4 接取NPC: 链数据带 thrower_npc → 用首环 thrower(10149)",
      getattr(S, "__accept_npc_index")(_rn, _gn) == 10149,
      str(getattr(S, "__accept_npc_index")(_rn, _gn)))
_gn.chain = {"task_order": [
    {"task_index": 2002101, "catcher_npc": 30029},
    {"task_index": 2002102, "catcher_npc": 10149},
    {"task_index": 2002103, "catcher_npc": 10149},
    {"task_index": 2002104, "catcher_npc": 30030},
    {"task_index": 2002105, "catcher_npc": 10149},
    {"task_index": 2002107, "catcher_npc": 10149},
]}
_npc_mv = getattr(S, "__accept_npc_index")(_rn, _gn)
check("F4 接取NPC: 无 thrower_npc → 多数表决 10149（首环 30029 不误用）",
      _npc_mv == 10149, str(_npc_mv))
check("F4 接取NPC: 神捕回归 = 13297（13297×2 > 13597×1）",
      getattr(S, "__accept_npc_index")(fresh_robot(), new_state()) == 13297)
_nav_fh = os.path.normpath(os.path.join(HERE, "..", "data", "chains", "fenghuo_nav.json"))
if os.path.exists(_nav_fh):
    import json as _json_fh
    _navfh = _json_fh.load(open(_nav_fh, encoding="utf-8"))
    _g3 = new_state_fh()
    _g3.chain = _navfh
    _npc3 = getattr(S, "__accept_npc_index")(fresh_robot(), _g3)
    check("F4 链声明文件 fenghuo_nav.json → 接取NPC=10149 秦琼", _npc3 == 10149, str(_npc3))
    _tis_fh = set()
    for _e in (_navfh.get("task_order") or []):
        try:
            _tis_fh.add(int(_e.get("task_index") or 0))
        except Exception:
            pass
    check("F4 链声明文件 fenghuo_nav.json task_order 列全 6 个任务号",
          {2002101, 2002102, 2002103, 2002104, 2002105, 2002107} <= _tis_fh,
          str(sorted(_tis_fh)))
    check("F4 链声明 chain_id 与中控下发一致（fenghuo_nav 白名单放行）",
          _navfh.get("chain_id") == "fenghuo_nav", str(_navfh.get("chain_id")))
    # 联调契约：中控 chain 载荷 task_order 6 项（2002106 是服务端跳号，非缺环）→
    # 次数扫描必须得到 5 个主 root（排除 2002107），且不得假设任务号连续。
    _fh_roots_nav = getattr(S, "__main_count_roots")(_navfh, _fh)
    check("F4 真实链载荷 → 次数扫 5 主 root（2002106 跳号不影响、2002107 排除）",
          _fh_roots_nav == [2002101, 2002102, 2002103, 2002104, 2002105],
          str(_fh_roots_nav))
    check("F4 链载荷 6 项均可归属本 run（含 2002107 收尾）",
          all(S.belongs_to_run(CFG, _fh, _t, "", "",
                              chain_tasks=getattr(S, "__roots_from_chain")(_navfh))
              for _t in (2002101, 2002102, 2002103, 2002104, 2002105, 2002107)),
          str(sorted(getattr(S, "__roots_from_chain")(_navfh))))
else:
    check("F4 链声明文件 fenghuo_nav.json 存在", False, _nav_fh)

# ---- F5 · 商店对话货币选择（P1-c / S14） ----
_shop_opts = [("#iBM#储备金购买杂货", 0), ("#iG#金币购买杂货", 0), ("我什么都不想做", 1)]
check("F5 is_shop_dialog 命中", S.is_shop_dialog(_shop_opts))
check("F5 is_shop_dialog 不误判任务对话",
      not S.is_shop_dialog([("#i901#大唐神捕", 0), ("离开", 1)]))
_pk = S.pick_shop_dialog_option(_shop_opts, "金币", 0)
check("F5 phase0 → 储备金购买 #0", _pk == 0, str(_pk))
_pk = S.pick_shop_dialog_option(_shop_opts, "金币", 1)
check("F5 phase1 + @金币 → 金币购买 #1", _pk == 1, str(_pk))
_wq = [("#iBM#储备金购买武器", 0), ("#iG#金币购买武器", 0), ("学习采矿", 0)]
check("F5 13007 武器店 phase0 → 储备金 #0", S.pick_shop_dialog_option(_wq, "", 0) == 0)
_dr = [("#iBM#储备金购买药品", 0), ("#iG#金币购买药品", 0), ("学习炼丹", 0)]
check("F5 13011 药店 phase0 → 储备金 #0", S.pick_shop_dialog_option(_dr, "", 0) == 0)
check("F5 商店对话只有关闭项 → None（绝不点第一项）",
      S.pick_shop_dialog_option([("我什么都不想做", 1)], "金币", 0) is None)
# 集成：STATE_SHOP 的商店对话被本模块消化并选储备金
_r = fresh_robot()
_g = new_state_fh()
_g.state = "SHOP"
_g.shop_npc = 13021
_g.shop_phase = 0
_r.m_share_daily = _g
_QUEST_ENGINE_STUB.CALL_LOG["schedule"] = []
_ret = S.on_task_event(_r, "show_dialog",
                       [0, 0, 13021, "客官要点什么？", 0, [("#iBM#储备金购买杂货", 0),
                                                           ("#iG#金币购买杂货", 0)]])
_s = _QUEST_ENGINE_STUB.CALL_LOG["schedule"]
check("F5 集成: STATE_SHOP 商店对话 → 储备金项 #0",
      _ret is True and _s and _s[-1]["data"]["option_index"] == 0, str(_s[-1:]))
# 非 SHOP 状态不误吞（放行 quest_engine / 按任务对话处理）
_g.state = "READY"
_QUEST_ENGINE_STUB.CALL_LOG["schedule"] = []
S.on_task_event(_r, "show_dialog", [0, 0, 999999, "x", 0, [("#iBM#储备金购买杂货", 0)]])
check("F5 非 SHOP 状态商店对话不误吞", not _QUEST_ENGINE_STUB.CALL_LOG["schedule"])

# ---- F6 · 商店采购状态机（P1-c） ----
# (a) 未配置商店（神捕）→ BUY_FAILED（P0 行为保持）
_r = fresh_robot()
_g = new_state()
_r.m_share_daily = _g
_r.m_quest.chain = _g.chain
getattr(S, "__start_shop_path")(_r, _g, _r.m_quest, mk_task(2028301),
                                {"counter_type": 11887, "object_index": 102031,
                                 "current_count": 0, "required_count": 1}, time.time() * 1000)
check("F6 神捕无 shop 配置 → BUY_FAILED 停止（回归）",
      _g.state == "STOPPED" and _g.stop_code == "BUY_FAILED", "%s/%s" % (_g.state, _g.stop_code))
# (b) 无 item（demand/12055 均空）→ SHOP_FAILED
_r = fresh_robot()
_g = new_state_fh()
_r.m_share_daily = _g
_r.m_quest.chain = _g.chain
getattr(S, "__start_shop_path")(_r, _g, _r.m_quest, mk_task_fh(2002103, counters=[]),
                                {"counter_type": 11886, "object_index": 0,
                                 "current_count": 0, "required_count": 1}, time.time() * 1000)
check("F6 拿不到 item_index → SHOP_FAILED 停止",
      _g.state == "STOPPED" and _g.stop_code == "SHOP_FAILED", "%s/%s" % (_g.state, _g.stop_code))
# (c) 背包已足 → 不采购
_r = fresh_robot()
_g = new_state_fh()
_r.m_share_daily = _g
_r.m_quest.chain = _g.chain
_r.m_quest.bag = {102031: [1, 1]}
getattr(S, "__start_shop_path")(_r, _g, _r.m_quest, mk_task_fh(2002103),
                                {"counter_type": 11886, "object_index": 102031,
                                 "current_count": 0, "required_count": 1}, time.time() * 1000)
check("F6 背包已有 → 直接回 READY（不再买）", _g.state == "READY", _g.state)
# (d) 本轮已下过单 → 不重购（防二次扣款）
_r = fresh_robot()
_g = new_state_fh()
_g.task_index = 2002103
_g.shop_bought = {2002103: 102031}
_r.m_share_daily = _g
_r.m_quest.chain = _g.chain
getattr(S, "__start_shop_path")(_r, _g, _r.m_quest, mk_task_fh(2002103),
                                {"counter_type": 11886, "object_index": 102031,
                                 "current_count": 0, "required_count": 1}, time.time() * 1000)
check("F6 已下过单未入包 → SHOP_FAILED（不重购，客户端 has_bought_item 同口径）",
      _g.state == "STOPPED" and "不重购" in _g.stop_reason, "%s/%s" % (_g.state, _g.stop_reason))
# (e) 正常启动：锁占用 + 目标图导航
_se.LOCK.update({"holder": "", "result": None, "reason": "", "skipped": False, "owner": ""})
_r = fresh_robot()
_g = new_state_fh()
_r.m_share_daily = _g
_q = _r.m_quest
_q.chain = _g.chain
_q.tasks[2002103] = mk_task_fh(2002103, counters=[counter(11886, 102031, 0, 1)],
                               attrs={12055: [102031]})
_r.m_mapid = 12
_r.m_pose = (2265, 1846)
_QUEST_ENGINE_STUB.CALL_LOG["teleport_click"] = []
_QUEST_ENGINE_STUB.CALL_LOG["schedule"] = []
getattr(S, "__start_shop_path")(_r, _g, _q, mk_task_fh(2002103),
                                {"counter_type": 11886, "object_index": 102031,
                                 "current_count": 0, "required_count": 1}, time.time() * 1000)
check("F6 采购启动: state=SHOP + 路由到 13011（102xxx 药品）",
      _g.state == "SHOP" and _g.shop_npc == 13011, "%s/%s" % (_g.state, _g.shop_npc))
check("F6 采购启动: 已占商店会话锁(owner=share_daily)", _se.LOCK["holder"] == "share_daily",
      _se.LOCK["holder"])
check("F6 采购启动: 跨图导航已发起(未到店不点击)",
      bool(_q.dijkstra_route) and not _QUEST_ENGINE_STUB.CALL_LOG["teleport_click"],
      "%s/%s" % (len(_q.dijkstra_route), _QUEST_ENGINE_STUB.CALL_LOG["teleport_click"][:1]))
# 到店 → 点击 + 建立 shop_ctx（先建后点）
_r.m_mapid = 616
_r.m_pose = (848, 740)
_QUEST_ENGINE_STUB.CALL_LOG["teleport_click"] = []
_g.click_gap_ms = 0
getattr(S, "__on_shop")(_r, _g, _q, time.time() * 1000)
_ctx = _q.shop_ctx or {}
check("F6 到店: 点击 NPC + shop_ctx(item=102031 count=1 errand/share_daily)",
      bool(_QUEST_ENGINE_STUB.CALL_LOG["teleport_click"]) and
      _ctx.get("item_index") == 102031 and _ctx.get("count") == 1
      and _ctx.get("errand") and _ctx.get("share_daily"),
      "%s / %s" % (_QUEST_ENGINE_STUB.CALL_LOG["teleport_click"][:1], _ctx))
check("F6 shop_ctx.npc_id = 商店 NPC(13011)", _ctx.get("npc_id") == 13011, str(_ctx.get("npc_id")))
# 商店页未到 → 重开会话（不卡死）
_g.shop_opened_ms = time.time() * 1000 - (S.SHOP_OPEN_WAIT + 2) * 1000
_g.shop_wait_until = 0
getattr(S, "__on_shop")(_r, _g, _q, time.time() * 1000)
check("F6 商店页超时 → 清会话重开（shop_ctx 清空 + 点击次数复位）",
      _q.shop_ctx is None and _g.shop_click_tries == 0,
      "%s/%s" % (_q.shop_ctx, _g.shop_click_tries))
# 购买被拒（buy_fail + 退避）→ SHOP_FAILED（13011 无 @keyword）
_q.shop_ctx = {"npc_id": 13011, "item_index": 102031, "count": 1, "errand": True,
               "share_daily": True, "buy_fail": "货币(储备金/银票)不够",
               "buy_wait_until": 0}
getattr(S, "__on_shop")(_r, _g, _q, time.time() * 1000)
check("F6 购买被拒 → SHOP_FAILED + 停止（13011 无兜底）",
      _g.state == "STOPPED" and _g.stop_code == "SHOP_FAILED" and _q.shop_ctx is None,
      "%s/%s/%s" % (_g.state, _g.stop_code, _q.shop_ctx))
check("F6 失败后释放商店会话锁", _se.LOCK["holder"] == "", _se.LOCK["holder"])
# 13021 有 @keyword 但金币路径默认关闭 → 原因要说清（P2）
_r = fresh_robot()
_g = new_state_fh()
_g.state = "SHOP"
_g.shop_npc = 13021
_g.shop_item = 101008
_g.shop_need = 1
_r.m_share_daily = _g
getattr(S, "__shop_fail")(_r, _g, _r.m_quest, "余额不足")
check("F6 有 @keyword 且 P1 金币关闭 → 停止原因含'金币路径默认关闭'",
      _g.state == "STOPPED" and "金币路径默认关闭" in _g.stop_reason, _g.stop_reason)
check("F6 P1 开关 SHOP_GOLD_FALLBACK=False", S.SHOP_GOLD_FALLBACK is False)
# 购买成功（回执 done）→ 回 READY + 释放锁
_r = fresh_robot()
_g = new_state_fh()
_g.state = "SHOP"
_g.shop_item = 102031
_g.shop_need = 1
_g.shop_lock_held = True
_r.m_share_daily = _g
_q = _r.m_quest
_q.chain = _g.chain
_q.tasks[2002103] = mk_task_fh(2002103)
_q.shop_ctx = {"npc_id": 13011, "item_index": 102031, "count": 1, "errand": True, "share_daily": True}
_se.LOCK.update({"holder": "share_daily", "owner": "share_daily", "result": "done",
                 "reason": "买到(数量回执90327)", "skipped": False})
getattr(S, "__on_shop")(_r, _g, _q, time.time() * 1000)
check("F6 购买成功(回执) → 回 READY + 清 shop_ctx + 放锁",
      _g.state == "READY" and _q.shop_ctx is None and _se.LOCK["holder"] == "",
      "%s/%s/%s" % (_g.state, _q.shop_ctx, _se.LOCK["holder"]))
# 回执'已满足但未购买' + 背包不足 → 失败（防死循环）
_r = fresh_robot()
_g = new_state_fh()
_g.state = "SHOP"
_g.shop_item = 102031
_g.shop_need = 1
_g.shop_lock_held = True
_r.m_share_daily = _g
_q = _r.m_quest
_q.chain = _g.chain
_q.tasks[2002103] = mk_task_fh(2002103)
_q.shop_ctx = {"npc_id": 13011, "item_index": 102031, "count": 1, "errand": True, "share_daily": True}
_se.LOCK.update({"holder": "share_daily", "owner": "share_daily", "result": "done",
                 "reason": "已满足, 未购买", "skipped": True})
getattr(S, "__on_shop")(_r, _g, _q, time.time() * 1000)
check("F6 回执 skipped 但背包不足 → SHOP_FAILED（不空转）",
      _g.state == "STOPPED" and _g.stop_code == "SHOP_FAILED", "%s/%s" % (_g.state, _g.stop_code))
# 锁被他人占用 → 不点 NPC；超时 → SHOP_BUSY
_r = fresh_robot()
_g = new_state_fh()
_g.state = "SHOP"
_g.shop_npc = 13011
_g.shop_item = 102031
_g.shop_need = 1
_g.shop_started_ms = time.time() * 1000
_r.m_share_daily = _g
_q = _r.m_quest
_q.chain = _g.chain
_q.tasks[2002103] = mk_task_fh(2002103)
_r.m_mapid = 616
_r.m_pose = (848, 740)
_se.LOCK.update({"holder": "errand", "owner": "errand", "result": None,
                 "reason": "", "skipped": False})
_QUEST_ENGINE_STUB.CALL_LOG["teleport_click"] = []
getattr(S, "__on_shop")(_r, _g, _q, time.time() * 1000)
check("F6 锁被占用 → 不点 NPC、不建 shop_ctx（让路不硬闯）",
      _g.state == "SHOP" and not _QUEST_ENGINE_STUB.CALL_LOG["teleport_click"]
      and _q.shop_ctx is None, "%s/%s" % (_g.state, _q.shop_ctx))
_g.shop_started_ms = time.time() * 1000 - (S.SHOP_LOCK_WAIT_MAX + 10) * 1000
getattr(S, "__on_shop")(_r, _g, _q, time.time() * 1000)
check("F6 等锁超时 → SHOP_BUSY 停止（记录原因）",
      _g.state == "STOPPED" and _g.stop_code == "SHOP_BUSY", "%s/%s" % (_g.state, _g.stop_code))
_se.LOCK.update({"holder": "", "owner": "", "result": None, "reason": "", "skipped": False})

# ---- F7 · 巡逻打计数（P1-e） ----
check("F7 12053 在 patrol_map 内 → 原样采用",
      getattr(S, "__validated_patrol_spot")(fresh_robot(), new_state_fh(), (5, 1810, 922))
      == (5, 1810, 922))
_r = fresh_robot()
_g = new_state_fh()
check("F7 12053 不在 patrol_map → 回退列表首图（12 长安城中）",
      getattr(S, "__validated_patrol_spot")(_r, _g, (26, 100, 100)) == (12, 2464, 3019),
      str(getattr(S, "__validated_patrol_spot")(_r, _g, (26, 100, 100))))
_r = fresh_robot()
_g = new_state()          # 神捕：无 patrol_map
check("F7 神捕（无 patrol_map）→ 原样返回（不回归）",
      getattr(S, "__validated_patrol_spot")(_r, _g, (12, 3392, 1184)) == (12, 3392, 1184))
# 暗雷任务：12053 → PATROL（集成）
_r = fresh_robot()
_g = new_state_fh()
_r.m_share_daily = _g
_q = _r.m_quest
_q.chain = _g.chain
q2002102 = mk_task_fh(2002102, counters=[counter(11883, 30860, 0, 1)],
                      attrs={12053: (9, 899, 813)})
_q.tasks[2002102] = q2002102
getattr(S, "__on_ready")(_r, _g, _q, time.time() * 1000)
check("F7 2002102 暗雷 → 进 PATROL + 巡逻中心 = 服务端指定图(9)",
      _g.state == "PATROL" and _g.patrol_center == (9, 899, 813),
      "%s/%s" % (_g.state, _g.patrol_center))
# 军机情报（COLLECTION×3）：计数未满 → PATROL；达 3 → 回 READY 等交付
_r = fresh_robot()
_g = new_state_fh()
_r.m_share_daily = _g
_q = _r.m_quest
_q.chain = _g.chain
_q.tasks[2002105] = mk_task_fh(2002105, counters=[counter(11886, 110185, 2, 3)],
                              attrs={12053: (11, 1100, 1100)})
getattr(S, "__on_ready")(_r, _g, _q, time.time() * 1000)
check("F7 2002105 军机情报(2/3) → PATROL 继续打", _g.state == "PATROL", _g.state)
_q.tasks[2002105] = mk_task_fh(2002105, counters=[counter(11886, 110185, 3, 3)],
                              attrs={12053: (11, 1100, 1100)})
_g.task_index = 2002105
_g.state = "PATROL"
_r.m_mapid = 11
_r.m_pose = (1100, 1100)
getattr(S, "__on_patrol")(_r, _g, _q, time.time() * 1000)
check("F7 2002105 计数达标(3/3) → 停巡逻等交付（不再游走）",
      _g.state in ("WAIT", "READY"), _g.state)

# ---- F8 · 押送/动态 NPC（P1-f） ----
# 同图实例在点击范围内 → 交回调用方点击
_r = fresh_robot()
_g = new_state_fh()
_g.state = "SUBMIT"
_g.task_index = 2002101
_r.m_share_daily = _g
_q = _r.m_quest
_q.chain = _g.chain
_q.tasks[2002101] = mk_task_fh(2002101, npc_index=30029, npc_id=30029,
                              attrs={12054: ((12, 100, 100),)})
_q.dyn_npc_meta = {9001: 30029}
_q.dynamic_npcs = {9001: [12, 200, 200]}
_r.m_mapid = 12
_r.m_pose = (300, 200)
_ok = getattr(S, "__goto_dynamic_catcher")(_r, _g, _q, 30029, time.time() * 1000)
check("F8 同图实例在范围内 → 交回点击路径(False) + 记录 target_npc_id",
      _ok is False and _g.target_npc_id == 9001, "%s/%s" % (_ok, _g.target_npc_id))
# 同图实例远 → 走过去（walk）
_QUEST_ENGINE_STUB.CALL_LOG["schedule"] = []
_q.dynamic_npcs = {9001: [12, 1900, 1900]}
_ok = getattr(S, "__goto_dynamic_catcher")(_r, _g, _q, 30029, time.time() * 1000)
check("F8 同图实例较远 → 实时坐标走过去（walk 到 dyn_npc 坐标）",
      _ok is True and _QUEST_ENGINE_STUB.CALL_LOG["schedule"] and
      _QUEST_ENGINE_STUB.CALL_LOG["schedule"][-1]["type"] == "walk",
      str(_QUEST_ENGINE_STUB.CALL_LOG["schedule"][-1:]))
# 无实例 + 12054 异图 → 跨图导航
_q.dyn_npc_meta = {}
_q.dynamic_npcs = {}
_q.tried_locations = set()
_q.tasks[2002101] = mk_task_fh(2002101, npc_index=30029, npc_id=30029,
                              attrs={12054: ((5, 100, 100),)})
_r.m_mapid = 12
_QUEST_ENGINE_STUB.CALL_LOG["schedule"] = []
_ok = getattr(S, "__goto_dynamic_catcher")(_r, _g, _q, 30029, time.time() * 1000)
check("F8 无实例 + 12054 异图 → 发起跨图导航", _ok is True and bool(_q.dijkstra_route),
      "%s/%s" % (_ok, len(_q.dijkstra_route)))
# 到点无实例 → 换点计数 / 超限退避
_q.dijkstra_route = []
_q.tasks[2002101] = mk_task_fh(2002101, npc_index=30029, npc_id=30029,
                              attrs={12054: ((12, 2265, 1846),)})
_r.m_mapid = 12
_r.m_pose = (2265, 1846)
_g.tried_locations = set()
_g.kill_tries = 0
_ok = getattr(S, "__goto_dynamic_catcher")(_r, _g, _q, 30029, time.time() * 1000)
check("F8 到点无实例 → 标记该点已去过并计数",
      _ok is True and (12, 2265, 1846) in _g.tried_locations and _g.kill_tries == 1,
      "%s/%s" % (_g.tried_locations, _g.kill_tries))
_g.kill_tries = 99
_g.respawn_until_ms = 0
getattr(S, "__goto_dynamic_catcher")(_r, _g, _q, 30029, time.time() * 1000)
check("F8 到点多次无实例 → 退避等 AOI 推送（不缓存旧坐标）",
      _g.respawn_until_ms > 0, str(_g.respawn_until_ms))
# 交付点击：用实例 npc_id（服务端按实例归属校验）
_r = fresh_robot()
_g = new_state_fh()
_g.state = "SUBMIT"
_g.task_index = 2002101
_r.m_share_daily = _g
_q = _r.m_quest
_q.chain = _g.chain
_q.tasks[2002101] = mk_task_fh(2002101, npc_index=30029, npc_id=30029,
                              attrs={12054: ((12, 200, 200),)})
_q.dyn_npc_meta = {9001: 30029}
_q.dynamic_npcs = {9001: [12, 200, 200]}
_r.m_mapid = 12
_r.m_pose = (250, 200)
_g.click_gap_ms = 0
_g.finish_try = 0
_QUEST_ENGINE_STUB.CALL_LOG["teleport_click"] = []
getattr(S, "__on_submit")(_r, _g, _q, time.time() * 1000)
_cl = _QUEST_ENGINE_STUB.CALL_LOG["teleport_click"]
check("F8 押送交付: 用动态实例 npc_id 点击（非 npc_index 盲点）",
      bool(_cl) and _cl[0][0] == 9001 and _cl[0][1] == 30029, str(_cl[:2]))
check("F8 押送交付: 实例对话归属 target_npc_id=9001", _g.target_npc_id == 9001,
      str(_g.target_npc_id))
# __on_kill 每 tick 刷新 12054（动态 NPC 会走动）
_r = fresh_robot()
_g = new_state_fh()
_g.state = "KILL"
_g.task_index = 2002104
_g.target_locs = [(12, 1, 1)]
_r.m_share_daily = _g
_q = _r.m_quest
_q.chain = _g.chain
_q.tasks[2002104] = mk_task_fh(2002104, npc_index=30030, npc_id=30030,
                              counters=[counter(11883, 30861, 0, 1)],
                              attrs={12054: ((5, 1882, 1306),)})
_r.m_mapid = 12
_r.m_pose = (100, 100)
_QUEST_ENGINE_STUB.CALL_LOG["schedule"] = []
getattr(S, "__on_kill")(_r, _g, _q, time.time() * 1000)
check("F8 __on_kill 每 tick 重读 12054（旧点被新点替换，目标会走动）",
      _g.target_locs == [(5, 1882, 1306)], str(_g.target_locs))
# F8b 目标图不可达（报告 R：动态目标可能刷在 26/27 等图）：记录 + 换点；连续 3 次停链
_r = fresh_robot()
_g = new_state_fh()
_g.state = "SUBMIT"
_g.task_index = 2002101
_r.m_share_daily = _g
_q = _r.m_quest
_q.chain = _g.chain
_q.tasks[2002101] = mk_task_fh(2002101, npc_index=30029, npc_id=30029,
                              attrs={12054: ((26, 100, 100),)})
_r.m_mapid = 12
_orig_route = _QUEST_ENGINE_STUB.__find_dijkstra_route
_QUEST_ENGINE_STUB.__find_dijkstra_route = lambda q, fm, tm, **kw: None
for _i in range(3):
    _g.tried_locations = set()
    _q.dijkstra_route = []
    getattr(S, "__goto_dynamic_catcher")(_r, _g, _q, 30029, time.time() * 1000)
    if _g.state == "STOPPED":
        break
_QUEST_ENGINE_STUB.__find_dijkstra_route = _orig_route
check("F8b 目标图连续 3 次无合法路径 → TARGET_UNREACHABLE 停链（不无限换路）",
      _g.state == "STOPPED" and _g.stop_code == "TARGET_UNREACHABLE",
      "%s/%s/%s" % (_g.state, _g.stop_code, _g.unreach_maps))
check("F8b 不可达图计数已记录(26→3)", int(_g.unreach_maps.get(26, 0)) >= 1,
      str(_g.unreach_maps))

# ---- F9 · 回收窗口（押送银两 / 军需品） ----
def recycle_case(ti=2002101, bag=None, server_list=None, want=101306, submits=0, wait_ms=0):
    _r = fresh_robot()
    _g = new_state_fh()
    _r.m_share_daily = _g
    _q = _r.m_quest
    _q.chain = _g.chain
    _q.tasks[ti] = mk_task_fh(ti, npc_index=30029, npc_id=30029,
                              counters=[counter(11886, want, 0, 1)],
                              attrs={12055: [want]})
    _q.bag = dict(bag or {})
    _g.recycle_submits = submits
    _g.recycle_submit_ms = wait_ms
    _g.recycle_task = ti
    _ROBOT_SENT[:] = []
    _ret = S.on_task_event(_r, "item_recycle",
                           [30029, "给予", "给我押送的银两", marshal.dumps(server_list or [])])
    return _r, _g, _q, _ret


_r, _g, _q, _ret = recycle_case(bag={101306: [555, 1]}, server_list=[101306])
check("F9 回收窗口: 发出 C2S_ITEM_RECYCLE(item_id=555) + 消化事件(True)",
      _ret is True and _ROBOT_SENT and _ROBOT_SENT[-1][1] == [555], str(_ROBOT_SENT[-1:]))
# want 优先：服务端列表顺序不同
_r, _g, _q, _ret = recycle_case(bag={101306: [555, 1], 999999: [666, 1]},
                                server_list=[999999, 101306])
check("F9 优先交'当前步骤需要'的物品（12055/需求优先于服务端列表顺序）",
      _ROBOT_SENT and _ROBOT_SENT[-1][1] == [555], str(_ROBOT_SENT[-1:]))
# 背包没有 → 不提交、记冷却、不删任务（绝不 @deltask）
_r, _g, _q, _ret = recycle_case(bag={}, server_list=[101306])
check("F9 背包无可交道具 → 不提交、不删任务、记冷却等待",
      _ret is True and not _ROBOT_SENT and _g.recycle_submit_ms > 0 and 2002101 in _q.tasks,
      "%s/%s" % (_ROBOT_SENT, _g.recycle_submit_ms))
# 提交上限熔断
_r, _g, _q, _ret = recycle_case(bag={101306: [555, 1]}, server_list=[101306], submits=3)
check("F9 回收提交 > 3 次未确认 → RECYCLE_STUCK 停止（S23⑥）",
      _g.state == "STOPPED" and _g.stop_code == "RECYCLE_STUCK", "%s/%s" % (_g.state, _g.stop_code))
# 冷却期内不重复提交
_r, _g, _q, _ret = recycle_case(bag={101306: [555, 1]}, server_list=[101306],
                                wait_ms=time.time() * 1000)
check("F9 提交后 5s 冷却内不重复提交", not _ROBOT_SENT, str(_ROBOT_SENT))
# 无本 run 在身任务 → 不消化（放行 quest_engine）
_r = fresh_robot()
_g = new_state_fh()
_r.m_share_daily = _g
_RET = S.on_task_event(_r, "item_recycle", [30029, "给予", "x", marshal.dumps([101306])])
check("F9 无本 run 任务 → 回收事件放行（不抢别的链）", _RET is False, str(_RET))
# 任务实例切换 → 计数复位
_g2 = new_state_fh()
_g2.recycle_task = 2002105
_g2.recycle_submits = 2
_r2 = fresh_robot()
_r2.m_share_daily = _g2
S.on_task_event(_r2, "add_task", [[2002103, 10149, 10149, 0, 1, [], None]])
check("F9 新任务实例到达 → 回收计数/下痕迹复位",
      _g2.recycle_submits == 0 and _g2.recycle_task == 0,
      "%s/%s" % (_g2.recycle_submits, _g2.recycle_task))
# 停止/人工停止都必须归还商店会话锁（否则 TTL 10min 挡住日常采购）
_se.LOCK.update({"holder": "share_daily", "owner": "share_daily", "result": None,
                 "reason": "", "skipped": False})
_r = fresh_robot()
_g = new_state_fh()
_r.m_share_daily = _g
getattr(S, "__request_stop")(_r, _g, "DAILY_LIMIT", "x")
check("F9 任何停止 → 释放商店会话锁", _se.LOCK["holder"] == "", _se.LOCK["holder"])
_se.LOCK.update({"holder": "share_daily", "owner": "share_daily", "result": None,
                 "reason": "", "skipped": False})
_r = fresh_robot()
_g = new_state_fh()
_r.m_share_daily = _g
S.dispatch_cmd(_r, {"cmd": "share_daily_stop"})
check("F9 人工停止 → 释放商店会话锁", _se.LOCK["holder"] == "", _se.LOCK["holder"])

# ---- F10 · 集成与常量 ----
_r = fresh_robot()
_g = new_state_fh()
_g.state = "SHOP"
_g.shop_item = 102031
_g.shop_need = 1
_g.shop_lock_held = True
_r.m_share_daily = _g
_q = _r.m_quest
_q.chain = _g.chain
_q.tasks[2002103] = mk_task_fh(2002103)
_q.shop_ctx = {"npc_id": 13011, "item_index": 102031, "count": 1, "errand": True, "share_daily": True}
_se.LOCK.update({"holder": "share_daily", "owner": "share_daily", "result": None,
                 "reason": "", "skipped": False})
_err = None
try:
    S.tick(_r, time.time() * 1000)
except Exception as e:
    _err = "%s: %s" % (type(e).__name__, e)
check("F10 tick 集成: STATE_SHOP 不抛异常", _err is None, _err or "")
check("F10 STATE_SHOP 常量与 tick 分派存在", S.STATE_SHOP == "SHOP")
_src_p1 = open(os.path.join(SCRIPT_DIR, "share_daily.py"), encoding="utf-8").read()
check("F10 tick 分派含 STATE_SHOP", "elif g.state == STATE_SHOP:" in _src_p1)
check("F10 商店会话锁复用 shop_errand.acquire/release",
      "shop_errand.acquire(robot_object, SHOP_LOCK_OWNER" in _src_p1
      and "shop_errand.release(robot_object, SHOP_LOCK_OWNER)" in _src_p1)
check("F10 零运行期协议注册（本模块不碰 cnet.set_format_dict）",
      "set_format_dict" not in _src_p1)
check("F10 回收窗口自实现（不转发 quest_engine.__on_item_recycle / 无 @deltask 兜底调用）",
      "def __on_item_recycle" in _src_p1
      and "quest_engine.__on_item_recycle" not in _src_p1
      and "@deltask %s" not in _src_p1)
check("F10 12055 已在树内消费（P1-d）", "parse_task_item_list" in _src_p1)
check("F10 动态 NPC 坐标表含 13007/13006/13021/13011",
      set(S.SHOP_NPC_POSITIONS.keys()) >= {13006, 13007, 13011, 13021},
      str(sorted(S.SHOP_NPC_POSITIONS.keys())))

# ================================================================
# 23) F11 · 2026-09-24 现场修复（神捕慢：点位判废 / 导航零进展复位）
#     —— robot0005274/5275/5278 死点位空转 30-66 次；robot0005256 静默 11 分钟
# ================================================================
_src_f11 = _src_p1   # 同文件（share_daily.py）源码，复用 F10 的读取
# A① 判废窗口内跳过 12054 点位路径 → 就地巡逻兜底可达（修复前该兜底永不触发）
_rA = fresh_robot()
_gA = new_state()
_rA.m_share_daily = _gA
_qA = _rA.m_quest
_qA.tasks[2028302] = mk_task(2028302, counters=[counter(11883, 18140, 0, 1)])
_gA.state = "KILL"
_gA.target_locs = [(12, 100, 100)]
_gA.tried_locations = set()
_gA.kill_tries = 0
_gA.tp_abandon_task = 2028302                            # 同一任务实例（不触发换任务重置）
_gA.tp_abandon_until_ms = time.time() * 1000 + 300000    # 判废窗口打开
_rA.m_mapid = 12
_rA.m_pose = (100, 100)
_QUEST_ENGINE_STUB.CALL_LOG["schedule"] = []
getattr(S, "__on_kill")(_rA, _gA, _qA, time.time() * 1000)
check("F11 A① 判废窗口内跳过点位路径 → 就地巡逻兜底（state=PATROL）",
      _gA.state == "PATROL" and _gA.kill_mode == "patrol",
      "%s/%s" % (_gA.state, _gA.kill_mode))
# A② 连续 3 次"点位耗尽退避" → 判本轮点位不可用（窗口打开）
_rA = fresh_robot()
_gA = new_state()
_rA.m_share_daily = _gA
_qA = _rA.m_quest
_qA.tasks[2028302] = mk_task(2028302, counters=[counter(11883, 18140, 0, 1)])
_gA.state = "KILL"
_gA.target_locs = [(12, 100, 100)]
_rA.m_mapid = 12
_rA.m_pose = (100, 100)
for _i in range(3):
    _gA.respawn_until_ms = 0
    _gA.kill_tries = 99
    getattr(S, "__on_kill")(_rA, _gA, _qA, time.time() * 1000)
check("F11 A② 连续 3 次退避 → 判本轮点位不可用（窗口打开）",
      _gA.tp_abandon_until_ms > time.time() * 1000, str(_gA.tp_abandon_until_ms))
check("F11 A②b 判废后计数归零（进入下一累计周期）", _gA.tp_retry_count == 0,
      str(_gA.tp_retry_count))
check("F11 A③ 源码: ② 进入条件含判废窗口（防回归）",
      "if g.target_locs and not (g.tp_abandon_until_ms and now_ms < g.tp_abandon_until_ms)" in _src_f11)
check("F11 A④ 源码: 退避路径累计计数 + 判废常量",
      "TP_ABANDON_RETRY_LIMIT" in _src_f11 and "TP_ABANDON_WINDOW_MS" in _src_f11)
# B① nav_busy（walk_pending_click 形态）+ 位置零变化 ≥45s → 清理全形态残留回 READY
_rB = fresh_robot()
_gB = new_state()
_rB.m_share_daily = _gB
_qB = _rB.m_quest
_gB.state = "KILL"
_qB.walk_pending_click = {"at_ms": 1}      # 旧实现漏网的形态（只认 dijkstra_route）
_rB.m_mapid = 12
_rB.m_pose = (100, 100)
_gB.nav_probe = (12, 100, 100)
_gB.nav_probe_ms = time.time() * 1000 - 46000
_okB = getattr(S, "__check_watchdog")(_rB, _gB, _qB, time.time() * 1000)
check("F11 B① nav_busy 零进展 ≥45s → 清残留 + 回 READY（5256 现场形态）",
      _okB is True and _qB.walk_pending_click is None and _gB.state == "READY",
      "%s/%s/%s" % (_okB, _qB.walk_pending_click, _gB.state))
# B② 位置有变化（有进展）→ 不误伤
_rB = fresh_robot()
_gB = new_state()
_rB.m_share_daily = _gB
_qB = _rB.m_quest
_gB.state = "KILL"
_qB.walk_pending_click = {"at_ms": 1}
_rB.m_mapid = 12
_rB.m_pose = (200, 200)                    # 与 nav_probe 不同 = 有进展
_gB.nav_probe = (12, 100, 100)
_gB.nav_probe_ms = time.time() * 1000 - 46000
_okB = getattr(S, "__check_watchdog")(_rB, _gB, _qB, time.time() * 1000)
check("F11 B② 位置有变化 → 不误伤（保留导航字段）",
      _okB is False and _qB.walk_pending_click is not None, str(_okB))
# B③ 对话中豁免（对话有独立的 30s 未关清理）
_rB = fresh_robot()
_gB = new_state()
_rB.m_share_daily = _gB
_qB = _rB.m_quest
_gB.state = "KILL"
_qB.walk_pending_click = {"at_ms": 1}
_qB.dialog_open = True
_rB.m_mapid = 12
_rB.m_pose = (100, 100)
_gB.nav_probe = (12, 100, 100)
_gB.nav_probe_ms = time.time() * 1000 - 60000
_okB = getattr(S, "__check_watchdog")(_rB, _gB, _qB, time.time() * 1000)
check("F11 B③ 对话中豁免零进展清理", _okB is False and _qB.walk_pending_click is not None,
      str(_okB))
# B④ 源码级：__clear_nav_fields 覆盖全形态 + 时间语义常量
check("F11 B④ 源码: __clear_nav_fields 全形态 + NAV_STALL_MS",
      "def __clear_nav_fields" in _src_f11 and "NAV_STALL_MS" in _src_f11
      and "walk_pending_click" in _src_f11)
# C① 开战窗口内不再点下一只（5256 形态：2s 内连点两只不同怪 → 双开战 → 战斗卡死）
_rC = fresh_robot()
_gC = new_state()
_rC.m_share_daily = _gC
_qC = _rC.m_quest
_qC.tasks[2028302] = mk_task(2028302, counters=[counter(11883, 18140, 0, 1)])
_gC.state = "KILL"
_qC.dyn_npc_meta = {9001: 18140}          # 本图有可点实例（若无窗口会立刻点击）
_qC.dynamic_npcs = {9001: [12, 100, 100]}
_rC.m_mapid = 12
_rC.m_pose = (100, 100)
_gC.kill_click_sent_ms = time.time() * 1000 - 1000    # 1s 前刚发过开战点击（窗口内）
_QUEST_ENGINE_STUB.CALL_LOG["teleport_click"] = []
getattr(S, "__on_kill")(_rC, _gC, _qC, time.time() * 1000)
check("F11 C① 开战窗口内不点下一只（防双开战）",
      _QUEST_ENGINE_STUB.CALL_LOG["teleport_click"] == [],
      str(_QUEST_ENGINE_STUB.CALL_LOG["teleport_click"]))
# C② 窗口超时 → 清标记放行重试（服务端未回/被拒场景）
_gC.kill_click_sent_ms = time.time() * 1000 - 4000    # 超 3.5s 窗口
_QUEST_ENGINE_STUB.CALL_LOG["teleport_click"] = []
getattr(S, "__on_kill")(_rC, _gC, _qC, time.time() * 1000)
check("F11 C② 窗口超时 → 清标记放行重试",
      _gC.kill_click_sent_ms > 0 and len(_QUEST_ENGINE_STUB.CALL_LOG["teleport_click"]) > 0,
      "sent=%s clicks=%s" % (_gC.kill_click_sent_ms, _QUEST_ENGINE_STUB.CALL_LOG["teleport_click"][:1]))
# C③ __click_kill 发点即记窗口
_rC2 = fresh_robot()
_gC2 = new_state()
_rC2.m_share_daily = _gC2
_qC2 = _rC2.m_quest
_gC2.click_gap_ms = 0
_QUEST_ENGINE_STUB.CALL_LOG["teleport_click"] = []
getattr(S, "__click_kill")(_rC2, _gC2, _qC2, 9001, 18140, time.time() * 1000)
check("F11 C③ 开战点击即记窗口时刻",
      _gC2.kill_click_sent_ms > 0, str(_gC2.kill_click_sent_ms))
# C④ 源码级：常量 + 战斗结束复位窗口
check("F11 C④ 源码: KILL_START_GRACE_MS + 战斗结束复位窗口",
      "KILL_START_GRACE_MS" in _src_f11 and "g.kill_click_sent_ms = 0" in _src_f11)
# C⑤ 热更兼容：旧实例（缺新字段）经 __check_watchdog 首帧补齐（client.py 热更约定）
_rC3 = fresh_robot()
_gC3 = new_state()
_rC3.m_share_daily = _gC3
_qC3 = _rC3.m_quest
_f3list = ("nav_probe_ms", "tp_retry_count", "tp_abandon_task", "tp_abandon_until_ms",
           "kill_click_sent_ms")
for _f3 in _f3list:
    try:
        delattr(_gC3, _f3)
    except Exception:
        pass
getattr(S, "__check_watchdog")(_rC3, _gC3, _qC3, time.time() * 1000)
check("F11 C⑤ 热更兼容：旧实例缺新字段 → 首帧补齐",
      all(hasattr(_gC3, _f3) for _f3 in _f3list),
      str([_f3 for _f3 in _f3list if not hasattr(_gC3, _f3)]))

# ================================================================
# 24) F12 · 修（2026-09-24）：战斗时长补回走路 ETA（防怪区"走路超时"误判）
#     现场 robot0005102 图 20（100% 怪区）：每场战斗结束即触发假超时 → 反复重寻路
# ================================================================
# F12① 行为：注入 nav_pause_ms（模拟 60s 前开战）→ 走"战斗结束"分支 → ETA 被补回
_rW = fresh_robot()
_gW = new_state()
_rW.m_share_daily = _gW
_qW = _rW.m_quest
_base_w = int(time.time() * 1000) + 10000
_qW.walk_target = (12, 3000, 2000)
_qW.walk_end_ms = _base_w
_rW.m_fight_state = True
S.tick(_rW, time.time() * 1000)                       # 首帧进战斗（记 nav_pause_ms）
_rW.m_fight_state = False
_gW.nav_pause_ms = int(time.time() * 1000) - 60000    # 假装 60s 前开战
S.tick(_rW, time.time() * 1000)                       # 战斗结束 → 补回
check("F12① 战斗时长补回走路 ETA（防怪区假超时）",
      _qW.walk_end_ms >= _base_w + 59000, "Δ=%s" % (_qW.walk_end_ms - _base_w))
# F12② 源码：记点/补回/热更补齐三件套（防回归）
_src_f12 = open(os.path.join(SCRIPT_DIR, "share_daily.py"), encoding="utf-8").read()
check("F12② 源码: 战斗记 nav_pause_ms + 结束补回 + 热更补齐列表",
      "g.nav_pause_ms = now_ms" in _src_f12
      and "quest.walk_end_ms += (now_ms - g.nav_pause_ms)" in _src_f12
      and '("nav_pause_ms", 0)' in _src_f12)

# ================================================================
# 25) 汇总
# ================================================================
print("\n自检目标: %s" % SCRIPT_DIR)
print("结果：%d 项，失败 %d 项" % (total, fails))
sys.exit(1 if fails else 0)
