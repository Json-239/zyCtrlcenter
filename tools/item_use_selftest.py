# -*- coding: utf-8 -*-
"""item_use 源码级自检（2026-09-22）

背景: 现场 TIDY_BAG(daily_ghost.__tidy_bag) 把背包里"非装备非任务"的物品一律当
人物道具用 C2S_USE_ITEM 使用 → 幻兽丹/亲密丹(use_on_summon=yes) 被服务端拒
(notice 1268 "未打开守护界面") 且**物品不消耗** → 每整理轮重试刷屏
(diag 实测: 单件刷 16 次, notice 1268 累计 2400+)。

本自检断言:
  ① 元数据解析包含 use_on_summon(item_param 解析);
  ② is_summon_only_item 判据正确(守护道具/守护经验丹无参战守护 → 跳过);
  ③ TIDY_BAG 的"使用可消耗品"分支确实调用该判据(源码形状);
  ④ 反证: 幻兽丹在旧行为下**会**被当人物道具使用(helper 返回 True = 必须跳过)。

用法:
  python tools/item_use_selftest.py <script 目录 或 daily_ghost.py 路径>
"""
import os
import re
import sys


def _resolve(path):
    if os.path.isdir(path):
        return os.path.join(path, "daily_ghost.py")
    return path


def _extract_func(src, name):
    m = re.search(r"^def %s\(.*?(?=\n\S|\Z)" % re.escape(name), src, re.S | re.M)
    return m.group(0) if m else None


def _extract_const(src, name):
    m = re.search(r"^%s\s*=\s*(\([^)]*\)|\d+)\s*(?:#.*)?$" % re.escape(name), src, re.M)
    return m.group(1) if m else None


def main():
    if len(sys.argv) < 2:
        print("用法: python tools/item_use_selftest.py <script 目录 或 daily_ghost.py 路径>")
        return 2
    src_path = _resolve(sys.argv[1])
    if not os.path.exists(src_path):
        print("[FAIL] 找不到文件: %s" % src_path)
        return 2
    src = open(src_path, encoding="utf-8", errors="replace").read()

    results = []

    def check(name, cond, detail=""):
        results.append((name, bool(cond), detail))

    # ---- ① 源码形状: 元数据解析 use_on_summon ----
    check("元数据解析包含 item_param/use_on_summon",
          ("item_param" in src and '"use_on_summon"' in src
           and 'use_on_summon": False' in src),
          "未找到 use_on_summon 元数据解析")
    check("TIDY_BAG 使用分支调用 is_summon_only_item",
          "if is_summon_only_item(" in src,
          "TIDY_BAG 未调用 is_summon_only_item")

    # ---- ② 提取纯函数并测判据 ----
    fsrc = _extract_func(src, "is_summon_only_item")
    csrc = _extract_const(src, "SUMMON_EXP_ITEM_INDEXES")
    check("提取 is_summon_only_item 源码", fsrc is not None, "正则未匹配到函数")
    check("提取 SUMMON_EXP_ITEM_INDEXES 常量", csrc is not None, "正则未匹配到常量")
    if fsrc and csrc:
        ns = {}
        try:
            exec("SUMMON_EXP_ITEM_INDEXES = %s" % csrc, ns)
            exec(fsrc, ns)
        except Exception as e:  # noqa
            check("exec 提取的源码", False, str(e))
            ns = {}
        fn = ns.get("is_summon_only_item")
        if fn is not None:
            # 幻兽丹/亲密丹: use_on_summon=True → 必须跳过(反证: 旧行为会当人物道具用)
            check("幻兽丹(use_on_summon=yes) → 跳过",
                  fn(101008, {"handler": "其它物品", "use_on_summon": True}) is True)
            check("亲密丹(use_on_summon=yes) → 跳过",
                  fn(108477, {"handler": "其它物品", "use_on_summon": True}) is True)
            # 人物经验丹: 非守护道具 → 不跳过(允许人物路径使用)
            check("人物经验丹(101372) → 不跳过",
                  fn(101372, {"handler": "其它物品", "use_on_summon": False}) is False)
            # 守护经验丹: 无参战守护 → 跳过(否则弹对话且不消耗, 同样循环)
            check("守护经验丹(101373)+无参战守护 → 跳过",
                  fn(101373, {"handler": "其它物品", "use_on_summon": False}, 0) is True)
            # 守护经验丹: 有参战守护 → 允许(服务端会真加经验)
            check("守护经验丹(101373)+有参战守护 → 不跳过",
                  fn(101373, {"handler": "其它物品", "use_on_summon": False}, 12345) is False)
            # 宝箱/药品等普通物品 → 不跳过
            check("宝箱(101079) → 不跳过",
                  fn(101079, {"handler": "其它物品", "use_on_summon": False}) is False)
            # 元数据缺失(空 dict) → 不跳过(保守: 不误伤普通物品)
            check("元数据缺失 → 不跳过(保守)",
                  fn(999999, {}) is False)

    # ---- ③ TIDY_BAG 分支形状（不用正则: 大文件上 (?:\s*#.*\n)* 会回溯爆炸）----
    i = src.find("if is_summon_only_item(")
    j = src.find("to_use.append", i) if i >= 0 else -1
    seg = src[i:j] if (i >= 0 and j > i) else ""
    check("TIDY_BAG: 跳过分支在 to_use.append 之前",
          i >= 0 and j > i and "skipped_summon += 1" in seg and "else:" in seg,
          "未匹配到 if is_summon_only_item → skipped_summon += 1 → else to_use.append")

    # ---- 输出 ----
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
