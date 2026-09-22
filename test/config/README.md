# config 测试说明

## 被测对象
- 模块：`internal/config/config.go`
- 关键函数：`Default` / `Load`（优先级 CLI > 环境变量 > config.local.json > 内置默认）

## 测试文件
| 文件 | 覆盖场景 |
|---|---|
| `config_test.go` | 默认端口与路径、CLI 覆盖、环境变量、优先级、占位符 token、地址拼接 |

## 前置条件
- 用 `t.TempDir()` 作 base/data/deploy；环境变量用 `t.Setenv`（自动还原）。

## 运行方式
- 单项：`go test ./test/config/ -run TestCLIWinsOverEnv -v`
- 模块级：`go test ./test/config/ -v`

## 覆盖场景清单
| 用例 | 场景 | 预期 |
|---|---|---|
| `TestDefaultPortsAreProjectSpecific` | 默认端口 | `28082` / `27200`（与参考项目错开），导出常量一致 |
| `TestDefaultPathsUnderWorkDir` | 默认路径 | data/logs/链目录/机器人程序名、保留 30 天、熔断 500MB、默认链与密码 |
| `TestLoadAppliesCLIFlags` | 全量 CLI 参数 | 端口/数据目录/链目录联动/部署目录/开关/token 全部生效 |
| `TestEnvOverridesDefaults` | 环境变量 | `CTRL_WEB_PORT`/`CTRL_CTRL_PORT`/`CTRL_RUNS_KEEP_DAYS` 生效 |
| `TestCLIWinsOverEnv` | CLI + 环境变量同时存在 | CLI 优先 |
| `TestLocalConfigTokenTemplateIgnored` | `config.local.json` 占位符 | 视为未配置（鉴权保持关闭） |
| `TestAddrHelpers` | 地址拼接 | `127.0.0.1:27200` / `127.0.0.1:28082` |

## 已知限制
- 未覆盖 `--base-dir` 与 `defaultDeployDir` 的自动探测分支（依赖真实目录存在）。
