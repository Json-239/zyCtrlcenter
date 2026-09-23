# -*- coding: utf-8 -*-
"""技能主动攻击 自检（2026-09-23）。

用户口径：
  · 抓鬼/野外战斗用技能（剧情/其它保持普攻 —— 8-26 教训）；
  · 仙族：纯伤害技（status==0 且 hurt>0）直接使用；人族/魔族：可用技能随机；
  · 等级/法力不满足 → 回退普攻。

链路：S2C_ROLE_SHARE_SKILL(90299 全量)/S2C_UPDATE_SHARE_SKILL(90334 增量)
  → msghandle.share_skill_handle → robot_operator.update_share_skill → robot.m_skills
  → daily_ghost 打 m_fight_kind → fight_tester.__pick_fight_skill → skill_attack.pick_skill
  → C2S_FIGHTCMD [1, 4, target, skill_index, proficiency]

用法：python tools/skill_attack_selftest.py [script_dir]
"""
import io
import marshal
import os
import sys
import types

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_DIR = os.path.normpath(os.path.join(
    HERE, "..", "deploy", "zones", "prod-240-2300", "script"))
script_dir = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_DIR
sys.path.insert(0, script_dir)

p3 = open(os.path.join(script_dir, "protocol3.py"), encoding="utf-8").read()
mh = open(os.path.join(script_dir, "msghandle.py"), encoding="utf-8").read()
ro = open(os.path.join(script_dir, "robot_operator.py"), encoding="utf-8").read()
dg = open(os.path.join(script_dir, "daily_ghost.py"), encoding="utf-8").read()
ft = open(os.path.join(script_dir, "fight_tester.py"), encoding="utf-8").read()
meta_src = open(os.path.join(script_dir, "skill_meta.py"), encoding="utf-8").read()

fails = 0
total = 0


def check(name, ok, detail=""):
    global fails, total
    total += 1
    fails += 0 if ok else 1
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  " + detail) if detail else ""))


# ================================================================ 静态
check("S1 skill_meta.py 生成物（60 技能 + race_of/is_damage）",
      "SKILL_META = {" in meta_src and "def race_of(" in meta_src and "def is_damage(" in meta_src)
check("S2 skill_attack.py 存在（pick_skill/describe）",
      os.path.exists(os.path.join(script_dir, "skill_attack.py")))
check("S3 protocol3 注册 90299/90334（常量+FORMAT_MS+g_handle_map）",
      "S2C_ROLE_SHARE_SKILL = protocol2.s2c_key.S2C_ROLE_SHARE_SKILL" in p3
      and "FORMAT_MS[S2C_ROLE_SHARE_SKILL] = ['b']" in p3
      and "S2C_ROLE_SHARE_SKILL : msghandle.share_skill_handle" in p3)
check("S4 msghandle.share_skill_handle 存在", "def share_skill_handle(" in mh)
check("S5 robot_operator.update_share_skill 解析（10701/10704）",
      "def update_share_skill(" in ro and "10701" in ro and "10704" in ro)
check("S6 daily_ghost 打 m_fight_kind 标记",
      'robot_object.m_fight_kind = "ghost" if g.fight_was_ghost else "wild"' in dg)
# 2026-09-23 第 5 参数由 proficiency 改为 reinc_class(0)（服务端实证：第5位是转世世系）
check("S7 fight_tester 技能发货（[1,4,target,skill,0]）+ 守护普攻 + 普攻回退",
      "[1, 4, target_index, _sidx, 0]" in ft and "[0, 1, target_index, 0, 0]" in ft
      and "def __pick_fight_skill(" in ft)
check("S8 fight_tester 战斗结束清缓存",
      "robot_object.m_fight_skill = None" in ft and 'robot_object.m_fight_kind = ""' in ft)

# ================================================================ 动态
import skill_meta  # noqa: E402
import skill_attack  # noqa: E402


def mk_robot(skills, level=50, mp=100000):
    return types.SimpleNamespace(m_skills=dict(skills), m_level=level, m_cur_mp=mp)


# D1 仙族（91 沧海龙卷 hurt=40）：选纯伤害技
r = mk_robot({91: 1, 71: 2, 81: 3}, level=10)
idx, prof = skill_attack.pick_skill(r)
check("D1 仙族: 选纯伤害技（91/71/81 之一）",
      idx in (91, 71, 81) and prof in (1, 2, 3) and skill_meta.is_damage(idx),
      "pick=%s/%s" % (idx, prof))

# D2 人族（11 迷魂香 status=11）：无特殊技能时随机门派技
r = mk_robot({11: 5, 12: 6}, level=1, mp=999)
idx, prof = skill_attack.pick_skill(r)
check("D2 人族: 无特殊技能 → 随机门派技（11/12 之内）且非伤害技",
      idx in (11, 12) and not skill_meta.is_damage(idx), "pick=%s" % idx)

# D2b 人族+特殊技能（901 初露锋芒/902 一石二鸟）→ **优先特殊技能随机**（用户口径）
r = mk_robot({11: 5, 12: 6, 901: 1, 902: 1}, level=50)
picks = set()
for _ in range(30):
    idx, _ = skill_attack.pick_skill(r)
    picks.add(idx)
check("D2b 人族: 优先特殊技能（901/902，30 次采样全在其中）",
      picks and picks <= {901, 902}, "picks=%s" % sorted(picks))

# D2c 仙族+门派伤害技+特殊技能 → 优先门派伤害技（81 电闪雷鸣 hurt=40）
r = mk_robot({81: 1, 901: 1, 902: 1}, level=50)
idx, _ = skill_attack.pick_skill(r)
check("D2c 仙族: 优先门派纯伤害技（81），不是特殊技能", idx == 81, "pick=%s" % idx)

# D3 等级过滤：13 需要 30 级 → 10 级不可用
r = mk_robot({13: 1}, level=10)
idx, _ = skill_attack.pick_skill(r)
check("D3 等级不够: (0,0) 回退普攻", idx == 0)

# D4 法力过滤：12 需 160 法力 → 50 不够
r = mk_robot({12: 1}, level=5, mp=50)
idx, _ = skill_attack.pick_skill(r)
check("D4 法力不够: (0,0) 回退普攻", idx == 0)
r = mk_robot({12: 1}, level=5, mp=160)
idx, _ = skill_attack.pick_skill(r)
check("D4b 法力刚好够: 选中 12", idx == 12)

# D5 无技能 → (0,0)
r = mk_robot({}, level=50)
idx, _ = skill_attack.pick_skill(r)
check("D5 无已学技能: (0,0)", idx == 0)

# D6 混合族（理论不会出现）: 不崩、返回可用项
r = mk_robot({11: 1, 91: 1}, level=50)
idx, _ = skill_attack.pick_skill(r)
check("D6 混合技能表: 不崩且返回可用项", idx in (11, 91), "pick=%s" % idx)

# D7 update_share_skill 解析全量 marshal blob
ns = {"marshal": marshal}
import re as _re
m = _re.search(r"(?ms)^def update_share_skill\(.*?(?=^def )", ro)
assert m, "未提取 update_share_skill"
exec(compile(m.group(0), "<update_share_skill>", "exec"), ns)  # noqa: S102
blob = marshal.dumps([{10701: 11, 10704: 3}, {10701: 91, 10704: 7}])
r = mk_robot({})
ns["update_share_skill"](r, [blob])
check("D7 全量 blob 解析 → m_skills {11:3, 91:7}",
      r.m_skills == {11: 3, 91: 7}, "skills=%s" % r.m_skills)

# D8 增量（dict 形态直传）与边界
r2 = mk_robot({11: 1})
ns["update_share_skill"](r2, [{10701: 12, 10704: 9}])
check("D8 增量 dict 形态解析", r2.m_skills.get(12) == 9, "skills=%s" % r2.m_skills)
ns["update_share_skill"](r2, [])
ns["update_share_skill"](r2, ["garbage"])
check("D8b 空/垃圾输入不崩", r2.m_skills.get(12) == 9)

# ================================================================ 汇总
print("\n自检目标: %s" % script_dir)
print("结果：%d 项，失败 %d 项" % (total, fails))
sys.exit(1 if fails else 0)
