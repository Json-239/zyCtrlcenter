# -*- coding: utf-8 -*-
"""跨图"乐观过图"回滚 + 传送选项优先免费 自检（2026-09-22 新手链卡死根因修复）。

现场（robot0003001，卡 30+ 次 ≈75 分钟 STUCK_NAV）：
  号实际在图 5，点 NPC 13003 想回图 11 —— 选项"请送我去长安东市集广场（3银"需储备金，
  服务端回通知 1116（储备金不足）→ **传送未执行**；但 quest_engine 的"点中目的地即乐观
  改图"把本地 m_mapid 改成了 11 → 之后以"图 11"身份发 destination 83（11→612），
  服务端认为它还在图 5 → start_index 不匹配 → 全拒 → 重走跳转点重试到超限。

修复（本脚本守护的 4 处）：
  1. robot_operator.handle_change_map：记录 m_srv_mapid（服务端确认图）+ 清 m_mapid_optimistic
  2. quest_engine 乐观改图处：置 m_mapid_optimistic = 目标图
  3. quest_engine 跳转被拒分支：若在乐观态 → 回滚 m_mapid = m_srv_mapid 并清标记
  4. quest_engine 目的地匹配：优先免费选项（_is_paid_jump_option），付费的只作后备

用法：python cross_map_rollback_selftest.py [script_dir]
"""
import hashlib
import os
import re
import sys

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_DIR = os.path.normpath(os.path.join(
    HERE, "..", "deploy", "zones", "prod-240-2300", "script"))
script_dir = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_DIR
qe_path = os.path.join(script_dir, "quest_engine.py")
op_path = os.path.join(script_dir, "robot_operator.py")
qe = open(qe_path, encoding="utf-8").read()
op = open(op_path, encoding="utf-8").read()

fails = 0


def check(name, ok, detail=""):
    global fails
    fails += 0 if ok else 1
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  " + detail) if detail else ""))


# ================================================================ 静态
# S1 robot_operator：记录服务端确认图 + 清乐观标记
check("S1 handle_change_map 记录 m_srv_mapid 并清 m_mapid_optimistic",
      "robot_object.m_srv_mapid = int(robot_object.m_mapid or 0)" in op
      and "robot_object.m_mapid_optimistic = 0" in op)

# S2 乐观改图处打标记
check("S2 乐观改图处置 m_mapid_optimistic（赋值语句）",
      "robot_object.m_mapid_optimistic = int(_done" in qe)

# S3 被拒分支回滚
i_rollback = qe.find('_opt = int(getattr(robot_object, "m_mapid_optimistic", 0) or 0)')
i_retry = qe.find('hop["retry"] = hop.get("retry", 0) + 1')
check("S3 跳转被拒分支回滚在 retry 递增之前",
      0 <= i_rollback < i_retry,
      "rollback@%s retry@%s" % (i_rollback, i_retry))
check("S3b 回滚动作完整（赋值回 m_srv_mapid + 清标记）",
      "robot_object.m_mapid = _srv" in qe and "robot_object.m_mapid_optimistic = 0" in qe)

# S4 优先免费
check("S4 价格识别 helper（正则 （数字银/两/金））",
      "_PAID_JUMP_RE = re.compile" in qe and "_is_paid_jump_option" in qe)
check("S4b 匹配循环优先免费 + 付费后备",
      "if _is_paid_jump_option(option_list[i][0]):" in qe
      and "_paid_best = None" in qe
      and "best = _paid_best" in qe)

# ================================================================ 动态
# D1: _is_paid_jump_option 纯函数（提取真实源码执行）
m_re = re.search(r'(?m)^_PAID_JUMP_RE = re\.compile\([^\n]*\)\n', qe)
m_fn = re.search(r'(?ms)^def _is_paid_jump_option\(text\):\n(?:.*?\n)*?\treturn .*?\n', qe)
assert m_re and m_fn, "未提取到价格识别函数"
ns = {"re": re}
exec(compile(m_re.group(0) + "\n" + m_fn.group(0), "<paid>", "exec"), ns)
paid = ns["_is_paid_jump_option"]
cases = [
    ("请送我去长安东市集广场（3银", True),
    ("请送我去长安城中擂台（4银）", True),
    ("请送我去大唐东-码头", False),
    ("请送我去烟云港", False),
    ("离开", False),
    ("请送我去金沙湾（免费）", False),   # 含"金"但无"（数字金" → 不误判
]
ok_all = all(paid(t) == want for t, want in cases)
check("D1 付费选项识别（含边界：地名带'金'不误判）",
      ok_all, ", ".join("%s→%s" % (t[:12], paid(t)) for t, _ in cases[:3]))

# D2: 回滚逻辑（提取真实源码块执行）
m_rb = re.search(
    r'(?ms)^\t\t\t_opt = int\(getattr\(robot_object, "m_mapid_optimistic", 0\) or 0\)\n.*?^\t\t\thop\["retry"\] = hop\.get\("retry", 0\) \+ 1\n',
    qe)
assert m_rb, "未提取到回滚块"
body = m_rb.group(0)
code = "def _rb(robot_object, hop, __emit):\n" + body
ns2 = {}
exec(compile(code, "<rollback>", "exec"), ns2)
RB = ns2["_rb"]


class FakeRobot(object):
    def __init__(self, m_mapid, srv, opt):
        self.m_mapid = m_mapid
        self.m_srv_mapid = srv
        self.m_mapid_optimistic = opt


emits = []
q1 = FakeRobot(m_mapid=11, srv=5, opt=11)   # 乐观态：本地 11，服务端确认 5
RB(q1, {"retry": 0}, lambda ro, ev: emits.append(ev))
check("D2 乐观态被拒 → 回滚到服务端确认图并清标记",
      q1.m_mapid == 5 and q1.m_mapid_optimistic == 0 and len(emits) == 1,
      "mapid=%s opt=%s emits=%d" % (q1.m_mapid, q1.m_mapid_optimistic, len(emits)))

q2 = FakeRobot(m_mapid=11, srv=11, opt=0)   # 非乐观态：不应动
RB(q2, {"retry": 0}, lambda ro, ev: None)
check("D2b 非乐观态被拒 → 不动 m_mapid（保持旧行为）",
      q2.m_mapid == 11 and q2.m_mapid_optimistic == 0)

q3 = FakeRobot(m_mapid=612, srv=612, opt=612)  # 乐观态但已与服务端一致 → 只清标记
RB(q3, {"retry": 0}, lambda ro, ev: None)
check("D2c 乐观但已一致 → 不回退，仅清标记",
      q3.m_mapid == 612 and q3.m_mapid_optimistic == 0)

# D3: handle_change_map 的两行关键赋值（按行提取执行）
_keys = ('robot_object.m_srv_mapid = int(robot_object.m_mapid or 0)',
         'robot_object.m_mapid_optimistic = 0')
_parts = []
for key in _keys:
    i = op.find(key)
    assert i > 0, "未找到 %s" % key
    j = op.rfind("\n", 0, i) + 1
    k = op.find("\n", i) + 1
    _parts.append(op[j:k].strip())
code3 = "def _cm(robot_object):\n\t" + _parts[0] + "\n\t" + _parts[1] + "\n"
ns3 = {}
exec(compile(code3, "<change_map>", "exec"), ns3)
CM = ns3["_cm"]
q4 = FakeRobot(m_mapid=612, srv=11, opt=11)   # 服务端推换图 → 确认图更新 + 清标记
CM(q4)
check("D3 S2C_CHANGE_MAP → 确认图落 612 且清乐观标记",
      q4.m_srv_mapid == 612 and q4.m_mapid_optimistic == 0,
      "srv=%s opt=%s" % (q4.m_srv_mapid, q4.m_mapid_optimistic))

print("\n自检目标: %s" % script_dir)
for f, s in ((qe_path, qe), (op_path, op)):
    print("  %s sha1=%s" % (os.path.basename(f),
                            hashlib.sha1(s.encode("utf-8")).hexdigest()[:12]))
total = 6 + 5
print("结果：%d 项，失败 %d 项" % (total, fails))
sys.exit(1 if fails else 0)
