using System.Security.Cryptography;
using System.Text;

namespace AgentStatus.TestWindow.Core;

// Optional consumer policy. This file is deliberately absent from the Runtime.
public static class DefaultInterpreter
{
    public const string Version = "default-v1";
    public static string? EndOutcome(Fact f) => f.OperationId is null or "" ? null
        : f.Kind == "state.reported" && f.Data.State is "completed" or "interrupted" or "failed" ? f.Data.State
        : f.Kind == "lifecycle.observed" && f.Data.Phase == "ended" && f.Data.Scope is "agent_turn" or "agent_run"
            && f.Data.Outcome is "completed" or "interrupted" or "failed" ? f.Data.Outcome : null;
    public static string EndKey(Fact f) => string.Join("\u001f", f.SessionKey, f.OperationId, f.Data.Scope ?? "agent_turn");
    public static DateTimeOffset EvidenceTime(Fact f) => f.Origin == "external" ? f.ReceivedAt
        : f.OccurredAt is { } time ? (time > f.ReceivedAt ? f.ReceivedAt : time)
        : f.Historical ? default : f.ReceivedAt;

    public static Session[] Project(IEnumerable<EvidenceSession> ledgers, DateTimeOffset now)
    {
        return ledgers.Where(s => s.Facts.LastOrDefault(f => f.Kind == "source.health")?.Data.Health != "superseded")
            .GroupBy(s => string.Join("\u001f", s.Agent, s.SessionNamespace, s.SessionId))
            .Select(group => ProjectSession(group.Key, group.ToArray(), now)).OrderBy(s => s.Agent).ThenBy(s => s.SessionId).ToArray();
    }

    private static Session ProjectSession(string key, EvidenceSession[] ledgers, DateTimeOffset now)
    {
        var facts = ledgers.SelectMany(s => s.Facts).OrderBy(f => f.Id).ToArray();
        var identity = ledgers[0];
        string? operation = null;
        long branchRevision = 0;
        var closed = new HashSet<string>();
        foreach (var f in facts)
        {
            if (f.Data.BranchChanged == true) { operation = null; branchRevision = f.Id; }
            if (f.OperationId is not { Length: > 0 } op) continue;
            bool starts = f.Kind == "input.observed" || f.Kind == "lifecycle.observed" && f.Data.Phase == "started" && f.Data.Scope is "agent_turn" or "agent_run"
                || f.Kind == "state.reported" && f.Data.State is "working" or "tool_activity" or "waiting_confirmation";
            if (starts && !closed.Contains(op)) operation = op;
            if (operation is not null && closed.Contains(operation) && !closed.Contains(op)
                && f.Kind is not ("source.heartbeat" or "source.health" or "session.observed")) operation = op;
            if (EndOutcome(f) is not null || f.Kind == "lifecycle.observed" && f.Data.Phase == "ended" && f.Data.Scope is "agent_turn" or "agent_run")
            { operation ??= op; closed.Add(op); }
        }
        if (branchRevision == 0) operation ??= facts.LastOrDefault(f => f.Kind is not ("source.health" or "source.heartbeat" or "session.observed") && f.OperationId is not null)?.OperationId;
        var tools = new Dictionary<string, bool>();
        var approvals = new Dictionary<string, bool>();
        string state = "unknown", confidence = "unknown", reason = "no_activity_evidence", surface = "unknown", origin = "file", basis = "";
        string? outcome = null, parent = null, agentVersion = null;
        Fact? evidence = null;
        bool ended = false, coverage = ledgers.All(s => s.Coverage == "complete" || s.IncompleteOperations is not null
            && !s.IncompleteOperations.Contains("*") && !s.IncompleteOperations.Contains(operation ?? "*"));
        var sourceHealth = facts.Where(f => f.Kind == "source.health").GroupBy(f => f.SourceId).ToDictionary(g => g.Key, g => g.Last().Data.Health ?? "healthy");
        foreach (var f in facts)
        {
            if (f.Kind == "session.observed")
            {
                surface = f.Data.Surface ?? surface; parent = f.Data.ParentSessionId ?? parent; agentVersion = f.Data.AgentVersion ?? agentVersion;
                continue;
            }
            if (f.Id < branchRevision || f.Kind is "source.heartbeat" or "source.health" || f.OperationId != operation) continue;
            if (ended) continue;
            origin = f.Origin; basis = f.ReportedBasis ?? ""; evidence = f;
            confidence = origin == "external" ? "external" : "inferred";
            reason = f.NativeType ?? f.Kind;
            switch (f.Kind)
            {
                case "input.observed": state = "working"; break;
                case "message.observed":
                    if (f.Data.Role != "assistant") break;
                    bool normalStop = f.Agent == "claude" && f.Data.StopReason is "end_turn" or "stop_sequence"
                        || f.Agent == "pi" && f.Data.StopReason == "stop";
                    if (normalStop && f.Data.HasTools != true && tools.Count == 0 && approvals.Count == 0 && coverage && f.Data.Outcome is not ("failed" or "interrupted"))
                    { state = "idle"; reason = "normal_message_end_inferred"; }
                    else if (f.Data.Outcome is "failed" or "interrupted" || f.Data.StopReason is "max_tokens" or "length" or "error" or "aborted")
                    { state = "unknown"; reason = "abnormal_message_end"; }
                    else state = "working";
                    break;
                case "tool.observed":
                    if (f.Data.CallId is { Length: > 0 } call)
                    { if (f.Data.Phase == "finished") tools.Remove(call); else tools[call] = true; }
                    state = "working"; break;
                case "approval.observed":
                    if (f.Data.RequestId is { Length: > 0 } request)
                    { if (f.Data.Phase == "resolved") approvals.Remove(request); else approvals[request] = true; }
                    state = "working"; break;
                case "lifecycle.observed":
                    if (f.Data.Phase == "started" && f.Data.Scope is "agent_turn" or "agent_run")
                    { state = "working"; confidence = origin == "file" ? "observed" : "external"; }
                    if (f.Data.Phase == "ended" && f.Data.Scope is "agent_turn" or "agent_run" && EndOutcome(f) is null)
                    { state = "unknown"; reason = "ended_without_outcome"; tools.Clear(); approvals.Clear(); ended = true; }
                    break;
                case "state.reported":
                    state = f.Data.State ?? "unknown";
                    if (state is "idle" or "offline" or "unknown") { tools.Clear(); approvals.Clear(); }
                    break;
            }
            if (EndOutcome(f) is { } end)
            {
                outcome = end; state = "idle"; confidence = origin == "file" ? "observed" : "external";
                reason = "explicit_end"; tools.Clear(); approvals.Clear(); ended = true;
            }
            if (!ended && state != "unknown")
            {
                if (approvals.Count > 0) { state = "waiting_confirmation"; confidence = origin == "external" ? "external" : "inferred"; }
                else if (tools.Count > 0) { state = "tool_activity"; confidence = origin == "external" ? "external" : "inferred"; }
            }
        }
        var time = evidence is null ? default : EvidenceTime(evidence);
        if (branchRevision > 0 && operation is null) { state = "unknown"; confidence = "inferred"; reason = "persisted_branch_changed"; }
        bool stale = time == default || now - time > TimeSpan.FromMinutes(2);
        if (stale && state is "working" or "tool_activity" or "waiting_confirmation") { state = "unknown"; confidence = "unknown"; reason = "stale_evidence"; }
        string health = evidence is not null && sourceHealth.TryGetValue(evidence.SourceId, out var value) ? value : "healthy";
        if (health is "degraded" or "unavailable") { state = "unknown"; confidence = "unknown"; reason = "source_unavailable"; }
        return new Session
        {
            SessionId = Convert.ToHexStringLower(SHA256.HashData(Encoding.UTF8.GetBytes(key)))[..32], NativeSessionId = identity.SessionId,
            SessionNamespace = identity.SessionNamespace, Agent = identity.Agent, Surface = surface, AgentVersion = agentVersion,
            ParentSessionId = parent, TurnId = operation, State = state, Confidence = confidence, Reason = reason, Outcome = outcome,
            UpdatedAt = time, Freshness = stale ? "stale" : "fresh", SourceHealth = health, PendingTools = tools, PendingApprovals = approvals,
            Origin = origin, ReportedBasis = basis, Coverage = ledgers.All(s=>s.Coverage=="complete") ? "complete" : "partial"
        };
    }
}
