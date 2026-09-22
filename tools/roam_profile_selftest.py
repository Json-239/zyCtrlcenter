# -*- coding: utf-8 -*-
"""游荡档位(profile)自检 —— 2026-09-22

背景:
  现场问题: 半月岛的号"站着不动"→ 不走路就踩不到暗雷 → 孵化进度为零。
  根因: 游荡默认档是"账号级随机"的真人挂机节奏(停顿概率 0.25~0.8、单次停顿
        25~90s 起、最长可达 ~4.5 分钟)。
  修法: 引入可扩展的**游荡档位**(ROAM_PROFILES):
        - default: 既有账号级随机(不改);
        - dense:   密集游荡(几乎不停/大步) —— 孵化(mount_egg)固定用 dense;
        后续"野外挂机/挖宝/挖矿/打猎"等用途注册新档位即可, 不用改游荡主循环。

本自检断言:
  ① ROAM_PROFILES 含 default/dense; dense 的停顿参数确实"密"(ratio 低、停顿短);
  ② apply_roam_profile: default 不覆盖任何参数(账号级随机保留)、dense 覆盖生效;
  ③ 未知档位 → 回退 default 并记日志(不抛异常);
  ④ 源码形状: random_walk 用 cmd["mode"] 取档位; mount_egg 启动游荡时固定传 dense。

用法:
  python tools/roam_profile_selftest.py <script 目录 或 random_walk.py 路径>
"""
import os
import sys


def _resolve(path):
    if os.path.isdir(path):
        return path
    return os.path.dirname(path)


def _extract_dict(src, marker):
    lines = src.splitlines()
    start = None
    for i, ln in enumerate(lines):
        if ln.startswith(marker):
            start = i
            break
    if start is None:
        return None
    out = []
    for ln in lines[start:]:
        out.append(ln)
        if ln.startswith("}"):
            break
    return "\n".join(out)


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


class _W(object):
    pass


def main():
    if len(sys.argv) < 2:
        print("用法: python tools/roam_profile_selftest.py <script 目录 或 random_walk.py 路径>")
        return 2
    script_dir = _resolve(sys.argv[1])
    rw_path = os.path.join(script_dir, "random_walk.py")
    me_path = os.path.join(script_dir, "mount_egg.py")
    if not os.path.exists(rw_path):
        print("[FAIL] 找不到 %s" % rw_path)
        return 2
    src = open(rw_path, encoding="utf-8", errors="replace").read()
    me_src = open(me_path, encoding="utf-8", errors="replace").read() if os.path.exists(me_path) else ""

    results = []

    def check(name, cond, detail=""):
        results.append((name, bool(cond), detail))

    # ---- ① 源码形状 ----
    check("random_walk 含 ROAM_PROFILES 注册表", "ROAM_PROFILES = {" in src)
    check('random_walk 从 cmd["mode"] 取档位',
          'apply_roam_profile(w, cmd.get("mode", "")' in src
          or "apply_roam_profile(w, cmd.get('mode', '')" in src)
    check("mount_egg 启动游荡固定传 dense",
          '"mode": "dense"' in me_src or "'mode': 'dense'" in me_src,
          "mount_egg 未固定 dense(孵化会站着不动踩不到暗雷)")

    # ---- ② 提取并执行 ----
    prof_src = _extract_dict(src, "ROAM_PROFILES = {")
    fn_src = _extract_func(src, "apply_roam_profile")
    check("提取 ROAM_PROFILES", prof_src is not None)
    check("提取 apply_roam_profile", fn_src is not None)
    ns = {}
    if prof_src and fn_src:
        try:
            exec(prof_src, ns)
            exec(fn_src, ns)
        except Exception as e:  # noqa
            check("exec 提取源码", False, str(e))
            ns = {}
    fn = ns.get("apply_roam_profile")
    profs = ns.get("ROAM_PROFILES") or {}

    if fn is not None:
        # default: 不覆盖账号级随机值
        w = _W()
        w.stand_ratio, w.stand_min_ms, w.stand_max_ms = 0.7, 60000, 180000
        w.step_min, w.step_max = 120, 300
        eff = fn(w, "default")
        check("default 档: 不覆盖 stand_ratio", w.stand_ratio == 0.7)
        check("default 档: 不覆盖 step_min", w.step_min == 120)
        check("default 档: profile=default", eff == "default" and getattr(w, "profile", "") == "default")

        # dense: 覆盖生效且"密"
        w2 = _W()
        w2.stand_ratio, w2.stand_min_ms, w2.stand_max_ms = 0.7, 60000, 180000
        w2.step_min, w2.step_max = 120, 300
        eff2 = fn(w2, "dense")
        check("dense 档: stand_ratio <= 0.1(几乎不停)", w2.stand_ratio <= 0.1,
              "stand_ratio=%s" % w2.stand_ratio)
        check("dense 档: stand_max_ms <= 5000(停顿短)", w2.stand_max_ms <= 5000,
              "stand_max_ms=%s" % w2.stand_max_ms)
        check("dense 档: step_min >= 200(大步赶路)", w2.step_min >= 200,
              "step_min=%s" % w2.step_min)
        check("dense 档: profile=dense", eff2 == "dense" and getattr(w2, "profile", "") == "dense")

        # 大小写/空格容错
        w3 = _W()
        w3.stand_ratio = 0.7
        eff3 = fn(w3, "  DENSE  ")
        check("档位名大小写/空格容错", eff3 == "dense" and w3.stand_ratio <= 0.1)

        # 未知档位: 回退 default + 记日志
        w4 = _W()
        w4.stand_ratio = 0.7
        logs = []
        eff4 = fn(w4, "mine", log=lambda m: logs.append(m))
        check("未知档位 → 回退 default", eff4 == "default" and w4.stand_ratio == 0.7)
        check("未知档位 → 记日志(可诊断)", len(logs) == 1 and "mine" in logs[0])

        # 预留扩展位说明
        check("dense 档带 desc(说明用途)", isinstance((profs.get("dense") or {}).get("desc"), str))

    nfail = 0
    for name, ok, detail in results:
        if ok:
            print("[PASS] %s" % name)
        else:
            nfail += 1
            print("[FAIL] %s %s" % (name, ("(%s)" % detail) if detail else ""))
    print("=== 结果: %s (共 %d 项断言, 失败 %d 项) ===" % ("PASS" if nfail == 0 else "FAIL", len(results), nfail))
    return 0 if nfail == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
