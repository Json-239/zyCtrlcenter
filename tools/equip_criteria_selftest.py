# -*- coding: utf-8 -*-
"""装备判据统一 + buff 卡恢复 自检(2026-09-21)。

生产背景(现场取证):
  - 本服 item_quality 与装备好坏无稳定关系/常反向: 213604 螺旋枪 quality4 = lv3 好武器,
    225601 贝壳项链 quality6 = lv0 新手白装(item.xml 模板 + BAG_QUALITY 日志)。
  - 旧判据"quality < 6 丢弃"把好装备当垃圾丢, 且没过滤 pos, 连已穿戴的也发丢弃
    (服务端 ret=16), 全天 1453 次无效请求。
  - 用户口径: "等级优先, 再者判断品质, 等级相同再判断品质"。

为什么源码级: bag_ops / auto_summon / msghandle 依赖机器人运行时(client 模板表、
robot_object、cnetwork 扩展), 离线 import 会失败。这里从**产物源码**抽取真实函数直接
执行(与 dialog_selftest 同思路), 另加静态断言(接线/过滤存在性)。

用法: python equip_criteria_selftest.py [script 目录]
      不带参数默认校验仓库副本 deploy/zones/prod-240-2300/script/;
      传线上副本路径可对线上文件再验一次。
"""
import hashlib
import os
import re
import sys
import types

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

HERE = os.path.dirname(os.path.abspath(__file__))
DIR = sys.argv[1] if len(sys.argv) > 1 else os.path.normpath(
    os.path.join(HERE, "..", "deploy", "zones", "prod-240-2300", "script"))

fails = 0


def check(name, ok, detail=""):
    global fails
    fails += 0 if ok else 1
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  " + detail) if detail else ""))


def load(fn):
    return open(os.path.join(DIR, fn), encoding="utf-8").read()


bag_src = load("bag_ops.py")
as_src = load("auto_summon.py")
cfg_src = load("config.py")
mh_src = load("msghandle.py")
print("自检目标目录: %s" % DIR)

# ---------------------------------------------------------------------------
# fake 运行时: client 模板表 / protocol3 / config / diag
# ---------------------------------------------------------------------------
# 真实项: 213604 螺旋枪=element_weapon_a_lv3, 225601 贝壳项链=element_neck_a_lv0
# 示例项(仅自检用, 结构同真实): 213700 同部位(lv3)项链, 225101 新手衣
ITEMS = {
    213604: ("element_weapon_a_lv3", "螺旋枪"),
    213700: ("element_neck_a_lv3", "示例lv3项链"),
    213701: ("element_neck_a_lv3", "示例lv3项链B"),
    225601: ("element_neck_a_lv0", "贝壳项链"),
    225101: ("element_body_a_lv0", "粗布衣"),
    110173: ("element_other_a_lv0", "追捕令"),
    190016: ("element_other_a_lv0", "畅享服务卡"),
    101316: ("mount_birth_element_00001", "青牛"),   # 2026-09-22 坐骑蛋(元数据判据)
}
client_mod = types.ModuleType("client")
client_mod.g_item_data_dict = {
    i: {"item_index": i, "item_name": nm, "related_element_index": rel}
    for i, (rel, nm) in ITEMS.items()
}
sys.modules["client"] = client_mod

proto_mod = types.ModuleType("protocol3")
proto_mod.C2S_USEITEM = 80090
proto_mod.C2S_DELITEM = 80093
proto_mod.C2S_REQUEST_CHANGE_TITLE = 90339
sys.modules["protocol3"] = proto_mod

cfg_mod = types.ModuleType("config")
cfg_mod.robot_drop_item_quality = 6
cfg_mod.robot_bag_cleanup = True
cfg_mod.robot_use_buff_items = [190016, 190018, 190033, 190039]
sys.modules["config"] = cfg_mod

diag_mod = types.ModuleType("diag")
diag_mod.log = lambda *a, **k: None
sys.modules["diag"] = diag_mod


class FakeRobot(object):
    def __init__(self, account="qa0001@xy3.com"):
        self.m_account = [account]
        self.sent = []
        self.m_bag_meta = {}
        self.m_bag_cache = {}
        self.m_owned_titles = []

    def send_message(self, mid, data):
        self.sent.append((mid, list(data)))
        return 0


# ---------------------------------------------------------------------------
# 源码提取: 常量 + 函数(真实产物, 直接执行)
# ---------------------------------------------------------------------------
def grab_const(src, name):
    m = re.search(r"(?m)^%s *=.*$" % re.escape(name), src)
    assert m, "缺少常量 %s" % name
    return m.group(0)


def grab_func(src, name):
    m = re.search(r"(?ms)^def %s[(].*?(?=^def |^# )" % re.escape(name), src)
    if m is None:
        m = re.search(r"(?ms)^def %s[(].*" % re.escape(name), src)
    assert m, "缺少函数 %s" % name
    return m.group(0)


def grab_const_multi(src, name):
    # 多行常量(元组): NAME = ( ... ^) —— 2026-09-22 蛋名名单 MOUNT_EGG_NAMES 用
    m = re.search(r"(?ms)^%s *= \(.*?^\)" % re.escape(name), src)
    assert m, "缺少多行常量 %s" % name
    return m.group(0)


NS = {"protocol3": proto_mod}   # bag_ops 模块级 import protocol3(提取块里没有 import)
parts = []
for n in ("POS_BAG_BEGIN", "POS_BAG_END", "POS_EQUIP_BEGIN", "POS_EQUIP_END",
          "DROP_PROTECT_INDEXES", "EGG_EQUIP_POS", "MOUNT_EGG_ELEMENT_PREFIX",
          "MOUNT_EGG_ELEMENT_KEYWORD", "GUARDIAN_EGG_INDEXES", "GUARDIAN_EGG_NAMES"):
    parts.append(grab_const(bag_src, n))
# 2026-09-22 孵化链路: auto_equip_best_items 现在调 is_egg_item/is_protected_item,
# 提取块必须带上蛋判据依赖链(否则 NameError —— 上一版漏了这组, 本自检曾 FAIL)。
parts.append(grab_const_multi(bag_src, "MOUNT_EGG_NAMES"))
for n in ("_is_in_bag_pos", "_item_data", "egg_kind_of", "egg_kind", "is_mount_egg",
          "is_guardian_egg", "is_egg_item", "egg_pos_ok", "is_protected_item",
          "_get_equip_slot", "parse_equip_lv", "_get_equip_lv",
          "meta_quality", "equip_score", "score_gt", "score_lt", "equip_slot",
          "collect_best_equipped", "should_drop_equip", "_emit_log",
          "auto_equip_best_items"):
    parts.append(grab_func(bag_src, n))
exec("\n".join(parts), NS)
check("bag_ops 源码块提取(21 函数 + 11 常量, 含蛋判据依赖链)", True)

# auto_summon 的背包整理(动态跑: 验证丢弃过滤/保护名单/buff 卡称谓安排)
AS = {}
aparts = []
for n in ("_BAG_CLEAN_COOLDOWN_MS", "_TITLE_RESTORE_DELAYS_MS"):
    aparts.append(grab_const(as_src, n))
for n in ("_find_bag", "_emit_level", "_emit", "_emit_warn", "_schedule_title_restore",
          "_try_title_restore", "_try_bag_cleanup"):
    aparts.append(grab_func(as_src, n))
exec("\n".join(aparts), AS)
check("auto_summon 源码块提取(_try_bag_cleanup 等)", True)

bo_mod = types.ModuleType("bag_ops")
for k, v in NS.items():
    setattr(bo_mod, k, v)
sys.modules["bag_ops"] = bo_mod

# msghandle 的称谓切换(动态跑: 验证排除会员称谓 9008 / 无候选不发)
# 补 msghandle 模块级 import(提取块里没有): protocol3 / hashlib / diag
MH = {"protocol3": proto_mod, "hashlib": hashlib, "diag": diag_mod}
exec(grab_func(mh_src, "change_to_normal_title"), MH)
check("msghandle 源码块提取(change_to_normal_title)", True)

# ---------------------------------------------------------------------------
# ① 打分 / 比较: (lv, quality), lv 优先, 同 lv 比 quality
# ---------------------------------------------------------------------------
check("parse_equip_lv(weapon_a_lv3)=3", NS["parse_equip_lv"]("element_weapon_a_lv3") == 3)
check("parse_equip_lv(neck_a_lv0)=0", NS["parse_equip_lv"]("element_neck_a_lv0") == 0)
check("parse_equip_lv(空/None/无lv)=0",
      NS["parse_equip_lv"]("") == 0 and NS["parse_equip_lv"](None) == 0
      and NS["parse_equip_lv"]("element_weapon_a") == 0)
check("parse_equip_lv 大小写不敏感", NS["parse_equip_lv"]("element_x_LV12") == 12)

s_good = NS["equip_score"](213604, {"quality": 4})    # 螺旋枪 (3, 4)
s_new = NS["equip_score"](225601, {"quality": 6})     # 贝壳项链 (0, 6)
check("equip_score: 螺旋枪=(3,4), 贝壳项链=(0,6)", s_good == (3, 4) and s_new == (0, 6),
      "good=%s new=%s" % (s_good, s_new))
check("lv 优先: lv3q4 优于 lv0q6(品质号反向不误判)",
      NS["score_gt"](s_good, s_new) and NS["score_lt"](s_new, s_good))
check("同 lv 比 quality: (3,6)>(3,4) 且 (3,4) 不 >(3,6)",
      NS["score_gt"]((3, 6), (3, 4)) and not NS["score_gt"]((3, 4), (3, 6))
      and NS["score_lt"]((3, 4), (3, 6)))
check("quality 未知不参与比较(不优也不劣)",
      not NS["score_gt"]((3, None), (3, 4)) and not NS["score_lt"]((3, None), (3, 4)))
check("equip_score 无 meta 时 quality=None",
      NS["equip_score"](213604, None) == (3, None))

# ---------------------------------------------------------------------------
# ② 丢弃过滤: pos 必须在背包区; 已穿戴(装备栏)/位置未知一律不丢
# ---------------------------------------------------------------------------
check("_is_in_bag_pos(背包 8192)=True", NS["_is_in_bag_pos"]([1, 1, 8192]) is True)
check("_is_in_bag_pos(装备栏 4098)=False", NS["_is_in_bag_pos"]([1, 1, 4098]) is False)
check("_is_in_bag_pos(包裹栏 25616)=False", NS["_is_in_bag_pos"]([1, 1, 25616]) is False)
check("_is_in_bag_pos(pos 缺失)=False(保守不丢)", NS["_is_in_bag_pos"]([1, 1]) is False)

# collect_best_equipped: 只统计装备栏(pos 0x1000~0x1100)
bag_t = {213700: [11, 1, 4098], 225601: [22, 1, 8192]}
be = NS["collect_best_equipped"](FakeRobot(), bag_t, {})
check("collect_best_equipped 只统计装备栏物品",
      list(be.keys()) == ["neck_a"] and be["neck_a"][0] == 3, "be=%s" % be)

# should_drop_equip(判据本体; pos 由调用方过滤)
check("有在穿 lv3: 背包 lv0 项链 → 丢",
      NS["should_drop_equip"](225601, {"quality": 6}, be, 6) is True)
check("有在穿 lv3: 背包同 lv(3) 项链 → 不丢(不比在穿差)",
      NS["should_drop_equip"](213700, {"quality": 6}, be, 6) is False)
check("有在穿 lv0: 背包 lv3 → 不丢(比在穿好, 该换装不该丢)",
      NS["should_drop_equip"](213700, {"quality": 4}, {"neck_a": (0, 6, 225601)}, 6) is False)
check("无在穿: 背包 lv0 → 丢(只丢白装)",
      NS["should_drop_equip"](225601, {"quality": 6}, {}, 6) is True)
check("无在穿: 背包 lv3 → 不丢(留着穿)",
      NS["should_drop_equip"](213700, {"quality": 4}, {}, 6) is False)
check("同 lv 比 quality: 在穿(3,4), 背包(3,6) → 不丢",
      NS["should_drop_equip"](213700, {"quality": 6}, {"neck_a": (3, 4, 1)}, 6) is False)
check("同 lv 比 quality: 在穿(3,6), 背包(3,4) → 丢",
      NS["should_drop_equip"](213700, {"quality": 4}, {"neck_a": (3, 6, 1)}, 6) is True)
check("quality 未知 → 不比不丢",
      NS["should_drop_equip"](213700, {"quality": None}, {"neck_a": (3, 6, 1)}, 6) is False)
check("反证: 已穿戴 lv0 白装若无 pos 过滤会被判丢(说明过滤是必要闸门)",
      NS["should_drop_equip"](225601, {"quality": 6}, {}, 6) is True
      and NS["_is_in_bag_pos"]([22, 1, 4098]) is False)

# ---------------------------------------------------------------------------
# ③ / ④ auto_equip_best_items 动态: 换装/丢弃/不动已穿戴
# ---------------------------------------------------------------------------
def run_equip(bag, meta):
    r = FakeRobot()
    r.m_bag_meta = meta
    e, d = NS["auto_equip_best_items"](r, dict(bag))
    return r, e, d


META = {213700: {"quality": 4, "is_equip": True},
        213701: {"quality": 6, "is_equip": True},
        225601: {"quality": 6, "is_equip": True},
        213604: {"quality": 4, "is_equip": True}}

# 用例 A: 同部位在穿 lv3, 背包 lv0 白装 → 只丢白装, 不换装, 不动在穿的
r, e, d = run_equip({213700: [111, 1, 4098], 225601: [222, 1, 8192]}, META)
check("A 在穿 lv3 + 背包 lv0: 不换装(e=0)", e == 0, "e=%s d=%s sent=%s" % (e, d, r.sent))
check("A 在穿 lv3 + 背包 lv0: 丢白装 1 件(d=1)", d == 1 and r.sent == [(80093, [222])],
      "sent=%s" % (r.sent,))
check("A 在穿的 111 未被发任何消息(不动已穿戴)", all(s[1] != [111] for s in r.sent))

# 用例 B: 同部位在穿 lv0, 背包 lv3 → 换装(USEITEM 背包件), 不丢任何东西
r, e, d = run_equip({225601: [222, 1, 4098], 213700: [111, 1, 8192]}, META)
check("B 在穿 lv0 + 背包 lv3: 换装(e=1, USEITEM 111)",
      e == 1 and d == 0 and r.sent == [(80090, [111])], "sent=%s" % (r.sent,))

# 用例 C: 同部位在穿(3,6), 背包(3,4) → 同 lv 比 quality: 不换装, 低品质件丢
r, e, d = run_equip({213700: [111, 1, 8192], 213701: [112, 1, 4098]}, META)
check("C 同 lv 在穿 quality 更高: 不换装(e=0) 且丢低品质件(d=1)",
      e == 0 and d == 1 and r.sent == [(80093, [111])], "e=%s d=%s sent=%s" % (e, d, r.sent))

# 用例 D: 背包只有装备栏物品(位置异常) → 一个消息都不发(绝不碰已穿戴)
r, e, d = run_equip({213604: [111, 1, 4098]}, META)
check("D 只含装备栏物品: 无任何发包(不丢已穿戴)",
      (e, d, r.sent) == (0, 0, []), "sent=%s" % (r.sent,))

# 用例 E: 追捕令 110173 在背包仍按旧逻辑丢弃(回归保留)
r, e, d = run_equip({110173: [333, 1, 8192]}, {110173: {"quality": 0, "is_equip": False}})
check("E 追捕令 110173 仍被丢弃(既有行为保留)",
      r.sent == [(80093, [333])], "sent=%s" % (r.sent,))

# 用例 F: 2026-09-22 孵化链路 —— 蛋(坐骑蛋/元气蛋)不参与穿/丢/任何发包
check("F 蛋判据: is_egg_item(101316/102603)=True, 普通装备/药品=False",
      NS["is_egg_item"](101316) is True and NS["is_egg_item"](102603) is True
      and NS["is_egg_item"](225601) is False and NS["is_protected_item"](101316) is True)
r, e, d = run_equip({101316: [444, 1, 8192]}, {101316: {"quality": 0, "is_equip": True}})
check("F 背包里的蛋一个消息都不发(不穿不丢)", (e, d, r.sent) == (0, 0, []),
      "sent=%s" % (r.sent,))
check("F 静态: 丢弃路径调 is_protected_item + 蛋跳过判据存在",
      "if is_protected_item(_it_idx):" in bag_src and "if is_egg_item(item_index):" in bag_src)

# ---------------------------------------------------------------------------
# ⑤ auto_summon._try_bag_cleanup 动态: pos 过滤 / 保护名单 / buff 卡
# ---------------------------------------------------------------------------
def run_clean(rbag, rmeta, now_ms=10 ** 12, st=None):
    r = FakeRobot()
    r.m_bag_cache = dict(rbag)
    r.m_bag_meta = rmeta
    if st is None:
        st = {"last_bag_clean_ms": -100000}
    ret = AS["_try_bag_cleanup"](r, st, now_ms)
    return r, ret, st


# 用例 F: 背包 lv0 白装(有在穿 lv3) → 丢; 已穿戴 lv0 白装 → 不丢
rbag = {213700: [111, 1, 4098], 225601: [222, 1, 8192], 225101: [444, 1, 4098]}
rmeta = {213700: {"quality": 4, "is_equip": True},
         225601: {"quality": 6, "is_equip": True},
         225101: {"quality": 6, "is_equip": True}}
r, ret, st = run_clean(rbag, rmeta)
check("F 背包白装被丢(带 lv/quality 日志), 已穿戴白装 444 不动",
      r.sent == [(80093, [222])], "sent=%s ret=%s" % (r.sent, ret))

# 用例 G: 已穿戴的 lv0 白装(同部位无更优、也没在穿基准) → 因 pos 过滤不丢
rbag = {225601: [222, 1, 4098]}
r, ret, st = run_clean(rbag, {225601: {"quality": 6, "is_equip": True}})
check("G 装备栏白装(pos=4098): 一个包都不发", r.sent == [] and ret is False,
      "sent=%s" % (r.sent,))

# 用例 H: 保护名单(助战令/药) + 锁定不丢
rbag = {190146: [555, 1, 8192], 102007: [666, 1, 8192], 225601: [222, 1, 8192]}
rmeta = {190146: {"quality": 0, "is_equip": True},
         102007: {"quality": 0, "is_equip": True},
         225601: {"quality": 6, "is_equip": True, "is_locked": True}}
r, ret, st = run_clean(rbag, rmeta)
check("H 助战令/药品/锁定装备: 全不丢", r.sent == [], "sent=%s" % (r.sent,))

# 用例 I: 用 buff 卡 → 发 USEITEM + 安排延迟称谓还原
rbag = {190016: [777, 1, 8192]}
r, ret, st = run_clean(rbag, {190016: {"quality": 0, "is_equip": False}}, now_ms=100000)
check("I 用 buff 卡: USEITEM 发出且安排 2 次称谓还原",
      r.sent == [(80090, [777])] and ret is True
      and len(st.get("title_restore_at") or []) == 2,
      "sent=%s st=%s" % (r.sent, st.get("title_restore_at")))

# 用例 J: _try_title_restore 到点后调用 msghandle.change_to_normal_title
r = FakeRobot()
r.m_owned_titles = [(9008, "畅享尊贵"), (3, "初显锋芒")]
st = {"title_restore_at": [100000, 106000]}
called = {"n": 0}


def _fake_change(ro):
    called["n"] += 1
    ro.sent.append((proto_mod.C2S_REQUEST_CHANGE_TITLE, [3]))
    return 3


mh_mod = types.ModuleType("msghandle")
mh_mod.change_to_normal_title = _fake_change
sys.modules["msghandle"] = mh_mod
ok1 = AS["_try_title_restore"](r, st, 99000)     # 未到点
ok2 = AS["_try_title_restore"](r, st, 100000)    # 到点第一次
ok3 = AS["_try_title_restore"](r, st, 106000)    # 到点第二次
check("J 称谓还原: 未到点不动, 到点各执行一次(共2次)",
      ok1 is False and ok2 is True and ok3 is True and called["n"] == 2,
      "calls=%s" % called["n"])
check("J 称谓还原后清空计划", not st.get("title_restore_at"))

# ---------------------------------------------------------------------------
# ⑥ change_to_normal_title 动态: 候选排除 9008, 无候选不发
# ---------------------------------------------------------------------------
r = FakeRobot(account="robot0001007@xy3.com")
pick = MH["change_to_normal_title"](r, [(9008, "畅享尊贵"), (3, "初显锋芒"), (17, "天命之人")])
check("称谓: 排除会员 9008, 选中常规称谓", pick in (3, 17) and r.sent == [(90339, [pick])],
      "pick=%s sent=%s" % (pick, r.sent))

r = FakeRobot()
pick = MH["change_to_normal_title"](r, [(9008, "畅享尊贵")])
check("称谓: 只有 9008 → 不发(返回0)", pick == 0 and r.sent == [], "pick=%s" % pick)

r = FakeRobot()
pick = MH["change_to_normal_title"](r, [(9008, "畅享尊贵"), (3, "初显锋芒")])
pick2 = MH["change_to_normal_title"](r)   # 不传参 → 用缓存(用卡后场景)
check("称谓: 用卡后不传参复用缓存", pick2 in (3,) and len(r.sent) == 2,
      "pick2=%s sent=%s" % (pick2, r.sent))

r = FakeRobot()
pick = MH["change_to_normal_title"](r, [])
check("称谓: 无任何已拥有 → 不发", pick == 0 and r.sent == [])

# ---------------------------------------------------------------------------
# ⑦ 静态接线断言(防回归: 过滤/保护/日志/配置)
# ---------------------------------------------------------------------------
check("配置: robot_use_buff_items 恢复为 4 张卡",
      "robot_use_buff_items = [190016, 190018, 190033, 190039]" in cfg_src)
check("配置: robot_drop_item_quality 注释新语义",
      "同等级品质下限" in cfg_src and "不再单独决定丢弃" in cfg_src)

i_drop = as_src.find("2) 丢弃多余的低等级装备")
i_pos = as_src.find("bag_ops._is_in_bag_pos(item)", i_drop)
i_prot = as_src.find("bag_ops.DROP_PROTECT_INDEXES", i_drop)
i_judge = as_src.find("bag_ops.should_drop_equip(", i_drop)
i_send = as_src.find("protocol3.C2S_DELITEM", i_drop)
check("auto_summon 丢弃: pos 过滤/保护名单 → 判据 → 发包 顺序正确",
      0 < i_drop < i_prot < i_pos < i_judge < i_send,
      "idx=%s/%s/%s/%s/%s" % (i_drop, i_pos, i_prot, i_judge, i_send))
check("auto_summon 丢弃: 旧判据(quality 阈值/未知品质跳过)已移除",
      'if q >= drop_q:' not in as_src and 'if m.get("quality") is None:' not in as_src)
check("auto_summon: 用卡成功后调用 _schedule_title_restore",
      as_src.find("_schedule_title_restore(st, now_ms)")
      < as_src.find("丢弃多余的低等级装备") and "_schedule_title_restore(st, now_ms)" in as_src)
check("auto_summon tick: 调用 _try_title_restore",
      "_try_title_restore(robot_object, st, now_ms)" in as_src)
check("auto_summon 丢弃日志带 lv/quality",
      "丢弃多余低等级装备 %s(lv=%s quality=%s)" in as_src)
check("bag_ops 换装日志带 lv/quality",
      "穿装备 %s(lv=%s quality=%s, 部位=%s)" in bag_src
      and "丢弃多余装备 %s(lv=%s quality=%s, 部位=%s)" in bag_src)
check("bag_ops 存在统一判据函数族",
      all(("def %s(" % n) in bag_src for n in
          ("equip_score", "score_gt", "score_lt", "collect_best_equipped",
           "should_drop_equip", "parse_equip_lv")))
check("msghandle: title_load_handle 复用 change_to_normal_title",
      "change_to_normal_title(robot_object, pairs, owned)" in mh_src)

q_src = load("quest_engine.py")
check("quest_engine: m_bag_cache 条目带 pos(__item_pos)",
      "def __item_pos(item)" in q_src
      and "_bag[item_index] = [item_id, count, __item_pos(item)]" in q_src)

print()
print("结果: 失败 %d 项" % fails)
sys.exit(1 if fails else 0)
