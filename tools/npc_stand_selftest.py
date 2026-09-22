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

用法：python npc_stand_selftest.py <quest_engine.py 路径>
"""
import hashlib
import math
import random
import re
import sys

path = sys.argv[1] if len(sys.argv) > 1 else 'quest_engine.py'
src = open(path, encoding='utf-8').read()

CONSTS = ['STAND_RADIUS_CELLS', 'STAND_MIN_CELLS', 'STAND_TRIES',
          'STAND_OFFSET_MIN_PX', 'STAND_OFFSET_MAX_PX', '_OFFSET_ROUND_EPS',
          'STAND_FB_WARN_GAP_MS', '_STAND_FB_WARN_NEXT_MS']
FUNCS = ['pick_stand_point', 'pick_stand_offset', '__stand_point']

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

# ① 常量口径
check('常量: 半径 7 格 / 最小 1 格 / 采样 24 次',
      (RADIUS, MINC, TRIES) == (7, 1, 24), '= (%s, %s, %s)' % (RADIUS, MINC, TRIES))

# ② 落在 [1, 7] 格内、不是 NPC 格、返回的是格中心
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
check('300 次采样：格距都在 1~7 格、都不是 NPC 格、都是格中心', not bad, ('问题: %s' % bad[:3]) if bad else '')

# ③ 分散性：300 次落在不同格的数量（证明"不叠在一起"）
spots = set()
for _ in range(300):
    pt = pick(g, gx0, gy0)
    spots.add((pt[0] // CELL, pt[1] // CELL)) if pt else None
check('落点分散（300 次 ≥ 60 个不同格）', len(spots) >= 60, '不同格数=%d' % len(spots))

# ③b 2026-09-22 新增：多出的两格（6~7 格）确实会被采样到 —— 否则"半径 5→7"只是纸面改动。
#   用固定种子 random.Random(20260922) 注入 rand → 结果**可复现**（不受全局随机状态/平台影响），
#   规避"随机偶发不稳定"。连续采样 300 次统计落在 6~7 格的次数（理论占比 ≈ (7-6)/(7-1) = 1/6）。
rng = random.Random(20260922)
far = 0
for _ in range(300):
    pt = pick(FakeGrid(), gx0, gy0, rand=rng.random)
    if pt == None:
        continue
    if 6 <= math.hypot(pt[0] // CELL - gx0, pt[1] // CELL - gy0) <= RADIUS:
        far += 1
check('半径 +2 有效：300 次采样有落点落在 6~7 格（固定种子，可复现）', far >= 1, '6~7 格次数=%d' % far)

# ④ 只在可走格落点：把 NPC 周围一整块设为阻挡，只留几格可走
#   2026-09-22: 阻挡盒从 ±6 格扩到 ±8 格 —— 半径 5→7 后采样范围变大，盒若不覆盖整个采样
#   范围，盒外（FakeGrid 默认可走）的格子会被合法采到并使本用例误报。
allow = {(gx0 + 3, gy0), (gx0 - 2, gy0 + 1), (gx0, gy0 + 4)}
blocked_all = {(x, y) for x in range(gx0 - 8, gx0 + 9) for y in range(gy0 - 8, gy0 + 9) if (x, y) not in allow}
g2 = FakeGrid(blocked_set=blocked_all)
hits = {pick(g2, gx0, gy0) for _ in range(60)}
hits.discard(None)
check('只落在允许的可走格上', hits and hits <= set((x * CELL + 8, y * CELL + 8) for x, y in allow), '落点=%s' % sorted(hits)[:3])

# ⑤ 周围全阻挡 → 返回 None（调用方保留原目标，行为不变）
g3 = FakeGrid(blocked_set={(x, y) for x in range(gx0 - 7, gx0 + 8) for y in range(gy0 - 7, gy0 + 8)})
check('无可走格时返回 None（不硬造点）', pick(g3, gx0, gy0) is None)

# ⑥ 注入 rand → 结果确定（同一序列两次结果一致）
seq = [0.1, 0.9, 0.35, 0.6, 0.05, 0.7]
r1 = pick(FakeGrid(), gx0, gy0, rand=lambda: seq.pop(0))
seq2 = [0.1, 0.9, 0.35, 0.6, 0.05, 0.7]
r2 = pick(FakeGrid(), gx0, gy0, rand=lambda: seq2.pop(0))
check('可注入随机数（同序列结果一致）', r1 == r2 and r1 is not None, 'r=%s' % (r1,))

# ---------------------------------------------------------------------------
# 以下为 2026-09-21 新增：偏移兜底（__stand_point 网格失败时的退化路径）
# ---------------------------------------------------------------------------

off = ns['pick_stand_offset']
OFF_MIN, OFF_MAX = ns['STAND_OFFSET_MIN_PX'], ns['STAND_OFFSET_MAX_PX']

# ⑦ 偏移距离严格落在 [32, 112]（含像素 round 取整误差；NPC 像素用钟馗的 1728,1056）
bad = []
for i in range(400):
    ox, oy = off(1728, 1056, rand=lambda i=i: i / 399.0)
    d = math.hypot(ox - 1728, oy - 1056)
    if not (OFF_MIN <= d <= OFF_MAX):
        bad.append(round(d, 2))
check('偏移兜底 400 次：实际距离都在 [32,112]px', not bad, ('越界: %s' % bad[:3]) if bad else '')

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
grid_box['data'] = FakeGrid(200, 200, {(x, y) for x in range(98, 119) for y in range(56, 77)})
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
    ok = 1 <= math.hypot(gx - 108, gy - 66) <= 7 and r[1] % CELL == CELL // 2 and r[2] % CELL == CELL // 2
check('__stand_point 有网格 → 优先网格采样（1~7 格中心）且不发 warn', ok, 'r=%s' % (r,))

emitted[:] = []
grid_box['data'] = None
r1 = sp(None, None, NPC, 10147, 2)
r2 = sp(None, None, None, 10147, 0)
check('__stand_point 战斗类(click_type=2)/无坐标 → 返回 None 且不发日志',
      r1 is None and r2 is None and not emitted)

print('\n结果：%d 项，失败 %d 项' % (len(checks), len(fails)))
sys.exit(1 if fails else 0)
