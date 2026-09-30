# -*- coding: utf-8 -*-
"""私人池 Wave 2（机器人端）**独立复核**断言 —— general-purpose-6, 2026-09-30

与作者自检 tools/personal_wave2_selftest.py 的区别（独立性）：
  1. 4 个模块走**整文件真实装载**（auto_summon/bag_ops/encourage_claim/pre_daily +
     robot_mgr + auto_roam），在真实模块对象上打探针/桩，而非抽取函数体 exec；
  2. 8 闸额外做 **AST 结构断言**（闸是函数第一条可执行语句, 早于任何状态修改）；
  3. 投递覆盖作者未测的边界（accounts=None / None 元素 / 非字符串元素 / reload 丢 set）；
  4. 让路覆盖 4 种任务意图（quest/ghost/share_daily/team）、10min 边界 599999/600000；
  5. 吃药链**行为反例**（私号照样吃）+ 两处 main_tester 级漏网行为只读演示（F 组 INFO）。

用法:
  python tools/personal_wave2_indep_review.py [script_dir]
  python tools/personal_wave2_indep_review.py [script_dir] --bak-suffix .bak_20260930_personalwave2 --expect-bad
只读: 不改任何生产文件; 装载的模块仅在本进程内。
"""
import ast
import os
import re
import sys
import types
import importlib
import importlib.util

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DEFAULT_SCRIPT = os.path.join(ROOT, "deploy", "zones", "prod-240-2300", "script")

PASS, FAIL, INFO = [], [], []


def ok(name, cond, detail=""):
    (PASS if cond else FAIL).append(name)
    print("  [%s] %s%s" % ("OK" if cond else "FAIL", name,
                           (" -- " + str(detail)) if detail else ""))


def info(name, cond, detail=""):
    INFO.append((name, bool(cond)))
    print("  [%s] %s%s" % ("i-OK" if cond else "i-NO", name,
                           (" -- " + str(detail)) if detail else ""))


# ------------------------------------------------------------------ 探针
class BombBool(object):
    def __bool__(self):
        raise RuntimeError("REVIEW_BOMB_TIDY")


class BombAttr(object):
    @property
    def bag_cleanup_tried(self):
        raise RuntimeError("REVIEW_BOMB_ATTR")


class Rec(object):
    def __init__(self, ret=None):
        self.calls = []
        self.ret = ret

    def __call__(self, *a, **k):
        self.calls.append((a, k))
        return self.ret


class RecDict(dict):
    def __init__(self, *a, **k):
        dict.__init__(self, *a, **k)
        self.gets = []
        self.items_calls = 0

    def get(self, k, d=None):
        self.gets.append(k)
        return dict.get(self, k, d)

    def items(self):
        self.items_calls += 1
        return dict.items(self)


class AttrProbe(object):
    """记录属性读取次数的对象。"""
    def __init__(self, **kw):
        object.__setattr__(self, "_reads", [])
        for k, v in kw.items():
            object.__setattr__(self, k, v)

    def __getattr__(self, item):
        if item.startswith("_"):
            raise AttributeError(item)
        reads = object.__getattribute__(self, "_reads")
        reads.append(item)
        return None


class RmStub(object):
    """可编程 robot_mgr 桩（记录入参身份）。"""
    def __init__(self):
        self.val = False
        self.calls = []

    def is_personal(self, robot_object):
        self.calls.append(robot_object)
        return self.val


def _stub(name, **attrs):
    m = sys.modules.get(name)
    if m is None or not isinstance(m, types.ModuleType):
        m = types.ModuleType(name)
        sys.modules[name] = m
    for k, v in attrs.items():
        setattr(m, k, v)
    return m


def _sn(args, obj):
    return types.SimpleNamespace(**dict(zip(args, obj)))


def main():
    args = sys.argv[1:]
    script_dir = None
    suffix = ""
    expect_bad = False
    i = 0
    while i < len(args):
        a = args[i]
        if a == "--bak-suffix" and i + 1 < len(args):
            suffix = args[i + 1]
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
    if script_dir not in sys.path:
        sys.path.insert(0, script_dir)   # 与机器人运行环境一致; importlib.reload 需要
    print("script_dir : %s" % script_dir)
    print("suffix     : %r  expect_bad=%s" % (suffix, expect_bad))

    def read(mod):
        p = os.path.join(script_dir, mod + ".py" + suffix)
        if not os.path.exists(p):
            return None, p
        return open(p, encoding="utf-8", errors="replace").read(), p

    def load_real(mod):
        path = os.path.join(script_dir, mod + ".py" + suffix)
        import importlib.machinery as _mach
        loader = _mach.SourceFileLoader(mod, path)
        spec = importlib.util.spec_from_loader(mod, loader)
        m = importlib.util.module_from_spec(spec)
        sys.modules[mod] = m
        loader.exec_module(m)
        return m

    SRC = {}
    for mod in ("robot_mgr", "auto_roam", "daily_ghost", "auto_summon", "bag_ops",
                "encourage_claim", "pre_daily", "share_daily"):
        s, p = read(mod)
        if s is None:
            print("[FATAL] 缺文件 %s" % p)
            return 2
        SRC[mod] = s

    # ---------------- 依赖桩（先装, 供整文件装载） ----------------
    cfg = _stub("config")
    p3 = _stub("protocol3", C2S_USEITEM=80218, C2S_USE_ITEM=80218,
               C2S_DELITEM=80216, C2S_PICK_ALL_ITEM=80217)
    _stub("diag", log=lambda *a, **k: None)
    _stub("error", NET_CLOSED=-1, ROBOT_DELETED=-2)
    _stub("quest_engine")
    _stub("quest_state", ST_DIALOG=99)
    _stub("keys")
    _stub("client", g_item_data_dict={}, apply_pending_cmds=lambda *a, **k: None,
          emit=lambda *a, **k: None)
    _stub("ctrl_client", emit=lambda *a, **k: None)
    _stub("shop_errand", status=lambda ro: {"active": False, "result": None},
          consume=lambda *a, **k: None, stop=lambda *a, **k: None,
          start=lambda *a, **k: {"ok": False, "result": "stub"},
          compute_need=lambda *a, **k: 0, bag_count=lambda *a, **k: 0)
    _stub("baidu", Baidu="b", BaiduRefer="r", BaiduGet="g")
    _stub("cnet", cnet=object)
    _stub("cnetwork", baidu_http_request=lambda *a, **k: ("", "0"))
    _stub("csv_file", read_csv_file_to_list=lambda *a, **k: None)
    _stub("robot", Robot=object)
    _stub("robot_def", robot_surname_list=[["张"]], robot_name_list=[["三"]])
    _stub("robot_operator", send_handshake=lambda *a, **k: None)

    rm_real = load_real("robot_mgr")            # 真模块（B 组）
    ar_real = load_real("auto_roam")            # 真模块（C 组）
    as_real = load_real("auto_summon")          # 真模块（D/E 组）
    bo_real = load_real("bag_ops")              # 真模块（D/F 组）
    ec_real = load_real("encourage_claim")      # 真模块（D 组）
    pd_real = load_real("pre_daily")            # 真模块（D 组）

    # ================================================================ A 文件事实（INFO）
    print("== A) 文件/备份/语法 ==")
    if not suffix:
        import hashlib
        xm_dir = os.path.join(ROOT, "..", "xm", "2d-xiyou-server", "robot",
                              "deploy", "single_robot_zy", "script")
        xm_dir = os.path.abspath(xm_dir)
        for mod in SRC:
            p1 = os.path.join(script_dir, mod + ".py" + suffix)
            p2 = os.path.join(xm_dir, mod + ".py")
            a = hashlib.sha1(open(p1, "rb").read()).hexdigest()
            b = ""
            if os.path.exists(p2):
                b = hashlib.sha1(open(p2, "rb").read()).hexdigest()
            info("A:双副本 sha1 相等 %s.py" % mod, a == b, "%s / %s" % (a[:12], b[:12]))
        for mod in SRC:
            for d, tag in ((script_dir, "zy"), (xm_dir, "xm")):
                info("A:备份存在 %s/%s" % (tag, mod),
                     os.path.exists(os.path.join(d, mod + ".py" + ".bak_20260930_personalwave2")))
    for mod in SRC:
        try:
            compile(SRC[mod], mod, "exec")
            good = True
        except Exception:
            good = False
        info("A:%s 语法编译通过" % mod, good)

    # ================================================================ B robot_mgr
    print("== B) robot_mgr: personal 投递 + is_personal（真模块） ==")
    mgr = rm_real.g_mgr
    ro_stub = types.SimpleNamespace(m_account={})

    ok("P2R:g_personal_accounts 为 set + is_personal 存在",
       isinstance(getattr(rm_real, "g_personal_accounts", None), set)
       and callable(getattr(rm_real, "is_personal", None)))

    if isinstance(getattr(rm_real, "g_personal_accounts", None), set):
        r1 = mgr.manage_robots("personal", ["robotA", ["robotB", "pw"], " ", "robotA"])
        ok("P2R:投递=全量覆盖(去重/对偶/空串)",
           rm_real.g_personal_accounts == {"robotA", "robotB"}
           and isinstance(r1, dict) and r1.get("personal") == 2
           and set(r1.keys()) >= {"added", "removed"}, (rm_real.g_personal_accounts, r1))
        mgr.manage_robots("personal", ["robotC"])
        ok("P2R:再推=全量替换", rm_real.g_personal_accounts == {"robotC"},
           rm_real.g_personal_accounts)
        rm_real.g_personal_accounts = {"robotZZ"}
        mgr.manage_robots("personal", [])
        ok("P2R:空列表=清空", rm_real.g_personal_accounts == set())
        rm_real.g_personal_accounts = {"robotZZ"}
        mgr.manage_robots("personal", None)
        ok("P2R:accounts=None=清空(容错)", rm_real.g_personal_accounts == set())
        # 边界备注（不入断言判定）
        mgr.manage_robots("personal", [None, 12345])
        info("B:边界记录 None→'None' / int→'12345'", True,
             sorted(rm_real.g_personal_accounts))
        mgr.manage_robots("personal", [])
    else:
        ok("P2R:投递=全量覆盖(去重/对偶/空串)", False, "no g_personal_accounts")
        ok("P2R:再推=全量替换", False)
        ok("P2R:空列表=清空", False)
        ok("P2R:accounts=None=清空(容错)", False)

    # 未知 action benign（坏版也应 PASS: CTRL）
    keep = set(getattr(rm_real, "g_personal_accounts", set()))
    r2 = mgr.manage_robots("frobnicate", ["robotX"])
    ok("CTRL:未知 action benign(名单不动+不抛)",
       isinstance(r2, dict) and "added" in r2
       and set(getattr(rm_real, "g_personal_accounts", set())) == keep, r2)

    # is_personal 行为
    ip = getattr(rm_real, "is_personal", None)
    if ip is not None:
        try:
            rm_real.g_personal_accounts = {"robotA"}
            ok("P2R:is_personal 成员/非成员",
               ip(types.SimpleNamespace(m_account=["robotA", "pw"])) is True
               and ip(types.SimpleNamespace(m_account=["robotZ"])) is False)
            ok("P2R:is_personal 异常兜底(缺 m_account/None/空列表)",
               ip(types.SimpleNamespace()) is False
               and ip(types.SimpleNamespace(m_account=None)) is False
               and ip(types.SimpleNamespace(m_account=[])) is False)
            ok("P2R:is_personal 判据=账号名(m_account[0], 非对象身份)",
               ip(types.SimpleNamespace(m_account=["robotA"])) is True
               and ip(types.SimpleNamespace(m_account=("robotA",))) is True)
        except Exception as e:
            ok("P2R:is_personal 成员/非成员", False, repr(e))
            ok("P2R:is_personal 异常兜底(缺 m_account/None/空列表)", False)
            ok("P2R:is_personal 判据=账号名(m_account[0], 非对象身份)", False)
        # reload 语义（热更/重启丢 set 的实证）
        rm_real.g_personal_accounts = {"robotA"}
        rm2 = importlib.reload(rm_real)
        lost = getattr(rm2, "g_personal_accounts", None) == set()
        ok("P2R:reload 后 set 复位为空(=丢标记, 需中控重推)",
           lost, getattr(rm2, "g_personal_accounts", None))
        rm2.g_mgr.manage_robots("personal", ["robotA"])
        ok("P2R:重推后恢复(is_personal 再次 True)",
           rm2.is_personal(types.SimpleNamespace(m_account=["robotA"])) is True)
        rm_real = rm2
    else:
        ok("P2R:is_personal 成员/非成员", False, "no is_personal")
        ok("P2R:is_personal 异常兜底(缺 m_account/None/空列表)", False)
        ok("P2R:is_personal 判据=账号名(m_account[0], 非对象身份)", False)
        ok("P2R:reload 后 set 复位为空(=丢标记, 需中控重推)", False)
        ok("P2R:重推后恢复(is_personal 再次 True)", False)

    # ================================================================ C auto_roam
    print("== C) auto_roam: 私号禁游荡 + 空摆摊（真模块） ==")
    rm_stub = RmStub()
    sys.modules["robot_mgr"] = rm_stub          # C 组用可控桩
    cfg.robot_auto_roam_when_idle = True
    cfg.robot_auto_roam_idle_sec = 90
    cfg.robot_auto_roam_map = "random"
    cfg.robot_auto_roam_mode = "default"
    booth_rec = Rec({"type": "booth_reply", "ok": True})

    class BoothStub(object):
        def __init__(self):
            self.calls = []
            self.ok = True
            self.raise_on = False

        def dispatch_cmd(self, ro, cmd):
            if self.raise_on:
                raise RuntimeError("REVIEW_BOOTH_RAISE")
            self.calls.append(cmd)
            if cmd.get("cmd") == "booth_stop":
                return {"ok": True, "cmd": "booth_stop"}
            return {"ok": bool(self.ok), "cmd": cmd.get("cmd")}

    booth_stub = BoothStub()
    sys.modules["booth"] = booth_stub
    rw_rec = Rec({"result": "ok"})
    sys.modules["random_walk"] = types.SimpleNamespace(dispatch_cmd=rw_rec)

    def ro_idle(**kw):
        ro = types.SimpleNamespace()
        ro.m_logined = True
        ro.m_fight_state = False
        ro.m_mapid = 11
        ro.m_pose = [1000, 1200]
        ro.m_account = ["robotT"]
        ro.__dict__.update(kw)
        return ro

    NOW = 10 ** 9
    rm_stub.val = True
    ok("P2A:is_idle_for_roam 私号全空闲→False",
       ar_real.is_idle_for_roam(ro_idle()) is False)
    rm_stub.val = False
    ok("CTRL:is_idle_for_roam 非私号空闲→True",
       ar_real.is_idle_for_roam(ro_idle()) is True)

    # tick 路由
    PB_OK = callable(getattr(ar_real, "_tick_personal_booth", None))
    rm_stub.val = True
    rw_rec.calls = []
    booth_stub.calls = []
    r0 = ro_idle()
    ar_real.tick(r0, NOW)
    ok("P2A:tick 私号→不走游荡(random_walk 0 次)+先起计时",
       PB_OK and not rw_rec.calls and getattr(r0, "m_auto_booth_idle_since", None) == NOW)
    r1x = ro_idle(m_auto_booth_idle_since=NOW - 200000)
    ar_real.tick(r1x, NOW)
    ok("P2A:tick 私号空闲够→空摆摊(booth_start, 非 random_walk)",
       PB_OK and len(booth_stub.calls) == 1 and booth_stub.calls[0].get("cmd") == "booth_start"
       and not rw_rec.calls, (booth_stub.calls, rw_rec.calls))
    c = booth_stub.calls[0] if len(booth_stub.calls) == 1 else {}
    ok("P2A:空摆摊参数(up_items=[]/当前图|坐标/hold=1800/scan 32x8)",
       PB_OK and c.get("up_items") == [] and c.get("mapid") == 11
       and c.get("cell") == [1000, 1200] and c.get("hold_sec") == 1800
       and c.get("scan") == {"cx": 1000, "cy": 1200, "step": 32, "max": 8}, c)
    rm_stub.val = False
    rw_rec.calls = []
    booth_stub.calls = []
    ar_real.tick(ro_idle(m_auto_roam_idle_since=NOW - 200000, m_auto_roam_last_ms=0), NOW)
    ok("CTRL:tick 非私号→原游荡照旧",
       len(rw_rec.calls) == 1 and not booth_stub.calls)

    # 让路: 4 种任务意图
    rm_stub.val = True
    yield_cases = {
        "quest.active": dict(m_quest=types.SimpleNamespace(active=True)),
        "ghost.enabled": dict(m_ghost=types.SimpleNamespace(enabled=True)),
        "share_daily.enabled": dict(m_share_daily=types.SimpleNamespace(enabled=True)),
        "team.member": dict(m_team=types.SimpleNamespace(role="member")),
    }
    all_yield = True
    for tag, kw in yield_cases.items():
        booth_stub.calls = []
        r = ar_real.tick(ro_idle(m_booth=types.SimpleNamespace(enabled=True), **kw), NOW)
        if not (r is True and len(booth_stub.calls) == 1
                and booth_stub.calls[0].get("cmd") == "booth_stop"):
            all_yield = False
            print("      [miss] %s -> %s %s" % (tag, r, booth_stub.calls))
    ok("P2A:摆摊中 4 种任务意图(quest/ghost/share_daily/team)→先收摊让路", all_yield)
    booth_stub.calls = []
    r = ar_real.tick(ro_idle(m_booth=types.SimpleNamespace(enabled=True)), NOW)
    ok("CTRL:摆摊中无任务→不收摊", r is False and not booth_stub.calls)

    # idle_since 复位
    ro_r = ro_idle(m_fight_state=True, m_auto_booth_idle_since=NOW - 200000)
    r = ar_real.tick(ro_r, NOW)
    ok("P2A:非空闲(战斗)→idle 计时复位 0",
       PB_OK and r is False and getattr(ro_r, "m_auto_booth_idle_since", None) == 0)

    # 10min 边界
    booth_stub.calls = []
    r = ar_real.tick(ro_idle(m_auto_booth_idle_since=NOW - 200000,
                             m_auto_booth_last_ms=NOW - 599999), NOW)
    ok("P2A:距上次起摆 599999ms(<10min)→不重试",
       PB_OK and r is False and not booth_stub.calls)
    r = ar_real.tick(ro_idle(m_auto_booth_idle_since=NOW - 200000,
                             m_auto_booth_last_ms=NOW - 600000), NOW)
    ok("P2A:距上次起摆 600000ms(=10min)→允许重试",
       PB_OK and r is True and len(booth_stub.calls) == 1)

    # 失败退避 + 异常容忍 + getattr 兜底
    booth_stub.ok = False
    booth_stub.calls = []
    ro_f = ro_idle(m_auto_booth_idle_since=NOW - 200000, m_auto_booth_last_ms=0)
    r = ar_real.tick(ro_f, NOW)
    ok("P2A:起摆失败(ok=False)→记 last_ms 退避",
       PB_OK and r is False and getattr(ro_f, "m_auto_booth_last_ms", None) == NOW)
    booth_stub.ok = True
    booth_stub.raise_on = True
    ro_e = ro_idle(m_auto_booth_idle_since=NOW - 200000, m_auto_booth_last_ms=0)
    try:
        r = ar_real.tick(ro_e, NOW)
        ok("P2A:dispatch 异常→吞掉不抛+按失败退避",
           PB_OK and r is False and getattr(ro_e, "m_auto_booth_last_ms", None) == NOW)
    except Exception as e:
        ok("P2A:dispatch 异常→吞掉不抛+按失败退避", False, repr(e))
    booth_stub.raise_on = False
    ro_g = ro_idle(m_auto_booth_idle_since=NOW - 200000)   # 缺 last_ms 字段
    try:
        ar_real.tick(ro_g, NOW)
        ok("P2A:新字段 getattr 兜底(旧实例无 m_auto_booth_*)", PB_OK)
    except Exception as e:
        ok("P2A:新字段 getattr 兜底(旧实例无 m_auto_booth_*)", False, repr(e))

    # 常量与 booth 契约
    hold = int(getattr(ar_real, "_PERSONAL_BOOTH_HOLD_SEC", 0) or 0)
    booth_src, _ = read("booth")
    m_hold = re.search(r"_HOLD_MAX_SEC\s*=\s*(\d+)", booth_src or "")
    ok("P2A:空摆摊 hold 常量=1800 且 = booth._HOLD_MAX_SEC",
       hold == 1800 and m_hold and int(m_hold.group(1)) == hold, (hold, m_hold and m_hold.group(1)))
    ok("P2A:scan 键(cx/cy/step/max)与 booth._build_scan_points 契约一致",
       all(k in (booth_src or "") for k in ('"cx"', '"cy"', '"step"', '"max"')))
    # booth 占位(_booth_dummy)不算占用 → 可起摆（新功能语义, P2A）
    r = ar_real.tick(ro_idle(m_auto_booth_idle_since=NOW - 200000,
                             m_collect_walk=types.SimpleNamespace(enabled=True, _booth_dummy=True)), NOW)
    _piok = getattr(ar_real, "_personal_idle_ok", None)
    ok("P2A:booth 占位(_booth_dummy)不算占用→可起摆",
       PB_OK and (r is True or (_piok is not None and _piok(ro_idle(
           m_collect_walk=types.SimpleNamespace(enabled=True, _booth_dummy=True))) is True)))

    # ================================================================ D 8 闸
    print("== D) 8 闸: 私号零触碰 + 非私号穿透（真模块/抽取+AST） ==")

    def ast_gate_first(src, fn, ret_repr):
        tree = ast.parse(src)
        for node in ast.walk(tree):
            if isinstance(node, ast.FunctionDef) and node.name == fn:
                body = list(node.body)
                if body and isinstance(body[0], ast.Expr) and isinstance(
                        body[0].value, ast.Constant) and isinstance(body[0].value.value, str):
                    body = body[1:]
                if not body or not isinstance(body[0], ast.Try):
                    return False
                tr = body[0]
                has_imp = any(isinstance(s, (ast.Import, ast.ImportFrom))
                              and (getattr(s, "module", "") == "robot_mgr"
                                   or any(al.name == "robot_mgr" for al in getattr(s, "names", [])))
                              for s in tr.body)
                has_if = False
                for s in tr.body:
                    if isinstance(s, ast.If):
                        t = s.test
                        if isinstance(t, ast.Call) and isinstance(t.func, ast.Attribute) \
                                and t.func.attr == "is_personal":
                            has_if = True
                            break
                return bool(has_imp and has_if)
        return None  # 函数不存在

    gates = [
        ("daily_ghost", "__tidy_bag", "0"),
        ("auto_summon", "_try_bag_cleanup", "False"),
        ("bag_ops", "auto_open_gifts", "0"),
        ("bag_ops", "auto_equip_best_items", "(0, 0)"),
        ("bag_ops", "auto_use_bag_expanders", "0"),
        ("encourage_claim", "_use_boxes", "0, False"),
        ("pre_daily", "_use_card", "False"),
        ("share_daily", "__bag_cleanup_retry", "False"),
    ]
    for mod, fn, ret in gates:
        r = ast_gate_first(SRC[mod], fn, ret)
        ok("P2G:AST 首语句闸 %s.%s" % (mod, fn), r is True, r)

    def extract(src, fn):
        lines = src.splitlines()
        pat = re.compile(r"^(\t*)def %s\(" % re.escape(fn))
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

    def exec_frag(src, fn, extra=None):
        frag = extract(src, fn)
        ns = {}
        if frag:
            exec(compile(frag, "<frag>", "exec"), ns)
        if extra:
            for k, v in extra.items():
                ns[k] = v
        return ns

    rm_stub.val = True

    # D1 daily_ghost.__tidy_bag（抽取）
    ns = exec_frag(SRC["daily_ghost"], "__tidy_bag", {"_ITEM_TIDYING": BombBool()})
    try:
        r = ns["__tidy_bag"](types.SimpleNamespace(m_account=["P"]))
        ok("P2G:daily_ghost.__tidy_bag 私号→0(真实体未触碰)", r == 0)
    except Exception as e:
        ok("P2G:daily_ghost.__tidy_bag 私号→0(真实体未触碰)", False, repr(e))
    rm_stub.val = False
    ns2 = exec_frag(SRC["daily_ghost"], "__tidy_bag", {"_ITEM_TIDYING": BombBool()})
    try:
        ns2["__tidy_bag"](types.SimpleNamespace(m_account=["P"]))
        ok("CTRL:daily_ghost.__tidy_bag 非私号穿透(到达真实体)", False, "未触发")
    except RuntimeError as e:
        ok("CTRL:daily_ghost.__tidy_bag 非私号穿透(到达真实体)", "REVIEW_BOMB_TIDY" in str(e))

    # D2 auto_summon._try_bag_cleanup（真模块）
    rm_stub.val = True
    rm_stub.calls = []
    cfg.robot_bag_cleanup = True
    st = RecDict()
    ro_d2 = types.SimpleNamespace(m_account=["P"])
    r = as_real._try_bag_cleanup(ro_d2, st, 0)
    ok("P2G:auto_summon._try_bag_cleanup 私号→False+state 零触碰",
       r is False and st.gets == [], (r, st.gets))
    ok("P2G:auto_summon._try_bag_cleanup 闸以 robot_object 入参",
       len(rm_stub.calls) == 1 and rm_stub.calls[0] is ro_d2, len(rm_stub.calls))
    rm_stub.val = False
    st2 = RecDict()
    as_real._try_bag_cleanup(types.SimpleNamespace(m_account=["P"]), st2, 0)
    ok("CTRL:auto_summon._try_bag_cleanup 非私号穿透(state 被读)",
       len(st2.gets) >= 1, st2.gets)

    # D3 bag_ops.auto_open_gifts（真模块）
    rm_stub.val = True
    bag = RecDict({"999001": [1, 1, 0x2010]})
    r = bo_real.auto_open_gifts(types.SimpleNamespace(m_account=["P"]), bag)
    ok("P2G:bag_ops.auto_open_gifts 私号→0+bag 零触碰",
       r == 0 and bag.gets == [] and bag.items_calls == 0,
       (r, bag.gets, bag.items_calls))
    rm_stub.val = False
    bag2 = RecDict({"999001": [1, 1, 0x2010]})
    bo_real.auto_open_gifts(types.SimpleNamespace(m_account=["P"]), bag2)
    ok("CTRL:bag_ops.auto_open_gifts 非私号穿透(遍历 bag)", bag2.items_calls >= 1,
       bag2.items_calls)

    # D4 bag_ops.auto_equip_best_items（真模块, drop 记录探针）
    rm_stub.val = True
    dpo = Rec(0)
    orig_dpo = bo_real.drop_pursuit_orders
    bo_real.drop_pursuit_orders = dpo
    r = bo_real.auto_equip_best_items(types.SimpleNamespace(m_account=["P"]), {})
    ok("P2G:bag_ops.auto_equip_best_items 私号→(0,0)+drop 零调用",
       r == (0, 0) and not dpo.calls, (r, len(dpo.calls)))
    rm_stub.val = False
    dpo.calls = []
    bo_real.auto_equip_best_items(types.SimpleNamespace(m_account=["P"]), {})
    ok("CTRL:bag_ops.auto_equip_best_items 非私号穿透(drop 被调用)", len(dpo.calls) >= 1)
    bo_real.drop_pursuit_orders = orig_dpo

    # D5 bag_ops.auto_use_bag_expanders（真模块, _now_ms 记录探针）
    rm_stub.val = True
    nw = Rec(123456)
    orig_nw = bo_real._now_ms
    bo_real._now_ms = nw
    r = bo_real.auto_use_bag_expanders(types.SimpleNamespace(m_account=["P"]), {})
    ok("P2G:bag_ops.auto_use_bag_expanders 私号→0+_now_ms 零调用",
       r == 0 and not nw.calls, (r, len(nw.calls)))
    rm_stub.val = False
    nw.calls = []
    bo_real.auto_use_bag_expanders(types.SimpleNamespace(m_account=["P"]), {})
    ok("CTRL:bag_ops.auto_use_bag_expanders 非私号穿透", len(nw.calls) >= 1)
    bo_real._now_ms = orig_nw

    # D6 encourage_claim._use_boxes（真模块, 属性读探针）
    rm_stub.val = True
    ro_ap = AttrProbe(m_account=["P"])
    r = ec_real._use_boxes(ro_ap)
    ok("P2G:encourage_claim._use_boxes 私号→(0,False)+属性零读",
       r == (0, False) and ro_ap._reads == [], (r, ro_ap._reads))
    rm_stub.val = False
    ro_ap2 = AttrProbe(m_account=["P"])
    ec_real._use_boxes(ro_ap2)
    ok("CTRL:encourage_claim._use_boxes 非私号穿透(属性被读)",
       len(ro_ap2._reads) >= 1, ro_ap2._reads)

    # D7 pre_daily._use_card（真模块, auto_summon 桩替换）
    as_stub = types.SimpleNamespace(get_state=Rec({}), use_buff_card=Rec(True))
    prev_as = sys.modules.get("auto_summon")
    sys.modules["auto_summon"] = as_stub
    rm_stub.val = True
    cfg.robot_claim_double_exp_use_card = True
    r = pd_real._use_card(types.SimpleNamespace(m_account=["P"]), 0)
    ok("P2G:pre_daily._use_card 私号→False+真实体零触碰",
       r is False and not as_stub.use_buff_card.calls and not as_stub.get_state.calls)
    rm_stub.val = False
    as_stub.use_buff_card.calls = []
    as_stub.get_state.calls = []
    r = pd_real._use_card(types.SimpleNamespace(m_account=["P"]), 0)
    ok("CTRL:pre_daily._use_card 非私号穿透(真实体被调用)",
       len(as_stub.use_buff_card.calls) >= 1 or len(as_stub.get_state.calls) >= 1)
    sys.modules["auto_summon"] = prev_as

    # D8 share_daily.__bag_cleanup_retry（抽取, 炸弹属性）
    rm_stub.val = True
    ns = exec_frag(SRC["share_daily"], "__bag_cleanup_retry")
    try:
        r = ns["__bag_cleanup_retry"](types.SimpleNamespace(m_account=["P"]), BombAttr())
        ok("P2G:share_daily.__bag_cleanup_retry 私号→False(真实体未触碰)", r is False)
    except Exception as e:
        ok("P2G:share_daily.__bag_cleanup_retry 私号→False(真实体未触碰)", False, repr(e))
    rm_stub.val = False
    ns2 = exec_frag(SRC["share_daily"], "__bag_cleanup_retry")
    try:
        ns2["__bag_cleanup_retry"](types.SimpleNamespace(m_account=["P"]), BombAttr())
        ok("CTRL:share_daily.__bag_cleanup_retry 非私号穿透", False, "未触发")
    except RuntimeError as e:
        ok("CTRL:share_daily.__bag_cleanup_retry 非私号穿透", "REVIEW_BOMB_ATTR" in str(e))

    # ================================================================ E 吃药链反例（CTRL）
    print("== E) 吃药链反例: 私号照样吃（CTRL 组, 坏版应同样 PASS） ==")
    rm_stub.val = True   # 私号身份
    ro = types.SimpleNamespace(
        m_account=["P"], m_cur_hp=100, m_max_hp=1000, m_cur_mp=500, m_max_mp=500,
        m_bag_cache={102007: [555001, 3, 0x2010]}, m_fight_state=False)
    sent = []

    def _send(msg, args=None):
        sent.append((msg, args))
        return 0
    ro.send_message = _send
    st = {}
    r = as_real._try_role_heal(ro, st, 100000)
    ok("CTRL:私号 _try_role_heal 仍发 C2S_USEITEM(吃药保留)",
       r is True and sent and sent[0][0] == 80218 and sent[0][1] == [555001],
       (r, sent))
    st2 = {}
    r2 = as_real.heal_tick(ro, 200000)
    ok("CTRL:私号 heal_tick 仍吃药(返回≥1)",
       r2 >= 1 and len(sent) >= 2, (r2, len(sent)))
    # share_daily.tick 内服用: 静态 + 无闸
    sd_tick = extract(SRC["share_daily"], "tick")
    ok("CTRL:share_daily.tick 调 auto_summon.heal_tick 且无机密闸",
       sd_tick is not None and "heal_tick" in sd_tick and "is_personal" not in sd_tick)
    frag_uh = extract(SRC["daily_ghost"], "__use_heal_item")
    ok("CTRL:daily_ghost.__use_heal_item 无 is_personal 闸(且仍发 USEITEM)",
       frag_uh is not None and "is_personal" not in frag_uh and "USEITEM" in frag_uh,
       frag_uh is None)

    # ================================================================ F 漏网行为演示（INFO）
    print("== F) 漏网行为演示（INFO, 不入判定） ==")
    sent2 = []
    ro2 = types.SimpleNamespace(m_account=["P"], m_fight_state=False,
                                m_bag_cache={110173: [700001, 1, 0x2010]})
    ro2.send_message = lambda msg, args=None: (sent2.append((msg, args)), 0)[1]
    prev_client = sys.modules.get("client")
    sys.modules["client"] = types.SimpleNamespace(g_item_data_dict={})
    rm_stub.val = True
    n = bo_real.tick_pursuit_order_cleanup(ro2, 500000)
    info("F:tick_pursuit_order_cleanup 对私号仍发丢追捕令(C2S_DELITEM)",
         n == 1 and sent2 and sent2[0][0] == 80216, (n, sent2))
    if prev_client is not None:
        sys.modules["client"] = prev_client
    # 拾取栏清理
    ns = exec_frag(SRC["daily_ghost"], "tick_pick_pocket_cleanup",
                   {"__now_ms": lambda: 600000, "__log": lambda *a, **k: None,
                    "protocol3": p3,
                    "_PICK_CLEAN_MS": 60000, "_PICK_STUCK_LIMIT": 5,
                    "_PICK_BACKOFF_MS": 600000, "_PICK_FULL_SIGNAL_MS": 600000})
    sent3 = []
    ro3 = types.SimpleNamespace(m_account=["P"], m_logined=True, m_fight_state=False,
                                m_bag_cache={"x": [1, 1, 0x6460]})
    ro3.send_message = lambda msg, args=None: (sent3.append((msg, args)), 0)[1]
    ok3 = False
    try:
        ok3 = ns["tick_pick_pocket_cleanup"](ro3, 600000) is True
    except Exception as e:
        print("      [F-detail] pick cleanup raised:", repr(e))
    info("F:tick_pick_pocket_cleanup 对私号仍发 C2S_PICK_ALL_ITEM",
         ok3 and sent3 and sent3[0][0] == 80217, (ok3, sent3))

    # 恢复真模块引用（防跨段污染）
    sys.modules["robot_mgr"] = rm_real

    # ================================================================ 结果
    p2 = [n for n in PASS + FAIL if n.startswith(("P2S:", "P2G:", "P2R:", "P2A:"))]
    p2_fail = [n for n in FAIL if n.startswith(("P2S:", "P2G:", "P2R:", "P2A:"))]
    ctrl_fail = [n for n in FAIL if n.startswith("CTRL:")]
    total = len(PASS) + len(FAIL)

    if expect_bad:
        sens = (len(p2_fail) == len(p2) and len(p2) >= 40 and not ctrl_fail)
        print("\n=== 坏版灵敏度: %s ===" % ("OK" if sens else "BAD"))
        print("    P2* 预期失败: %d/%d; CTRL 意外失败: %d" % (len(p2_fail), len(p2), len(ctrl_fail)))
        for n in ctrl_fail:
            print("      [意外FAIL] %s" % n)
        return 0 if sens else 1

    print("\n=== 结果: %s (断言 %d, 失败 %d; P2*=%d CTRL=%d; INFO=%d) ===" % (
        "PASS" if not FAIL else "FAIL", total, len(FAIL), len(p2),
        len([n for n in PASS if n.startswith("CTRL:")]), len(INFO)))
    for n in FAIL:
        print("  [FAIL] %s" % n)
    return 0 if not FAIL else 1


if __name__ == "__main__":
    sys.exit(main())
