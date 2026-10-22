using AgentStatus.TestWindow.Core;

namespace AgentStatus.TestWindow;

internal static class Display
{
    public static string Value(string? value) => string.IsNullOrWhiteSpace(value) ? "未提供" : value;
    public static string Time(DateTimeOffset value) => value == default ? "未提供" : value.ToLocalTime().ToString("yyyy-MM-dd HH:mm:ss");
    public static string Agent(string value) => value switch
    { "codex" => "Codex", "claude" => "Claude Code", "deepseek" => "DeepSeek", "pi" => "Pi", _ => Value(value) };
    public static string Surface(string value) => value switch
    { "cli" => "CLI", "desktop" => "桌面", "subagent" => "子 Agent", "unknown" => "未识别入口", _ => Value(value) };
    public static string State(string value) => value switch
    { "working" => "工作中", "tool_activity" => "工具活动", "waiting_confirmation" => "等待确认（推断）", "idle" => "空闲", "unknown" => "未知", "offline" => "离线", _ => Value(value) };
    public static string Confidence(string value) => value switch
    { "observed" => "直接观察", "inferred" => "推断", "external" => "外部报告", "unknown" => "未知", _ => Value(value) };
    public static string Freshness(string value) => value switch
    { "fresh" => "有效", "stale" => "已过期", _ => Value(value) };
    public static string Outcome(string? value) => value switch
    { "completed" => "明确回合完成", "interrupted" => "回合中断", "failed" => "回合失败", "unknown" => "结果未知", _ => "未提供" };
    public static string Health(string value) => value switch
    { "healthy" => "正常", "degraded" => "降级", "unavailable" => "不可用", "superseded" => "已有新代际", "reported" => "已接收外部报告", _ => Value(value) };
    public static string Capability(string value) => value switch
    { "partial" => "部分支持", "unsupported" => "不支持", "unverified" => "尚未验证", "explicit_records_only" => "仅明确结束记录", "inferred_requests_only" => "仅审批请求推断", _ => Value(value) };
    public static string Event(string value) => value switch
    { "snapshot" => "初始事实快照", "reset" => "事实间隙 / 重置", "session.observed" => "会话记录", "input.observed" => "输入记录", "message.observed" => "消息记录", "tool.observed" => "工具记录", "approval.observed" => "审批记录", "lifecycle.observed" => "生命周期记录", "state.reported" => "外部状态报告", "source.heartbeat" => "来源心跳", "source.health" => "来源健康", _ => Value(value) };
    public static string EventState(ClientEvent e)
    {
        if (e.IsNotification)
        {
            string result = e.NotificationOutcome switch { "completed" => "任务完成", "interrupted" => "任务中断", "failed" => "任务失败",
                "timeout" => "任务结束（超时推断）", "inferred" => "任务结束（推断）", _ => "结束（结果未知）" };
            return result + ((e.Fact?.Origin ?? e.Session?.Origin) == "external" ? "（外部报告）" : "");
        }
        if (e.Fact is not { } f) return "—";
        return f.Kind switch
        {
            "lifecycle.observed" => $"{(f.Data.Phase == "started" ? "开始" : "结束")} / {Value(f.Data.Scope)}",
            "message.observed" => $"{Value(f.Data.Role)} / {Value(f.Data.StopReason)}",
            "tool.observed" => f.Data.Phase switch { "requested" => "工具请求记录", "started" => "工具开始记录", _ => "工具结果记录" },
            "approval.observed" => f.Data.Phase == "requested" ? "审批请求记录" : "审批决定记录",
            "state.reported" => Value(f.Data.State),
            "source.health" => Health(f.Data.Health ?? ""),
            _ => "已观察"
        };
    }
    public static string EventDescription(ClientEvent e)
    {
        if (!string.IsNullOrWhiteSpace(e.Summary)) return e.Summary;
        if (e.Fact is not { } f) return "未提供";
        string prefix = f.Historical ? "历史记录，不生成通知；" : e.IsNotification ? "本次任务结束通知；" : "";
        return prefix + $"{EventState(e)}；原生类型：{Value(f.NativeType)}；原因：{Value(f.NativeReason)}；operation：{Value(f.OperationId)}";
    }
    public static string Reason(string? value) => value switch
    {
        "normal_message_end_inferred" => "正常消息已结束，客户端推断空闲；没有明确整轮完成证据",
        "abnormal_message_end" => "消息异常或截断结束，不能推断任务完成",
        "explicit_end" => "明确结束边界（外部来源仍属于来源声明）",
        "stale_evidence" => "活动证据已过期，无法确认当前状态",
        "source_unavailable" => "当前证据来源不可用或降级",
        "inactivity_timeout" => "普通活动 5 分钟、审批等待 30 分钟无新活动，客户端推断任务结束；未确认成功或中断",
        _ => Value(value)
    };
    public static string FactDetail(ClientEvent e) => e.Fact is not { } f ? e.Session is { } session
        ? $"{e.Summary}{Environment.NewLine}Agent：{Agent(session.Agent)}；本轮开始：{(session.StartedAt is { } start ? Time(start) : "未提供")}；结束：{(session.EndedAt is { } end ? Time(end) : "未提供")}；总时长：{Duration(session, e.ReceivedAt)}"
        : e.Summary : $"""
        {Event(f.Kind)}：{EventState(e)}
        来源：{Origin(f.Origin)} / {Value(f.SourceId)}
        producer_id：{Value(f.ProducerId)}    event_id：{Value(f.EventId)}    fact_id：{f.Id}
        Agent：{Agent(f.Agent)}    会话：{Value(f.SessionId)}    命名空间：{Value(f.SessionNamespace)}
        执行 ID：{Value(f.OperationId)}    范围：{Value(f.Data.Scope)}    结果：{Value(f.Data.Outcome)}
        原生时间：{(f.OccurredAt is { } time ? Time(time) : "未提供")}    Runtime 接收：{Time(f.ReceivedAt)}    客户端接收：{Time(e.ReceivedAt)}
        依据：{Basis(f)}    历史：{(f.Historical ? "是" : "否")}    序号缺口：{(f.SequenceGap ? "是" : "未发现（无序号时无法保证）")}
        原生类型：{Value(f.NativeType)}    原生原因：{Value(f.NativeReason)}
        工具调用：{Value(f.Data.CallId)}    审批请求：{Value(f.Data.RequestId)}    审批决定：{Value(f.Data.Decision)}
        {EventDescription(e)}
        """.ReplaceLineEndings(Environment.NewLine);
    public static string SessionState(Session s) => State(s.State) + (s.State == "idle" && s.Confidence == "inferred" ? "（推断）" : "");
    public static string SessionOutcome(Session s) => s.Origin == "external" && s.Outcome is "completed" or "interrupted" or "failed"
        ? (s.Outcome switch { "completed" => "任务完成", "interrupted" => "任务中断", _ => "任务失败" }) + "（外部报告）" : Outcome(s.Outcome);
    public static string Origin(string? origin) => origin switch { "file" => "文件", "external" => "外部报告", _ => Value(origin) };
    public static string Basis(Fact f) => f.Origin == "file" ? "文件直接记录" : $"外部声明 / {Value(f.ReportedBasis)}";
    public static Color StateColor(string value) => value switch
    { "working" => Color.FromArgb(29, 78, 216), "tool_activity" => Color.FromArgb(109, 40, 217), "waiting_confirmation" => Color.FromArgb(146, 64, 14), "idle" => Color.FromArgb(21, 100, 65), _ => Color.FromArgb(92, 104, 122) };

    public static string Activity(Session? latest, DateTimeOffset now)
    {
        if (latest is null || latest.State == "idle" && latest.EndedAt is null) return "无活动";
        string state = latest.EndedAt is null ? latest.State == "waiting_confirmation" ? "等待确认" : State(latest.State)
            : latest.Outcome switch { "completed" => "任务完成", "interrupted" => "任务中断", "failed" => "任务失败",
                _ => latest.Reason == "inactivity_timeout" ? "任务结束（超时推断）" : "任务结束（推断）" };
        string label = $"{Agent(latest.Agent)}：{state}";
        if (latest.Origin == "external") label += "（外部报告）";
        if (latest.EndedAt is null && latest.Freshness == "stale") label += "（证据过期）";
        return $"{label} · {Duration(latest, now)}";
    }

    public static string Duration(Session session, DateTimeOffset now)
    {
        if (session.StartedAt is not { } start) return "时长未知";
        var elapsed = TimeSpan.FromSeconds(Math.Max(0, Math.Floor(((session.EndedAt ?? now) - start).TotalSeconds)));
        string duration = elapsed.TotalHours >= 1 ? $"{(long)elapsed.TotalHours}h{elapsed.Minutes}m{elapsed.Seconds}s"
            : elapsed.TotalMinutes >= 1 ? $"{(int)elapsed.TotalMinutes}m{elapsed.Seconds}s" : $"{elapsed.Seconds}s";
        return duration;
    }

    public static string Detail(Session s) => $"""
        Agent：{Agent(s.Agent)}
        入口：{Surface(s.Surface)}
        Agent 版本：{Value(s.AgentVersion)}

        统一会话 ID：{Value(s.SessionId)}
        原生会话 ID：{Value(s.NativeSessionId)}
        会话命名空间：{Value(s.SessionNamespace)}
        证据来源：{Origin(s.Origin)}
        来源声明依据：{Value(s.ReportedBasis)}
        覆盖范围：{s.Coverage}
        解释规则：{s.RuleVersion}
        父会话 ID：{Value(s.ParentSessionId)}
        当前回合 ID：{Value(s.TurnId)}

        当前状态：{SessionState(s)}
        可信度：{Confidence(s.Confidence)}
        证据新鲜度：{Freshness(s.Freshness)}
        结束结果：{SessionOutcome(s)}
        判断原因：{Reason(s.Reason)}
        更新时间：{Time(s.UpdatedAt)}
        来源健康：{Health(s.SourceHealth)}

        未结束工具调用：
        {Ids(s.PendingTools)}

        待处理审批请求：
        {Ids(s.PendingApprovals)}

        注：仅展示状态元信息。工具活动不保证工具正在执行；审批请求不保证需要人工操作。
        """.ReplaceLineEndings(Environment.NewLine);

    private static string Ids(Dictionary<string, bool>? values) => values is null ? "未提供"
        : values.Count == 0 ? "无已观察到的待处理项" : string.Join(Environment.NewLine, values.Where(x => x.Value).Select(x => x.Key));
}
