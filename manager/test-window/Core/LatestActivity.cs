namespace AgentStatus.TestWindow.Core;

// One live display round. Snapshots and diagnostic projections never enter it.
internal sealed class LatestActivity
{
    public static readonly TimeSpan ActivityTimeout = TimeSpan.FromMinutes(5);
    public static readonly TimeSpan ApprovalTimeout = TimeSpan.FromMinutes(30);
    public static readonly TimeSpan SettleDelay = TimeSpan.FromSeconds(3);
    public static readonly TimeSpan NoticeDuration = TimeSpan.FromSeconds(10);
    private readonly HashSet<string> tools = new(), approvals = new();
    private Session? current;
    private string source = "";
    private DateTimeOffset? candidate;
    private DateTimeOffset noticeUntil;
    private bool waiting;

    public void Reset()
    { current = null; tools.Clear(); approvals.Clear(); candidate = null; waiting = false; }

    public Session? Visible(DateTimeOffset now) => current is null || current.EndedAt is not null && now >= noticeUntil ? null : current;

    private bool Matches(Fact f) => current is not null && current.Agent == f.Agent && current.SessionNamespace == f.SessionNamespace
        && current.NativeSessionId == f.SessionId && current.TurnId == f.OperationId;

    public ClientEvent? Apply(Fact f, DateTimeOffset now)
    {
        if (f.Historical) return null;
        if (f.Kind == "source.health")
        {
            if (current is not null && current.EndedAt is null && f.SourceId == source && f.Data.Health is "degraded" or "unavailable")
                current = current with { State = "unknown", Confidence = "unknown", SourceHealth = f.Data.Health, Reason = "source_unavailable" };
            return null;
        }
        if (f.Kind is not ("input.observed" or "message.observed" or "tool.observed" or "approval.observed" or "lifecycle.observed" or "state.reported")) return null;
        if (f.Kind == "lifecycle.observed" && f.Data.Scope is not ("agent_turn" or "agent_run")) return null;
        if (f.Kind == "state.reported" && f.Data.State is "idle" or "offline") { Reset(); return null; }
        string? end = DefaultInterpreter.EndOutcome(f);
        bool same = Matches(f);
        if (same && current!.EndedAt is not null && end is not null && (f.Data.Scope ?? "agent_turn") == current.OperationScope) return null;
        if (!same || current!.EndedAt is not null)
        {
            Reset();
            current = new Session
            {
                SessionId = f.SessionKey, NativeSessionId = f.SessionId, Agent = f.Agent, SessionNamespace = f.SessionNamespace,
                TurnId = f.OperationId, StartedAt = end is null ? now : null, RuleVersion = "live-v1"
            };
        }
        source = f.SourceId;
        candidate = null;
        current = current! with
        {
            Origin = f.Origin, ReportedBasis = f.ReportedBasis ?? "", Confidence = f.Origin == "external" ? "external" : "inferred",
            UpdatedAt = now, LastActivityAt = now, Freshness = "fresh", SourceHealth = "healthy",
            OperationScope = f.Data.Scope ?? current.OperationScope, Reason = f.NativeType ?? f.Kind
        };
        if (end is not null) return Finish(end, now, now, f);
        string state = "working";
        switch (f.Kind)
        {
            case "message.observed":
                if (f.Data.Outcome is "failed" or "interrupted" || f.Data.StopReason is "max_tokens" or "length" or "error" or "aborted") state = "unknown";
                else if (!f.SequenceGap && f.Data.Role == "assistant" && f.Data.HasTools != true && tools.Count == 0 && approvals.Count == 0
                    && (f.Agent == "claude" && f.Data.StopReason is "end_turn" or "stop_sequence" || f.Agent == "pi" && f.Data.StopReason == "stop")) candidate = now;
                break;
            case "tool.observed":
                if (f.Data.CallId is { } call) { if (f.Data.Phase == "finished") tools.Remove(call); else tools.Add(call); }
                break;
            case "approval.observed":
                if (f.Data.RequestId is { } request) { if (f.Data.Phase == "resolved") approvals.Remove(request); else approvals.Add(request); }
                break;
            case "lifecycle.observed":
                if (f.Data.Phase == "ended") state = "unknown";
                break;
            case "state.reported": state = f.Data.State ?? "unknown"; break;
        }
        waiting = approvals.Count > 0 || state == "waiting_confirmation";
        if (state != "unknown") state = waiting ? "waiting_confirmation" : tools.Count > 0 ? "tool_activity" : state;
        current = current with { State = state, PendingTools = tools.ToDictionary(id => id, _ => true), PendingApprovals = approvals.ToDictionary(id => id, _ => true) };
        return null;
    }

    public ClientEvent? Tick(DateTimeOffset now)
    {
        if (current is null || current.EndedAt is not null) return null;
        if (candidate is { } stop && now >= stop + SettleDelay) return Finish("inferred", stop, stop + SettleDelay);
        var deadline = current.LastActivityAt + (waiting ? ApprovalTimeout : ActivityTimeout);
        if (now >= deadline) return Finish("timeout", deadline, deadline);
        if (!waiting && now - current.LastActivityAt > TimeSpan.FromMinutes(2))
            current = current with { State = "unknown", Confidence = "unknown", Freshness = "stale", Reason = "stale_evidence" };
        return null;
    }

    private ClientEvent Finish(string outcome, DateTimeOffset endedAt, DateTimeOffset notifiedAt, Fact? fact = null)
    {
        bool inferred = fact is null;
        current = current! with
        {
            State = "idle", EndedAt = endedAt, Outcome = inferred ? null : outcome,
            Confidence = inferred ? "inferred" : current.Origin == "external" ? "external" : "observed",
            Reason = inferred ? outcome == "timeout" ? "inactivity_timeout" : "normal_message_end_inferred" : "explicit_end"
        };
        noticeUntil = notifiedAt + NoticeDuration;
        candidate = null;
        return new(notifiedAt, inferred ? "inferred.end" : "fact", current,
            inferred ? outcome == "timeout" ? "任务结束（超时推断）；没有新的活动证据，未确认成功或中断。"
                : "任务结束（推断）；正常消息结束后 3 秒没有新活动。" : "",
            fact, true, outcome);
    }
}
