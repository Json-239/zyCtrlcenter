# -*- coding: utf-8 -*-
"""位置上报频率回落自检（2026-09-21）：quest_walk_report_interval_sec 0.1 -> 1.0。

背景：位置包 C2S_NOTIFY_POSITION(80360) 占全部发包 ~99%（走路 10Hz 上报）；
服务端设计值本就是 1 次/秒（用户口径 + 服务端 role_report_pos 无频率校验佐证）。
0.1 是 8-26/27 为掩盖"逐段发"走路卡顿改的；9-11 整条路径机制（quest_whole_path，
服务端逐点消费路径推进角色位置）后，位置上报已非平滑主因 → 回落 1.0。

热更注意（本脚本校验）：
  quest_engine 对配置是 `config.quest_walk_report_interval_sec` **每次属性访问**
  （无 from import / 无模块级或默认参数缓存）→ 热更 reload config 模块即生效。
  但机器人端默认只 reload ["quest_engine"]（client.py），必须显式下发
  {"modules": ["config", "quest_engine"]}，否则读到的仍是旧值。

本脚本为**源码级自检**（不 import 运行时依赖）：正则/文本断言 + 双副本一致性。
用法：python pos_report_interval_selftest.py [<仓库script目录> [<线上script目录>]]
      不带参数默认校验仓库副本；线上目录存在时一并校验（不存在=SKIP）。
"""
import hashlib
import os
import re
import sys

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

HERE = os.path.dirname(os.path.abspath(__file__))
REPO_DIR = os.path.normpath(os.path.join(HERE, "..", "deploy", "zones", "prod-240-2300", "script"))
ONLINE_DIR = os.path.normpath(r"F:\ZyBin\xm\2d-xiyou-server\robot\deploy\single_robot_zy\script")
if len(sys.argv) > 1:
    REPO_DIR = sys.argv[1]
if len(sys.argv) > 2:
    ONLINE_DIR = sys.argv[2]

fails = 0


def check(name, ok, detail=""):
    global fails
    fails += 0 if ok else 1
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name, ("  " + detail) if detail else ""))


def info(msg):
    print("[INFO] %s" % msg)


TARGET = "quest_walk_report_interval_sec"
ASSIGN_RE = re.compile(r"(?m)^%s\s*=\s*([0-9.]+)\s*$" % TARGET)
BLOCK_RE = re.compile(r"(?ms)^# 行走中位置上报间隔.*?^%s\s*=\s*[0-9.]+\s*$" % TARGET)


def read(path):
    return open(path, "rb").read().decode("utf-8-sig")


def sha(path):
    return hashlib.sha256(open(path, "rb").read()).hexdigest()


def norm(text):
    # 归一化行尾：CRLF/孤立 CR -> LF，再去尾部空白（块尾换行不参与逐字比较）
    return text.replace("\r\n", "\n").replace("\r", "").rstrip()


# ==================================================================
# A. 仓库副本：值 / 注释依据 / 回退预案
# ==================================================================
check("S0 仓库 script 目录存在", os.path.isdir(REPO_DIR), REPO_DIR)
repo_cfg = read(os.path.join(REPO_DIR, "config.py"))
repo_qe = read(os.path.join(REPO_DIR, "quest_engine.py"))
repo_ghost = read(os.path.join(REPO_DIR, "daily_ghost.py"))
repo_cli = read(os.path.join(REPO_DIR, "client.py"))

m = ASSIGN_RE.search(repo_cfg)
check("S1 仓库 config: %s == 1.0" % TARGET,
      bool(m) and float(m.group(1)) == 1.0,
      m.group(0) if m else "未找到赋值行")

mb = BLOCK_RE.search(repo_cfg)
block = mb.group(0) if mb else ""
check("S2 仓库 config: 注释写明按服务端设计值回落(80360 1 次/秒)",
      "80360 本来就是 1 次/秒" in block)
check("S3 仓库 config: 注释含 9-11 整条路径机制依据",
      "整条路径" in block and "quest_whole_path" in block)
check("S4 仓库 config: 注释含回退预案(0.5 中间档)",
      "0.5 为中间档" in block and "走路卡顿" in block)

# ==================================================================
# B. 使用点（quest_engine.py）：全部为 config.属性访问 → 热更 reload config 即生效
# ==================================================================
n_expr = repo_qe.count("config.%s * 1000" % TARGET)          # walk_report_next_ms 重算 x4
n_dt = len(re.findall(r"(?m)^\s*_dt = config\.%s\s*$" % TARGET, repo_qe))  # _dt 兜底 x1
check("S5 quest_engine: %s 使用点 5 处(=4 重算 + 1 兜底)" % TARGET,
      n_expr == 4 and n_dt == 1, "expr=%d dt=%d" % (n_expr, n_dt))
check("S6 quest_engine: 无 from config import（避免导入期取值缓存）",
      "from config import" not in repo_qe)
check("S7 quest_engine: 无模块级直接缓存(^\w+ = config.%s)" % TARGET,
      not re.search(r"(?m)^\w+\s*=\s*config\.%s" % TARGET, repo_qe))
check("S8 quest_engine: 顶层 import config",
      bool(re.search(r"(?m)^import config\b", repo_qe)))
check("S9 daily_ghost 不读该配置（抓鬼独立兜底上报不受影响）",
      TARGET not in repo_ghost)

# 热更链路提示：默认 modules 不含 config → 必须显式带
m_cli = re.search(r'(?m)modules\s*=\s*cmd\.get\("modules"\)\s*or\s*\[([^\]]*)\]', repo_cli)
check("S10 client: reload 默认 modules 不含 config（热更须显式 [\"config\",...]）",
      bool(m_cli) and "config" not in m_cli.group(1),
      ("默认 modules=[%s]" % m_cli.group(1).strip()) if m_cli else "未找到默认 modules")

# ==================================================================
# C. 线上副本：同值 + 改动块逐字一致 + quest_engine 双副本一致
# ==================================================================
if os.path.isdir(ONLINE_DIR):
    onl_cfg_path = os.path.join(ONLINE_DIR, "config.py")
    onl_qe_path = os.path.join(ONLINE_DIR, "quest_engine.py")
    onl_cfg = read(onl_cfg_path)

    mo = ASSIGN_RE.search(onl_cfg)
    check("S11 线上 config: %s == 1.0" % TARGET,
          bool(mo) and float(mo.group(1)) == 1.0,
          mo.group(0) if mo else "未找到赋值行")

    mb2 = BLOCK_RE.search(onl_cfg)
    check("S12 两份 config 改动块逐字一致(归一化 BOM/行尾)",
          bool(mb) and bool(mb2) and norm(block) == norm(mb2.group(0)))

    check("S13 两份 quest_engine.py 逐字节一致",
          sha(os.path.join(REPO_DIR, "quest_engine.py")) == sha(onl_qe_path))

    # 既有环境差异提示（不判 FAIL）：robot_account / ctrl_server_port / BOM+CRLF
    raw_o = open(onl_cfg_path, "rb").read()
    if raw_o.startswith(b"\xef\xbb\xbf") and b"\r\n" in raw_o:
        info("线上 config 为 UTF-8 BOM + CRLF（仓库为无 BOM + LF）——既有差异, 本次未动")
    if "or 27200" in onl_cfg and "or 17200" in repo_cfg:
        info("既有差异: 仓库副本 ctrl_server_port 默认 17200 / 线上 27200；"
             "robot_account 亦不同（线上=(1,1) 由中控下发）——本次未动, 见文档")
else:
    info("SKIP 线上副本目录不存在: %s" % ONLINE_DIR)

print("")
print("RESULT: %s" % ("ALL PASS" if fails == 0 else "%d FAILED" % fails))
sys.exit(1 if fails else 0)
