# -*- coding: utf-8 -*-
"""游荡"跳转NPC对话防抢关"自检 —— 2026-09-23

背景（现场取证 robot0001108 卡地牢 654 出不来）:
  机器人被随机游荡抽进 654(地牢) 后，链数据没有出边 → 补了牢头出口边后，机器人
  正常走到牢头、点开对话（选项 ['劫狱','离开这个是非之地','出狱与劫狱说明','让我再想想吧']），
  quest_engine 也匹配到了"离开这个是非之地"并把 dialog_click 排进 quest.pending(拟人延迟 0.5s+)。
  **但 random_walk.tick 每帧先跑 __close_dialog_if_open()**：它把刚开的牢头对话当"残留对话"
  点掉「让我再想想吧」(close_flag=1)，顺带 quest.pending=None 把排好的选项点击清掉 →
  目的地永远点不到 → 跨图看门狗重试 3 次后放弃。diag.log 实测同账号 3 次
  "WALK_CLOSE_DIALOG ... opt=3"，与三次对话打开一一对应。

修复: __close_dialog_if_open 开头加让位闸 —— quest.pending 是 dialog_click（正在点选项）
  就直接 return，不抢关；其它情况（残留对话/信息型对话）保持原样关闭。

本自检（不改文件）:
  1) 从生产脚本抽取 __close_dialog_if_open 逐例执行：
     - dialog_click 在途 → 不发关闭、不清对话、不动 pending（新行为）；
     - 无 pending / pending=其它类型 → 照旧点关闭并清态（旧行为不回归）；
     - 无对话 → 空操作；
  2) 静态检查：让位分支出现在"扫关闭项"之前（防止将来挪位置失效）。

用法:
  python tools/roam_jump_dialog_selftest.py [script目录]
"""
import os
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
    print("  [%s] %s%s" % ("OK" if cond else "FAIL", name, (" —— " + detail) if detail else ""))


class _FakeProto(object):
    C2S_CLICK_DIALOG = 9999


class _FakeDiag(object):
    def __init__(self):
        self.lines = []

    def log(self, msg):
        self.lines.append(msg)


class _Robot(object):
    def __init__(self):
        self.m_account = ["qa_jump_dialog@x.com"]
        self.sent = []

    def send_message(self, msgid, data):
        self.sent.append((msgid, list(data)))


class _Quest(object):
    def __init__(self, dialog, pending):
        self.dialog_open = dialog is not None
        self.dialog = dialog
        self.pending = pending


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


# 与服务端 turnkey_ground.xml 同形的牢头对话（末项 close_flag=1）
JAIL_DIALOG = [0, 0, 13459, "牢房重地，闲人免进！", 0,
               [("劫狱", 0), ("离开这个是非之地", 0), ("出狱与劫狱说明", 0), ("让我再想想吧", 1)]]
CLICK_PENDING = {"type": "dialog_click", "at_ms": 0, "data": {"option_index": 1}}


def main():
    script_dir = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_SCRIPT
    path = os.path.join(script_dir, "random_walk.py")
    src = open(path, encoding="utf-8").read()
    print("脚本: %s" % path)

    # ---------------- 静态检查 ----------------
    print("== 1) 静态检查（让位分支在扫关闭项之前）")
    fn = _extract_func(src, "__close_dialog_if_open")
    ok("函数存在", bool(fn))
    if fn:
        _gate = fn.find('_p.get("type") == "dialog_click"')
        _scan = fn.find("for i, opt in enumerate(dlg[5])")
        ok("让位分支存在且在扫选项之前", _gate > 0 and _scan > 0 and _gate < _scan,
           "gate=%d scan=%d" % (_gate, _scan))

    # ---------------- 行为用例（抽取函数本体执行） ----------------
    print("== 2) 行为用例（抽取函数本体执行）")
    fake_proto = _FakeProto()
    fake_diag = _FakeDiag()
    _saved = {}
    try:
        # 函数体内 `import protocol3` / `diag.log(...)` —— 用桩模块注册在 sys.modules,
        # 用例执行期间保持注册（早前版在 exec 后就弹出 → import 失败被内层 except 吞,
        # 旧行为用例全部静默跳过）。
        for _name, _obj in (("protocol3", fake_proto), ("diag", fake_diag)):
            _saved[_name] = sys.modules.get(_name)
            sys.modules[_name] = _obj
        ns = {"diag": fake_diag}
        exec(compile(fn, path, "exec"), ns)
        close_fn = ns["__close_dialog_if_open"]

        def run(quest):
            r = _Robot()
            close_fn(r, quest)
            return r, quest

        # 用例 1: dialog_click 在途（跳转对话处理中）→ 不抢关
        q = _Quest(list(JAIL_DIALOG), dict(CLICK_PENDING))
        r, _ = run(q)
        ok("dialog_click 在途 → 不关对话", len(r.sent) == 0, "sent=%s" % r.sent)
        ok("dialog_click 在途 → 对话态保留", q.dialog_open and q.dialog is not None)
        ok("dialog_click 在途 → pending 不被清", isinstance(q.pending, dict)
           and q.pending.get("type") == "dialog_click")

        # 用例 2: 无 pending → 照旧点关闭并清态（旧行为）
        q = _Quest(list(JAIL_DIALOG), None)
        r, _ = run(q)
        ok("无 pending → 点关闭选项(opt=3)", r.sent == [(9999, [0, 3])], "sent=%s" % r.sent)
        ok("无 pending → 清对话态", (not q.dialog_open) and q.dialog is None)

        # 用例 3: pending 是别的动作(如重试点击) → 照旧关（游荡防卡语义不变）
        q = _Quest(list(JAIL_DIALOG), {"type": "click", "at_ms": 0, "data": {"npc_id": 13459}})
        r, _ = run(q)
        ok("pending=click → 照旧点关闭", r.sent == [(9999, [0, 3])], "sent=%s" % r.sent)

        # 用例 4: 无对话 → 空操作
        q = _Quest(None, None)
        r, _ = run(q)
        ok("无对话 → 空操作", len(r.sent) == 0 and not q.dialog_open)

        # 用例 5: 信息型对话(全关闭项) + dialog_click 在途 → 也不抢关（由 quest_engine 处理）
        info = [0, 0, 13459, "x", 0, [("知道了", 1)]]
        q = _Quest(list(info), dict(CLICK_PENDING))
        r, _ = run(q)
        ok("信息型对话 + dialog_click 在途 → 不抢关", len(r.sent) == 0)
    except Exception as e:
        ok("抽取执行 __close_dialog_if_open", False, str(e))
    finally:
        for _name, _old in _saved.items():
            if _old is None:
                sys.modules.pop(_name, None)
            else:
                sys.modules[_name] = _old

    print("\n结果: PASS=%d FAIL=%d" % (len(PASS), len(FAIL)))
    if FAIL:
        print("失败项:")
        for f in FAIL:
            print("  - %s" % f)
    return 1 if FAIL else 0


if __name__ == "__main__":
    sys.exit(main())
