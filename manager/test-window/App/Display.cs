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
    { "observed" => "直接观察", "inferred" => "推断", "unknown" => "未知", _ => Value(value) };
    public static string Freshness(string value) => value switch
    { "fresh" => "有效", "stale" => "已过期", _ => Value(value) };
    public static string Outcome(string? value) => value switch
    { "completed" => "明确回合完成", "interrupted" => "回合中断", "failed" => "回合失败", "unknown" => "结果未知", _ => "未提供" };
    public static string Health(string value) => value switch
    { "healthy" => "正常", "degraded" => "降级", "unavailable" => "不可用", "superseded" => "已有新代际", _ => Value(value) };
    public static string Capability(string value) => value switch
    { "partial" => "部分支持", "unsupported" => "不支持", "unverified" => "尚未验证", "explicit_records_only" => "仅明确结束记录", "inferred_requests_only" => "仅审批请求推断", _ => Value(value) };
    public static string Event(string value) => value switch
    { "snapshot" => "初始快照", "reset" => "流重置 / 事件间隙", "state.changed" => "状态变化", "turn.ended" => "任务结束通知", "source.degraded" => "来源降级", _ => Value(value) };
    public static string EventState(ClientEvent e) => e.Type != "turn.ended"
        ? e.Session is { } session ? State(session.State) : "—"
        : e.Session?.Outcome switch
        { "completed" => "任务完成", "interrupted" => "任务中断", "failed" => "任务失败", _ => "结束（结果未知）" };
    public static string EventDescription(ClientEvent e)
    {
        if (!string.IsNullOrWhiteSpace(e.Summary)) return e.Summary;
        if (e.Type == "turn.ended") return "Agent 明确回合结束记录；这是结束通知。";
        if (e.Type == "state.changed" && e.Session?.State == "idle") return "当前为空闲；状态同步，不是新的完成通知。";
        return Reason(e.Session?.Reason);
    }
    public static string Reason(string? value) => value switch
    {
        "message_end_without_settled_event" => "消息已结束，尚无整轮稳定结束证据",
        "message_end_without_turn_boundary" => "消息已结束，尚无明确回合结束边界",
        "stale_evidence" => "活动证据已过期，无法确认当前状态",
        _ => Value(value)
    };
    public static Color StateColor(string value) => value switch
    { "working" => Color.FromArgb(29, 78, 216), "tool_activity" => Color.FromArgb(109, 40, 217), "waiting_confirmation" => Color.FromArgb(146, 64, 14), "idle" => Color.FromArgb(21, 100, 65), _ => Color.FromArgb(92, 104, 122) };

    public static string Activity(Session[] sessions, DateTimeOffset now)
    {
        var available = sessions.Where(s => s.SourceHealth is not ("superseded" or "unavailable"));
        var active = available.Where(s => s.Freshness == "fresh" &&
            s.State is "working" or "tool_activity" or "waiting_confirmation")
            .OrderByDescending(s => s.UpdatedAt).ThenBy(s => s.SessionId).FirstOrDefault();
        var latest = active ?? available.OrderByDescending(s => s.UpdatedAt).ThenBy(s => s.SessionId).FirstOrDefault();
        string label = active is not null ? $"{Agent(active.Agent)}：{State(active.State)}"
            : $"无活动：{(latest?.State == "idle" ? "空闲" : "未知")}";
        if (latest is null || latest.UpdatedAt == default) return $"{label}...未提供";
        var elapsed = TimeSpan.FromSeconds(Math.Max(0, Math.Floor((now - latest.UpdatedAt).TotalSeconds)));
        string duration = elapsed.TotalHours >= 1 ? $"{(long)elapsed.TotalHours}h{elapsed.Minutes}m{elapsed.Seconds}s"
            : elapsed.TotalMinutes >= 1 ? $"{(int)elapsed.TotalMinutes}m{elapsed.Seconds}s" : $"{elapsed.Seconds}s";
        return $"{label}...{duration}";
    }

    public static string Detail(Session s) => $"""
        Agent：{Agent(s.Agent)}
        入口：{Surface(s.Surface)}
        Agent 版本：{Value(s.AgentVersion)}

        统一会话 ID：{Value(s.SessionId)}
        原生会话 ID：{Value(s.NativeSessionId)}
        父会话 ID：{Value(s.ParentSessionId)}
        当前回合 ID：{Value(s.TurnId)}

        当前状态：{State(s.State)}
        可信度：{Confidence(s.Confidence)}
        证据新鲜度：{Freshness(s.Freshness)}
        结束结果：{Outcome(s.Outcome)}
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
