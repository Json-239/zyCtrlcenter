# -*- coding: utf-8 -*-
"""追捕令全系列丢弃自检 —— 2026-09-23

用户口径: "背包里面的追捕令都丢弃"。

背景: `bag_ops.auto_equip_best_items` 步骤 0 原有丢弃逻辑**只匹配 110173**
"积满灰尘的追捕令", 而现场多个号 (robot0001000/1001/1002) 背包里还压着
190077 35级 / 190078 45级 / 190079 55级 各 5~13 个。背包有追捕令会让服务端在
点**钟馗**时反复弹小三藏"追捕令引导"对话 → 抓鬼领助战令/接任务卡住。

本自检断言(共 6 组):
  ① 源码形状: 常量/函数齐备; 步骤 0 改为调用 drop_pursuit_orders(不再写死 110173);
     安全检查顺序 = 保护名单 → 是追捕令 → 背包位置 → _DROP_GUARD;
  ② 纯函数: 名字判据(全系列命中, "过时的通缉令"不命中) / 编号段兜底
     (段内 True, **190067/190068 扳指必须 False**) / 名字优先于编号段;
  ③ 与机器人端 xml/item.xml 对齐(强断言):
     - xml 里每一条名字含"追捕令"的条目 → is_pursuit_order_item 必须 True(不漏);
     - 编号段覆盖到的 xml 条目 → 必须全部是追捕令(不误伤, 如 190067/190068 扳指);
  ④ 端到端行为(桩机器人 + 桩 protocol3): 背包里各等级追捕令全部发 C2S_DELITEM
     并从 bag 移除; 保护名单/非背包位置/数量<=0 一律不动;
  ⑤ 防重复闸门: 同一实例第二次调用不再发包(共享 _DROP_GUARD);
  ⑥ 兜底重试 tick(2026-09-23 追加): 按号节流 / 战斗与对话闸门 / 无追捕令时不占
     节流也不发包 / 挂载点 main_tester.tester 存在。

用法:
  python tools/bag_ops_pursuit_order_selftest.py <script 目录 或 bag_ops.py 路径>
"""
import os
import re
import sys


def _dir_of(path):
    return path if os.path.isdir(path) else os.path.dirname(path)


def _extract_func(src, name):
    lines = src.splitlines()
    for i, ln in enumerate(lines):
        if ln.startswith("def %s(" % name):
            out = [ln]
            for ln2 in lines[i + 1:]:
                if ln2.strip() and not ln2[0].isspace():
                    break
                out.append(ln2)
            return "\n".join(out)
    return None


def _extract_const(src, name):
    m = re.search(r"^%s\s*=\s*(.+?)\s*(?:#.*)?$" % re.escape(name), src, re.M)
    return m.group(1) if m else None


def _load_xml_pursuit(xml_path):
    """解析机器人端 xml/item.xml → (追捕令名条目 {idx: name}, 全部条目 {idx: name})。"""
    import xml.etree.ElementTree as ET
    all_items = {}
    want = {}
    if not os.path.exists(xml_path):
        return want, all_items
    tree = ET.parse(xml_path)
    for node in tree.getroot().findall("item_entry"):
        try:
            idx = int(node.get("item_index"))
        except Exception:
            continue
        name = node.get("item_name") or ""
        all_items[idx] = name
        if "追捕令" in name:
            want[idx] = name
    return want, all_items


def main():
    if len(sys.argv) < 2:
        print("用法: python tools/bag_ops_pursuit_order_selftest.py <script 目录 或 bag_ops.py 路径>")
        return 2
    script_dir = _dir_of(sys.argv[1])
    bag_path = os.path.join(script_dir, "bag_ops.py")
    if not os.path.exists(bag_path):
        print("[FAIL] 找不到 %s" % bag_path)
        return 2
    src = open(bag_path, encoding="utf-8", errors="replace").read()

    results = []

    def check(name, cond, detail=""):
        results.append((name, bool(cond), detail))

    # ---------------------------------------------------------------- ① 源码形状
    for fn in ("_item_name", "is_pursuit_order_name", "is_pursuit_order_index",
               "is_pursuit_order_item", "drop_pursuit_orders",
               "_meta_is_locked", "_pursuit_retry_blocked",
               "tick_pursuit_order_cleanup"):
        check("定义了 %s()" % fn, _extract_func(src, fn) is not None)
    kw = _extract_const(src, "PURSUIT_ORDER_NAME_KEYWORD")
    check("常量 PURSUIT_ORDER_NAME_KEYWORD = 追捕令", kw == '"追捕令"', "实际=%s" % kw)
    seg_src = _extract_const(src, "PURSUIT_ORDER_FALLBACK_SEGMENTS")
    check("常量 PURSUIT_ORDER_FALLBACK_SEGMENTS 存在", seg_src is not None)
    check("常量 _PURSUIT_RETRY_GAP_MS 存在", _extract_const(src, "_PURSUIT_RETRY_GAP_MS") is not None)
    # 挂载点: 抓鬼模式的号会 continue 跳过 quest_engine.tester, auto_summon.tick 只在
    # 空闲跑, 只有 main_tester.tester 的"与模式无关"区能覆盖一直在抓鬼的号。
    try:
        mt_src = open(os.path.join(script_dir, "main_tester.py"),
                      encoding="utf-8", errors="replace").read()
    except Exception:
        mt_src = ""
    check("main_tester.tester 挂了 tick_pursuit_order_cleanup",
          "tick_pursuit_order_cleanup(robot_object, now)" in mt_src
          and "import bag_ops" in mt_src,
          "未找到挂载点(抓鬼号不会重试)")

    aeb = _extract_func(src, "auto_equip_best_items") or ""
    check("步骤 0 调用 drop_pursuit_orders(robot_object, bag)",
          "drop_pursuit_orders(robot_object, bag)" in aeb)
    check("旧的写死判据 if item_index == 110173 已移除",
          "item_index == 110173" not in aeb,
          "仍存在硬编码 110173 判断")
    check("返回的丢弃数计入追捕令", "dropped + _drop_pursuit" in aeb)

    dpo = _extract_func(src, "drop_pursuit_orders") or ""
    # 安全检查顺序: 保护名单 → 是追捕令 → 背包位置 → 闸门
    def _pos_of(needle):
        i = dpo.find(needle)
        return i if i >= 0 else 10 ** 9
    order_pairs = [("is_protected_item(item_index)", "保护名单判断"),
                   ("is_pursuit_order_item(item_index)", "追捕令判据"),
                   ("_is_in_bag_pos(item)", "背包位置判断"),
                   ("item_recently_dropped(item[0])", "防重复闸门")]
    for needle, label in order_pairs:
        check("drop_pursuit_orders 含 %s" % label, needle in dpo)
    check("安全检查顺序 保护名单→追捕令→背包位置→闸门",
          _pos_of("is_protected_item(item_index)") < _pos_of("is_pursuit_order_item(item_index)")
          < _pos_of("_is_in_bag_pos(item)") < _pos_of("item_recently_dropped(item[0])"),
          "顺序: %s" % [(_pos_of(n), l) for n, l in order_pairs])
    check("丢弃时走 C2S_DELITEM + mark_item_dropped",
          "protocol3.C2S_DELITEM" in dpo and "mark_item_dropped(item[0])" in dpo)
    check("丢弃有日志(含物品名)",
          "_emit_log(" in dpo and "追捕令" in dpo and "_item_name(item_index)" in dpo)
    check("丢弃日志带实例 id 与加锁标记(诊断用)",
          "id=%s locked=%s" in dpo and "_meta_is_locked(robot_object, item_index)" in dpo)

    # ------------------------------------------------------------- ② 纯函数行为
    fnames = ("_item_name", "is_pursuit_order_name", "is_pursuit_order_index",
              "is_pursuit_order_item", "_meta_is_locked")
    fns = {}
    for f in fnames:
        fns[f] = _extract_func(src, f)
    ns = {}
    if all(fns.values()) and kw and seg_src:
        try:
            exec("PURSUIT_ORDER_NAME_KEYWORD = %s" % kw, ns)
            exec("PURSUIT_ORDER_FALLBACK_SEGMENTS = %s" % seg_src, ns)
            # _item_data 桩: 由外部测试注入(见下)
            ns["_item_data"] = lambda idx: ns.get("_FAKE_ITEM_DATA", {}).get(idx)
            ns["_FAKE_ITEM_DATA"] = {}
            for f in fnames:
                exec(fns[f], ns)
            check("exec 提取的纯函数可加载", True)
        except Exception as e:  # noqa
            check("exec 提取的纯函数可加载", False, "%s: %s" % (type(e).__name__, e))
            ns = {}

    name_fn = ns.get("is_pursuit_order_name")
    idx_fn = ns.get("is_pursuit_order_index")
    item_fn = ns.get("is_pursuit_order_item")

    if name_fn:
        for n in ("15级追捕令", "25级追捕令", "35级追捕令", "45级追捕令",
                  "55级追捕令", "65级追捕令", "积满灰尘的追捕令"):
            check("名字判据命中 %s" % n, name_fn(n) is True)
        for n in ("过时的通缉令", "象牙扳指", "白玉扳指", "金创药", "助战令", "", None):
            check("名字判据不命中 %r" % (n,), name_fn(n) is False)

    if idx_fn:
        seg = ns.get("PURSUIT_ORDER_FALLBACK_SEGMENTS") or ()
        check("编号段=3 段 (110173 + 190061-190066 + 190069-190098)",
              tuple(seg) == ((110173, 110173), (190061, 190066), (190069, 190098)),
              "实际=%s" % (tuple(seg),))
        for i in (110173, 190061, 190066, 190069, 190077, 190078, 190079, 190098):
            check("编号段兜底命中 %s" % i, idx_fn(i) is True)
        for i in (190067, 190068, 190060, 190099, 190100, 190146, 102007, 102010, 10):
            check("编号段不命中 %s" % i, idx_fn(i) is False)
        for bad in ("abc", None, [], 1.5):
            check("编号非法值 %r 保守 False" % (bad,), idx_fn(bad) is False)

    if item_fn:
        # 名字优先: 段外 index + 追捕令名字 → True(服务端新增等级自动覆盖, 不写死清单)
        ns["_FAKE_ITEM_DATA"] = {190099: {"item_name": "75级追捕令"},
                                 190061: {"item_name": "15级追捕令"},
                                 190067: {"item_name": "象牙扳指"},
                                 190068: {"item_name": "白玉扳指"},
                                 190146: {"item_name": "助战令"}}
        check("段外 190099 但名字=75级追捕令 → True(名字优先, 新等级不漏)",
              item_fn(190099) is True)
        check("段内 190061 名字=15级追捕令 → True", item_fn(190061) is True)
        check("段内 190067 名字=象牙扳指 → False(名字优先于编号段, 不误伤扳指)",
              item_fn(190067) is False)
        check("段内 190068 名字=白玉扳指 → False", item_fn(190068) is False)
        check("段内 190146 名字=助战令 → False(保护名单物品不因段误判)",
              item_fn(190146) is False)
        # 无名字(物品表未加载/缺条目) → 回退编号段
        ns["_FAKE_ITEM_DATA"] = {}
        check("查不到名 190077 → True(编号段兜底)", item_fn(190077) is True)
        check("查不到名 190067 → False(编号段外, 保守不丢)", item_fn(190067) is False)
        check("查不到名 110173 → True(编号段兜底)", item_fn(110173) is True)

    # --------------------------------------------- ③ 与机器人端 item.xml 对齐(强断言)
    want, all_items = _load_xml_pursuit(os.path.join(script_dir, "xml", "item.xml"))
    if not all_items:
        check("读取 script/xml/item.xml", False, "文件缺失或为空")
    else:
        check("读取 script/xml/item.xml", True)
        check("xml 里存在追捕令条目(应 37 条)", len(want) == 37, "实际 %d 条" % len(want))
        if item_fn:
            miss = []
            for idx in sorted(want):
                ns["_FAKE_ITEM_DATA"] = {i: {"item_name": n} for i, n in all_items.items()}
                if item_fn(idx) is not True:
                    miss.append(idx)
            check("xml 中每条追捕令 → is_pursuit_order_item 均 True(不漏)", not miss,
                  "漏判 %s" % miss)
        if idx_fn:
            bad = []
            seg = ns.get("PURSUIT_ORDER_FALLBACK_SEGMENTS") or ()
            for i in sorted(all_items):
                if any(lo <= i <= hi for lo, hi in seg) and "追捕令" not in all_items[i]:
                    bad.append((i, all_items[i]))
            check("编号段覆盖到的 xml 条目全是追捕令(不误伤)", not bad,
                  "误伤 %s" % bad)
            # 段内条目若不认识名字(离线场景) 也必须真的是追捕令
            seg_hit = [i for i in sorted(all_items)
                       if any(lo <= i <= hi for lo, hi in seg)]
            check("段内已存在条目 %d 件全部命中名字判据" % len(seg_hit),
                  all("追捕令" in all_items[i] for i in seg_hit))

    # ------------------------------------------------- ④/⑤ 端到端行为(桩机器人)
    if dpo:
        try:
            import types
            n2 = {}
            n2["PURSUIT_ORDER_NAME_KEYWORD"] = ns.get("PURSUIT_ORDER_NAME_KEYWORD", "追捕令")
            n2["PURSUIT_ORDER_FALLBACK_SEGMENTS"] = ns.get(
                "PURSUIT_ORDER_FALLBACK_SEGMENTS", ((110173, 110173),))
            n2["_item_data"] = lambda idx: n2["_FAKE"].get(idx)
            n2["_FAKE"] = {}
            for f in fnames:
                exec(fns[f], n2)
            # 真闸门(与 bag_ops 同一实现, 直接抽源码)
            for f in ("_now_ms", "item_recently_dropped", "mark_item_dropped"):
                exec(_extract_func(src, f), n2)
            exec("_DROP_GUARD = {}", n2)
            exec("_DROP_GUARD_MS = %s" % _extract_const(src, "_DROP_GUARD_MS"), n2)
            # 真保护名单判据(仅用编号名单那一层: 蛋判据需要 client, 这里桩掉)
            protect = set(eval(_extract_const(src, "DROP_PROTECT_INDEXES")))
            n2["_PROTECT"] = protect
            n2["is_protected_item"] = lambda idx: idx in n2["_PROTECT"]
            # 真位置判据
            exec("POS_BAG_BEGIN = %s" % _extract_const(src, "POS_BAG_BEGIN"), n2)
            exec("POS_BAG_END = %s" % _extract_const(src, "POS_BAG_END"), n2)
            exec(_extract_func(src, "_is_in_bag_pos"), n2)
            logs = []
            n2["_emit_log"] = lambda ro, lv, msg: logs.append((lv, msg))
            proto = types.ModuleType("protocol3")
            proto.C2S_DELITEM = "C2S_DELITEM"
            n2["protocol3"] = proto
            exec(dpo, n2)
            drop_fn = n2["drop_pursuit_orders"]

            class StubRobot(object):
                def __init__(self):
                    self.sent = []

                def send_message(self, key, args):
                    self.sent.append((key, tuple(args)))

            def mkbag(spec):
                # spec: {index: (item_id, count, pos)}
                return dict((k, list(v)) for k, v in spec.items())

            n2["_FAKE"] = {190077: {"item_name": "35级追捕令"},
                           190078: {"item_name": "45级追捕令"},
                           190079: {"item_name": "55级追捕令"},
                           110173: {"item_name": "积满灰尘的追捕令"},
                           190146: {"item_name": "助战令"},
                           102007: {"item_name": "金创药"},
                           213204: {"item_name": "断玉刀"},
                           225601: {"item_name": "贝壳项链"}}
            n2["_DROP_GUARD"].clear()

            # ④-1 各等级追捕令全丢
            bag = mkbag({190077: (7701, 13, 8203), 190078: (7801, 9, 8206),
                         190079: (7901, 9, 8202), 110173: (17301, 1, 8210)})
            ro = StubRobot()
            n = drop_fn(ro, bag)
            check("④ 各等级追捕令全丢(4 件)", n == 4, "n=%s" % n)
            check("④ 发了 4 条 C2S_DELITEM",
                  len(ro.sent) == 4 and all(k == "C2S_DELITEM" for k, _ in ro.sent),
                  "sent=%s" % ro.sent)
            check("④ 参数是实例 item_id(item[0])",
                  sorted(a[0] for _, a in ro.sent) == [7701, 7801, 7901, 17301],
                  "sent=%s" % ro.sent)
            check("④ 背包条目已移除", not bag, "剩余=%s" % bag)
            check("④ 日志含物品名", any("35级追捕令" in m for _, m in logs), "logs=%s" % logs)

            # ④-2 保护名单最前: 即便名字/段都像追捕令也不动
            n2["_DROP_GUARD"].clear()
            n2["_PROTECT"] = protect | {190077}
            bag = mkbag({190077: (7702, 13, 8203), 190146: (14601, 3, 8200),
                         102007: (2007, 994, 8192)})
            ro = StubRobot()
            n = drop_fn(ro, bag)
            check("④ 保护名单物品一件不动(含名字像追捕令的保护物)", n == 0 and not ro.sent,
                  "n=%s sent=%s" % (n, ro.sent))
            check("④ 保护名单物品仍在背包", set(bag.keys()) == {190077, 190146, 102007})
            n2["_PROTECT"] = protect
            n2["_DROP_GUARD"].clear()

            # ④-3 非背包位置(装备栏 4108 / 包裹栏 25617 / 位置未知) 一律不动
            bag = mkbag({190077: (7703, 13, 4108), 190078: (7803, 9, 25617),
                         190079: (7903, 9, None)})
            ro = StubRobot()
            n = drop_fn(ro, bag)
            check("④ 非背包位置不动(装备栏/包裹栏/位置未知)", n == 0 and not ro.sent
                  and len(bag) == 3, "n=%s sent=%s" % (n, ro.sent))

            # ④-4 非追捕令装备/普通物品不动
            bag = mkbag({213204: (21301, 1, 8211), 225601: (22501, 1, 8212)})
            ro = StubRobot()
            n = drop_fn(ro, bag)
            check("④ 非追捕令物品不动", n == 0 and not ro.sent and len(bag) == 2)

            # ④-5 数量<=0 / 空 item / None → 跳过不崩
            bag = {190077: [7704, 0, 8203], 190078: None, 190079: [7904]}
            ro = StubRobot()
            try:
                n = drop_fn(ro, bag)
                check("④ 数量0/None/短列表 不抛异常且不误丢", n == 0 and not ro.sent,
                      "n=%s sent=%s" % (n, ro.sent))
            except Exception as e:  # noqa
                check("④ 数量0/None/短列表 不抛异常且不误丢", False,
                      "%s: %s" % (type(e).__name__, e))
            check("④ 空背包返回 0", drop_fn(StubRobot(), {}) == 0)
            check("④ bag=None 返回 0(不抛异常)", drop_fn(StubRobot(), None) == 0)

            # ⑤ 防重复闸门: 同一实例第二次不发包
            n2["_DROP_GUARD"].clear()
            bag = mkbag({190077: (7705, 13, 8203)})
            ro = StubRobot()
            n1 = drop_fn(ro, bag)
            bag2 = mkbag({190077: (7705, 13, 8203)})   # 服务端未确认, 本地又收到旧快照
            n2v = drop_fn(ro, bag2)
            check("⑤ 首次丢 1 件", n1 == 1)
            check("⑤ 同一实例第二次被闸门拦下(不重复发包)",
                  n2v == 0 and len(ro.sent) == 1, "n2=%s sent=%s" % (n2v, ro.sent))
            check("⑤ 闸门记录了该实例", 7705 in n2["_DROP_GUARD"])
            # 反例: 清闸门(等于不过闸) → 会重复丢
            n2["_DROP_GUARD"].clear()
            ro2 = StubRobot()
            drop_fn(ro2, mkbag({190077: (7705, 13, 8203)}))
            n2["_DROP_GUARD"].clear()
            drop_fn(ro2, mkbag({190077: (7705, 13, 8203)}))
            check("⑤ 反例: 不过闸会重复丢 2 次(说明闸门有效)",
                  len(ro2.sent) == 2, "sent=%s" % ro2.sent)

            # ------------------------------------------- ⑥ 兜底重试 tick(桩)
            exec(_extract_func(src, "_pursuit_retry_blocked"), n2)
            exec(_extract_func(src, "tick_pursuit_order_cleanup"), n2)
            n2["_PURSUIT_RETRY_GAP_MS"] = int(eval(_extract_const(src, "_PURSUIT_RETRY_GAP_MS")))
            n2["_PURSUIT_RETRY_LAST"] = {}
            tick_fn = n2["tick_pursuit_order_cleanup"]
            gap = n2["_PURSUIT_RETRY_GAP_MS"]
            # quest_state 桩: exec 环境里 import quest_state 拿不到真模块(script 目录
            # 不在 sys.path), 这里临时注册一个只有 ST_DIALOG 的假模块。
            import types as _types
            _qs = _types.ModuleType("quest_state")
            _qs.ST_DIALOG = "DIALOG"
            _saved_qs = sys.modules.get("quest_state", None)
            sys.modules["quest_state"] = _qs

            class StubBagRobot(object):
                def __init__(self, bag, account="acc", fight=False, quest=None):
                    self.sent = []
                    self.m_bag_cache = bag
                    self.m_account = (account,)
                    self.m_fight_state = fight
                    self.m_quest = quest

                def send_message(self, key, args):
                    self.sent.append((key, tuple(args)))

            class _Q(object):
                def __init__(self, state):
                    self.state = state

            n2["_DROP_GUARD"].clear()
            n2["_PURSUIT_RETRY_LAST"].clear()
            # ⑥-1 正常重试: 丢 1 件; 60s 内再 tick(服务端未删、又收到旧快照)被节流
            ro3 = StubBagRobot(mkbag({190077: (8801, 5, 8203)}), account="accT")
            ro3.m_bag_meta = {190077: {"is_locked": True}}	# 只为验证日志里的加锁标记
            t1 = tick_fn(ro3, 1000000)
            ro3.m_bag_cache = mkbag({190077: (8807, 5, 8203)})	# 服务端没删, 又收到旧快照
            t2 = tick_fn(ro3, 1001000)
            check("⑥ 首次 tick 丢弃 1 件", t1 == 1 and len(ro3.sent) == 1,
                  "t1=%s sent=%s" % (t1, ro3.sent))
            check("⑥ 60s 内第二次 tick 被节流(不发包)", t2 == 0 and len(ro3.sent) == 1,
                  "t2=%s sent=%s" % (t2, ro3.sent))
            check("⑥ 丢弃日志带加锁标记(诊断用)",
                  any("locked=是" in m for _, m in logs), "logs=%s" % logs[-2:])
            # ⑥-2 节流到期后可再试(换新实例 id, 避开丢弃闸门)
            ro3.m_bag_cache = mkbag({190077: (8806, 5, 8203)})
            t3 = tick_fn(ro3, 1000000 + gap + 1)
            check("⑥ 节流到期后再试(新实例)", t3 == 1 and len(ro3.sent) == 2, "t3=%s" % t3)
            # ⑥-3 战斗状态不试(服务端 FightRoleStatus 必拒, 省发包)
            ro4 = StubBagRobot(mkbag({190077: (8802, 5, 8203)}), account="accF", fight=True)
            check("⑥ 战斗中不重试", tick_fn(ro4, 2000000) == 0 and not ro4.sent)
            # ⑥-4 对话状态不试(DialogRoleStatus 必拒)
            ro5 = StubBagRobot(mkbag({190077: (8803, 5, 8203)}), account="accD",
                               quest=_Q("DIALOG"))
            check("⑥ 对话中不重试", tick_fn(ro5, 3000000) == 0 and not ro5.sent)
            # ⑥-5 没追捕令: 不发包、也不占节流(紧接着放入追捕令, 同一秒就能丢)
            ro6 = StubBagRobot(mkbag({213204: (8804, 1, 8211)}), account="accN")
            t5 = tick_fn(ro6, 4000000)
            ro6.m_bag_cache = mkbag({190077: (8805, 2, 8204)})
            t6 = tick_fn(ro6, 4000500)
            check("⑥ 无追捕令返回 0 且不占节流", t5 == 0 and t6 == 1,
                  "t5=%s t6=%s sent=%s" % (t5, t6, ro6.sent))
            # ⑥-6 空/None 背包不崩
            check("⑥ m_bag_cache 空或 None 返回 0",
                  tick_fn(StubBagRobot(None, account="accE"), 5000000) == 0
                  and tick_fn(StubBagRobot({}, account="accE2"), 5000000) == 0)
            if _saved_qs is not None:
                sys.modules["quest_state"] = _saved_qs
            else:
                sys.modules.pop("quest_state", None)
        except Exception as e:  # noqa
            check("④⑤⑥ 端到端桩测试可执行", False, "%s: %s" % (type(e).__name__, e))

    nfail = 0
    for name, ok, detail in results:
        if ok:
            print("[PASS] %s" % name)
        else:
            nfail += 1
            print("[FAIL] %s %s" % (name, ("(%s)" % detail) if detail else ""))
    print("=== 结果: %s (共 %d 项断言, 失败 %d 项) ===" % (
        "PASS" if nfail == 0 else "FAIL", len(results), nfail))
    return 0 if nfail == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
