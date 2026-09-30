# -*- coding: utf-8 -*-
"""物品三问题修复自检（2026-09-29）——装备被脱/粗布包只装1个/拾取栏不清

背景（现场实证）:
  ① daily_ghost.__tidy_bag 遍历 quest.bag 无位置过滤, 把装备栏里已穿的装备
     也当"可消耗品"发 C2S_USE_ITEM → 服务端对装备栏物品 use=解下(脱下) →
     装备在背包/装备栏之间低频来回跳(兽牙坠实例 8243↔4103 实测), 抽查大量号
     "身上没装备"; 且装备在机器人端 xml 无 item_handler, 旧"装备物品"分支永不命中。
  ② bag_ops.auto_use_bag_expanders 只在登录首次跑一次 + 发完 pop + 按 item_index
     覆盖 → 现场 41 个号背包里粗布包没装。
  ③ 机器人从未发 C2S_PICK_ALL_ITEM(80125) → 临时拾取栏(PICK 0x6450~0x64ff)
     物品 5 分钟过期消失。

本自检:
  A 源码形状: 六文件关键改动存在(含"不得再有"的反向断言);
  B tick_pick_pocket_cleanup 行为(mock): 发现即发/节流/战斗跳过/无进展退避;
  C auto_use_bag_expanders 行为(mock): 背包区判据/不 pop/extra_ids/节流/上限;
  D 坏版灵敏度: 抽掉关键条件后对应断言必须失败。

用法:
  python tools/item_pouch_fix_selftest.py <script 目录>
"""
import re
import sys
import types


def _read(path):
    return open(path, encoding="utf-8", errors="replace").read()


def _extract_func(src, name):
    m = re.search(r"^def %s\(.*?(?=\n\S|\Z)" % re.escape(name), src, re.S | re.M)
    return m.group(0) if m else None


def _extract_const_line(src, name):
    m = re.search(r"^%s\s*=.*$" % re.escape(name), src, re.M)
    return m.group(0) if m else None


def main():
    if len(sys.argv) < 2:
        print("用法: python tools/item_pouch_fix_selftest.py <script 目录>")
        return 2
    sd = sys.argv[1].rstrip("/\\")

    def p(name):
        return sd + "/" + name

    try:
        dg = _read(p("daily_ghost.py"))
        bo = _read(p("bag_ops.py"))
        mh = _read(p("msghandle.py"))
        mt = _read(p("main_tester.py"))
        p3 = _read(p("protocol3.py"))
        ec = _read(p("encourage_claim.py"))
    except Exception as e:
        print("[FAIL] 读取脚本目录失败: %s" % e)
        return 2

    results = []

    def check(name, cond, detail=""):
        results.append((name, bool(cond), detail))

    # ---------------- A. 源码形状 ----------------
    # daily_ghost.__tidy_bag
    i_tidy = dg.find("def __tidy_bag(")
    j_tidy = dg.find("\ndef ", i_tidy + 1) if i_tidy >= 0 else -1
    seg_tidy = dg[i_tidy:j_tidy] if (i_tidy >= 0 and j_tidy > i_tidy) else ""
    check("A1 tidy: 只处理背包区(0x2000-0x3000 pos 过滤)存在",
          "not (0x2000 <= _pos < 0x3000)" in seg_tidy)
    check("A2 tidy: 装备双判据(_get_equip_slot + meta rel)存在",
          "_get_equip_slot(_idx)" in seg_tidy and 'startswith("element_")' in seg_tidy)
    check("A3 tidy: 扩容包跳过(_is_bag_expander)存在",
          "_is_bag_expander(_idx)" in seg_tidy)
    check("A4 tidy: 装备 to_drop/to_wear 分支已移除",
          "to_drop.append" not in seg_tidy and "to_wear.append" not in seg_tidy
          and "TIDY_BAG drop equip" not in seg_tidy and "TIDY_BAG wear equip" not in seg_tidy)
    check("A5 tidy: C2S_DROP_ITEM 不再出现(装备丢由 bag_ops 专链)",
          "C2S_DROP_ITEM" not in seg_tidy)
    check("A6 tidy: 数据源优先 m_bag_cache",
          'getattr(robot_object, "m_bag_cache", None) or quest.bag' in seg_tidy)
    # daily_ghost: 拾取栏清理
    check("A7 pick: tick_pick_pocket_cleanup 存在且发 C2S_PICK_ALL_ITEM",
          "def tick_pick_pocket_cleanup(" in dg
          and "send_message(protocol3.C2S_PICK_ALL_ITEM, [])" in dg)
    check("A8 pick: PICK 区判据(0x6450-0x6500)",
          "0x6450 <= _p < 0x6500" in dg)
    check("A9 pick: 战斗中/摆摊中跳闸存在",
          "lock_pocket(PICK)" in dg and "摆摊中不动背包/拾取" in dg)
    # daily_ghost: 元数据 rel 解析
    check("A10 meta: __load_item_meta 解析 related_element_index(rel)",
          '"use_on_summon": False, "rel": ""' in dg
          and 'info["rel"] = rel.text.strip()' in dg)
    # bag_ops
    check("B1 bag_ops: POS_PICK/ POS_TROUGH 常量存在",
          "POS_PICK_BEGIN = 0x6450" in bo and "POS_TROUGH_BEGIN = 0x6410" in bo)
    i_bx = bo.find("def auto_use_bag_expanders(")
    j_bx = bo.find("\ndef ", i_bx + 1) if i_bx >= 0 else -1
    if j_bx <= 0:
        j_bx = len(bo)   # 该函数是文件末尾(2026-09-29 起), 截到文件尾
    seg_bx = bo[i_bx:j_bx] if i_bx >= 0 else ""
    check("B2 扩容: extra_ids 参数与合并逻辑存在",
          "extra_ids=None" in seg_bx and "for _pair in (extra_ids or [])" in seg_bx)
    check("B3 扩容: 不再 pop(修复只装1个)",
          "bag.pop(item_index, None)" not in seg_bx)
    check("B4 扩容: 按号节流+会话上限存在",
          "_BAGX_MIN_GAP_MS" in seg_bx and "_BAGX_MAX_PER_SESSION" in seg_bx)
    # msghandle
    check("C1 msghandle: 原始列表收集 _bagx_raw 并传 extra_ids",
          "_bagx_raw.append((_ii, _iid))" in mh
          and "extra_ids=_bagx_raw" in mh)
    check("C2 msghandle: 扩容包调用已移出登录一次性(在 bag_opened 分支外)",
          mh.find("extra_ids=_bagx_raw") > mh.find('robot_object.bag_opened = True'))
    # main_tester
    check("C3 main_tester: 已挂 tick_pick_pocket_cleanup",
          "_dg.tick_pick_pocket_cleanup(robot_object, now)" in mt)
    # protocol3
    check("C4 protocol3: C2S_PICK_ALL_ITEM 别名+格式注册",
          "C2S_PICK_ALL_ITEM = protocol2.c2s_key.C2S_PICK_ALL_ITEM" in p3
          and "FORMAT_MC[C2S_PICK_ALL_ITEM] = protocol2.get_c2s_format(C2S_PICK_ALL_ITEM)" in p3)
    # encourage_claim
    check("C5 encourage: _use_boxes 背包区 pos 过滤存在",
          "not (0x2000 <= _pos < 0x3000)" in ec)

    # ---------------- B. tick_pick_pocket_cleanup 行为(mock) ----------------
    fpick = _extract_func(dg, "tick_pick_pocket_cleanup")
    check("B5 提取 tick_pick_pocket_cleanup 源码", fpick is not None)
    if fpick:
        ns = {}
        consts = "\n".join(x for x in (
            _extract_const_line(dg, "_PICK_CLEAN_MS"),
            _extract_const_line(dg, "_PICK_STUCK_LIMIT"),
            _extract_const_line(dg, "_PICK_BACKOFF_MS"),
            _extract_const_line(dg, "_PICK_FULL_SIGNAL_MS")) if x)
        logs = []
        sent = []
        try:
            exec(consts + "\n" + fpick, ns)
            ns["__now_ms"] = lambda: 0
            ns["__log"] = lambda ro, lv, msg: logs.append(msg)
            ns["diag"] = types.SimpleNamespace(log=lambda m: None)
            ns["protocol3"] = types.SimpleNamespace(C2S_PICK_ALL_ITEM=80125)

            class R(object):
                def __init__(self, bag, fight=False, booth=False, full_ms=0):
                    self.m_logined = True
                    self.m_fight_state = fight
                    self.m_bag_cache = bag
                    self.m_account = ["test@x"]
                    if full_ms:
                        self.m_bag_full_ms = full_ms
                    if booth:
                        self.m_booth = types.SimpleNamespace(enabled=True)
                def send_message(self, mid, args):
                    sent.append((mid, tuple(args)))
                    return 0

            # B5.1 无 PICK 物品 → 不发
            # 注: now_ms 用真实量级时间戳(毫秒), 否则首次调用会被 60s 节流挡住(测试陷阱)
            T0 = 1800000000000
            r = R({1: [11, 1, 8192]})
            ok = ns["tick_pick_pocket_cleanup"](r, T0)
            check("B5.1 无 PICK 物品不发", (ok is False) and len(sent) == 0)
            # B5.2 有 PICK 物品 → 发 1 条 80125
            r = R({1001: [111, 1, 0x6452]})
            ok = ns["tick_pick_pocket_cleanup"](r, T0)
            check("B5.2 有 PICK 物品发 1 条(80125, 空参)",
                  (ok is True) and sent == [(80125, ())])
            # B5.3 节流 30s 内不发
            ok = ns["tick_pick_pocket_cleanup"](r, T0 + 30000)
            check("B5.3 60s 节流内不发", (ok is False) and len(sent) == 1)
            # B5.4 战斗中不发
            r2 = R({1001: [111, 1, 0x6452]}, fight=True)
            ok = ns["tick_pick_pocket_cleanup"](r2, T0 + 200000)
            check("B5.4 战斗中不发", (ok is False) and len(sent) == 1)
            # B5.5 摆摊中不发
            r3 = R({1001: [111, 1, 0x6452]}, booth=True)
            ok = ns["tick_pick_pocket_cleanup"](r3, T0 + 300000)
            check("B5.5 摆摊中不发", (ok is False) and len(sent) == 1)
            # B5.6 无进展(数量不减) → 达到上限后退避
            t = T0
            r4 = R({1001: [111, 1, 0x6452], 1002: [222, 1, 0x6453]})
            oks = []
            for _i in range(6):
                t += 61000
                oks.append(ns["tick_pick_pocket_cleanup"](r4, t))
            st = getattr(r4, "m_pick_clean", {})
            check("B5.6 连续无进展 → 第6次不发并退避(backoff 10分钟)",
                  oks[:5] == [True] * 5 and oks[5] is False
                  and int(st.get("backoff_until", 0)) >= t)
            t2 = int(st.get("backoff_until", 0)) - 1000   # 退避期内(未到期)
            ok = ns["tick_pick_pocket_cleanup"](r4, t2)
            check("B5.7 退避期内不发", ok is False)
            # B5.8 数量减少/清空 → stuck 复位(重新可发)
            r5 = R({1001: [111, 1, 0x6452]})
            ns["tick_pick_pocket_cleanup"](r5, T0)  # 第1次
            r5.m_bag_cache = {}                      # 物品已被清空
            ok = ns["tick_pick_pocket_cleanup"](r5, T0 + 61000)
            st5 = getattr(r5, "m_pick_clean", {})
            check("B5.8 PICK 清空后不再发且计数复位",
                  ok is False and int(st5.get("stuck", -1)) == 0)
            # B5.9 第二信号: 无 PICK 物品但近期有 839 包满转移 → 仍发一次
            r6 = R({1: [11, 1, 8192]}, full_ms=T0 - 60000)   # 1 分钟前有包满信号
            sn0 = len(sent)
            ok = ns["tick_pick_pocket_cleanup"](r6, T0)
            check("B5.9 无 PICK 物品但近期 839 信号 → 发一次",
                  (ok is True) and len(sent) == sn0 + 1)
            # B5.10 839 信号过期(>10分钟) 且无 PICK 物品 → 不发
            r7 = R({1: [11, 1, 8192]}, full_ms=T0 - 3600000)  # 1 小时前
            sn1 = len(sent)
            ok = ns["tick_pick_pocket_cleanup"](r7, T0)
            check("B5.10 839 信号过期且无 PICK 物品 → 不发",
                  (ok is False) and len(sent) == sn1)
        except Exception as e:
            check("B5.0 mock 执行 tick_pick_pocket_cleanup", False, str(e))

    # ---------------- C. auto_use_bag_expanders 行为(mock) ----------------
    fbx = _extract_func(bo, "auto_use_bag_expanders")
    check("C6 提取 auto_use_bag_expanders 源码", fbx is not None)
    if fbx:
        # 依赖: _now_ms/_is_in_bag_pos/_is_bag_expander + 常量; client 用假表注入 sys.modules
        f_now = _extract_func(bo, "_now_ms")
        f_pos = _extract_func(bo, "_is_in_bag_pos")
        f_exp = _extract_func(bo, "_is_bag_expander")
        consts = "\n".join(x for x in (
            _extract_const_line(bo, "POS_BAG_BEGIN"),
            _extract_const_line(bo, "POS_BAG_END"),
            _extract_const_line(bo, "BAG_EXPAND_KEYWORDS"),
            _extract_const_line(bo, "BAG_EXPAND_EXCLUDE_INDEXES"),
            _extract_const_line(bo, "_BAGX_GUARD"),
            _extract_const_line(bo, "_BAGX_MIN_GAP_MS"),
            _extract_const_line(bo, "_BAGX_MAX_PER_SESSION"),
            _extract_const_line(bo, "_BAGX_SESSION_RESET_MS"),
            _extract_const_line(bo, "_BAGX_MAX_SEND_PER_ROUND"),
            "C2S_USEITEM = 80218") if x)
        exps = []
        try:
            if f_now and f_pos and f_exp:
                old = sys.modules.get("client", None)
                sys.modules["client"] = types.SimpleNamespace(g_item_data_dict={
                    172102: {"item_name": "粗布包"},
                    102007: {"item_name": "金创药"},
                    108000: {"item_name": "轻便包裹"},
                })
                ns2 = {}
                exec(consts + "\n" + f_now + "\n" + f_pos + "\n" + f_exp + "\n" + fbx, ns2)
                ns2["protocol3"] = types.SimpleNamespace(C2S_USEITEM=80218)
                ns2["_emit_log"] = lambda ro, lv, msg: exps.append(msg)

                class R2(object):
                    def __init__(self):
                        self.m_account = ["bx@x"]
                    def send_message(self, mid, args):
                        exps.append((mid, tuple(args)))
                        return 0

                # C6.1 背包区扩容包 → 发且不 pop
                # 注: 时间用真实量级毫秒(1800000000000), 否则首调被 60s 节流挡(测试陷阱)
                T0 = 1800000000000
                r = R2()
                bag = {172102: [111, 1, 8192]}
                used = ns2["auto_use_bag_expanders"](r, bag, None, T0)
                check("C6.1 背包区扩容包发送且不 pop",
                      used == 1 and (172102 in bag)
                      and ((80218, (111,)) in exps))
                # C6.2 节流 60s 内不发
                used = ns2["auto_use_bag_expanders"](r, bag, None, T0 + 1000)
                check("C6.2 60s 节流内不发", used == 0)
                # C6.3 60s 后放行 + 包裹栏(非背包区)不处理
                used = ns2["auto_use_bag_expanders"](r, {172102: [111, 1, 25617]}, None, T0 + 61000)
                check("C6.3 非背包区(包裹栏)不发", used == 0)
                # C6.4 extra_ids(被覆盖的第二实例) → 一起发(上限2)
                used = ns2["auto_use_bag_expanders"](r, {172102: [111, 1, 8192]},
                                                     [(172102, 222)], T0 + 122000)
                check("C6.4 extra_ids 第二实例一起发(2条)",
                      used == 2 and (80218, (111,)) in exps and (80218, (222,)) in exps)
                # C6.5 会话上限: 已发 1+2=3; 再发到 8 后停
                t = T0 + 122000
                cnt = 0
                while cnt < 5:
                    t += 61000
                    u = ns2["auto_use_bag_expanders"](r, {172102: [111, 1, 8192]}, None, t)
                    cnt += 1
                    if u == 0:
                        break
                u2 = ns2["auto_use_bag_expanders"](r, {172102: [111, 1, 8192]}, None, t + 61000)
                check("C6.5 会话上限(8)后停发", u2 == 0)
                if old is not None:
                    sys.modules["client"] = old
            else:
                check("C6.0 提取依赖函数(_now_ms/_is_in_bag_pos/_is_bag_expander)", False)
        except Exception as e:
            check("C6.0 mock 执行 auto_use_bag_expanders", False, str(e))

    # ---------------- D. 坏版灵敏度 ----------------
    # 做法: 把断言抽成纯函数, 对原文与"抽掉关键条件"的变体分别求值, 必须一真一假。
    def tidy_pos_filter_ok(s):
        i = s.find("def __tidy_bag(")
        j = s.find("\ndef ", i + 1) if i >= 0 else -1
        seg = s[i:j] if (i >= 0 and j > i) else ""
        return "not (0x2000 <= _pos < 0x3000)" in seg

    def bx_throttle_ok(s):
        return "if t0 - int(rec[0]) < _BAGX_MIN_GAP_MS:" in s

    def pick_send_ok(s):
        return "send_message(protocol3.C2S_PICK_ALL_ITEM, [])" in s

    check("D1 灵敏度: 抽掉 tidy 背包区过滤 → 同款检查失败",
          tidy_pos_filter_ok(dg)
          and (not tidy_pos_filter_ok(dg.replace("not (0x2000 <= _pos < 0x3000)", "False"))))
    check("D2 灵敏度: 抽掉扩容节流 → 同款检查失败",
          bx_throttle_ok(bo)
          and (not bx_throttle_ok(bo.replace(
              "if t0 - int(rec[0]) < _BAGX_MIN_GAP_MS:", "if False:"))))
    check("D3 灵敏度: 抽掉拾取栏发送 → 同款检查失败",
          pick_send_ok(dg)
          and (not pick_send_ok(dg.replace(
              "send_message(protocol3.C2S_PICK_ALL_ITEM, [])", "pass"))))
    check("D4 灵敏度: tidy 内不含对装备栏 pos(0x1000)的放行分支",
          "0x1000 <= _pos" not in seg_tidy and "POS_EQUIP_BEGIN <= _pos" not in seg_tidy)

    # ---------------- 输出 ----------------
    nfail = 0
    for name, ok, detail in results:
        if ok:
            print("[PASS] %s" % name)
        else:
            nfail += 1
            print("[FAIL] %s %s" % (name, ("(" + str(detail) + ")") if detail else ""))
    print("=== 结果: %s (共 %d 项断言, 失败 %d 项) ===" % ("PASS" if nfail == 0 else "FAIL", len(results), nfail))
    return 0 if nfail == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
