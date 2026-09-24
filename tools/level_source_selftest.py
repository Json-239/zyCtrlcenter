# -*- coding: utf-8 -*-
"""等级来源守卫 自检（2026-09-24）。背景：多个号"已确认等级"虚高（实测 28/30 级号被写成 59/61），
虚高期间 >=31 → 被当抓鬼候选派活。根因（服务端源码实证）：
  ① S2C_ROLE_LEVEL(90423) 是"附近广播"：服务端 sender_role_.py send_role_level →
     role_object.send_to_near_role（role_object.py:608 注释「除了自己」），报文 =
     [升级者 role_id, 其 active_class, 其等级]。机器人端原实现不校验 role_id 直接
     m_level = datalist[2] → 抓鬼/新手村扎堆时把"附近别人的等级"写到自己身上。
  ② S2C_MATCH_REINC_VALUE(90150) 是"按转世档位"的增量属性推送，第 1 字段 = 属性所属
     reinc_class（9115 一世/9116 二世）。服务端 reinc_ruler.count_ability 对**所有档**
     计数并各自推送（reinc_property.__do_after_count_ability）→ 非活跃档也带自己的
     PROPERTY_LEVEL(8406)/FPP(8440)/HP(8416-8419)，原实现不校验档位直接应用会污染当前状态。
修复（msghandle.py）：
  90423 只认本机 role_id（服务端从不给自己发 ⇒ 实际"永远拒绝"是预期行为）；
  90150 只应用 reinc_class == m_active_class（登录 90201 记录）的推送，等级只升不降；
  90201 记录 m_active_class；
  等级写入统一走 __set_robot_level（old/new/source/class 仅值变化时打 diag）。用法：
  python tools/level_source_selftest.py [script 目录]
  不带参数默认校验仓库副本 deploy/zones/prod-240-2300/script；
  传生产目录可再验一次线上文件。实现方式：ast 精确提取 msghandle.py 的 5 个函数与 2 个模块常量，
  stub（diag/keys/marshal/time/robot_mgr/ctrl_client）后 exec，用假 robot 对象**直接调处理器**；
  keys 常量值从 script/keys/*.py 实读，既做漂移断言也供 stub 使用。
"""
import ast
import io
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
DEFAULT_DIR = os.path.normpath(os.path.join(
    HERE, "..", "deploy", "zones", "prod-240-2300", "script"))
PROD_COPY = "F:/ZyBin/xm/2d-xiyou-server/robot/deploy/single_robot_zy/script"
SCRIPT_DIR = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_DIR

fails = 0
total = 0


def check(name, ok, detail=""):
    global fails, total
    total += 1
    fails += 0 if ok else 1
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  " + detail) if detail else ""))


# ================================================================
# 0) 读源文件 + ast 精确提取（避免正则跨段误抓）
# ================================================================
SRC_PATH = os.path.join(SCRIPT_DIR, "msghandle.py")
if not os.path.exists(SRC_PATH):
    print("找不到 %s" % SRC_PATH)
    sys.exit(2)
SRC = open(SRC_PATH, encoding="utf-8").read()
TREE = ast.parse(SRC)
_LINES = SRC.splitlines()

_WANT_FUNCS = ("__level_diag", "__set_robot_level", "role_level_handle",
               "match_reinc_value_handle", "match_total_reinc_data_handle")
_WANT_CONSTS = ("_LEVEL_DIAG_INTERVAL_SEC", "_LEVEL_DIAG_LAST")


def _frag(names):
    picked = []
    for node in TREE.body:
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and node.name in names:
            picked.append((node.lineno, node.end_lineno))
        elif isinstance(node, ast.Assign):
            for t in node.targets:
                if isinstance(t, ast.Name) and t.id in names:
                    picked.append((node.lineno, node.end_lineno))
    picked.sort()
    return "\n".join("\n".join(_LINES[a - 1:b]) for a, b in picked), picked


FRAG, _picked = _frag(_WANT_FUNCS + _WANT_CONSTS)
FUNC_SRC = {}
for node in TREE.body:
    if isinstance(node, ast.FunctionDef):
        FUNC_SRC[node.name] = "\n".join(_LINES[node.lineno - 1:node.end_lineno])

# ================================================================
# 1) 静态断言（源码级，防回归）
# ================================================================
_missing = [n for n in _WANT_FUNCS if n not in FUNC_SRC]
check("S1 msghandle.py 含 5 个目标函数（2 辅助 + 3 处理器）",
      not _missing, "missing=%s" % _missing)

_rl = FUNC_SRC.get("role_level_handle", "")
check("S2 90423 校验 role_id（datalist[0] != robot_object.m_id）",
      "datalist[0] != robot_object.m_id" in _rl and "m_id" in _rl)
check("S3 90423 不再直接写 m_level（无 robot_object.m_level = int(）",
      "robot_object.m_level = int(" not in _rl and "__set_robot_level(" in _rl)
check("S4 90423 拒绝分支打低频 diag（ROLE_LEVEL_IGNORE）",
      "ROLE_LEVEL_IGNORE" in _rl and "__level_diag(" in _rl)

_rv = FUNC_SRC.get("match_reinc_value_handle", "")
check("S5 90150 档位闸（reinc_class != active_class → 跳过）",
      "reinc_class != active_class" in _rv and "REINC_VALUE_IGNORE" in _rv)
check("S6 90150 活跃档未知时保守丢弃（active_class is None）",
      "active_class is None" in _rv)
check("S7 90150 等级只升不降（降级 → LEVEL_DOWN_IGNORE）",
      "LEVEL_DOWN_IGNORE" in _rv and "new_level > old_level" in _rv)
check("S8 90150 等级写入带来源（__set_robot_level(..., \"90150\", ...)）",
      '"90150"' in _rv and "robot_object.m_level = int(" not in _rv)

_tr = FUNC_SRC.get("match_total_reinc_data_handle", "")
check("S9 90201 记录 m_active_class",
      "robot_object.m_active_class = active_class" in _tr)
check("S10 90201 等级写入带来源（__set_robot_level(..., \"90201\", ...)）",
      '"90201"' in _tr and "robot_object.m_level = int(" not in _tr)

_helpers = FUNC_SRC.get("__level_diag", "") + FUNC_SRC.get("__set_robot_level", "")
check("S11 辅助函数存在且 diag 低频（_LEVEL_DIAG_INTERVAL_SEC 节流）",
      "__level_diag" in FUNC_SRC and "_LEVEL_DIAG_INTERVAL_SEC" in _helpers)
check("S12 等级写入 diag 仅值变化时打（old == new → return False）",
      "if old_level == new_level:" in _helpers and "LEVEL_SET" in _helpers)

# 协议注册未被动（三条 S2C → 处理器映射）
P3 = open(os.path.join(SCRIPT_DIR, "protocol3.py"), encoding="utf-8").read()
check("S13 protocol3 三条注册未变",
      "S2C_ROLE_LEVEL : msghandle.role_level_handle" in P3
      and "S2C_MATCH_REINC_VALUE : msghandle.match_reinc_value_handle" in P3
      and "S2C_MATCH_TOTAL_REINC_DATA : msghandle.match_total_reinc_data_handle" in P3)

# keys 常量与 keys/*.py 实值一致（drift 守卫）
def _keyval(fname, name):
    p = os.path.join(SCRIPT_DIR, "keys", fname)
    txt = open(p, encoding="utf-8").read()
    m = re.search(r"(?m)^%s\s*=\s*(-?\d+)" % re.escape(name), txt)
    return int(m.group(1)) if m else None


KEY_EXPECT = [
    ("property_.py", "PROPERTY_LEVEL", 8406),
    ("property_.py", "PROPERTY_FPP", 8440),
    ("property_.py", "PROPERTY_CUR_HP", 8416),
    ("property_.py", "PROPERTY_MAX_HP", 8418),
    ("property_.py", "PROPERTY_CUR_MP", 8417),
    ("property_.py", "PROPERTY_MAX_MP", 8419),
    ("reinc_.py", "ACTIVE_CLASS", 9112),
    ("reinc_.py", "REINC_FIRST", 9115),
    ("reinc_.py", "REINC_SECOND", 9116),
]
_bad = []
for _fn, _kn, _want in KEY_EXPECT:
    if _keyval(_fn, _kn) != _want:
        _bad.append("%s=%s(期望%d)" % (_kn, _keyval(_fn, _kn), _want))
check("S14 keys/*.py 常量与用例一致（等级/自由点/血法/档位）",
      not _bad, " ".join(_bad))

# ================================================================
# 2) stub 环境 + exec 提取片段
# ================================================================
LOGS = []
ROBOTS = {}

_diag = types.ModuleType("diag")
_diag.log = lambda msg: LOGS.append(str(msg))
sys.modules["diag"] = _diag

_ctrl = types.ModuleType("ctrl_client")
_ctrl.emit = lambda d: None
sys.modules["ctrl_client"] = _ctrl

_keys = types.ModuleType("keys")
for _fn, _kn, _want in KEY_EXPECT:
    setattr(_keys, _kn, _keyval(_fn, _kn))
sys.modules["keys"] = _keys


class _GMgr(object):
    def get_robot_object_by_fd(self, fd):
        return ROBOTS.get(fd)


_rbm = types.ModuleType("robot_mgr")
_rbm.g_mgr = _GMgr()
sys.modules["robot_mgr"] = _rbm

NS = {
    "diag": _diag,
    "keys": _keys,
    "marshal": marshal,
    "time": time,
    "robot_mgr": _rbm,
    "ctrl_client": _ctrl,
}
try:
    exec(compile(FRAG, "<msghandle-level-guard>", "exec"), NS)  # noqa: S102 自检专用
    _exec_ok = True
    _exec_err = ""
except Exception as e:
    _exec_ok = False
    _exec_err = "%s: %s" % (type(e).__name__, e)

check("D0 提取片段可执行（stub 后无模块级 NameError）", _exec_ok, _exec_err)

rl_handle = NS.get("role_level_handle")
rv_handle = NS.get("match_reinc_value_handle")
tr_handle = NS.get("match_total_reinc_data_handle")


def make_robot(account="a0001", level=30, active_class=None, rid=1001):
    ro = types.SimpleNamespace()
    ro.m_id = rid
    ro.m_account = [account, "pw", ""]
    ro.m_level = level
    ro.m_fpp = None
    ro.m_attrs = {}
    ro.m_cur_hp = 0
    ro.m_max_hp = 0
    ro.m_cur_mp = 0
    ro.m_max_mp = 0
    if active_class is not None:
        ro.m_active_class = active_class
    return ro


def bind(fd, ro):
    ROBOTS[fd] = ro
    LOGS[:] = []
    return ro


def logs_with(tag):
    return [x for x in LOGS if tag in x]


def blob90150(d):
    return marshal.dumps(d, 0)


def blob90201(active, first_prop, second_prop=None):
    d = {_keys.ACTIVE_CLASS: active,
         _keys.REINC_FIRST: ({}, first_prop, {}, {}),
         _keys.REINC_SECOND: ({}, second_prop or {}, {}, {})}
    return marshal.dumps(d, 0)


print("--- %s" % SCRIPT_DIR)

# ================================================================
# 3) 动态：90423（附近广播，别人的等级一律忽略）
# ================================================================
if _exec_ok:
    # D1 别人的 role_id → 拒绝
    ro = bind(11, make_robot(account="d1", level=28, rid=1001))
    rl_handle(11, [2002, _keys.REINC_FIRST, 59])   # 邻居 2002 的等级 59
    check("D1 90423 别人 role_id(59) → m_level 不变（仍 28）",
          ro.m_level == 28 and len(logs_with("ROLE_LEVEL_IGNORE")) == 1,
          "level=%s logs=%s" % (ro.m_level, logs_with("ROLE_LEVEL_IGNORE")[:1]))

    # D2 自己的 role_id → 接受（防御分支，服务端实际不发）
    ro = bind(12, make_robot(account="d2", level=28, rid=1001))
    rl_handle(12, [1001, _keys.REINC_FIRST, 40])
    check("D2 90423 本机 role_id → 接受（28→40, source=90423）",
          ro.m_level == 40
          and any("source=90423" in x for x in logs_with("LEVEL_SET")),
          "level=%s logs=%s" % (ro.m_level, logs_with("LEVEL_SET")[:1]))

    # D3 同值 → 不产生 LEVEL_SET（仅值变化时打）
    ro = bind(13, make_robot(account="d3", level=40, rid=1001))
    rl_handle(13, [1001, _keys.REINC_FIRST, 40])
    check("D3 90423 同值 → 无 LEVEL_SET 日志（不刷屏）",
          ro.m_level == 40 and not logs_with("LEVEL_SET"))

    # ============================================================
    # 4) 动态：90150（档位闸 + 只升不降）
    # ============================================================
    # D4 非活跃档 8406=59 → 拒绝
    ro = bind(21, make_robot(account="d4", level=28, active_class=_keys.REINC_FIRST))
    rv_handle(21, [_keys.REINC_SECOND, blob90150({8406: 59})])
    check("D4 90150 非活跃档(9116) 8406=59 → 拒绝（仍 28, REINC_VALUE_IGNORE）",
          ro.m_level == 28 and len(logs_with("REINC_VALUE_IGNORE")) == 1,
          "level=%s" % ro.m_level)

    # D5 活跃档升级 → 接受
    ro = bind(22, make_robot(account="d5", level=28, active_class=_keys.REINC_FIRST))
    rv_handle(22, [_keys.REINC_FIRST, blob90150({8406: 40})])
    check("D5 90150 活跃档 8406=40（28→40）→ 接受（source=90150）",
          ro.m_level == 40 and any("source=90150" in x for x in logs_with("LEVEL_SET")),
          "level=%s" % ro.m_level)

    # D6 活跃档降级 → 拒绝
    ro = bind(23, make_robot(account="d6", level=40, active_class=_keys.REINC_FIRST))
    rv_handle(23, [_keys.REINC_FIRST, blob90150({8406: 20})])
    check("D6 90150 活跃档 8406=20（40→20）→ 拒绝降级（仍 40, LEVEL_DOWN_IGNORE）",
          ro.m_level == 40 and len(logs_with("LEVEL_DOWN_IGNORE")) == 1,
          "level=%s" % ro.m_level)

    # D7 活跃档血法应用
    ro = bind(24, make_robot(account="d7", level=30, active_class=_keys.REINC_FIRST))
    rv_handle(24, [_keys.REINC_FIRST, blob90150({8416: 100, 8418: 200, 8417: 11, 8419: 22})])
    check("D7 90150 活跃档血法(8416/8418/8417/8419) → 应用",
          (ro.m_cur_hp, ro.m_max_hp, ro.m_cur_mp, ro.m_max_mp) == (100, 200, 11, 22),
          "hp=%s/%s mp=%s/%s" % (ro.m_cur_hp, ro.m_max_hp, ro.m_cur_mp, ro.m_max_mp))

    # D8 非活跃档血法/fpp → 全部不动（点4判断：所有键同受档位闸）
    ro = bind(25, make_robot(account="d8", level=30, active_class=_keys.REINC_FIRST))
    ro.m_fpp = 4
    rv_handle(25, [_keys.REINC_SECOND, blob90150({8416: 9999, 8440: 7, 8406: 61})])
    check("D8 90150 非活跃档血法/fpp/等级 → 全部不动",
          ro.m_cur_hp == 0 and ro.m_fpp == 4 and ro.m_level == 30,
          "hp=%s fpp=%s level=%s" % (ro.m_cur_hp, ro.m_fpp, ro.m_level))

    # D9 活跃档 fpp + m_attrs 合并生效
    ro = bind(26, make_robot(account="d9", level=30, active_class=_keys.REINC_FIRST))
    rv_handle(26, [_keys.REINC_FIRST, blob90150({8440: 5, 2297: 1})])
    check("D9 90150 活跃档 fpp=5 + m_attrs 合并（8440/2297）",
          ro.m_fpp == 5 and ro.m_attrs.get(8440) == 5 and ro.m_attrs.get(2297) == 1,
          "fpp=%s attrs=%s" % (ro.m_fpp, sorted(ro.m_attrs.keys())))

    # D10 非活跃档不得污染 m_attrs（活跃档快照）
    ro = bind(27, make_robot(account="d10", level=30, active_class=_keys.REINC_FIRST))
    rv_handle(27, [_keys.REINC_FIRST, blob90150({2297: 1})])
    rv_handle(27, [_keys.REINC_SECOND, blob90150({9999: 123})])
    check("D10 90150 非活跃档键不入 m_attrs（9999 不出现）",
          ro.m_attrs.get(2297) == 1 and 9999 not in ro.m_attrs,
          "attrs=%s" % sorted(ro.m_attrs.keys()))

    # D11 活跃档未知（02 未到）→ 保守丢弃
    ro = bind(28, make_robot(account="d11", level=28))    # 无 m_active_class
    rv_handle(28, [_keys.REINC_FIRST, blob90150({8406: 40})])
    check("D11 90150 活跃档未知 → 保守丢弃（仍 28）",
          ro.m_level == 28 and len(logs_with("REINC_VALUE_IGNORE")) == 1)

    # D12 活跃档但无 8406 → 等级不变，其余键照常应用
    ro = bind(29, make_robot(account="d12", level=33, active_class=_keys.REINC_FIRST))
    rv_handle(29, [_keys.REINC_FIRST, blob90150({8416: 5})])
    check("D12 90150 活跃档无 8406 → 等级不变(33) 而血照常(5)",
          ro.m_level == 33 and ro.m_cur_hp == 5,
          "level=%s hp=%s" % (ro.m_level, ro.m_cur_hp))

    # D13 低频 diag：同账号连续两条拒绝 → 只 1 条
    ro = bind(30, make_robot(account="d13", level=28, active_class=_keys.REINC_FIRST))
    rv_handle(30, [_keys.REINC_SECOND, blob90150({8406: 59})])
    rv_handle(30, [_keys.REINC_SECOND, blob90150({8406: 60})])
    check("D13 低频 diag：同账号连续两条非活跃档 → 仅 1 条日志",
          len(logs_with("REINC_VALUE_IGNORE")) == 1,
          "n=%d" % len(logs_with("REINC_VALUE_IGNORE")))

    # ============================================================
    # 5) 动态：90201（记录活跃档 + 权威全量）
    # ============================================================
    # D14 记录 m_active_class + 等级来自活跃档
    ro = bind(31, make_robot(account="d14", level=30))    # 无 m_active_class
    tr_handle(31, [blob90201(_keys.REINC_FIRST, {8406: 28, 8440: 2, 8416: 50, 8418: 100},
                             {8406: 59})])
    check("D14 90201 记录 m_active_class=9115 + 等级取活跃档(28, 非 9116 的 59)",
          getattr(ro, "m_active_class", None) == _keys.REINC_FIRST and ro.m_level == 28,
          "active=%s level=%s" % (getattr(ro, "m_active_class", None), ro.m_level))

    # D15 90201 权威：即使更低也接受（不受只升不降限制）
    ro = bind(32, make_robot(account="d15", level=60))
    tr_handle(32, [blob90201(_keys.REINC_FIRST, {8406: 30})])
    check("D15 90201 权威全量：60→30 也接受（转世重登语义, 不受 90150 限制）",
          ro.m_level == 30)

    # D16 90201 后 90150 用新档位放行（联动）
    ro = bind(33, make_robot(account="d16", level=0))
    tr_handle(33, [blob90201(_keys.REINC_SECOND, {8406: 35})])
    LOGS[:] = []
    rv_handle(33, [_keys.REINC_SECOND, blob90150({8406: 36})])
    check("D16 90201(active=9116) 后 90150 class=9116 → 放行（35→36）",
          ro.m_level == 36 and getattr(ro, "m_active_class", None) == _keys.REINC_SECOND,
          "level=%s active=%s" % (ro.m_level, getattr(ro, "m_active_class", None)))

    # D17 90201 非 dict blob / 空数据不炸
    ro = bind(34, make_robot(account="d17", level=30))
    rv_handle(34, [_keys.REINC_FIRST, b"\x00\x01"])
    tr_handle(34, [b"\x00\x01"])
    rl_handle(34, [])
    check("D17 坏 blob/空包不抛异常且状态不变", ro.m_level == 30)

else:
    print("[SKIP] 动态用例因 D0 失败跳过")

# ================================================================
print("----")
print("%d/%d 通过, %d 失败" % (total - fails, total, fails))
if SCRIPT_DIR != PROD_COPY and os.path.isdir(PROD_COPY):
    print("提示: 可再执行 python tools/level_source_selftest.py %s 复验生产副本" % PROD_COPY)
sys.exit(1 if fails else 0)
