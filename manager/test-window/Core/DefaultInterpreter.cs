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
        var live = new LatestActivity();
        foreach (var fact in facts)
        {
            live.Tick(fact.ReceivedAt);
            live.Apply(fact with { SequenceGap = fact.SequenceGap || ledgers.Any(s => s.Coverage != "complete" && (s.IncompleteOperations is null || s.IncompleteOperations.Contains("*") || s.IncompleteOperations.Contains(fact.OperationId ?? "*"))) }, fact.ReceivedAt);
        }
        live.Tick(now);
        var identity = ledgers[0];
        var session = live.Current ?? new Session { State = "unknown", Reason = "no_activity_evidence" };
        var metadata = facts.Where(f => f.Kind == "session.observed").LastOrDefault()?.Data;
        return session with {
            SessionId = Convert.ToHexStringLower(SHA256.HashData(Encoding.UTF8.GetBytes(key)))[..32],
            NativeSessionId = identity.SessionId, SessionNamespace = identity.SessionNamespace, Agent = identity.Agent,
            Surface = metadata?.Surface ?? "unknown", ParentSessionId = metadata?.ParentSessionId,
            AgentVersion = metadata?.AgentVersion, Coverage = ledgers.All(s => s.Coverage == "complete") ? "complete" : "partial"
        };
    }
}
