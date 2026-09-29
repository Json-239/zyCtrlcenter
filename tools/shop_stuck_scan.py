# -*- coding: utf-8 -*-
"""shop_stuck_scan.py — 只读：商店买药卡住专项扫描（2026-09-28 "552 死循环"口径）。

口径文档：docs/04-测试/分析-20260928-商店买药卡住排查.md（当日共发现三类循环）：
  · A 类(552 穷号)：穷号（reserve < 200×211=42200）买 200 个金创药被服务端 552 拒绝；
    552 无失败回传，daily_ghost 只能等 90s 超时 → 60s 冷却 → 每 ~150 秒一轮死循环。
    判定：买药启动≥3 且 通知码552≥3 且 买药超时(90s)≥1。
  · B 类(导航/未达)：跨图 4~5 跳或跳转被拒，90s 内到不了药店 → 超时循环（无 552）。
  · C 类(判定分裂，最大量级)：背包明明有药（执行器报"已满足, 未购买 ×N"），
    daily_ghost 复查却认为"无背包疗伤道具" → "买药完成但背包无药" → 60s 冷却循环。
    判定：买药完成但背包无药≥3 且 已满足,未购买≥3。

数据源（全只读）：
  A. F:/ZyBin/zyCtrlcenter/data/bot_logs/<账号>/runs_<date>.log（JSON Lines）
  B. http://127.0.0.1:28082/api/status（只读快照；可传 --status-file 用离线快照）
  C. 可选 --diag-dir：diag.log* 关键词计数（大文件流式读，仅计数）

用法：
  python tools/shop_stuck_scan.py                       # 扫今天(20260928)
  python tools/shop_stuck_scan.py --date 20260928
  python tools/shop_stuck_scan.py --diag-dir "<script 目录>"   # 顺带统计三份 diag
  python tools/shop_stuck_scan.py --status-file X.json  # 离线状态快照
输出：控制台摘要 + --json 明细（默认 tools/_tmp_scan/shop_stuck_scan.json）
红线：只读；不修改任何生产文件；不发送任何控制指令。
"""
import argparse
import io
import json
import os
import re
import sys
import time

BOT_LOGS = r"F:/ZyBin/zyCtrlcenter/data/bot_logs"
STATUS_API = "http://127.0.0.1:28082/api/status"
OUT_DEFAULT = r"F:/ZyBin/zyCtrlcenter/tools/_tmp_scan/shop_stuck_scan.json"
BUDGET = 200 * 211		# 一次采购 200 个金创药所需储备金（42200）

PATS = {
	"buy_start": "买药启动",
	"timeout90": "买药超时(90s)",
	"notice552": "通知码 552",
	"paid1110": "商店购买已扣储备金",
	"lowhp": "血量过低",
	"satisfied": "已满足, 未购买",		# C 类：执行器认为背包有药 → 跳过购买
	"done_nobag": "买药完成但背包无药",	# C 类：daily_ghost 复查仍认为无药 → 60s 冷却
}
TS_RE = re.compile(r'"ts":(\d+)')


def scan_account_file(path):
	cnt = {k: 0 for k in PATS}
	start_ts = []
	with io.open(path, encoding="utf-8", errors="ignore") as f:
		for line in f:
			hit = False
			for k, needle in PATS.items():
				if needle in line:
					cnt[k] += 1
					hit = True
			if hit and "买药启动" in line:
				m = TS_RE.search(line)
				if m:
					start_ts.append(int(m.group(1)))
	return cnt, start_ts


def hms(ts):
	try:
		return time.strftime("%H:%M:%S", time.localtime(int(ts)))
	except Exception:
		return "?"


def load_status(path):
	if path:
		with io.open(path, encoding="utf-8") as f:
			return json.load(f)
	try:
		import urllib.request
		with urllib.request.urlopen(STATUS_API, timeout=8) as r:
			return json.loads(r.read().decode("utf-8", "ignore"))
	except Exception as e:
		print("[warn] 无法获取 /api/status（%s），穷号清单跳过（可用 --status-file 离线快照）" % e)
		return None


def scan_diag(diag_dir):
	"""diag.log* 关键词计数（208MB 级大文件，流式逐行只做子串计数，不打印内容）。"""
	out = {}
	if not diag_dir or not os.path.isdir(diag_dir):
		return out
	for name in ("diag.log.2", "diag.log.1", "diag.log"):
		fp = os.path.join(diag_dir, name)
		if not os.path.isfile(fp):
			continue
		c = {"buy_start": 0, "notice552": 0, "timeout90": 0}
		try:
			with io.open(fp, encoding="utf-8", errors="ignore") as f:
				for line in f:
					if "买药启动" in line:
						c["buy_start"] += 1
					if "通知码 552" in line:
						c["notice552"] += 1
					if "买药超时(90s)" in line:
						c["timeout90"] += 1
		except Exception as e:
			c["err"] = str(e)
		out[name] = c
	return out


def main():
	ap = argparse.ArgumentParser(description="商店买药卡住专项扫描（只读）")
	ap.add_argument("--date", default=time.strftime("%Y%m%d"), help="日志日期 YYYYMMDD（默认今天）")
	ap.add_argument("--logs-dir", default=BOT_LOGS)
	ap.add_argument("--status-file", default="", help="离线 /api/status 快照 JSON")
	ap.add_argument("--diag-dir", default="", help="可选：顺带统计 diag.log*（传 script 目录）")
	ap.add_argument("--json", default=OUT_DEFAULT, help="明细 JSON 输出路径")
	args = ap.parse_args()

	suffix = "runs_%s.log" % args.date
	rows = []
	total = {k: 0 for k in PATS}
	for acc in sorted(os.listdir(args.logs_dir)):
		fp = os.path.join(args.logs_dir, acc, suffix)
		if not os.path.isfile(fp):
			continue
		cnt, starts = scan_account_file(fp)
		if not any(cnt.values()):
			continue
		rows.append({
			"account": acc, "counts": cnt,
			"first": hms(min(starts)) if starts else "", "last": hms(max(starts)) if starts else "",
			"rounds": cnt["buy_start"],
			"stuck_552": cnt["buy_start"] >= 3 and cnt["notice552"] >= 3 and cnt["timeout90"] >= 1,
			"stuck_cache": cnt["done_nobag"] >= 3 and cnt["satisfied"] >= 3,
		})
		for k, v in cnt.items():
			total[k] += v

	rows.sort(key=lambda r: (-(r["counts"]["notice552"] + r["counts"]["done_nobag"]), r["account"]))

	print("== 商店买药卡住扫描（date=%s）==" % args.date)
	print("[总览] 命中账号 %d；买药启动 %d；552 拒单 %d；买药超时 %d；无药空转 %d；已满足跳过 %d；扣款成功 %d；血量过低 %d" % (
		len(rows), total["buy_start"], total["notice552"], total["timeout90"],
		total["done_nobag"], total["satisfied"], total["paid1110"], total["lowhp"]))
	print("[号级] 账号 | 启动 | 552 | 超时 | 无药空转 | 已满足 | 扣款 | 首(启动) | 末(启动) | 判定")
	for r in rows:
		c = r["counts"]
		tag = []
		if r["stuck_552"]:
			tag.append("A类(552穷号)")
		if r["stuck_cache"]:
			tag.append("C类(判定分裂)")
		if not tag and (c["buy_start"] >= 3 and c["timeout90"] >= 1):
			tag.append("B类(导航/超时?)")
		print("  %s | %d | %d | %d | %d | %d | %d | %s | %s | %s" % (
			r["account"].split("@")[0], c["buy_start"], c["notice552"], c["timeout90"],
			c["done_nobag"], c["satisfied"], c["paid1110"], r["first"], r["last"],
			"+".join(tag) if tag else "-"))

	st = load_status(args.status_file)
	poor = []
	if st and isinstance(st.get("robots"), list):
		for rb in st["robots"]:
			rv = rb.get("reserve")
			if rv is None:
				continue
			if int(rv) < BUDGET:
				poor.append({"account": rb.get("account"), "reserve": int(rv),
					"level": rb.get("level"), "state": rb.get("state"), "mapid": rb.get("mapid")})
		poor.sort(key=lambda x: x["reserve"])
		print("[穷号] reserve < %d（买不起 200 个金创药）共 %d 个：" % (BUDGET, len(poor)))
		for p in poor:
			print("  %s reserve=%s lv=%s state=%s map=%s" % (
				str(p["account"]).split("@")[0], p["reserve"], p["level"], p["state"], p["mapid"]))
		stuck_acc = set(r["account"] for r in rows if r["stuck_552"] or r["stuck_cache"])
		inter = [p for p in poor if p["account"] in stuck_acc]
		if inter:
			print("[交集] 卡住循环 ∩ 穷号：%s（先治理这批）" % ", ".join(
				str(p["account"]).split("@")[0] for p in inter))

	diag = scan_diag(args.diag_dir)
	if diag:
		print("[diag] 分文件计数（买药启动/552/超时）：")
		for k, v in diag.items():
			print("  %s: %s" % (k, v))

	payload = {"date": args.date, "total": total, "accounts": rows, "poor_reserve": poor, "diag": diag}
	outdir = os.path.dirname(args.json)
	if outdir and not os.path.isdir(outdir):
		os.makedirs(outdir)
	with io.open(args.json, "w", encoding="utf-8", newline="\n") as f:
		f.write(json.dumps(payload, ensure_ascii=False, indent=2))
	print("[输出] %s" % args.json)


if __name__ == "__main__":
	main()
