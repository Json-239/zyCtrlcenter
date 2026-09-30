# -*- coding: utf-8 -*-
"""私人池 Wave 2（机器人端）自检 —— 2026-09-30

规格 = `docs/04-测试/分析-20260930-私人池实施前勘查.md` §7-9 + 计划 §1.7/§4：
  ① 8 个物品行为点：私号全关（判据 `robot_mgr.is_personal(robot_object)` 函数首行早退）；
  ② 吃药链保留（不查标记）；
  ③ 标记投递：`robot_mgr.manage_robots` 新 `action:"personal"` 分支（全量名单覆盖/空集清空，
     进程内号级 set；空集=机器人模式）；
  ④ 空闲只摆摊：`auto_roam.is_idle_for_roam` 私号 False + 私号空闲走 booth 空清单
     （`booth_start, up_items=[]`，有任务意图先收摊让路）。

断言分组（对 `.bak_20260930_personalwave2` 坏版跑同断言必 FAIL = 坏版灵敏度）:
  P2S:* 静态（8 闸位置 + robot_mgr/auto_roam 结构 + 常量）
  P2G:* 8 闸行为（私号→默认返回+真实体未触碰"炸药"; 闸以 robot_object 入参）
  P2R:* robot_mgr personal 投递动态（覆盖/清空/非成员/空集=机器人模式/未知 action 无副作用）
  P2A:* auto_roam（私号禁游荡/空摆摊参数/让路收摊/退避间隔/失败退避）
  CTRL:* 控制组（非私号穿透=不受影响; 既有行为回归; 吃药链不挂闸）——坏版也应 PASS

用法:
  python tools/personal_wave2_selftest.py [script_dir]
  python tools/personal_wave2_selftest.py [script_dir] \
        --bak-suffix .bak_20260930_personalwave2 --expect-bad

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


def ok(name, cond, detail=""):
    (PASS if cond else FAIL).append(name)
    print("  [%s] %s%s" % ("OK" if cond else "FAIL", name,
                           (" —— " + str(detail)) if detail else ""))


def _extract_func(src, name):
    """抽取模块级或类内函数(类内自动 dedent 一级), 供 exec 驱动。"""
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


def _has_gate(fn_src, ret_text):
    """函数体内含"首行私号闸 + 指定返回值"（空白归一化后判定）。"""
    if not fn_src:
        return False
    s = fn_src.replace("\t", " ").replace("  ", " ")
    if "import robot_mgr as _rm_p" not in s:
        return False
    if "if _rm_p.is_personal(robot_object):" not in s:
        return False
    idx = s.find("_rm_p.is_personal(robot_object):")
    tail = s[idx:idx + 200]
    return ("return %s" % ret_text) in tail


class _Bomb(object):
    """取 truthy 即炸药（证明"闸后真实体被执行"）。"""
    def __bool__(self):
        raise RuntimeError("BOMB_BOOL")


class _BombCall(object):
    """一调用即炸药。"""
    def __call__(self, *a, **k):
        raise RuntimeError("BOMB_CALL")


class _BombItems(object):
    """bag.items() 即炸药。"""
    def items(self):
        raise RuntimeError("BOMB_BAG_ITEMS")


class _BombAttr(object):
    """指定属性访问即炸药（property; getattr 只吞 AttributeError 不吞 RuntimeError）。"""
    @property
    def bag_cleanup_tried(self):
        raise RuntimeError("BOMB_ATTR")

    @property
    def m_bag_cache(self):
        raise RuntimeError("BOMB_ATTR")


class _RecordCall(object):
    """记录调用次数的可调用对象（用于目标函数带外层 catch-all、炸药会被吞的场合）。"""
    def __init__(self, ret):
        self.ret = ret
        self.calls = 0

    def __call__(self, *a, **k):
        self.calls += 1
        return self.ret


class _RecordDict(dict):
    """记录 .get 读过的键（同上，探针式穿透证明）。"""
    def __init__(self):
        dict.__init__(self)
        self.gets = []

    def get(self, k, d=None):
        self.gets.append(k)
        return dict.get(self, k, d)


class _RMStub(object):
    """robot_mgr 桩: is_personal 可编程 + 记录入参。"""
    def __init__(self):
        self.val = False
        self.calls = []

    def is_personal(self, robot_object):
        self.calls.append(robot_object)
        return self.val


class _BoothStub(object):
    """booth 桩: 记录 dispatch 调用; 可编程返回。"""
    def __init__(self):
        self.calls = []
        self.ok = True

    def dispatch_cmd(self, robot_object, cmd):
        self.calls.append(cmd)
        if cmd.get("cmd") == "booth_stop":
            return {"type": "booth_reply", "cmd": "booth_stop", "ok": True}
        return {"type": "booth_reply", "cmd": cmd.get("cmd"),
                "ok": bool(self.ok), "msg": "stub"}


class _RWStub(object):
    def __init__(self):
        self.calls = []

    def dispatch_cmd(self, robot_object, cmd):
        self.calls.append(cmd)
        return {"result": "ok"}


def _set_module(name, obj):
    sys.modules[name] = obj


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

    def _read(basename):
        p = os.path.join(script_dir, basename + bak_suffix)
        if not os.path.exists(p):
            return None, p
        return open(p, encoding="utf-8", errors="replace").read(), p

    print("script_dir : %s" % script_dir)
    print("bak_suffix : %r" % bak_suffix)

    SRC = {}
    for mod in ("robot_mgr", "auto_roam", "daily_ghost", "auto_summon", "bag_ops",
                "encourage_claim", "pre_daily", "share_daily"):
        s, p = _read(mod + ".py")
        if s is None:
            print("[FAIL] 找不到 %s" % p)
            return 2
        SRC[mod] = s

    # ================================================================ 1) 静态
    print("== 1) 静态：8 闸位置 + robot_mgr/auto_roam 结构 ==")
    GATES = [
        ("daily_ghost", "__tidy_bag", "0"),
        ("auto_summon", "_try_bag_cleanup", "False"),
        ("bag_ops", "auto_open_gifts", "0"),
        ("bag_ops", "auto_equip_best_items", "(0, 0)"),
        ("bag_ops", "auto_use_bag_expanders", "0"),
        ("encourage_claim", "_use_boxes", "0, False"),
        ("pre_daily", "_use_card", "False"),
        ("share_daily", "__bag_cleanup_retry", "False"),
    ]
    gate_fns = {}
    for mod, fn, ret in GATES:
        frag = _extract_func(SRC[mod], fn)
        gate_fns[(mod, fn)] = frag
        ok("P2S:%s.%s 首行私号闸(return %s)" % (mod, fn, ret), _has_gate(frag, ret))

    n_bagops = SRC["bag_ops"].count("_rm_p.is_personal(robot_object)")
    ok("P2S:bag_ops 恰 3 处闸(开礼包/穿装/扩容)", n_bagops == 3, n_bagops)
    for mod in ("daily_ghost", "auto_summon", "encourage_claim", "pre_daily", "share_daily"):
        n = SRC[mod].count("_rm_p.is_personal(robot_object)")
        ok("P2S:%s 恰 1 处闸" % mod, n == 1, n)
    ok("P2S:auto_roam 恰 2 处(is_idle_for_roam+tick)",
       SRC["auto_roam"].count("_rm_p.is_personal(robot_object)") == 2,
       SRC["auto_roam"].count("_rm_p.is_personal(robot_object)"))

    ok("P2S:robot_mgr 有 personal 分支", 'elif action == "personal":' in SRC["robot_mgr"])
    ok("P2S:robot_mgr 有 g_personal_accounts = set()",
       "g_personal_accounts = set()" in SRC["robot_mgr"])
    ok("P2S:robot_mgr 有 def is_personal(robot_object)",
       "def is_personal(robot_object):" in SRC["robot_mgr"])
    ok("P2S:auto_roam 有 _tick_personal_booth 定义",
       "def _tick_personal_booth(robot_object, now_ms):" in SRC["auto_roam"])
    ok("P2S:auto_roam 空摆摊参数(booth_start + up_items:[])",
       '"cmd": "booth_start"' in SRC["auto_roam"] and '"up_items": []' in SRC["auto_roam"])
    _consts = dict(re.findall(r"^(_PERSONAL_BOOTH_\w+)\s*=\s*(\d+)", SRC["auto_roam"], re.M))
    ok("P2S:auto_roam 常量(HOLD=1800/GAP=600000/SCAN 32x8)",
       _consts.get("_PERSONAL_BOOTH_HOLD_SEC") == "1800"
       and _consts.get("_PERSONAL_BOOTH_MIN_GAP_MS") == "600000"
       and _consts.get("_PERSONAL_BOOTH_SCAN_STEP") == "32"
       and _consts.get("_PERSONAL_BOOTH_SCAN_MAX") == "8", _consts)

    # 吃药链不挂闸(CTRL: 两版都应 PASS)
    for mod, fn in (("auto_summon", "_try_role_heal"), ("auto_summon", "heal_tick"),
                    ("auto_summon", "_heal_buy_tick"), ("daily_ghost", "__use_heal_item"),
                    ("share_daily", "tick")):
        frag = _extract_func(SRC[mod], fn)
        ok("CTRL:吃药/会话链 %s.%s 不含私号闸(保留)" % (mod, fn),
           frag is not None and "is_personal" not in frag)

    # ================================================================ 2) 8 闸动态
    print("== 2) 8 闸动态（私号→早退且真实体未触碰; 非私号→穿透）==")
    rm_stub = _RMStub()
    _set_module("robot_mgr", rm_stub)
    # 各模块依赖桩
    cfg_stub = types.ModuleType("config")
    _set_module("config", cfg_stub)
    p3_stub = types.ModuleType("protocol3")
    _set_module("protocol3", p3_stub)
    cli_stub = types.ModuleType("client")
    cli_stub.g_item_data_dict = {}
    _set_module("client", cli_stub)

    def _mk_ns(frag_src, extra=None):
        ns = {}
        if frag_src:
            try:
                exec(compile(frag_src, "<frag>", "exec"), ns)
            except Exception as e:  # noqa
                return None, e
        if extra:
            ns.update(extra)
        return ns, None

    def _run_gate(case_mod, case_fn, ns, ro, args=(), bomb=None, bomb_setup=None):
        """返回 ("ok", ret) 或 ("raise", msg)。"""
        if bomb_setup:
            bomb_setup(ns)
        fn = ns.get(case_fn)
        if fn is None:
            return ("missing", None)
        try:
            return ("ok", fn(ro, *args))
        except RuntimeError as e:
            return ("raise", str(e))
        except Exception as e:  # noqa
            return ("raise", "%s: %s" % (type(e).__name__, e))

    def _case_runner(label, mod, fn, ret_text, bomb_tag, setup_bomb, mkcalls):
        """P(私号) + N(非私号) + identity 三断言。"""
        frag = gate_fns.get((mod, fn))
        if frag is None:
            ok("P2G:%s 提取函数" % label, False)
            return
        # -- P: 私号 → 默认返回, 真实体(炸药)未触碰
        ns, err = _mk_ns(frag)
        if ns is None:
            ok("P2G:%s exec" % label, False, err)
            return
        rm_stub.val = True
        rm_stub.calls = []
        ro_p = mkcalls["mkcalls"]()
        setup_bomb(ns)
        status, res = _run_gate(mod, fn, ns, ro_p, mkcalls.get("args", ()),
                                bomb_setup=None)
        ok("P2G:%s 私号→早退 %s(真实体未执行)" % (label, ret_text),
           status == "ok" and res == mkcalls.get("want", None), (status, res))
        ok("P2G:%s 闸以 robot_object 入参调用" % label,
           len(rm_stub.calls) == 1 and rm_stub.calls[0] is ro_p,
           len(rm_stub.calls))
        # -- N: 非私号 → 穿透到真实体(炸药抛出)
        ns2, err2 = _mk_ns(frag)
        if ns2 is None:
            ok("CTRL:%s exec(ns2)" % label, False, err2)
            return
        rm_stub.val = False
        ro_n = mkcalls["mkcalls"]()
        setup_bomb(ns2)
        status2, res2 = _run_gate(mod, fn, ns2, ro_n, mkcalls.get("args", ()),
                                  bomb_setup=None)
        ok("CTRL:%s 非私号穿透(到达真实体, 不受闸影响)" % label,
           status2 == "raise" and bomb_tag in str(res2), (status2, res2))

    def _ro_plain():
        return types.SimpleNamespace(m_account=["robotT"])

    def _ro_bombattr():
        return _BombAttr()

    _case_runner("daily_ghost.__tidy_bag", "daily_ghost", "__tidy_bag", "0", "BOMB_BOOL",
                 lambda ns: ns.update({"_ITEM_TIDYING": _Bomb()}),
                 {"mkcalls": _ro_plain, "want": 0})
    _case_runner("bag_ops.auto_open_gifts", "bag_ops", "auto_open_gifts", "0", "BOMB_BAG_ITEMS",
                 lambda ns: None,
                 {"mkcalls": _ro_plain, "args": (_BombItems(),), "want": 0})
    _case_runner("bag_ops.auto_equip_best_items", "bag_ops", "auto_equip_best_items", "(0, 0)",
                 "BOMB_CALL",
                 lambda ns: ns.update({"drop_pursuit_orders": _BombCall()}),
                 {"mkcalls": _ro_plain, "args": ({},), "want": (0, 0)})
    _case_runner("encourage_claim._use_boxes", "encourage_claim", "_use_boxes", "0, False",
                 "BOMB_ATTR",
                 lambda ns: None,
                 {"mkcalls": _ro_bombattr, "want": (0, False)})

    # auto_summon._try_bag_cleanup: 外层 try/except 会吞炸药 → 用"记录探针 st 字典"证明穿透
    frag = gate_fns.get(("auto_summon", "_try_bag_cleanup"))
    if frag is not None:
        ns, err = _mk_ns(frag)
        rm_stub.val = True
        rm_stub.calls = []
        cfg_stub.robot_bag_cleanup = True
        st_p = _RecordDict()
        status, res = _run_gate("auto_summon", "_try_bag_cleanup", ns, _ro_plain(), (st_p, 0))
        ok("P2G:auto_summon._try_bag_cleanup 私号→早退 False + 真实体探针未触碰",
           status == "ok" and res is False and st_p.gets == [], (status, res, st_p.gets))
        ns2, err2 = _mk_ns(frag)
        rm_stub.val = False
        st_n = _RecordDict()
        _run_gate("auto_summon", "_try_bag_cleanup", ns2, _ro_plain(), (st_n, 0))
        ok("CTRL:auto_summon._try_bag_cleanup 非私号穿透(探针被读取)",
           len(st_n.gets) >= 1, st_n.gets)
    else:
        ok("P2G:auto_summon._try_bag_cleanup 提取", False)

    # bag_ops.auto_use_bag_expanders: 同上 → 记录探针 _now_ms
    frag = gate_fns.get(("bag_ops", "auto_use_bag_expanders"))
    if frag is not None:
        ns, err = _mk_ns(frag)
        rm_stub.val = True
        rm_stub.calls = []
        probe_p = _RecordCall(123)
        ns["_now_ms"] = probe_p
        status, res = _run_gate("bag_ops", "auto_use_bag_expanders", ns, _ro_plain(), ({},))
        ok("P2G:bag_ops.auto_use_bag_expanders 私号→早退 0 + 真实体探针未调用",
           status == "ok" and res == 0 and probe_p.calls == 0, (status, res, probe_p.calls))
        ns2, err2 = _mk_ns(frag)
        rm_stub.val = False
        probe_n = _RecordCall(123)
        ns2["_now_ms"] = probe_n
        _run_gate("bag_ops", "auto_use_bag_expanders", ns2, _ro_plain(), ({},))
        ok("CTRL:bag_ops.auto_use_bag_expanders 非私号穿透(探针被调用)",
           probe_n.calls >= 1, probe_n.calls)
    else:
        ok("P2G:bag_ops.auto_use_bag_expanders 提取", False)

    # pre_daily._use_card: 同上 → 记录探针 auto_summon.get_state
    frag = gate_fns.get(("pre_daily", "_use_card"))
    as_stub = types.ModuleType("auto_summon")
    rec_box = {"n": 0}

    def _as_get_state(ro):
        rec_box["n"] += 1
        raise RuntimeError("PROBE_HIT")

    as_stub.get_state = _as_get_state
    _prev_as = sys.modules.get("auto_summon")
    _set_module("auto_summon", as_stub)
    if frag is not None:
        ns, err = _mk_ns(frag)
        rm_stub.val = True
        rm_stub.calls = []
        cfg_stub.robot_claim_double_exp_use_card = True
        status, res = _run_gate("pre_daily", "_use_card", ns, _ro_plain(), (0,))
        ok("P2G:pre_daily._use_card 私号→早退 False + 真实体探针未触碰",
           status == "ok" and res is False and rec_box["n"] == 0, (status, res, rec_box["n"]))
        ns2, err2 = _mk_ns(frag)
        rm_stub.val = False
        rec_box["n"] = 0
        _run_gate("pre_daily", "_use_card", ns2, _ro_plain(), (0,))
        ok("CTRL:pre_daily._use_card 非私号穿透(探针被调用)", rec_box["n"] >= 1, rec_box["n"])
    else:
        ok("P2G:pre_daily._use_card 提取", False)
    if _prev_as is not None:
        _set_module("auto_summon", _prev_as)
    else:
        sys.modules.pop("auto_summon", None)

    # share_daily.__bag_cleanup_retry: g 用炸药属性对象
    frag = gate_fns.get(("share_daily", "__bag_cleanup_retry"))
    if frag is not None:
        ns, err = _mk_ns(frag)
        rm_stub.val = True
        rm_stub.calls = []
        g_bomb = _BombAttr()
        status, res = _run_gate("share_daily", "__bag_cleanup_retry", ns, _ro_plain(),
                                (g_bomb,))
        ok("P2G:share_daily.__bag_cleanup_retry 私号→早退 False(真实体未执行)",
           status == "ok" and res is False, (status, res))
        ns2, err2 = _mk_ns(frag)
        rm_stub.val = False
        status2, res2 = _run_gate("share_daily", "__bag_cleanup_retry", ns2, _ro_plain(),
                                  (g_bomb,))
        ok("CTRL:share_daily.__bag_cleanup_retry 非私号穿透(到达真实体)",
           status2 == "raise" and "BOMB_ATTR" in str(res2), (status2, res2))
    else:
        ok("P2G:share_daily.__bag_cleanup_retry 提取函数", False)

    # ================================================================ 3) robot_mgr
    print("== 3) robot_mgr：personal 投递 + is_personal ==")
    ns_m = {"g_personal_accounts": set()}
    frag_m = _extract_func(SRC["robot_mgr"], "manage_robots")
    frag_ip = _extract_func(SRC["robot_mgr"], "is_personal")
    if frag_m:
        try:
            exec(compile(frag_m, "<mgr>", "exec"), ns_m)
        except Exception as e:  # noqa
            ok("P2R:exec manage_robots", False, repr(e))
    else:
        ok("P2R:提取 manage_robots", False)
    if frag_ip:
        try:
            exec(compile(frag_ip, "<mgr>", "exec"), ns_m)
        except Exception as e:  # noqa
            ok("P2R:exec is_personal", False, repr(e))
    else:
        ok("P2R:提取 is_personal", False)
    mgr = ns_m.get("manage_robots")
    is_p = ns_m.get("is_personal")
    self_stub = types.SimpleNamespace(m_account={})

    if mgr is not None:
        # 覆盖推名单（含去重/对偶/空串）
        r1 = mgr(self_stub, "personal", ["robotA", ["robotB", "pw"], " ", "robotA"])
        ok("P2R:personal 全量覆盖(去重/对偶/空串忽略)",
           isinstance(r1, dict) and ns_m["g_personal_accounts"] == set(["robotA", "robotB"]),
           ns_m.get("g_personal_accounts"))
        if is_p is not None:
            ok("P2R:is_personal 成员/非成员/异常兜底",
               is_p(types.SimpleNamespace(m_account=["robotA"])) is True
               and is_p(types.SimpleNamespace(m_account=["robotZ"])) is False
               and is_p(types.SimpleNamespace()) is False
               and is_p(types.SimpleNamespace(m_account=[])) is False)
            ok("P2R:闸以账号名判据(m_account[0])",
               is_p(types.SimpleNamespace(m_account=["robotA"])) is True)
        # 再推 → 全量替换
        mgr(self_stub, "personal", ["robotC"])
        ok("P2R:再推=全量替换(旧名单清掉)",
           ns_m["g_personal_accounts"] == set(["robotC"]), ns_m.get("g_personal_accounts"))
        # 空集 → 清空（=机器人模式; 先预置名单再推空, 防"从未设置"巧合通过）
        ns_m["g_personal_accounts"] = set(["robotZZ"])
        mgr(self_stub, "personal", [])
        ok("P2R:空集=清空(机器人模式兜底)",
           ns_m["g_personal_accounts"] == set(), ns_m.get("g_personal_accounts"))
        ok("CTRL:空集时 is_personal 恒 False(安全兜底)",
           is_p is None or (is_p(types.SimpleNamespace(m_account=["robotA"])) is False
                            and is_p(types.SimpleNamespace(m_account=["robotC"])) is False))
        # 未知 action benign（复核要查的点）
        before = set(ns_m["g_personal_accounts"])
        r2 = mgr(self_stub, "frobnicate", ["robotX"])
        ok("CTRL:未知 action benign(返回 result, 名单不动, 不抛)",
           isinstance(r2, dict) and "added" in r2 and ns_m["g_personal_accounts"] == before,
           r2)
        # add/remove 分支不受影响(空操作驱动, 不依赖真实建连环境)
        ns_m["config"] = types.ModuleType("config")
        try:
            r3 = mgr(self_stub, "add", [])
            r4 = mgr(self_stub, "remove", ["robotQ"])
            ok("CTRL:add/remove 既有分支形状不变(空操作)",
               isinstance(r3, dict) and isinstance(r4, dict)
               and r3.get("added") == [] and r4.get("removed") == [])
        except Exception as e:  # noqa
            ok("CTRL:add/remove 既有分支形状不变(空操作)", False, repr(e))
    else:
        ok("P2R:personal 全量覆盖", False, "manage_robots 未提取到")

    # ================================================================ 4) auto_roam
    print("== 4) auto_roam：私号禁游荡 + 空摆摊 ==")
    rc_stub = types.ModuleType("config")
    rc_stub.robot_auto_roam_when_idle = True
    rc_stub.robot_auto_roam_idle_sec = 90
    rc_stub.robot_auto_roam_map = "random"
    rc_stub.robot_auto_roam_mode = "default"
    _set_module("config", rc_stub)
    booth_stub = _BoothStub()
    _set_module("booth", booth_stub)
    rw_stub = _RWStub()
    _set_module("random_walk", rw_stub)
    rm_stub.val = False
    rm_stub.calls = []

    ns_a = {}
    for fn in ("_now_ms", "_cfg", "_task_pending", "_personal_idle_ok",
               "_tick_personal_booth", "is_idle_for_roam", "tick"):
        frag = _extract_func(SRC["auto_roam"], fn)
        if frag is None:
            ok("P2A:提取 %s" % fn, False)
            continue
        try:
            exec(compile(frag, "<ar>", "exec"), ns_a)
        except Exception as e:  # noqa
            ok("P2A:exec %s" % fn, False, repr(e))
    for m2 in re.finditer(r"^(_PERSONAL_BOOTH_\w+)\s*=\s*(\d+)", SRC["auto_roam"], re.M):
        ns_a[m2.group(1)] = int(m2.group(2))

    def _ro_idle(**kw):
        ro = types.SimpleNamespace()
        ro.m_logined = True
        ro.m_fight_state = False
        ro.m_mapid = 11
        ro.m_pose = [1000, 1200]
        ro.m_account = ["robotT"]
        for k, v in kw.items():
            setattr(ro, k, v)
        return ro

    idle_fn = ns_a.get("is_idle_for_roam")
    tick_fn = ns_a.get("tick")
    pb_fn = ns_a.get("_tick_personal_booth")

    if idle_fn is not None:
        rm_stub.val = True
        ok("P2A:is_idle_for_roam 私号全空闲 → False(禁自发游荡)",
           idle_fn(_ro_idle()) is False)
        rm_stub.val = False
        ok("CTRL:is_idle_for_roam 非私号全空闲 → True",
           idle_fn(_ro_idle()) is True)
        ok("CTRL:is_idle_for_roam 非私号抓鬼中 → False",
           idle_fn(_ro_idle(m_ghost=types.SimpleNamespace(enabled=True))) is False)
        ok("CTRL:is_idle_for_roam 非私号在队 → False",
           idle_fn(_ro_idle(m_team=types.SimpleNamespace(role="member"))) is False)
    else:
        ok("P2A:is_idle_for_roam 提取", False)

    if pb_fn is not None:
        now = 1000000
        # 1) 起摆: 私号空闲超过阈值 → booth_start(up_items=[], cell=当前坐标, mapid=当前图)
        booth_stub.calls = []
        booth_stub.ok = True
        ro1 = _ro_idle(m_auto_booth_idle_since=now - 200000, m_auto_booth_last_ms=0)
        r = pb_fn(ro1, now)
        c1 = booth_stub.calls[0] if booth_stub.calls else {}
        ok("P2A:私号空闲→空摆摊(booth_start, up_items=[], 当前图/坐标, hold=1800)",
           r is True and len(booth_stub.calls) == 1
           and c1.get("cmd") == "booth_start" and c1.get("up_items") == []
           and c1.get("mapid") == 11 and c1.get("cell") == [1000, 1200]
           and int(c1.get("hold_sec") or 0) == 1800, c1)
        ok("P2A:起摆后计时复位 + 记 last_ms(退避基准)",
           ro1.m_auto_booth_idle_since == 0 and ro1.m_auto_booth_last_ms == now)
        # 2) 最短间隔: 刚尝试过(<10min) → 不再起摆
        booth_stub.calls = []
        ro2 = _ro_idle(m_auto_booth_idle_since=now - 200000, m_auto_booth_last_ms=now - 1000)
        r = pb_fn(ro2, now)
        ok("P2A:两次起摆最短间隔 10 分钟(未到不重试)", r is False and not booth_stub.calls)
        # 3) 失败退避: dispatch ok=False → last_ms 记录(下次 ≥10min)
        booth_stub.calls = []
        booth_stub.ok = False
        ro3 = _ro_idle(m_auto_booth_idle_since=now - 200000, m_auto_booth_last_ms=0)
        r = pb_fn(ro3, now)
        ok("P2A:起摆失败(等级<50/无摊点等) → 记 last_ms 退避",
           r is False and ro3.m_auto_booth_last_ms == now, ro3.m_auto_booth_last_ms)
        booth_stub.ok = True
        # 4) 摆摊中 + 任务意图 → 先收摊让路
        booth_stub.calls = []
        ro4 = _ro_idle(m_booth=types.SimpleNamespace(enabled=True),
                       m_ghost=types.SimpleNamespace(enabled=True))
        r = pb_fn(ro4, now)
        ok("P2A:摆摊中+任务意图 → 收摊让路(booth_stop)",
           r is True and len(booth_stub.calls) == 1
           and booth_stub.calls[0].get("cmd") == "booth_stop", booth_stub.calls)
        # 5) 摆摊中无任务 → 保持(不动)
        booth_stub.calls = []
        ro5 = _ro_idle(m_booth=types.SimpleNamespace(enabled=True))
        r = pb_fn(ro5, now)
        ok("CTRL:摆摊中无任务 → 保持不动", r is False and not booth_stub.calls)
        # 6) 占用闸: 战斗中/采购中/真实游荡中 → 不起摆
        booth_stub.calls = []
        ro6 = _ro_idle(m_fight_state=True, m_auto_booth_idle_since=now - 200000)
        r = pb_fn(ro6, now)
        ok("P2A:战斗帧内不起摆(等空闲)", r is False and not booth_stub.calls)
        ro7 = _ro_idle(m_auto_booth_idle_since=now - 200000,
                       m_quest=types.SimpleNamespace(shop_ctx=object()))
        r = pb_fn(ro7, now)
        ok("P2A:商店采购占用中不起摆", r is False and not booth_stub.calls)
        ro8 = _ro_idle(m_auto_booth_idle_since=now - 200000,
                       m_collect_walk=types.SimpleNamespace(enabled=True))
        r = pb_fn(ro8, now)
        ok("CTRL:真实游荡进行中不起摆", r is False and not booth_stub.calls)
    else:
        ok("P2A:_tick_personal_booth 提取", False)

    if tick_fn is not None:
        # 7) tick 级: 私号 → 走空摆摊
        rm_stub.val = True
        booth_stub.calls = []
        booth_stub.ok = True
        ro9 = _ro_idle(m_auto_booth_idle_since=0)
        r = tick_fn(ro9, 2000000)
        ok("P2A:tick 私号 → 不游荡不立即起摆(先起计时)",
           r is False and not booth_stub.calls and not rw_stub.calls
           and ro9.m_auto_booth_idle_since == 2000000)
        ro10 = _ro_idle(m_auto_booth_idle_since=2000000 - 200000)
        r = tick_fn(ro10, 2000000)
        ok("P2A:tick 私号空闲够 → 空摆摊(而非游荡)",
           r is True and len(booth_stub.calls) == 1 and not rw_stub.calls,
           (booth_stub.calls, rw_stub.calls))
        # 8) tick 级: 非私号 → 原游荡行为不受影响
        rm_stub.val = False
        rw_stub.calls = []
        booth_stub.calls = []
        ro11 = _ro_idle(m_auto_roam_idle_since=2000000 - 200000, m_auto_roam_last_ms=0)
        r = tick_fn(ro11, 2000000)
        ok("CTRL:tick 非私号 → 原自发游荡照旧(random_walk)",
           r is True and len(rw_stub.calls) == 1 and not booth_stub.calls,
           (rw_stub.calls, booth_stub.calls))
    else:
        ok("P2A:tick 提取", False)

    # ================================================================ 5) 结果
    def _grp(prefix, items=None):
        src = PASS + FAIL if items is None else items
        return [n for n in src if n.startswith(prefix)]

    p2 = _grp("P2S:") + _grp("P2G:") + _grp("P2R:") + _grp("P2A:")
    p2_fail = [n for n in FAIL if n.startswith(("P2S:", "P2G:", "P2R:", "P2A:"))]
    ctrl_fail = _grp("CTRL:", FAIL)
    total = len(PASS) + len(FAIL)

    if expect_bad:
        sens_ok = (len(p2_fail) == len(p2) and len(p2) >= 35 and not ctrl_fail)
        print("\n=== 坏版灵敏度: %s ===" % ("OK" if sens_ok else "BAD"))
        print("    P2* 预期失败: %d/%d（应全部 FAIL）" % (len(p2_fail), len(p2)))
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
