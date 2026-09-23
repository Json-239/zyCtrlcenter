# -*- coding: utf-8 -*-
"""中控看门狗：每 N 秒探测一次中控 HTTP；不可达则自动拉起并记日志。

背景（2026-09-23）：中控进程当天多次"无征兆消失"（Go 日志无 panic，像被外部终止），
且中断会连带子 agent/面板不可用。看门狗提供自愈 + 留痕（什么时候挂的、拉起是否成功）。

用法（独立后台运行，日志 logs/watchdog.log）：
  python tools/ctrlcenter_watchdog.py            # 前台跑（Ctrl+C 停）
  start /b python tools/ctrlcenter_watchdog.py   # Windows 后台

可选参数：
  --interval 10        探测间隔（秒）
  --port 28082         中控端口
  --once               只探测一次（用于自检）
"""
import argparse
import datetime
import json
import os
import subprocess
import sys
import time
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
LOG = os.path.join(ROOT, "logs", "watchdog.log")


def _log(msg):
    try:
        os.makedirs(os.path.dirname(LOG), exist_ok=True)
        ts = datetime.datetime.now().strftime("%Y-%m-%d %H:%M:%S")
        with open(LOG, "a", encoding="utf-8") as f:
            f.write("%s %s\n" % (ts, msg))
    except Exception:
        pass
    print("%s %s" % (datetime.datetime.now().strftime("%H:%M:%S"), msg))


def _alive(port, timeout=5):
    """HTTP 可达即视为存活（比查进程更贴近"服务可用"）。"""
    try:
        with urllib.request.urlopen("http://127.0.0.1:%d/api/status" % port, timeout=timeout) as r:
            json.load(r)
        return True
    except Exception:
        return False


def _start():
    """拉起中控：复用项目既有的 _start_detached.bat（含 --auto-robot）。"""
    bat = os.path.join(ROOT, "_start_detached.bat")
    if os.path.exists(bat):
        subprocess.Popen(["cmd", "/c", bat], cwd=ROOT,
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                         creationflags=getattr(subprocess, "DETACHED_PROCESS", 0))
    else:
        exe = os.path.join(ROOT, "zyctrlcenter.exe")
        subprocess.Popen([exe, "--auto-robot"], cwd=ROOT,
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                         creationflags=getattr(subprocess, "DETACHED_PROCESS", 0))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--interval", type=int, default=10)
    ap.add_argument("--port", type=int, default=28082)
    ap.add_argument("--once", action="store_true")
    args = ap.parse_args()

    if args.once:
        ok = _alive(args.port)
        print("alive=%s" % ok)
        return 0 if ok else 1

    _log("看门狗启动（每 %ds 探测 :%d）" % (args.interval, args.port))
    down_since = None
    while True:
        if _alive(args.port):
            if down_since:
                _log("中控已恢复（宕机 %ds）" % int(time.time() - down_since))
                down_since = None
        else:
            if down_since is None:
                down_since = time.time()
                _log("★中控不可达 → 拉起中控")
                _start()
            else:
                # 已拉过还在等：拉起后 60 秒仍不可达 → 再拉一次（节流）
                if time.time() - down_since > 60 and int(time.time() - down_since) % 60 < args.interval:
                    _log("中控仍不可达（已 %.0fs），再次拉起" % (time.time() - down_since))
                    _start()
        time.sleep(args.interval)


if __name__ == "__main__":
    sys.exit(main())
