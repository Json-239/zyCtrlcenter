# -*- coding: utf-8 -*-
"""导出技能静态表 → 机器人脚本 skill_meta.py（2026-09-23 技能主动攻击用）。

数据源：服务端 `config/skill/{human,demon,immortal}_skill.xml`（种族分文件）
        + `config/school.xml`（门派归属，可选校验）。

导出字段（每条技能）：
  name 技能名 / race 种族(human|demon|immortal) / school 门派 / level 学习等级门槛
  hurt 基础伤害(base_hurt_num) / status 状态号(status_index; 0=无状态)
  mp 法力消耗(base_cost_mp) / targets 目标数(base_target_num)
  magic 法术系(magic_type_name) / plan 方案(skill_plan)

分类口径（与用户口径一致）：
  · 纯伤害技: status == 0 且 hurt > 0   → 仙族法术（"直接使用"）
  · 状态/辅助技: status != 0            → 人族(昏睡/封印/毒)/魔族(附攻/附防)（"随机使用"）

用法：python tools/export_skill_meta.py [server_root] [script_dir]
"""
import io
import os
import re
import sys

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.normpath(os.path.join(HERE, ".."))
server_root = sys.argv[1] if len(sys.argv) > 1 else "F:/ZyBin/xm/2d-xiyou-server"
script_dir = sys.argv[2] if len(sys.argv) > 2 else os.path.join(
    ROOT, "deploy", "zones", "prod-240-2300", "script")

RACE_FILES = (("human", "human_skill.xml"), ("demon", "demon_skill.xml"),
              ("immortal", "immortal_skill.xml"))
# 2026-09-23 特殊技能（extern_skill.xml）：每号都有的"新手"类伤害技 ——
#   901 初露锋芒（单体物理法术）/ 902 一石二鸟（远程打 2 个）。用户口径：
#   人/魔的门派技是控制/辅助（昏睡/封印/护甲），"打伤害"要用这两个。
EXTERN_FILE = "extern_skill.xml"
SKILL_ROOT = os.path.join(server_root, "config", "skill")


def field(body, key):
    m = re.search(r"<%s[^>]*>([^<]*)</%s>" % (key, key), body)
    return m.group(1).strip() if m else ""


def main():
    rows = {}
    for race, fn in RACE_FILES:
        path = os.path.join(SKILL_ROOT, fn)
        src = open(path, encoding="utf-8", errors="replace").read()
        entries = re.findall(
            r'<skill_entry skill_index="(\d+)" skill_name="([^"]*)"[^>]*>(.*?)</skill_entry>',
            src, re.S)
        for idx, name, body in entries:
            def _num(k, default=0):
                v = field(body, k)
                try:
                    return int(float(v)) if v else default
                except Exception:
                    return default
            rows[int(idx)] = {
                "name": name,
                "race": race,
                "school": field(body, "skill_school"),
                "level": _num("limit_level"),
                "hurt": _num("base_hurt_num"),
                "status": _num("status_index"),
                "mp": _num("base_cost_mp"),
                "targets": _num("base_target_num", 1),
                "magic": field(body, "magic_type_name"),
                "plan": field(body, "skill_plan"),
            }
    # 特殊技能（新手类 901/902 等；magic_type_name == "新手" 过滤）
    try:
        src = open(os.path.join(SKILL_ROOT, EXTERN_FILE),
                   encoding="utf-8", errors="replace").read()
        entries = re.findall(
            r'<skill_entry skill_index="(\d+)" skill_name="([^"]*)"[^>]*>(.*?)</skill_entry>',
            src, re.S)
        for idx, name, body in entries:
            if field(body, "magic_type_name") != "新手":
                continue

            def _num_ex(k, default=0):
                v = field(body, k)
                try:
                    return int(float(v)) if v else default
                except Exception:
                    return default

            rows[int(idx)] = {
                "name": name,
                "race": "common",
                "school": "新手",
                "level": _num_ex("role_lower_level"),
                "hurt": 9,  # 伤害写在 effect_describe（"使对方N目标受到物理伤害"）→ 标记为伤害型
                "status": 0,
                "mp": _num_ex("base_cost_mp"),
                "targets": _num_ex("base_target_num", 1),
                "magic": "新手",
                "plan": field(body, "skill_plan"),
                "special": True,
            }
    except Exception as e:
        print("★extern 特殊技能解析失败: %s" % e)

    if not rows:
        print("★未解析到任何技能，检查路径: %s" % SKILL_ROOT)
        return 1

    out = os.path.join(script_dir, "skill_meta.py")
    lines = [
        "# -*- coding: utf-8 -*-",
        '"""技能静态表（2026-09-23 生成；勿手改 —— 重新生成：tools/export_skill_meta.py）。',
        "",
        "来源: 服务端 config/skill/{human,demon,immortal}_skill.xml。",
        "字段: name/race/school/level/hurt/status/mp/targets/magic/plan。",
        "分类: status==0 且 hurt>0 = 纯伤害技（仙族法术）; status!=0 = 状态/辅助技（人/魔）。",
        '"""',
        "",
        "SKILL_META = {",
    ]
    for idx in sorted(rows):
        r = rows[idx]
        _sp = ', "special": True' if r.get("special") else ''
        lines.append('    %d: {"name": "%s", "race": "%s", "school": "%s", "level": %d, '
                     '"hurt": %s, "status": %d, "mp": %d, "targets": %d, "magic": "%s", "plan": "%s"%s},' % (
                         idx, r["name"], r["race"], r["school"], r["level"],
                         str(r["hurt"]), r["status"], r["mp"], r["targets"], r["magic"], r["plan"], _sp))
    lines.append("}")
    lines.append("")
    lines.append("")
    lines.append("def race_of(skill_index):")
    lines.append('\t"""技能所属种族（无此技能 → \"\"）。"""')
    lines.append("\tm = SKILL_META.get(int(skill_index or 0))")
    lines.append('\treturn m["race"] if m else ""')
    lines.append("")
    lines.append("")
    lines.append("def is_damage(skill_index):")
    lines.append('\t"""是否纯伤害技（status==0 且 hurt>0；仙族法术）。"""')
    lines.append("\tm = SKILL_META.get(int(skill_index or 0))")
    lines.append("\treturn bool(m) and m[\"status\"] == 0 and float(m[\"hurt\"]) > 0")
    lines.append("")
    open(out, "w", encoding="utf-8").write("\n".join(lines))

    # 统计摘要
    from collections import Counter
    by_race = Counter(v["race"] for v in rows.values())
    dmg = sum(1 for v in rows.values() if v["status"] == 0 and float(v["hurt"]) > 0)
    print("已导出 %d 个技能 → %s" % (len(rows), out))
    print("  种族分布: %s | 纯伤害技: %d | 状态/辅助技: %d" % (
        dict(by_race), dmg, len(rows) - dmg))
    return 0


if __name__ == "__main__":
    sys.exit(main())
