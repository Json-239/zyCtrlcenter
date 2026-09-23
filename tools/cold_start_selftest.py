# -*- coding: utf-8 -*-
"""冷启动导入自检（2026-09-23 生产事故固化）。

真实事故（今早"机器人通道未连接"的根因）：
  quest_engine.py 在模块级写了
      _PAID_JUMP_RE = re.compile(r"[（(]\\s*\\d+\\s*(银|两|金)")
  但整个文件**没有 `import re`** ——
    · `python -m py_compile` 只查语法 → 通过（查不出 NameError）；
    · 热更（reload）在同一个模块字典里执行：679 行之前的定义已生效、之后中断 →
      生产上"部分生效"，把问题掩盖了一整天；
    · 机器人**冷启动**（exe 重启）→ 模块级 NameError → 连锁 ImportError →
      进程"启动即退出"（中控日志：已启动 pid=xxx → 机器人进程退出: <nil>）→
      控制通道永远连不上（面板报"机器人通道未连接"）。

本脚本用**真实 import**（mock 掉打包内置的 cnetwork C 扩展）抓这类"模块级错误"：
  1) 逐个 import 核心模块 —— 任何 NameError / ImportError 立即 FAIL；
  2) 静态扫描：代码里用了 X. 但文件没有 import X（覆盖常见标准库，去注释/去字符串后匹配）；
  3) 双副本一致性（生产副本 single_robot_zy 存在时比对核心文件）。

用法：python tools/cold_start_selftest.py [script_dir]
改完任何 script/*.py 后务必跑一次（比 py_compile 强：能抓 NameError）。
"""
import contextlib
import hashlib
import importlib
import io
import os
import re
import sys
from unittest.mock import MagicMock

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_DIR = os.path.normpath(os.path.join(
    HERE, "..", "deploy", "zones", "prod-240-2300", "script"))
PROD_COPY = "F:/ZyBin/xm/2d-xiyou-server/robot/deploy/single_robot_zy/script"
script_dir = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_DIR

fails = 0
total = 0


def check(name, ok, detail=""):
    global fails, total
    total += 1
    fails += 0 if ok else 1
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  " + detail) if detail else ""))


# ================================================================ 1) 冷启动 import
# cnetwork 是打包进 robot_single_robot.exe 的 C 扩展（script 目录里没有 .py），
# 纯 Python 下用 MagicMock 顶替；import 时若模块级代码真调用它，mock 也能承接。
CORE = ["config", "protocol3", "robot_mgr", "quest_state", "quest_engine",
        "random_walk", "daily_ghost", "auto_roam", "robot_operator",
        "msghandle", "cnet"]

sys.path.insert(0, script_dir)
sys.modules["cnetwork"] = MagicMock(name="cnetwork")

for mod in CORE:
    buf = io.StringIO()
    try:
        with contextlib.redirect_stdout(buf), contextlib.redirect_stderr(buf):
            importlib.import_module(mod)
        check("import %s" % mod, True)
    except Exception as e:  # noqa: BLE001 —— 自检就是要抓所有异常
        tail = [ln for ln in buf.getvalue().strip().splitlines() if ln.strip()][-2:]
        check("import %s" % mod, False,
              "%s: %s%s" % (type(e).__name__, str(e)[:140],
                            (" | " + " / ".join(tail)[:160]) if tail else ""))

# ================================================================ 2) 静态扫描
# 常见标准库：用了 X. 但文件里没有任何 import X / from X import。
STD_MODS = ["re", "json", "time", "math", "random", "os", "sys", "traceback",
            "hashlib", "base64", "collections", "struct", "datetime", "itertools"]


def strip_line(line):
    """去掉行尾注释与字符串字面量（够用的粗过滤，避免把文案/正则里的 X. 当代码）。"""
    line = re.sub(r"#.*$", "", line)
    line = re.sub(r"'[^']*'", "''", line)
    line = re.sub(r'"[^"]*"', '""', line)
    return line


def scan_file(path):
    src = open(path, encoding="utf-8").read()
    imported = set()
    for raw in src.splitlines():
        line = strip_line(raw).strip()
        m = re.match(r"import\s+([\w\s,]+)$", line)
        if m:
            for x in m.group(1).split(","):
                imported.add(x.strip().split(" ")[0])
        m = re.match(r"from\s+(\w+)\s+import", line)
        if m:
            imported.add(m.group(1))
    missing = []
    for mod in STD_MODS:
        if mod in imported:
            continue
        used = 0
        for raw in src.splitlines():
            line = strip_line(raw)
            if re.search(r"(?<![\w.])%s\.\w+" % re.escape(mod), line):
                used += 1
        if used:
            missing.append((mod, used))
    return missing


py_files = [f for f in sorted(os.listdir(script_dir))
            if f.endswith(".py") and not f.endswith(".bak")]
scan_bad = 0
for fn in py_files:
    try:
        for mod, used in scan_file(os.path.join(script_dir, fn)):
            scan_bad += 1
            check("静态: %s 用了 %s. 但未 import" % (fn, mod), False,
                  "命中 %d 处（2026-09-23 事故即此形态）" % used)
    except Exception as e:  # noqa: BLE001
        check("静态扫描 %s" % fn, False, "%s: %s" % (type(e).__name__, str(e)[:120]))
check("静态扫描（%d 个 .py）无'用了未 import'的模块" % len(py_files), scan_bad == 0)

# ================================================================ 3) 双副本一致性
if os.path.isdir(PROD_COPY):
    diff = []
    for fn in py_files:
        p1 = os.path.join(script_dir, fn)
        p2 = os.path.join(PROD_COPY, fn)
        if not os.path.exists(p2):
            diff.append(fn + "(生产副本缺失)")
            continue
        h1 = hashlib.sha1(open(p1, "rb").read()).hexdigest()
        h2 = hashlib.sha1(open(p2, "rb").read()).hexdigest()
        if h1 != h2:
            diff.append(fn)
    check("双副本一致（%d 个 .py）" % len(py_files), not diff,
          "不一致: %s" % ", ".join(diff[:6]) if diff else "")
else:
    print("[SKIP] 生产副本目录不存在，跳过双副本比对: %s" % PROD_COPY)

# ================================================================ 汇总
print("\n自检目标: %s" % script_dir)
print("结果：%d 项，失败 %d 项" % (total, fails))
sys.exit(1 if fails else 0)
