# -*- coding: utf-8 -*-
"""验收采样：每 N 秒抓一次 /api/status，只留目标号，落 CSV（UTF-8）。只读，不改任何状态。

用法: python tools/sample_status.py <账号> <次数> <间隔秒> [status_url]
示例: python tools/sample_status.py robot0001006@xy3.com 30 10            # 单轮 ≈5 分钟
      python tools/sample_status.py robot0001006@xy3.com 5 5 http://127.0.0.1:18082/api/status
来源: docs/04-测试/验证-20260923-大唐神捕-验收附录（日志契约与采样）.md §2（已实测可用）
"""
import csv, json, sys, time, urllib.request

if len(sys.argv) < 4:
    print(__doc__)
    sys.exit(2)

account = sys.argv[1]
n, gap = int(sys.argv[2]), int(sys.argv[3])
url = sys.argv[4] if len(sys.argv) > 4 else "http://127.0.0.1:28082/api/status"
cols = ["ts", "state", "task_index", "last_task", "mapid", "pos",
        "fight", "stuck_count", "err_code", "money"]
out = "sample_%s.csv" % account.split("@")[0]
with open(out, "w", newline="", encoding="utf-8") as fh:
    w = csv.writer(fh); w.writerow(cols)
    for _ in range(n):
        try:
            with urllib.request.urlopen(url, timeout=5) as r:
                d = json.load(r)
            bot = next((b for b in d.get("robots", []) if b.get("account") == account), None)
            if bot:
                w.writerow([int(time.time()), bot.get("state"), bot.get("task_index"),
                            bot.get("last_task"), bot.get("mapid"),
                            "(%s,%s)" % tuple(bot.get("pos") or (0, 0)),
                            bot.get("fight"), bot.get("stuck_count"),
                            bot.get("err_code") or "", bot.get("money")])
            else:
                w.writerow([int(time.time()), "NOT_FOUND"])
        except Exception as e:
            w.writerow([int(time.time()), "SAMPLE_ERR", str(e)[:60]])
        time.sleep(gap)
print("done -> %s（%d 行）" % (out, n))
