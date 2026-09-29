# -*- coding: utf-8 -*-
"""组队抓鬼·阶段 2（机器人端）自检 —— 2026-09-29。

范围（计划文档 docs/04-测试/计划-20260929-组队功能.md §5 阶段 2 + §3.4 助战令子项）:
  ① daily_ghost: 队员待命分支(role=member: 不接任务/不找鬼/不 CLICK/不发移动; 只参战);
  ② daily_ghost: 队长助战令链(接任务前使用 C2S_USEITEM[190146]; 购买链复用; team_token 上报);
  ③ team_captain: team_promote 命令 + on_promote 角色切换(旧队长→队员待命) + 队员态上报;
  ④ team_captain: 归队"同图走回"最小版(quest_engine A* 载体; route_walk 优先);
  ⑤ protocol3/msghandle: C2S_TEAM_PROMOTE(80197)/S2C_TEAM_PROMOTE(90393) 注册与分发;
  ⑥ P1/P2 事件层守卫(2026-09-29 独立复核补丁): member 在 on_load_task/on_add_task/
     on_finish_task/on_show_dialog 四处零动作(只记录/不发移动/不自主收工/不点选项) +
     归队 TOO_FAR 新坐标重置 nav_walk_tries(P2)。

本脚本四段:
  A 静态: 源码形状(上面 ①-⑤ 逐项; 函数体用 _extract_func 精确取段, 防"同名撞车");
  B 语义: 关键闸门顺序(助战令使用在开对话前: pre-hook 行号 < 钟馗空菜单等待行; 成员待命
          在疗伤/状态机之前); "成员分支内不得出现 __accept_welfare/pre_daily/__goto/__patrol";
  C 灵敏度: 对两份内存变异版(member 守卫删除 / promote 发送删除)跑同套检查 → 必须 FAIL;
  D 行尾: 每个被改文件行尾一致(全 CRLF 或全 LF, 不得混排) + 协议键两处注册齐全。

用法: python tools/team_phase2_selftest.py [script_dir]
      (默认 ...\\2d-xiyou-server\\robot\\deploy\\single_robot_zy\\script —— 当前唯一运行副本)
"""
import io
import os
import re
import sys

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

DEFAULT_DIR = r"F:\ZyBin\xm\2d-xiyou-server\robot\deploy\single_robot_zy\script"

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


def _line_of(src, pattern):
    for i, ln in enumerate(src.split("\n")):
        if pattern in ln:
            return i
    return -1


def run_checks(files, tag):
    """files: {basename: content} —— 对给定内容跑全部静态/语义检查。返回失败数。"""
    before = len([r for r in results if not r[1]])

    dg = files.get("daily_ghost.py", "")
    cfg = files.get("config.py", "")
    tc = files.get("team_captain.py", "")
    p3 = files.get("protocol3.py", "")
    mh = files.get("msghandle.py", "")
    ar = files.get("auto_roam.py", "")
    cl = files.get("client.py", "")

    # ---------- ① 配置开关 ----------
    check("[%s] config: robot_ghost_use_token_team 存在且独立(默认 True)" % tag,
        re.search(r"robot_ghost_use_token_team\s*=\s*True", cfg) is not None)
    check("[%s] config: 单人开关 robot_ghost_use_token 仍为 False(单人口径不变)" % tag,
        re.search(r"robot_ghost_use_token\s*=\s*False", cfg) is not None)

    # ---------- ② daily_ghost 常量与函数 ----------
    check("[%s] daily_ghost: 使用窗口/上报节流常量" % tag,
        "TOKEN_USE_WINDOW_MS" in dg and "TEAM_TOKEN_EMIT_MS" in dg)
    for fn in ("__team_token_enabled", "__token_use_window_active", "__team_token_report",
               "__use_token", "__maybe_use_team_token"):
        check("[%s] daily_ghost: 函数 %s 存在" % (tag, fn), _extract_func(dg, fn) != "")
    use_fn = _extract_func(dg, "__use_token")
    check("[%s] __use_token: 发 C2S_USEITEM + 本地乐观扣包" % tag,
        "protocol3.C2S_USEITEM" in use_fn and "item[1] = int(item[1]) - 1" in use_fn)
    check("[%s] __use_token: 记录 token_use_ts/use_count + team_token(ok)" % tag,
        "g.token_use_ts" in use_fn and "token_use_count" in use_fn
        and '__team_token_report(robot_object, g, True' in use_fn)
    mbt = _extract_func(dg, "__must_buy_token")
    check("[%s] __must_buy_token: 队长走独立开关(team)" % tag,
        "robot_ghost_use_token_team" in mbt and '== "captain"' in mbt)
    check("[%s] __must_buy_token: 使用窗口内不再买(防买卖循环)" % tag,
        "__token_use_window_active(g)" in mbt)
    rpt = _extract_func(dg, "__team_token_report")
    check("[%s] __team_token_report: 只队长 + 开关 + 失败节流 + team_token 事件" % tag,
        '"captain"' in rpt and "__team_token_enabled(g)" in rpt
        and "TEAM_TOKEN_EMIT_MS" in rpt and '"type": "team_token"' in rpt)

    # ---------- ③ 队员待命（dispatch_cmd + tick） ----------
    check("[%s] daily_ghost: dispatch_cmd 有 member 分支" % tag,
        'str(cmd.get("role", "solo")) == "member"' in dg)
    check("[%s] dispatch_cmd member: 置 role=member / MEMBER 态 / 返回 mode=member" % tag,
        'g.role = "member"' in dg and '__set_state(g, "MEMBER")' in dg
        and '"mode": "member"' in dg)
    check("[%s] daily_ghost: tick 有 member 待命早退分支" % tag,
        'str(getattr(g, "role", "solo")) == "member"' in dg)
    # 取 dispatch member 分支文本（从 role 判断到 "R1" 注释前）做"零动作"断言
    _i = dg.find('if str(cmd.get("role", "solo")) == "member":')
    _j = dg.find("2026-09-28 R1", _i) if _i >= 0 else -1
    mem_branch = dg[_i:_j] if (_i >= 0 and _j > _i) else ""
    check("[%s] member 启动分支: 不含 领福利/领双/接任务/找鬼/点鬼" % tag,
        mem_branch != "" and "__accept_welfare" not in mem_branch
        and "pre_daily" not in mem_branch and "on_show_dialog" not in mem_branch)
    check("[%s] member 启动分支: 有互斥停止(任务链/分享日常/游荡)" % tag,
        '{"cmd": "stop"}' in mem_branch and "share_daily_stop" in mem_branch
        and "m_collect_walk" in mem_branch)
    # tick member 分支（取到 return 0 的第一个块）
    _k = dg.find('if str(getattr(g, "role", "solo")) == "member":')
    _m = dg.find("return 0", _k) if _k >= 0 else -1
    tick_member = dg[_k:_m + 8] if (_k >= 0 and _m > _k) else ""
    check("[%s] tick member: 早退且零动作(无 __goto/__patrol/__goto_xy)" % tag,
        tick_member != "" and "return 0" in tick_member
        and "__goto" not in tick_member and "__patrol" not in tick_member)

    # ---------- ④ 助战令使用时机（开对话前） ----------
    pre = _line_of(dg, "__maybe_use_team_token(robot_object, g, quest)")
    wait = _line_of(dg, "if __broker_empty_waiting(g, now_ms):")
    check("[%s] READY: 使用助战令 pre-hook 在'去钟馗'之前" % tag,
        pre > 0 and wait > 0 and pre < wait,
        "pre_hook=%s broker_wait=%s" % (pre, wait))
    check("[%s] on_bag_add: 到账即使用(队长)" % tag,
        "__use_token(robot_object, g)" in _extract_func(dg, "on_bag_add"))
    accept_block = dg[dg.find("elif accept_opt is not None:"):][:1200]
    check("[%s] 接任务分支: 失败上报 team_token(false, 3 种 reason)" % tag,
        "__team_token_report(robot_object, g, False" in accept_block
        and "NO_MONEY" in accept_block and "BUY_FAIL" in accept_block
        and "NO_TOKEN" in accept_block)

    # ---------- ⑤ team_captain: promote + 队员态 + 归队走回 ----------
    check("[%s] team_captain: team_promote 命令(校验+发送 C2S_TEAM_PROMOTE)" % tag,
        'if name == "team_promote":' in tc and "protocol3.C2S_TEAM_PROMOTE" in tc
        and "not_captain" in tc and "target_not_member" in tc)
    op = _extract_func(tc, "on_promote")
    check("[%s] on_promote: team_promoted 事件 + 双向角色切换" % tag,
        '"type": "team_promoted"' in op and 't.role = "captain"' in op
        and 't.role = "member"' in op and '"role": "member"' in op)
    check("[%s] team_captain: 队员态上报 team_member_state(暂离/归队)" % tag,
        tc.count('"type": "team_member_state"') >= 2)
    check("[%s] team_captain: 归队走回(minimal) 三件套" % tag,
        "__start_walk_back" in tc and "__tick_walk_back" in tc and "__try_route_walk" in tc)
    walk = _extract_func(tc, "__start_walk_back")
    check("[%s] 走回: 同图复用 quest_engine A*(跨图只记日志)" % tag,
        "quest_engine.__do_walk" in walk and "跨图" in walk)
    tickw = _extract_func(tc, "__tick_walk_back")
    check("[%s] 走回: 到达(<=200px)发普通归队 arrival=1" % tag,
        "protocol3.C2S_TEAM_MEMBER_COME_BACK, [1]" in tickw and "<=" in tickw)

    # ---------- ⑤b 接口增量(2026-09-29 与 pool-fix-go 定稿) ----------
    check("[%s] daily_ghost: 队员方向防御(误发 solo/captain → 归一 member)" % tag,
        '_t0_role == "member" and _cmd_role != "member"' in dg
        and 'cmd["role"] = "member"' in dg)
    tsb = _extract_func(dg, "team_status_block")
    check("[%s] daily_ghost: team_status_block(心跳队伍块, 无队 None)" % tag,
        tsb != "" and '"captain_role_id"' in tsb and "token_use_ts" in tsb
        and "return None" in tsb)
    check("[%s] client: 心跳带 team 块(不在队省略)" % tag,
        "_dg_team.team_status_block(ro)" in cl and 'st["team"]' in cl)
    check("[%s] auto_roam: 在队期间禁止自发游荡" % tag,
        ('getattr(robot_object, "m_team", None)' in ar
         and 'in ("captain", "member", "applicant")' in ar))
    check("[%s] team_captain: team_promoted 带 new_team_name" % tag,
        '"new_team_name"' in tc)

    # ---------- ⑤c P1/P2 事件层守卫(2026-09-29 独立复核补丁) ----------
    ms_fn = _extract_func(dg, "__member_standby")
    check("[%s] daily_ghost: __member_standby 守卫函数(role==member)" % tag,
        ms_fn != "" and '"member"' in ms_fn)
    check("[%s] daily_ghost: 事件层调用齐全(≥4 处 + 定义)" % tag,
        dg.count("__member_standby(g)") >= 5)
    # ① on_load_task: 队员只缓存任务 → continue(不发移动)
    lt = _extract_func(dg, "on_load_task")
    _pi = lt.find("__member_standby(g)")
    _pj = lt.find("if ti == GHOST_SUBMIT_TASK", _pi) if _pi >= 0 else -1
    _pseg = lt[_pi:_pj] if (_pi >= 0 and _pj > _pi) else ""
    check("[%s] on_load_task 队员守卫: 只缓存不动作(continue/无 __goto)" % tag,
        _pseg != "" and "continue" in _pseg and "__goto(robot_object" not in _pseg)
    # ② on_add_task: 队员只 __cache_task + 上报, 不切状态不发移动
    at2 = _extract_func(dg, "on_add_task")
    _pi = at2.find("__member_standby(g)")
    _pj = at2.find("if ti == GHOST_SUBMIT_TASK", _pi) if _pi >= 0 else -1
    _pseg = at2[_pi:_pj] if (_pi >= 0 and _pj > _pi) else ""
    check("[%s] on_add_task 队员守卫: 只写缓存+上报(无 __goto/__set_state)" % tag,
        _pseg != "" and "__cache_task" in _pseg and "__goto(robot_object" not in _pseg
        and "__set_state" not in _pseg)
    # ③ on_finish_task: done 计数保留(+1 在守卫之前) + 禁自主收工
    ft2 = _extract_func(dg, "on_finish_task")
    _pc = ft2.find("g.done_count += 1")
    _pg = ft2.find("__member_standby(g)")
    check("[%s] on_finish_task 队员守卫: done 计数在守卫前(保留 +1)" % tag,
        _pc >= 0 and _pg > _pc)
    _pj = ft2.find("if g.done_count >= g.daily_limit", _pg) if _pg >= 0 else -1
    _pseg = ft2[_pg:_pj] if (_pg >= 0 and _pj > _pg) else ""
    check("[%s] on_finish_task 队员守卫: 拦自主收工(DONE/游荡/READY/切换)" % tag,
        _pseg != "" and "return True" in _pseg and "__emit_ghost_done" not in _pseg
        and '"DONE"' not in _pseg and '"READY"' not in _pseg
        and "run_switch_pending" not in _pseg)
    # ④ on_show_dialog: 早退 return True(不点选项, 不落 pre_daily 分支)
    sd2 = _extract_func(dg, "on_show_dialog")
    _pi = sd2.find("__member_standby(g)")
    _pj = sd2.find("# 2026-09-22 领双前置", _pi) if _pi >= 0 else -1
    _pseg = sd2[_pi:_pj] if (_pi >= 0 and _pj > _pi) else ""
    check("[%s] on_show_dialog 队员守卫: 早退 return True(不点选项)" % tag,
        _pseg != "" and "return True" in _pseg and "pre_daily" not in _pseg)
    # ⑤ P2: on_come_back_too_far 收到新坐标 → 重置走回尝试计数
    cbf = _extract_func(tc, "on_come_back_too_far")
    _pr = cbf.find('t.return_phase = "nav"')
    _pn = cbf.find("t.nav_walk_tries = 0")
    check("[%s] team_captain P2: TOO_FAR 新坐标 → 重置 nav_walk_tries" % tag,
        _pr >= 0 and _pn > _pr)

    # ---------- ⑥ 协议注册 ----------
    check("[%s] protocol3: C2S_TEAM_PROMOTE 常量+格式注册" % tag,
        "C2S_TEAM_PROMOTE = protocol2.c2s_key.C2S_TEAM_PROMOTE" in p3
        and "FORMAT_MC[C2S_TEAM_PROMOTE]" in p3)
    check("[%s] protocol3: S2C_TEAM_PROMOTE 常量+格式+分发" % tag,
        "S2C_TEAM_PROMOTE = protocol2.s2c_key.S2C_TEAM_PROMOTE" in p3
        and "FORMAT_MS[S2C_TEAM_PROMOTE]" in p3
        and "S2C_TEAM_PROMOTE : msghandle.team_promote_handle" in p3)
    check("[%s] msghandle: team_promote_handle 转发 team_captain.on_promote" % tag,
        "def team_promote_handle" in mh and "team_captain.on_promote" in mh)

    return len([r for r in results if not r[1]]) - before


def main():
    script_dir = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_DIR
    names = ["daily_ghost.py", "config.py", "team_captain.py", "protocol3.py", "msghandle.py",
             "auto_roam.py", "client.py"]
    files = {}
    missing = []
    for n in names:
        p = os.path.join(script_dir, n)
        if not os.path.exists(p):
            missing.append(n)
            continue
        files[n] = _read(p)
    if missing:
        print("[FAIL] 缺少文件: %s (%s)" % (missing, script_dir))
        sys.exit(2)

    print("== A/B: 静态+语义 ==")
    run_checks(files, "sys")

    print("\n== C: 灵敏度(坏版必须 FAIL) ==")
    # C1: 删除 member 守卫 → member 检查必须失败
    bad = dict(files)
    bad_dg = files["daily_ghost.py"].replace('str(getattr(g, "role", "solo")) == "member"',
        'str(getattr(g, "role", "solo")) == "___never___"', 1)
    bad["daily_ghost.py"] = bad_dg
    _n0 = len(results)
    f1 = run_checks(bad, "sens-member")
    # C2: 删除 promote 发送 → promote 检查必须失败
    bad2 = dict(files)
    bad2["team_captain.py"] = files["team_captain.py"].replace(
        "protocol3.C2S_TEAM_PROMOTE", "protocol3.C2S_TEAM_QUIT", 1)
    f2 = run_checks(bad2, "sens-promote")
    # C3: 删除 auto_roam 组队闸 + client 队伍块 → 对应检查必须失败
    bad3 = dict(files)
    bad3["auto_roam.py"] = files["auto_roam.py"].replace(
        'in ("captain", "member", "applicant")', 'in ("__never__",)', 1)
    bad3["client.py"] = files["client.py"].replace(
        "_dg_team.team_status_block(ro)", "_dg_team.___never___(ro)", 1)
    f3 = run_checks(bad3, "sens-roam-client")
    # C4: P1 事件层守卫整体删除(定义+4 调用点) → 对应检查必须失败
    bad4 = dict(files)
    bad4["daily_ghost.py"] = files["daily_ghost.py"].replace(
        "__member_standby(g)", "___never___(g)")
    f4 = run_checks(bad4, "sens-p1")
    # C5: P2 归队 TOO_FAR 的 tries 重置删除(段内替换, 兼容 CRLF) → 必须失败
    bad5 = dict(files)
    _btc = files["team_captain.py"]
    _bi = _btc.find("def on_come_back_too_far")
    _bj = _btc.find("\ndef ", _bi + 1) if _bi >= 0 else -1
    if _bi >= 0 and _bj > _bi:
        _bseg = _btc[_bi:_bj].replace(
            "t.nav_walk_tries = 0", "t.nav_walk_tries = t.nav_walk_tries", 1)
        bad5["team_captain.py"] = _btc[:_bi] + _bseg + _btc[_bj:]
    f5 = run_checks(bad5, "sens-p2")
    _sens_n = len(results) - _n0
    del results[_n0:]   # 坏版的 FAIL 是预期行为, 不计入总账
    check("[C] 灵敏度: member 守卫删除 → 坏版命中 %d 项(>0)" % f1, f1 > 0)
    check("[C] 灵敏度: promote 发送删除 → 坏版命中 %d 项(>0)" % f2, f2 > 0)
    check("[C] 灵敏度: auto_roam/client 闸删除 → 坏版命中 %d 项(>0)" % f3, f3 > 0)
    check("[C] 灵敏度: P1 事件层守卫删除 → 坏版命中 %d 项(>0)" % f4, f4 > 0)
    check("[C] 灵敏度: P2 tries 重置删除 → 坏版命中 %d 项(>0)" % f5, f5 > 0)
    check("[C] 灵敏度: 坏版检查项共 %d 条已执行并清理" % _sens_n, _sens_n > 0)

    print("\n== D: 行尾/双注册 ==")
    for n in names:
        raw = open(os.path.join(script_dir, n), "rb").read()
        crlf = raw.count(b"\r\n")
        lf = raw.count(b"\n")
        check("[D] %s 行尾一致(全CRLF或全LF)" % n,
            crlf == 0 or crlf == lf, "crlf=%d lf=%d" % (crlf, lf))

    fails = [r for r in results if not r[1]]
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
