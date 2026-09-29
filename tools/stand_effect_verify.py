# -*- coding: utf-8 -*-
"""钟馗站位改进（2026-09-29：半径 7→15 格 + 外圈优先 + 贴脸必走 + 站位避让）效果采证。

背景：现场反馈"钟馗处仍很密集"（交付高峰 13~34 号/分钟回图 24 交付，7 格=112px 覆盖不住）。
热更 quest_engine（单脚本 reload）后，用机器人运行日志做前后对比（数据源：
data/bot_logs/<账号>/runs_20260929.log，JSON Lines）：

  1) 站位点距离分布：解析"对话站位: NPC 10146 (图24,1728,1056) → 站到 (x,y)（N 格内随机）"，
     按文本里的 N 分版本（7=旧版 / 15=新版，文本即版本标志，不用猜时间点）；
  2) 图 24 贴脸号数：解析 robot_state（mapid/pos），对称窗口 + 快照新鲜度过滤，
     统计距钟馗(1728,1056) <60px / <100px 的号数（旧版基线 ≈29，与团队口径吻合）；
  3) 异常率：warn/error 与关键词（超时/失败/STUCK/对话）条数，按窗口时长归一到条/分钟。

用法：
  python tools/stand_effect_verify.py [--hot <unix_ts>] [--win 600] [--data <bot_logs目录>]
  --hot 省略时自动取今天第一条"15 格内随机"日志的 ts；--win 默认 600 秒（前后对称窗口）。
"""
import glob
import io
import json
import math
import os
import re
import sys
import time

NPC = (1728, 1056)          # 图 24 钟馗
MAP24 = 24

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_DATA = os.path.normpath(os.path.join(HERE, "..", "data", "bot_logs"))


def parse_args(argv):
    hot, win, data = None, 600, DEFAULT_DATA
    i = 1
    while i < len(argv):
        if argv[i] == '--hot' and i + 1 < len(argv):
            hot = int(argv[i + 1]); i += 2
        elif argv[i] == '--win' and i + 1 < len(argv):
            win = int(argv[i + 1]); i += 2
        elif argv[i] == '--data' and i + 1 < len(argv):
            data = argv[i + 1]; i += 2
        else:
            i += 1
    return hot, win, data


STAND_RE = re.compile(r'对话站位: NPC (\d+) \(图(\d+),(\d+),(\d+)\) → 站到 \((\d+),(\d+)\)（(\d+) 格内随机')
KEY = ('超时', '失败', 'STUCK', '对话')


def iter_logs(data):
    files = sorted(glob.glob(os.path.join(data, '*', 'runs_*.log')))
    for fp in files:
        acct = os.path.basename(os.path.dirname(fp))
        for line in io.open(fp, encoding='utf-8', errors='ignore'):
            # 只解析相关行：站位日志 / robot_state 快照 / warn/error
            if ('站到' not in line and '"robot_state"' not in line
                    and '"level":"warn"' not in line and '"level":"error"' not in line):
                continue
            try:
                yield acct, json.loads(line)
            except Exception:
                continue


def detect_hot(data):
    """第一遍：找第一条"15 格内随机"日志的 ts。"""
    for _acct, d in iter_logs(data):
        if '站到' in d.get('msg', ''):
            m = STAND_RE.search(d['msg'])
            if m and m.group(7) == '15':
                return d.get('ts', 0)
    return None


def collect(data, hot, win, now):
    pre_dist, post_dist = [], []
    pre_state, post_state = {}, {}     # 账号 -> (ts, pos)，仅 mapid==24 且落在窗口内
    pre_kw, post_kw = {}, {}
    cnt = {'pre.warn': 0, 'post.warn': 0, 'pre.err': 0, 'post.err': 0}
    for acct, d in iter_logs(data):
        ts = d.get('ts', 0)
        if not (hot - win <= ts):
            continue
        in_pre = ts < hot
        msg = d.get('msg', '')
        if '站到' in msg:
            m = STAND_RE.search(msg)
            if not m:
                continue
            dist = math.hypot(int(m.group(5)) - NPC[0], int(m.group(6)) - NPC[1])
            (pre_dist if in_pre else post_dist).append(dist)
            continue
        if d.get('type') == 'robot_state':
            if int(d.get('mapid') or 0) != MAP24:
                continue
            pos = d.get('pos') or [0, 0]
            if in_pre:
                pre_state[acct] = (ts, pos)
            else:
                post_state[acct] = (ts, pos)
            continue
        # 普通日志：异常关键词/等级（后窗口封顶到 now）
        if (not in_pre) and ts > now:
            continue
        lv = d.get('level')
        tgt = in_pre
        if lv == 'warn':
            cnt['pre.warn' if tgt else 'post.warn'] += 1
        elif lv == 'error':
            cnt['pre.err' if tgt else 'post.err'] += 1
        for k in KEY:
            if k in msg:
                dk = ('pre.' if tgt else 'post.') + k
                pre_kw[dk] = pre_kw.get(dk, 0) + 1
    return pre_dist, post_dist, pre_state, post_state, pre_kw, cnt


def main():
    hot, win, data = parse_args(sys.argv)
    now = int(time.time())
    if hot is None:
        hot = detect_hot(data)
    if hot is None:
        print('未探到热更点（没有"15 格内随机"日志）；请用 --hot 指定。')
        return 1
    pre_dist, post_dist, pre_state, post_state, kw, cnt = collect(data, hot, win, now)
    pre_min = win / 60.0
    post_min = max(1e-9, (min(now, hot + win) - hot) / 60.0)

    def q(a, p):
        a = sorted(a)
        return a[min(len(a) - 1, int(len(a) * p))] if a else -1

    def fmt_dist(a, mins):
        if not a:
            return 'n=0'
        return 'n=%d (%.1f/分钟) 中位=%.0f p25=%.0f p75=%.0f p90=%.0f <60px=%.1f%% <100px=%.1f%%' % (
            len(a), len(a) / mins, q(a, .5), q(a, .25), q(a, .75), q(a, .9),
            100.0 * sum(1 for v in a if v < 60) / len(a),
            100.0 * sum(1 for v in a if v < 100) / len(a))

    print('热更点 ts=%d (%s)，前后窗口各 %.1f 分钟；后窗口实际 %.1f 分钟' % (
        hot, time.strftime('%Y-%m-%d %H:%M:%S', time.localtime(hot)), pre_min, post_min))
    print()
    print('=== 1) 站位点距钟馗(1728,1056) 像素距离 ===')
    print('旧版(7 格) :', fmt_dist(pre_dist, pre_min))
    print('新版(15 格):', fmt_dist(post_dist, post_min))

    def fresh(d, edge):
        return {a: v for a, v in d.items() if edge - v[0] <= 180}

    print()
    print('=== 2) 图 24 贴脸号数（每号取窗口末尾快照，只计新鲜 ≤180s 的活跃号）===')
    for name, d, edge in (('热更前', pre_state, hot), ('热更后', post_state, min(now, hot + win))):
        f = fresh(d, edge)
        c60 = sum(1 for ts, p in f.values() if math.hypot(p[0] - NPC[0], p[1] - NPC[1]) < 60)
        c100 = sum(1 for ts, p in f.values() if math.hypot(p[0] - NPC[0], p[1] - NPC[1]) < 100)
        print('%s: 活跃图24号=%d, <60px=%d(%.1f%%), <100px=%d' % (
            name, len(f), c60, 100.0 * c60 / max(1, len(f)), c100))

    print()
    print('=== 3) 异常率（条/分钟，窗口归一）===')
    print('warn : 前=%.1f 后=%.1f | error: 前=%.2f 后=%.2f' % (
        cnt['pre.warn'] / pre_min, cnt['post.warn'] / post_min,
        cnt['pre.err'] / pre_min, cnt['post.err'] / post_min))
    for k in KEY:
        print('%s: 前=%.1f 后=%.1f' % (
            k, kw.get('pre.' + k, 0) / pre_min, kw.get('post.' + k, 0) / post_min))
    return 0


if __name__ == '__main__':
    sys.exit(main())
