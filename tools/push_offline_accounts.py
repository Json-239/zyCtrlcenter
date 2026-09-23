# -*- coding: utf-8 -*-
"""批量恢复：把中控表里"掉线/陈旧"的号重新推给机器人（重启机器人后一键补号）。

背景：机器人进程重启后，中控旧状态可能仍是 online → 水位器认为达标不补号
（2026-09-23 生产踩过：230 个号没回来、游戏里看不见）。中控已有 hello 自愈
（机器人重连时自动把该区号标记离线），本脚本是**运维兜底 + 加速恢复**：
水位器每 60s 才补 ≤5 个，这里一次把目标号全推下去（分批发，间隔 0.7s）。

用法（中控 API 默认 127.0.0.1:28082）：
  python tools/push_offline_accounts.py               # 推"中控认为离线"的号
  python tools/push_offline_accounts.py --stale 30    # 推"30 秒内无心跳"的号（含假在线）
  python tools/push_offline_accounts.py --dry         # 只看数量不推
  python tools/push_offline_accounts.py --api http://127.0.0.1:28082 --batch 30
"""
import argparse
import io
import json
import sys
import time
import urllib.request

try:
    sys.stdout = io.TextIOWrapper(sys.stdout.buffer, encoding="utf-8", errors="replace")
except Exception:
    pass


def _get(api, path):
    with urllib.request.urlopen(api + path, timeout=15) as r:
        return json.load(r)


def _post(api, path, payload):
    body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    req = urllib.request.Request(api + path, data=body,
                                 headers={"Content-Type": "application/json"}, method="POST")
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.load(r)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--api", default="http://127.0.0.1:28082")
    ap.add_argument("--stale", type=int, default=0,
                    help=">0: 推\"N 秒内无心跳\"的号（含中控仍标 online 的假在线）")
    ap.add_argument("--batch", type=int, default=30)
    ap.add_argument("--dry", action="store_true", help="只看不推")
    args = ap.parse_args()

    st = _get(args.api, "/api/status")
    rs = st.get("robots") or []
    now = time.time()
    if args.stale > 0:
        targets = [x.get("account") for x in rs
                   if x.get("account") and x.get("online")
                   and (now - (x.get("last_seen") or 0)) > args.stale]
        label = "中控标在线但 %d 秒无心跳" % args.stale
    else:
        targets = [x.get("account") for x in rs
                   if x.get("account") and not x.get("online")]
        label = "中控标离线"
    print("待推送（%s）: %d 个%s" % (label, len(targets), "（dry-run，不推）" if args.dry else ""))
    if args.dry or not targets:
        return 0

    sent = 0
    for i in range(0, len(targets), args.batch):
        chunk = targets[i:i + args.batch]
        try:
            _post(args.api, "/api/robots/manage", {"action": "add", "accounts": chunk})
            sent += len(chunk)
        except Exception as e:
            print("  批次 %d 失败: %s: %s" % (i // args.batch + 1, type(e).__name__, e))
        time.sleep(0.7)
    print("已推送: %d 个（机器人开始登录，1~3 分钟恢复）" % sent)
    return 0


if __name__ == "__main__":
    sys.exit(main())
