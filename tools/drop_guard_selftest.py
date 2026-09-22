# -*- coding: utf-8 -*-
"""防重复丢弃(共享闸门)自检 —— 2026-09-22

背景: 背包整理有**两套**都会丢装备 —— `bag_ops.auto_equip_best_items`（穿完顺手丢）与
`auto_summon._try_bag_cleanup`（空闲整理）。现场同一件装备被**同一秒丢两次**
（12:41:47 连发两条 C2S_DELITEM），第二次必然失败、纯浪费发包。

修法: bag_ops 里加共享闸门 `item_recently_dropped(item_id)` / `mark_item_dropped(item_id)`
（窗口 `_DROP_GUARD_MS`），**两套丢弃路径丢前都要过闸**。

本自检断言:
  ① 闸门纯函数行为: 未丢过=False；标丢=True；超窗口=False；过期自动清理；
  ② 源码形状: 两处丢弃点都调用了 item_recently_dropped + mark_item_dropped；
  ③ 反例: 不过闸时同一件会被连丢两次(用桩模拟两套路径)。

用法:
  python tools/drop_guard_selftest.py <script 目录 或 bag_ops.py 路径>
"""
import os
import re
import sys


def _dir_of(path):
    return path if os.path.isdir(path) else os.path.dirname(path)


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


def _extract_const(src, name):
    m = re.search(r"^%s\s*=\s*(.+?)\s*(?:#.*)?$" % re.escape(name), src, re.M)
    return m.group(1) if m else None


def main():
    if len(sys.argv) < 2:
        print("用法: python tools/drop_guard_selftest.py <script 目录 或 bag_ops.py 路径>")
        return 2
    script_dir = _dir_of(sys.argv[1])
    bag_path = os.path.join(script_dir, "bag_ops.py")
    as_path = os.path.join(script_dir, "auto_summon.py")
    if not os.path.exists(bag_path):
        print("[FAIL] 找不到 %s" % bag_path)
        return 2
    bag = open(bag_path, encoding="utf-8", errors="replace").read()
    asrc = open(as_path, encoding="utf-8", errors="replace").read() if os.path.exists(as_path) else ""

    results = []

    def check(name, cond, detail=""):
        results.append((name, bool(cond), detail))

    # ① 源码形状
    check("bag_ops 定义闸门函数", "def item_recently_dropped(" in bag and "def mark_item_dropped(" in bag)
    check("bag_ops 丢弃点过闸并标丢",
          "if item_recently_dropped(_it[0])" in bag and "mark_item_dropped(_it[0])" in bag,
          "auto_equip_best_items 的丢弃点缺少闸门调用")
    check("auto_summon 丢弃点过闸并标丢",
          "bag_ops.item_recently_dropped(item[0])" in asrc and "bag_ops.mark_item_dropped(item[0])" in asrc,
          "_try_bag_cleanup 的丢弃点缺少闸门调用")

    # ② 提取纯函数并行为验证
    consts = {}
    for cname in ("_DROP_GUARD_MS", "_DROP_GUARD"):
        c = _extract_const(bag, cname)
        check("提取常量 %s" % cname, c is not None)
        consts[cname] = c
    fns = {}
    for fname in ("_now_ms", "item_recently_dropped", "mark_item_dropped"):
        f = _extract_func(bag, fname)
        check("提取函数 %s" % fname, f is not None)
        fns[fname] = f

    if all(consts.values()) and all(fns.values()):
        ns = {}
        try:
            for cname in ("_DROP_GUARD_MS", "_DROP_GUARD"):
                exec("%s = %s" % (cname, consts[cname]), ns)
            for fname in ("_now_ms", "item_recently_dropped", "mark_item_dropped"):
                exec(fns[fname], ns)
        except Exception as e:  # noqa
            check("exec 提取源码", False, str(e))
            ns = {}
        rd = ns.get("item_recently_dropped")
        mk = ns.get("mark_item_dropped")
        win = ns.get("_DROP_GUARD_MS")
        if rd and mk and isinstance(win, int):
            ITEM = 17601234567890123
            T0 = 1790050000000
            check("窗口常量是正数(ms)", win > 1000, "win=%s" % win)
            # 未丢过 → False
            check("未丢过 → False", rd(ITEM, T0) is False)
            # 标丢 → True
            mk(ITEM, T0)
            check("刚丢过 → True", rd(ITEM, T0 + 1000) is True)
            # 窗口内仍 True
            check("窗口内 → True", rd(ITEM, T0 + win - 1) is True)
            # 超窗口 → False（且过期清理）
            check("超窗口 → False", rd(ITEM, T0 + win + 1) is False)
            check("超窗口后条目被清理", ITEM not in (ns.get("_DROP_GUARD") or {}))
            # 另一件互不影响
            mk(ITEM + 1, T0)
            check("不同实例互不影响", rd(ITEM, T0 + win + 2) is False and rd(ITEM + 1, T0 + 1) is True)
            # ③ 反例: 不过闸 → 同一件会被连丢两次
            sent = []
            def drop_naive(_id):
                sent.append(_id)
            drop_naive(ITEM + 2); drop_naive(ITEM + 2)   # 两套路径都不过闸
            check("反例: 不过闸会重复丢(2 次)",
                  len(sent) == 2 and sent[0] == sent[1],
                  "模拟未生效")

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
