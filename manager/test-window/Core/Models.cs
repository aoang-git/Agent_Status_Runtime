using System.Text.Json;

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
    public string SourceHealth { get; init; } = "";
    public string Freshness { get; init; } = "";
    public Dictionary<string, bool>? PendingTools { get; init; }
    public Dictionary<string, bool>? PendingApprovals { get; init; }
}

public sealed record RuntimeSnapshot(int Schema, string Epoch, long Revision, Session[] Sessions);
public sealed record RuntimeInfo(string Version, int Schema, string Mode, string Epoch);
public sealed record SourceStatus(string Id, string Agent, string Health, string? Reason, int? Format);
public sealed record Health(Dictionary<string, string> Roots, SourceStatus[] Sources, string Watcher);
public sealed record RuntimeEvent(long Id, string Type, Session? Session);
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
public sealed record ClientEvent(DateTimeOffset ReceivedAt, string Type, Session? Session, string Summary);
public sealed record StateView(Session[] Sessions, ClientEvent[] Events, Cursor? Cursor, long Version);
