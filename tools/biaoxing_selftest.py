# -*- coding: utf-8 -*-
"""镖行天下（押镖）机器人端实施自检 —— 2026-09-30

规格：`docs/04-测试/计划-20260930-镖行天下.md` + `分析-20260930-镖行天下任务链.md`。
本批（机器人端首期，40-59 档）：
  ① `share_daily_cfg.csv` 新增两行：
     - `share_daily_镖行天下`（keywords=运镖任务|领取报酬、chain_task=2001107（抵押品子链归属）、
       shop=四店前缀路由（24 件抵押品全覆盖）、accept_ticket=2001101（隐藏票））；
     - `share_daily_镖局嘱托`（P0-1 前置补做 5001607：keywords=尝试一次运镖|领取报酬、
       accept_ticket=5001607；由中控按 key 派发、daily_limit=1 收尾）；
  ② `share_daily.py` 新增 P2 构件：接票预检（现金 ≥1 金）/ 前置 5001607 检测（专用码）/
     30min 限时观测 / 打劫战斗善后归因 / 交付与加成日志；
  ③ 主驱动（接取/交付/item_recycle/采购/定制战斗/掉任务冷却）全部复用既有通用状态机。

断言分组（对 `.bak_20260930_biaoxing` 坏版跑同断言必 FAIL = 坏版灵敏度）:
  S:*   静态接线（常量/函数/调用点/状态字段/csv 行）
  D:*   行为（config 解析 / 预检 / 观测构件 / 对话严格选择规格）
  CTRL:* 控制组（既有玩法行与既有对话行为不受影响）——坏版也应 PASS

用法:
  python tools/biaoxing_selftest.py [script_dir]
  python tools/biaoxing_selftest.py [script_dir] \
        --bak-suffix .bak_20260930_biaoxing --expect-bad

只读：仅读文件 + 抽取函数体在本进程 exec 驱动（不启动机器人、不改任何文件）。
"""
import os
import re
import sys
import types

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DEFAULT_SCRIPT = os.path.join(ROOT, "deploy", "zones", "prod-240-2300", "script")
PASS, FAIL = [], []

# 24 件抵押品 → 期望商店（前缀路由；20011.xml:503-530 清单）
COLLATERAL_EXPECT = {
    101334: 13021, 101008: 13021, 101009: 13021, 101004: 13021, 101031: 13021,
    108086: 13021, 101403: 13021, 101404: 13021,
    102031: 13011, 102033: 13011, 102034: 13011, 102036: 13011,
    210102: 13007, 210202: 13007, 210302: 13007, 210602: 13007, 210702: 13007,
    210902: 13007,
    220602: 13006, 220302: 13006, 220102: 13006, 220402: 13006, 220202: 13006,
    220502: 13006,
}


def ok(name, cond, detail=""):
    (PASS if cond else FAIL).append(name)
    print("  [%s] %s%s" % ("OK" if cond else "FAIL", name,
                           (" —— " + str(detail)) if detail else ""))


def _extract_func(src, name):
    lines = src.splitlines()
    pat = re.compile(r"^(\t*)def %s\(" % re.escape(name))
    for i, ln in enumerate(lines):
        m = pat.match(ln)
        if not m:
            continue
        indent = m.group(1)
        out = [ln[len(indent):]]
        for ln2 in lines[i + 1:]:
            if ln2.strip() and not ln2.startswith(indent + "\t") and not ln2.startswith(indent + " "):
                break
            out.append(ln2[len(indent):] if ln2.startswith(indent) else ln2)
        return "\n".join(out)
    return None


def _extract_class(src, name):
    lines = src.splitlines()
    pat = re.compile(r"^(\t*)class %s\(?[^)]*\)?:" % re.escape(name))
    for i, ln in enumerate(lines):
        m = pat.match(ln)
        if not m:
            continue
        indent = m.group(1)
        out = [ln[len(indent):]]
        for ln2 in lines[i + 1:]:
            if ln2.strip() and not ln2.startswith(indent + "\t") and not ln2.startswith(indent + " "):
                break
            out.append(ln2[len(indent):] if ln2.startswith(indent) else ln2)
        return "\n".join(out)
    return None


def main():
    args = sys.argv[1:]
    script_dir = None
    bak_suffix = ""
    expect_bad = False
    i = 0
    while i < len(args):
        a = args[i]
        if a == "--bak-suffix" and i + 1 < len(args):
            bak_suffix = args[i + 1]
            i += 2
        elif a == "--expect-bad":
            expect_bad = True
            i += 1
        elif script_dir is None:
            script_dir = a
            i += 1
        else:
            i += 1
    script_dir = script_dir or DEFAULT_SCRIPT

    sd_path = os.path.join(script_dir, "share_daily.py" + bak_suffix)
    csv_path = os.path.join(script_dir, "share_daily_cfg.csv" + bak_suffix)
    for p in (sd_path, csv_path):
        if not os.path.exists(p):
            print("[FAIL] 找不到 %s" % p)
            return 2
    print("script_dir : %s" % script_dir)
    print("bak_suffix : %r" % bak_suffix)
    src = open(sd_path, encoding="utf-8", errors="replace").read()
    csv_raw = open(csv_path, "rb").read()
    csv_text = csv_raw.decode("utf-8-sig")

    # ================================================================ 静态
    print("== 1) 静态接线 ==")
    ok("S:常量块(BIAOXING_KEY/DEPOSIT/TIME_LIMIT/MAIN_TASKS/BONUS)",
       all(k in src for k in ("BIAOXING_KEY = ", "BIAOXING_DEPOSIT_MONEY = 10000",
                              "BIAOXING_TIME_LIMIT_MS = 30 * 60 * 1000",
                              "BIAOXING_MAIN_TASKS = ", "BIAOXING_BONUS_TASK = 20011999")))
    ok("S:试点放行开关(预检内读 robot_biaoxing_skip_precheck + warn 文案)",
       "robot_biaoxing_skip_precheck" in src and "预检已放行" in src
       and ("robot_biaoxing_skip_precheck" in
            (_extract_func(src, "biaoxing_accept_precheck") or "")))
    ok("S:放行 warn 去重位 bx_precheck_warned（预检函数内读 m_share_daily 标记）",
       "bx_precheck_warned" in (_extract_func(src, "biaoxing_accept_precheck") or ""))
    ok("S:战斗归因门 bx_accepted（note_fight_end 首查）",
       "bx_accepted" in (_extract_func(src, "biaoxing_note_fight_end") or ""))
    ok("S:接票成功置 bx_accepted（on_notice ADD_TASK 段）",
       "g.bx_accepted = True" in src)
    ok("S:无匹配分支全量 dump（格式串 + helper 定义/调用）",
       "接取对话无本 run 选项: n=%d" in src and "def __fmt_accept_options(" in src
       and "__fmt_accept_options(option_list)" in (_extract_func(src, "__on_show_dialog") or ""))
    ok("S:构成函数齐全(__is_biaoxing/accept_precheck/note_fight_end/finish/drop/tick_note)",
       all(("def %s(" % f) in src for f in (
           "__is_biaoxing", "biaoxing_accept_precheck", "biaoxing_note_fight_end",
           "biaoxing_note_finish", "biaoxing_note_drop", "biaoxing_tick_note")))
    ok("S:状态字段(7 个 bx_ 观测位)", all(("self.%s" % f) in src for f in (
        "bx_accept_logged", "bx_need_prereq", "bx_task_start_ms", "bx_timeout_warned",
        "bx_last_fight_end_ms", "bx_accepted", "bx_precheck_warned")))
    ok("S:__cmd_start 复位观测位", "g.bx_accept_logged = False" in src
       and "g.bx_last_fight_end_ms = 0" in src)
    ok("S:__on_accept 接票预检调用", "biaoxing_accept_precheck(robot_object)" in
       ( _extract_func(src, "__on_accept") or ""))
    ok("S:on_notice 前置 5001607 专用码 NEED_PREREQ",
       "NEED_PREREQ" in src and "前置任务未完成" in src
       and "bx_need_prereq = True" in src)
    ok("S:__on_task_finish 交付观测调用", "biaoxing_note_finish(" in
       (_extract_func(src, "__on_task_finish") or ""))
    ok("S:__on_task_drop 归因调用", "biaoxing_note_drop(" in
       (_extract_func(src, "__on_task_drop") or ""))
    ok("S:tick 战斗结束标记 + 限时观测", "biaoxing_note_fight_end(" in src
       and "biaoxing_tick_note(" in (_extract_func(src, "tick") or ""))

    row = None
    row2 = None
    for ln in csv_text.splitlines():
        if ln.startswith("share_daily_镖行天下,"):
            row = ln
        elif ln.startswith("share_daily_镖局嘱托,"):
            row2 = ln
    ok("S:csv 新增 biaoxing 行", row is not None)
    if row:
        parts = row.split(",")
        def g(k):
            return parts[k].strip() if len(parts) > k else ""
        ok("S:csv 行字段(keywords/chain_task/shop/accept_ticket)",
           g(1) == "运镖任务|领取报酬" and g(8) == "2001107" and g(10) == "2001101"
           and g(9) == "13021:101|108@金币;13007:210;13006:220;13011:102",
           (g(1), g(8), g(9), g(10)))
    ok("S:csv 前置补做行(镖局嘱托, P0-1)", row2 is not None)
    if row2:
        p2 = row2.split(",")
        def g2(k):
            return p2[k].strip() if len(p2) > k else ""
        ok("S:镖局嘱托行字段(keywords/accept_ticket=5001607/无shop)",
           g2(1) == "尝试一次运镖|领取报酬" and g2(10) == "5001607" and g2(9) == "",
           (g2(1), g2(9), g2(10)))

    # ================================================================ config 解析
    print("== 2) config 解析（真实类驱动真实 csv）==")
    cls_src = _extract_class(src, "ShareDailyConfig")
    ok("CTRL:提取 ShareDailyConfig", cls_src is not None)
    cfg = None
    if cls_src:
        ns_c = {}
        try:
            exec(compile(cls_src, "<cfg>", "exec"), ns_c)
            cfg = ns_c["ShareDailyConfig"]()
            cfg.load(csv_path)
        except Exception as e:  # noqa
            ok("D:实例化+load", False, repr(e))
            cfg = None
    if cfg is not None:
        K = "share_daily_镖行天下"
        ok("D:load 成功且含 biaoxing 行", cfg.loaded and K in cfg.keys_order)
        ok("D:keywords 拆分", cfg.get_keywords(K) == ["运镖任务", "领取报酬"],
           cfg.get_keywords(K))
        ok("D:accept_ticket=2001101", cfg.get_accept_ticket(K) == 2001101)
        ok("D:chain_task 含 2001107（保 share_key 不漂移）",
           cfg.is_chained_task(K, 2001107) is True)
        ok("CTRL:2001107 非 chain 子键（不误挂 reply）",
           cfg.is_chained_share_key(K, "share_daily_镖行天下07") is False)
        # 24 件抵押品前缀路由（逐件核，失败给明细；全对走一条汇总断言防刷屏）
        bad = [(it, cfg.find_shop_npc(K, it), w) for it, w in COLLATERAL_EXPECT.items()
               if cfg.find_shop_npc(K, it) != w]
        ok("D:24 件抵押品前缀路由全对(101/108→13021,102→13011,210→13007,220→13006)",
           not bad, bad[:3])
        ok("CTRL:未知名物品 → 无店（不误路由）", cfg.find_shop_npc(K, 999999) == 0)
        ok("D:shop 关键词(金币)继承", cfg.get_shop_keyword(K, 13021) == "金币")
        # P0-1 前置补做行（镖局嘱托）
        K2 = "share_daily_镖局嘱托"
        ok("D:镖局嘱托 keywords 拆分",
           cfg.get_keywords(K2) == ["尝试一次运镖", "领取报酬"], cfg.get_keywords(K2))
        ok("D:镖局嘱托 accept_ticket=5001607", cfg.get_accept_ticket(K2) == 5001607)
        ok("CTRL:镖局嘱托无 shop 配置（find_shop_npc=0）",
           cfg.find_shop_npc(K2, 101008) == 0)
        # CTRL：既有行不受影响
        ok("CTRL:神捕/烽火行解析不变",
           cfg.get_keywords("share_daily_大唐神捕") == ["大唐神捕", "回复张忍慎"]
           and cfg.get_keywords("share_daily_宫廷10") == ["烽火大唐", "回复秦琼"]
           and cfg.get_keywords("share_daily_捉鬼") == ["捉鬼", "交付捉鬼任务"])
        ok("CTRL:烽火 shop/chain_task 不变",
           cfg.find_shop_npc("share_daily_宫廷10", 102007) == 13011
           and cfg.is_chained_task("share_daily_宫廷10", 2002107) is True)

    # ================================================================ 构件行为
    print("== 3) P2 构件行为 ==")
    ns = {}
    for m in re.finditer(r"^(BIAOXING_\w+)\s*=\s*(.+?)(?:\s*#.*)?$", src, re.M):
        try:
            ns[m.group(1)] = eval(m.group(2), {}, {})
        except Exception:
            pass
    logs = []
    ns["__log"] = lambda ro, lvl, msg: logs.append((str(lvl), str(msg)))
    ns["__now_ms"] = lambda: 1000000000.0
    ok("D:常量解析齐全(6 个)", all(k in ns for k in (
        "BIAOXING_KEY", "BIAOXING_DEPOSIT_MONEY", "BIAOXING_TIME_LIMIT_MS",
        "BIAOXING_MAIN_TASKS", "BIAOXING_BONUS_TASK", "BIAOXING_FIGHT_GRACE_MS")),
       sorted(k for k in ns if k.startswith("BIAOXING")))

    for fn in ("__is_biaoxing", "biaoxing_accept_precheck", "biaoxing_note_fight_end",
               "biaoxing_note_finish", "biaoxing_note_drop", "biaoxing_tick_note",
               "__fmt_accept_options"):
        frag = _extract_func(src, fn)
        ok("D:提取 %s" % fn, frag is not None)
        if frag:
            try:
                exec(compile(frag, "<fn>", "exec"), ns)
            except Exception as e:  # noqa
                ok("D:exec %s" % fn, False, repr(e))

    is_bx = ns.get("__is_biaoxing")
    precheck = ns.get("biaoxing_accept_precheck")
    n_fight = ns.get("biaoxing_note_fight_end")
    n_finish = ns.get("biaoxing_note_finish")
    n_drop = ns.get("biaoxing_note_drop")
    n_tick = ns.get("biaoxing_tick_note")

    if is_bx is not None:
        class _G(object):
            pass
        gb = _G()
        gb.share_key = "share_daily_镖行天下"
        ok("D:__is_biaoxing 主键 True", is_bx(gb) is True)
        gb.share_key = "share_daily_镖行天下07"
        ok("D:__is_biaoxing 子键 True", is_bx(gb) is True)
        gb.share_key = "share_daily_宫廷10"
        ok("CTRL:__is_biaoxing 他家玩法 False", is_bx(gb) is False)
        ok("CTRL:__is_biaoxing 空/异常兜底 False",
           is_bx(_G()) is False and is_bx(None) is False)

    # 试点开关：config 桩（可编程 robot_biaoxing_skip_precheck）
    cfg_sw = types.ModuleType("config")
    _prev_cfg = sys.modules.get("config")
    sys.modules["config"] = cfg_sw
    if precheck is not None:
        ro = types.SimpleNamespace(m_money=5000)
        code, why = precheck(ro)
        ok("D:预检 现金 5000 → 拒(BALANCE_LOW)", code == "BALANCE_LOW" and "押金" in why,
           (code, why))
        ro = types.SimpleNamespace(m_money=0)
        ok("D:预检 现金 0 → 拒", precheck(ro)[0] == "BALANCE_LOW")
        ro = types.SimpleNamespace()
        ok("D:预检 无 m_money → 拒(保守)", precheck(ro)[0] == "BALANCE_LOW")
        ro = types.SimpleNamespace(m_money=10000)
        ok("D:预检 恰好 1 金 → 过", precheck(ro) == ("", ""))
        ro = types.SimpleNamespace(m_money=123456)
        ok("D:预检 充裕 → 过", precheck(ro) == ("", ""))
        # 2026-09-30 试点开关三态 + 放行 warn 去重（每 run 一条，防 tick 级刷屏）
        cfg_sw.robot_biaoxing_skip_precheck = True
        logs[:] = []
        gsw = types.SimpleNamespace(bx_precheck_warned=False)
        ro_sw = types.SimpleNamespace(m_money=5000, m_share_daily=gsw)
        code, why = precheck(ro_sw)
        ok("D:试点开关=True → 放行(现金 5000 也过)+warn 留痕",
           code == "" and why == "" and any("预检已放行" in m for _l, m in logs)
           and gsw.bx_precheck_warned is True, (code, logs))
        logs[:] = []
        code2, _w2 = precheck(ro_sw)   # 同 run 高频再调 → 不再记
        ok("D:放行 warn 去重（同 run 第二次不再刷）",
           code2 == "" and not any("预检已放行" in m for _l, m in logs), logs)
        logs[:] = []
        precheck(types.SimpleNamespace(m_money=5000))   # 无 m_share_daily → 不崩、不刷
        ok("D:预检无 m_share_daily 兜底（放行且不记日志）",
           not any("预检已放行" in m for _l, m in logs), logs)
        cfg_sw.robot_biaoxing_skip_precheck = False
        logs[:] = []
        ok("D:试点开关=False → 恢复拦截(BALANCE_LOW)且不记放行日志",
           precheck(types.SimpleNamespace(m_money=5000))[0] == "BALANCE_LOW"
           and not any("预检已放行" in m for _l, m in logs))
        delattr(cfg_sw, "robot_biaoxing_skip_precheck")
        ok("D:开关属性缺省(未配置) → 按 False 拦截",
           precheck(types.SimpleNamespace(m_money=5000))[0] == "BALANCE_LOW")
    if _prev_cfg is not None:
        sys.modules["config"] = _prev_cfg
    else:
        sys.modules.pop("config", None)

    if n_fight is not None:
        # 2026-09-30 归因门：未接票（残留抓鬼战）不归因；已接票正常
        logs[:] = []
        g0 = types.SimpleNamespace(bx_last_fight_end_ms=0, bx_accepted=False)
        ok("D:未接票战斗结束 → 不归因不落标记不记日志",
           n_fight(None, g0, 12345) is False and g0.bx_last_fight_end_ms == 0
           and not logs, logs)
        logs[:] = []
        g1 = types.SimpleNamespace(bx_last_fight_end_ms=0, bx_accepted=True)
        ok("D:已接票战斗结束 → 落标记+日志",
           n_fight(None, g1, 12345) is True and g1.bx_last_fight_end_ms == 12345
           and any("打劫战斗结束" in m for _l, m in logs), logs)

    if n_finish is not None:
        logs[:] = []
        g2 = types.SimpleNamespace(bx_task_start_ms=999, bx_timeout_warned=True)
        ok("D:完成(主)→清限时计时+日志",
           n_finish(None, g2, 2001101) is True and g2.bx_task_start_ms == 0
           and g2.bx_timeout_warned is False and any("交付完成" in m for _l, m in logs), logs)
        logs[:] = []
        ok("D:完成(加成 20011999)→只记日志", n_finish(None, g2, 20011999) is True
           and any("加成" in m and "核销" in m for _l, m in logs), logs)
        ok("CTRL:他链任务号 → 不处理", n_finish(None, g2, 2002107) is False)

    if n_drop is not None:
        logs[:] = []
        g3 = types.SimpleNamespace(bx_task_start_ms=1, bx_timeout_warned=True,
                                   bx_last_fight_end_ms=999999000)
        ok("D:掉任务归因=战斗善后(60s 窗口内)",
           n_drop(None, g3, 999999123, 999999123) is True
           and any("打劫战败" in m for _l, m in logs) and g3.bx_task_start_ms == 0, logs)
        # 注: 签名 (ro, g, ti, now_ms) —— 上面按 (None, g3, 999999123, 999999123) 传参
        logs[:] = []
        g4 = types.SimpleNamespace(bx_task_start_ms=1, bx_timeout_warned=False,
                                   bx_last_fight_end_ms=0)
        ok("D:掉任务归因=超时/回收",
           n_drop(None, g4, 2001101, 500000000) is True
           and any("超时" in m for _l, m in logs), logs)

    if n_tick is not None:
        logs[:] = []
        g5 = types.SimpleNamespace(bx_task_start_ms=1000000000 - 29 * 60 * 1000,
                                   bx_timeout_warned=False, task_index=2001101)
        ok("D:限时 29min → 不告警", n_tick(None, g5, 1000000000) is False and not logs)
        g6 = types.SimpleNamespace(bx_task_start_ms=1000000000 - 31 * 60 * 1000,
                                   bx_timeout_warned=False, task_index=2001101)
        ok("D:限时 31min → 告警一次",
           n_tick(None, g6, 1000000000) is True and g6.bx_timeout_warned is True
           and any("超 30 分钟" in m for _l, m in logs), logs)
        ok("D:限时 告警幂等(同轮再来一次 False)",
           n_tick(None, g6, 1000000000 + 1000) is False)
        g7 = types.SimpleNamespace(bx_task_start_ms=1, bx_timeout_warned=False, task_index=0)
        ok("D:无在身任务 → 不告警", n_tick(None, g7, 1000000000 + 90000000) is False)
        g8 = types.SimpleNamespace(bx_accepted=False, bx_task_start_ms=0,
                                   bx_timeout_warned=False, task_index=2001101)
        n_tick(None, g8, 1000000000)
        ok("D:恢复兜底：有在身任务 → bx_accepted 置位（重登/重派场景）",
           g8.bx_accepted is True)

    fmt = ns.get("__fmt_accept_options")
    if fmt is not None:
        s1 = fmt([("尝试一次运镖。", 0), ("我还有别的事情", 0)])
        ok("D:dump 基本形态 (i,close,text)",
           "(0,0,'尝试一次运镖。')" in s1 and "(1,0,'我还有别的事情')" in s1, s1)
        s2 = fmt([("关闭", 1)])
        ok("D:dump 可见 close 标记", "(0,1,'关闭')" in s2, s2)
        s3 = fmt([("尝试一次 运镖\u3000。", 0)])
        ok("D:dump 保空白/全角原样(repr 转义可见)",
           "尝试一次 运镖" in s3 and "\\u3000" in s3, s3)
        s4 = fmt([("x" * 100, 0)])
        ok("D:dump 单项截断≤48", ("x" * 48) in s4 and ("x" * 49) not in s4)
        s5 = fmt([None, ("t",), ()])
        ok("D:dump 畸形项不崩", isinstance(s5, str) and "'t'" in s5, s5)
        ok("D:dump 空列表 → 空串", fmt([]) == "")

    # ================================================================ 对话严格选择（规格回归）
    print("== 4) 对话严格选择（既有函数 + 镖行天下对话样例）==")
    ns_d = {}
    for fn in ("__opt_text", "__opt_close", "pick_accept_dialog_option",
               "pick_dialog_option_index", "is_leave_option"):
        frag = _extract_func(src, fn)
        if frag:
            try:
                exec(compile(frag, "<dlg>", "exec"), ns_d)
            except Exception as e:  # noqa
                ok("D:exec %s" % fn, False, repr(e))
    ns_d["ACCEPT_OPTION_KEYWORDS"] = ("接受", "领取", "接取")
    pick_accept = ns_d.get("pick_accept_dialog_option")
    pick_generic = ns_d.get("pick_dialog_option_index")
    KWS = ["运镖任务", "领取报酬"]

    if pick_accept is not None:
        dl = [("领取运镖任务", 0), ("我还有别的事情", 0)]
        idx, kind = pick_accept(dl, KWS, has_pending=False)
        ok("CTRL:董江票对话 → 选『领取运镖任务』", idx == 0 and kind == "accept", (idx, kind))
        dl2 = [("尝试一次运镖。", 0), ("领取运镖任务", 0)]
        idx2, _k2 = pick_accept(dl2, KWS, has_pending=False)
        ok("CTRL:混合含前置票 → 不点『尝试一次运镖。』", idx2 == 1, idx2)
        dl3 = [("镖行天下活动说明", 0), ("我还有别的事情", 0)]
        idx3, kind3 = pick_accept(dl3, KWS, has_pending=False)
        ok("CTRL:活动说明(含名不含'运镖任务') → 不误点(None)", idx3 is None and kind3 == "",
           (idx3, kind3))
        dl4 = [("领取报酬。", 0)]
        idx4, kind4 = pick_accept(dl4, KWS, has_pending=True)
        ok("CTRL:交付对话『领取报酬。』可点(A 路径, kind=%s)" % kind4, idx4 == 0, (idx4, kind4))

    if pick_generic is not None:
        dl5 = [("领取报酬。", 0)]
        ok("CTRL:交付态通用选择 → 命中『领取报酬』",
           pick_generic(dl5, "", KWS) == 0)
        dl6 = [("我还是再想想", 0)]
        ok("CTRL:无命中 → 退回第一个非关闭(旧行为)",
           pick_generic(dl6, "", KWS) == 0)
        # P0-1 前置补做（镖局嘱托）对话样例
        K2W = ["尝试一次运镖", "领取报酬"]
        if pick_accept is not None:
            dl7 = [("尝试一次运镖。", 0), ("我还有别的事情", 0)]
            idx7, kind7 = pick_accept(dl7, K2W, has_pending=False)
            ok("CTRL:前置票对话 → 选『尝试一次运镖。』(kind=%s)" % kind7,
               idx7 == 0, (idx7, kind7))
        ok("CTRL:前置交付『领取报酬』通用命中",
           pick_generic([("领取报酬", 0)], "", K2W) == 0)
        if pick_accept is not None:
            idx8, kind8 = pick_accept([("请尝试一次运镖啊", 0)], K2W, has_pending=False)
            ok("CTRL:选择器口径=子串匹配（'尝试一次运镖' 命中 '请尝试一次运镖啊'）",
               idx8 == 0, (idx8, kind8))

    # ================================================================ 结果
    def _grp(prefix, items=None):
        s = PASS + FAIL if items is None else items
        return [n for n in s if n.startswith(prefix)]

    sd_all = _grp("S:") + _grp("D:")
    sd_fail = [n for n in FAIL if n.startswith(("S:", "D:"))]
    ctrl_fail = _grp("CTRL:", FAIL)
    total = len(PASS) + len(FAIL)

    if expect_bad:
        sens_ok = (len(sd_fail) == len(sd_all) and len(sd_all) >= 20 and not ctrl_fail)
        print("\n=== 坏版灵敏度: %s ===" % ("OK" if sens_ok else "BAD"))
        print("    S/D 预期失败: %d/%d（应全部 FAIL）" % (len(sd_fail), len(sd_all)))
        print("    CTRL 意外失败: %d（应 0）" % len(ctrl_fail))
        for n in ctrl_fail:
            print("      [意外FAIL] %s" % n)
        return 0 if sens_ok else 1

    print("\n=== 结果: %s (共 %d 项断言, 失败 %d 项) ===" % (
        "PASS" if not FAIL else "FAIL", total, len(FAIL)))
    for n in FAIL:
        print("  [FAIL] %s" % n)
    return 0 if not FAIL else 1


if __name__ == "__main__":
    sys.exit(main())
