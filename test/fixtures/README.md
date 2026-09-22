# 报文夹具（test/fixtures）

> 规则单一事实源：[docs/04-测试/夹具规范.md](../../docs/04-测试/夹具规范.md)
> 与 [测试规范](../../docs/04-测试/测试规范.md) §3.4「必须有实际数据支撑」。

## 结构

```
fixtures/
├── README.md            本文件：夹具来源与规则
├── events/
│   ├── task_progress.json        真实抓包（runs_20260919.jsonl）
│   ├── robot_state.json          真实抓包
│   ├── error_task_stuck.json     真实抓包
│   ├── error_stuck_wait_next.json 构造（quest_engine.py:3185 看门狗上报格式 STUCK_<state>，real=false）
│   ├── ghost_offline.json        真实抓包
│   ├── chain_done.json           真实抓包
│   ├── log_dialog.json           真实抓包
│   ├── robot_manage_reply.json   真实抓包
│   ├── robot_online.json         构造（协议表字段，real=false）
│   └── status_reply.json         构造（文档示例报文，real=false，trimmed=true）
├── chains/
│   ├── newbie_full.mini.json    真实导出裁剪（data/chains/newbie_full.json；**基座链**：npcs 含钟馗 10146、map_grids 含图9/10、dijkstra 1 条）
│   ├── zhongkui_nav.mini.json   真实导出裁剪（data/chains/zhongkui_nav.json；**只留抓鬼专属** ghost_maps/ghost_map_pos/maps——坐标与网格由中控从基座组装）
│   └── zhuaogui_nav.sample.json 真实导出裁剪（data/chains/zhuaogui_nav.json，含未知字段与扁平 npcs 形状；透传与组装用例用）
├── maps/
│   └── maps.sample.json        真实 map.csv 裁剪（6 张图；完整 131 张见 data/maps.json）
├── accounts/
│   └── accounts.sample.json    真实账号库裁剪（3 个号 + 其在 47.96.8.240:2300 的可用状态）
├── gameproto/
│   └── frames.json             游戏服协议字节夹具（13 帧；hex 由参考实现 register_robot.py 实际打包）
└── robotcfg/
    ├── config_py.sample.json      机器人 config.py 样例（构造；验证"只改三个键"的写入逻辑）
    └── config_py_env.sample.json  现场 config.py 形状（构造；值为环境变量表达式 → 验证"跳过不改"）
```

> `fixtures/events/` 由元测试 `fixtures_test.go` 强制校验 provenance；
> `fixtures/chains/` 由 `test/chainlib` 的夹具加载器校验（必须 `real=true` 且有 `how`）；
> `fixtures/maps/` 由 `test/maplib` 的夹具加载器校验（必须 `real=true` 且有 `how`）；
> `fixtures/accounts/` 由 `test/accounts` 的夹具加载器校验（必须 `real=true` 且有 `how`）；
> `fixtures/gameproto/` 由 `test/gameproto` 的元测试校验（`real=true` + `how`，且**逐字节**重打包比对）；
> `fixtures/robotcfg/` 由 `test/zones` 的夹具加载器校验（必须带 `provenance.how`）。

## 每条夹具必须带 provenance

| 字段 | 含义 | 要求 |
|---|---|---|
| `real` | 是否真实抓包 | 必填（bool） |
| `trimmed` | 是否裁剪过内容 | 必填（bool）；真实报文裁剪必须说明裁剪范围 |
| `raw` | 原始行（未改动） | `real=true` 时必填（原样一行 JSON） |
| `how` | 来源/获取方式/构造依据 | 必填（非空） |
| `source` | 源文件路径或文档 | 建议填 |

- **构造数据必须显式 `real=false`**，并在 `how` 写清构造依据（哪份协议文档/哪张表）。
- 元测试 `test/fixtures/fixtures_test.go::TestFixtureProvenance` 会校验以上规则；新增夹具不满足即全量失败。

## 真实夹具怎么来的（可复现）

```powershell
# 从参考项目真实运行日志尾部抽取（2026-09-19）
$src = "F:/ZyBin/xm/2d-xiyou-server/robot/ctrlcenter/data/runs_20260919.jsonl"
Get-Content $src -Tail 20000 -Encoding UTF8 |
    Where-Object { $_ -match '"type":\s*"error"' } | Select-Object -First 1
```

> `status_reply` 在参考实现里**不落运行历史**（每 3 秒一条、量太大，只更新内存状态），
> 因此没有真实抓包可引，只能用文档示例构造并显式标注 `real=false`。

### 协议字节夹具（gameproto/frames.json）

游戏服协议是**二进制帧**，抓包不便；但"字节是否正确"恰恰是最关键的。
做法：用**参考实现**（`register_robot.py` 的 `build_frame` / `_pack_str_ex`）**实际打包**出 hex，
存进夹具（`provenance.real=true` + `how` 说明生成方式），再由 `test/gameproto` 用我们自己的编码器重打包
做**逐字节**比对。重新生成（参考项目目录下）：

```powershell
python -c "import json,sys; sys.path.insert(0,'.'); import register_robot as rr; print(rr.build_frame(255, [200], [4]).hex().upper())"
```

> 字符集说明：夹具为 UTF-8（当前环境统一 UTF-8）；`GBK` 在本实现里**明确报错**，
> 不会静默乱码，见 [游戏服探测协议](../../docs/03-协议/游戏服探测协议.md) §5。
