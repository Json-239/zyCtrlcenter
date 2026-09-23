# -*- coding: utf-8 -*-
"""采集探针自检（2026-09-22）

覆盖：开关判据（默认关闭 / 指定号生效）、采集图白名单、可疑模板号启发式、
候选挑选（有坐标才算）、抓鬼/游荡中不抢操作、异常不抛。

用法：python tools/gather_probe_selftest.py <script 目录>
"""
import os
import sys
import types

RESULTS = []


def check(name, cond, detail=""):
    RESULTS.append((name, bool(cond), detail))


class FakeRobot(object):
    def __init__(self, account="qa0001@xy3.com", mapid=10):
        self.m_account = [account]
        self.m_mapid = mapid
        self.m_logined = True
        self.m_npc_add_raw = {}
        self.m_npc_positions = {}
        self.sent = []

    def send_message(self, mid, data):
        self.sent.append((mid, list(data)))
        return 0


def main():
    if len(sys.argv) < 2:
        print("用法: python tools/gather_probe_selftest.py <script 目录>")
        return 2
    script_dir = sys.argv[1]
    sys.path.insert(0, script_dir)

    # config/diag 桩（探针内部是惰性 import，这里先塞进 sys.modules）
    cfg = types.ModuleType("config")
    cfg.robot_gather_probe_account = ""
    cfg.robot_gather_probe_click = False
    sys.modules["config"] = cfg
    diag = types.ModuleType("diag")
    diag.log = lambda *a, **k: None
    sys.modules["diag"] = diag

    import gather_probe as gp

    # ① 开关：默认关闭 / 指定号生效
    check("空配置 → 探针关闭", gp.probe_account() == "")
    cfg.robot_gather_probe_account = "robot0001000@xy3.com"
    check("指定号 → 返回该号", gp.probe_account() == "robot0001000@xy3.com")
    check("click 默认 False", gp.click_enabled() is False)
    cfg.robot_gather_probe_click = True
    check("click=True 生效", gp.click_enabled() is True)

    # ② 采集图白名单（gather.xml 的 11 张图）
    check("is_gather_map: 6/10/17 是采集图",
          gp.is_gather_map(6) and gp.is_gather_map(10) and gp.is_gather_map(17))
    check("is_gather_map: 1/11/24 不是采集图",
          not gp.is_gather_map(1) and not gp.is_gather_map(11) and not gp.is_gather_map(24))

    # ③ 可疑模板号启发式
    check("可疑模板: 17xxx 命中", gp.is_candidate_tpl(17100) and gp.is_candidate_tpl(17411))
    check("常规 NPC 模板不算可疑", not gp.is_candidate_tpl(13403) and not gp.is_candidate_tpl(10146))
    check("非法输入不算可疑", not gp.is_candidate_tpl(0) and not gp.is_candidate_tpl(9999)
          and not gp.is_candidate_tpl(100000) and not gp.is_candidate_tpl(None))

    # ④ 候选挑选：必须有坐标
    r = FakeRobot()
    r.m_npc_add_raw = {555001: [17100, "raw"], 555002: [13403, "raw"]}
    r.m_npc_positions = {555001: [0, 320, 480]}   # 有坐标的可疑点
    cand = gp._pick_candidate(r)
    check("候选挑选: 命中 17100 模板 + 坐标", cand == (555001, 17100, 320, 480), "cand=%s" % (cand,))
    r2 = FakeRobot()
    r2.m_npc_add_raw = {555003: [13403, "raw"]}   # 只有常规 NPC
    check("候选挑选: 只有常规 NPC → None", gp._pick_candidate(r2) is None)

    # ⑤ 抓鬼中不抢操作（tick 对抓到鬼的号零动作、不抛异常）
    r3 = FakeRobot(account="robot0001000@xy3.com")
    r3.m_ghost = type("G", (), {"enabled": True})()
    try:
        gp.tick(r3, 1000)
        check("抓鬼中: tick 零动作不抛", r3.sent == [])
    except Exception as e:  # noqa
        check("抓鬼中: tick 零动作不抛", False, str(e))

    # ⑥ 非指定号零开销
    r4 = FakeRobot(account="other@xy3.com")
    try:
        gp.tick(r4, 1000)
        check("非指定号: tick 零动作", r4.sent == [] and getattr(r4, "m_gather_probe", None) is None)
    except Exception as e:  # noqa
        check("非指定号: tick 零动作", False, str(e))

    nfail = 0
    for name, ok, detail in RESULTS:
        if ok:
            print("[PASS] %s" % name)
        else:
            nfail += 1
            print("[FAIL] %s %s" % (name, ("(" + str(detail) + ")") if detail else ""))
    print("结果: %d 项，失败 %d 项" % (len(RESULTS), nfail))
    return 0 if nfail == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
