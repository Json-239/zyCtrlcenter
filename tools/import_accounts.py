# -*- coding: utf-8 -*-
"""把 Python 中控的账号库（SQLite）**导入**成中控账号池 JSON。

用法:
    python tools/import_accounts.py [<db> [<out.json>]]

默认:
    db  = E:\robot_accounts\robot_accounts.db   （可用环境变量 CTRL_ACCOUNT_DB 覆盖）
    out = <项目>/data/accounts.json

输出结构（中控侧直接读这个文件当账号池）:
{
  "_comment": "...",
  "generated_at": 1789800000,
  "source": "<db 路径>",
  "servers": {"1": "47.96.8.240:2300", ...},
  "accounts": [
    {"name": "robot0001000@xy3.com", "password": "123456", "level": 33, "role_name": "金永祥",
     "chain_done": false, "task_type": "", "note": "", "fpp": 0, "done": 0, "added_at": 0,
     "zones": {"47.96.8.240:2300": {"usable": true, "verified": true, "msg": "..."}}}
  ]
}
"""
import io
import json
import os
import sqlite3
import sys
import time

try:
    sys.stdout.reconfigure(encoding="utf-8")
except Exception:
    pass

BASE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DEFAULT_DB = os.environ.get("CTRL_ACCOUNT_DB", r"E:\robot_accounts\robot_accounts.db")


def main():
    db = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_DB
    out = sys.argv[2] if len(sys.argv) > 2 else os.path.join(BASE, "data", "accounts.json")
    if not os.path.exists(db):
        print("找不到账号库: %s" % db)
        return 1

    c = sqlite3.connect(db)
    c.row_factory = sqlite3.Row

    servers = {str(r["id"]): r["host_port"] for r in c.execute("select id, host_port from servers")}
    zones = {}  # account -> {host_port: state}
    for r in c.execute(
            "select a.account as account, s.host_port as hp, st.verified, st.usable, st.verify_msg, "
            "       st.level, st.role_name, st.chain_done, st.fpp, st.done "
            "from account_server_state st "
            "join accounts a on a.id = st.account_id "
            "join servers s on s.id = st.server_id"):
        zones.setdefault(r["account"], {})[r["hp"]] = {
            "verified": bool(r["verified"]),
            "usable": bool(r["usable"]),
            "msg": r["verify_msg"] or "",
            "level": r["level"] or 0,
            "role_name": r["role_name"] or "",
            "chain_done": bool(r["chain_done"]),
            "fpp": r["fpp"] or 0,
            "done": r["done"] or 0,
        }

    accounts = []
    for r in c.execute(
            "select account, password, level, role_name, chain_done, task_type, note, fpp, done, "
            "       created_at, last_online from accounts order by account"):
        name = r["account"]
        accounts.append({
            "name": name,
            "password": r["password"] or "",
            "level": r["level"] or 0,
            "role_name": r["role_name"] or "",
            "chain_done": bool(r["chain_done"]),
            "task_type": r["task_type"] or "",
            "note": r["note"] or "",
            "fpp": r["fpp"] or 0,
            "done": r["done"] or 0,
            "added_at": int(r["created_at"] or 0),
            "last_online": int(r["last_online"] or 0),
            "zones": zones.get(name, {}),
        })
    c.close()

    d = os.path.dirname(out)
    if d and not os.path.isdir(d):
        os.makedirs(d)
    payload = {
        "_comment": "账号池（由 tools/import_accounts.py 从 Python 中控账号库导入；可重复导入覆盖）",
        "generated_at": int(time.time()),
        "source": db,
        "servers": servers,
        "accounts": accounts,
    }
    with io.open(out, "w", encoding="utf-8", newline="\n") as f:
        json.dump(payload, f, ensure_ascii=False, indent=1)

    with_zone = sum(1 for a in accounts if a["zones"])
    usable = sum(1 for a in accounts if any(z.get("usable") for z in a["zones"].values()))
    print("已导入 %d 个账号（其中有区状态的 %d 个、至少一区可用的 %d 个）→ %s"
          % (len(accounts), with_zone, usable, out))
    return 0


if __name__ == "__main__":
    sys.exit(main())
