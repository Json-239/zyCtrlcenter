# -*- coding: utf-8 -*-
"""每日/登陆/在线/等级奖励补领自检（2026-09-28）。

覆盖：
  ① 静态: 4 accept + 4 界面 C2S 已注册 FORMAT_MC; 4 界面 S2C 已注册 FORMAT_MS 且路由到
     encourage_claim; daily_ghost 两个钩子(tick 顶部 / on_notice 顶部)文本在位;
     双副本 encourage_claim/protocol3/daily_ghost 字节一致(生产副本存在时)。
  ② 纯函数: 在线 index 解析 / 等待表(1..16) / 查询节奏。
  ③ 动态(stub 驱动 + 假时钟):
     - 未登录: 一个包都不发;
     - 登录后 20s: 先发在线界面查询；回执 index → 到点发在线 accept; 成功回执 index 前进
       → 按等待表(60s)排下一格; 到点再 accept;
     - 每日/登陆/等级: 15 分钟前不发查询/accept; 到点查询 → can=1 才 accept;
       accept 回执 can=2 → 记"今日已领"+落盘;
     - 1416 → 在线退避阶梯(60s)且退避内不再发;
     - 1439 → 每日/登陆/等级 15 分钟门槛重臂(在线不受影响);
     - 储备金箱开箱: 领到后 5s 用 C2S_USEITEM(实例 id)开箱, 单轮 ≤8;
     - 在线 index 清空 → done 且落盘;
     - 反例: 全部发包 ⊆ {8 个 encourage 协议, C2S_USEITEM}; 不碰其它协议;
     - 反例: 协议未注册(热更未重启) → tick/on_notice 全静默零发包。
用法：python tools/encourage_selftest.py
"""
import os
import sys
import tempfile

try:
	sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
	pass

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.normpath(os.path.join(HERE, ".."))
SCRIPT = os.path.normpath(os.path.join(ROOT, "deploy", "zones", "prod-240-2300", "script"))
PROD_COPY = "F:/ZyBin/xm/2d-xiyou-server/robot/deploy/single_robot_zy/script"
sys.path.insert(0, SCRIPT)

from unittest.mock import MagicMock  # noqa: E402

sys.modules.setdefault("cnetwork", MagicMock(name="cnetwork"))

fails = 0
total = 0


def check(name, ok, detail=""):
	global fails, total
	total += 1
	fails += 0 if ok else 1
	print("[%s] %s%s" % ("PASS" if ok else "FAIL", name, (" — " + detail) if detail else ""))


def rd(path):
	with open(path, "r", encoding="utf-8") as f:
		return f.read()


def rd_b(path):
	with open(path, "rb") as f:
		return f.read()


# 状态目录: 指向临时目录, 不污染生产 state/
_TMP = tempfile.mkdtemp(prefix="enc_selftest_")
os.environ["ZCC_ENCOURAGE_STATE_DIR"] = _TMP

import protocol3  # noqa: E402
import encourage_claim as ec  # noqa: E402
from protocol2 import c2s_key as ck  # noqa: E402
from protocol2 import s2c_key as sk  # noqa: E402

NEED_C2S = [ck.C2S_ENCOURAGE_EVERYDAY_INTERFACE, ck.C2S_ACCEPT_EVERYDAY_ENCOURAGE,
	ck.C2S_ENCOURAGE_LOGIN_INTERFACE, ck.C2S_ACCEPT_LOGIN_ENCOURAGE,
	ck.C2S_ENCOURAGE_ONLINE_INTERFACE, ck.C2S_ACCEPT_ONLINE_ENCOURAGE,
	ck.C2S_ENCOURAGE_LEVEL_INTERFACE, ck.C2S_ACCEPT_LEVEL_ENCOURAGE]
NEED_S2C = [sk.S2C_ENCOURAGE_EVERYDAY_INTERFACE, sk.S2C_ENCOURAGE_LOGIN_INTERFACE,
	sk.S2C_ENCOURAGE_ONLINE_INTERFACE, sk.S2C_ENCOURAGE_LEVEL_INTERFACE]

# ================================================================ ① 静态
_missing_mc = [k for k in NEED_C2S if k not in protocol3.FORMAT_MC]
check("静态: 8 个 encourage C2S 均注册 FORMAT_MC", not _missing_mc, repr(_missing_mc))
_missing_ms = [k for k in NEED_S2C if k not in protocol3.FORMAT_MS]
check("静态: 4 个界面 S2C 均注册 FORMAT_MS", not _missing_ms, repr(_missing_ms))
_route_ok = True
_route_detail = []
for _k, _fn in ((NEED_S2C[0], "on_everyday_interface"), (NEED_S2C[1], "on_login_interface"),
		(NEED_S2C[2], "on_online_interface"), (NEED_S2C[3], "on_level_interface")):
	_h = protocol3.g_handle_map.get(_k)
	if _h is None or getattr(_h, "__name__", "") != _fn:
		_route_ok = False
		_route_detail.append("%s→%s" % (_k, getattr(_h, "__name__", _h)))
check("静态: 4 个界面 S2C 路由到 encourage_claim 处理器", _route_ok, repr(_route_detail))
check("静态: accept 无参数格式([])", all(protocol3.FORMAT_MC[k] == [] for k in NEED_C2S if k in (80040, 80286, 80171, 80255)),
	repr([protocol3.FORMAT_MC.get(k) for k in (80040, 80286, 80171, 80255)]))
check("静态: 回执格式 [ '', 1 ] / [ '' ]", (
	protocol3.FORMAT_MS.get(sk.S2C_ENCOURAGE_EVERYDAY_INTERFACE) == ["", 1]
	and protocol3.FORMAT_MS.get(sk.S2C_ENCOURAGE_ONLINE_INTERFACE) == [""]),
	repr(protocol3.FORMAT_MS.get(sk.S2C_ENCOURAGE_EVERYDAY_INTERFACE)) + "/" +
	repr(protocol3.FORMAT_MS.get(sk.S2C_ENCOURAGE_ONLINE_INTERFACE)))

_dg_src = rd(os.path.join(SCRIPT, "daily_ghost.py"))
check("静态: daily_ghost.tick 顶部含 encourage_claim.tick 钩子", "encourage_claim.tick(robot_object, now)" in _dg_src)
check("静态: daily_ghost.on_notice 顶部含 encourage_claim.on_notice 钩子", "encourage_claim.on_notice(robot_object, datalist)" in _dg_src)
check("静态: 钩子在 g.enabled 早退之前(tick)",
	_dg_src.find("encourage_claim.tick(robot_object, now)") < _dg_src.find('g = getattr(robot_object, "m_ghost", None)\n\tif g is None or not g.enabled:\n\t\treturn 0'),
	"")
if os.path.isdir(PROD_COPY):
	_same = True
	_diff = []
	for _f in ("encourage_claim.py", "protocol3.py", "daily_ghost.py"):
		_a = os.path.join(SCRIPT, _f)
		_b = os.path.join(PROD_COPY, _f)
		if (not os.path.exists(_b)) or rd_b(_a) != rd_b(_b):
			_same = False
			_diff.append(_f)
	check("静态: 双副本一致(encourage_claim/protocol3/daily_ghost)", _same, repr(_diff))
else:
	check("静态: 双副本一致(生产副本不存在, 跳过)", True, "skip")

# ================================================================ ② 纯函数
check("纯: parse_online_index 正常/边界", (
	ec.parse_online_index("encourage_online1") == 1
	and ec.parse_online_index("encourage_online16") == 16
	and ec.parse_online_index("encourage_online17") == 0
	and ec.parse_online_index("") == 0
	and ec.parse_online_index(None) == 0))
_wait_ok = all(ec.online_wait_after(_i) == _e for _i, _e in (
	(1, 60), (2, 120), (3, 180), (4, 240), (5, 300), (6, 420), (7, 540), (8, 720),
	(9, 900), (10, 900), (11, 900), (12, 900), (13, 900), (14, 900), (15, 900), (16, 0)))
check("纯: 在线等待表与 online_encourage.xml 一致(1..16)", _wait_ok)
check("纯: 查询节奏 每日/登陆=30min, 等级=2h", (
	ec._gap_ms("everyday") == 30 * 60 * 1000 and ec._gap_ms("login") == 30 * 60 * 1000
	and ec._gap_ms("level") == 2 * 60 * 60 * 1000))


# ================================================================ ③ 动态 stub
class StubQuest(object):
	def __init__(self):
		# 与 quest_engine 同构: key=item_index, value=[实例item_id, count]
		self.bag = {101079: [990000000001, 16], 190146: [990000000002, 3], 102007: [990000000003, 5]}


ALL_SENT = []				# 全会话累计(反例白名单用)


class StubRobot(object):
	def __init__(self):
		self.m_logined = True
		self.m_fd = 7
		self.m_account = ["qa_enc@xy3.com"]
		self.m_quest = StubQuest()
		self.sent = []

	def send_message(self, msgid, args):
		self.sent.append((msgid, list(args)))
		ALL_SENT.append((msgid, list(args)))
		return 0


CLOCK = {"now": 1700000000000}
ec._now_ms = lambda: CLOCK["now"]


def adv(ms):
	CLOCK["now"] += int(ms)


R_HOLDER = [None]
REPORTS = []
ec._log = lambda ro, level, msg: None
ec._report = lambda ro, msg: REPORTS.append(msg)
# S2C 处理器按 fd 取 robot: 自检环境用 stub 顶替 robot_mgr 查找
ec._robot_by_fd = lambda fd: R_HOLDER[0]
ec._READY_CACHE[0] = False
ec._READY_CACHE[1] = 0

# 清临时目录里可能的历史盘
for _f in os.listdir(_TMP):
	try:
		os.remove(os.path.join(_TMP, _f))
	except Exception:
		pass

R = StubRobot()
R_HOLDER[0] = R
ACC_EVERYDAY, ACC_LOGIN, ACC_ONLINE, ACC_LEVEL = 80040, 80286, 80171, 80255
IFC_EVERYDAY, IFC_LOGIN, IFC_ONLINE, IFC_LEVEL = 80315, 80275, 80287, 80150
USE_ITEM = 80218
ALLOWED = set([ACC_EVERYDAY, ACC_LOGIN, ACC_ONLINE, ACC_LEVEL,
	IFC_EVERYDAY, IFC_LOGIN, IFC_ONLINE, IFC_LEVEL, USE_ITEM])


def ids_sent(rb=None):
	rb = rb or R
	return [m for m, _a in rb.sent]


def reset_sent(rb=None):
	rb = rb or R
	rb.sent = []


# --- ③-1 未登录: 零发包
R.m_logined = False
reset_sent()
ec.tick(R)
check("动态: 未登录 tick 零发包", ids_sent() == [], repr(ids_sent()))
R.m_logined = True

# --- ③-2 登录后 20s: 先发在线界面查询(在线不受 15 分钟限制)
reset_sent()
ec.tick(R)					# 会话初始化
check("动态: 首个 tick 不发包(等在线上线缓冲)", ids_sent() == [])
check("动态: 15 分钟前不发每日/登陆/等级查询", IFC_EVERYDAY not in ids_sent() and IFC_LOGIN not in ids_sent() and IFC_LEVEL not in ids_sent())
adv(21000)
ec.tick(R)
check("动态: 上线 ~20s 发在线界面查询(80287)", ids_sent() == [IFC_ONLINE], repr(ids_sent()))

# 回执: 在线 index=encourage_online1 → 5 秒后尝试 accept
reset_sent()
ec.on_online_interface(7, ["encourage_online1"])
h = 0
while ACC_ONLINE not in ids_sent() and h <= 50:
	h += 1
	adv(1000)
	ec.tick(R)
check("动态: 在线查询回执后到点发 accept(80171)", ACC_ONLINE in ids_sent() and h <= 50,
	"h=%d %s" % (h, repr(ids_sent())))

# accept 成功回执: index 前进到 2 → 按等待表 60s 排下一格
reset_sent()
ec.on_online_interface(7, ["encourage_online2"])
_st = R.m_enc_claim["sys"]["online"]
check("动态: 在线第 1 格成功 → 等待 60s(等待表)", abs(int(_st["next_at"]) - (CLOCK["now"] + 60000)) <= 1000,
	repr(_st.get("next_at")))
check("动态: 在线第 1 格成功已上报", any("在线奖励已领取第 1 格" in m for m in REPORTS), repr(REPORTS[-2:]))
check("动态: 在线进度已落盘(online_idx=2)", (
	ec._load_persist(R).get("online_idx") == "encourage_online2"), repr(ec._load_persist(R)))
adv(30000)
reset_sent()
ec.tick(R)
check("动态: 等待期(30s<60s)不重发", ACC_ONLINE not in ids_sent(), repr(ids_sent()))
adv(31000)					# 累计 61s
reset_sent()
ec.tick(R)
check("动态: 满 60s 后发下一格 accept", ACC_ONLINE in ids_sent(), repr(ids_sent()))

# --- ③-3 1416 失败 → 退避阶梯 60s
reset_sent()
ec.on_notice(R, [1416, "你还没到领取时间"])
_st = R.m_enc_claim["sys"]["online"]
check("动态: 1416 → 退避 60s(阶梯第 1 级)", (
	_st["phase"] == "idle" and abs(int(_st["next_at"]) - (CLOCK["now"] + 60000)) <= 1000), repr(_st))
adv(30000)
reset_sent()
ec.tick(R)
check("动态: 退避期内不发 online accept", ACC_ONLINE not in ids_sent(), repr(ids_sent()))

# --- ③-4 15 分钟到点: 每日/登陆/等级查询
adv(1000 * 1000)			# 越过 base_at(990s)
reset_sent()
ec.tick(R)
_sent = ids_sent()
check("动态: 到点后发每日/登陆/等级查询", (
	IFC_EVERYDAY in _sent and IFC_LOGIN in _sent and IFC_LEVEL in _sent), repr(_sent))

# 每日回执 can=1 → 发 accept(80040)
reset_sent()
ec.on_everyday_interface(7, ["encourage_everyday8", 1])
adv(900)					# 回执后留 800ms 观察窗(等失败提示) 再动作
ec.tick(R)
check("动态: 每日 can=1 → 发 accept(80040)", ids_sent() == [ACC_EVERYDAY], repr(ids_sent()))

# 每日 accept 回执 can=2 → 记成功 + 落盘 + 开箱延时
reset_sent()
REPORTS_len = len(REPORTS)
ec.on_everyday_interface(7, ["encourage_everyday8", 2])
check("动态: 每日 accept 回执 can=2 → 上报领取", (
	len(REPORTS) > REPORTS_len or any("每日奖励已领取" in m for m in REPORTS)), repr(REPORTS[-2:]))
check("动态: everyday_date 落盘", ec._load_persist(R).get("everyday_date") == ec._today_str(),
	repr(ec._load_persist(R)))
check("动态: 领到奖励后 5s 开箱延时", int(R.m_enc_claim.get("tidy_at") or 0) - CLOCK["now"] == 5000,
	repr(R.m_enc_claim.get("tidy_at")))

# 开箱: 5s 后 tick → C2S_USEITEM 用 101079 实例 id, 单轮 ≤8
adv(5100)
reset_sent()
ec.tick(R)
_uses = [a for m, a in R.sent if m == USE_ITEM]
check("动态: 延时到 → C2S_USEITEM 开箱(实例 id=990000000001)", (
	len(_uses) == ec.BOX_USE_MAX_PER_ROUND and all(a == [990000000001] for a in _uses)),
	repr(_uses[:3]) + " n=%d" % len(_uses))
check("动态: 只碰储备金箱(未用助战令/金创药)", (
	not any(a == [990000000002] or a == [990000000003] for a in _uses)), repr(_uses))

# 新一轮: 10s 后继续开(16 个分两轮)
_st = R.m_enc_claim
check("动态: 首轮未开完 → 排下一轮(10s)", int(_st.get("tidy_at") or 0) - CLOCK["now"] == 10000,
	repr(_st.get("tidy_at")))
adv(10100)
reset_sent()
ec.tick(R)
check("动态: 第二轮开箱(剩余 8)", len([a for m, a in R.sent if m == USE_ITEM]) == 8,
	repr([m for m, _a in R.sent]))

# --- ③-5 登陆/等级 can=0 不 accept
reset_sent()
ec.on_login_interface(7, ["encourage_login1", 0])
ec.on_level_interface(7, ["encourage_level20", 0])
ec.tick(R)
check("动态: 登陆/等级 can=0 → 不发 accept", (
	ACC_LOGIN not in ids_sent() and ACC_LEVEL not in ids_sent()), repr(ids_sent()))

# --- ③-6 等级 can=1 连领(回执 can=1 → 继续 accept)
reset_sent()
ec.on_level_interface(7, ["encourage_level20", 1])
adv(900)
ec.tick(R)
check("动态: 等级 can=1 → accept(80255)", ids_sent() == [ACC_LEVEL], repr(ids_sent()))
reset_sent()
ec.on_level_interface(7, ["encourage_level25", 1])
adv(1600)					# 连领间隔 1.5s
ec.tick(R)
check("动态: 等级连领(下一档 can=1 → 再 accept)", ids_sent() == [ACC_LEVEL], repr(ids_sent()))

# --- ③-7 1439 → 15 分钟门槛重臂, 在线不受影响
reset_sent()
_before_base = R.m_enc_claim["base_at"]
ec.on_notice(R, [1439, "登陆时间还不足15分钟"])
check("动态: 1439 → base_at 重臂 +990s", (
	int(R.m_enc_claim["base_at"]) - (CLOCK["now"] + 990000) == 0 and
	R.m_enc_claim["base_at"] > _before_base), repr(R.m_enc_claim.get("base_at")))
_st_on = R.m_enc_claim["sys"]["online"]
check("动态: 1439 不推迟在线(在线不受 15 分钟门)", int(_st_on["next_at"]) < int(R.m_enc_claim["base_at"]),
	"%s vs %s" % (_st_on["next_at"], R.m_enc_claim["base_at"]))
reset_sent()
ec.tick(R)
check("动态: 1439 重臂后 每日/登陆/等级 不发包", (
	ACC_EVERYDAY not in ids_sent() and ACC_LOGIN not in ids_sent() and ACC_LEVEL not in ids_sent()
	and IFC_EVERYDAY not in ids_sent()), repr(ids_sent()))

# --- ③-8 在线领完(index 空) → done + 落盘
adv(990000 + 10000)			# 重新越过 base_at, 顺带在线退避过期
reset_sent()
ec.on_online_interface(7, [""])
_st_on = R.m_enc_claim["sys"]["online"]
check("动态: 在线 index 清空 → done", bool(_st_on.get("done")), repr(_st_on))
check("动态: online_done 落盘", bool(ec._load_persist(R).get("online_done")), repr(ec._load_persist(R)))
adv(1000)
reset_sent()
ec.tick(R)
check("动态: 在线 done 后不再发 online 包", (
	ACC_ONLINE not in ids_sent() and IFC_ONLINE not in ids_sent()), repr(ids_sent()))

# --- ③-9 反例: 全程发包白名单(累计全部会话)
_all = set([m for m, _a in ALL_SENT])
check("反例: 全程发包 ⊆ {8 encourage 协议 + C2S_USEITEM}", _all <= ALLOWED, repr(_all - ALLOWED))
check("反例: 全程至少发过 4 类包(在线查询/领取/每日/等级/开箱)", len(_all & ALLOWED) >= 4, repr(sorted(_all)))

# --- ③-10 反例: 协议未注册(模拟热更未重启) → 全静默
_saved = {}
for _k in NEED_C2S:
	if _k in protocol3.FORMAT_MC:
		_saved[_k] = protocol3.FORMAT_MC.pop(_k)
ec._READY_CACHE[0] = False
ec._READY_CACHE[1] = 0
check("反例: 协议被移除后 proto_ready=False", ec.proto_ready() is False)
R2 = StubRobot()
reset_sent(R2)
ec.tick(R2)
ec.on_notice(R2, [1439, "x"])
check("反例: 未注册时 tick/on_notice 零发包", ids_sent(R2) == [], repr(ids_sent(R2)))
for _k, _v in _saved.items():
	protocol3.FORMAT_MC[_k] = _v
ec._READY_CACHE[0] = False
ec._READY_CACHE[1] = 0
check("恢复: 协议恢复后 proto_ready=True", ec.proto_ready() is True)

# ================================================================
print("")
print("encourage_selftest: %d 项, 失败 %d" % (total, fails))
sys.exit(1 if fails else 0)
