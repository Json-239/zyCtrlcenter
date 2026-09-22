# gameproto 测试说明

## 被测对象
- 模块：`internal/gameproto/gameproto.go`
- 关键函数：`Pack` / `Unpack` / `PackFrame` / `PackFrameRaw` / `WriteFrame` / `ReadFrame`；
  `Writer` / `Reader`（流式）；`ParseRoleList` / `BuildRoleList` / `BuildRoleFrame`；`NormalizeCoding`
- 协议：帧 `[type int32 LE][pblen int32 LE][body]`；字段 `1/2/4/8`=有符号整数 LE、字符串=长度前缀 + 编码字节
  （见 [游戏服探测协议](../../docs/03-协议/游戏服探测协议.md)）

## 测试文件
| 文件 | 覆盖场景 |
|---|---|
| `gameproto_test.go` | 夹具 provenance / 打包字节一致 / 解包字段一致 / 流式读写 / 角色表解析 / 编码 / 异常路径 |

## 前置条件
- 夹具：`test/fixtures/gameproto/frames.json`（13 帧）——**hex 由参考实现实际打包生成**，
  不是手写；每条带 `provenance.real/how`，元测试强制校验。
- 全部内存操作，不联网、不写项目目录。

## 运行方式
- 单项：`go test ./test/gameproto/ -run TestPackMatchesReferenceFixtureBytes -v`
- 模块级：`go test ./test/gameproto/ -v`

## 覆盖场景清单
| 用例 | 场景 | 预期 |
|---|---|---|
| `TestFrameFixturesHaveProvenance` | 夹具来源声明 | 必须 `real=true` 且有 `how`（无实际数据支撑不算数） |
| `TestPackMatchesReferenceFixtureBytes` | 用 `PackFrame` 重打包每条夹具 | 与参考实现生成的字节**逐字节一致** |
| `TestUnpackReferenceFixtureBytes` | 解包每条夹具 | msgid/字段值全对，且恰好读满（无残留） |
| `TestWriterMatchesLoginFixtureAndReaderParsesIt` | 流式 `Writer` 组登录帧 + `Reader` 解析 | 字节与夹具一致；账号/md5 解出正确；`Remaining()==0` |
| `TestHandshakeFixturesUseInt32Body` | 255 带 `[200]`、101 带版本号字符串 | 整数与字符串字段宽度正确 |
| `TestParseRoleListFixture` | 90132 一个角色 | `测试甲 / 21 / turn=0 / role_id=1001` |
| `TestParseRoleListEmpty` | 90132 数量 0 | 空列表且**不报错** |
| `TestParseRoleListTruncated` | 角色体被砍一半 | 报错（不返回半条数据） |
| `TestGBKCodingFailsFastInsteadOfCorrupting` | GBK/未知编码 | 明确返回 `ErrUnsupportedCoding`（宁可不支持，也不要乱码） |
| `TestNormalizeCoding` | `""`/`utf8`/`UTF-8`/`gbk`/`GB2312` | 归一化正确 |
| `TestUnpackTruncatedBody` | int32 字段不足、字符串长度越界 | 报错 |
| `TestUnpackUnknownFormat` | fmt 给 `3` | 报错（只支持 1/2/4/8） |
| `TestPackRejectsMismatchedFields` | fmt 与 vals 数量不等 | 报错 |
| `TestReadFrameTruncatedStream` | 头部声明长度 > 实际字节 | EOF 类错误（不死等、不返回残缺帧） |
| `TestReaderErrorIsSticky` | 读失败后继续读 | 错误粘性：返回零值并保持错误 |

| `TestRegisterFixturesMatchOurPacking` | 注册协议夹具（106/702/104/700） | 逐字节一致 + provenance |
| `TestBuildRegisterFrameMatchesFixture` | 104 的 8 字段顺序 | 与夹具字节一致；字段数不为 8 报错 |
| `TestParseRegisterResult` | 700 的 [db_ret,err_item,errid] | OK()/可重试判定/人话说明正确 |

## 已知限制
- 只内置 UTF-8 编码表（GBK 明确报错）；需要 GBK 时在 `encodeStr/decodeStr` 接入编码表。
- 未做超大帧压测（`MaxBodyBytes=4MB` 仅作上限保护）。
