# -*- coding: utf-8 -*-
"""给机器人部署目录的 config.py 调整「账号段起点」（只改数字字面量，其余内容原样保留）。

用途：为某个区准备**独立部署目录**时，避免多个 robot 进程登录同一批账号（会互踢）。

用法：
    python tools/patch_robot_config.py <部署目录> <起始序号> [密码]

示例：
    python tools/patch_robot_config.py deploy/zones/prod-240-2300 5502
        → config.py 里 robot_account = ["robot000", (5502, 6000), ...]

安全约束（与中控写 config.py 的口径一致）：
    - 只替换 `(旧起点, 旧终点)` 数字字面量（默认终点 6000），不动其它内容/注释；
    - 若形状不符合预期，直接报错退出，不猜。
"""
import io
import os
import re
import sys


def main():
    if len(sys.argv) < 3:
        print(__doc__)
        return 1
    deploy_dir = sys.argv[1]
    start_seq = sys.argv[2]
    if not start_seq.isdigit():
        print("起始序号必须是数字")
        return 1
    path = os.path.join(deploy_dir, "script", "config.py")
    if not os.path.exists(path):
        print("找不到 config.py: %s" % path)
        return 1

    src = io.open(path, encoding="utf-8").read()
    # 只匹配**行首（允许缩进）的生效赋值**，跳过 "#robot_account = ..." 这类注释行
    pat = re.compile(
        r'(?m)^([ \t]*robot_account[ \t]*=[ \t]*\[[ \t]*"[^"]*"[ \t]*,[ \t]*\([ \t]*)'
        r'(\d+)([ \t]*,[ \t]*)(\d+)([ \t]*\))')
    m = pat.search(src)
    if not m:
        print("未找到 robot_account 账号段（形状不符），未改动")
        return 1

    old = "%s, %s" % (m.group(2), m.group(4))
    new = "%s, %s" % (start_seq, m.group(4))
    if m.group(2) == start_seq:
        print("账号段起点已是 %s，无需改动" % start_seq)
        return 0

    out = src[:m.start(2)] + start_seq + src[m.end(2):]
    io.open(path, "w", encoding="utf-8", newline="").write(out)
    print("已调整账号段: (%s) -> (%s)  [%s]" % (old, new, path))
    return 0


if __name__ == "__main__":
    sys.exit(main())
