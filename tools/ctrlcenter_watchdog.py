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

2026-10-14 多实例化（实例管理页 V1，见 docs/04-测试/分析-20261014-实例管理页方案.md）：
  - 读 <项目根>/instances.json（实例注册表，与 exe 同级；由中控 /api/instances 维护）。
    文件不存在/解析失败 → **完全退化到单实例旧行为**（--port 探测 + 默认 bat 拉起）；
  - 存在时逐实例探测：先探 http://127.0.0.1:<web_port>/api/ping（轻量），失败回退
    /api/status（兼容旧版实例）；每实例独立"观察 3 周期 + 退避 60/120/300"状态机；
  - 拉起对象 = 该实例的 start_script（cmd /c start "" /min，工作目录=项目根）；
    脚本缺失 → 只记日志**不动作**（宁可不拉，也不误拉成别的栈）；
  - 进程计数告警改按实例（ctrl_match 命令行匹配，PowerShell，只读）统计，各自 >1 才告警；
    无法按实例区分（无 ctrl_match/探测失败）时退回全机计数，告警文案注明。

2026-10-14 人工停止（防"面板点了停止又被看门狗拉回来"）：
  - instances.json 支持 "stopped": true —— 由中控在 停止/归档 前写入；看门狗对该实例
    跳过探测/拉起（进入时记一条日志；「启动」清除标记后记"恢复探测"）；
  - 注册表**每轮重读**（不再启动时一次性缓存）：人工停止即时生效，归档/删除的实例自然
    从名单消失（顺带修掉"归档后旧缓存仍按旧脚本拉起"的隐患）；
    读空/解析失败 → 仅提示一次并跳过本轮（不动作，等文件恢复）。

用法（独立后台运行，日志 logs/watchdog.log）：
  python tools/ctrlcenter_watchdog.py            # 前台跑（Ctrl+C 停）
  start /b python tools/ctrlcenter_watchdog.py   # Windows 后台

可选参数：
  --interval 10        探测间隔（秒）
  --port 28082         单实例模式的探测端口（无 instances.json 时生效）
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
INSTANCES_FILE = os.path.join(ROOT, "instances.json")  # 多实例注册表（与 exe 同级）
HEARTBEAT_FILE = os.path.join(ROOT, "logs", "heartbeat.json")  # 2026-10-28 中控心跳（静默死亡取证）

EXE_NAME = "zyctrlcenter.exe"    # 中控可执行文件名（tasklist 过滤 + 兜底拉起共用）
PROBE_TIMEOUT = 8                # HTTP 探测超时（秒）；2026-10-10 由 5s 提高
CONFIRM_CYCLES = 3               # 进程在但 HTTP 不通：连续 N 个探测周期仍不通才允许拉起
BACKOFF_SECS = (60, 120, 300)    # 退避档位：第 k 次拉起后等待 BACKOFF_SECS[k-1]（封顶末项）
PROBE_PATHS = ("/api/ping", "/api/status")  # 先轻量端点；失败回退（兼容旧版实例）


def _heartbeat_note():
    """读中控心跳文件，返回 “最后心跳 Ns 前 goroutines=X online=Y”；缺失/损坏返回空串。

    2026-10-28 取证加固：中控每 5s 写 logs/heartbeat.json。探测失败时带上它，
    可区分 “卡死”（心跳停在死亡前、进程仍在）与 “被杀”（心跳停 + 进程消失）。
    """
    try:
        with open(HEARTBEAT_FILE, "r", encoding="utf-8") as f:
            d = json.load(f)
        ts = int(d.get("ts") or 0)
        if ts <= 0:
            return ""
        age = int(time.time()) - ts
        if age < 0:
            age = 0
        return "最后心跳 %ds 前 goroutines=%s online=%s" % (
            age, d.get("goroutines", "?"), d.get("online", "?"))
    except Exception:
        return ""


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
    """HTTP 可达即视为存活（比查进程更贴近"服务可用"）。

    2026-10-14：先探 /api/ping（超轻）；失败回退 /api/status（兼容没有 ping 的旧版实例）。
    """
    for path in PROBE_PATHS:
        try:
            with urllib.request.urlopen("http://127.0.0.1:%d%s" % (port, path), timeout=timeout) as r:
                json.load(r)
            return True
        except Exception:
            continue
    return False


def _proc_count():
    """统计本机 zyctrlcenter.exe 实例数（tasklist，只读）；探测失败返回 None（视作未知）。

    2026-10-10：用于 ①拉起前的进程确认 ②多实例（疑似误拉孤儿）告警。
    2026-10-14：多实例模式下的"无法按实例区分"兜底计数。
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


def _proc_count_for(ctrl_match):
    """按命令行匹配统计"某实例"的中控进程数（PowerShell，只读）。

    用法红线同 Go 侧（internal/services/instances）：只按命令行/路径匹配，绝不按镜像名杀。
    无法区分（ctrl_match 为空/探测失败）→ None（调用方退回全机计数做保守判断）。
    """
    if not ctrl_match:
        return None
    lit = ctrl_match.replace("'", "''")
    ps = ("$m = '%s'; @(Get-CimInstance Win32_Process -Filter \"Name='%s'\" | "
          "Where-Object { $_.CommandLine -like ('*' + "
          "[System.Management.Automation.WildcardPattern]::Escape($m) + '*') }).Count"
          % (lit, EXE_NAME))
    try:
        p = subprocess.run(
            ["powershell", "-NoProfile", "-Command", ps],
            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=10,
            creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
        text = (p.stdout or b"").decode("utf-8", "replace").strip()
        return int(text) if text.lstrip("-").isdigit() else None
    except Exception:
        return None


def _start():
    """拉起中控（单实例模式）：复用项目既有的 _start_detached.bat（含 --auto-robot）。"""
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


def _start_instance(inst):
    """拉起指定实例（多实例模式）：cmd /c start "" /min <该实例的 start_script>。

    脚本缺失/不存在 → 只记日志不动作（宁可不拉，也不误拉成别的栈）。
    """
    script = (inst.get("start_script") or "").strip()
    if not script:
        _log("★实例 %s 不可达但未配置 start_script，无法自动拉起（不动作）" % inst["id"])
        return False
    path = script if os.path.isabs(script) else os.path.join(ROOT, script)
    if not os.path.exists(path):
        _log("★实例 %s 不可达但启动脚本不存在: %s（不动作）" % (inst["id"], path))
        return False
    subprocess.Popen(["cmd", "/c", "start", "", "/min", path], cwd=ROOT,
                     stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                     creationflags=getattr(subprocess, "DETACHED_PROCESS", 0))
    _log("实例 %s 已拉起（%s）" % (inst["id"], os.path.basename(path)))
    return True


def _load_instances():
    """读 <项目根>/instances.json；返回 [{id,name,port,start_script,ctrl_match}]。

    文件不存在/解析失败/无有效条目 → []（完全退化到单实例旧行为）。
    """
    try:
        with open(INSTANCES_FILE, "r", encoding="utf-8") as f:
            data = json.load(f)
    except FileNotFoundError:
        return []
    except Exception as e:
        _log("实例注册表读取失败（退回单实例模式）: %s" % e)
        return []
    out = []
    for it in (data.get("instances") or []):
        if not isinstance(it, dict):
            continue
        port = it.get("web_port")
        if not isinstance(port, int) or isinstance(port, bool) or port <= 0:
            continue
        out.append({
            "id": str(it.get("id") or ("p%d" % port)),
            "name": str(it.get("name") or ""),
            "port": port,
            "start_script": str(it.get("start_script") or ""),
            "ctrl_match": str(it.get("ctrl_match") or ""),
            # 人工停止标记（严格 JSON true 才生效；缺省/其它值 = 未停止）
            "stopped": it.get("stopped") is True,
        })
    return out


def _new_state():
    """单目标探测状态机（与旧版单实例逻辑一致）。"""
    return {
        "down_since": None,      # 首次不可达时间戳；恢复后清空
        "unreach_streak": 0,     # 连续不可达周期数；恢复后清零
        "start_count": 0,        # 本次宕机已拉起次数（退避档位）；恢复后清零
        "last_start": 0.0,       # 最近一次拉起时间
        "multi_logged": False,   # 多实例告警去重；实例数回落才重新告警
        "multi_last_ts": 0.0,    # 多实例告警的周期刷新时间
    }


def _tick(st, ok, n_proc, label, start_fn, multi_note):
    """一个探测周期（旧单实例逻辑原样抽出；多实例模式每实例各自一份 state）。

    label：日志前缀（单实例="中控"；多实例="实例 <id>(:<port>)"）。
    n_proc：本次进程计数（None=未知，按"进程可能还在"保守处理）。
    multi_note：n_proc>1 告警的补充说明（按实例/全机计数区分）。
    """
    now = time.time()

    # 多实例检测（只记不杀）：每次探测均统计；>1 时边沿告警 + 每 60s 刷新，避免刷屏
    if n_proc is not None and n_proc > 1:
        if not st["multi_logged"] or now - st["multi_last_ts"] >= 60:
            _log("★%s检测到 %d 个中控实例（%s）" % (label, n_proc, multi_note))
            st["multi_logged"] = True
            st["multi_last_ts"] = now
    else:
        st["multi_logged"] = False

    if ok:
        if st["down_since"] is not None:
            _log("%s已恢复（宕机 %ds，期间拉起 %d 次）"
                 % (label, int(now - st["down_since"]), st["start_count"]))
        st["down_since"] = None
        st["unreach_streak"] = 0
        st["start_count"] = 0
        st["last_start"] = 0.0
        return
    if st["down_since"] is None:
        st["down_since"] = now
    st["unreach_streak"] += 1

    if (n_proc is None or n_proc >= 1) and st["unreach_streak"] < CONFIRM_CYCLES:
        # 进程还在（HTTP 只是忙/慢）；计数未知（None）同样按"在"保守处理：先观察，
        # 不立即拉起（2026-10-10 防单次忙误判；2026-10-12 None 语义修复——20:57 误拉实例成因之一）
        note = _heartbeat_note()
        _log("%s进程存在但 HTTP 无响应, 观察中 %d/%d%s" % (
            label, st["unreach_streak"], CONFIRM_CYCLES, ("；" + note) if note else ""))
        return

    # 退避：第 k 次拉起后，需等满 BACKOFF_SECS[k-1]（封顶最后一档）才允许下一次
    wait = BACKOFF_SECS[min(max(st["start_count"] - 1, 0), len(BACKOFF_SECS) - 1)]
    if st["start_count"] == 0 or now - st["last_start"] >= wait:
        if st["start_count"] == 0:
            note = _heartbeat_note()
            _log("★%s不可达 → 拉起中控（procs=%s）%s" % (
                label, n_proc, ("；" + note) if note else ""))
        else:
            _log("%s仍不可达（已 %ds），第 %d 次拉起（间隔 %ds）"
                 % (label, int(now - st["down_since"]), st["start_count"] + 1, wait))
        start_fn()
        st["start_count"] += 1
        st["last_start"] = now


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--interval", type=int, default=10)
    ap.add_argument("--port", type=int, default=28082)
    ap.add_argument("--once", action="store_true")
    args = ap.parse_args()

    insts = _load_instances()

    if args.once:
        # --once 保持只读：不拉起、不清理
        if insts:
            any_alive = False
            for it in insts:
                ok = _alive(it["port"])
                n = _proc_count_for(it["ctrl_match"])
                tail = " stopped=人工停止" if it.get("stopped") else ""
                print("[%s] port=%d alive=%s procs=%s%s" % (it["id"], it["port"], ok, n, tail))
                any_alive = any_alive or ok
            print("alive=%s" % any_alive)   # 兼容旧格式（任一实例活着）
            print("procs=%s" % _proc_count())
            return 0 if any_alive else 1
        ok = _alive(args.port)
        print("alive=%s" % ok)                 # 首行格式保持兼容；退出码同旧：0/1
        print("procs=%s" % _proc_count())      # 只读实例数
        return 0 if ok else 1

    if insts:
        _log("看门狗启动（多实例模式：%d 个实例，每 %ds 逐实例探测，HTTP 超时 %ds）"
             % (len(insts), args.interval, PROBE_TIMEOUT))
        states = {it["id"]: _new_state() for it in insts}
        stopped_notified = set()   # 已提示"人工停止"的实例（恢复时清）
        empty_notified = False     # 注册表读空/解析失败只提示一次
        while True:
            # 2026-10-14 人工停止：每轮重读注册表——stopped=true 跳过探测/拉起；
            # 删除的实例自然从名单消失（不再被启动时的旧缓存拉起）。
            cur = _load_instances()
            if not cur:
                if not empty_notified:
                    _log("★实例注册表读取为空或失败：跳过探测（不动作，等文件恢复）")
                    empty_notified = True
                time.sleep(args.interval)
                continue
            empty_notified = False
            cur_ids = set()
            for it in cur:
                iid = it["id"]
                cur_ids.add(iid)
                label = "实例 %s(:%d) " % (iid, it["port"])
                if it.get("stopped"):
                    if iid not in stopped_notified:
                        _log("%s已人工停止（stopped=true），跳过探测/自动拉起；点「启动」恢复" % label)
                        stopped_notified.add(iid)
                    states.pop(iid, None)
                    continue
                if iid in stopped_notified:
                    _log("%s已解除人工停止，恢复探测" % label)
                    stopped_notified.discard(iid)
                st = states.get(iid)
                if st is None:
                    st = _new_state()
                    states[iid] = st
                # 进程确认：能按命令行区分 → 实例计数；否则退回全机计数（保守）+ 告警注明
                n = _proc_count_for(it["ctrl_match"])
                if n is None:
                    note = "无法按实例区分，按全机计数，可能含其它实例"
                    n_for_check = _proc_count()
                else:
                    note = "疑似误拉孤儿，请人工清理"
                    n_for_check = n
                ok = _alive(it["port"])
                _tick(st, ok, n_for_check, label,
                      lambda it=it: _start_instance(it), note)
            for k in [k for k in states if k not in cur_ids]:
                del states[k]
            stopped_notified.intersection_update(cur_ids)  # 已删除实例的提示状态一并清
            time.sleep(args.interval)

    # ---- 单实例旧行为（无 instances.json 时完全保留）----
    _log("看门狗启动（每 %ds 探测 :%d，HTTP 超时 %ds）" % (args.interval, args.port, PROBE_TIMEOUT))
    st = _new_state()
    while True:
        ok = _alive(args.port)
        n_proc = _proc_count()
        _tick(st, ok, n_proc, "中控", _start, "疑似误拉孤儿，请人工清理")
        time.sleep(args.interval)


if __name__ == "__main__":
    sys.exit(main())
