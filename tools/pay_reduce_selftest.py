# -*- coding: utf-8 -*-
"""抓鬼付费降量自检 —— 2026-09-23

背景（生产取证，2026-09-23 全天 410 个活跃号）:
  `13255`（"请送我去幽冥界 5银"）全天点击 8035 次 = 40175 银; 其中
  "等刷鬼超时 → 回 24 点钟馗放弃重接"触发 2306 次(28.7%)。而回 24 点"放弃捉鬼
  任务" 2365 次里服务端确认 S2C_DROP_TASK 仅 736 次(31%) —— 其余 69% 被拒:
  服务端 config/task/20195.xml `action_ticket_drop_task.money_count=500`
  （放弃扣 500 银两），而生产号银两只几十~几百(diag.log MATCH_TOTAL_ROLE 实证)
  → 必然被拒。被拒后的行为与"不回去"完全等价（回刷鬼图继续等鬼），
  白花一次 13255(5 银) + 8~13 分钟往返。

修复（daily_ghost.py，单向: 首次被拒后本会话不再回 24 放弃）:
  ① `__note_abandon_nodrop`: 点"放弃捉鬼任务"(记 abandon_click_ms)后
     GHOST_ABANDON_NODROP_MS(60s) 内没等到 DROP_TASK(on_drop_task 清标记)
     → 记 abandon_fail_seen += 1（服务端拒绝证据）。
  ② `__should_skip_broker_abandon`: 被拒过(fail_seen>0) 且"不回 24"次数
     < GHOST_WAIT_EXTEND_MAX 时 → 跳过"回 24 放弃"：留原地换图等鬼
     (rounds 回退到 3 走既有换图/巡逻档), 省 5 银 + 往返时间。
  ③ 兜底: 延长上限(2 次 ≈ 6 轮/180s)后仍回落一次（任务真卡死时靠回钟馗重接）。
  ④ 记忆清理: on_drop_task(放弃成功)/on_finish_task(交付完成)/reset(新会话)
     清零 → 不会因旧记忆永久关闭该路径。

用法:
  python tools/pay_reduce_selftest.py <script 目录 或 daily_ghost.py 路径>
"""
import os
import re
import sys
import types

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass


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


def _block_between(src, start_marker, end_marker):
    """截取 [start_marker, end_marker) 之间的文本（找不到返回 None）。"""
    i = src.find(start_marker)
    if i < 0:
        return None
    j = src.find(end_marker, i)
    if j < 0:
        return None
    return src[i:j]


class _G(object):
    """GhostState 最简替身: 只带本自检用到的字段。"""

    def __init__(self, **kw):
        for k, v in kw.items():
            setattr(self, k, v)


def main():
    if len(sys.argv) < 2:
        print("用法: python tools/pay_reduce_selftest.py <script 目录 或 daily_ghost.py 路径>")
        return 2
    script_dir = _resolve(sys.argv[1])
    dh_path = os.path.join(script_dir, "daily_ghost.py")
    if not os.path.exists(dh_path):
        print("[FAIL] 找不到 %s" % dh_path)
        return 2
    dh = open(dh_path, encoding="utf-8", errors="replace").read()

    results = []

    def check(name, cond, detail=""):
        results.append((name, bool(cond), detail))

    # ============================================================ A. 源码形状
    check("daily_ghost 定义 GHOST_ABANDON_NODROP_MS = 60000",
          "GHOST_ABANDON_NODROP_MS = 60000" in dh)
    check("daily_ghost 定义 GHOST_WAIT_EXTEND_MAX = 2",
          "GHOST_WAIT_EXTEND_MAX = 2" in dh)
    check("daily_ghost 定义 __note_abandon_nodrop(被拒判定)", "def __note_abandon_nodrop(" in dh)
    check("daily_ghost 定义 __should_skip_broker_abandon(降频闸)",
          "def __should_skip_broker_abandon(" in dh)

    # GhostState 新增三字段（在 __init__ 里赋值）
    init_block = _block_between(dh, "class GhostState(object):", "\tdef reset(self):")
    check("GhostState.__init__ 有 abandon_click_ms 字段",
          init_block is not None and "self.abandon_click_ms = 0" in init_block)
    check("GhostState.__init__ 有 abandon_fail_seen 字段",
          init_block is not None and "self.abandon_fail_seen = 0" in init_block)
    check("GhostState.__init__ 有 ghost_wait_extend 字段",
          init_block is not None and "self.ghost_wait_extend = 0" in init_block)

    reset_block = _block_between(dh, "\tdef reset(self):", "\ndef __now_ms():")
    check("reset() 清零 abandon_click_ms(新会话重给机会)",
          reset_block is not None and "self.abandon_click_ms = 0" in reset_block)
    check("reset() 清零 abandon_fail_seen",
          reset_block is not None and "self.abandon_fail_seen = 0" in reset_block)
    check("reset() 清零 ghost_wait_extend",
          reset_block is not None and "self.ghost_wait_extend = 0" in reset_block)

    # 点"放弃"处记在途
    pick_block = _block_between(dh, "abandon_opt = None", "if abandon_opt is not None:")
    check("on_show_dialog 放弃分支前有扫描(原文保留)",
          pick_block is not None and '"放弃" in _txt and "捉鬼" in _txt' in pick_block)
    click_block = _block_between(dh, "if abandon_opt is not None:",
                                 "__log(robot_object, \"warn\",\n\t\t\t\t\t\"钟馗已有任务但本地无记录")
    check("点击放弃处记 abandon_click_ms = __now_ms()",
          click_block is not None and "g.abandon_click_ms = __now_ms()" in click_block)

    # on_drop_task 清理
    drop_block = _block_between(dh, "def on_drop_task(", "def on_show_dialog(")
    check("on_drop_task 清 abandon_click_ms(放弃成功)",
          drop_block is not None and "g.abandon_click_ms = 0" in drop_block)
    check("on_drop_task 清 abandon_fail_seen(服务端受理=有扣款能力)",
          drop_block is not None and "g.abandon_fail_seen = 0" in drop_block)
    check("on_drop_task 清 ghost_wait_extend",
          drop_block is not None and "g.ghost_wait_extend = 0" in drop_block)

    # on_finish_task 清理
    fin_block = _block_between(dh, "def on_finish_task(", "def on_drop_task(")
    check("on_finish_task 清 abandon_fail_seen(交付完成=链路推进)",
          fin_block is not None and "g.abandon_fail_seen = 0" in fin_block)
    check("on_finish_task 清 ghost_wait_extend",
          fin_block is not None and "g.ghost_wait_extend = 0" in fin_block)

    # tick: 被拒判定调用
    nav_pos = dh.find("__note_abandon_nodrop(robot_object, g, now_ms)")
    pre_pos = dh.find("2026-09-22 领双前置(pre_daily): 抓鬼启动前先跑")
    check("tick 每帧调用 __note_abandon_nodrop", nav_pos > 0)
    check("被拒判定调用在 pre_daily 之前(任何状态都会执行)",
          nav_pos > 0 and pre_pos > 0 and nav_pos < pre_pos)

    # WAIT_GHOST rounds>=6 闸：位置/顺序/内容
    gate_pos = dh.find("if __should_skip_broker_abandon(g):")
    abandon_true_pos = dh.find("g.abandon_task = True")
    check("WAIT_GHOST rounds>=6 分支调用降频闸", gate_pos > 0)
    check("降频闸在 g.abandon_task = True(回24) 之前(先拦)",
          gate_pos > 0 and abandon_true_pos > 0 and gate_pos < abandon_true_pos)
    gate_block = _block_between(dh, "if __should_skip_broker_abandon(g):",
                                "# 2026-09-05 修复")
    check("闸分支含 ghost_wait_extend += 1",
          gate_block is not None and "g.ghost_wait_extend = int(getattr(g, \"ghost_wait_extend\", 0)) + 1" in gate_block)
    check("闸分支 rounds 回退到 3(走既有换图/巡逻档)",
          gate_block is not None and "g.rounds = 3" in gate_block)
    check("闸分支不回 24(块内无 __goto)",
          gate_block is not None and "__goto(" not in gate_block)
    check("闸分支不发钟馗点击(npc_id 不出现)",
          gate_block is not None and "BROKER_NPC_ID" not in gate_block)
    check("闸分支置 WAIT_GHOST 并 return(不落原回落路径)",
          gate_block is not None and '__set_state(g, "WAIT_GHOST", now_ms)' in gate_block
          and "return 0" in gate_block)
    check("闸分支日志含省 5 银口径(便于生产核对)",
          gate_block is not None and "不回 24 白跑(省 5 银+往返)" in gate_block)

    # 既有保护不被破坏
    check("原 ABANDON_FAIL_LIMIT 逻辑保留(不受本改动影响)",
          "if g.abandon_tries > ABANDON_FAIL_LIMIT:" in dh)
    check("原 2026-09-05 回落逻辑保留(有 hunt 时回钟馗放弃)",
          "# 2026-09-05 修复: 等刷鬼太久" in dh and "g.abandon_task = True" in dh)
    check("无 hunt 任务时路径未改动(仍 reset 回 READY 接取)",
          "等刷鬼 %d 轮仍无鬼, 回钟馗重新接取触发重新刷鬼" in dh)

    # ============================================================ B. 行为测试
    ns = {}
    for _m in re.finditer(r"^(GHOST_(?:ABANDON_NODROP_MS|WAIT_EXTEND_MAX))\s*=\s*(\d+)", dh, re.M):
        ns[_m.group(1)] = int(_m.group(2))
    ns["__log"] = lambda ro, lvl, msg: logs.append((lvl, msg))
    logs = []
    for _name in ("__note_abandon_nodrop", "__should_skip_broker_abandon"):
        frag = _extract_func(dh, _name)
        check("提取 daily_ghost.%s" % _name, frag is not None)
        if frag:
            try:
                exec(frag, ns)
            except Exception as e:  # noqa
                check("exec daily_ghost.%s" % _name, False, str(e))
    note = ns.get("__note_abandon_nodrop")
    skip = ns.get("__should_skip_broker_abandon")
    check("常量注入: GHOST_ABANDON_NODROP_MS=60000", ns.get("GHOST_ABANDON_NODROP_MS") == 60000)
    check("常量注入: GHOST_WAIT_EXTEND_MAX=2", ns.get("GHOST_WAIT_EXTEND_MAX") == 2)

    if note is not None:
        # 未到 60s: 不记
        g = _G(abandon_click_ms=100000, abandon_fail_seen=0)
        check("被拒判定: 点过放弃但仅 30s → False(不记)",
              note(None, g, 130000) is False)
        check("被拒判定: 30s 未记 → fail_seen 仍 0", g.abandon_fail_seen == 0)
        check("被拒判定: 30s 未记 → click_ms 保留(继续等回执)",
              g.abandon_click_ms == 100000)
        # 超过 60s: 记一笔并清在途
        check("被拒判定: 61s 无 DROP → True(记被拒)",
              note(None, g, 161001) is True)
        check("被拒判定: fail_seen 累加为 1", g.abandon_fail_seen == 1)
        check("被拒判定: 记完清 abandon_click_ms(不重复记)",
              g.abandon_click_ms == 0)
        check("被拒判定: 同一时刻再次调用 → False(无在途)",
              note(None, g, 161001) is False)
        check("被拒判定: fail_seen 不变(不重复计数)", g.abandon_fail_seen == 1)
        # 累计
        g2 = _G(abandon_click_ms=200000, abandon_fail_seen=2)
        check("被拒判定: 已有 2 次再记 → 3", note(None, g2, 261000) is True and g2.abandon_fail_seen == 3)
        # 从未点过放弃: 不记(正常超时不受影响)
        g3 = _G(abandon_click_ms=0, abandon_fail_seen=0)
        check("被拒判定: 从未点过放弃 → False(超时误伤无关)",
              note(None, g3, 999999) is False and g3.abandon_fail_seen == 0)
        check("被拒判定: 缺字段(旧状态对象) → False(向后兼容)",
              note(None, type("X", (), {})(), 999999) is False)
        check("被拒判定: 日志含口径说明(便于生产核对)",
              any("放弃需扣 500 银两" in m for _l, m in logs))

    if skip is not None:
        # 没被拒过: 不拦(首次照旧回 24 试)
        check("降频闸: fail_seen=0, extend=0 → False(首次不拦)",
              skip(_G(abandon_fail_seen=0, ghost_wait_extend=0)) is False)
        # 被拒过且未达上限: 拦
        check("降频闸: fail_seen=1, extend=0 → True(拦)",
              skip(_G(abandon_fail_seen=1, ghost_wait_extend=0)) is True)
        check("降频闸: fail_seen=1, extend=1 → True(拦)",
              skip(_G(abandon_fail_seen=1, ghost_wait_extend=1)) is True)
        check("降频闸: fail_seen=2, extend=0 → True(拦)",
              skip(_G(abandon_fail_seen=2, ghost_wait_extend=0)) is True)
        # 达上限: 回落兜底
        check("降频闸: fail_seen=1, extend=2(=上限) → False(回落兜底)",
              skip(_G(abandon_fail_seen=1, ghost_wait_extend=2)) is False)
        check("降频闸: fail_seen=3, extend=5 → False(超限回落)",
              skip(_G(abandon_fail_seen=3, ghost_wait_extend=5)) is False)
        # 缺字段: 不拦(向后兼容, 不会因字段缺失而停掉回落路径)
        check("降频闸: 缺字段(旧状态对象) → False(向后兼容)",
              skip(type("X", (), {})()) is False)
        # 与 on_drop_task/on_finish_task 清零的联动语义(模拟)
        gd = _G(abandon_fail_seen=3, ghost_wait_extend=2)
        gd.abandon_click_ms = 0
        gd.abandon_fail_seen = 0
        gd.ghost_wait_extend = 0
        check("联动: 放弃成功/交付完成清零后 → 闸放行(False)",
              skip(gd) is False)

    # ============================================================ C. 一致性
    check("三字段全部出现 ≥3 处(定义/清理/使用链路完整)",
          all(dh.count(k) >= 3 for k in
              ("abandon_click_ms", "abandon_fail_seen", "ghost_wait_extend")))
    check("被拒判定只认'点过放弃'的标记(不误伤普通超时)",
          "if not _acm or (now_ms - _acm) <= GHOST_ABANDON_NODROP_MS:" in dh)
    check("降频闸不改事件/心跳协议面(无新 __emit 调用)",
          gate_block is not None and "__emit" not in gate_block)

    # ============================================================ 汇总
    passed = sum(1 for _n, ok, _d in results if ok)
    failed = [(n, d) for n, ok, d in results if not ok]
    for n, ok, d in results:
        if not ok:
            print("[FAIL] %s %s" % (n, ("| " + str(d)) if d else ""))
    print("-" * 68)
    print("pay_reduce_selftest: %d/%d PASS" % (passed, len(results)))
    if failed:
        print("FAILED %d 项:" % len(failed))
        for n, _d in failed:
            print("  - %s" % n)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
