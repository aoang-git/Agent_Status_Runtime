# Agent_Status_Runtime

跨 Agent 的本机事实采集与广播服务。**v0.2 将采集事实与状态推断分开**：Runtime 增量读取四类工具的持久化记录，或接收用户编写的 Hook／插件上报，保存白名单元信息，通过 HTTP/SSE 广播。Runtime 不安装、加载或执行插件，也不修改 Agent 配置。

| 来源 | 文件格式 | 提供的事实与限制 |
|---|---|---|
| Codex CLI／桌面本地会话 | rollout JSONL | 会话、消息、工具、明确回合开始／结束；缺少回合 ID 不归属完成 |
| Claude Code CLI／桌面本地 Code | transcript JSONL | 输入、消息原生停止原因、工具；消息结束不代表整轮成功 |
| DeepSeek Harness | v3/v4 JSONL、Zstd | 回合边界、工具、审批；恢复补写及继承前缀不当作新完成 |
| Pi coding agent | v3 JSONL | 输入、消息停止原因、工具、持久化分支变化；不等同于内存中的整轮稳定结束 |
| 用户自助接入 | 本机 HTTP／通用 CLI | 任意 Agent 名称；外部声明始终保留外部来源，不升级为 Runtime 证实 |

CLI／桌面共用文件解析器，桌面兼容性仍需独立验证。云端、WSL／远程文件和无持久化会话不会自动覆盖。完整协议、字段、恢复边界及回滚步骤见 [v0.2 接入协议](doc/v0.2接入协议.md)。

## Windows 运行

在项目目录独立启动 Runtime，另开终端查询：

```powershell
.\bin\agent-statusd.exe serve --data-dir .\.runtime\live
.\bin\agent-statusd.exe status --data-dir .\.runtime\live
.\bin\agent-statusd.exe doctor --data-dir .\.runtime\live
.\bin\agent-statusd.exe watch --data-dir .\.runtime\live
.\bin\agent-status-test.exe --data-dir .\.runtime\live
```

`status` 输出事实快照，`watch` 输出事实 SSE；二者不推断工作／空闲。前台服务按 Ctrl+C 停止。默认仅监听 `127.0.0.1:9120`；省略 `--data-dir` 时使用当前用户配置目录中的 `AgentStatusRuntime`。不注册自启动。

默认扫描 `~/.codex/sessions`、`~/.claude/projects`、`~/.dsh/sessions`、`~/.pi/agent/sessions`，识别 `CODEX_HOME`、`CLAUDE_CONFIG_DIR`、`DSH_HOME`、`PI_CODING_AGENT_SESSION_DIR`。非标准位置可传多个 `--root codex=D:\path\sessions`；传入后只扫描指定根。不存在的根目录会在后续补偿扫描中重查。

## 让自己的 Agent 实现接入

```powershell
# 离线也能生成；输出路径和协议，不包含令牌。
.\bin\agent-statusd.exe integration-prompt --data-dir .\.runtime\live

# 一条 UTF-8 JSON 从 stdin 传入；校验时不连接服务。
Get-Content -Raw .\event.json | .\bin\agent-statusd.exe report --validate-only
Get-Content -Raw .\event.json | .\bin\agent-statusd.exe report --data-dir .\.runtime\live --best-effort
```

将生成的提示词交给实际使用的 Agent，让它检查自身版本及官方扩展机制，编写确定性的原生回调转发程序，并交付配置差异、验证和卸载说明。没有可靠生命周期回调时，只能上报 `inferred`／`self_report`，不能伪造原生完成。本仓库不代为安装任何专用插件。

通用示例默认保持 stdout 为空，上报失败只输出脱敏错误代码且正常退出；运行时不应启动服务或阻断原工具：

```powershell
# PowerShell 7：-Mode Http 改为直接调用本机写接口。
pwsh -File .\examples\report-state.ps1 -DataDir .\.runtime\live -Exe .\bin\agent-statusd.exe -Agent my-agent -Session session-1 -Operation request-1 -State completed -Mode Cli

# Python 标准库：--mode http 改为直接 HTTP。
python .\examples\report_state.py --data-dir .\.runtime\live --exe .\bin\agent-statusd.exe --agent my-agent --session session-1 --operation request-1 --state completed --mode cli
```

两个示例只生成 `state.reported / self_report`，不证明某个具体 Agent 生命周期兼容。接入者必须提供真实会话、稳定的本轮执行 ID；独立重试需复用同一 `--event-id`／`-EventId`。先连接专用测试 Runtime 验证，禁止向真实 Agent 日志注入测试记录。

## Windows 测试窗口

`bin/agent-status-test.exe` 是 Windows x64 自包含程序，运行无需另装 .NET。可双击启动；未指定目录时优先使用程序旁项目的 `.runtime/live`（有连接文件时），否则使用用户配置目录。窗口允许选择数据目录和重新连接，**不会启动、停止或配置 Runtime**。

窗口读取 v2 事实并在客户端使用 `default-v1` 规则解释：

- 会话页和顶部摘要显示当前活动、依据、来源、证据新鲜度及待配对工具／审批。
- 事实页保留最近 500 条接收记录，可清空和暂停滚动；明确结束、普通空闲、推断空闲、外部完成分别显示。
- 文件中的 Codex／DSH 明确正常结束可产生完成通知；Claude／Pi 正常消息结束且没有未配对工具时只能推断空闲。
- 外部完成标为“任务完成（外部报告）”。`native_event` 只是外部发送者的依据声明。
- 历史事实、首次快照和流重置不恢复顶部状态、不新增结束提醒。证据不足或来源不可用时先保留未知；普通活动连续 5 分钟、审批等待连续 30 分钟无新活动时，生成一次“任务结束（超时推断）”，不宣称成功或中断。

顶部仅跟随本次连接后最新的有效事实，显示一个来源、状态和本轮累计时长，不受列表筛选影响。开始时间取本轮首次实时活动的客户端接收时间；同一轮的消息、工具和状态切换不重新计时。另一会话／执行的新活动立即接管显示，不保留旧任务供恢复。明确结束立即通知；Claude／Pi 正常消息结束且没有待处理工具或审批时，观察 3 秒再推断结束，期间新活动会取消候选结束。结束后固定时长，通知显示 10 秒后清除，仅保留“已连接｜无活动”；更新的活动可立即替换通知。推断结束不撤回，后续活动按新一轮重新计时。普通活动两分钟后标记证据过期，审批等待使用 30 分钟时限；心跳、来源健康与快照接收不续期。这些是调试客户端规则，不改写核心事实，正式上位机可采用自己的规则。

断线保留诊断快照和接收记录，清除顶部活动及计时，按 1、2、5、10 秒重试，后续最多 10 秒；每次重读连接文件。45 秒没有事件／心跳时断开。诊断和事实快照每 5 秒核对；每次 SSE 连接重新建立基线，不重放断线期间的活动供顶部显示。核心仍提供 `epoch:revision` 恢复能力供其他消费者使用。关闭窗口不影响后台采集。

## 接口、安全与恢复边界

只读发现文件为 `<data-dir>/private/connection.json`，写入发现文件为同目录的 `ingest.json`；均含 `url`／`token`，每次服务启动轮换凭据。令牌不放入 URL，不展示、不记录。

| 接口 | 凭据 |
|---|---|
| `GET /v2/info`、`/v2/schema`、`/v2/health`、`/v2/capabilities` | 只读 |
| `GET /v2/snapshot`、`/v2/events` | 只读 |
| `POST /v2/ingest` | 写入；只读令牌不可写，写入令牌不可读 |

只接受 HTTP loopback IP；官方客户端／示例禁用代理和自动重定向。HTTP 拒绝带非空 `Origin` 的浏览器请求。v0.2 不保留 `/v1` 状态服务，旧接口返回升级错误，Runtime 与窗口须一起更新。

写入每次一条事件，最多 16 KiB；拒绝未知类型、未知字段、嵌套原始对象及非法元信息。`201` 表示新事件持久化成功，`200` 表示重复并返回原事实 ID，`409` 表示同 ID 不同内容。外部幂等范围为最近 10 万条或七天，以先淘汰者为准；持有写凭据的本机程序可声明任意 producer，首版没有逐插件身份认证。

事实 SSE 保留最近 4096 条；快照保留每个来源会话最近 32 个 operation 的摘要，最多 2048 条配对／边界记录，整个快照最多 8 MiB。超限或序号缺口标为覆盖不完整，`incomplete_operations` 区分旧历史裁剪与当前证据缺失。不能将快照当完整审计日志。原生时间不重排接收顺序，没有来源序号就不能保证发现所有丢失。

JSONL 半行、Zstd 半帧不推进检查点；单行上限 8 MiB，压缩读取 16 MiB，解压帧 32 MiB。未知版本、损坏或超限只降级相应来源，存储失败不推进检查点。数据库只保存白名单事实与检查点，不保存提示词、正文、工具参数或输出。来源漏记、持久化延迟、内存分支及进程崩溃仍会影响观察完整性；不提供离线外部事件队列。

## 构建、测试和升级

Runtime 需要 Go 1.26+，纯 Go SQLite 无需 C 编译器。构建脚本优先使用 `.tools/go` 便携工具链：

```powershell
.\scripts\build.ps1 -Test
# 或在 runtime 目录：go test ./...；go vet ./...
.\scripts\build-test-window.ps1 -Test
# 单独运行窗口测试：
dotnet test .\manager\test-window\Tests\Tests.csproj
```

窗口独立使用 .NET 10 SDK 发布，不构建或替换 Runtime。交付物统一放在 `bin`，每个程序及目标平台只保留最新版，不保留旧版本或临时构建副本。构建遇到文件占用时，可核验可执行文件路径与启动参数后关闭本项目进程，等待释放后在原位置重试；构建后校验产物并恢复此前运行的实例。集成测试使用独立临时 Runtime 和构造日志，不能操作真实 Agent 日志。

v0.2 使用独立的 `private/facts-v2.sqlite`；保留 v0.1 `state.sqlite`，不把旧推断状态导入事实库。首次启动建立历史基线，不重复提醒旧完成。回滚需停止 v0.2，同时恢复旧 Runtime 和旧窗口，使用保留的旧数据库；不要在运行中复制单个 SQLite 文件或让两个版本同时占用同一数据目录。详见 [迁移与回滚](doc/v0.2接入协议.md#迁移与回滚)。

项目文档：

- [v0.2 接入协议与示例](doc/v0.2接入协议.md)
- [v0.2 验证与交付记录](doc/v0.2验证记录.md)
- [公共 JSON Schema](runtime/internal/monitor/protocol/schema.json) · [协议样本](runtime/internal/monitor/protocol/samples.json) · [通用接入提示词模板](runtime/internal/monitor/protocol/integration-prompt.md)
- [第一阶段实现方案](doc/第一阶段实现方案.md) · [软件架构设计](doc/agent状态统一监测项目架构.md) · [可行性研究](doc/agent状态统一监测计划.md)
- [v0.1 验证记录（历史）](doc/v0.1验证记录.md) · [测试窗口验证记录](doc/测试窗口验证记录.md)
