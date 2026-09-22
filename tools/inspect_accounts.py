# -*- coding: utf-8 -*-
"""查看账号库（robot_accounts.db）概览：服务器列表 / 账号数 / 指定账号是否存在。

用法：
    python tools/inspect_accounts.py [db_path] [账号前缀过滤]

默认库路径：<项目>/data/robot_accounts.db
"""
import os
import sqlite3
import sys

try:  # Windows 控制台默认非 UTF-8，中文会乱码
    sys.stdout.reconfigure(encoding="utf-8")
except Exception:
    pass

BASE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DB = sys.argv[1] if len(sys.argv) > 1 else os.path.join(BASE, "data", "robot_accounts.db")
KEYWORD = sys.argv[2] if len(sys.argv) > 2 else ""

if not os.path.exists(DB):
    print("库不存在: %s" % DB)
    sys.exit(1)

c = sqlite3.connect(DB)
c.row_factory = sqlite3.Row

print("库文件: %s (%.2f MB)" % (DB, os.path.getsize(DB) / 1048576.0))
print("--- servers ---")
for r in c.execute("select id, host_port, name, is_current, created_at from servers order by id"):
    print("  id=%s %-24s %-12s current=%s" % (r["id"], r["host_port"], r["name"] or "-", r["is_current"]))

print("--- accounts ---")
print("  总数: %s" % c.execute("select count(*) from accounts").fetchone()[0])
print("  链表完成: %s" % c.execute(
    "select count(*) from accounts where chain_done=1").fetchone()[0])
print("  前 5 条:")
for r in c.execute("select account, level, chain_done, role_name from accounts order by id limit 5"):
    print("    %-26s level=%-4s chain_done=%s role=%s" % (
        r["account"], r["level"], r["chain_done"], r["role_name"] or "-"))

if KEYWORD in ("--account", "-a"):
    # python tools/inspect_accounts.py <db> --account <账号>  → 打印完整行（含密码，供启动机器人用）
    if len(sys.argv) < 4:
        print("用法: inspect_accounts.py <db> --account <账号>")
        sys.exit(1)
    name = sys.argv[3]
    rows = list(c.execute(
        "select id,account,password,level,role_name,chain_done,note from accounts where account=?",
        (name,)))
    if not rows:
        print("账号不存在: %s" % name)
    for r in rows:
        print("id=%s account=%s password=%s level=%s role=%s chain_done=%s note=%s" % (
            r["id"], r["account"], r["password"], r["level"], r["role_name"],
            r["chain_done"], r["note"]))
    c.close()
    sys.exit(0)

if KEYWORD == "--usable":
    # python tools/inspect_accounts.py <db> --usable <host:port> [limit]
    #   列出"该服/区可用"（account_server_state.usable=1）的账号，供挑选登录目标
    if len(sys.argv) < 4:
        print("用法: inspect_accounts.py <db> --usable <host:port> [limit]")
        sys.exit(1)
    host_port = sys.argv[3]
    limit = int(sys.argv[4]) if len(sys.argv) > 4 else 15
    row = c.execute("select id from servers where host_port=?", (host_port,)).fetchone()
    if row is None:
        print("库里没有该服: %s" % host_port)
        sys.exit(1)
    sid = row["id"]
    total = c.execute(
        "select count(*) from account_server_state where server_id=? and usable=1", (sid,)).fetchone()[0]
    print("服 %s (id=%s) 可用账号数: %s" % (host_port, sid, total))
    for r in c.execute(
            "select a.account, a.level, s.verify_msg, s.verified_at "
            "from account_server_state s join accounts a on a.id = s.account_id "
            "where s.server_id=? and s.usable=1 order by a.account limit ?", (sid, limit)):
        print("    %-26s level=%-4s %s" % (r["account"], r["level"], r["verify_msg"] or ""))
    c.close()
    sys.exit(0)

if KEYWORD:
    print("--- 匹配 %r ---" % KEYWORD)
    rows = list(c.execute(
        "select account, level, chain_done, note from accounts where account like ? order by account limit 10",
        ("%" + KEYWORD + "%",)))
    for r in rows:
        print("    %-26s level=%-4s chain_done=%s note=%s" % (
            r["account"], r["level"], r["chain_done"], r["note"] or "-"))
    if not rows:
        print("    （无匹配）")
c.close()
