# -*- coding: utf-8 -*-
"""孵化链路(坐骑蛋/元气蛋)自检（2026-09-22 交付）。

覆盖（源码级：直接 import 真实 bag_ops / mount_egg，注入最小 stub 驱动）：
  1) 蛋种判据: 坐骑蛋(元数据 related_element_index 前缀 / element_name 关键字 / 名字兜底,
     含 101317-101319 的"白羊羊 vs 白色羊驼"双份 xml 兼容)、元气蛋(编号 101340/102603、
     名字兜底)、非蛋不误判;
  2) 位置判据: 坐骑蛋=装备栏五行珠槽 4108, 元气蛋=背包区, 交叉/未知一律 False;
  3) 保护集合: is_protected_item 覆盖所有蛋(含装备栏里的), 普通物品不受影响;
     另静态断言 bag_ops 的丢弃/右键使用两条路径确实调了保护/跳过判据;
  4) 放蛋: place_mount_egg 发 C2S_CHANGE_ITEM_POCKET(80032) [id,1,BAG,EQUIP,4108],
     等位置回执(wait_pos→ok), 超时报 place_failed, 元气蛋不移动;
  5) battles 只在目标图内战斗胜利累加(异图/阵亡不算);
  6) notice: 818/819 → hatched, tick 收工 reason=hatched;
  7) 超时 → reason=timeout; 
  8) 心跳契约字段齐全 + 重复下发幂等(refreshed, 不重发协议);
  9) 挂钩静态断言: msghandle 路由 90211→mount_egg、fight_tester/robot_operator 消费标记、
     client.py 命令分发与未登录缓冲、状态上报;
  10) 反例: 旧(单份)名字名单 → 兜底失配; msghandle 删掉路由 → 检查函数报 FAIL
     (证明本自检对这两处修复有敏感度)。

用法：python hatch_selftest.py [script 目录]
  不带参数默认校验仓库副本；传线上副本 script 目录可再验一次线上文件。
"""
import os
import re
import sys
import types
import traceback

try:
	sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
	pass

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_DIR = os.path.normpath(os.path.join(
	HERE, "..", "deploy", "zones", "prod-240-2300", "script"))
SCRIPT_DIR = os.path.normpath(sys.argv[1] if len(sys.argv) > 1 else DEFAULT_DIR)

fails = 0
total = 0


def check(name, ok, detail=""):
	global fails, total
	total += 1
	fails += 0 if ok else 1
	print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
		("  " + detail) if detail else ""))


def read_src(fname):
	path = os.path.join(SCRIPT_DIR, fname)
	with open(path, encoding="utf-8", errors="replace") as f:
		return f.read()


# ---------------------------------------------------------------- 最小 stub
# 这些模块在生产端由机器人运行时提供(C 扩展/网络/运行时对象)，离线 import 会失败，
# 这里只补"被测模块真正用到"的最小面。bag_ops 与 mount_egg 本体是**真实源码**。
PROTO_CHANG_POCKET = 80032		# protocol2/c2s_key.py: C2S_CHANGE_ITEM_POCKET = 80032

p3 = types.ModuleType("protocol3")
p3.C2S_CHANG_POCKET = PROTO_CHANG_POCKET
sys.modules["protocol3"] = p3

ITEM_DATA = {
	101316: {"related_element_index": "mount_birth_element_00001", "element_name": "需要孵化的坐骑step", "item_name": "青牛"},
	101317: {"related_element_index": "mount_birth_element_00001", "element_name": "需要孵化的坐骑step", "item_name": "白羊羊"},
	101318: {"related_element_index": "mount_birth_element_00001", "element_name": "需要孵化的坐骑step", "item_name": "醉羊羊"},
	101319: {"related_element_index": "mount_birth_element_00001", "element_name": "需要孵化的坐骑step", "item_name": "酷羊羊"},
	102603: {"related_element_index": None, "element_name": None, "item_name": "元气蛋"},
	101340: {"related_element_index": None, "element_name": None, "item_name": "元气蛋"},
	225601: {"related_element_index": "element_neck_a_lv0", "element_name": "项链", "item_name": "贝壳项链"},
	102007: {"related_element_index": None, "element_name": None, "item_name": "金创药"},
	999999: {"related_element_index": None, "element_name": None, "item_name": "白色羊驼"},
}
cl = types.ModuleType("client")
cl.g_item_data_dict = ITEM_DATA
sys.modules["client"] = cl

DIAG = []
dg = types.ModuleType("diag")
dg.log = lambda msg: DIAG.append(msg)
sys.modules["diag"] = dg

RW_CALLS = []
rw = types.ModuleType("random_walk")


def _rw_dispatch(robot_object, cmd):
	RW_CALLS.append(cmd)
	return {"cmd": cmd.get("cmd"), "result": "ok"}


rw.dispatch_cmd = _rw_dispatch
sys.modules["random_walk"] = rw

QE_EVENTS = []
qe = types.ModuleType("quest_engine")


def _qe_emit(robot_object, ev):
	QE_EVENTS.append(ev)


def _qe_get_quest(robot_object, create=False):
	raise RuntimeError("stub: 无任务上下文")


qe.__emit = _qe_emit
qe.get_quest = _qe_get_quest
sys.modules["quest_engine"] = qe

sys.dont_write_bytecode = True	# 不写线上目录的 __pycache__（自检不污染部署）
sys.path.insert(0, SCRIPT_DIR)
import bag_ops			# noqa: E402
import mount_egg		# noqa: E402


class StubRobot(object):
	"""机器人对象替身：只带被测代码真实用到的字段/方法。"""

	def __init__(self, bag=None, mapid=6, level=45):
		self.m_bag_cache = bag if bag is not None else {}
		self.m_mapid = mapid
		self.m_logined = True
		self.m_level = level
		self.m_cur_hp = 100
		self.m_max_hp = 100
		self.m_pose = [100, 100]
		self.m_account = ["t@x.com"]
		self.sent = []

	def send_message(self, proto, args):
		self.sent.append((proto, args))
		return 99


def new_hatch(active=True, kind="mount", mapid=6, egg_item=101316, now=10 ** 12):
	h = mount_egg.HatchState()
	h.active = active
	h.kind = kind
	h.mapid = mapid
	h.egg_item = egg_item
	h.since_ms = now
	h.deadline_ms = now + 3600 * 1000
	h.place_state = "ok"
	h.arrived = True
	return h


NOW = 10 ** 12

# ---------------------------------------------------------------- ① 蛋种判据
check("①蛋种-坐骑蛋元数据(related_element_index 前缀)",
	bag_ops.egg_kind_of(101316, ITEM_DATA[101316]) == "mount")
check("①蛋种-坐骑蛋元数据(element_name 关键字兜底)",
	bag_ops.egg_kind_of(555001, {"related_element_index": None, "element_name": "需要孵化的坐骑step", "item_name": "?"}) == "mount"
	and bag_ops.egg_kind_of(555002, {"related_element_index": "", "element_name": "", "item_name": "白色羊驼"}) == "mount")
check("①蛋种-名字兜底双份xml兼容(白羊羊/白色羊驼 都可认)",
	bag_ops.egg_kind_of(101317, ITEM_DATA[101317]) == "mount"
	and bag_ops.egg_kind_of(101317, {"related_element_index": None, "element_name": None, "item_name": "白色羊驼"}) == "mount"
	and bag_ops.egg_kind_of(909001, {"item_name": "白羊羊"}) == "mount"
	and bag_ops.egg_kind_of(909002, {"item_name": "白色羊驼"}) == "mount")
check("①蛋种-元气蛋编号/名字",
	bag_ops.egg_kind(101340) == "guardian" and bag_ops.egg_kind(102603) == "guardian"
	and bag_ops.egg_kind_of(777001, {"item_name": "元气蛋"}) == "guardian"
	and bag_ops.egg_kind_of(777002, {"item_name": "守护蛋"}) == "guardian")
check("①蛋种-非蛋不误判",
	bag_ops.egg_kind(225601) is None and bag_ops.egg_kind_of(225601, ITEM_DATA[225601]) is None
	and bag_ops.egg_kind_of(885001, {"related_element_index": "element_weapon_a_lv3", "item_name": "惊鸿剑"}) is None
	and bag_ops.is_egg_item(102007) is False)

# ---------------------------------------------------------------- ② 位置判据
check("②位置-坐骑蛋=4108 / 元气蛋=背包 / 交叉与未知 False",
	bag_ops.egg_pos_ok("mount", 4108) and not bag_ops.egg_pos_ok("mount", 8192)
	and bag_ops.egg_pos_ok("guardian", 8192) and not bag_ops.egg_pos_ok("guardian", 4108)
	and not bag_ops.egg_pos_ok("mount", None) and not bag_ops.egg_pos_ok("guardian", True))

# ---------------------------------------------------------------- ③ 保护集合
check("③保护-蛋(两种, 含装备栏)与清单物品受保护, 普通物品不受影响",
	bag_ops.is_protected_item(101316) and bag_ops.is_protected_item(102603)
	and bag_ops.is_protected_item(101317) and bag_ops.is_protected_item(190146)
	and bag_ops.is_protected_item(102007) and not bag_ops.is_protected_item(225601)
	and not bag_ops.is_protected_item(110173))
_src_bag = read_src("bag_ops.py")
check("③保护-静态: 丢弃路径(auto_equip_best_items)调用 is_protected_item",
	"is_protected_item(_it_idx)" in _src_bag)
check("③保护-静态: 右键使用路径(auto_open_gifts)跳过蛋(is_egg_item)",
	"if is_egg_item(item_index):" in _src_bag)

# ---------------------------------------------------------------- ④ 放蛋
ro = StubRobot(bag={101316: [5001, 1, 8192]})
reply = mount_egg.dispatch_cmd(ro, {"cmd": "hatch_start", "kind": "mount", "mapid": 6,
	"egg_item": 101316, "max_minutes": 5})
h = getattr(ro, "m_hatch", None)
check("④放蛋-坐骑蛋在背包: 发换包裹协议 [id,1,BAG,EQUIP,4108] 并等回执",
	reply.get("result") == "ok" and reply.get("place") == "wait_pos"
	and ro.sent and ro.sent[-1] == (PROTO_CHANG_POCKET, [5001, 1, 5158, 5170, 4108])
	and h is not None and h.place_state == "wait_pos")

# 位置回执：蛋出现在 4108 → tick 置 ok
ro.m_bag_cache[101316] = [5001, 1, 4108]
mount_egg.tick(ro, NOW + 1000)
check("④放蛋-收到位置回执(蛋落到 4108) → place_state=ok",
	getattr(ro, "m_hatch").place_state == "ok")

# 超时未见回执 → place_failed
ro2 = StubRobot(bag={101316: [5002, 1, 8192]})
mount_egg.dispatch_cmd(ro2, {"cmd": "hatch_start", "kind": "mount", "mapid": 6,
	"egg_item": 101316, "max_minutes": 5})
h2 = ro2.m_hatch
mount_egg.tick(ro2, h2.place_deadline_ms + 1)
check("④放蛋-超时未见回执 → 结束 reason=place_failed",
	h2 is not None and not h2.active and h2.reason == "place_failed")

# 元气蛋: 在背包不移动
ro3 = StubRobot(bag={102603: [6001, 1, 8192]})
reply3 = mount_egg.dispatch_cmd(ro3, {"cmd": "hatch_start", "kind": "guardian", "mapid": 10,
	"egg_item": 102603, "max_minutes": 30})
check("④放蛋-元气蛋在背包: 不发换包裹协议, place=ok",
	reply3.get("result") == "ok" and reply3.get("place") == "ok" and not ro3.sent)

# 没有蛋 → no_egg
ro4 = StubRobot(bag={})
reply4 = mount_egg.dispatch_cmd(ro4, {"cmd": "hatch_start", "kind": "mount", "mapid": 6,
	"egg_item": 101316, "max_minutes": 5})
check("④放蛋-没有蛋 → 回执 error reason=no_egg",
	reply4.get("result") == "error" and reply4.get("reason") == "no_egg")

# ---------------------------------------------------------------- ⑤ battles
ro5 = StubRobot(bag={101316: [5001, 1, 4108]}, mapid=6)
ro5.m_hatch = new_hatch(now=NOW)
mount_egg.on_fight_end(ro5)
ro5.m_mapid = 7
mount_egg.on_fight_end(ro5)			# 异图不算
ro5.m_mapid = 6
ro5.m_cur_hp = 0
mount_egg.on_fight_end(ro5)			# 阵亡不算
check("⑤battles-只在目标图内胜利累加(异图/阵亡不算)",
	ro5.m_hatch.battles == 1)

# ---------------------------------------------------------------- ⑥ notice
ro6 = StubRobot(bag={101316: [5001, 1, 4108]}, mapid=6)
ro6.m_hatch = new_hatch(now=NOW)
mount_egg.on_notice_args(ro6, [818, [("青牛",), ("青牛",)]])
hatched_ok = ro6.m_hatch.hatched
RW_CALLS[:] = []
mount_egg.tick(ro6, NOW + 1)
check("⑥notice-818 → hatched, tick 收工 reason=hatched 且停游荡",
	hatched_ok and not ro6.m_hatch.active and ro6.m_hatch.reason == "hatched"
	and any(c.get("cmd") == "random_walk_stop" for c in RW_CALLS))

ro6b = StubRobot(bag={101316: [5001, 1, 4108]}, mapid=6)
ro6b.m_hatch = new_hatch(now=NOW)
mount_egg.on_notice_args(ro6b, [819, [("青牛",)]])
check("⑥notice-819(槽满, 蛋内绑定) 也置 hatched",
	ro6b.m_hatch.hatched is True)

ro6c = StubRobot(bag={102603: [6001, 1, 8192]}, mapid=10)
ro6c.m_hatch = new_hatch(kind="guardian", mapid=10, egg_item=102603, now=NOW)
mount_egg.on_notice_args(ro6c, [785, [("137",)]])
mount_egg.on_notice_args(ro6c, [784, []])
check("⑥notice-785 记元气值 / 784 元气蛋孵出置 hatched",
	ro6c.m_hatch.pneuma == 137 and ro6c.m_hatch.hatched is True)

# ---------------------------------------------------------------- ⑦ 超时
ro7 = StubRobot(bag={101316: [5001, 1, 4108]}, mapid=6)
h7 = new_hatch(now=NOW)
h7.deadline_ms = NOW - 1
ro7.m_hatch = h7
mount_egg.tick(ro7, NOW)
check("⑦超时-max_minutes 到点 → 结束 reason=timeout",
	not h7.active and h7.reason == "timeout")

# ---------------------------------------------------------------- ⑧ 契约/幂等
st = mount_egg.status_dict(ro4)
CONTRACT = ("active", "kind", "egg_item", "mapid", "battles", "hatched", "reason", "since_ms")
check("⑧契约-心跳 hatch 字段齐全且未参与孵化给全零/空",
	all(k in st for k in CONTRACT) and st["active"] is False and st["battles"] == 0)
check("⑧契约-reason 枚举与 notice 常量和契约一致",
	mount_egg.REASON_HATCHED == "hatched" and mount_egg.REASON_TIMEOUT == "timeout"
	and mount_egg.REASON_LEFT_MAP == "left_map" and mount_egg.REASON_STOPPED == "stopped"
	and mount_egg.REASON_NO_EGG == "no_egg" and mount_egg.REASON_PLACE_FAILED == "place_failed"
	and (mount_egg.NOTICE_MOUNT_EGG_HATCH, mount_egg.NOTICE_MOUNT_EGG_HATCH_FULL,
		mount_egg.NOTICE_MOUNT_EGG_HATCH_MAY, mount_egg.NOTICE_EGG_HATCH_SUMMON_FULL,
		mount_egg.NOTICE_EGG_ADD_PNEUMA_TIPS, mount_egg.NOTICE_EGG_HATCH_NORMAL_SUMMON,
		mount_egg.NOTICE_EGG_ADD_PNEUMA) == (818, 819, 820, 782, 783, 784, 785))

ro8 = StubRobot(bag={101316: [5001, 1, 8192]})
r8a = mount_egg.dispatch_cmd(ro8, {"cmd": "hatch_start", "kind": "mount", "mapid": 6,
	"egg_item": 101316, "max_minutes": 5})
r8b = mount_egg.dispatch_cmd(ro8, {"cmd": "hatch_start", "kind": "mount", "mapid": 6,
	"egg_item": 101316, "max_minutes": 5})
n_proto = len([c for c in ro8.sent if c[0] == PROTO_CHANG_POCKET])
check("⑧幂等-重复 hatch_start 只回执 refreshed, 不重发放蛋协议",
	r8a.get("result") == "ok" and r8b.get("refreshed") is True and n_proto == 1)

# ---------------------------------------------------------------- ⑨ 挂钩静态断言
src_msgh = read_src("msghandle.py")
check("⑨挂钩-msghandle 90211 路由到 mount_egg.on_notice_args",
	"mount_egg.on_notice_args" in src_msgh and "import mount_egg" in src_msgh)
src_ft = read_src("fight_tester.py")
check("⑨挂钩-fight_tester 一场只结算一次(m_hatch_fight_open 置位+消费)",
	"m_hatch_fight_open = True" in src_ft and "consume_hatch_fight_end(robot_object)" in src_ft)
src_op = read_src("robot_operator.py")
check("⑨挂钩-robot_operator S2C_PLAYER_STOP_FIGHTING 也消费标记",
	"fight_tester.consume_hatch_fight_end(robot_object)" in src_op)
src_cli = read_src("client.py")
check("⑨挂钩-client 分发 hatch_* + 未登录缓冲 + 状态上报",
	'("hatch_start", "hatch_stop", "hatch_status")' in src_cli
	and 'startswith("hatch_")' in src_cli
	and "g_pending_cmds.setdefault(ro.m_account[0], []).append(cmd)" in src_cli
	and 'st["hatch"] = _me.status_dict(ro)' in src_cli)
src_me = read_src("mount_egg.py")
check("⑨挂钩-mount_egg 调 place_mount_egg / egg_pos_ok, 且 end 时停游荡",
	"bag_ops.place_mount_egg(robot_object" in src_me
	and 'bag_ops.egg_pos_ok(h.kind, egg["pos"])' in src_me
	and '"cmd": "random_walk_stop"' in src_me)

# ---------------------------------------------------------------- ⑩ 反例(证明敏感度)
# 反例A: 只收"服务端名"的旧名单 → 机器人端心跳名(白羊羊)兜底失配
_bagsrc = read_src("bag_ops.py")
_stripped = _bagsrc.replace('"白羊羊", "醉羊羊", "酷羊羊", ', "")
_ns = {"__name__": "bag_ops_oldname"}
exec(compile(_stripped, "<bag_ops old name list>", "exec"), _ns)
_old_kind = _ns["egg_kind_of"](101317, {"related_element_index": None, "element_name": None, "item_name": "白羊羊"})
check("⑩反例-名单若不含机器人端名(白羊羊) → 兜底失配(演示修正必要性)",
	_old_kind is None)

# 反例B: msghandle 删掉路由 → 静态检查失败
_route_pat = re.compile(r"import mount_egg\s*\n\s*mount_egg\.on_notice_args")
check("⑩反例-msghandle 删掉路由后检查函数会报 FAIL",
	not _route_pat.search(src_msgh.replace("mount_egg.on_notice_args", "pass"))
	and bool(_route_pat.search(src_msgh)))

print()
print("脚本目录: %s" % SCRIPT_DIR)
print("=== 结果: %s (共 %d 项断言, 失败 %d 项) ===" % (
	"PASS" if fails == 0 else "FAIL", total, fails))
sys.exit(1 if fails else 0)
