# -*- coding: utf-8 -*-
"""组队·邀请挂起重发(修1) + 邀请窗口(修2) 自检 —— 2026-09-30（team_captain.py）。

背景（现场 1006 队, 2026-09-30 12:43 建队后）:
  · 队员在**战斗**中收到邀请并发出 C2S_TEAM_INVITE_ACCESS → 被服务端**静默丢弃**
    （team_invite_access 的 can_operate_team 无战斗豁免, 对比 team_offer 有
    BugID=15440）→ 永久错过; 实锤: 1000 12:43:22/27 三次"同意入队"全在战斗中。
  · 队长邀请只有单轮 ~10s 窗口, 之后"邀请超时自动确认"会把**没进来的**成员
    一并标记 ready 并**停发邀请** → 失败被掩盖（实锤: 1006 4 人"确认"真入队仅 1005）。

修1（队员侧, `__tick_invite_access_retry`）:
  on_team_invite 时若 m_fight_state 为真 → 不空发 ACCESS, 挂起(rid, 同 rid 去重不重置
  节流); tick 在非战斗时每 5s 重发, 上限 _INVITE_ACCESS_MAX=12 次后 warn 一次;
  入队成功(on_build_team)或 reset 后清除。

修2（队长侧, tick 邀请窗口）:
  建队后 _INVITE_WINDOW_MS=90000ms 内, 每 5s 对"无服务端入队证据"的成员补发邀请;
  证据 = entered_role_ids（S2C_APPEND_ACTIVE/PARTED_TEAM_MEMBER 广播 + BUILD_TEAM
  名单; remove 时撤销）。auto-confirm 保持 10s 只做**本地就绪标记**, 不再因此停发
  —— 核心不变式: 不得"标记 ready 后停发"。

本自检（stub 真跑, 不连服务器）:
  A 修1 行为: 战斗中挂起(不空发/去重不重置节流) → 非战斗到点重发 → 节流内不重发 →
    上限 12 次 warn 一次停止 → on_build_team 清挂起 → 非战斗邀请保持即时同意;
  B 修2 行为: 建队后窗口开启日志 → 窗口内每 5s 重发(含 auto-confirm 后**继续**重发,
    核心不变式) → 入队广播(暂离/活跃)后不再打扰 → 窗口结束 warn 一次且不再发;
  C 兼容: 新字段存在/reset 清零/旧对象(缺字段)不崩;
  D 静态: 源码挂钩(函数/常量/tick 接线/日志锚点/非回归);
  E 灵敏度: ① 备份版(.bak_20260930_teamfix2=修1/修2 前)跑同套断言必 FAIL;
            ② 内存变异(窗口失效)行为必 FAIL;
  F 行尾 + 双副本(可选 second_dir)。

用法: python tools/team_invite_window_selftest.py [script_dir [second_dir]]
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
EVENTS = []


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
    mod = types.ModuleType(name)
    mod.__file__ = "<selftest:%s>" % name
    exec(compile(src, mod.__file__, "exec"), mod.__dict__)
    return mod


class Clock(object):
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
        self.sends = []
        self.m_quest = None

    def get_role_name(self):
        return self.role_name

    def get_role_id(self):
        return self.m_id

    def send_message(self, msg_id, data):
        self.sends.append((msg_id, list(data)))
        return 0


def count_msg(robot, msg):
    return sum(1 for m, d in robot.sends if m == msg)


def last_targets(robot, msg):
    out = []
    for m, d in robot.sends:
        if m == msg:
            out.append(tuple(d))
    return out


def _has_msg(evs, since, substr):
    return any(substr in str(e.get("msg", "")) for e in evs[since:])


# ================================================================ A: 修1 行为
def run_fix1(tc, tag, out):
    clock = Clock()
    setattr(tc, "__now_ms", clock.ms)

    def add(name, cond, detail=""):
        out.append(("[%s] %s" % (tag, name), bool(cond), detail))

    ACC = "C2S_TEAM_INVITE_ACCESS"

    # A1: 战斗中邀请 → 挂起不空发
    robot = FakeRobot()
    t = tc.TeamState()
    t.role = "member"
    t.captain_role_id = 1090414
    robot.m_team = t
    robot.m_fight_state = True
    n0 = len(EVENTS)
    tc.on_team_invite(robot, [1090414, "队长"])
    add("A1 战斗中邀请→不发 ACCESS", count_msg(robot, ACC) == 0,
        "acc=%d" % count_msg(robot, ACC))
    add("A2 挂起 rid 记录", int(getattr(t, "pending_invite_rid", 0)) == 1090414)
    add("A3 挂起日志'挂起待非战斗重发'", _has_msg(EVENTS, n0, "挂起待非战斗重发"))

    # A4: 同 rid 重复邀请(仍战斗) → 去重: tries/last_ms 不变, 不重复日志
    lm0 = int(getattr(t, "pending_invite_last_ms", 0))
    clock.v += 2.0
    n1 = len(EVENTS)
    tc.on_team_invite(robot, [1090414, "队长"])
    add("A4 同 rid 重复邀请去重(节流不重置)",
        int(getattr(t, "pending_invite_last_ms", 0)) == lm0
        and int(getattr(t, "pending_invite_tries", 0)) == 0)
    add("A5 去重不重复挂起日志", not _has_msg(EVENTS, n1, "挂起待非战斗重发"))

    # A6: 战斗结束, 节流内不重发
    robot.m_fight_state = False
    clock.v += 1.0		# 距 arm 3s
    tc.tick(robot, clock.v)
    add("A6 非战斗但节流内(3s<5s)不重发", count_msg(robot, ACC) == 0,
        "acc=%d" % count_msg(robot, ACC))

    # A7: 到点重发
    clock.v += 3.0		# 距 arm 6s
    n2 = len(EVENTS)
    tc.tick(robot, clock.v)
    add("A7 非战斗到点重发 ACCESS", count_msg(robot, ACC) == 1,
        "acc=%d" % count_msg(robot, ACC))
    add("A8 重发计数=1 + 日志'非战斗重发同意入队'",
        int(getattr(t, "pending_invite_tries", 0)) == 1
        and _has_msg(EVENTS, n2, "非战斗重发同意入队"))

    # A9: 下一个 5s 窗口再重发
    clock.v += 9.0
    tc.tick(robot, clock.v)
    add("A9 下窗口再重发(第 2 次)", count_msg(robot, ACC) == 2,
        "acc=%d" % count_msg(robot, ACC))

    # A10: 上限 12 次后 warn 一次并停止
    n3 = len(EVENTS)
    raised = ""
    try:
        for _i in range(11):	# 已发 2 次, 补 10 次达 12
            clock.v += 9.0
            tc.tick(robot, clock.v)
    except Exception:
        import traceback
        raised = traceback.format_exc()
    add("A10 补满 12 次不抛异常", raised == "", raised[:150])
    add("A11 重发上限=12(总 ACCESS=12)", count_msg(robot, ACC) == 12,
        "acc=%d" % count_msg(robot, ACC))
    # 再走两个窗口 → 不再发, warn 一次
    clock.v += 9.0
    tc.tick(robot, clock.v)
    clock.v += 9.0
    tc.tick(robot, clock.v)
    warns = [e for e in EVENTS[n3:] if "邀请挂起重发" in str(e.get("msg", ""))]
    add("A12 达上限 warn 一次且不再重发",
        len(warns) == 1 and count_msg(robot, ACC) == 12,
        "warns=%d acc=%d" % (len(warns), count_msg(robot, ACC)))

    # A13: on_build_team 清挂起 → 不再重发
    robot2 = FakeRobot()
    t2 = tc.TeamState()
    t2.role = "member"
    t2.captain_role_id = 1090414
    robot2.m_team = t2
    robot2.m_fight_state = True
    tc.on_team_invite(robot2, [1090414, "队长"])
    add("A13 前置: 挂起已建立", int(getattr(t2, "pending_invite_rid", 0)) == 1090414)
    robot2.m_fight_state = False
    tc.on_build_team(robot2, [1090414, "测试队", [], []])
    add("A14 on_build_team 清挂起 + setup_done",
        int(getattr(t2, "pending_invite_rid", 0)) == 0
        and bool(getattr(t2, "setup_done", False)))
    clock.v += 30.0
    tc.tick(robot2, clock.v)
    add("A15 入队后不再重发 ACCESS", count_msg(robot2, ACC) == 0,
        "acc=%d" % count_msg(robot2, ACC))

    # A16: 非战斗邀请保持即时同意(既有行为)
    robot3 = FakeRobot()
    t3 = tc.TeamState()
    t3.role = "member"
    t3.captain_role_id = 1090414
    robot3.m_team = t3
    robot3.m_fight_state = False
    tc.on_team_invite(robot3, [1090414, "队长"])
    add("A16 非战斗邀请即时发 ACCESS(既有行为)",
        count_msg(robot3, ACC) == 1 and int(getattr(t3, "pending_invite_rid", 0)) == 0)
    return out


# ================================================================ B: 修2 行为
def run_fix2(tc, tag, out):
    clock = Clock()
    setattr(tc, "__now_ms", clock.ms)

    def add(name, cond, detail=""):
        out.append(("[%s] %s" % (tag, name), bool(cond), detail))

    OFF = "C2S_TEAM_OFFER"

    def build_captain(rids=(11, 22)):
        robot = FakeRobot(rid=111)
        ret = tc.dispatch_cmd(robot, {"cmd": "team_setup_captain",
            "members": [{"role_id": r} for r in rids], "next_action": None})
        tc.on_build_team(robot, [111, "测试队", [], []])
        return robot

    # B1: 建队 → 首发邀请 + 窗口开启日志
    robot = build_captain()
    t = robot.m_team
    add("B1 建队首发邀请 x2", count_msg(robot, OFF) == 2,
        "off=%d" % count_msg(robot, OFF))
    add("B2 窗口开启日志", any("邀请窗口开启" in str(e.get("msg", "")) for e in EVENTS))
    add("B3 窗口常量=90s(静态字段)", int(getattr(tc, "_INVITE_WINDOW_MS", 0)) == 90000)

    # B4: 窗口内每 5s 重发(10s 内, auto-confirm 前)
    clock.v += 6.0
    tc.tick(robot, clock.v)
    add("B4 窗口内重发(6s) 未入队成员 x2", count_msg(robot, OFF) == 4,
        "off=%d" % count_msg(robot, OFF))
    add("B5 重发日志'邀请窗口重发'",
        any("邀请窗口重发" in str(e.get("msg", "")) for e in EVENTS))

    # B6(核心不变式): +11s auto-confirm 触发, 但邀请**继续**(同 tick 窗口块补发)
    n0 = len(EVENTS)
    off0 = count_msg(robot, OFF)
    clock.v += 5.0		# 建队后 11s
    tc.tick(robot, clock.v)
    add("B6 auto-confirm 触发(本地就绪)", _has_msg(EVENTS, n0, "邀请超时, 自动确认全部成员"))
    add("B7 核心不变式: 确认后仍继续补发邀请", count_msg(robot, OFF) == off0 + 2,
        "off=%d -> %d" % (off0, count_msg(robot, OFF)))

    # B8: 暂离广播 = 真入队证据 → 不再对该员补邀
    tc.on_append_parted_member(robot, [11])
    add("B8 暂离广播计入 entered", 11 in (getattr(t, "entered_role_ids", []) or []))
    clock.v += 5.0
    n1 = count_msg(robot, OFF)
    tc.tick(robot, clock.v)
    add("B9 已入队(暂离)成员不再被补邀(剩余只发 22)",
        count_msg(robot, OFF) == n1 + 1 and last_targets(robot, OFF)[-1] == (22,),
        "targets_tail=%s" % (last_targets(robot, OFF)[-2:],))

    # B10: 活跃广播同样计入 → 全员入队后窗口内零打扰
    tc.on_append_active_member(robot, [22])
    add("B10 活跃广播计入 entered", 22 in (getattr(t, "entered_role_ids", []) or []))
    clock.v += 5.0
    n2 = count_msg(robot, OFF)
    tc.tick(robot, clock.v)
    add("B11 全员入队后窗口内不再发邀请", count_msg(robot, OFF) == n2,
        "off=%d" % count_msg(robot, OFF))

    # B12: remove 撤销证据 → 窗口内可被重新补邀
    tc.on_remove_member(robot, [11])
    add("B12 离开队伍撤销 entered", 11 not in (getattr(t, "entered_role_ids", []) or []))
    clock.v += 5.0
    n3 = count_msg(robot, OFF)
    tc.tick(robot, clock.v)
    add("B13 离开成员被重新补邀", count_msg(robot, OFF) == n3 + 1
        and last_targets(robot, OFF)[-1] == (11,),
        "targets_tail=%s" % (last_targets(robot, OFF)[-2:],))

    # B14: 窗口结束(>90s) warn 一次, 不再发
    robotB = build_captain()
    clock.v += 95.0
    n4 = len(EVENTS)
    off_b0 = count_msg(robotB, OFF)
    tc.tick(robotB, clock.v)
    tc.tick(robotB, clock.v + 9.0)
    add("B14 窗口结束 warn(仍有未入队成员)",
        any(e.get("level") == "warn" and "邀请窗口结束" in str(e.get("msg", ""))
            for e in EVENTS[n4:]))
    adds = [e for e in EVENTS[n4:] if "邀请窗口结束" in str(e.get("msg", ""))]
    add("B15 窗口结束日志只打一次", len(adds) == 1,
        "close_logs=%d" % len(adds))
    add("B16 窗口结束后不再补发", count_msg(robotB, OFF) == off_b0,
        "off=%d -> %d" % (off_b0, count_msg(robotB, OFF)))

    # B17: reset 清窗口状态(重派 setup 后 entered/window_closed 归零)
    tc.dispatch_cmd(robotB, {"cmd": "team_setup_captain",
        "members": [{"role_id": 33}], "next_action": None})
    tB = robotB.m_team
    add("B17 重派后 entered/窗口标记已清零",
        (getattr(tB, "entered_role_ids", None) or []) == []
        and not getattr(tB, "invite_window_closed", False))
    return out


# ================================================================ C: 兼容
def run_compat(tc, tag, out):
    clock = Clock()
    setattr(tc, "__now_ms", clock.ms)

    def add(name, cond, detail=""):
        out.append(("[%s] %s" % (tag, name), bool(cond), detail))

    t = tc.TeamState()
    add("C1 修1/修2 新字段存在", all(hasattr(t, k) for k in (
        "pending_invite_rid", "pending_invite_last_ms", "pending_invite_tries",
        "pending_invite_warned", "entered_role_ids", "invite_window_closed")))
    t.pending_invite_rid = 9
    t.pending_invite_tries = 3
    t.pending_invite_warned = True
    t.entered_role_ids = [11]
    t.invite_window_closed = True
    t.reset()
    add("C2 reset 清零修1/修2 字段",
        t.pending_invite_rid == 0 and t.pending_invite_tries == 0
        and t.pending_invite_warned is False
        and (t.entered_role_ids or []) == [] and t.invite_window_closed is False)

    # C3: 旧对象(缺修1/修2字段)不崩
    robot = FakeRobot()
    t2 = tc.TeamState()
    t2.role = "member"
    t2.captain_role_id = 7
    robot.m_team = t2
    for f in ("pending_invite_rid", "pending_invite_last_ms", "pending_invite_tries",
              "pending_invite_warned", "entered_role_ids", "invite_window_closed"):
        try:
            delattr(t2, f)
        except Exception:
            pass
    ok1 = True
    try:
        tc.tick(robot, clock.v)
    except Exception:
        ok1 = False
    add("C3 队员旧对象 tick 不崩", ok1)

    robot2 = FakeRobot()
    ret = tc.dispatch_cmd(robot2, {"cmd": "team_setup_captain",
        "members": [{"role_id": 11}], "next_action": None})
    tc.on_build_team(robot2, [111, "队", [], []])
    t3 = robot2.m_team
    for f in ("entered_role_ids", "invite_window_closed"):
        try:
            delattr(t3, f)
        except Exception:
            pass
    ok2 = True
    try:
        clock.v += 6.0
        tc.tick(robot2, clock.v)
    except Exception:
        ok2 = False
    add("C4 队长旧对象 tick 不崩(窗口块 getattr 兜底)", ok2)
    add("C5 旧对象窗口仍可补发", count_msg(robot2, "C2S_TEAM_OFFER") >= 2,
        "off=%d" % count_msg(robot2, "C2S_TEAM_OFFER"))
    return out


# ================================================================ D: 静态
def run_static(src, tag, out):
    def add(name, cond, detail=""):
        out.append(("[%s] %s" % (tag, name), bool(cond), detail))

    add("D1 __tick_invite_access_retry 存在", "def __tick_invite_access_retry(" in src)
    add("D2 修1 常量(5000/12)", "_INVITE_ACCESS_GAP_MS = 5000" in src
        and "_INVITE_ACCESS_MAX = 12" in src)
    add("D3 修2 窗口常量=90000", "_INVITE_WINDOW_MS = 90000" in src)
    add("D4 tick 队员分支接线", "return __tick_invite_access_retry(robot_object, t)" in src)
    fn = _extract_func(src, "__tick_invite_access_retry")
    add("D5 修1: 战斗闸+上限 warn+重发 ACCESS",
        'getattr(robot_object, "m_fight_state", False)' in fn
        and "_INVITE_ACCESS_MAX" in fn and "protocol3.C2S_TEAM_INVITE_ACCESS" in fn
        and "邀请挂起重发" in fn)
    iv = _extract_func(src, "on_team_invite")
    add("D6 on_team_invite: 战斗挂起分支(去重不重置节流)",
        "pending_invite_rid" in iv and "_prev != rid" in iv
        and "挂起待非战斗重发同意" in iv)
    add("D7 on_build_team(队员): 清挂起",
        "pending_invite_rid = 0" in _extract_func(src, "on_build_team"))
    tk = _extract_func(src, "tick")
    add("D8 tick 窗口: entered 过滤+重发+结束 warn",
        "entered_role_ids" in tk and "邀请窗口重发" in tk
        and "邀请窗口结束" in tk)
    add("D9 核心不变式: auto-confirm 后无 return 停发(落窗块)",
        "自动确认全部成员" in tk and "_INVITE_WINDOW_MS" in tk)
    ap = _extract_func(src, "on_append_parted_member")
    aa = _extract_func(src, "on_append_active_member")
    add("D10 入队广播计入 entered(暂离/活跃)",
        "entered_role_ids" in ap and "entered_role_ids" in aa)
    rm = _extract_func(src, "on_remove_member")
    add("D11 remove 撤销 entered", "entered_role_ids" in rm)
    add("D12 非回归: 建队重发仍在(__tick_create_retry)", "__tick_create_retry" in src)
    add("D13 非回归: promote/归队函数仍在",
        "def on_promote" in src and "def __tick_return" in src)
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

    print("== A-D: 修1/修2 行为 + 兼容 + 静态（当前版） ==")
    n_before = len(results)
    run_fix1(tc, "sys", results)
    run_fix2(tc, "sys", results)
    run_compat(tc, "sys", results)
    run_static(src, "sys", results)
    _print_items(results[n_before:])

    print("\n== E: 灵敏度（坏版必须 FAIL） ==")
    bak = path + ".bak_20260930_teamfix2"
    if os.path.exists(bak):
        src_bak = _read(bak)
        tc_bak = load_from_source("tc_bak_fix2", src_bak)
        out_bak = []
        run_fix1(tc_bak, "bak-fix1", out_bak)
        run_fix2(tc_bak, "bak-fix2", out_bak)
        run_static(src_bak, "bak-static", out_bak)
        fails_bak = _fails(out_bak)
        check("[E1] 灵敏度: 备份版(.bak_20260930_teamfix2)跑同套断言命中 %d 项(>0)"
            % len(fails_bak), len(fails_bak) > 0,
            "命中示例: %s" % ([r[0] for r in fails_bak[:3]],))
        for nm, _ok2, _dt in fails_bak[:6]:
            print("    [bak-FAIL] %s" % nm)
        check("[E1b] 灵敏度: 备份版命中'修1/修2'主断言",
            any(("A1" in r[0]) or ("B4" in r[0]) or ("D1" in r[0]) or ("D3" in r[0])
                for r in fails_bak))
    else:
        check("[E1] 灵敏度: 备份版缺失(跳过文件级, 由内存变异覆盖)", True, bak)

    mut = src.replace("if _elapsed <= _INVITE_WINDOW_MS:", "if False:", 1)
    tc_mut = load_from_source("tc_mut_fix2", mut)
    out_mut = []
    run_fix2(tc_mut, "mut-fix2", out_mut)
    fails_mut = _fails(out_mut)
    check("[E2] 灵敏度: 窗口失效(内存变异)命中 %d 项(>0)" % len(fails_mut),
        len(fails_mut) > 0,
        "命中示例: %s" % ([r[0] for r in fails_mut[:3]],))
    for nm, _ok2, _dt in fails_mut[:6]:
        print("    [mut-FAIL] %s" % nm)

    print("\n== F: 行尾 / 双副本 ==")
    raw = open(path, "rb").read()
    crlf = raw.count(b"\r\n")
    lf = raw.count(b"\n")
    check("[F1] team_captain.py 行尾一致(全CRLF或全LF)",
        crlf == 0 or crlf == lf, "crlf=%d lf=%d" % (crlf, lf))
    if len(sys.argv) > 2:
        path2 = os.path.join(sys.argv[2], "team_captain.py")
        if os.path.exists(path2):
            raw2 = open(path2, "rb").read()
            check("[F2] 双副本逐字节一致", raw == raw2,
                "%d vs %d bytes" % (len(raw), len(raw2)))
        else:
            check("[F2] 双副本: second_dir 缺 team_captain.py", False, path2)

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
