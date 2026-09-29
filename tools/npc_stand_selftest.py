# -*- coding: utf-8 -*-
"""对话站位随机化自检（针对"一堆机器人叠在钟馗身上，玩家点不到 NPC"）。

为什么不 import quest_engine：它依赖机器人运行时（marshal/cnetwork 等），离线 import 会失败。
这里同样是**源码级自检**：从产物文件里抽出 pick_stand_point / pick_stand_offset /
__stand_point 与相关常量直接执行，验的就是真正上线跑的那份代码。

2026-09-21 扩充：__stand_point 失败时会退化为"NPC 像素坐标 + 随机偏移"（网格不可用时的
兜底路径）。新增用例覆盖：偏移距离严格落在 [32,80]、角度/半径可注入、click_type!=0 返回 None。

2026-09-22（站位半径扩大，配合 quest_engine 常量同步 5→7）：
  ① 网格采样半径 [1,5] → [1,7] 格；兜底偏移 [32,80] → [32,112]px（上限 = 半径格数 × 16）；
  ② 新增"多出的两格确实会被用到"用例（固定随机种子 → 结果可复现，不靠运气）；
  ③ 新增"兜底上限 == 半径格数 × 16"耦合断言，防止以后单独改一处再改岔；
  ④ 用例④的阻挡盒 ±6 → ±8 格（随半径扩到 7；盒必须覆盖整个采样范围，否则盒外可走格被
     合法采到会让该用例误报）。
2026-09-21r2（配合两处修复同步）：
  ① 生产 __stand_point 改为函数内 `import robot_path`（原来引用全局名 → NameError）→ 替身
     需注册进 sys.modules，函数的局部 import 才能绑到它；
  ② 退化兜底 warn 加了"同账号 5 分钟一条"节流 → 抽取节流常量、在每个"应报 warn"的用例前
     清空节流表，并新增一条节流用例。
断言口径未放宽（原有各项判据一字未改，新增项只覆盖新行为）。

2026-09-29（钟馗站位改进，用户批准；现场"钟馗处仍很密集"——交付高峰 13~34 号/分钟）：
  ① A 半径 7 → 15 格，兜底偏移 [32,112] → [32,240]（上限 = 半径 × 16，旧耦合断言自动跟随）；
  ② A 外圈优先：采样半径改偏外分布 r = lo + (hi-lo) × u**0.5 → 新增"外半圈占比"分布断言
     （固定种子可复现；理论 ≈75%，阈值取 0.65 留容差）；
  ③ B 贴脸必走：号距 NPC < 48px 时候选点必须距当前位置 ≥ 32px（否则命中 __do_walk 的
     20px"已在目标附近"跳过捷径 → 号原地不动）。三层用例：pick_stand_point（硬约束）、
     __stand_point（贴脸判据生效、不贴脸不启用）、pick_stand_offset（锥内采样数学保证）；
  ④ C 站位避让：__stand_point 读同进程其它号位置（stub robot_mgr），采样避开 2 格内有号
     的点；软约束（全部命中时回退普通候选，不返回 None）。

用法：python npc_stand_selftest.py <quest_engine.py 路径>
"""
import hashlib
import math
import random
import re
import sys
import types

path = sys.argv[1] if len(sys.argv) > 1 else 'quest_engine.py'
src = open(path, encoding='utf-8').read()

CONSTS = ['STAND_RADIUS_CELLS', 'STAND_MIN_CELLS', 'STAND_TRIES',
          'STAND_OUTER_POW', 'STAND_CLOSE_PX', 'STAND_MIN_FROM_DIST_PX', 'STAND_AVOID_PEER_PX',
          'STAND_OFFSET_MIN_PX', 'STAND_OFFSET_MAX_PX', '_OFFSET_ROUND_EPS',
          'STAND_FB_WARN_GAP_MS', '_STAND_FB_WARN_NEXT_MS']
FUNCS = ['pick_stand_point', 'pick_stand_offset', '__peer_avoid_points', '__stand_point']

parts = ['import random', 'import math', 'import time']
for name in CONSTS:
    m = re.search(r'(?m)^%s\s*=.*$' % re.escape(name), src)
    assert m, '缺少常量 %s' % name
    parts.append(m.group(0))
for fn in FUNCS:
    m = re.search(r'(?ms)^def %s\(.*?(?=^\S)' % re.escape(fn), src)
    assert m, '缺少函数 %s' % fn
    parts.append(m.group(0))
ns = {}
exec('\n'.join(parts), ns)

CELL = 16


class FakeGrid(object):
    """假网格：w*h 全可走，blocked_set 里的格子阻挡；坐标 → 格中心。"""

    def __init__(self, w=60, h=60, blocked_set=()):
        self.w, self.h = w, h
        self.blocked_set = set(blocked_set)

    def blocked(self, gx, gy):
        if gx < 0 or gy < 0 or gx >= self.w or gy >= self.h:
            return True
        return (gx, gy) in self.blocked_set

    def to_coord(self, gx, gy):
        return gx * CELL + CELL // 2, gy * CELL + CELL // 2


fails = []
checks = []


def check(name, ok, detail=''):
    checks.append(name)
    print('[%s] %s %s' % ('PASS' if ok else 'FAIL', name, detail))
    if not ok:
        fails.append(name)


print('自检目标: %s (sha1=%s)' % (path, hashlib.sha1(src.encode('utf-8')).hexdigest()[:12]))
pick = ns['pick_stand_point']
RADIUS, MINC, TRIES = ns['STAND_RADIUS_CELLS'], ns['STAND_MIN_CELLS'], ns['STAND_TRIES']
OUTER_POW = ns['STAND_OUTER_POW']
CLOSE_PX, MIN_FROM = ns['STAND_CLOSE_PX'], ns['STAND_MIN_FROM_DIST_PX']
AVOID_PX = ns['STAND_AVOID_PEER_PX']

# ① 常量口径（2026-09-29：半径 7→15；新增外圈/贴脸/避让常量）
check('常量: 半径 15 格 / 最小 1 格 / 采样 24 次',
      (RADIUS, MINC, TRIES) == (15, 1, 24), '= (%s, %s, %s)' % (RADIUS, MINC, TRIES))
check('常量: 外圈 pow=0.5 / 贴脸 48px / 贴脸下限 32px / 避让 32px',
      (OUTER_POW, CLOSE_PX, MIN_FROM, AVOID_PX) == (0.5, 48, 32, 32),
      '= (%s, %s, %s, %s)' % (OUTER_POW, CLOSE_PX, MIN_FROM, AVOID_PX))
check('贴脸下限 32 > walk_synced_skip_dist(20)（否则被"已在目标附近"跳过 ≠ 必走）',
      MIN_FROM > 20, '=%s' % MIN_FROM)

# ② 落在 [1, 15] 格内、不是 NPC 格、返回的是格中心
g = FakeGrid()
gx0, gy0 = 30, 30
bad = []
for _ in range(300):
    pt = pick(g, gx0, gy0)
    if pt is None:
        bad.append('None')
        continue
    px, py = pt
    gx, gy = px // CELL, py // CELL
    d_cells = math.hypot(gx - gx0, gy - gy0)   # 玩家视角：格距
    if not (MINC <= d_cells <= RADIUS):
        bad.append('格距 %s' % round(d_cells, 2))
    if (gx, gy) == (gx0, gy0):
        bad.append('站到 NPC 格')
    if px % CELL != CELL // 2 or py % CELL != CELL // 2:
        bad.append('不是格中心')
check('300 次采样：格距都在 1~15 格、都不是 NPC 格、都是格中心', not bad, ('问题: %s' % bad[:3]) if bad else '')

# ③ 分散性：300 次落在不同格的数量（证明"不叠在一起"）
spots = set()
for _ in range(300):
    pt = pick(g, gx0, gy0)
    spots.add((pt[0] // CELL, pt[1] // CELL)) if pt else None
check('落点分散（300 次 ≥ 60 个不同格）', len(spots) >= 60, '不同格数=%d' % len(spots))

# ③b 2026-09-22 新增（2026-09-29 半径口径更新）：新半径上限附近的格确实会被采样到。
#   用固定种子 random.Random(20260922) 注入 rand → 结果**可复现**（不受全局随机状态/平台影响）。
#   外圈优先分布下 14~15 格的理论占比 ≈ 13.7%（u ≥ 0.863），300 次期望 ≈ 41 次。
rng = random.Random(20260922)
far = 0
for _ in range(300):
    pt = pick(FakeGrid(), gx0, gy0, rand=rng.random)
    if pt == None:
        continue
    if 14 <= math.hypot(pt[0] // CELL - gx0, pt[1] // CELL - gy0) <= RADIUS:
        far += 1
check('半径 +8 有效：300 次采样有落点落在 14~15 格（固定种子，可复现）', far >= 1, '14~15 格次数=%d' % far)

# ③c 2026-09-29 新增：**外圈优先** —— 实际落点在外半圈（格距 ≥ radius/2 = 7.5 格）的占比。
#   理论：r = lo+(hi-lo)×u**0.5，(r-lo)/(hi-lo) ≥ 0.5 ⟺ u ≥ 0.25 → 采样占比 75%；
#   斜角越界拒绝集中在最外环带（约 10% 采样），对占比影响有限 → 断言阈值 0.65 留容差。
rng = random.Random(20260929)
outer = 0
total = 0
for _ in range(800):
    pt = pick(FakeGrid(200, 200), 100, 100, rand=rng.random)
    if pt == None:
        continue
    total += 1
    if math.hypot(pt[0] // CELL - 100, pt[1] // CELL - 100) >= RADIUS / 2.0:
        outer += 1
ratio = (outer / float(total)) if total else -1
check('外圈优先：800 次落点外半圈(≥7.5 格)占比 ≥ 0.65', total > 0 and ratio >= 0.65,
      '占比=%.3f (%d/%d)' % (ratio, outer, total))

# ④ 只在可走格落点：把 NPC 周围一整块设为阻挡，只留几格可走
#   2026-09-22: 阻挡盒从 ±6 格扩到 ±8 格（半径 5→7）；2026-09-29 再扩到 ±16 格
#   （半径 7→15）—— 盒必须覆盖整个采样范围，否则盒外（FakeGrid 默认可走）的格子会被合法采到。
allow = {(gx0 + 3, gy0), (gx0 - 2, gy0 + 1), (gx0, gy0 + 4)}
blocked_all = {(x, y) for x in range(gx0 - 16, gx0 + 17) for y in range(gy0 - 16, gy0 + 17) if (x, y) not in allow}
g2 = FakeGrid(blocked_set=blocked_all)
hits = {pick(g2, gx0, gy0) for _ in range(60)}
hits.discard(None)
check('只落在允许的可走格上', hits and hits <= set((x * CELL + 8, y * CELL + 8) for x, y in allow), '落点=%s' % sorted(hits)[:3])

# ⑤ 周围全阻挡 → 返回 None（调用方保留原目标，行为不变）
g3 = FakeGrid(blocked_set={(x, y) for x in range(gx0 - 16, gx0 + 17) for y in range(gy0 - 16, gy0 + 17)})
check('无可走格时返回 None（不硬造点）', pick(g3, gx0, gy0) is None)

# ⑥ 注入 rand → 结果确定（同一序列两次结果一致）
seq = [0.1, 0.9, 0.35, 0.6, 0.05, 0.7]
r1 = pick(FakeGrid(), gx0, gy0, rand=lambda: seq.pop(0))
seq2 = [0.1, 0.9, 0.35, 0.6, 0.05, 0.7]
r2 = pick(FakeGrid(), gx0, gy0, rand=lambda: seq2.pop(0))
check('可注入随机数（同序列结果一致）', r1 == r2 and r1 is not None, 'r=%s' % (r1,))

# ⑥b 2026-09-29 新增：贴脸必走（硬约束）—— 传 from_xy + min_from_dist_px 后，
#   所有返回点距 from_xy ≥ 32px（号贴在 NPC 格中心正北 8px；400 次采样无一违例才通过）。
from_xy = (100 * CELL + 8, 100 * CELL + 8 - 8)   # 距 NPC 格中心 8px（贴脸）
bad = []
got = 0
for _ in range(400):
    pt = pick(FakeGrid(200, 200), 100, 100, from_xy=from_xy, min_from_dist_px=32)
    if pt == None:
        bad.append('None')
        continue
    got += 1
    if math.hypot(pt[0] - from_xy[0], pt[1] - from_xy[1]) < 32:
        bad.append(pt)
check('贴脸必走（采样）：400 次返回点都距当前位置 ≥32px', not bad and got == 400,
      ('违例: %s' % bad[:3]) if bad else '全部 %d 次通过' % got)

# ⑥c 2026-09-29 新增：站位避让（软约束）—— avoid_pts 附近 32px 内不给点；避不开时保底。
#   正常可达场景：800 次采样都应避开 peer（软约束只有在"全部尝试都被占"时才回退）。
peer = ((100 * CELL + 8 + 16, 100 * CELL + 8), (100 * CELL + 8 - 16, 100 * CELL + 8 - 32))
bad = []
for _ in range(800):
    pt = pick(FakeGrid(200, 200), 100, 100, avoid_pts=peer, avoid_r=32)
    if pt == None:
        bad.append('None')
        continue
    for ap in peer:
        if math.hypot(pt[0] - ap[0], pt[1] - ap[1]) < 32:
            bad.append(pt)
check('站位避让：800 次采样都避开 peer 32px 邻域（软约束在可达时须完全生效）', not bad,
      ('违例: %s' % bad[:3]) if bad else '')

# ⑥d 2026-09-29 新增：避让软约束保底 —— avoid_r 大到"所有候选都被视为占用"（极端拥挤）
#   时**回退普通候选而不返回 None**（软约束不能把号留在人堆里；硬约束只有贴脸必走）。
pt = pick(FakeGrid(200, 200), 100, 100, avoid_pts=((100 * CELL + 8, 100 * CELL + 8),), avoid_r=2000)
check('避让软约束保底：全部候选中被占 → 仍返回普通候选（≠ None）', pt is not None, 'pt=%s' % (pt,))

# ⑥e 2026-09-29 新增：avoid_pts 空/None 时行为与旧版一致（不触发避让逻辑，返回正常点）
p_none = pick(FakeGrid(200, 200), 100, 100, avoid_pts=None, avoid_r=32)
p_empty = pick(FakeGrid(200, 200), 100, 100, avoid_pts=(), avoid_r=32)
check('avoid_pts 为空 → 不启用避让（正常返回点）', p_none is not None and p_empty is not None,
      '=%s / %s' % (p_none, p_empty))

# ---------------------------------------------------------------------------
# 以下为 2026-09-21 新增：偏移兜底（__stand_point 网格失败时的退化路径）
# ---------------------------------------------------------------------------

off = ns['pick_stand_offset']
OFF_MIN, OFF_MAX = ns['STAND_OFFSET_MIN_PX'], ns['STAND_OFFSET_MAX_PX']

# ⑦ 偏移距离严格落在 [32, 240]（含像素 round 取整误差；NPC 像素用钟馗的 1728,1056）
bad = []
for i in range(400):
    ox, oy = off(1728, 1056, rand=lambda i=i: i / 399.0)
    d = math.hypot(ox - 1728, oy - 1056)
    if not (OFF_MIN <= d <= OFF_MAX):
        bad.append(round(d, 2))
check('偏移兜底 400 次：实际距离都在 [32,240]px', not bad, ('越界: %s' % bad[:3]) if bad else '')

# ⑦b 2026-09-29 新增：贴脸必走（兜底路径）—— 传 from_xy(=号位置) 时，点必须距 from_xy ≥ 32px。
#   NPC(1728,1056)，号(1740,1060)（距 NPC 12.6px < 48 → 贴脸）：锥内采样 + r 下限 d+32。
#   同时复核"距 NPC 仍在 [32,240]"（新约束不得破坏旧口径）。
bad = []
badv = []
for i in range(400):
    ox, oy = off(1728, 1056, rand=lambda i=i: (i * 7 % 400) / 399.0,
                 from_xy=(1740, 1060), min_from_dist_px=32)
    df = math.hypot(ox - 1740, oy - 1060)
    if df < 32:
        bad.append(round(df, 2))
    dv = math.hypot(ox - 1728, oy - 1056)
    if not (OFF_MIN <= dv <= OFF_MAX):
        badv.append(round(dv, 2))
check('贴脸必走（兜底）：400 次都距当前位置 ≥32px，且距 NPC 仍在 [32,240]px',
      not bad and not badv, ('距号越界: %s 距NPC越界: %s' % (bad[:3], badv[:3])) if (bad or badv) else '')

# ⑧ 角度可注入 + 结果确定（ang=0 + 半径取下限侧 → 只朝 +x 偏移）
h1 = off(1000, 1000, rand=lambda: 0.0, ang=0.0)
h2 = off(1000, 1000, rand=lambda: 0.0, ang=0.0)
check('偏移兜底：角度可注入、同参数结果一致', h1 == h2 and h1[1] == 1000 and h1[0] > 1000, '=%s' % (h1,))

# ⑨ 下限口径：32 > config.walk_synced_skip_dist(20)（偏移后不会被"已在目标附近"跳过走路）
check('偏移下限 32 > walk_synced_skip_dist(20)', OFF_MIN > 20, '=(%s,%s)' % (OFF_MIN, OFF_MAX))

# ⑨b 2026-09-22 新增耦合断言：兜底像素上限 == 网格采样半径格数 × 16（格边长）。
#   否则只改一处时会出现"网格可用走 7 格、网格不可用只散 5 格"的两套现场口径。
check('兜底上限 == 半径格数 × 16px', OFF_MAX == RADIUS * CELL, '= %s vs %s×%s' % (OFF_MAX, RADIUS, CELL))

# ⑩ __stand_point 分支：无网格/采样失败 → 兜底偏移 + warn；战斗类/无坐标 → None 不打扰
sp = ns['__stand_point']
NPC = [24, 1728, 1056]


class StubMapGrid(object):
    """robot_path.MapGrid 的替身：把 FakeGrid 包一层（只用到 to_grid/blocked/to_coord）。"""

    def __init__(self, mapid, grid):
        self.grid = grid

    def to_grid(self, x, y):
        return int(x) // CELL, int(y) // CELL

    def blocked(self, gx, gy):
        return self.grid.blocked(gx, gy)

    def to_coord(self, gx, gy):
        return self.grid.to_coord(gx, gy)


class StubRobotPath(object):
    MapGrid = StubMapGrid


grid_box = {'data': None}
emitted = []
# 生产 __stand_point 现在在函数内 `import robot_path`（局部 import）→ 该 import 走 sys.modules，
# 所以替身注册到 sys.modules（ns['robot_path'] 对局部 import 无效，仅对旧式全局引用有效）。
sys.modules['robot_path'] = StubRobotPath
ns['robot_path'] = StubRobotPath
ns['__chain_grid_for'] = lambda quest, mapid: grid_box['data']
FB_NEXT = ns['_STAND_FB_WARN_NEXT_MS']   # 兜底 warn 节流表：账号 → 下次可报时刻(ms)


def _emit(robot_object, ev):
    emitted.append(ev)


ns['__emit'] = _emit

emitted[:] = []
FB_NEXT.clear()   # r2: 兜底 warn 有 5 分钟节流，逐例清空以保证本用例必报
grid_box['data'] = None
r = sp(None, None, NPC, 10147, 0)
d = math.hypot(r[1] - 1728, r[2] - 1056) if r else -1
warn = emitted[0].get('msg', '') if emitted else ''
check('__stand_point 无网格 → 偏移兜底[24,x,y] + warn(无网格, 带 npc_id/图号)',
      bool(r) and r[0] == 24 and OFF_MIN <= d <= OFF_MAX
      and bool(emitted) and emitted[0].get('level') == 'warn' and '无网格' in warn and '10147' in warn,
      'r=%s warn=%s' % (r, warn[:46]))

emitted[:] = []
FB_NEXT.clear()
# 2026-09-29: 采样失败盒随半径 7→15 扩到覆盖整个采样范围（NPC 格 (108,66) ± 15 格）。
grid_box['data'] = FakeGrid(200, 200, {(x, y) for x in range(93, 124) for y in range(51, 82)})
r = sp(None, None, NPC, 10147, 0)
d = math.hypot(r[1] - 1728, r[2] - 1056) if r else -1
warn = emitted[0].get('msg', '') if emitted else ''
check('__stand_point 采样失败 → 偏移兜底 + warn(采样失败)',
      bool(r) and OFF_MIN <= d <= OFF_MAX and '采样失败' in warn, 'r=%s warn=%s' % (r, warn[:46]))

# ⑪b r2 新增：兜底 warn 的同账号节流（只压"重复上报"，不改落点）
class StubRobot(object):
    def __init__(self, acct):
        self.m_account = [acct]


emitted[:] = []
FB_NEXT.clear()
r1 = sp(StubRobot('robot_a@x'), None, NPC, 10147, 0)
r2_ = sp(StubRobot('robot_a@x'), None, NPC, 10147, 0)
r3 = sp(StubRobot('robot_b@x'), None, NPC, 10147, 0)
check('兜底 warn 节流：同账号 5 分钟内只报 1 条、换账号照报（落点不受影响）',
      len(emitted) == 2 and bool(r1) and bool(r2_) and bool(r3)
      and ns['STAND_FB_WARN_GAP_MS'] == 300000,
      'warn=%d 条 / 窗口=%dms' % (len(emitted), ns['STAND_FB_WARN_GAP_MS']))

emitted[:] = []
grid_box['data'] = FakeGrid(200, 200)
r = sp(None, None, NPC, 10147, 0)
ok = bool(r) and r[0] == 24 and not emitted
if ok:
    gx, gy = r[1] // CELL, r[2] // CELL
    ok = 1 <= math.hypot(gx - 108, gy - 66) <= 15 and r[1] % CELL == CELL // 2 and r[2] % CELL == CELL // 2
check('__stand_point 有网格 → 优先网格采样（1~15 格中心）且不发 warn', ok, 'r=%s' % (r,))

# ⑪d 2026-09-29 新增 B（__stand_point 层）：号贴脸（距 NPC 8px）时，网格采样返回点
#   必须距号当前位置 ≥32px（否则命中 __do_walk 的 20px 跳过捷径 → 号原地不动）。
class StubRobotPose(object):
    def __init__(self, mapid, x, y, acct='a@x'):
        self.m_mapid = mapid
        self.m_pose = [x, y]
        self.m_account = [acct]


emitted[:] = []
grid_box['data'] = FakeGrid(200, 200)
rob_close = StubRobotPose(24, 1728, 1064)     # 距 NPC(1728,1056) 8px → 贴脸
ok = True
r = None
for _ in range(200):
    r = sp(rob_close, None, NPC, 10147, 0)
    if not r or math.hypot(r[1] - 1728, r[2] - 1064) < 32:
        ok = False
        break
check('__stand_point 贴脸：200 次返回点都距当前位置 ≥32px', ok, 'r=%s' % (r,))

# ⑪d2 边界：号在别的图（m_mapid≠NPC 图）→ 不启用贴脸约束（跨图坐标不可比），
#   且返回点仍是合法的 1~15 格内点。
rob_other = StubRobotPose(11, 1728, 1064)
ok = True
for _ in range(60):
    r = sp(rob_other, None, NPC, 10147, 0)
    if not r or r[0] != 24:
        ok = False
        break
    if not (1 <= math.hypot(r[1] // CELL - 108, r[2] // CELL - 66) <= 15):
        ok = False
        break
check('__stand_point 跨图号：不启用贴脸约束（防别图坐标误算），落点仍 1~15 格', ok, 'r=%s' % (r,))

# ⑪e 2026-09-29 新增 C（__stand_point 层）：站位避让 —— stub robot_mgr 注入同图 peer。
class StubMgr(object):
    def __init__(self, robots):
        self.m_robots = robots


_rm = types.ModuleType('robot_mgr')
_my_ro = StubRobotPose(24, 9999, 9999, 'me@x')      # 自己（图 24，离 NPC 很远 → 不贴脸）
_peer_ro = StubRobotPose(24, 1788, 1056, 'peer@x')  # 同图 peer（距 NPC 60px）
_other_ro = StubRobotPose(11, 1728, 1056, 'o@x')    # 别图号（不参与避让）
_rm.g_mgr = StubMgr({1: _my_ro, 2: _peer_ro, 3: _other_ro})
sys.modules['robot_mgr'] = _rm
pap = ns['__peer_avoid_points']
pts = pap(_my_ro, 24)
check('__peer_avoid_points：只收集同图其它号（排除自己/跨图号）',
      pts == [(1788, 1056)], 'pts=%s' % (pts,))
pts2 = pap(None, 24)
check('__peer_avoid_points：robot_object=None（自检场景）→ 同图号都在、跨图号不在',
      sorted(pts2) == sorted([(9999, 9999), (1788, 1056)]), 'pts=%s' % (pts2,))
emitted[:] = []
ok = True
r = None
for _ in range(200):
    r = sp(_my_ro, None, NPC, 10147, 0)
    if not r or math.hypot(r[1] - 1788, r[2] - 1056) < 32:
        ok = False
        break
check('__stand_point 站位避让：200 次返回点都避开同图 peer 的 32px 邻域', ok, 'r=%s' % (r,))
sys.modules.pop('robot_mgr', None)   # 清 stub，避免影响后续用例（保持用例独立）

emitted[:] = []
grid_box['data'] = None
r1 = sp(None, None, NPC, 10147, 2)
r2 = sp(None, None, None, 10147, 0)
check('__stand_point 战斗类(click_type=2)/无坐标 → 返回 None 且不发日志',
      r1 is None and r2 is None and not emitted)

print('\n结果：%d 项，失败 %d 项' % (len(checks), len(fails)))
sys.exit(1 if fails else 0)
