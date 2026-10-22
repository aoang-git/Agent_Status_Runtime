# Agent_Status_Runtime
跨 Agent 的本机状态监测服务。当前实现为 **v0.1 被动文件监测**：自动发现本地会话，增量解析持久化记录，通过 CLI、HTTP 和 SSE 提供状态。整个过程不修改 Agent 配置。

当前支持：

| Agent | 文件来源 | 能力边界 |
|---|---|---|
| Codex CLI / 桌面本地会话 | rollout JSONL | 活动、工具证据、明确回合结束/中断；缺少回合 ID 的结束不通知 |
| Claude Code CLI / 桌面本地 Code | transcript JSONL | 会话、活动与工具证据；消息结束仅为候选，不发成功通知 |
| DeepSeek Harness | v3/v4 JSONL、Zstd 帧 | 活动、工具、审批请求推断、明确结束；恢复补写与继承前缀不通知 |
| Pi coding agent | v3 JSONL | 会话、消息/工具活动、已持久化分支变化；不承诺 session-level 完成 |

CLI 与桌面共用文件解析器，桌面版本兼容性仍需独立验证。云端、WSL/远程文件、无持久化会话不会因本机客户端存在就自动被覆盖。人工等待普遍无法靠文件确认；`unknown` 表示证据不足，静默不会变成完成。

### Windows 启动

已构建程序位于 `bin/agent-statusd.exe`。在项目目录运行：

```powershell
.\bin\agent-statusd.exe serve --data-dir .\.runtime\live
```

另一个终端查询或订阅：

```powershell
.\bin\agent-statusd.exe status --data-dir .\.runtime\live
.\bin\agent-statusd.exe doctor --data-dir .\.runtime\live
.\bin\agent-statusd.exe watch --data-dir .\.runtime\live
```

前台服务按 Ctrl+C 停止。默认仅监听 `127.0.0.1:9120`，使用当前用户受限目录中的随机令牌。省略 `--data-dir` 时，使用系统用户配置目录下的 `AgentStatusRuntime`。不注册自启动，也不安装 Hooks。

默认寻找 `~/.codex/sessions`、`~/.claude/projects`、`~/.dsh/sessions`、`~/.pi/agent/sessions`，识别 `CODEX_HOME`、`CLAUDE_CONFIG_DIR`、`DSH_HOME`、`PI_CODING_AGENT_SESSION_DIR`。非标准位置可临时传入多个 `--root codex=D:\path\sessions`；传入后只扫描指定根，不修改任何 Agent 文件。未创建的默认目录会由后续补偿扫描发现。

### 构建与测试

需要 Go 1.26+。构建前停止正在运行的同名程序。Windows 构建脚本优先使用项目内 `.tools/go` 便携工具链，否则使用 PATH 中的 Go：

```powershell
.\scripts\build.ps1 -Test
```

标准 Go 命令：

```text
cd runtime
go test ./...
go vet ./...
go build -trimpath -o ../bin/agent-statusd ./cmd/agent-statusd
```

依赖由 `runtime/go.mod` 与 `go.sum` 锁定。Go 纯代码 SQLite，无需 C 编译器。二进制、构建缓存和本机状态均已加入忽略列表。

### 接口与已知限制

`GET /v1/info`、`/v1/health`、`/v1/status`、`/v1/sessions`、`/v1/capabilities`、`/v1/events` 均需 `Authorization: Bearer <token>`。连接地址与令牌保存在 `<data-dir>/private/connection.json`；CLI 自动读取，不将令牌放进 URL。HTTP 不接收跨源浏览器请求。

状态快照附 `epoch`、`revision`；SSE 的 `Last-Event-ID` 或 `after` 游标为 `epoch:revision`。首次订阅发送 snapshot，重放窗口外发送 reset；最多保留 512 条事件。终止通知为 `turn.ended`，携带 `completed / interrupted / failed / unknown`。只有 `completed` 才表示当前回合明确正常结束。

首次发现、重启恢复和文件重建先静默建立历史快照，因此不会把已有完成记录当成新通知；新发现文件在首次扫描边界内结束的短会话也不会弹完成提醒。运行中的追加记录才产生新的终止通知。活跃证据默认两分钟后过期为 unknown，可通过 `--stale-after` 调整。

文件通知合并触发增量读取，默认每 5 秒补偿发现。JSONL 半行、Zstd 半帧不推进检查点；日志格式损坏、未知版本、压缩编码歧义或超限会在 doctor 中降级。单行上限 8 MiB、压缩帧读取上限 16 MiB、解压上限 32 MiB；遇到超限停在安全游标，不跳过后假装完整。

Runtime 仅读取 Agent 文件，数据库保存白名单状态元信息及检查点，不保存提示词、正文或工具参数。实际持久化延迟、内存分支选择、Agent 崩溃和上游漏记录仍可能使实时状态未知。此版本不提供完整五态认证；Hooks、原生服务自启动和更老格式后续适配。

项目文档：

- [第一阶段实现方案](doc/第一阶段实现方案.md)：Codex、Claude Code、DeepSeek Harness、Pi 的接入分析、实施顺序与验收标准。
- [软件架构设计](doc/agent状态统一监测项目架构.md)
- [可行性研究](doc/agent状态统一监测计划.md)
- [v0.1 验证记录](doc/v0.1验证记录.md)
