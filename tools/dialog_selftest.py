# -*- coding: utf-8 -*-
"""对话模块自检（针对"钟馗对话卡死"的真实案例）。

为什么不 import daily_ghost：它依赖机器人运行时（cnetwork 等扩展），离线 import 会失败。
这里改成**源码级自检** —— 从产物文件里抽出"判定用的常量 + 两个纯函数"直接执行，
验的就是真正上线跑的那份代码。

用法：python dialog_selftest.py <daily_ghost.py 路径>
"""
import hashlib
import re
import sys

path = sys.argv[1] if len(sys.argv) > 1 else 'daily_ghost.py'
src = open(path, encoding='utf-8').read()

CONSTS = ['BROKER_NODLG_LIMIT', 'DIALOG_CLOSE_AFTER_MS', 'DIALOG_CLOSE_TRIES',
          '_GHOST_DIALOG_KEYS', '_DIALOG_CLOSE_KEYS']
FUNCS = ['dialog_is_ours', 'pick_dialog_close_option', 'pick_ghost_dialog_close']

parts = []
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

CASES = [
    # 实测卡死那条：引导对话/双倍菜单（无抓鬼业务词，但有服务端关闭项）
    ([["续领双倍时间", 0], ["我什么都不想做", 0]], False, 1),
    # 钟馗接取菜单：有业务词 → 交给业务逻辑，绝不能点关闭
    ([["领取助战令", 0], ["接受捉鬼任务", 0], ["我什么都不想做", 0]], True, None),
    # 钟馗已有任务菜单（放弃捉鬼）
    ([["放弃捉鬼任务", 0], ["我什么都不想做", 0]], True, None),
    # 挑战类对话：无关闭项 → 不动它（仍走旧路径）
    ([["进行挑战", 0], ["我准备好了", 0]], False, None),
    ([["离开", 0]], False, 0),
    # 不可点的选项不算
    ([["我什么都不想做", 1]], False, None),
    ([], False, None),
]

fails = 0
print('自检目标: %s (sha1=%s)' % (path, hashlib.sha1(src.encode('utf-8')).hexdigest()[:12]))
for opts, want_ours, want_close in CASES:
    ours = ns['dialog_is_ours'](opts)
    close = ns['pick_ghost_dialog_close'](opts)   # 生产判据（唯一入口）
    ok = (ours == want_ours) and (close == want_close)
    fails += 0 if ok else 1
    print('[%s] %s → ours=%s(期望%s) 点关闭项=%s(期望%s)'
          % ('PASS' if ok else 'FAIL', opts, ours, want_ours, close, want_close))

for name, want in (('BROKER_NODLG_LIMIT', 8), ('DIALOG_CLOSE_AFTER_MS', 8000), ('DIALOG_CLOSE_TRIES', 2)):
    got = ns[name]
    ok = got == want
    fails += 0 if ok else 1
    print('[%s] %s = %s（期望 %s）' % ('PASS' if ok else 'FAIL', name, got, want))

print('\n结果：%d 项，失败 %d 项' % (len(CASES) + 3, fails))
sys.exit(1 if fails else 0)
