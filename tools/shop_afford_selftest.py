# -*- coding: utf-8 -*-
"""买药修复与穷号闸自检（2026-09-28）。

依据: docs/04-测试/实施spec-20260928-买药修复与穷号处置.md §3（P0-1/P0-2/P0-3 + R1）
      与 §4 自检用例点。

覆盖四段:
  A 静态: 源码形状（落点/常量/字段/顺序: 短路在 acquire 之前; reset 不清 streak;
           R1 闸在 ghost_start 分支最前; 实际下单量日志）。
  B 动态: shop_errand.start 真模块行为 —— 按 m_reserve 降量、可买数 0 返回 no_money
          且**不占锁/不调执行器**、reserve=0(未同步)放行。
          口径(2026-09-28 决策 B): 可买数=reserve//price; **≥2 才减 1 余量**
          (≥2 → 31167/211 = 146; =1 不减 → reserve 211~421 的准穷号可买 1 个自愈;
          =0 → no_money)。spec §3.2 用例点写的 147 是"未留余量"上限(147*211=31017)。
  C 动态: quest_engine.on_shop_notice_args(552) → notify_rejected + notify_failed
          (result=failed, reason 含 552 与 货币); 编排层 1 秒内可见。
  D 动态: daily_ghost.__heal_buy_tick 连败冷却 60/120/240/480/960/1800 封顶;
          买到药归零; GhostState.reset() 不清 buy_fail_streak。
  E 动态: daily_ghost.dispatch_cmd ghost_start 的 R1 兜底闸(源码截段真执行):
          穷(0<reserve<211=1 个药价)且无药 → no_money; 有药/未同步(0)/≥211 → 放行。

用法: python tools/shop_afford_selftest.py [script_dir]
"""
import os
import sys
import textwrap
import types

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_DIR = os.path.normpath(os.path.join(
    HERE, "..", "deploy", "zones", "prod-240-2300", "script"))

results = []


def check(name, cond, detail=""):
    results.append((name, bool(cond), detail))


def _resolve(path):
    if os.path.isdir(path):
        return path
    return os.path.dirname(path)


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


def _extract_class(src, name):
    lines = src.splitlines()
    for i, ln in enumerate(lines):
        if ln.startswith("class %s(" % name) or ln.startswith("class %s:" % name):
            out = [ln]
            for ln2 in lines[i + 1:]:
                if ln2.strip() and not ln2[0].isspace():
                    break
                out.append(ln2)
            return "\n".join(out)
    return None


def main():
    if len(sys.argv) >= 2:
        script_dir = _resolve(sys.argv[1])
    else:
        script_dir = DEFAULT_DIR
    se_path = os.path.join(script_dir, "shop_errand.py")
    qe_path = os.path.join(script_dir, "quest_engine.py")
    dg_path = os.path.join(script_dir, "daily_ghost.py")
    if not (os.path.exists(se_path) and os.path.exists(qe_path) and os.path.exists(dg_path)):
        print("[FAIL] script 目录缺文件: %s" % script_dir)
        return 2
    se_src = open(se_path, encoding="utf-8", errors="replace").read()
    qe_src = open(qe_path, encoding="utf-8", errors="replace").read()
    dg_src = open(dg_path, encoding="utf-8", errors="replace").read()

    if script_dir not in sys.path:
        sys.path.insert(0, script_dir)

    # ================================================================ A. 静态形状
    check("A1: shop_errand 可买数0 返回 no_money(不占锁)",
          '"result": "no_money"' in se_src and '"count": 0}' in se_src)
    check("A2: shop_errand 降量公式=可买数≥2 才减 1 余量(B 案)",
          "_raw = _reserve // _price" in se_src
          and "_cap = _raw - 1 if _raw >= 2 else _raw" in se_src)
    check("A3: 降量短路在 acquire 之前(不占锁)",
          0 < se_src.find('"result": "no_money"') < se_src.find("ok, held = acquire("))
    check("A4: reserve=0 视为未同步不拦(_reserve > 0 才降量)",
          "if _price > 0 and _reserve > 0:" in se_src)
    check("A5: quest_engine 552 拒绝回传(notify_rejected + notify_failed)",
          "_se.notify_rejected(robot_object, _nid, _why)" in qe_src
          and '_se.notify_failed(robot_object, "服务端拒绝:%s(码 %d)" % (_why, _nid))' in qe_src)
    check("A6: daily_ghost 常量 GHOST_RESERVE_FLOOR = 211(方案甲自愈线)",
          "GHOST_RESERVE_FLOOR = 211" in dg_src)
    check("A7: GhostState 新增 buy_fail_streak 字段",
          "self.buy_fail_streak = 0" in dg_src)
    _frag_reset = _extract_func(dg_src, "reset") or ""
    check("A8: buy_fail_streak 不随 reset 清(跨会话累计)",
          "buy_fail_streak" not in _frag_reset)
    check("A9: 失败分支指数冷却(封顶 1800)",
          "min(1800, 60 * (2 ** min(g.buy_fail_streak - 1, 5)))" in dg_src)
    check("A10: no_money 分支 30 分钟长冷却(与升级冷却取较大值)",
          "30 * 60 * 1000" in dg_src and "_cd30 > int(getattr(g, \"heal_cooldown_until\", 0) or 0)" in dg_src)
    check("A11: 买药启动日志打印实际下单量(_res.get(\"count\", _cnt))",
          '_res.get("count", _cnt)' in dg_src)
    check("A12: R1 闸在 ghost_start 分支最前(互斥/停链之前)",
          dg_src.find('拒绝启动抓鬼: 储备金不足') > 0
          and dg_src.find('拒绝启动抓鬼: 储备金不足')
          < dg_src.find('切换到抓鬼, 停止任务链'))

    # ================================================================ B. shop_errand 降量/短路
    import importlib
    se = importlib.import_module("shop_errand")

    class _StubQe(types.ModuleType):
        def __init__(self):
            types.ModuleType.__init__(self, "quest_engine")
            self.calls = []

        def dispatch_cmd(self, ro, cmd):
            self.calls.append(cmd)
            return {"result": "ok"}

    class _Rob(object):
        def __init__(self, reserve=0, acct="afford_a@x"):
            self.m_account = [acct, "pw"]
            self.m_reserve = reserve

    stub_qe = _StubQe()
    _prev_qe = sys.modules.get("quest_engine")
    sys.modules["quest_engine"] = stub_qe

    # B1 降量: 31167 // 211 = 147, -1 余量 → 146
    ro = _Rob(31167, "afford_b1@x")
    res = se.start(ro, 102007, count=200, owner="errand")
    check("B1: reserve=31167 请求200 → 实际下单 146(147-1 余量)",
          res.get("ok") and res.get("count") == 146,
          (res, stub_qe.calls[-1].get("count") if stub_qe.calls else None))
    check("B1b: 执行器收到的是降量后的 count",
          bool(stub_qe.calls) and stub_qe.calls[-1].get("count") == 146, stub_qe.calls[-1:])
    se.consume(ro, "errand")
    check("B1c: spec 用例点 147 = 未留余量上限(仅文档口径, 防回归)",
          31167 // 211 == 147)

    # B2 可买数 0 → no_money, 不占锁, 不调执行器
    n_calls = len(stub_qe.calls)
    ro2 = _Rob(100, "afford_b2@x")
    res2 = se.start(ro2, 102007, count=200, owner="errand")
    check("B2: reserve=100(<211) → no_money 且 count=0",
          (not res2.get("ok")) and res2.get("result") == "no_money" and res2.get("count") == 0, res2)
    check("B2b: no_money 不占锁(holder 为空)", se.holder(ro2) is None, se.holder(ro2))
    check("B2c: no_money 不调执行器", len(stub_qe.calls) == n_calls, len(stub_qe.calls))

    # B3 reserve=0(未同步) 放行不拦
    ro3 = _Rob(0, "afford_b3@x")
    res3 = se.start(ro3, 102007, count=200, owner="errand")
    check("B3: reserve=0(未同步) 放行, count=200",
          res3.get("ok") and res3.get("count") == 200, res3)
    se.consume(ro3, "errand")

    # B4 边界: 467 → 467//211=2 ≥2 → 1(准穷号可买 1 个)
    ro4 = _Rob(467, "afford_b4@x")
    res4 = se.start(ro4, 102007, count=200, owner="errand")
    check("B4: reserve=467(<500) 降为 1 个(仍能发起, 不短路)",
          res4.get("ok") and res4.get("count") == 1, res4)
    se.consume(ro4, "errand")

    # B5 边界(决策 B): 421 → 421//211=1(=1 不减) → 买 1 个(旧 A 案此处置 no_money)
    ro5 = _Rob(421, "afford_b5@x")
    res5 = se.start(ro5, 102007, count=200, owner="errand")
    check("B5(B 案): reserve=421(可买数=1) → 不减余量, 买 1 个",
          res5.get("ok") and res5.get("count") == 1, res5)
    se.consume(ro5, "errand")

    # B6 边界(决策 B): 211(恰好 1 个价) → 买 1 个(准穷号自愈路径)
    ro6 = _Rob(211, "afford_b6@x")
    res6 = se.start(ro6, 102007, count=200, owner="errand")
    check("B6(B 案): reserve=211(恰好 1 个) → 买 1 个",
          res6.get("ok") and res6.get("count") == 1, res6)
    se.consume(ro6, "errand")

    # B7 边界: 210(<1 个价) → no_money(真买不起)
    ro7 = _Rob(210, "afford_b7@x")
    res7 = se.start(ro7, 102007, count=200, owner="errand")
    check("B7(边界): reserve=210(可买数=0) → no_money",
          (not res7.get("ok")) and res7.get("result") == "no_money", res7)

    if _prev_qe is not None:
        sys.modules["quest_engine"] = _prev_qe
    else:
        del sys.modules["quest_engine"]

    # ================================================================ C. 552 回传
    events = []
    ns_nt = {
        "_SHOP_FAIL_NOTICE": {551: "金钱不够", 552: "货币(储备金/银票)不够",
                              1248: "离 NPC 太远(超过 600)"},
        "SHOP_FAIL_BACKOFF_SEC": 180,
        "__parse_add_item_link": lambda a: None,
        "__shop_purchase_done": lambda *a: True,
        "__unwrap_notice_arg": lambda a: a,
        "__emit": lambda ro, ev: events.append(ev),
        "__now_ms": lambda: 1000000,
    }
    frag_nt = _extract_func(qe_src, "on_shop_notice_args")
    check("C0: 提取 on_shop_notice_args", frag_nt is not None)
    fn_nt = None
    if frag_nt:
        try:
            exec(frag_nt, ns_nt)
            fn_nt = ns_nt.get("on_shop_notice_args")
        except Exception as e:  # noqa
            check("C0: exec on_shop_notice_args", False, str(e))

    class _RobQ(object):
        def __init__(self, acct):
            self.m_account = [acct, "pw"]
            self.m_quest = types.SimpleNamespace(
                shop_ctx={"item_index": 102007, "count": 200}, active=False)

    if fn_nt is not None:
        roq = _RobQ("afford_c1@x")
        # 编排层锁: owner=drug 持有中(模拟 __heal_buy_tick 启动后的会话)
        ok_lk, _h = se.acquire(roq, "drug", 102007, 200)
        check("C1: 锁已就位(前置)", ok_lk is True, _h)
        ret = fn_nt(roq, 552, [])
        st = se.status(roq)
        check("C1a: 552 → 编排层 result=failed(不再等 90s 超时)",
              ret is True and st.get("result") == "failed", st)
        check("C1b: reason 含 552 与 货币(储备金)",
              "552" in (st.get("reason") or "") and "货币" in (st.get("reason") or ""), st.get("reason"))
        check("C1c: rejected 记录原因(供调用方决策)",
              "货币" in (st.get("reason") or ""), st.get("reason"))
        check("C1d: 退避语义保留(buy_wait_until 已写)",
              int(roq.m_quest.shop_ctx.get("buy_wait_until", 0)) > 0
              and roq.m_quest.shop_ctx.get("buy_fail") == "货币(储备金/银票)不够",
              roq.m_quest.shop_ctx)
        se.consume(roq, "drug")

    # ================================================================ D. 连败冷却(指数升级)
    ns_gs = {}
    frag_gs = _extract_class(dg_src, "GhostState")
    check("D0: 提取 GhostState", frag_gs is not None)
    GS = None
    if frag_gs:
        try:
            exec(frag_gs, ns_gs)
            GS = ns_gs.get("GhostState")
        except Exception as e:  # noqa
            check("D0: exec GhostState", False, str(e))
    if GS is not None:
        gs = GS()
        gs.buy_fail_streak = 3
        gs.reset()
        check("D1: 重登(reset)后连败计数保留", int(gs.buy_fail_streak) == 3, gs.buy_fail_streak)

    logs = []
    need_holder = {"v": None}
    ns_buy = {
        "__log": lambda ro, lvl, msg: logs.append((lvl, msg)),
        "__set_state": lambda g, st, now: setattr(g, "state", st),
        "__need_heal_item": lambda ro: need_holder["v"],
        "__is_low_hp": lambda ro: True,
        "__is_low_mp": lambda ro: False,
        "__consume_nav": lambda *a: 0,
        "HEAL_ITEM_HP_INDEX": 102007,
        "HEAL_ITEM_MP_INDEX": 102010,
        "DRUG_BUY_COUNT": 200,
        "DRUG_TARGET_COUNT": 200,
        "error": types.SimpleNamespace(ROBOT_DELETED=-999),
    }
    frag_buy = _extract_func(dg_src, "__heal_buy_tick")
    check("D2: 提取 __heal_buy_tick", frag_buy is not None)
    buy = None
    if frag_buy:
        try:
            exec(frag_buy, ns_buy)
            buy = ns_buy.get("__heal_buy_tick")
        except Exception as e:  # noqa
            check("D2: exec __heal_buy_tick", False, str(e))

    if buy is not None and GS is not None:
        rob = _Rob(0, "afford_d@x")
        rob.m_quest = None
        g = GS()
        g.heal_ctx = {"phase": "buy", "rounds": 0}
        expect = [60, 120, 240, 480, 960, 1800, 1800]
        all_ok = True
        detail = []
        for i, exp_s in enumerate(expect):
            se.acquire(rob, "drug", 102007, 200)
            se.notify_failed(rob, "服务端拒绝:货币(储备金/银票)不够(码 552)")
            now = 1000000 + i * 10000000
            logs[:] = []
            buy(rob, g, None, now)
            got_ms = int(g.heal_cooldown_until) - now
            detail.append(got_ms)
            if got_ms != exp_s * 1000:
                all_ok = False
        check("D3: 连败冷却序列 60/120/240/480/960/1800(封顶)", all_ok, detail)
        check("D4: 失败路径进 1299 分支(日志'买药失败', 非'买药超时')",
              any("买药失败" in m for _l, m in logs)
              and not any("买药超时" in m for _l, m in logs), logs[-2:])
        # 成功 → streak 归零(模拟真实调用方: buy phase 且 heal_ctx 在)
        se.acquire(rob, "drug", 102007, 200)
        se.notify_done(rob, 102007, "买到 item_index=102007", False)
        need_holder["v"] = 102007
        g.heal_ctx = {"phase": "buy", "rounds": 0}
        logs[:] = []
        buy(rob, g, None, 200000000)
        check("D5: 买到药 → 连败计数归零",
              int(g.buy_fail_streak) == 0 and g.heal_ctx and g.heal_ctx.get("phase") == "use_item",
              (g.buy_fail_streak, g.heal_ctx))
        # no_money → 30 分钟长冷却(与既有冷却取较大值)
        need_holder["v"] = None
        g2 = GS()
        g2.heal_ctx = {"phase": "buy", "rounds": 0}
        g2.heal_cooldown_until = 300000000  # 既有冷却更晚 → 不被缩短
        rob2 = _Rob(100, "afford_d2@x")
        rob2.m_quest = None
        rob2.m_bag_cache = {}
        rob2.m_reserve = 100
        logs[:] = []
        buy(rob2, g2, None, 250000000)
        check("D6: no_money → 30 分钟长冷却且不缩短既有冷却",
              (g2.heal_ctx is None) and int(g2.heal_cooldown_until) == 300000000
              and any("no_money" in m or "储备金不足买不起药" in m for _l, m in logs),
              (g2.heal_cooldown_until, logs[-2:]))

    # ================================================================ E. R1 接单闸(源码截段真执行)
    def _r1_extract(src):
        lines = src.splitlines()
        si = None
        for i, ln in enumerate(lines):
            if ln == '\tif name == "ghost_start":':
                si = i + 1
                break
        if si is None:
            return None
        ei = None
        for j in range(si, len(lines)):
            if lines[j].strip().startswith("# 2026-08-24 互斥"):
                ei = j
                break
        if ei is None:
            return None
        seg = textwrap.dedent("\n".join(lines[si:ei]))
        body = textwrap.indent(seg, "    ")
        return "def _r1(name, robot_object, GHOST_RESERVE_FLOOR, HEAL_ITEM_HP_INDEX, __emit):\n" \
               + body + "\n    return None\n"

    r1_src = _r1_extract(dg_src)
    check("E0: 截取 ghost_start 的 R1 闸段", r1_src is not None)
    r1 = None
    if r1_src:
        ns_r1 = {}
        try:
            exec(r1_src, ns_r1)
            r1 = ns_r1.get("_r1")
        except Exception as e:  # noqa
            check("E0: exec R1 段", False, str(e))

    if r1 is not None:
        emits = []

        def _mk_rob(rsv, bag):
            r = _Rob(rsv, "afford_e%sp@x" % rsv)
            r.m_bag_cache = dict(bag)
            return r

        r = r1("ghost_start", _mk_rob(200, {}), 211, 102007,
               lambda ro, ev: emits.append(ev))
        check("E1: 穷(200<211)且无药 → 拒绝(result=no_money)",
              isinstance(r, dict) and r.get("result") == "no_money", r)
        check("E1b: 拒绝时发 warn 日志(锚点'拒绝启动抓鬼')",
              any("拒绝启动抓鬼" in (e.get("msg") or "") for e in emits), emits)
        r = r1("ghost_start", _mk_rob(200, {102007: [111, 5]}), 211, 102007,
               lambda ro, ev: emits.append(ev))
        check("E2(反例): 穷但背包有金创药 → 放行", r is None, r)
        r = r1("ghost_start", _mk_rob(0, {}), 211, 102007,
               lambda ro, ev: emits.append(ev))
        check("E3(反例): reserve=0(未同步) → 放行", r is None, r)
        r = r1("ghost_start", _mk_rob(211, {}), 211, 102007,
               lambda ro, ev: emits.append(ev))
        check("E4(边界): reserve=211(=阈值) → 放行(可买 1 个自愈)", r is None, r)
        r = r1("ghost_start", _mk_rob(600, {}), 211, 102007,
               lambda ro, ev: emits.append(ev))
        check("E5(反例): reserve=600(≥阈值) → 放行", r is None, r)

    # ================================================================ 汇总
    nfail = 0
    for name, ok, detail in results:
        if ok:
            print("[PASS] %s" % name)
        else:
            nfail += 1
            print("[FAIL] %s %s" % (name, ("(%s)" % (detail,)) if detail else ""))
    print("=== 结果: %s (共 %d 项断言, 失败 %d 项) ===" % (
        "PASS" if nfail == 0 else "FAIL", len(results), nfail))
    return 0 if nfail == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
