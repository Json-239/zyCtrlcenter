# -*- coding: utf-8 -*-
"""游荡"定向派发被误判全排除"修复自检 —— 2026-09-23

背景（现场取证, agent-3）:
  中控游荡池(keeper)每 10s 按图补位，派发形态是 **(mapid=具体图, 不带 maps 白名单)**。
  机器人端"排除图"闸的旧判定 `if not _raw_maps and not is_random_map(_raw_map): 拒绝`，
  把"**未给白名单**"当成"白名单被剔空" → keeper 的定向派发 100% 被当场拒收：
  实测 60 派/分、54 拒/分，同号同图每 10s 重复，当日累计 3342 次（昨日 82 次），
  游荡名额实际全靠机器人 auto_roam 撑。

修复: random_walk.__maps_gate_reject(raw_map, raw_maps, maps_given) 纯函数 ——
  仅当调用方**确实给了白名单**(maps_given=True) 且过滤后为空 且目标是具体图 → 拒绝；
  未给白名单(定向派发) → 放行（具体图是否属排除集由单图闸单独判）。

本自检（不改文件）:
  1) 静态检查：调用点已换成 __maps_gate_reject(...)，且 _maps_given 取自 cmd["maps"] 是否给出；
     旧的 `not _raw_maps and not is_random_map(_raw_map)` 直判不应再出现在 dispatch_cmd；
  2) 行为矩阵（抽取函数本体执行）：
     - 定向派发(mapid=1, maps 未给) → 放行（本次修复的核心）；
     - 显式空白名单([], 给了) → 拒绝；
     - 白名单被排除集剔空([24] 过滤后为 []) → 拒绝；
     - 白名单非空([6,17]) → 放行；
     - 随机图(mapid="random")无论给不给白名单 → 不在此闸拒绝。

用法:
  python tools/roam_dispatch_gate_selftest.py [script目录]
"""
import os
import sys

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


def main():
    script_dir = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_SCRIPT
    path = os.path.join(script_dir, "random_walk.py")
    src = open(path, encoding="utf-8").read()
    print("脚本: %s" % path)

    print("== 1) 静态检查（调用点与取值来源）")
    ok("__maps_gate_reject 已定义", "def __maps_gate_reject(" in src)
    ok("调用点已换成纯函数", "__maps_gate_reject(_raw_map, _raw_maps, _maps_given)" in src)
    ok("_maps_given 取自 cmd[\"maps\"] 是否给出",
       "_maps_given = _raw_maps is not None" in src)
    # 旧直判不应再出现在 dispatch_cmd（函数体在新 helper 里已改为 maps_given 版本）
    ok("旧直判已移除", "if not _raw_maps and not is_random_map(_raw_map):" not in src)

    print("== 2) 行为矩阵（抽取函数本体执行）")
    gate_fn = _extract_func(src, "__maps_gate_reject")
    rnd_fn = _extract_func(src, "is_random_map")
    if not gate_fn or not rnd_fn:
        ok("抽取 __maps_gate_reject / is_random_map", False)
        print("\n结果: PASS=%d FAIL=%d" % (len(PASS), len(FAIL)))
        return 1
    ns = {}
    exec(compile(rnd_fn, path, "exec"), ns)
    exec(compile(gate_fn, path, "exec"), ns)
    gate = ns["__maps_gate_reject"]

    cases = [
        # (raw_map, raw_maps, maps_given, 期望拒绝?, 说明)
        (1, None, False, False, "定向派发(keeper: 具体图, 未给白名单) → 必须放行"),
        (654, None, False, False, "定向派发到具体图(先由单图闸判排除) → 此闸放行"),
        (1, [], True, True, "显式给了空白名单 → 拒绝"),
        (1, [], True, True, "白名单被排除集剔空(调用前已过滤, 此处收到的就是 []) → 拒绝"),
        (1, [6, 17], True, False, "白名单非空 → 放行"),
        ("random", None, False, False, "随机图未给白名单 → 此闸不拒"),
        ("random", [6, 17], True, False, "随机图给了白名单 → 此闸不拒"),
    ]
    for raw_map, raw_maps, given, want_reject, desc in cases:
        got = bool(gate(raw_map, raw_maps, given))
        ok(desc, got == want_reject, "gate=%s want=%s" % (got, want_reject))

    print("\n结果: PASS=%d FAIL=%d" % (len(PASS), len(FAIL)))
    if FAIL:
        print("失败项:")
        for f in FAIL:
            print("  - %s" % f)
    return 1 if FAIL else 0


if __name__ == "__main__":
    sys.exit(main())
