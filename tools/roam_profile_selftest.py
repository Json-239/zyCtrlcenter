# -*- coding: utf-8 -*-
"""游荡档位(profile) + 任务形态(随机图/白名单/限时) 自检 —— 2026-09-22

背景:
  现场问题: 半月岛的号"站着不动"→ 不走路就踩不到暗雷 → 孵化进度为零。
  根因: 游荡默认档是"账号级随机"的真人挂机节奏(停顿概率 0.25~0.8、单次停顿
        25~90s 起、最长可达 ~4.5 分钟)。
  修法: 引入可扩展的**游荡档位**(ROAM_PROFILES):
        - default: 既有账号级随机(不改);
        - dense:   密集游荡(几乎不停/大步) —— 孵化(mount_egg)固定用 dense;
        - gather:  采集占位档(采集动作模块未合入: 明确回退 default + 记日志, 不静默);
        后续"野外挂机/挖宝/挖矿/打猎"等用途注册新档位即可, 不用改游荡主循环。

  2026-09-22 扩展(四用途: 野外/选图/采集占位/固定孵化图): 档位之外再加"任务形态"层:
        mapid = 具体图号 或 "random"(从链数据里有网格的图随机挑, 每号不同)
        maps  = 可选白名单(孵化图 [6,17,34,40])
        minutes = 限时(分钟; 到点自动停)
        解析/校验集中在纯函数 resolve_roam_target 里(非法参数明确拒绝, 不静默改图)。

本自检断言:
  ① ROAM_PROFILES 含 default/dense/gather; dense 的停顿参数确实"密"; gather 是占位档;
  ② apply_roam_profile: default 不覆盖任何参数(账号级随机保留)、dense 覆盖生效、
     未知档位回退 default 且记日志、**gather 占位档明确回退 + 记日志**(不静默按 default 跑);
  ③ maps 白名单参数解析(norm_map_list): 数组/字符串/单个数字/去重/非法项;
  ④ mode=random 选图逻辑(pick_random_map/resolve_roam_target, 纯函数可注入随机):
     抽签避开当前图、白名单里没有可用图/指定图不在白名单/指定图没有网格 → 明确拒绝;
  ⑤ 源码形状: 随机图/白名单/限时确实接进了 random_walk; mount_egg 启动游荡固定传 dense。

用法:
  python tools/roam_profile_selftest.py <script 目录 或 random_walk.py 路径>
"""
import os
import random
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


class _SeqRng(object):
    """自检用随机源: randrange 固定返回 0(取候选第一个), 让"抽签"可断言。"""

    def randrange(self, n):
        return 0


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
    check('random_walk 从命令取档位(mode)',
          'apply_roam_profile(w, _prof_req' in src
          or 'apply_roam_profile(w, cmd.get("mode", "")' in src
          or "apply_roam_profile(w, cmd.get('mode', '')" in src)
    check("mount_egg 启动游荡固定传 dense",
          '"mode": "dense"' in me_src or "'mode': 'dense'" in me_src,
          "mount_egg 未固定 dense(孵化会站着不动踩不到暗雷)")
    check("random_walk 用纯函数解析目标图(随机图/白名单)",
          "resolve_roam_target(" in src and "def resolve_roam_target(" in src,
          "没有 resolve_roam_target: 任务形态没参数化")
    check("random_walk 支持限时(minutes → deadline_ms)",
          "deadline_ms" in src and '__apply_minutes(w, cmd)' in src,
          "minutes 没落地(限时游荡不会自动停)")
    check("random_walk 的图校验用 getattr 兜底(热更兼容)",
          'getattr(w, "deadline_ms"' in src and 'getattr(w, "map_req"' in src,
          "新字段没有 getattr 兜底, 热更后旧实例会 AttributeError")

    # ---- ② 提取纯函数并执行(按依赖顺序) ----
    ns = {}
    frags = [
        ("ROAM_PROFILES", _extract_dict(src, "ROAM_PROFILES = {")),
        ("apply_roam_profile", _extract_func(src, "apply_roam_profile")),
        ("is_random_map", _extract_func(src, "is_random_map")),
        ("norm_map_list", _extract_func(src, "norm_map_list")),
        ("pick_random_map", _extract_func(src, "pick_random_map")),
        ("resolve_roam_target", _extract_func(src, "resolve_roam_target")),
    ]
    for _name, _frag in frags:
        check("提取 %s" % _name, _frag is not None)
        if not _frag:
            continue
        try:
            exec(_frag, ns)
        except Exception as e:  # noqa
            check("exec %s" % _name, False, str(e))

    fn = ns.get("apply_roam_profile")
    profs = ns.get("ROAM_PROFILES") or {}
    norm = ns.get("norm_map_list")
    pick = ns.get("pick_random_map")
    resolve = ns.get("resolve_roam_target")
    is_rand = ns.get("is_random_map")

    # ---- ③ 档位行为 ----
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

        # gather 占位档: 明确回退 + 记日志 + 不携带节奏参数(不会静默按采集跑)
        w5 = _W()
        w5.stand_ratio, w5.step_min = 0.7, 120
        logs5 = []
        eff5 = fn(w5, "gather", log=lambda m: logs5.append(m))
        check("gather 占位档: 注册在 ROAM_PROFILES", "gather" in profs)
        check("gather 占位档: 带 desc/placeholder/fallback",
              isinstance((profs.get("gather") or {}).get("desc"), str)
              and bool((profs.get("gather") or {}).get("placeholder"))
              and (profs.get("gather") or {}).get("fallback") == "default")
        check("gather 占位档: 明确回退 default", eff5 == "default" and w5.stand_ratio == 0.7
              and getattr(w5, "profile", "") == "default" and getattr(w5, "profile_req", "") == "gather")
        check("gather 占位档: 记日志说明'未实现'(不静默)",
              len(logs5) == 1 and "gather" in logs5[0] and ("未实现" in logs5[0] or "占位" in logs5[0]),
              logs5)

    # ---- ④ maps 白名单解析 ----
    if norm is not None:
        check("白名单: [6,'17',34] → [6,17,34]", norm([6, "17", 34]) == [6, 17, 34])
        check('白名单: "6,17,34,40" → [6,17,34,40]', norm("6,17,34,40") == [6, 17, 34, 40])
        check("白名单: 重复项去重", norm([6, 6, "17", 17]) == [6, 17])
        check("白名单: 单个数字 6 → [6]", norm(6) == [6])
        check("白名单: 非法项忽略('abc' → [])", norm("abc") == [])
        check("白名单: None → []", norm(None) == [])
        check("白名单: 0/负数/非数字全非法 → []", norm([0, -3, "x"]) == [])
        check("白名单: 混入非法项只留合法项", norm([6, "x", 17]) == [6, 17])

    # ---- ⑤ mode=random 选图逻辑(纯函数, 可注入随机) ----
    if is_rand is not None:
        check("随机词识别: ' RANDOM '", is_rand(" RANDOM ") is True)
        check("随机词识别: 数字/None 不是", is_rand(6) is False and is_rand(None) is False)

    if pick is not None:
        check("抽签: 注入 rng 取候选第一个", pick([6, 10, 17], rng=_SeqRng()) == 6)
        check("抽签: 避开当前图", pick([6, 10, 17], exclude=6, rng=_SeqRng()) == 10)
        check("抽签: 候选只剩它时仍返回它", pick([6], exclude=6, rng=_SeqRng()) == 6)
        check("抽签: 无候选 → None", pick([], rng=_SeqRng()) is None)
        check("抽签: 真 rng 可复现(两次同 seed 结果一致)",
              pick([6, 10, 17], rng=random.Random(1)) == pick([6, 10, 17], rng=random.Random(1)))
        check("抽签: 结果一定在候选内", pick([6, 10, 17], rng=random.Random(7)) in (6, 10, 17))

    if resolve is not None:
        # 随机图 + 白名单: 候选 = 白名单 ∩ 有网格图, 避开当前图
        t1 = resolve("random", [6, 17, 34, 40], [6, 10, 17], current_map=6, rng=_SeqRng())
        check("随机图+白名单: 抽到 17(白名单∩网格, 避开当前图 6)", t1 == (17, [6, 17, 34, 40], ""), t1)
        # 随机图无白名单: 候选 = 全部有网格图
        t2 = resolve("random", None, [6, 10], current_map=6, rng=_SeqRng())
        check("随机图无白名单: 抽到 10(避开当前图 6)", t2 == (10, [], ""), t2)
        # 白名单里没有可用图 → 明确拒绝
        t3 = resolve("random", [34], [6, 10], current_map=6, rng=_SeqRng())
        check("随机图: 白名单无可用图 → 拒绝", t3[0] == 0 and "网格" in t3[2], t3)
        # 没有网格数据 → 明确拒绝(不盲抽)
        t4 = resolve("random", None, [], current_map=6, rng=_SeqRng())
        check("随机图: 没有可用网格 → 拒绝", t4[0] == 0 and t4[2], t4)
        # 指定图: 合法
        t5 = resolve(6, [6, 17], [6, 10])
        check("指定图: 合法(图 6 ∈ 白名单且 ∈ 网格)", t5 == (6, [6, 17], ""), t5)
        # 指定图不在白名单 → 拒绝(不静默换图)
        t6 = resolve(10, [6, 17], [6, 10])
        check("指定图: 不在白名单 → 拒绝", t6[0] == 0 and "白名单" in t6[2], t6)
        # 指定图没有网格 → 拒绝
        t7 = resolve(17, [6, 17], [6, 10])
        check("指定图: 没有网格 → 拒绝", t7[0] == 0 and "网格" in t7[2], t7)
        # 缺 mapid → 拒绝
        t8 = resolve(None, None, [6, 10])
        check("缺 mapid: 明确拒绝", t8[0] == 0 and "mapid" in t8[2], t8)
        # 非法 mapid → 拒绝
        t9 = resolve("abc", None, [6, 10])
        check("非法 mapid: 明确拒绝", t9[0] == 0 and "非法" in t9[2], t9)
        # 网格未知(None)时跳过网格校验(向后兼容: 旧命令不带链也能跑)
        t10 = resolve(6, "6,17", None)
        check("网格未知: 跳过网格校验但仍解析白名单", t10 == (6, [6, 17], ""), t10)
        # 白名单给了但解析不出 → 拒绝
        t11 = resolve("random", "abc", [6, 10])
        check("白名单全非法 → 拒绝", t11[0] == 0 and "白名单" in t11[2], t11)

    nfail = 0
    for name, ok, detail in results:
        if ok:
            print("[PASS] %s" % name)
        else:
            nfail += 1
            print("[FAIL] %s %s" % (name, ("(" + str(detail) + ")") if detail else ""))
    print("=== 结果: %s (共 %d 项断言, 失败 %d 项) ===" % ("PASS" if nfail == 0 else "FAIL", len(results), nfail))
    return 0 if nfail == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
