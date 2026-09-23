# -*- coding: utf-8 -*-
"""领双前置「跨重启持久化」自检 (2026-09-23, pre_daily.py)。

用户现场口径:
  "关于领双, 不要一直领取, 一天领一次就好了" —— 机器人进程重启 11+ 次, 内存态
  m_pre_daily.done_date 每次归零 → 每轮抓鬼启动又跑去图12 领一次(双倍有时长+每周总量
  限制, 重复领取纯浪费)。修复 = 关键状态(done_date/last_ok_date)按账号落盘, 原子写,
  读失败/损坏按空处理。

本脚本用**两个独立模块实例**(模拟两次进程启动, 共享同一状态目录)验证:
  T1 首次领取(内存态) → T2 新实例/新 Robot 对象 → 不再启动(跨重启不重复领)
  T3 语义不变(should_claim 今日已处理 → False)     T4 跨日自动重算(旧日期 → 可领)
  T5 文件损坏/结构不对不崩(按空处理)               T6 原子写(写后可解析, 无 .tmp 残留)
  T7 失败也记 done_date(当天不再尝试) 但**不上报已领**  T8 无账号不写文件且不崩
  T9 状态目录不存在时自动创建                      T10 状态文件是目录(打开失败)不崩
  T11 心跳字段格式 = YYYYMMDD(中控 todayKey 口径)   T12 多账号互不串扰

用法: python tools/pre_daily_persist_selftest.py [pre_daily.py 路径]
      不带参数默认校验仓库副本(deploy/zones/prod-240-2300/script/pre_daily.py)。
状态文件全部写在临时目录(ZCC_PRE_DAILY_STATE_DIR), **不碰生产脚本目录**。
"""
import hashlib
import importlib.util
import json
import os
import shutil
import sys
import tempfile
import time

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_PRE = os.path.normpath(os.path.join(
    HERE, "..", "deploy", "zones", "prod-240-2300", "script", "pre_daily.py"))

path = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_PRE
src = open(path, encoding="utf-8").read()
fails = 0
total = 0

# 状态目录 = 临时目录(必须在加载被测模块之前设置: pre_daily 只在**调用时**读它,
# 但早设一步更保险, 也保证后续任何路径都不会落到生产脚本目录)
STATE_DIR = tempfile.mkdtemp(prefix="zcc_pre_daily_")
os.environ["ZCC_PRE_DAILY_STATE_DIR"] = STATE_DIR


def check(name, ok, detail=""):
    global fails, total
    total += 1
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
    robot_claim_double_exp_timeout_ms = 120000


class FakeQuestState(object):
    ST_DIALOG = "DIALOG"


class FakeQuest(object):
    def __init__(self):
        self.pending = None
        self.walk_target = None
        self.dijkstra_route = []
        self.dijkstra_final = None
        self.dijkstra_waiting = False
        self.dialog = None
        self.dialog_open = False
        self.chain = {"npcs": {}, "dijkstra": {}}

    def set_state(self, s):
        pass


QUEST = FakeQuest()


class FakeQE(object):
    def __init__(self):
        self.teleport_calls = []


FakeQE.get_quest = lambda self, ro, create=False: QUEST
FakeQE.__schedule = lambda self, quest, p: setattr(quest, "pending", p)
FakeQE.__teleport_click = lambda self, ro, quest, npc_id, idx, ct, delay, **kw: self.teleport_calls.append(npc_id)
FakeQE.__find_dijkstra_route = lambda self, quest, a, b, **kw: None


class FakeAS(object):
    def __init__(self):
        self.card_calls = []


FakeAS.get_state = lambda self, ro: getattr(ro, "m_auto_summon", None)
FakeAS.use_buff_card = lambda self, ro, st, now_ms, reason="": True
FakeAS._try_title_restore = lambda self, ro, st, now_ms: None


class FakeDiag(object):
    def log(self, msg):
        pass


class FakeCtrl(object):
    def emit(self, ev):
        pass


sys.modules["config"] = FakeConfig()
sys.modules["quest_state"] = FakeQuestState()
sys.modules["auto_summon"] = FakeAS()
sys.modules["diag"] = FakeDiag()
sys.modules["ctrl_client"] = FakeCtrl()


def load_instance(tag):
    """加载一个**独立的模块实例**(模拟一次进程启动: 模块级状态全新, 只共享状态文件)。"""
    qe = FakeQE()
    sys.modules["quest_engine"] = qe
    spec = importlib.util.spec_from_file_location("pre_daily_" + tag, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod, qe


class FakeRobot(object):
    def __init__(self, acc="persist_a@xy3.com", **kw):
        self.m_logined = True
        self.m_account = [acc]
        self.m_level = 45
        self.m_mapid = 12
        self.m_auto_summon = {}
        for k, v in kw.items():
            setattr(self, k, v)


def state_file(acc):
    return os.path.join(STATE_DIR, "pre_daily_%s.json" % acc)


def read_state_file(acc):
    with open(state_file(acc), encoding="utf-8") as f:
        return json.load(f)


def write_raw(acc, raw):
    with open(state_file(acc), "w", encoding="utf-8") as f:
        f.write(raw)


ACC = "persist_a@xy3.com"

# ---------------------------------------------------------------- 静态断言
check("S1 源码含持久化要素(状态目录/读写/原子替换/坏文件兜底)",
      "STATE_DIR_ENV" in src and "_load_persist" in src and "_save_persist" in src
      and "os.replace" in src and "json.dump" in src and "json.load" in src
      and "def double_claim_date(" in src,
      "len=%d" % len(src))

pre_a, _qe_a = load_instance("a")
check("S2 模块接口: 持久化函数 + 心跳字段函数都在",
      all(hasattr(pre_a, f) for f in (
          "_state_dir", "_state_path", "_load_persist", "_save_persist",
          "double_claim_date", "should_claim", "_get_state", "_finish")))

# ---------------------------------------------------------------- T1 首次领取 + 落盘
QUEST.__init__()
ro1 = FakeRobot(ACC)
st1 = pre_a._get_state(ro1)
t1_claim = pre_a.claim_double_exp(ro1)
st1["phase"] = "dialog"
st1["claim_clicked"] = True
t1_close = pre_a.on_close_dialog(ro1)		# 真实成功收尾路径 → _finish(ok=True) → 落盘
today = pre_a._today_str()
compact_today = today.replace("-", "")
try:
    disk1 = read_state_file(ACC)
except Exception as e:
    disk1 = {}
    print("      读状态文件失败: %r" % (e,))
check("T1 成功领取 → 状态落盘(done_date/last_ok_date=今天)",
      t1_claim is True and t1_close is True
      and disk1.get("done_date") == today and disk1.get("last_ok_date") == today,
      "claim=%s close=%s disk=%s" % (t1_claim, t1_close, disk1))

# ---------------------------------------------------------------- T2 跨重启不重复领
pre_b, _qe_b = load_instance("b")		# 新"进程"
ro2 = FakeRobot(ACC)				# 新 Robot 对象(内存态全新)
st2 = pre_b._get_state(ro2)
t2_mem = (st2.get("done_date"), st2.get("last_ok_date"))
t2_claim = pre_b.claim_double_exp(ro2)
t2_hb = pre_b.double_claim_date(ro2)
check("T2 新进程/新 Robot: 内存态从盘回填(done_date=今天) → 不再启动领双",
      t2_mem == (today, today) and t2_claim is False
      and t2_hb == compact_today,
      "mem=%s claim=%s hb=%s" % (t2_mem, t2_claim, t2_hb))

# ---------------------------------------------------------------- T3 语义不变
check("T3 should_claim 语义不变: 今日已处理 → (False,'今日已处理')",
      pre_b.should_claim(st2, today, True) == (False, "今日已处理")
      and pre_b.should_claim({}, today, False) == (False, "配置关闭"))

# ---------------------------------------------------------------- T4 跨日重算
pre_c, _qe_c = load_instance("c")
ro3 = FakeRobot(ACC)
pre_c._save_persist(ro3, {"done_date": "2000-01-01", "last_ok_date": "2000-01-01"})
st3 = pre_c._get_state(ro3)
t4_claim = pre_c.claim_double_exp(ro3)
t4_hb = pre_c.double_claim_date(ro3)
check("T4 跨日重算: 盘上是旧日期 → 回填后仍允许启动; 心跳不报已领",
      st3.get("done_date") == "2000-01-01" and t4_claim is True and t4_hb == "",
      "mem=%s claim=%s hb=%r" % (st3.get("done_date"), t4_claim, t4_hb))

# ---------------------------------------------------------------- T5 坏文件不崩
pre_d, _qe_d = load_instance("d")
bad_cases = []
for i, raw in enumerate(("{not json", "[1,2,3]", '{"done_date": 12345}', "")):
    write_raw(ACC, raw)
    ro = FakeRobot(ACC)
    st = pre_d._get_state(ro)
    okflag = pre_d.claim_double_exp(ro)
    bad_cases.append((raw[:12], st.get("done_date") or "", bool(okflag)))
check("T5 文件损坏/结构不对: 按空处理不崩, 且照常允许领取",
      all(c[1] == "" and c[2] is True for c in bad_cases),
      "%s" % (bad_cases,))

# ---------------------------------------------------------------- T6 原子写
pre_e, _qe_e = load_instance("e")
os.remove(state_file(ACC))
ro4 = FakeRobot(ACC)
pre_e._save_persist(ro4, {"done_date": today, "last_ok_date": today, "active": True})
tmp_left = os.path.exists(state_file(ACC) + ".tmp")
try:
    disk6 = read_state_file(ACC)
    parsed = isinstance(disk6, dict)
except Exception:
    disk6, parsed = {}, False
check("T6 原子写: 写后可解析(只落 STATE_KEYS, 瞬时流程态不落盘) + 无 .tmp 残留",
      parsed and disk6.get("done_date") == today and "active" not in disk6
      and not tmp_left,
      "disk=%s tmp=%s" % (disk6, tmp_left))

# ---------------------------------------------------------------- T7 失败也记日期
pre_f, _qe_f = load_instance("f")
os.remove(state_file(ACC))
ro5 = FakeRobot(ACC)
pre_f.claim_double_exp(ro5)
st5 = pre_f._get_state(ro5)
st5["phase"] = "dialog"
pre_f.on_close_dialog(ro5)			# 未点过领取(claim_clicked=False) → _finish(ok=False)
pre_g, _qe_g = load_instance("g")
ro6 = FakeRobot(ACC)
t7_claim = pre_g.claim_double_exp(ro6)
t7_hb = pre_g.double_claim_date(ro6)
check("T7 失败(放弃)也记 done_date → 当天不再尝试; 但**不**上报已领(心跳为空)",
      t7_claim is False and t7_hb == ""
      and read_state_file(ACC).get("done_date") == today
      and not read_state_file(ACC).get("last_ok_date"),
      "claim=%s hb=%r disk=%s" % (t7_claim, t7_hb, read_state_file(ACC)))

# ---------------------------------------------------------------- T8 无账号不写不崩
pre_h, _qe_h = load_instance("h")
ro7 = FakeRobot(ACC)
ro7.m_account = []
p8 = pre_h._state_path(ro7)
saved8 = pre_h._save_persist(ro7, {"done_date": today})
t8_claim = pre_h.claim_double_exp(ro7)
check("T8 账号取不到: 状态路径为空/不落盘(也不写共享占位文件), 流程照常(不崩)",
      p8 == "" and saved8 is False and t8_claim in (True, False)
      and not os.path.exists(os.path.join(STATE_DIR, "pre_daily__.json"))
      and not os.path.exists(os.path.join(STATE_DIR, "pre_daily_?.json")),
      "path=%r saved=%s claim=%s" % (p8, saved8, t8_claim))

# ---------------------------------------------------------------- T9 目录自动创建
pre_i, _qe_i = load_instance("i")
nested = os.path.join(STATE_DIR, "deep", "nest")
os.environ["ZCC_PRE_DAILY_STATE_DIR"] = nested
ro8 = FakeRobot("persist_nest@xy3.com")
saved9 = pre_i._save_persist(ro8, {"done_date": today})
exists9 = os.path.exists(os.path.join(nested, "pre_daily_persist_nest@xy3.com.json"))
os.environ["ZCC_PRE_DAILY_STATE_DIR"] = STATE_DIR
check("T9 状态目录不存在 → 自动创建并写入", saved9 is True and exists9,
      "saved=%s exists=%s dir=%s" % (saved9, exists9, nested))

# ---------------------------------------------------------------- T10 状态文件是目录
pre_j, _qe_j = load_instance("j")
weird = os.path.join(STATE_DIR, "pre_daily_dir_acc@xy3.com.json")
try:
    os.makedirs(weird)
except Exception:
    pass
ro9 = FakeRobot("dir_acc@xy3.com")
t10_loaded = pre_j._load_persist(ro9)
t10_claim = pre_j.claim_double_exp(ro9)
check("T10 状态文件是目录(打开必失败): 读按空处理, 领取不受影响",
      t10_loaded == {} and t10_claim is True,
      "loaded=%s claim=%s" % (t10_loaded, t10_claim))

# ---------------------------------------------------------------- T11 心跳格式
pre_k, _qe_k = load_instance("k")
ro10 = FakeRobot("fmt_acc@xy3.com")
pre_k._save_persist(ro10, {"done_date": today, "last_ok_date": today})
hb = pre_k.double_claim_date(ro10)
ro11 = FakeRobot("fmt_acc2@xy3.com")
pre_k._save_persist(ro11, {"done_date": today, "last_ok_date": "2000-01-01"})
hb_old = pre_k.double_claim_date(ro11)
check("T11 心跳字段=YYYYMMDD(?8 位数字, 中控 todayKey 口径); 旧 last_ok_date 不报",
      hb == compact_today and len(hb) == 8 and hb.isdigit() and hb_old == "",
      "hb=%r old=%r expect=%s" % (hb, hb_old, compact_today))

# ---------------------------------------------------------------- T12 多账号不串扰
pre_l, _qe_l = load_instance("l")
acc_x, acc_y = "persist_x@xy3.com", "persist_y@xy3.com"
ro_x, ro_y = FakeRobot(acc_x), FakeRobot(acc_y)
pre_l._save_persist(ro_x, {"done_date": today, "last_ok_date": today})
t12_y_claim = pre_l.claim_double_exp(ro_y)
check("T12 按账号隔离: x 已领不影响 y(可领); 文件名各自独立",
      os.path.exists(state_file(acc_x)) and not os.path.exists(state_file(acc_y))
      and t12_y_claim is True,
      "x=%s y=%s claim_y=%s" % (os.path.exists(state_file(acc_x)),
                                os.path.exists(state_file(acc_y)), t12_y_claim))

# ---------------------------------------------------------------- 结果
print("\n自检目标: %s" % path)
print("sha1=%s" % hashlib.sha1(src.encode("utf-8")).hexdigest()[:12])
print("状态目录(临时): %s" % STATE_DIR)
print("结果：%d 项，失败 %d 项" % (total, fails))
try:
    shutil.rmtree(STATE_DIR, ignore_errors=True)
except Exception:
    pass
sys.exit(1 if fails else 0)
