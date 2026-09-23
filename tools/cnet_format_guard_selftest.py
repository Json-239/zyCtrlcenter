# -*- coding: utf-8 -*-
"""协议表"一次性"守卫自检（2026-09-23）。

背景（源码级取证，全文见 docs/04-测试/分析-20260923-商店崩溃版本.md）:
  cnetwork(机器人内嵌 C 扩展)的 SetFormatDict 只**借用**传入的 dict(不 Py_INCREF),
  却对上一次传入的指针做 Py_DECREF(pynetwork.h:41-55) → 同一进程第二次调用会把
  protocol3.FORMAT_MS/FORMAT_MC 的引用计数多减 1 → dict 被释放、C 层留下悬垂指针 →
  下一次收包 PyDict_GetItem 访问已释放内存 = 0xc0000005、全号掉线。
  生产实证: 2026-09-23 16:42:31 / 16:47:53 两次崩溃均由"运行期补注册协议表"触发。

覆盖：
  ① cnet.set_format_dict 首调放行（冷启动路径正常）
  ② 首调后持有 dict 强引用（使 C 层借用指针永不悬垂）
  ③ 第二次调用 raise RuntimeError（替代静默内存破坏）
  ④ 异常信息可读且含指引（冷启动一次 / 改 protocol3 静态注册 / 重启）
  ⑤ raise 发生在触达 C 层之前（不产生第二次 C 调用）
  ⑥ 静态核对：cnet.py 含守卫实现 + 根因说明；协议表注册调用点唯一（client.init）
  ⑦ 双副本 cnet.py 字节一致（生产副本存在时）
用法：python tools/cnet_format_guard_selftest.py
"""
import os
import re
import sys

try:
	sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
	pass

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.normpath(os.path.join(HERE, ".."))
SCRIPT = os.path.normpath(os.path.join(ROOT, "deploy", "zones", "prod-240-2300", "script"))
PROD_COPY = "F:/ZyBin/xm/2d-xiyou-server/robot/deploy/single_robot_zy/script"
sys.path.insert(0, SCRIPT)
# 打包内置的 C 扩展 cnetwork 在纯 Python 下用 MagicMock 顶替（同冷启动自检）
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
	return open(path, encoding="utf-8").read()


# ---------------------------------------------------------------- ①~⑤ 运行期守卫
import cnet as _cnet  # noqa: E402

_cnet._format_dicts_held = None		# 复位守卫（等价冷启动）
_cdict, _sdict = {"1": []}, {"90327": [8, 2]}
_cnet.set_format_dict(_cdict, _sdict)
check("护栏: 首调放行(冷启动)", _cnet._format_dicts_held == (_cdict, _sdict),
	repr(_cnet._format_dicts_held))
check("护栏: 首调后持有 dict 强引用(使 C 层借用指针永不悬垂)",
	_cnet._format_dicts_held[1] is _sdict)

_C_CALLS = _cnet.cnetwork.set_format_dict.call_count
_raised = None
try:
	_cnet.set_format_dict({"1": []}, {"90327": [8, 2]})
except Exception as e:
	_raised = e
check("护栏: 第二次调用 raise RuntimeError(不再静默破坏内存)",
	isinstance(_raised, RuntimeError), repr(_raised))
check("护栏: 异常信息可读且含指引(冷启动一次/改 protocol3 静态注册/重启)",
	_raised is not None and "只允许冷启动调用一次" in str(_raised)
	and "protocol3" in str(_raised) and "重启" in str(_raised), str(_raised))
check("护栏: raise 发生在触达 C 层之前(不产生第二次 C 调用)",
	_cnet.cnetwork.set_format_dict.call_count == _C_CALLS,
	"%s → %s" % (_C_CALLS, _cnet.cnetwork.set_format_dict.call_count))

# ---------------------------------------------------------------- ⑥ 静态核对
_cnet_src = rd(os.path.join(SCRIPT, "cnet.py"))
check("静态: cnet.py 含守卫实现 + 根因说明",
	"只允许冷启动调用一次" in _cnet_src and "悬垂指针" in _cnet_src
	and "_format_dicts_held" in _cnet_src)
check("静态: 守卫注释指向取证文档",
	"分析-20260923-商店崩溃版本.md" in _cnet_src)
_active_calls = [f for f in sorted(os.listdir(SCRIPT)) if f.endswith(".py")
	and re.search(r"^\s*(?!\s*#).*cnet\.set_format_dict\s*\(",
		rd(os.path.join(SCRIPT, f)), re.M)]
check("静态: 协议表注册调用点唯一(client.init)", _active_calls == ["client.py"], repr(_active_calls))
_cl = rd(os.path.join(SCRIPT, "client.py"))
check("静态: client.py 协议/框架层禁热更名单含 cnet/protocol3/msghandle",
	'_NO_RELOAD = ("protocol3", "msghandle", "cnet"' in _cl)
_ro = rd(os.path.join(SCRIPT, "robot_operator.py"))
check("静态: robot_operator 无运行期补注册(禁 set_format_dict 活代码)",
	re.search(r"^\s*def\s+install_shop_protocol_hooks", _ro, re.M) is None
	and re.search(r"^\s*(?!\s*#).*cnet\.set_format_dict\s*\(", _ro, re.M) is None)
_srt = rd(os.path.join(SCRIPT, "shop_errand.py"))
check("静态: shop_errand 无运行期补注册(install_protocol_hooks 不出现)",
	"install_protocol_hooks" not in _srt)

# ---------------------------------------------------------------- ⑦ 双副本
if os.path.isdir(PROD_COPY):
	_b = os.path.join(PROD_COPY, "cnet.py")
	check("双副本: cnet.py 字节一致",
		os.path.exists(_b) and open(os.path.join(SCRIPT, "cnet.py"), "rb").read() == open(_b, "rb").read())
else:
	check("双副本检查跳过(生产副本不存在)", True)

print("\n结果：%d 项，失败 %d 项" % (total, fails))
sys.exit(1 if fails else 0)
