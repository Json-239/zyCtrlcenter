# -*- coding: utf-8 -*-
"""装备链路修复批自检 —— 2026-10-12（穿戴触发/回执化/工具槽/任务品禁穿禁丢）。

背景（10-10 只读审计定论，本批修复）:
  ① 触发面窄: 穿戴评估原来只在 S2C_ROLE_ITEM 全量快照触发(≈2 次/天), 会话中途
     入包不评估(robot0005089 狐皮衣入包 3.5h 未触发);
  ② 假成功日志: 条件不符(力量/外形)时服务端静默拒, 本地在"发送时"就打"穿装备"
     (robot0005291 盘龙棍 6 次/天全败);
  ③ 工具槽(0x100e)无路径: 御空神剑 504402/矿镐 108014(equip_class=特殊)被整体跳过;
  ④ 神捕/镖行任务采购品(24 件抵押品)被自动穿/丢会破坏"回收提交"。

本自检（源码级提取真实函数 + fake 运行时, 与 equip_criteria_selftest 同手法）:
  ① on_bag_add 触发+节流(60s/号) + 忙豁免(战斗/对话/摆摊/缓存空)
  ② pending 三态: 成功 / 未生效(120s 重发, 3 次放弃) / 缓存缺失保守跳过 + 跨天恢复
  ③ NO_EQUIP_ITEMS: 24 件任务品不穿不丢(含"劣于已穿也不丢")
  ④ 工具槽: 空穿 / 已占不换 / 多工具稳定排序 / 非背包区不发
  ⑤ 丢弃保护: 锁定装备不丢; 工具只穿不丢
  ⑥ 坏版灵敏度: 去节流 / 去 NO_EQUIP / 去 giveup 各自使对应断言失败

用法: python tools/equip_flow_20261012_selftest.py [script 目录]
      不带参数默认 deploy/zones/prod-240-2300/script(需含 xml/item.xml)。
"""
import hashlib
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
ROOT = os.path.dirname(HERE)
SCRIPT = sys.argv[1] if len(sys.argv) > 1 else os.path.normpath(
    os.path.join(HERE, "..", "deploy", "zones", "prod-240-2300", "script"))

PASS, FAIL = [], []


def check(name, cond, detail=""):
    (PASS if cond else FAIL).append(name)
    print("[%s] %s%s" % ("PASS" if cond else "FAIL", name, ("  " + detail) if detail else ""))


# ---------------------------------------------------------------------------
# fake 运行时
# ---------------------------------------------------------------------------
K = types.ModuleType("keys")
K.ITEM_INDEX, K.ITEM_ID = 1001, 1002
K.ATTR_DATA, K.ITEM_COUNT = 1552, 1010
K.ITEM_QUALITY, K.QUALITY_WASTE = 1011, 0
K.EQUIP_CLASS = 1012
K.COOKIE_DATA, K.COOKIE_PW_LOCK, K.COOKIE_TIME_LOCK = 1013, 5366, 5367
K.ITEM_POSITION = 5356
sys.modules["keys"] = K

QS = types.ModuleType("quest_state")
QS.ST_DIALOG, QS.ST_SHOP, QS.ST_READY = "DIALOG", "SHOP", "READY"
sys.modules["quest_state"] = QS

proto3_mod = types.ModuleType("protocol3")
proto3_mod.C2S_USEITEM = 80090
proto3_mod.C2S_DELITEM = 80093
proto3_mod.C2S_CHANG_POCKET = 80091
sys.modules["protocol3"] = proto3_mod

cfg_mod = types.ModuleType("config")
cfg_mod.robot_drop_item_quality = 6
sys.modules["config"] = cfg_mod

diag_mod = types.ModuleType("diag")
diag_mod.log = lambda *a, **k: None
sys.modules["diag"] = diag_mod

rm_mod = types.ModuleType("robot_mgr")
rm_mod.is_personal = lambda ro: False
sys.modules["robot_mgr"] = rm_mod

# fake xmlparser: 直接 stdlib ET 解析真实生产 xml(验证 _load_tool_indexes 对真数据有效)
import xml.etree.ElementTree as _ET


class _FakeXmlparser(object):
    @staticmethod
    def _real(path):
        real = path if os.path.isabs(path) else os.path.join(SCRIPT, path)
        if not os.path.exists(real):
            real = os.path.join(SCRIPT, "xml", os.path.basename(path))
        return real

    @classmethod
    def get_root_from_xml(cls, path):
        return _ET.parse(cls._real(path)).getroot()

    @staticmethod
    def get_child_element_text(parent, name, default=""):
        # 复刻真实 xmlparser.get_child_element_value: 先子元素, 找不到取属性
        # (item_entry 的 item_index 是属性; equip_class 是子元素)
        e = parent.find(name)
        if e is not None:
            if e.text is None or e.text.strip() == "":
                return default
            return e.text
        v = parent.get(name)
        if v is None:
            return default
        return v

    @staticmethod
    def get_child_element_int(parent, name, default=0):
        t = _FakeXmlparser.get_child_element_text(parent, name, "")
        if t == "":
            return default
        try:
            return int(t)
        except Exception:
            return default


sys.modules["xmlparser"] = _FakeXmlparser

# 装备模板(client.g_item_data_dict 结构)
ITEMS = {
    213604: ("element_weapon_a_lv3", "螺旋枪"),
    213700: ("element_neck_a_lv3", "示例lv3项链"),
    213701: ("element_neck_a_lv3", "示例lv3项链B"),
    225601: ("element_neck_a_lv0", "贝壳项链"),
    210602: ("element_weapon_a_lv1", "寒铁枪"),      # 神捕/镖行采购品
    210902: ("element_weapon_a_lv1", "九节鞭"),      # 同上
    108014: ("", "矿镐"),                            # 工具(equip_class=特殊, 无 element)
    504402: ("", "御空神剑"),                        # 工具
}
client_mod = types.ModuleType("client")
client_mod.g_item_data_dict = {
    i: {"item_index": i, "item_name": nm, "related_element_index": rel}
    for i, (rel, nm) in ITEMS.items()
}
sys.modules["client"] = client_mod


class FakeQuest(object):
    def __init__(self, state="READY"):
        self.state = state
        self.bag = {}
        self.shop_ctx = None


class FakeRobot(object):
    def __init__(self, account="qa_equip@xy3.com"):
        self.m_account = [account]
        self.sent = []
        self.m_bag_meta = {}
        self.m_bag_cache = {}
        self.m_fight_state = False
        self.m_quest = None
        self.m_booth = None

    def send_message(self, mid, data):
        self.sent.append((mid, list(data)))
        return 0


# ---------------------------------------------------------------------------
# 源码提取
# ---------------------------------------------------------------------------
def _read(fn):
    return open(os.path.join(SCRIPT, fn), encoding="utf-8", errors="replace").read()


def _sha1_of(fn):
    return hashlib.sha1(open(os.path.join(SCRIPT, fn), "rb").read()).hexdigest()


def grab_const(src, name):
    m = re.search(r"(?m)^%s *=.*$" % re.escape(name), src)
    assert m, "缺少常量 %s" % name
    return m.group(0)


def grab_const_multi(src, name):
    m = re.search(r"(?ms)^%s *= \(.*?^\)" % re.escape(name), src)
    assert m, "缺少多行常量 %s" % name
    return m.group(0)


def grab_func(src, name):
    m = re.search(r"(?ms)^def %s[(].*?(?=^def |^# )" % re.escape(name), src)
    if m is None:
        m = re.search(r"(?ms)^def %s[(].*" % re.escape(name), src)
    assert m, "缺少函数 %s" % name
    return m.group(0)


BAG_CONSTS = ("POS_BAG_BEGIN", "POS_BAG_END", "POS_EQUIP_BEGIN", "POS_EQUIP_END",
              "DROP_PROTECT_INDEXES", "EGG_EQUIP_POS", "MOUNT_EGG_ELEMENT_PREFIX",
              "MOUNT_EGG_ELEMENT_KEYWORD", "GUARDIAN_EGG_INDEXES", "GUARDIAN_EGG_NAMES",
              "_DROP_GUARD", "_DROP_GUARD_MS",
              "PURSUIT_ORDER_NAME_KEYWORD", "PURSUIT_ORDER_FALLBACK_SEGMENTS",
              "TOOL_EQUIP_POS", "TOOL_EQUIP_CLASS", "_TOOL_INDEX_CACHE", "_TOOL_LOAD_TRIED",
              "_EQUIP_ACK_TIMEOUT_MS", "_EQUIP_MAX_TRIES", "_EQUIP_PENDING_KEEP_MS")
BAG_FUNCS = ("_is_in_bag_pos", "_item_data", "egg_kind_of", "egg_kind", "is_egg_item",
             "is_protected_item", "is_no_equip_item",
             "_get_equip_slot", "parse_equip_lv", "_get_equip_lv", "meta_quality",
             "equip_score", "score_gt", "score_lt", "collect_best_equipped",
             "should_drop_equip", "_now_ms", "item_recently_dropped", "mark_item_dropped",
             "_emit_log",
             "_item_name", "is_pursuit_order_name", "is_pursuit_order_index",
             "is_pursuit_order_item", "_meta_is_locked", "drop_pursuit_orders",
             "_equip_pending_map", "_equip_giveup_map", "_equip_day_key",
             "_equip_pending_hit", "_equip_given_up", "_register_equip_pending",
             "_tick_equip_pending",
             "_load_tool_indexes", "is_tool_item", "_tool_slot_empty",
             "_collect_bag_tools", "_equip_tool_if_slot_empty",
             "auto_equip_best_items")


def build_bag_ns(bag_text):
    ns = {"protocol3": proto3_mod}
    parts = [grab_const(bag_text, n) for n in BAG_CONSTS]
    parts.append(grab_const_multi(bag_text, "MOUNT_EGG_NAMES"))
    parts.append(grab_const_multi(bag_text, "NO_EQUIP_ITEMS"))
    parts += [grab_func(bag_text, n) for n in BAG_FUNCS]
    exec("\n".join(parts), ns)
    return ns


def build_bo_mod(bag_ns, counter):
    mod = types.ModuleType("bag_ops")
    for k, v in bag_ns.items():
        setattr(mod, k, v)
    _real = bag_ns["auto_equip_best_items"]

    def _counted(robot_object, bag, now_ms=None):
        counter["n"] += 1
        return _real(robot_object, bag, now_ms)

    mod.auto_equip_best_items = _counted
    sys.modules["bag_ops"] = mod      # quest_engine 侧 import bag_ops 取本次构建
    return mod


def build_qe_ns(qe_text, bo_mod):
    ns = {"marshal": marshal, "keys": K, "quest_state": QS, "diag": diag_mod,
          "time": time, "bag_ops": bo_mod,
          "__bag_meta_from_template": lambda item: None,
          "__shop_purchase_done": lambda *a, **k: None}
    parts = [grab_const(qe_text, "EQUIP_EVAL_MIN_GAP_MS")]
    parts += [grab_func(qe_text, n) for n in
              ("__loads_blob", "__item_count", "__item_pos", "__bag_quality",
               "__bag_is_equip", "__bag_is_locked", "__equip_eval_on_bag_add",
               "on_bag_add")]
    exec("\n".join(parts), ns)
    return ns


bag_src = _read("bag_ops.py")
qe_src = _read("quest_engine.py")
print("自检目标目录: %s" % SCRIPT)
print("bag_ops.py sha1=%s" % _sha1_of("bag_ops.py"))
print("quest_engine.py sha1=%s" % _sha1_of("quest_engine.py"))

CALLS = {"n": 0}
BNS = build_bag_ns(bag_src)
BO_MOD = build_bo_mod(BNS, CALLS)
sys.modules["bag_ops"] = BO_MOD
QNS = build_qe_ns(qe_src, BO_MOD)
check("bag_ops/quest_engine 源码块提取(含回执/工具/节流依赖链)", True)

# 日志捕获: 替换 NS 里的 _emit_log(函数体按 globals 解析, 换掉即生效)
LOGS = []


def _cap_log(robot_object, level, msg):
    LOGS.append((level, msg))


_orig_emit = BNS["_emit_log"]
BNS["_emit_log"] = _cap_log


def make_blob(item_index, item_id, pos, count=1):
    obj = {K.ITEM_INDEX: item_index, K.ITEM_ID: item_id, 1000: pos,
           K.ATTR_DATA: {K.ITEM_COUNT: count}}
    return [marshal.dumps(obj)]


NOW0 = 10 ** 13        # 固定基准(ms) —— 避免真实时钟让"节流/超时"用例不稳定

# ---------------------------------------------------------------------------
# ① on_bag_add 触发 + 节流 + 忙豁免
# ---------------------------------------------------------------------------
ONB = QNS["on_bag_add"]
r = FakeRobot()
r.m_quest = FakeQuest()
CALLS["n"] = 0
ONB(r, make_blob(213700, 111, 8192))
check("① 入包触发评估(首次)", CALLS["n"] == 1, "n=%s sent=%s" % (CALLS["n"], r.sent))
check("① 端到端: 入包即穿(USEITEM 111 发出)", r.sent == [(80090, [111])], "sent=%s" % r.sent)
ONB(r, make_blob(213700, 112, 8192))
check("① 节流: 60s 内第二次不触发", CALLS["n"] == 1, "n=%s" % CALLS["n"])
r.m_equip_eval_ms -= 61000
ONB(r, make_blob(213700, 113, 8192))
check("① 节流到期后再次触发", CALLS["n"] == 2, "n=%s" % CALLS["n"])
r.m_fight_state = True
r.m_equip_eval_ms -= 61000
ONB(r, make_blob(213700, 114, 8192))
check("① 战斗中不触发", CALLS["n"] == 2, "n=%s" % CALLS["n"])
r.m_fight_state = False
r.m_quest.state = QS.ST_DIALOG
r.m_equip_eval_ms -= 61000
ONB(r, make_blob(213700, 115, 8192))
check("① 对话中不触发", CALLS["n"] == 2, "n=%s" % CALLS["n"])
r.m_quest.state = QS.ST_READY
r.m_booth = types.SimpleNamespace(enabled=True)
r.m_equip_eval_ms -= 61000
ONB(r, make_blob(213700, 116, 8192))
check("① 摆摊中不触发", CALLS["n"] == 2, "n=%s" % CALLS["n"])
r.m_booth = None
r.m_bag_cache = None
r.m_equip_eval_ms -= 61000
ONB(r, make_blob(213700, 117, 8192))
check("① m_bag_cache 空不触发", CALLS["n"] == 2, "n=%s" % CALLS["n"])

# ---------------------------------------------------------------------------
# ② pending 三态
# ---------------------------------------------------------------------------
REG = BNS["_register_equip_pending"]
TICK = BNS["_tick_equip_pending"]

# (a) 成功: 位置进装备栏
r2 = FakeRobot()
bag2 = {213700: [111, 1, 8192]}
REG(r2, 111, 213700, "neck_a", 3, 4, NOW0)
LOGS[:] = []
bag2[213700][2] = 4098
TICK(r2, bag2, NOW0 + 1000)
check("② 成功态: 记'穿装备成功'+清除 pending",
      any("穿装备成功" in m for _, m in LOGS) and 111 not in r2.m_equip_pending,
      "logs=%s" % [m for _, m in LOGS])

# (b) 未生效: 仍背包, 120s 超时 → warn 重发 → 3 次放弃
r3 = FakeRobot()
bag3 = {213700: [111, 1, 8192]}
REG(r3, 111, 213700, "neck_a", 3, 4, NOW0)
LOGS[:] = []
TICK(r3, bag3, NOW0 + 121000)
check("② 超时第1次: warn'穿戴未生效 第1次'+重发",
      any(lv == "warn" and "穿戴未生效" in m and "第1次" in m for lv, m in LOGS)
      and r3.sent == [(80090, [111])] and r3.m_equip_pending[111]["tries"] == 1,
      "sent=%s tries=%s" % (r3.sent, r3.m_equip_pending.get(111, {}).get("tries")))
TICK(r3, bag3, NOW0 + 242000)
check("② 超时第2次: warn 第2次+再重发",
      len([1 for lv, m in LOGS if "穿戴未生效" in m]) == 2 and len(r3.sent) == 2
      and r3.m_equip_pending[111]["tries"] == 2)
TICK(r3, bag3, NOW0 + 363000)
check("② 超时第3次: 放弃(条件不符)+清 pending+登记 giveup",
      any("放弃穿戴" in m for _, m in LOGS) and 111 not in r3.m_equip_pending
      and r3.m_equip_giveup.get(111), "giveup=%s" % r3.m_equip_giveup)

# (c) 放弃后不再重发(同日); 跨天恢复
r3.sent[:] = []
BNS["auto_equip_best_items"](r3, {213700: [111, 1, 8192]}, NOW0 + 364000)
check("② 放弃后同日不再重发", r3.sent == [], "sent=%s" % r3.sent)
BNS["auto_equip_best_items"](r3, {213700: [111, 1, 8192]}, NOW0 + 2 * 24 * 3600 * 1000)
check("② 跨天后 giveup 清除: 重新尝试(发 USE)", r3.sent == [(80090, [111])], "sent=%s" % r3.sent)

# (d) 缓存无该物品: 保守跳过(不算失败)
r4 = FakeRobot()
REG(r4, 222, 213700, "neck_a", 3, 4, NOW0)
r4.sent[:] = []
TICK(r4, {}, NOW0 + 121000)
check("② 缓存无该物品: 保守跳过(不重发不计数)",
      r4.sent == [] and r4.m_equip_pending[222]["tries"] == 0)
bag4b = {213700: [999, 1, 8192]}      # 该 item_index 现在是别的实例(item_id 999≠222)
TICK(r4, bag4b, NOW0 + 200000)
check("② 实例不匹配(该 index 是别的实例): 保守跳过",
      r4.sent == [] and r4.m_equip_pending[222]["tries"] == 0)

# ---------------------------------------------------------------------------
# ③ NO_EQUIP_ITEMS(神捕/镖行任务品)不穿不丢
# ---------------------------------------------------------------------------
r5 = FakeRobot()
BNS["_DROP_GUARD"].clear()
BNS["auto_equip_best_items"](r5, {210602: [555, 1, 8192]})
check("③ 任务品不被穿(无 USEITEM)", all(m != 80090 for m, _ in r5.sent), "sent=%s" % r5.sent)
check("③ 任务品不被丢(无 DELITEM)", all(m != 80093 for m, _ in r5.sent), "sent=%s" % r5.sent)

r6 = FakeRobot()
r6.m_bag_meta = {210602: {"quality": 1}, 213604: {"quality": 4}}
BNS["_DROP_GUARD"].clear()
e6, d6 = BNS["auto_equip_best_items"](r6, {213604: [666, 1, 4098], 210602: [555, 1, 8192]})
check("③ 任务品(劣于已穿)也不被丢", (e6, d6) == (0, 0) and r6.sent == [],
      "e=%s d=%s sent=%s" % (e6, d6, r6.sent))
check("③ should_drop_equip 对任务品恒 False",
      BNS["should_drop_equip"](210602, {"quality": 1}, {"weapon_a": (3, 4, 213604)}, 6) is False
      and BNS["should_drop_equip"](210902, {"quality": 1}, {}, 6) is False)
check("③ 名单=24 件(镖行抵押品全量)+ 判断函数",
      len(BNS["NO_EQUIP_ITEMS"]) == 24 and BNS["is_no_equip_item"](220202)
      and BNS["is_no_equip_item"](101334) and not BNS["is_no_equip_item"](213700))

# ---------------------------------------------------------------------------
# ④ 工具槽(0x100e): 空穿 / 已占不换 / 稳定排序 / 非背包区不发
# ---------------------------------------------------------------------------
check("④ 工具识别(真 xml): 108014/504402 是工具, 210602 不是",
      BNS["is_tool_item"](108014) and BNS["is_tool_item"](504402)
      and not BNS["is_tool_item"](210602) and not BNS["is_tool_item"](213700))

r7 = FakeRobot()
BNS["_DROP_GUARD"].clear()
BNS["auto_equip_best_items"](r7, {108014: [777, 1, 8192]})
check("④ 工具槽空: 发 USE 工具+登记 pending",
      r7.sent == [(80090, [777])] and 777 in r7.m_equip_pending,
      "sent=%s" % r7.sent)

r8 = FakeRobot()
BNS["_DROP_GUARD"].clear()
BNS["auto_equip_best_items"](r8, {108014: [777, 1, 8192], 504402: [778, 1, 4110]})
check("④ 工具槽已占: 不互换(不发任何包, 含不丢)",
      r8.sent == [], "sent=%s" % r8.sent)

r9 = FakeRobot()
BNS["_DROP_GUARD"].clear()
BNS["auto_equip_best_items"](r9, {504402: [888, 1, 8192], 108014: [777, 1, 8192]})
check("④ 多工具稳定取 item_index 最小(108014)", r9.sent == [(80090, [777])], "sent=%s" % r9.sent)

r10 = FakeRobot()
BNS["_DROP_GUARD"].clear()
BNS["auto_equip_best_items"](r10, {108014: [777, 1, 25616]})
check("④ 工具不在背包区(包裹栏): 不发", r10.sent == [], "sent=%s" % r10.sent)

# ---------------------------------------------------------------------------
# ⑤ 丢弃保护: 锁定装备不丢(工具不丢已在④隐式覆盖)
# ---------------------------------------------------------------------------
r11 = FakeRobot()
r11.m_bag_meta = {213700: {"quality": 4, "is_locked": True}, 213701: {"quality": 6}}
BNS["_DROP_GUARD"].clear()
e11, d11 = BNS["auto_equip_best_items"](r11, {213701: [112, 1, 4098], 213700: [111, 1, 8192]})
check("⑤ 锁定装备(同部位低品质)不丢", r11.sent == [] and d11 == 0,
      "e=%s d=%s sent=%s" % (e11, d11, r11.sent))

# 对照: 同场景去掉锁定 → 应被丢(证明该用例有区分度)
r12 = FakeRobot()
r12.m_bag_meta = {213700: {"quality": 4}, 213701: {"quality": 6}}
BNS["_DROP_GUARD"].clear()
e12, d12 = BNS["auto_equip_best_items"](r12, {213701: [112, 1, 4098], 213700: [111, 1, 8192]})
check("⑤ 对照: 同场景无锁定 → 被丢(用例敏感)",
      e12 == 0 and d12 == 1 and r12.sent == [(80093, [111])], "d=%s sent=%s" % (d12, r12.sent))

# ---------------------------------------------------------------------------
# ⑥ 坏版灵敏度
# ---------------------------------------------------------------------------
# ⑥-1 去掉节流判断 → "60s 内不触发"必须失败
bad1 = qe_src.replace("if now - _last < EQUIP_EVAL_MIN_GAP_MS:", "if False:")
check("⑥ 坏版1 构造(节流判断已移除)", bad1 != qe_src)
if bad1 != qe_src:
    C1 = {"n": 0}
    BNS1 = build_bag_ns(bag_src)
    BO1 = build_bo_mod(BNS1, C1)
    QNS1 = build_qe_ns(bad1, BO1)
    r13 = FakeRobot()
    r13.m_quest = FakeQuest()
    QNS1["on_bag_add"](r13, make_blob(213700, 111, 8192))
    QNS1["on_bag_add"](r13, make_blob(213700, 112, 8192))
    check("⑥ 坏版灵敏度1: 去节流 → 60s 内第二次也触发(原断言会失败)", C1["n"] == 2,
          "n=%s" % C1["n"])

# ⑥-2 去掉 NO_EQUIP 判断 → "任务品不被穿"必须失败
bad2 = bag_src.replace("\t\tif is_no_equip_item(item_index):", "\t\tif False:")
check("⑥ 坏版2 构造(NO_EQUIP 判断已移除)", bad2 != bag_src)
if bad2 != bag_src:
    BNS2 = build_bag_ns(bad2)
    r14 = FakeRobot()
    BNS2["_DROP_GUARD"].clear()
    BNS2["auto_equip_best_items"](r14, {210602: [555, 1, 8192]})
    check("⑥ 坏版灵敏度2: 去 NO_EQUIP → 任务品被穿(原断言会失败)",
          any(m == 80090 for m, _ in r14.sent), "sent=%s" % r14.sent)

# ⑥-3 去掉 giveup 检查 → "放弃后不重发"必须失败
bad3 = bag_src.replace("if _equip_given_up(robot_object, best[1][0]):", "if False:")
check("⑥ 坏版3 构造(giveup 检查已移除)", bad3 != bag_src)
if bad3 != bag_src:
    BNS3 = build_bag_ns(bad3)
    r15 = FakeRobot()
    r15.m_equip_giveup = {111: BNS3["_equip_day_key"](NOW0)}
    BNS3["_DROP_GUARD"].clear()
    BNS3["auto_equip_best_items"](r15, {213700: [111, 1, 8192]}, NOW0)
    check("⑥ 坏版灵敏度3: 去 giveup → 已放弃实例仍被重发(原断言会失败)",
          any(m == 80090 for m, _ in r15.sent), "sent=%s" % r15.sent)

# ---------------------------------------------------------------------------
print("")
print("结果: PASS=%d FAIL=%d" % (len(PASS), len(FAIL)))
if FAIL:
    print("失败项:")
    for f in FAIL:
        print("  - %s" % f)
    sys.exit(1)
print("全部通过")
