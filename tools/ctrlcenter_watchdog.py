# -*- coding: utf-8 -*-
"""中控看门狗：每 N 秒探测一次中控 HTTP；不可达则自动拉起并记日志。

背景（2026-09-23）：中控进程当天多次"无征兆消失"（Go 日志无 panic，像被外部终止），
且中断会连带子 agent/面板不可用。看门狗提供自愈 + 留痕（什么时候挂的、拉起是否成功）。

2026-10-10 加固（15:31 事故：忙误判 → 连续误拉 4 个中控实例 → 每个被误拉实例在端口占用
FATAL 退出前又各多拉起一个 robot 进程 → 4 个孤儿 robot 互抢单控制通道，全队掉线）：
  - 拉起前置进程确认：zyctrlcenter.exe 还在（HTTP 只是忙/慢）→ 先观察 3 个周期（约 30s）
    仍不可达才拉起；进程确认不在才立即拉起；
  - 拉起退避：拉起后仍未恢复时，下一次拉起间隔 60→120→300s（上限 300s），恢复后重置；
  - HTTP 探测超时 5s→8s（高负载时降低误判）；
  - 每轮统计 zyctrlcenter.exe 实例数，>1 时记日志（只记不杀；孤儿实例请人工清理）。
2026-10-12 修补（20:57 误拉实例成因之一）：tasklist 探测失败（procs=None）此前会"立即拉起"，
  现与"进程存在"同口径——先观察 3 个周期；拉起点日志附 procs=N 证据。

用法（独立后台运行，日志 logs/watchdog.log）：
  python tools/ctrlcenter_watchdog.py            # 前台跑（Ctrl+C 停）
  start /b python tools/ctrlcenter_watchdog.py   # Windows 后台

可选参数：
  --interval 10        探测间隔（秒）
  --port 28082         中控端口
  --once               只探测一次（用于自检；只读：不拉起、不清理）
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

EXE_NAME = "zyctrlcenter.exe"    # 中控可执行文件名（tasklist 过滤 + 兜底拉起共用）
PROBE_TIMEOUT = 8                # HTTP 探测超时（秒）；2026-10-10 由 5s 提高
CONFIRM_CYCLES = 3               # 进程在但 HTTP 不通：连续 N 个探测周期仍不通才允许拉起
BACKOFF_SECS = (60, 120, 300)    # 退避档位：第 k 次拉起后等待 BACKOFF_SECS[k-1]（封顶末项）


def _log(msg):
    try:
        os.makedirs(os.path.dirname(LOG), exist_ok=True)
        ts = datetime.datetime.now().strftime("%Y-%m-%d %H:%M:%S")
        with open(LOG, "a", encoding="utf-8") as f:
            f.write("%s %s\n" % (ts, msg))
    except Exception:
        pass
    print("%s %s" % (datetime.datetime.now().strftime("%H:%M:%S"), msg))


def _alive(port, timeout=PROBE_TIMEOUT):
    """HTTP 可达即视为存活（比查进程更贴近"服务可用"）。"""
    try:
        with urllib.request.urlopen("http://127.0.0.1:%d/api/status" % port, timeout=timeout) as r:
            json.load(r)
        return True
    except Exception:
        return False


def _proc_count():
    """统计本机 zyctrlcenter.exe 实例数（tasklist，只读）；探测失败返回 None（视作未知）。

    2026-10-10：用于 ①拉起前的进程确认 ②多实例（疑似误拉孤儿）告警。
    """
    try:
        p = subprocess.run(
            ["tasklist", "/FI", "IMAGENAME eq %s" % EXE_NAME, "/FO", "CSV", "/NH"],
            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=5,
            creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
        text = (p.stdout or b"").decode("utf-8", "replace")
    except Exception:
        return None
    # 无匹配时 tasklist 只输出一条本地化 INFO 行（不含镜像名），自然计 0。
    return sum(1 for line in text.splitlines() if EXE_NAME.lower() in line.lower())


def _start():
    """拉起中控：复用项目既有的 _start_detached.bat（含 --auto-robot）。"""
    bat = os.path.join(ROOT, "_start_detached.bat")
    if os.path.exists(bat):
        subprocess.Popen(["cmd", "/c", bat], cwd=ROOT,
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                         creationflags=getattr(subprocess, "DETACHED_PROCESS", 0))
    else:
        exe = os.path.join(ROOT, EXE_NAME)
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
        print("alive=%s" % ok)                 # 首行格式保持兼容；退出码同旧：0/1
        print("procs=%s" % _proc_count())      # 新增：只读实例数
        return 0 if ok else 1

    _log("看门狗启动（每 %ds 探测 :%d，HTTP 超时 %ds）" % (args.interval, args.port, PROBE_TIMEOUT))
    down_since = None       # 首次不可达时间戳；恢复后清空
    unreach_streak = 0      # 连续不可达周期数；恢复后清零
    start_count = 0         # 本次宕机已拉起次数（退避档位）；恢复后清零
    last_start = 0.0        # 最近一次拉起时间
    multi_logged = False    # 多实例告警去重；实例数回落才重新告警
    multi_last_ts = 0.0     # 多实例告警的周期刷新时间
    while True:
        ok = _alive(args.port)
        n_proc = _proc_count()

        # 多实例检测（只记不杀）：每次探测均统计；>1 时边沿告警 + 每 60s 刷新，避免刷屏
        if n_proc is not None and n_proc > 1:
            now = time.time()
            if not multi_logged or now - multi_last_ts >= 60:
                _log("★检测到 %d 个中控实例（疑似误拉孤儿，请人工清理）" % n_proc)
                multi_logged = True
                multi_last_ts = now
        else:
            multi_logged = False

        if ok:
            if down_since is not None:
                _log("中控已恢复（宕机 %ds，期间拉起 %d 次）" % (int(time.time() - down_since), start_count))
            down_since = None
            unreach_streak = 0
            start_count = 0
            last_start = 0.0
        else:
            if down_since is None:
                down_since = time.time()
            unreach_streak += 1

            if (n_proc is None or n_proc >= 1) and unreach_streak < CONFIRM_CYCLES:
                # 进程还在（HTTP 只是忙/慢）；tasklist 探测失败（None）同样按"在"保守处理：先观察，
                # 不立即拉起（2026-10-10 防单次忙误判；2026-10-12 None 语义修复——20:57 误拉实例成因之一）
                _log("中控进程存在但 HTTP 无响应, 观察中 %d/%d" % (unreach_streak, CONFIRM_CYCLES))
            else:
                # 退避：第 k 次拉起后，需等满 BACKOFF_SECS[k-1]（封顶最后一档）才允许下一次
                wait = BACKOFF_SECS[min(max(start_count - 1, 0), len(BACKOFF_SECS) - 1)]
                if start_count == 0 or time.time() - last_start >= wait:
                    if start_count == 0:
                        _log("★中控不可达 → 拉起中控（procs=%s）" % n_proc)
                    else:
                        _log("中控仍不可达（已 %ds），第 %d 次拉起（间隔 %ds）"
                             % (int(time.time() - down_since), start_count + 1, wait))
                    _start()
                    start_count += 1
                    last_start = time.time()
        time.sleep(args.interval)


if __name__ == "__main__":
    sys.exit(main())
