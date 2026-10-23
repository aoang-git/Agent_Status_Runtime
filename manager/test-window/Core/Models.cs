using System.Text.Json;
using System.Text.Json.Serialization;

namespace AgentStatus.TestWindow.Core;

public static class Wire
{
    public static readonly JsonSerializerOptions Json = new()
    {
        PropertyNamingPolicy = JsonNamingPolicy.SnakeCaseLower,
        PropertyNameCaseInsensitive = false
    };

    public static T Read<T>(string json) => JsonSerializer.Deserialize<T>(json, Json)
        ?? throw new InvalidDataException("服务返回了空数据。");
}

public sealed record Session
{
    public string SessionId { get; init; } = "";
    public string NativeSessionId { get; init; } = "";
    public string Agent { get; init; } = "";
    public string Surface { get; init; } = "";
    public string? AgentVersion { get; init; }
    public string? ParentSessionId { get; init; }
    public string? TurnId { get; init; }
    public string State { get; init; } = "unknown";
    public string Confidence { get; init; } = "unknown";
    public string Reason { get; init; } = "";
    public string? Outcome { get; init; }
    public DateTimeOffset UpdatedAt { get; init; }
    public DateTimeOffset? StartedAt { get; init; }
    public DateTimeOffset? EndedAt { get; init; }
    public DateTimeOffset LastActivityAt { get; init; }
    public string OperationScope { get; init; } = "agent_turn";
    public string SourceHealth { get; init; } = "";
    public string Freshness { get; init; } = "";
    public Dictionary<string, bool>? PendingTools { get; init; }
    public Dictionary<string, bool>? PendingApprovals { get; init; }
    public string SessionNamespace { get; init; } = "";
    public string Origin { get; init; } = "file";
    public string ReportedBasis { get; init; } = "";
    public string Coverage { get; init; } = "complete";
    public string RuleVersion { get; init; } = DefaultInterpreter.Version;
}

public sealed record FactData
{
    public string? Surface { get; init; }
    public string? AgentVersion { get; init; }
    public string? ParentSessionId { get; init; }
    public string? Role { get; init; }
    public string? StopReason { get; init; }
    public string? Phase { get; init; }
    public string? Scope { get; init; }
    public string? Outcome { get; init; }
    public string? CallId { get; init; }
    public string? RequestId { get; init; }
    public string? Decision { get; init; }
    public string? State { get; init; }
    public string? MessageId { get; init; }
    public string? ParentNodeId { get; init; }
    public bool? BranchChanged { get; init; }
    public bool? HasTools { get; init; }
    public string? Health { get; init; }
    public string? Reason { get; init; }
}
public sealed record Fact
{
    public int Schema { get; init; } = 3;
    public long Id { get; init; }
    public string ProducerId { get; init; } = "";
    public string EventId { get; init; } = "";
    public string Agent { get; init; } = "";
    public string SessionNamespace { get; init; } = "";
    public string SessionId { get; init; } = "";
    public string? OperationId { get; init; }
    public DateTimeOffset? OccurredAt { get; init; }
    public DateTimeOffset ReceivedAt { get; init; }
    public string Kind { get; init; } = "";
    public FactData Data { get; init; } = new();
    public string? NativeType { get; init; }
    public string? NativeReason { get; init; }
    public string? ReportedBasis { get; init; }
    public string? StreamId { get; init; }
    public long? SourceSequence { get; init; }
    public string Origin { get; init; } = "";
    public bool Historical { get; init; }
    public string SourceId { get; init; } = "";
    public bool SequenceGap { get; init; }
    [JsonIgnore] public string SessionKey => string.Join("\u001f", Agent, SessionNamespace, SessionId);
    [JsonIgnore] public string LedgerKey => string.Join("\u001f", SourceId, SessionKey);
}
public sealed record EvidenceSession(string Agent, string SessionNamespace, string SessionId, string SourceId,
    string Origin, Fact[] Facts, string Coverage = "complete", string[]? IncompleteOperations = null)
{
    [JsonIgnore] public string Key => string.Join("\u001f", SourceId, Agent, SessionNamespace, SessionId);
}
public sealed record StreamBoundary(int Schema, string Epoch, long Revision);
public sealed record RecentFacts(int Schema, string Epoch, long Revision, Fact[] Facts, ulong Dropped = 0);
public sealed record RuntimeInfo(string Version, int Schema, string Mode, string Epoch);
public sealed record SourceStatus(string Id, string Agent, string Health, string? Reason, int? Format, string? SessionNamespace = null, string Origin = "file");
public sealed record Health(Dictionary<string, string> Roots, SourceStatus[] Sources, string Watcher,
    DateTimeOffset StartedAt = default, DateTimeOffset LastScanAt = default, long ScanMilliseconds = 0,
    bool CapacityExceeded = false, Dictionary<string, JsonElement>? Queue = null);
public sealed record Capability
{
    public string Agent { get; init; } = "";
    public string[]? Surfaces { get; init; }
    public int[]? Formats { get; init; }
    public string Activity { get; init; } = "";
    public string Tools { get; init; } = "";
    public string HumanWait { get; init; } = "";
    public string TurnEnd { get; init; } = "";
    public string? DesktopCertification { get; init; }
    public string? NoSession { get; init; }
    public string[] Facts { get; init; } = [];
    public string[] Limitations { get; init; } = [];
}

public sealed record Cursor(string Epoch, long Revision)
{
    public static Cursor Parse(string id)
    {
        var parts = id.Split(':');
        if (parts.Length != 2 || string.IsNullOrWhiteSpace(parts[0]) ||
            !long.TryParse(parts[1], out var revision) || revision < 0)
            throw new InvalidDataException("无效的事件游标。");
        return new(parts[0], revision);
    }
    public override string ToString() => $"{Epoch}:{Revision}";
}

public sealed record SseMessage(string Id, string Type, string Data);
public sealed record ClientEvent(DateTimeOffset ReceivedAt, string Type, Session? Session, string Summary,
    Fact? Fact = null, bool IsNotification = false, string? NotificationOutcome = null);
public sealed record StateView(Session[] Sessions, ClientEvent[] Events, Cursor? Cursor, long Version, string Coverage = "complete", Session? Activity = null);
