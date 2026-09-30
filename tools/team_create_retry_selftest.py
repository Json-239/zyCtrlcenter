# -*- coding: utf-8 -*-
"""队长"建队回执丢失"重发自检 —— 2026-09-30（team_captain.tick 建队重发）。

背景（现场 robot0001006, 2026-09-30 12:12:02）:
  队长在抓鬼战斗中收到 team_setup_captain, 发出 C2S_TEAM_CREATE 后**再无
  S2C_BUILD_TEAM**（"队伍已建"计数=0）；4 名队员一直"等邀请"、中控 job 卡 pending。
  根因链（只读核实）:
    · 服务端 create_team 先查 can_operate_team(True)（script/team/team_operator.py:18-22）,
      战斗中该项为假 → **静默 return**, 无失败回包/无提示;
    · 战斗 FightRoleStatus 登记了 DISABLE_OPERATE_TEAM
      （script/restrict/restrict_status.py:65）, 且战斗状态不提示
      （restrict_status.py:28, 注释"不显示 战斗状态下不能XXX", BugID=14683）→ 完全无感;
    · 机器人端原实现: dispatch_cmd 只发一次建队包, tick 对 !setup_done 直接 return 0
      → 无任何重发, setup_done 恒假, 邀请永不发出。
修复（本自检对象）:
  team_captain.tick: captain 且 !setup_done → __tick_create_retry:
    · 成员为空 → 不重发（不需要建队）;
    · 战斗（m_fight_state）→ 不重发且不消耗重发计数（服务端必静默拒绝）;
    · 非战斗按 8s 节流重发 C2S_TEAM_CREATE, 打 info 日志"建队重发(第 N 次…)";
    · 重发满 _CREATE_RETRY_MAX=20 次后 warn 一次("建队无回执")并停止, 不崩;
    · on_build_team 置 setup_done=True 后 tick 自动不再进入（停止重发）。
      （既有邀请重发/归队/promote/队员等待/apply/disband 逻辑不动。）

本自检（stub 真跑 团队模块, 不连服务器）:
  A 触发: dispatch 首发 1 次; 节流内不重发; 到点重发 + 日志 + 计数;
  B 战斗: 战斗中不重发且计数不动; 战斗结束到点续发;
  C 停止: on_build_team 后建队不再重发（既有邀请重发不受影响）;
  D 空成员: 不重发（首发保持既有行为）;
  E 上限: 重发满 20 次后 warn 一次并停止（不抛异常）;
  F 兼容: 新字段存在 + reset 清零 + 旧对象（缺新字段）不崩仍能重发;
  G 静态: 源码挂钩（函数/常量/tick 接线/dispatch 记时/非回归）;
  H 灵敏度: ① 备份版（.bak_20260930_teamretry）跑同套断言必 FAIL;
            ② 内存变异（去掉 tick 接线）跑同套行为断言必 FAIL;
  I 行尾: team_captain.py 行尾一致（全 CRLF 或全 LF）; 可选校验双副本逐字节一致。

用法: python tools/team_create_retry_selftest.py [script_dir [second_dir]]
      （默认 ...\\2d-xiyou-server\\robot\\deploy\\single_robot_zy\\script;
        给 second_dir 时附加校验两副本 team_captain.py 逐字节一致）
"""
import io
import os
import sys
import types

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

DEFAULT_SCRIPT = r"F:\ZyBin\xm\2d-xiyou-server\robot\deploy\single_robot_zy\script"

results = []


def check(name, cond, detail=""):
    results.append((name, bool(cond), detail))
    print("[%s] %s%s" % ("PASS" if cond else "FAIL", name,
        (" | %s" % detail) if (detail and not cond) else ""))


def _read(path):
    with io.open(path, "r", encoding="utf-8", errors="replace") as f:
        return f.read()


def _extract_func(src, name):
    lines = src.split("\n")
    for i, ln in enumerate(lines):
        if ln.startswith("def %s(" % name):
            out = [ln]
            for ln2 in lines[i + 1:]:
                if ln2.strip() and not ln2.startswith(("\t", " ", "#")):
                    break
                out.append(ln2)
            return "\n".join(out)
    return ""


# ================================================================ stub 依赖
EVENTS = []		# __emit 捕获（stub ctrl_client.emit）


def _install_stubs():
    config = types.ModuleType("config")

    protocol3 = types.ModuleType("protocol3")
    for k in ("C2S_TEAM_CREATE", "C2S_TEAM_OFFER", "C2S_TEAM_QUIT", "C2S_TEAM_DISMISS",
              "C2S_TEAM_PROMOTE", "C2S_TEAM_INVITE_ACCESS", "C2S_TEAM_PASS",
              "C2S_TEAM_MEMBER_COME_BACK", "C2S_TEAM_COME_BACK_FAST"):
        setattr(protocol3, k, k)

    error = types.ModuleType("error")
    error.NET_CLOSED = -1

    robot_mgr = types.ModuleType("robot_mgr")

    class _GMrg(object):
        def drop_robot(self, robot):
            pass
    robot_mgr.g_mgr = _GMrg()

    diag = types.ModuleType("diag")
    diag.log = lambda *a, **kw: None

    ctrl_client = types.ModuleType("ctrl_client")

    def _emit_ev(ev):
        EVENTS.append(dict(ev))
    ctrl_client.emit = _emit_ev

    sys.modules["config"] = config
    sys.modules["protocol3"] = protocol3
    sys.modules["error"] = error
    sys.modules["robot_mgr"] = robot_mgr
    sys.modules["diag"] = diag
    sys.modules["ctrl_client"] = ctrl_client


def load_from_source(name, src):
    """把源码 exec 成模块（依赖走 sys.modules 里的 stub）。"""
    mod = types.ModuleType(name)
    mod.__file__ = "<selftest:%s>" % name
    exec(compile(src, mod.__file__, "exec"), mod.__dict__)
    return mod


class Clock(object):
    """虚拟时钟（秒）；patch 模块 __now_ms 后全部时间线可控。"""

    def __init__(self, v=1000000.0):
        self.v = v

    def ms(self):
        return self.v * 1000.0


class FakeRobot(object):
    def __init__(self, rid=111, name="测试号"):
        self.m_account = ("robtest@x.com",)
        self.m_fight_state = False
        self.m_id = rid
        self.role_name = name
        self.sends = []		# [(msg, data), ...]
        self.m_quest = None

    def get_role_name(self):
        return self.role_name

    def get_role_id(self):
        return self.m_id

    def send_message(self, msg_id, data):
        self.sends.append((msg_id, list(data)))
        return 0


def count_create(robot):
    return sum(1 for m, d in robot.sends if m == "C2S_TEAM_CREATE")


def count_offer(robot):
    return sum(1 for m, d in robot.sends if m == "C2S_TEAM_OFFER")


# ================================================================ 行为矩阵
def run_behavior(tc, tag, out):
    """对给定模块跑行为断言；结果追加进 out。返回 out。"""
    clock = Clock()
    setattr(tc, "__now_ms", clock.ms)

    def add(name, cond, detail=""):
        out.append(("[%s] %s" % (tag, name), bool(cond), detail))

    def has_msg(since, substr):
        return any(substr in str(e.get("msg", "")) for e in EVENTS[since:])

    # ---- A: 非战斗触发 ----
    robot = FakeRobot()
    n0 = len(EVENTS)
    ret = tc.dispatch_cmd(robot, {"cmd": "team_setup_captain",
        "members": [{"role_id": 3010019}, {"role_id": 2010817}], "next_action": None})
    add("A1 dispatch 回执 ok 且首发建队包 x1",
        ret.get("result") == "ok" and count_create(robot) == 1,
        "ret=%s count=%d" % (ret.get("result"), count_create(robot)))
    clock.v += 3.0
    tc.tick(robot, clock.v)
    add("A2 节流内(3s<8s)不重发", count_create(robot) == 1,
        "count=%d" % count_create(robot))
    clock.v += 6.0		# 距首发 9s
    tc.tick(robot, clock.v)
    add("A3 非战斗到点(>=8s)重发建队包", count_create(robot) == 2,
        "count=%d" % count_create(robot))
    add("A4 重发打日志'建队重发'", has_msg(n0, "建队重发"))
    clock.v += 9.0
    tc.tick(robot, clock.v)
    add("A5 下一窗口再重发(第 2 次)", count_create(robot) == 3,
        "count=%d" % count_create(robot))
    t = robot.m_team
    add("A6 重发计数=2", int(getattr(t, "create_retries", -1)) == 2,
        "retries=%s" % getattr(t, "create_retries", None))

    # ---- B: 战斗中不重发 ----
    robot.m_fight_state = True
    c0 = count_create(robot)
    r0 = int(getattr(t, "create_retries", 0) or 0)
    n1 = len(EVENTS)
    clock.v += 30.0
    tc.tick(robot, clock.v)
    add("B1 战斗中不重发(不发无效包)", count_create(robot) == c0,
        "count=%d" % count_create(robot))
    add("B2 战斗中不消耗重发计数",
        int(getattr(t, "create_retries", 0) or 0) == r0)
    add("B3 战斗中不打'建队重发'日志", not has_msg(n1, "建队重发"))
    robot.m_fight_state = False
    clock.v += 1.0		# 距上次重发已 >=8s
    tc.tick(robot, clock.v)
    add("B4 战斗结束后到点续发", count_create(robot) == c0 + 1,
        "count=%d" % count_create(robot))

    # ---- C: on_build_team 后停止 ----
    c0 = count_create(robot)
    tc.on_build_team(robot, [111, "测试队", [], []])
    add("C1 on_build_team 置 setup_done", bool(getattr(t, "setup_done", False)))
    clock.v += 100.0
    tc.tick(robot, clock.v)
    add("C2 建队成功后不再重发建队包", count_create(robot) == c0,
        "count=%d" % count_create(robot))
    add("C3 既有邀请逻辑不受影响(已发 C2S_TEAM_OFFER)",
        count_offer(robot) >= len(getattr(t, "member_role_ids", [])),
        "offer=%d" % count_offer(robot))

    # ---- D: 成员为空不重发 ----
    robot2 = FakeRobot()
    ret2 = tc.dispatch_cmd(robot2, {"cmd": "team_setup_captain", "members": []})
    add("D1 空成员 dispatch 仍首发 x1(既有行为)",
        ret2.get("result") == "ok" and count_create(robot2) == 1,
        "count=%d" % count_create(robot2))
    clock.v += 30.0
    tc.tick(robot2, clock.v)
    add("D2 空成员不重发", count_create(robot2) == 1,
        "count=%d" % count_create(robot2))

    # ---- E: 上限 warn 且停止 ----
    robot3 = FakeRobot()
    tc.dispatch_cmd(robot3, {"cmd": "team_setup_captain",
        "members": [{"role_id": 4000813}]})
    n3 = len(EVENTS)
    raised = ""
    try:
        for _i in range(25):
            clock.v += 9.0
            tc.tick(robot3, clock.v)
    except Exception:
        import traceback
        raised = traceback.format_exc()
    add("E1 连跑 25 个重发窗口不抛异常", raised == "", raised[:200])
    add("E2 重发上限=20(总创建包=1+20)", count_create(robot3) == 21,
        "count=%d" % count_create(robot3))
    warns = [e for e in EVENTS[n3:] if "建队无回执" in str(e.get("msg", ""))]
    add("E3 达上限 warn 一次('建队无回执')", len(warns) == 1,
        "warns=%d" % len(warns))
    clock.v += 9.0
    tc.tick(robot3, clock.v)
    add("E4 warn 后不再重发", count_create(robot3) == 21,
        "count=%d" % count_create(robot3))

    # ---- F: 兼容 ----
    t_new = tc.TeamState()
    add("F1 TeamState 新字段存在", all(hasattr(t_new, k) for k in
        ("last_create_ms", "create_retries", "create_warned")))
    t_new.last_create_ms = 123
    t_new.create_retries = 7
    t_new.create_warned = True
    t_new.reset()
    add("F2 reset 清零新字段", t_new.last_create_ms == 0
        and t_new.create_retries == 0 and t_new.create_warned is False)
    robot4 = FakeRobot()
    tc.dispatch_cmd(robot4, {"cmd": "team_setup_captain",
        "members": [{"role_id": 501}]})
    t4 = robot4.m_team
    for f2 in ("last_create_ms", "create_retries", "create_warned"):
        try:
            delattr(t4, f2)
        except Exception:
            pass
    clock.v += 9.0
    ok_noraise = True
    try:
        tc.tick(robot4, clock.v)
    except Exception:
        ok_noraise = False
    add("F3 旧对象(缺新字段)重发不崩", ok_noraise)
    add("F4 旧对象(缺新字段)仍能重发", count_create(robot4) == 2,
        "count=%d" % count_create(robot4))
    return out


# ================================================================ 静态检查
def run_static(src, tag, out):
    def add(name, cond, detail=""):
        out.append(("[%s] %s" % (tag, name), bool(cond), detail))

    add("G1 __tick_create_retry 函数存在", "def __tick_create_retry(" in src)
    add("G2 常量 8000/20 存在",
        "_CREATE_RETRY_GAP_MS = 8000" in src and "_CREATE_RETRY_MAX = 20" in src)
    add("G3 tick !setup_done → 调 __tick_create_retry",
        "return __tick_create_retry(robot_object, t)" in src)
    add("G4 dispatch 首发后记 last_create_ms",
        "t.last_create_ms = __now_ms()" in src)
    fn = _extract_func(src, "__tick_create_retry")
    add("G5 重发函数: 战斗闸(m_fight_state)",
        'getattr(robot_object, "m_fight_state", False)' in fn)
    add("G6 重发函数: 空成员闸", "if not t.member_role_ids:" in fn)
    add("G7 重发函数: 发 C2S_TEAM_CREATE + 日志'建队重发'",
        "protocol3.C2S_TEAM_CREATE" in fn and "建队重发" in fn)
    add("G8 重发函数: 上限 warn'建队无回执'(含 _CREATE_RETRY_MAX)",
        "建队无回执" in fn and "_CREATE_RETRY_MAX" in fn)
    add("G9 非回归: on_build_team 仍置 setup_done=True",
        "t.setup_done = True" in _extract_func(src, "on_build_team"))
    add("G10 非回归: 邀请重发节流仍在",
        "t.last_invite_ms = now_ms" in src)
    return out


def _fails(out):
    return [r for r in out if not r[1]]


def _print_items(items):
    for name, ok, detail in items:
        print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
            (" | %s" % detail) if (detail and not ok) else ""))


def main():
    script_dir = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_SCRIPT
    path = os.path.join(script_dir, "team_captain.py")
    if not os.path.exists(path):
        print("[FAIL] 缺少 %s" % path)
        sys.exit(2)

    _install_stubs()
    src = _read(path)
    tc = load_from_source("tc_real", src)

    print("== A-G: 行为矩阵 + 静态挂钩（当前版） ==")
    n_before = len(results)
    run_static(src, "sys", results)
    run_behavior(tc, "sys", results)
    _print_items(results[n_before:])

    print("\n== H: 灵敏度（坏版必须 FAIL） ==")
    # H1: 备份版（修复前）跑同套断言 → 必 FAIL
    bak = path + ".bak_20260930_teamretry"
    if os.path.exists(bak):
        src_bak = _read(bak)
        tc_bak = load_from_source("tc_bak", src_bak)
        out_bak = []
        run_static(src_bak, "bak-static", out_bak)
        run_behavior(tc_bak, "bak-behavior", out_bak)
        fails_bak = _fails(out_bak)
        check("[H1] 灵敏度: 备份版跑同套断言命中 %d 项(>0)" % len(fails_bak),
            len(fails_bak) > 0,
            "命中示例: %s" % ([r[0] for r in fails_bak[:3]],))
        for nm, _ok2, _dt in fails_bak[:6]:
            print("    [bak-FAIL] %s" % nm)
        check("[H1b] 灵敏度: 备份版命中'重发/接线'主断言",
            any(("A3" in r[0]) or ("E2" in r[0]) or ("G1" in r[0]) or ("G3" in r[0])
                for r in fails_bak))
    else:
        check("[H1] 灵敏度: 备份版缺失(跳过文件级, 由内存变异覆盖)", True, bak)

    # H2: 内存变异（去掉 tick 接线 = 修复前等价接线）→ 行为必 FAIL
    mut = src.replace("return __tick_create_retry(robot_object, t)", "return 0")
    tc_mut = load_from_source("tc_mut", mut)
    out_mut = []
    run_behavior(tc_mut, "mut-behavior", out_mut)
    fails_mut = _fails(out_mut)
    check("[H2] 灵敏度: 去掉 tick 接线(内存变异)命中 %d 项(>0)" % len(fails_mut),
        len(fails_mut) > 0,
        "命中示例: %s" % ([r[0] for r in fails_mut[:3]],))
    for nm, _ok2, _dt in fails_mut[:6]:
        print("    [mut-FAIL] %s" % nm)

    print("\n== I: 行尾 / 双副本 ==")
    raw = open(path, "rb").read()
    crlf = raw.count(b"\r\n")
    lf = raw.count(b"\n")
    check("[I1] team_captain.py 行尾一致(全CRLF或全LF)",
        crlf == 0 or crlf == lf, "crlf=%d lf=%d" % (crlf, lf))
    if len(sys.argv) > 2:
        path2 = os.path.join(sys.argv[2], "team_captain.py")
        if os.path.exists(path2):
            raw2 = open(path2, "rb").read()
            check("[I2] 双副本逐字节一致", raw == raw2,
                "sha 不同: %d vs %d bytes" % (len(raw), len(raw2)))
        else:
            check("[I2] 双副本: second_dir 缺 team_captain.py", False, path2)

    fails = _fails(results)
    print("\n==== 合计 %d 项, 失败 %d ====" % (len(results), len(fails)))
    if fails:
        print("失败项:")
        for r in fails:
            print("  - %s | %s" % (r[0], r[2]))
        sys.exit(1)
    print("全绿。")
    sys.exit(0)


if __name__ == "__main__":
    main()
