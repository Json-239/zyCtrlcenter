# -*- coding: utf-8 -*-
"""热更(reload)缓存保活 + 网格三级兜底 修复自检 —— 2026-09-28

现场（robot0003001@xy3.com，只读取证）:
  · 11:24:27 机器人进程重拉（冷启动，缓存全空）；11:26:30 重连上线，意图 newbie，
    RESTORE 补发的 start_chain **不带 chain 字段**（中控 Payloads.For 对 newbie 返回
    nil，diag.log 存证: {'accounts': [...], 'chain_id': 'newbie_full', 'cmd': 'start_chain'}）
    → 机器人端该号 quest.chain 为空，跨图/NPC 全靠 client.py 无条件合并的全局
    g_nav_chain（其它号 ghost_start 命令带入的基座链数据）。
  · 11:26:32→11:27:51 跨图 6→5→9→10→11 成功（dijkstra 来自全局缓存）。
  · **11:27:52 热更 quest_engine（reload_reply ok）→ importlib.reload 重跑顶层代码
    → g_nav_chain / g_chain_grid_cache / g_chain_dijkstra_cache / g_chain_cache 全部归零。**
  · 11:27:51 恰在图 11 落点（先于清空 1 秒），11:27:53 图内走路取网格：
    __chain_grid_for → quest.chain 空 + g_chain_grid_cache 空 → None →
    GRID_MISSING「地图 11 无寻路网格」→ quest.active=True → ST_ERROR 终态
    （tick 对 ERROR 直接 return；仅服务端 add_task/update_task 推送才自愈，本号没有）。
  · 11:28 起其它号的 ghost_start 已把全局缓存自然重填，但 3001 已停链，不自动恢复。

修复（quest_engine.py，两处）:
  1) 顶层 __reload_keep_any/__reload_keep_dict: reload 保活 4 个全局缓存
     （importlib.reload 复用模块 __dict__，只重跑顶层代码 → 旧值在 globals() 里可读；
     首次导入无旧值 → 原初始值，语义不变）。
  2) __chain_grid_for 第三级兜底 g_nav_chain["map_grids"]: g_nav_chain 由 client.py 对
     所有带 chain 命令无条件合并，而 g_chain_grid_cache 只在 set_quest_chain 执行时填
     （满额/已停的抓鬼号在 done_limit 分支提前 return，链只进 g_nav_chain）——新手链号
     （quest.chain 空）过去只认第二级，窗口内即 GRID_MISSING。

用法: python tools/grid_cache_reload_selftest.py [script_dir]
"""
import contextlib
import importlib
import io
import os
import sys
from unittest.mock import MagicMock

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_DIR = os.path.normpath(os.path.join(
    HERE, "..", "deploy", "zones", "prod-240-2300", "script"))
script_dir = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_DIR

fails = 0
total = 0


def check(name, ok, detail=""):
    global fails, total
    total += 1
    fails += 0 if ok else 1
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  " + detail) if detail else ""))


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


qe_path = os.path.join(script_dir, "quest_engine.py")
if not os.path.exists(qe_path):
    print("[FAIL] 找不到 %s" % qe_path)
    sys.exit(2)
qe = open(qe_path, encoding="utf-8", errors="replace").read()

# ============================================================ A. 源码形状
check("A1: 定义 __reload_keep_any(reload 保活: 初始 None 型)",
      "def __reload_keep_any(name):" in qe)
check("A2: 定义 __reload_keep_dict(reload 保活: 初始 {} 型)",
      "def __reload_keep_dict(name):" in qe)
check("A3: g_chain_cache 走保活入口", 'g_chain_cache = __reload_keep_dict("g_chain_cache")' in qe)
check("A4: g_nav_chain 走保活入口", 'g_nav_chain = __reload_keep_any("g_nav_chain")' in qe)
check("A5: g_chain_grid_cache 走保活入口",
      'g_chain_grid_cache = __reload_keep_dict("g_chain_grid_cache")' in qe)
check("A6: g_chain_dijkstra_cache 走保活入口",
      'g_chain_dijkstra_cache = __reload_keep_dict("g_chain_dijkstra_cache")' in qe)
check("A7: __chain_grid_for 含 g_nav_chain 第三级兜底",
      'g_nav_chain.get("map_grids")' in qe)
# 三级读取顺序: quest.chain → g_chain_grid_cache → g_nav_chain
_i_q = qe.find('grids = quest.chain.get("map_grids"')
_i_c = qe.find("grid_data = g_chain_grid_cache.get(str(mapid)")
_i_n = qe.find('g_nav_chain.get("map_grids")')
check("A8: 三级顺序正确(quest.chain → 全局网格缓存 → g_nav_chain)",
      0 <= _i_q < _i_c < _i_n, "位置: %s/%s/%s" % (_i_q, _i_c, _i_n))
check("A9: 旧的无条件清零写法已移除(g_nav_chain = None 独占一行)",
      "\ng_nav_chain = None\n" not in qe)

# ============================================================ B. reload 保活(单元语义)
ns_any, ns_dict = {}, {}
frag_any = _extract_func(qe, "__reload_keep_any")
frag_dict = _extract_func(qe, "__reload_keep_dict")
check("B0a: 提取 __reload_keep_any", frag_any is not None)
check("B0b: 提取 __reload_keep_dict", frag_dict is not None)
if frag_any and frag_dict:
    try:
        exec(frag_any, ns_any)
        exec(frag_dict, ns_dict)
    except Exception as e:  # noqa: BLE001
        check("B0c: exec 保活函数", False, "%s: %s" % (type(e).__name__, e))
keep_any = ns_any.get("__reload_keep_any")
keep_dict = ns_dict.get("__reload_keep_dict")

if keep_any is not None and keep_dict is not None:
    # 首次导入: globals() 无旧值 → 原初始值(None / {})
    check("B1: 首次导入 g_nav_chain → None(无旧值)", keep_any("g_nav_chain") is None)
    check("B2: 首次导入 g_chain_grid_cache → {}(无旧值)",
          keep_dict("g_chain_grid_cache") == {})
    # reload: 旧值在 globals() 里 → 保留(同一对象)
    G11 = {"w": 282, "h": 213, "rows": ["1" * 282] * 213}
    nav = {"npcs": {"13520": [[11, 3507, 1035]]}, "map_grids": {"11": G11}}
    grid = {"11": G11}
    ns_any["g_nav_chain"] = nav
    ns_dict["g_chain_grid_cache"] = grid
    check("B3: reload 保留 g_nav_chain(现场: 13520 坐标/网格在 reload 后必须还在)",
          keep_any("g_nav_chain") is nav)
    check("B4: reload 保留 g_chain_grid_cache(现场根因数据)",
          keep_dict("g_chain_grid_cache") is grid)
    # 反例: 坏值/空值 → 回初始值
    ns_any["g_nav_chain"] = "corrupted"
    ns_dict["g_chain_grid_cache"] = ["bad"]
    check("B5(反例): 非 dict 旧值 → 回初始值",
          keep_any("g_nav_chain") is None and keep_dict("g_chain_grid_cache") == {})
    ns_any["g_nav_chain"] = {}
    check("B6(反例): g_nav_chain 空 dict → None(恢复'未填充'语义)",
          keep_any("g_nav_chain") is None)

# ============================================================ C. __chain_grid_for 三级真值表
ns_cg = {}
frag_cg = _extract_func(qe, "__chain_grid_for")
check("C0: 提取 __chain_grid_for", frag_cg is not None)
if frag_cg:
    try:
        exec(frag_cg, ns_cg)
    except Exception as e:  # noqa: BLE001
        check("C0b: exec __chain_grid_for", False, "%s: %s" % (type(e).__name__, e))
cg = ns_cg.get("__chain_grid_for")


class _Q(object):
    def __init__(self, chain=None):
        self.chain = chain


G_CHAIN = {"w": 282, "h": 213, "rows": ["chain"]}
G_CACHE = {"w": 282, "h": 213, "rows": ["cache"]}
G_NAV = {"w": 282, "h": 213, "rows": ["nav"]}

if cg is not None:
    # C1 第 1 级: quest.chain 命中(优先于后两级)
    ns_cg["g_chain_grid_cache"] = {"11": G_CACHE}
    ns_cg["g_nav_chain"] = {"map_grids": {"11": G_NAV}}
    r = cg(_Q({"map_grids": {"11": G_CHAIN}}), 11)
    check("C1: quest.chain 命中且优先级最高", r is G_CHAIN)
    # C2 第 2 级: quest.chain 空 → 全局网格缓存
    r = cg(_Q(None), 11)
    check("C2: quest.chain 空 → g_chain_grid_cache", r is G_CACHE)
    # C3 第 3 级(本次修复): 缓存也空 → g_nav_chain["map_grids"]
    ns_cg["g_chain_grid_cache"] = {}
    r = cg(_Q(None), 11)
    check("C3: 缓存空 → g_nav_chain 兜底(3001 现场形态)", r is G_NAV)
    # C4 全空 → None(GRID_MISSING 原文语义保持)
    ns_cg["g_nav_chain"] = {"npcs": {}}
    r = cg(_Q(None), 11)
    check("C4(反例): 三级全空 → None(仍报 GRID_MISSING, 语义不变)", r is None)
    # C5 g_nav_chain 未填充(None)不崩
    ns_cg["g_nav_chain"] = None
    r = cg(_Q(None), 11)
    check("C5(反例): g_nav_chain=None → None 且不崩", r is None)
    # C6 传入 int / str 的 mapid 都能命中(str 化键)
    ns_cg["g_nav_chain"] = {"map_grids": {"11": G_NAV}}
    r1, r2 = cg(_Q(None), 11), cg(_Q(None), "11")
    check("C6: mapid int/str 入参均可命中", r1 is G_NAV and r2 is G_NAV)
    # C7 其它图不受影响(键不存在 → None)
    r = cg(_Q(None), 12)
    check("C7(反例): 其它图无数据 → None", r is None)

# ============================================================ D. 真实 reload 集成(事故精确复现)
# 隔离导入: mock 打包 C 扩展 cnetwork(同 cold_start_selftest 口径); 只 import 仓库副本。
_old_path = list(sys.path)
try:
    sys.path.insert(0, script_dir)
    sys.modules.setdefault("cnetwork", MagicMock(name="cnetwork"))
    buf = io.StringIO()
    with contextlib.redirect_stdout(buf), contextlib.redirect_stderr(buf):
        importlib.import_module("quest_engine")
    qe_mod = sys.modules["quest_engine"]
    check("D1: 隔离 import quest_engine 成功", True)
except Exception as e:  # noqa: BLE001
    qe_mod = None
    check("D1: 隔离 import quest_engine 成功", False, "%s: %s" % (type(e).__name__, str(e)[:160]))

if qe_mod is not None:
    # D2 模拟"运行期缓存已填充"(其它号 ghost_start 带入的基座链)
    qe_mod.g_nav_chain = {"npcs": {"13520": [[11, 3507, 1035]]},
                          "map_grids": {"11": G_NAV}, "dijkstra": {"6": [{"x": 1}]}}
    qe_mod.g_chain_grid_cache["11"] = G_NAV
    qe_mod.g_chain_dijkstra_cache["6"] = [{"x": 1}]
    qe_mod.g_chain_cache["robot0003001@xy3.com"] = {"cmd": "start_chain",
                                                    "chain_id": "newbie_full"}
    try:
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf), contextlib.redirect_stderr(buf):
            importlib.reload(qe_mod)
        check("D2: importlib.reload 执行成功(热更路径)", True)
    except Exception as e:  # noqa: BLE001
        check("D2: importlib.reload 执行成功(热更路径)", False,
              "%s: %s" % (type(e).__name__, str(e)[:160]))
    # D3 事故精确复现: reload 后缓存必须还在(修复前此处为 None/{}, 即 3001 的根因)
    check("D3: reload 后 g_chain_grid_cache 仍含图 11(事故复现锚点)",
          isinstance(qe_mod.g_chain_grid_cache, dict)
          and qe_mod.g_chain_grid_cache.get("11") is not None)
    check("D4: reload 后 g_nav_chain 仍在(13520 坐标/网格)",
          isinstance(qe_mod.g_nav_chain, dict)
          and qe_mod.g_nav_chain.get("npcs", {}).get("13520"))
    check("D5: reload 后 g_chain_dijkstra_cache 仍在", "6" in qe_mod.g_chain_dijkstra_cache)
    check("D6: reload 后 g_chain_cache 仍在(断线续跑命令)",
          "robot0003001@xy3.com" in qe_mod.g_chain_cache)
    # D7 端到端: 3001 场景 —— quest.chain 空 + reload 后直接取网格, 必须能取到
    fn = None
    for k, v in qe_mod.__dict__.items():
        if k.endswith("__chain_grid_for"):
            fn = v
            break
    class _Q2(object):
        chain = None
    if fn is not None:
        r = fn(_Q2(), 11)
        check("D7: reload 后 __chain_grid_for(None quest, 11) 仍可命中缓存",
              r is not None)
    else:
        check("D7: 在模块字典中找到 __chain_grid_for", False)
else:
    check("D2-D7: 真实 reload 集成(依赖 D1)", False, "跳过")

try:
    sys.path[:] = _old_path
except Exception:
    pass

# ============================================================ 汇总
print("\n========== 汇总 ==========")
print("通过 %d / %d，失败 %d" % (total - fails, total, fails))
if fails:
    print("结论: FAIL（修复或环境有问题, 请勿热更）")
    sys.exit(1)
print("结论: 全绿（可热更）")
sys.exit(0)
