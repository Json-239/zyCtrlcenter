# -*- coding: utf-8 -*-
"""booth.py 行为自检（2026-09-24 摆摊单号最小验证）。

覆盖（全部 mock 环境, 不触碰生产/不写 diag.log）:
  · 计划文件读取（缺文件/账号不符/停用/坏 JSON 保留旧值/节流）
  · 激活前置（等级、参数）
  · 状态机全流程: INIT→WAIT_MAP→GOTO→OPEN→UP→HOLD→(CLOSE)→VERIFY→DONE
  · 走路（A* 交互 stub）: 无网格失败 / 直接到达 / 分批发送
  · 上架物品选择: 保护/装备/锁定/无 meta/pos 不在背包/找不到 → skip
  · S2C 回调: 90009 自己/他人、90059 回执匹配/不匹配、90147 售出累计、
    90109 收摊、notice 拒绝（开摊期记录 / 上架期判失败）
  · 开摊超时重试上限 → FAIL
  · dispatch_cmd: booth_start/stop/status
  · 模块级 main_tester._BOOTH_MISSING 复位（首次加载 + reload 幂等）
用法: python tools/booth_selftest.py
"""
import io
import json
import os
import shutil
import sys
import tempfile
import types

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

HERE = os.path.dirname(os.path.abspath(__file__))
REPO_SCRIPT = os.path.normpath(os.path.join(
    HERE, "..", "deploy", "zones", "prod-240-2300", "script"))

fails = []
total = 0


def check(name, ok, detail=""):
    global total
    total += 1
    if not ok:
        fails.append(name)
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  " + str(detail)) if detail else ""))


# ---------------------------------------------------------------------------
# 环境 stub（不 import 生产重模块；error 用真文件）
# ---------------------------------------------------------------------------
sys.path.insert(0, REPO_SCRIPT)

# diag: 收集日志（不写生产 diag.log）
_diag_lines = []
_diag_mod = types.ModuleType("diag")
_diag_mod.log = lambda msg: _diag_lines.append(str(msg))
sys.modules["diag"] = _diag_mod

# main_tester: 只带 _BOOTH_MISSING 标志
_mt_mod = types.ModuleType("main_tester")
_mt_mod._BOOTH_MISSING = True          # 模拟"booth.py 曾缺失"的现状
sys.modules["main_tester"] = _mt_mod

# bag_ops: is_protected_item 可由用例改写
_bagops_mod = types.ModuleType("bag_ops")
_bagops_mod.is_protected_item = lambda idx: False
sys.modules["bag_ops"] = _bagops_mod

# protocol3: 只暴露三个 C2S id
_p3_mod = types.ModuleType("protocol3")
_p3_mod.C2S_SELLER_START_BOOTH = 80170
_p3_mod.C2S_UP_SELLER_ITEM = 80236
_p3_mod.C2S_SELLER_CLOSE_BOOTH = 80070
_p3_mod.C2S_PLAYERMOVE = 10001
_p3_mod.C2S_NOTIFY_POSITION = 10002
sys.modules["protocol3"] = _p3_mod

# robot_path: FakeFinder（用例控制返回路径）
class _FakeFinder(object):
    PATH = None            # None = 寻不到路

    def __init__(self, grid):
        self.grid = grid

    def find_path(self, fx, fy, tx, ty):
        if _FakeFinder.PATH is None:
            return None
        return [tuple(p) for p in _FakeFinder.PATH]


_rp_mod = types.ModuleType("robot_path")
_rp_mod.GridPathFinder = _FakeFinder
sys.modules["robot_path"] = _rp_mod

# quest_engine: grid_for 用例控制
_qe_mod = types.ModuleType("quest_engine")
_qe_mod._GRID = object()
_qe_mod.grid_for = lambda mapid: _qe_mod._GRID
sys.modules["quest_engine"] = _qe_mod

# config: quest_walk_speed
_cfg_mod = types.ModuleType("config")
_cfg_mod.quest_walk_speed = 5.0
sys.modules["config"] = _cfg_mod

# robot_operator: set_pose 直接写 m_pose（本地推进用）
_ro_op = types.ModuleType("robot_operator")


def _fake_set_pose(ro, x, y, mapid=None, source=""):
    ro.m_pose[0] = int(x)
    ro.m_pose[1] = int(y)


_ro_op.set_pose = _fake_set_pose
sys.modules["robot_operator"] = _ro_op

# client: 物品名表
_cl_mod = types.ModuleType("client")
_cl_mod.g_item_data_dict = {
    170001: {"item_name": "云母粉"},
    111029: {"item_name": "乌金"},
    108477: {"item_name": "小颗亲密丹"},
    225601: {"item_name": "贝壳项链"},
}
sys.modules["client"] = _cl_mod

import importlib

booth = importlib.import_module("booth")

# 计划文件走临时目录（不动生产 script 目录）
_tmpdir = tempfile.mkdtemp(prefix="booth_selftest_")
booth._PLAN_PATH = os.path.join(_tmpdir, "booth_selftest.json")

ACCT = "robot0001032@xy3.com"


def write_plan(d):
    with io.open(booth._PLAN_PATH, "w", encoding="utf-8") as f:
        f.write(json.dumps(d, ensure_ascii=False))
    booth._PLAN_CACHE["ms"] = 0        # 绕过节流（测试专用）


def rm_plan():
    try:
        os.remove(booth._PLAN_PATH)
    except OSError:
        pass
    booth._PLAN_CACHE["ms"] = 0


class FakeRobot(object):
    def __init__(self, acct=ACCT, level=50, mapid=11, pos=(1409, 1109)):
        self.m_account = [acct, "pw"]
        self.m_logined = True
        self.m_level = level
        self.m_mapid = mapid
        self.m_pose = [pos[0], pos[1], 0]
        self.m_id = 8000216
        self.m_fight_state = False
        self.m_bag_cache = {}
        self.m_bag_meta = {}
        self.m_booth = None
        self.m_collect_walk = None
        self.sent = []

    def get_role_id(self):
        return self.m_id

    def send_message(self, msgid, data):
        self.sent.append((msgid, data))
        return 1


def base_plan(**kw):
    p = {
        "enabled": True,
        "account": ACCT,
        "mapid": 11,
        "cell": [1409, 1109],
        "rect": [1379, 1079, 1439, 1139],
        "booth_name": "杂货小摊",
        "booth_poster": "",
        "up_items": [],
        "hold_sec": 2,
        "max_open_retry": 2,
        "map_wait_sec": 60,
    }
    p.update(kw)
    return p


def set_bag(ro, items):
    """items: [(item_index, item_id, count, pos, meta)]"""
    ro.m_bag_cache = {}
    ro.m_bag_meta = {}
    for idx, iid, cnt, pos, meta in items:
        ro.m_bag_cache[idx] = [iid, cnt, pos]
        ro.m_bag_meta[idx] = meta or {"quality": 1, "is_equip": False, "is_locked": False}


# ================================================================ 1) 加载与复位
check("模块加载: main_tester._BOOTH_MISSING 复位",
      _mt_mod._BOOTH_MISSING is False, _mt_mod._BOOTH_MISSING)

# reload 幂等（模拟再次热更）
_mt_mod._BOOTH_MISSING = True
booth = importlib.reload(booth)
booth._PLAN_PATH = os.path.join(_tmpdir, "booth_selftest.json")
check("reload 幂等: 标记再次复位", _mt_mod._BOOTH_MISSING is False)

# ================================================================ 2) 计划读取
rm_plan()
check("无计划: tick 不建对象", booth.tick(FakeRobot(), 1000) is None)

write_plan(base_plan(account="other@x"))
ro0 = FakeRobot()
booth.tick(ro0, 1000)
check("账号不符: 不建对象", ro0.m_booth is None)

write_plan(base_plan(enabled=False))
ro0 = FakeRobot()
booth.tick(ro0, 1000)
check("计划停用: 不建对象", ro0.m_booth is None)

# 坏 JSON: 保留旧数据（先写好的, 再写坏的）
write_plan(base_plan())
booth._PLAN_CACHE["ms"] = 0
p1 = booth._load_plan()
with io.open(booth._PLAN_PATH, "w", encoding="utf-8") as f:
    f.write('{"enabled": tr')       # 半截
booth._PLAN_CACHE["ms"] = 0
booth._PLAN_CACHE["sig"] = None     # 强制重读（sig 变化路径）
p2 = booth._load_plan()
check("坏 JSON 保留旧数据", p2 is not None and p2.get("account") == ACCT, p2)

# ================================================================ 3) 激活前置
rm_plan()
write_plan(base_plan())
ro = FakeRobot(level=30)
booth.tick(ro, 1000)
check("等级<50: 不激活", ro.m_booth is None or not ro.m_booth.enabled)

ro = FakeRobot(mapid=38)
booth._PLAN_CACHE["ms"] = 0
booth.tick(ro, 1000)
b = ro.m_booth
check("等级>=50: 激活建对象", b is not None and b.enabled)
check("激活初始: INIT→WAIT_MAP(图不符)", b.state == "WAIT_MAP", b.state)
check("激活: 未发任何包", len(ro.sent) == 0, ro.sent)

# WAIT_MAP 超时
booth.tick(ro, 1000 + 61 * 1000)
check("WAIT_MAP 超时 → FAIL", b.state == "FAIL" and not b.enabled, b.state)

# ================================================================ 4) 全流程（地图=11, 已在目标点）
rm_plan()
write_plan(base_plan(up_items=[
    {"item_index": 170001, "name": "云母粉", "price": 1000},
    {"item_index": 111029, "name": "乌金", "price": 1500},
]))
ro = FakeRobot()
set_bag(ro, [
    (170001, 17617794429167748, 210, 8192, None),
    (111029, 17616325550435108, 35, 8206, None),
])
t = 10000
booth.tick(ro, t)                    # INIT→GOTO
check("流程: INIT→GOTO", ro.m_booth.state == "GOTO", ro.m_booth.state)
t += 1000
booth.tick(ro, t)                    # GOTO: 已到位 → OPEN
check("流程: GOTO 到位→OPEN", ro.m_booth.state == "OPEN", ro.m_booth.state)
t += 2000                         # 等 open_hold_until(1.5s) 过
booth.tick(ro, t)                    # OPEN: 发开摊
sent0 = list(ro.sent)
check("流程: 开摊发包 80170 [name,poster]",
      sent0 and sent0[-1][0] == 80170 and sent0[-1][1] == ["杂货小摊", ""], sent0[-1:])

# 开摊回执（自己 role_id）
booth.on_seller_start_booth(ro, [8000216, 1, "杂货小摊", 1409, 1109])
t += 1000
booth.tick(ro, t)                    # OPEN: open_ok → UP
b = ro.m_booth
check("流程: 开摊回执→UP", b.state == "UP", b.state)
check("流程: up_queue=2 件", len(b.up_queue) == 2,
      [(u["name"], u["price"]) for u in b.up_queue])
check("流程: 背包快照已记", 170001 in b.bag_before and b.bag_before[170001][1] == 210,
      b.bag_before)

t += 1000
booth.tick(ro, t)                    # UP: 发第一件
sent1 = list(ro.sent)
check("流程: 上架发包 80236 [item_id,price]",
      sent1[-1][0] == 80236 and sent1[-1][1] == [17617794429167748, 1000], sent1[-1:])
booth.on_up_item(ro, [17617794429167748, 1000])
t += 1000
booth.tick(ro, t)                    # UP: 发第二件
check("流程: 第二件发送", ro.sent[-1] == (80236, [17616325550435108, 1500]), ro.sent[-1])
booth.on_up_item(ro, [17616325550435108, 1500])
t += 1000
booth.tick(ro, t)                    # UP: 空 → HOLD
b = ro.m_booth
check("流程: 全部上架→HOLD", b.state == "HOLD" and len(b.up_done) == 2, b.state)

# 售出事件
booth.on_sold_item(ro, [12345, "买家甲", 1000, 3, ""])
check("售出: sold_count/income 累计", b.sold_count == 3 and b.income == 3000,
      (b.sold_count, b.income))

t += 3000
booth.tick(ro, t)                    # HOLD 超时: 发收摊 → VERIFY
check("流程: 收摊发包 80070",
      ro.sent[-1] == (80070, []) and ro.m_booth.state == "VERIFY", ro.sent[-1:])
booth.on_close_booth(ro, [8000216])
set_bag(ro, [
    (170001, 17617794429167748, 207, 8192, None),   # 卖出 3
    (111029, 17616325550435108, 35, 8206, None),
])
t += 3000
booth.tick(ro, t)                    # VERIFY → RESULT/DONE
b = ro.m_booth
check("流程: VERIFY→DONE", b.state == "DONE" and not b.enabled, b.state)
result_line = [x for x in _diag_lines if x.startswith("BOOTH_SELFTEST RESULT")]
check("流程: RESULT 日志(ok, 售出核对)",
      result_line and "ok=True" in result_line[-1] and "(170001, 3)" in result_line[-1],
      result_line[-1] if result_line else "(无)")
check("流程: DONE 后 tick 不重复激活",
      (booth.tick(ro, t + 1000) is None) and ro.m_booth.state == "DONE")

# ================================================================ 5) 走路分支
rm_plan()
write_plan(base_plan())
ro = FakeRobot(pos=(1300, 1000))     # 距目标 ~150px
_qe_mod._GRID = None                 # 无网格
t = 20000
booth.tick(ro, t)                    # INIT→GOTO
t += 1000
booth.tick(ro, t)                    # GOTO → fail:no_grid
check("走路: 无网格 → FAIL", ro.m_booth.state == "FAIL",
      [x for x in _diag_lines if "no_grid" in x][-1:])

rm_plan()
write_plan(base_plan())
_qe_mod._GRID = object()
_FakeFinder.PATH = [(1320, 1010), (1340, 1030), (1360, 1050)]
ro = FakeRobot(pos=(1300, 1000))
booth._PLAN_CACHE["ms"] = 0
t = 30000
booth.tick(ro, t)                    # INIT→GOTO
t += 1000
booth.tick(ro, t)                    # GOTO: 发一批 PLAYERMOVE
mv = [x for x in ro.sent if x[0] == 10001]
check("走路: 发送 C2S_PLAYERMOVE 批次",
      mv and mv[-1][1][0] == 2 and len(mv[-1][1][1]) == 4, mv[-1:] if mv else None)
# 本地推进：时间推过批次估算 → tick 应把 m_pose 推到该批终点（服务端不回发本人移动）
t += 3000
booth.tick(ro, t)
check("走路: 本地推进 m_pose → 批终点(1360,1050)",
      tuple(ro.m_pose[:2]) == (1360, 1050), tuple(ro.m_pose[:2]))
npos = [x for x in ro.sent if x[0] == 10002]
check("走路: 本地推进时上报 C2S_NOTIFY_POSITION [2,x,y,1]",
      npos and npos[-1][1] == [2, 1360, 1050, 1], npos[-1:] if npos else None)

# 到最后一格附近（dist<=20 → arrived, 不再寻路）
_FakeFinder.PATH = None
ro.m_pose = [1409, 1109, 0]
booth._PLAN_CACHE["ms"] = 0
t += 2000
booth.tick(ro, t)
check("走路: 到点→OPEN", ro.m_booth.state == "OPEN", ro.m_booth.state)

# ================================================================ 6) 开摊超时重试上限
rm_plan()
write_plan(base_plan(max_open_retry=2))
ro = FakeRobot()
t = 40000
booth.tick(ro, t)                    # INIT→GOTO
t += 1000
booth.tick(ro, t)                    # GOTO→OPEN
t += 4000
booth.tick(ro, t)                    # 发开摊 (try1)
t += 4000
booth.tick(ro, t)                    # 超时 → try2
t += 4000
booth.tick(ro, t)                    # 超时 → 超上限 → FAIL
check("开摊: 超时重试上限 → FAIL", ro.m_booth.state == "FAIL"
      and len([x for x in ro.sent if x[0] == 80170]) == 2,
      [x for x in ro.sent if x[0] == 80170])

# ================================================================ 6b) 1191 扫描换点
rm_plan()
write_plan(base_plan(cell=[1409, 1109],
                     scan={"cx": 1409, "cy": 1109, "step": 32, "max": 3}))
_FakeFinder.PATH = [(1377, 1077)]
ro = FakeRobot(pos=(1409, 1109))
t = 46000
booth.tick(ro, t)                    # 激活 INIT→GOTO
t += 1000
booth.tick(ro, t)                    # GOTO(到位)→OPEN
t += 2000                         # 等 open_hold_until(1.5s) 过
booth.tick(ro, t)                    # 发开摊 try1
b = ro.m_booth
check("扫描: 初始 cell", b.cell == (1409, 1109) and b.scan_enabled, (b.cell, b.scan_enabled))
open1 = [x for x in ro.sent if x[0] == 80170]
check("扫描: 第一次开摊已发", len(open1) == 1, open1)
booth.on_notice(ro, "[1191, []]")     # 此地禁止摆摊
t += 500
booth.tick(ro, t)                     # → 换点 → GOTO
check("扫描: 1191 → 换点 GOTO", b.state == "GOTO" and b.cell == (1377, 1077),
      (b.state, b.cell))
check("扫描: 失败点入 tried", b.scan_tried and b.scan_tried[-1] == (1409, 1109),
      b.scan_tried)
check("扫描: 剩余点 2", len(b.scan_points) == 2, b.scan_points)
t += 1000
booth.tick(ro, t)                     # GOTO: 发批次走 45px
t += 3000
booth.tick(ro, t)                     # 本地推进 → arrived → OPEN
t += 2000                             # 等 open_hold_until(1.5s) 过
booth.tick(ro, t)                     # 发开摊 try1（新点）
check("扫描: 新点开摊已发", len([x for x in ro.sent if x[0] == 80170]) == 2,
      [x for x in ro.sent if x[0] == 80170])
booth.on_seller_start_booth(ro, [8000216, 1, "杂货小摊", 1377, 1077])
t += 500
booth.tick(ro, t)                     # → UP
check("扫描: 第二点开摊成功 → UP", b.state == "UP" and b.open_ok, (b.state, b.cell))

# ================================================================ 7) 上架被拒 notice
rm_plan()
write_plan(base_plan(up_items=[{"item_index": 170001, "name": "云母粉", "price": 1000}]))
ro = FakeRobot()
set_bag(ro, [(170001, 111, 5, 8192, None)])
t = 50000
booth.tick(ro, t)
t += 1000
booth.tick(ro, t)
booth.on_seller_start_booth(ro, [8000216, 1, "杂货小摊", 1409, 1109])
t += 1000
booth.tick(ro, t)                    # → UP
t += 1000
booth.tick(ro, t)                    # 发上架
booth.on_notice(ro, "该物品不可交易")
check("上架: notice 拒绝 → up_failed",
      ro.m_booth.up_pending is None and len(ro.m_booth.up_failed) == 1,
      (ro.m_booth.up_pending, ro.m_booth.up_failed))

# ================================================================ 8) 物品选择规则
rm_plan()
write_plan(base_plan(up_items=[
    {"item_index": 170001, "name": "云母粉", "price": 1000},   # 正常
    {"item_index": 102007, "name": "金创药", "price": 10},     # 保护（改写 bag_ops）
    {"item_index": 225601, "name": "贝壳项链", "price": 100},  # 装备（meta）
    {"item_index": 111029, "name": "乌金", "price": 1500},     # 锁定（meta）
    {"item_index": 108477, "name": "小颗亲密丹", "price": 50}, # pos 不在背包
    {"item_index": 999999, "name": "无元数据", "price": 50},   # 在背包但无 meta
    {"item_index": 777777, "name": "不存在", "price": 50},     # 不在背包
]))
ro = FakeRobot()
ro.m_bag_cache = {
    170001: [101, 10, 8192], 102007: [102, 99, 8193], 225601: [103, 1, 8198],
    111029: [104, 5, 8206], 108477: [105, 5, 21760], 999999: [106, 1, 8200],
}
ro.m_bag_meta = {
    170001: {"is_equip": False, "is_locked": False},
    102007: {"is_equip": False, "is_locked": False},
    225601: {"is_equip": True, "is_locked": False},
    111029: {"is_equip": False, "is_locked": True},
    108477: {"is_equip": False, "is_locked": False},
    # 999999 无 meta
}
_old_prot = _bagops_mod.is_protected_item
_bagops_mod.is_protected_item = lambda ix: ix in (102007, 190146)
picked = booth._pick_up_items(ro, booth._load_plan())
_bagops_mod.is_protected_item = _old_prot
by_idx = {p["item_index"]: p for p in picked}
ok_pick = (by_idx[170001].get("item_id") == 101 and not by_idx[170001]["skip_reason"])
ok_prot = by_idx[102007]["skip_reason"] == "protected"
ok_equip = by_idx[225601]["skip_reason"] == "is_equip"
ok_lock = by_idx[111029]["skip_reason"] == "is_locked"
ok_pos = by_idx[108477]["skip_reason"].startswith("pos_not_bag")
ok_nometa = by_idx[999999]["skip_reason"] == "no_meta"
ok_notfound = by_idx[777777]["skip_reason"] == "not_found"
check("选品: 正常命中", ok_pick, by_idx[170001])
check("选品: 保护跳过", ok_prot, by_idx[102007])
check("选品: 装备跳过", ok_equip, by_idx[225601])
check("选品: 锁定跳过", ok_lock, by_idx[111029])
check("选品: 非背包位跳过", ok_pos, by_idx[108477])
check("选品: 无 meta 保守跳过", ok_nometa, by_idx[999999])
check("选品: 不在背包 → not_found", ok_notfound, by_idx[777777])

# ================================================================ 9) S2C 边界
rm_plan()
write_plan(base_plan())
ro = FakeRobot()
t = 60000
booth.tick(ro, t)
b = ro.m_booth
# 他人开摊不应置 open_ok
booth.on_seller_start_booth(ro, [99001, 1, "别人的摊", 1, 1])
check("S2C: 他人开摊不置 open_ok", not b.open_ok)
# 非激活号收到 S2C：静默
ro2 = FakeRobot(acct="robot0009999@xy3.com")
booth.on_up_item(ro2, [1, 2])
booth.on_sold_item(ro2, [1, "x", 2, 3, ""])
check("S2C: 非激活号静默 (无对象无异常)", ro2.m_booth is None)
# 90059 不匹配 pending
b.up_pending = {"item_id": 888, "name": "x", "item_index": 1}
booth.on_up_item(ro, [777, 10])
check("S2C: 90059 不匹配 → pending 保留", b.up_pending is not None and b.up_pending["item_id"] == 888)
b.up_pending = None
# notice 非激活不记
n_before = len([x for x in _diag_lines if "NOTICE" in x])
booth.on_notice(ro2, "摆摊成功")     # ro2 无 m_booth
check("S2C: notice 非激活不记录",
      len([x for x in _diag_lines if "NOTICE" in x]) == n_before)
# 数字码形态的 notice（如 [1191, []] = 此地禁止摆摊）也要记录（曾被关键词过滤漏记）
n0 = len([x for x in _diag_lines if x.startswith("BOOTH_SELFTEST NOTICE")])
booth.on_notice(ro, "[1191, []]")    # ro 激活中（b.enabled=True）
n1 = len([x for x in _diag_lines if x.startswith("BOOTH_SELFTEST NOTICE")])
check("S2C: 数字码 notice 也记录(已去关键词过滤)",
      n1 == n0 + 1 and "[1191" in _diag_lines[-1], _diag_lines[-1] if _diag_lines else "")

# ================================================================ 10) 激活中撤计划 → DISABLED
rm_plan()
write_plan(base_plan())
ro = FakeRobot(mapid=11, pos=(1409, 1109))
t = 70000
booth.tick(ro, t)                    # INIT→GOTO
t += 1000
booth.tick(ro, t)                    # →OPEN
b = ro.m_booth
check("撤计划前: 激活中", b.enabled and b.state == "OPEN", b.state)
rm_plan()
booth._PLAN_CACHE["ms"] = 0
t += 1000
booth.tick(ro, t)
check("撤计划: →DISABLED 且不再动作", b.state == "DISABLED" and not b.enabled, b.state)

# ================================================================ 10b) 游荡占位
rm_plan()
write_plan(base_plan())
ro = FakeRobot(mapid=11, pos=(1409, 1109))
t = 75000
booth.tick(ro, t)                    # 激活
check("占位: 激活后 m_collect_walk 是 dummy(enabled)",
      ro.m_collect_walk is not None and ro.m_collect_walk.enabled is True
      and ro.m_collect_walk.mapid == 11 and ro.m_booth.walk_dummy, ro.m_collect_walk)
t += 1000
booth.tick(ro, t)                    # → GOTO
t += 1000
booth.tick(ro, t)                    # → OPEN
rm_plan()
booth._PLAN_CACHE["ms"] = 0
t += 1000
booth.tick(ro, t)                    # 撤计划 → DISABLED → 恢复占位
check("占位: 停用后恢复原值(None)", ro.m_collect_walk is None,
      ro.m_collect_walk)

# 原有游荡对象(enabled) → 不抢占
rm_plan()
write_plan(base_plan())
ro = FakeRobot(mapid=11, pos=(1409, 1109))
old_walk = types.SimpleNamespace(enabled=True, mapid=38, state="roam")
ro.m_collect_walk = old_walk
t = 76000
booth.tick(ro, t)
check("占位: 已在游荡则不抢占", ro.m_collect_walk is old_walk
      and ro.m_booth.walk_dummy is False)

# 残留禁用对象 → 占位替换, 停用恢复
rm_plan()
write_plan(base_plan())
ro = FakeRobot(mapid=11, pos=(1409, 1109))
old_dead = types.SimpleNamespace(enabled=False, mapid=38, state="done")
ro.m_collect_walk = old_dead
t = 77000
booth.tick(ro, t)
check("占位: 残留禁用对象被替换为 dummy",
      isinstance(ro.m_collect_walk, booth._DummyCollect) and ro.m_booth.walk_dummy)
rm_plan()
booth._PLAN_CACHE["ms"] = 0
t += 1000
booth.tick(ro, t)
check("占位: 停用恢复为残留对象", ro.m_collect_walk is old_dead, ro.m_collect_walk)

# 上一轮占位的残留（跨 reload 换类：isinstance 失效 → 必须靠标记识别）——
# 不能被当"真实游荡"保留, 停用后必须恢复为 None（防残留被 random_walk 消费：生产事故）
rm_plan()
write_plan(base_plan())
ro = FakeRobot(mapid=11, pos=(1409, 1109))
leftover = types.SimpleNamespace(_booth_dummy=True, enabled=True, mapid=11, state="booth")
ro.m_collect_walk = leftover
t = 78000
booth.tick(ro, t)
check("占位: 残留占位按标记识别替换(walk_saved=None)",
      ro.m_booth.walk_dummy and ro.m_booth.walk_saved is None
      and getattr(ro.m_collect_walk, "_booth_dummy", False) is True, ro.m_collect_walk)
rm_plan()
booth._PLAN_CACHE["ms"] = 0
t += 1000
booth.tick(ro, t)
check("占位: 停用后不留残留（恢复 None）", ro.m_collect_walk is None, ro.m_collect_walk)

# ================================================================ 11) dispatch_cmd
rm_plan()
ro = FakeRobot()
r = booth.dispatch_cmd(ro, {"cmd": "booth_status"})
check("dispatch: status(未激活)", r and r["ok"] and r["enabled"] is False, r)
r = booth.dispatch_cmd(ro, {"cmd": "booth_start", "mapid": 11, "hold_sec": 1,
                            "up_items": [{"item_index": 170001, "price": 100}]})
check("dispatch: start 激活", r and r["ok"] and ro.m_booth.enabled, r)
r = booth.dispatch_cmd(ro, {"cmd": "booth_stop"})
check("dispatch: stop 停用", r and r["ok"] and not ro.m_booth.enabled, r)
r = booth.dispatch_cmd(ro, {"cmd": "booth_status"})
check("dispatch: status(已停)", r and r["state"] == "DISABLED", r)

# ================================================================ 12) 心跳字段
ro = FakeRobot()
set_bag(ro, [(170001, 111, 5, 8192, None)])
write_plan(base_plan())
t = 80000
booth.tick(ro, t)
b = ro.m_booth
check("心跳: 字段齐 (enabled/state/sold_count/income/name)",
      all(hasattr(b, k) for k in ("enabled", "state", "sold_count", "income", "name")))
check("心跳: 名字已冻结", b.name == "杂货小摊", b.name)

# ================================================================ 汇总
shutil.rmtree(_tmpdir, ignore_errors=True)
print("")
print("=" * 60)
print("booth_selftest: %d/%d PASS, %d FAIL" % (total - len(fails), total, len(fails)))
if fails:
    print("FAIL 列表:")
    for f in fails:
        print("  - " + f)
sys.exit(1 if fails else 0)
