# -*- coding: utf-8 -*-
"""抓鬼前「领双」前置 + 畅享服务卡使用时机 自检(2026-09-22, pre_daily.py)。

背景(现场口径与已核实事实):
- 用户要求: 抓鬼前每天一次去图12 点薛仁贵 13035 领多倍经验(「直接领取（双倍）」→
  「领取一小时双倍」), 2.5 倍路径"可用才优先"; 失败/异常绝不阻塞抓鬼。
- 畅享服务卡新口径: "尽量也要在领双的时候使用; 新手的时候先不用, 留着抓鬼时用"。

本脚本为**源码级 + 动态执行**自检:
  - 静态断言: 四个集成点(daily_ghost ghost_start/tick/on_show_dialog/on_close_dialog)、
    auto_summon.use_buff_card 的新手门槛、config 两个新配置项存在;
  - 动态断言: 直接加载 pre_daily.py(模块级只 import time, 不依赖机器人运行时) +
    注入 fake config/quest_engine/quest_state/auto_summon/diag/ctrl_client,
    跑真实纯函数与真实 tick/on_dialog/on_close_dialog 流程:
      ①按文本选选项(命中/未命中/多个候选/2.5 优先/按小时数) ②"今日已领"跨日重置
      ③额度为 0 短路 ④新手不用卡判据(含反例) ⑤失败不阻塞(异常路径返回而非抛出)。

用法: python pre_daily_selftest.py [pre_daily.py 路径]
      不带参数默认校验仓库副本; 传线上副本路径可再验一次线上文件。
"""
import hashlib
import os
import sys
import types

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_PRE = os.path.normpath(os.path.join(
    HERE, "..", "deploy", "zones", "prod-240-2300", "script", "pre_daily.py"))

path = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_PRE
SCRIPT_DIR = os.path.dirname(os.path.abspath(path))
src = open(path, encoding="utf-8").read()
fails = 0


def check(name, ok, detail=""):
    global fails
    fails += 0 if ok else 1
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  " + detail) if detail else ""))


# ---------------------------------------------------------------- fake 依赖注入
class FakeConfig(object):
    robot_claim_double_exp = True
    robot_claim_double_exp_hours = 1
    robot_claim_double_exp_prefer_25 = False
    robot_claim_double_exp_use_card = True
    robot_claim_double_exp_npc = 13035
    robot_claim_double_exp_map_pos = [12, 3931, 882]
    # 2026-09-22 看门狗可配后, 本自检显式钉住 120 秒(与生产默认 5 分钟解耦,
    # 用例里的时间步进都是按 120 秒设计的)
    robot_claim_double_exp_timeout_ms = 120000
    robot_use_buff_items = [190016]
    robot_use_buff_items_min_level = 31
    robot_use_buff_items_idle = False


class FakeQuestState(object):
    ST_DIALOG = "DIALOG"
    ST_IDLE = "IDLE"


class FakeQuest(object):
    def __init__(self):
        self.pending = None
        self.walk_target = None
        self.walk_end_ms = 0
        self.walk_pending_hop = None
        self.walk_pending_click = None
        self.dijkstra_route = []
        self.dijkstra_final = None
        self.dijkstra_waiting = False
        self.dijkstra_jumper = None
        self.dialog = None
        self.dialog_open = False
        self.state = ""
        self.chain = {"npcs": {}, "dijkstra": {}}

    def set_state(self, s):
        self.state = s


QUEST = FakeQuest()


class FakeQE(object):
    """quest_engine 的最小替身(只覆盖 pre_daily 用到的接口)。"""
    def __init__(self):
        self.scheduled = []
        self.teleport_calls = []
        self.route_ret = None	# __find_dijkstra_route 返回值(默认 None=无路径)


FakeQE.get_quest = lambda self, ro, create=False, _q=QUEST: _q
FakeQE.__schedule = lambda self, quest, p: (self.scheduled.append(p), setattr(quest, "pending", p))
FakeQE.__human_delay = lambda self, ms: 0


def _qe_teleport(self, ro, quest, npc_id, npc_index, click_type, delay, **kw):
    self.teleport_calls.append((npc_id, npc_index, click_type, delay))
    quest.click_dbg = {"npc_id": npc_id}
    quest.pending = {"type": "walk", "data": {"npc_id": npc_id}}


FakeQE.__teleport_click = _qe_teleport
FakeQE.__find_dijkstra_route = lambda self, quest, a, b, **kw: kw.get("_fake_ret", None)


class FakeAS(object):
    """auto_summon 最小替身: 记录 use_buff_card / _try_title_restore 调用。"""
    def __init__(self):
        self.card_calls = []
        self.title_calls = []


FakeAS.get_state = lambda self, ro: getattr(ro, "m_auto_summon", None)


def _as_use_buff_card(self, ro, st, now_ms, reason=""):
    self.card_calls.append(reason)
    return True


FakeAS.use_buff_card = _as_use_buff_card
FakeAS._try_title_restore = lambda self, ro, st, now_ms: self.title_calls.append(now_ms)

FAKE_CONFIG = FakeConfig()
FAKE_QE = FakeQE()
FAKE_AS = FakeAS()
FAKE_DIAG_LOGS = []


class FakeDiag(object):
    def log(self, msg):
        FAKE_DIAG_LOGS.append(msg)


sys.modules["config"] = FAKE_CONFIG
sys.modules["quest_engine"] = FAKE_QE
sys.modules["quest_state"] = FakeQuestState()
sys.modules["auto_summon"] = FAKE_AS
sys.modules["diag"] = FakeDiag()


class FakeCtrl(object):
    def __init__(self):
        self.events = []

    def emit(self, ev):
        self.events.append(ev)


FAKE_CTRL = FakeCtrl()
sys.modules["ctrl_client"] = FAKE_CTRL

import importlib.util
_spec = importlib.util.spec_from_file_location("pre_daily_under_test", path)
pre_daily = importlib.util.module_from_spec(_spec)
sys.modules["pre_daily_under_test"] = pre_daily
_spec.loader.exec_module(pre_daily)


class FakeRobot(object):
    def __init__(self, **kw):
        self.m_logined = True
        self.m_account = ["acc1@xy3.com"]
        self.m_level = 45
        self.m_mapid = 12
        self.m_auto_summon = {}
        self.sent = []
        for k, v in kw.items():
            setattr(self, k, v)

    def send_message(self, mid, data):
        self.sent.append((mid, data))
        return 0


# 实测形态(仓库 diag.log:285): 薛仁贵引导对话 npc_id=0, 只给「直接领取（双倍）」
DIALOG_ENTRY = [0, 0, 0, "player1,薛仁贵:13035:1&@2凡有心效力朝廷者都可以在此领取雇佣时间，"
                "得到双倍奖励……你本周剩余的双倍时间为#c06FAE616#cffffff小时。", 0,
                [("直接领取（双倍）", 0), ("我什么都不想做", 1)]]
DIALOG_STEP12 = [0, 0, 0, "玩家在不开启畅游服务的情况下可使用双倍时间。双倍和畅游服务的2.5倍是共享每周总奖励多倍时长的。", 0,
                 [("领取一小时双倍", 0), ("领取两小时双倍", 0), ("领取四小时双倍", 0), ("返回", 0)]]
DIALOG_STEP11 = [0, 0, 0, "请选择你要领取的2.5倍经验时数。领取2.5倍时间会额外打开畅游服务。", 0,
                 [("领取一小时2.5倍", 0), ("领取两小时2.5倍", 0), ("领取四小时2.5倍", 0), ("返回", 0)]]
DIALOG_ENTRY_25 = [0, 0, 0, "……领取雇佣时间……", 0,
                   [("领取并开启畅游服务（2.5倍）", 0), ("直接领取（双倍）", 0), ("我什么都不想做", 1)]]
DIALOG_LIMIT = [0, 0, 0, "你本周的多倍经验时间已经领完，请下周再来领取。", 0, [("离开", 1)]]
BROKER_MENU = [("捉鬼【组队完成】", 0), ("活动说明", 0), ("领取助战令", 0), ("离开", 1)]
BROKER_MENU2 = [("活动说明", 0), ("领取助战令", 0), ("放弃捉鬼任务", 0), ("离开", 1)]

# ---------------------------------------------------------------- 静态断言
check("S1 接口/常量: PRE_DAILY_ATTR / NPC 13035 / 图12(3931,882) / 超时常量",
      pre_daily.PRE_DAILY_ATTR == "m_pre_daily"
      and pre_daily.DEFAULT_NPC_ID == 13035
      and pre_daily.DEFAULT_NPC_POS == [12, 3931, 882]
      and pre_daily.TOTAL_TIMEOUT_MS >= 60000
      and all(hasattr(pre_daily, f) for f in (
          "claim_double_exp", "is_active", "tick", "on_dialog", "on_close_dialog",
          "pick_claim_option", "is_claim_dialog", "is_limit_notice",
          "pick_close_option", "is_newbie_level", "should_claim", "is_nav_busy")))

# 集成点(源码级): daily_ghost 四处 + auto_summon 门槛 + config 两个新项
_dg_path = os.path.join(SCRIPT_DIR, "daily_ghost.py")
_as_path = os.path.join(SCRIPT_DIR, "auto_summon.py")
_cfg_path = os.path.join(SCRIPT_DIR, "config.py")
_dg = open(_dg_path, encoding="utf-8").read() if os.path.exists(_dg_path) else ""
_as = open(_as_path, encoding="utf-8").read() if os.path.exists(_as_path) else ""
_cfg = open(_cfg_path, encoding="utf-8").read() if os.path.exists(_cfg_path) else ""

check("S2 daily_ghost 四处集成点(ghost_start/tick/on_show_dialog/on_close_dialog)",
      _dg.count("import pre_daily") == 4
      and "pre_daily.claim_double_exp(robot_object)" in _dg
      and "if not pre_daily.tick(robot_object, now_ms):" in _dg
      and "pre_daily.on_dialog(robot_object, datalist)" in _dg
      and "pre_daily.on_close_dialog(robot_object)" in _dg,
      "import 数=%d" % _dg.count("import pre_daily"))

check("S3 auto_summon: use_buff_card 新手门槛 + _try_bag_cleanup 空闲用卡开关",
      "def use_buff_card(" in _as
      and "robot_use_buff_items_min_level" in _as
      and 'getattr(_cfg, "robot_use_buff_items_idle", False)' in _as
      and "use_buff_card(robot_object, st, now_ms, reason=\"背包整理: \")" in _as)

check("S4 config 新增两个用卡门槛项 + 领双配置段",
      "robot_use_buff_items_min_level = 31" in _cfg
      and "robot_use_buff_items_idle = False" in _cfg
      and "robot_claim_double_exp = True" in _cfg
      and "robot_claim_double_exp_hours = 1" in _cfg
      and "robot_claim_double_exp_prefer_25 = False" in _cfg)

# ---------------------------------------------------------------- 动态断言
# T1 入口层: 实测只给双倍 → 选它; 关闭项跳过
idx, layer = pre_daily.pick_claim_option(DIALOG_ENTRY[5], 1, False)
check("T1 按文本选选项(入口层): 「直接领取（双倍）」命中/跳过关闭项",
      (idx, layer) == (0, "enter_double"), "idx=%s layer=%s" % (idx, layer))

# T2 2.5 优先: allow_25=True 选 2.5; False 选双倍(两个候选都命中)
idx25, l25 = pre_daily.pick_claim_option(DIALOG_ENTRY_25[5], 1, True)
idxd, ld = pre_daily.pick_claim_option(DIALOG_ENTRY_25[5], 1, False)
check("T2 多个候选: 2.5 优先(allow_25=True→#0) / 关掉则退回双倍(#1)",
      (idx25, l25) == (0, "enter_25") and (idxd, ld) == (1, "enter_double"),
      "25=(%s,%s) double=(%s,%s)" % (idx25, l25, idxd, ld))

# T3 中间层按小时数: 1/2/4 各自命中; 2.5 层同理
i1, L1 = pre_daily.pick_claim_option(DIALOG_STEP12[5], 1, False)
i2, L2 = pre_daily.pick_claim_option(DIALOG_STEP12[5], 2, False)
i4, L4 = pre_daily.pick_claim_option(DIALOG_STEP12[5], 4, False)
i25, L25 = pre_daily.pick_claim_option(DIALOG_STEP11[5], 1, True)
check("T3 中间层按小时数: 1→#0 / 2→#1 / 4→#2; 2.5 层「领取一小时2.5倍」",
      (i1, L1) == (0, "hour_double") and (i2, L2) == (1, "hour_double")
      and (i4, L4) == (2, "hour_double") and (i25, L25) == (0, "hour_25"),
      "1=%s 2=%s 4=%s 2.5=%s" % ((i1, L1), (i2, L2), (i4, L4), (i25, L25)))

# T4 未命中: 钟馗菜单/无关对话 → (None, None)(不误点)
n1 = pre_daily.pick_claim_option(BROKER_MENU, 1, False)
n2 = pre_daily.pick_claim_option([("离开", 1)], 1, True)
check("T4 未命中: 钟馗菜单/只有关闭项 → (None,None) 不误点",
      n1 == (None, None) and n2 == (None, None), "n1=%s n2=%s" % (n1, n2))

# T5 领双对话识别(命中) + 反例(钟馗菜单/药店不命中); 额度不足提示识别
ok_hit = pre_daily.is_claim_dialog(DIALOG_ENTRY[3], DIALOG_ENTRY[5]) \
    and pre_daily.is_claim_dialog(DIALOG_STEP12[3], DIALOG_STEP12[5]) \
    and pre_daily.is_claim_dialog(DIALOG_ENTRY_25[3], DIALOG_ENTRY_25[5])
ok_miss = (not pre_daily.is_claim_dialog("捉鬼任务…", BROKER_MENU)) \
    and (not pre_daily.is_claim_dialog("购买药品", [("储备金购买", 0), ("离开", 1)])) \
    and (not pre_daily.is_claim_dialog("正常对话", BROKER_MENU2))
ok_limit = pre_daily.is_limit_notice(DIALOG_LIMIT[3]) \
    and pre_daily.is_limit_notice("你本周所剩余的多倍时间已经不足两小时。") \
    and (not pre_daily.is_limit_notice("捉鬼任务"))
check("T5 对话识别: 领双对话命中 / 钟馗·药店反例不命中 / 额度不足提示命中", ok_hit and ok_miss and ok_limit,
      "hit=%s miss=%s limit=%s" % (ok_hit, ok_miss, ok_limit))

# T6 关闭项选择: close_flag 优先; 文本兜底; 无 → None
c1 = pre_daily.pick_close_option(DIALOG_ENTRY[5])
c2 = pre_daily.pick_close_option([("返回", 0), ("离开", 0)])
c3 = pre_daily.pick_close_option([("直接领取（双倍）", 0)])
check("T6 关闭项: close_flag=1 优先(#1) / 文本兜底(#1) / 无 → None",
      c1 == 1 and c2 == 1 and c3 is None, "c1=%s c2=%s c3=%s" % (c1, c2, c3))

# T7 跨日重置: done_date=今天 → 不启动; 昨天 → 启动; 配置关闭 → 不启动
today = pre_daily._today_str()
s_today = {"done_date": today}
s_old = {"done_date": "2000-01-01"}
check("T7 今日已领不重复 / 跨日自动重算 / 配置关闭短路",
      pre_daily.should_claim(s_today, today, True) == (False, "今日已处理")
      and pre_daily.should_claim(s_old, today, True) == (True, "")
      and pre_daily.should_claim({}, today, False)[0] is False)

# T8 新手不用卡判据(含反例): 30<31=新手; 31>=31=非新手; 等级未同步(0)=新手; 门槛关=都可用
check("T8 新手判据: 30→新手 / 31→非新手(反例) / 0(未同步)→新手 / 门槛0→不拦",
      pre_daily.is_newbie_level(30, 31) is True
      and pre_daily.is_newbie_level(31, 31) is False
      and pre_daily.is_newbie_level(0, 31) is True
      and pre_daily.is_newbie_level(1, 0) is False
      and pre_daily.is_newbie_level(45, 31) is False)

# T9 失败不阻塞(异常路径返回而非抛出)
ok_nothrow = True
r1 = r2 = r3 = r4 = r5 = r6 = None
try:
    r1 = pre_daily.claim_double_exp(None)
    r2 = pre_daily.tick(None)
    r3 = pre_daily.on_dialog(FakeRobot(), None)
    r4 = pre_daily.on_close_dialog(None)
    r5 = pre_daily.pick_claim_option(None)
    r6 = pre_daily.is_claim_dialog(None, None)
    ok_nothrow = (r1 is False and r2 is True and r3 is False and r4 is False
                  and r5 == (None, None) and r6 is False)
except Exception as e:
    ok_nothrow = False
    print("      异常泄漏: %r" % (e,))
check("T9 异常路径不抛: claim(None)=False / tick(None)=True / on_dialog(None)=False / on_close(None)=False",
      ok_nothrow, "r1=%s r2=%s r3=%s r4=%s r5=%s r6=%s" % (r1, r2, r3, r4, r5, r6))

# T10 全链路(动态): claim → 用卡 → 导航 → 等对话 → 点入口 → 点一小时 → 关对话=完成 →
#     同日不重复 → 跨日可重来
del FAKE_QE.scheduled[:]
del FAKE_QE.teleport_calls[:]
del FAKE_AS.card_calls[:]
QUEST.__init__()
ro = FakeRobot()
st = pre_daily._get_state(ro)
step = []
step.append(("claim", pre_daily.claim_double_exp(ro), st.get("active"), st.get("phase")))
step.append(("tick_card", pre_daily.tick(ro), st.get("phase"), len(FAKE_AS.card_calls)))
step.append(("tick_nav", pre_daily.tick(ro), st.get("phase"), len(FAKE_QE.teleport_calls)))
step.append(("tick_nav2", pre_daily.tick(ro), st.get("phase"), 0))
QUEST.pending = None	# 模拟导航链路走完(走到位并点了 NPC)
step.append(("tick_wait", pre_daily.tick(ro), st.get("phase"), st.get("tries")))
d1 = pre_daily.on_dialog(ro, DIALOG_ENTRY)
click1 = QUEST.pending["data"]["option_index"] if (QUEST.pending or {}).get("type") == "dialog_click" else None
QUEST.pending = None
d2 = pre_daily.on_dialog(ro, DIALOG_STEP12)
click2 = QUEST.pending["data"]["option_index"] if (QUEST.pending or {}).get("type") == "dialog_click" else None
clicked_flag = st.get("claim_clicked")
QUEST.pending = None
d3 = pre_daily.on_close_dialog(ro)
after = (st.get("active"), st.get("done_date"), st.get("last_ok_date"))
again = pre_daily.claim_double_exp(ro)
st["done_date"] = "2000-01-01"
cross_day = pre_daily.claim_double_exp(ro)
ok = (step[0] == ("claim", True, True, "card")
      and step[1] == ("tick_card", False, "nav", 1)
      and step[2] == ("tick_nav", False, "nav", 1)
      and step[3] == ("tick_nav2", False, "nav", 0)
      and step[4] == ("tick_wait", False, "wait_dialog", 0)
      and d1 is True and click1 == 0		# 入口层: 直接领取（双倍）
      and d2 is True and click2 == 0		# 中间层: 领取一小时双倍
      and clicked_flag is True
      and d3 is True and after[0] is False and after[1] == today and after[2] == today
      and again is False and cross_day is True)
check("T10 全链路: 用卡→导航→等对话→点双倍→点一小时→关对话完成; 同日不再领/跨日可重来", ok,
      "steps=%s clicks=(%s,%s) after=%s again=%s cross=%s" % (
          step, click1, click2, after, again, cross_day))

# T11 额度为 0 短路: 提示对话 → 记 info + 今日不再尝试(不刷屏) + 点关闭项收尾
QUEST.__init__()
ro2 = FakeRobot()
st2 = pre_daily._get_state(ro2)
pre_daily.claim_double_exp(ro2)
st2["phase"] = "dialog"
del FAKE_DIAG_LOGS[:]
ret = pre_daily.on_dialog(ro2, DIALOG_LIMIT)
close_idx = QUEST.pending["data"]["option_index"] if (QUEST.pending or {}).get("type") == "dialog_click" else None
again2 = pre_daily.claim_double_exp(ro2)
ok_lim = (ret is True and st2.get("limited") is True and st2.get("active") is False
          and st2.get("done_date") == today and close_idx == 0
          and again2 is False
          and any("额度不足" in m for m in FAKE_DIAG_LOGS))
check("T11 额度为0短路: 记 info/标记今日不再尝试/点关闭项收尾/同日不再领", ok_lim,
      "ret=%s limited=%s close=%s again=%s log=%s" % (
          ret, st2.get("limited"), close_idx, again2, FAKE_DIAG_LOGS[-1:]))

# T12 文本缺失(文案变化): 记 warn + 放弃 + 点关闭项(不阻塞)
QUEST.__init__()
ro3 = FakeRobot()
st3 = pre_daily._get_state(ro3)
pre_daily.claim_double_exp(ro3)
st3["phase"] = "dialog"
WEIRD = [0, 0, 0, "……领取雇佣时间……", 0, [("冻结多倍时间（需花费1金）", 0), ("我什么都不想做", 1)]]
ret3 = pre_daily.on_dialog(ro3, WEIRD)
close_idx3 = QUEST.pending["data"]["option_index"] if (QUEST.pending or {}).get("type") == "dialog_click" else None
check("T12 文案变化: 无可点领取项 → 放弃(不阻塞)+点关闭项+记日志",
      ret3 is True and st3.get("active") is False and close_idx3 == 1,
      "ret=%s active=%s close=%s" % (ret3, st3.get("active"), close_idx3))

# T13 导航失败不阻塞: 无跨图路径 → 放弃并记 warn(不写 quest 错误/不抛)
QUEST.__init__()
ro4 = FakeRobot(m_mapid=24)	# 当前图 24, 目标 12 → 走跨图预检
st4 = pre_daily._get_state(ro4)
pre_daily.claim_double_exp(ro4)
FAKE_QE.route_ret = None
del FAKE_QE.teleport_calls[:]
del FAKE_DIAG_LOGS[:]
t1 = pre_daily.tick(ro4)		# card → nav
t2 = pre_daily.tick(ro4)		# nav: 预检失败 → 放弃
ok_nav = (t1 is False and t2 is True and st4.get("active") is False
          and not FAKE_QE.teleport_calls
          and any("无跨图路径" in m for m in FAKE_DIAG_LOGS))
check("T13 导航失败(无跨图路径)不阻塞: 放弃+不调 __teleport_click+记 warn", ok_nav,
      "t1=%s t2=%s teleport=%s active=%s" % (t1, t2, FAKE_QE.teleport_calls, st4.get("active")))

# T14 整体超时看门狗: 到点放弃(返回 True), 不卡抓鬼
QUEST.__init__()
ro5 = FakeRobot()
st5 = pre_daily._get_state(ro5)
pre_daily.claim_double_exp(ro5)
st5["start_ms"] = pre_daily._now_ms() - pre_daily.TOTAL_TIMEOUT_MS - 1
t5 = pre_daily.tick(ro5)
check("T14 整体超时看门狗: 到点放弃并返回 True", t5 is True and st5.get("active") is False,
      "t=%s active=%s" % (t5, st5.get("active")))

# T15 重复 ghost_start(重置→重发)不重启: 不重复用卡; 超时残留才允许推翻
QUEST.__init__()
ro6 = FakeRobot()
st6 = pre_daily._get_state(ro6)
del FAKE_AS.card_calls[:]
c1 = pre_daily.claim_double_exp(ro6)
pre_daily.tick(ro6)			# card 阶段: 用卡一次, phase → nav
c2 = pre_daily.claim_double_exp(ro6)	# 重复下发: 上一轮进行中 → 不重置
pre_daily.tick(ro6)			# 不应再走 card 阶段(不再用卡)
st6["start_ms"] = pre_daily._now_ms() - pre_daily.TOTAL_TIMEOUT_MS * 2 - 1
c3 = pre_daily.claim_double_exp(ro6)	# 超时残留 → 允许重启
check("T15 重复 ghost_start 不重启(不重复用卡) / 超时残留可重启",
      c1 is True and c2 is True and len(FAKE_AS.card_calls) == 1
      and c3 is True and st6.get("phase") == "card",
      "c1=%s c2=%s cards=%d c3=%s phase=%s" % (
          c1, c2, len(FAKE_AS.card_calls), c3, st6.get("phase")))

# ---------------------------------------------------------------- 结果
print("\n自检目标: %s" % path)
print("sha1=%s" % hashlib.sha1(src.encode("utf-8")).hexdigest()[:12])
total = 4 + 15
print("结果：%d 项，失败 %d 项" % (total, fails))
sys.exit(1 if fails else 0)
