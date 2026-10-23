namespace AgentStatus.TestWindow.Core;

public sealed class MonitorState
{
    private readonly object gate = new();
    private readonly Queue<Fact> facts = new();
    private readonly Queue<ClientEvent> events = new();
    private readonly LatestActivity activity = new();
    private Cursor? cursor;
    private string? diagnosticEpoch;
    private long version;
    private bool monitoring, incomplete;
    private int bytes;
    private readonly Dictionary<string, DateTimeOffset> incompleteOperations = new();
    private void Lost(Fact fact)
    {
        string key = fact.LedgerKey + "\u001f" + fact.OperationId;
        incompleteOperations[key] = fact.ReceivedAt;
        if (incompleteOperations.Count > 4096) incompleteOperations.Remove(incompleteOperations.MinBy(p => p.Value).Key);
    }

    private void Retain(Fact fact)
    {
        if (facts.Any(f => f.Id == fact.Id)) return;
        facts.Enqueue(fact); bytes += System.Text.Json.JsonSerializer.SerializeToUtf8Bytes(fact, Wire.Json).Length;
        while (facts.Count > 4096 || bytes > 8 * 1024 * 1024 || facts.TryPeek(out var first) && fact.ReceivedAt - first.ReceivedAt >= TimeSpan.FromMinutes(10))
        { var removed = facts.Dequeue(); bytes -= System.Text.Json.JsonSerializer.SerializeToUtf8Bytes(removed, Wire.Json).Length; Lost(removed); incomplete = true; }
    }
    public StateView View(DateTimeOffset? now = null)
    {
        lock (gate)
        {
            var time = now ?? DateTimeOffset.UtcNow;
            while (facts.TryPeek(out var first) && time - first.ReceivedAt >= TimeSpan.FromMinutes(10))
            { var removed = facts.Dequeue(); bytes -= System.Text.Json.JsonSerializer.SerializeToUtf8Bytes(removed, Wire.Json).Length; Lost(removed); incomplete = true; }
            if (monitoring && activity.Tick(time) is { } notification) Add(notification);
            foreach (var key in incompleteOperations.Where(p => time - p.Value > TimeSpan.FromMinutes(20)).Select(p => p.Key).ToArray()) incompleteOperations.Remove(key);
            var ledgers = facts.GroupBy(f => f.LedgerKey).Select(g => new EvidenceSession(g.First().Agent,
                g.First().SessionNamespace, g.First().SessionId, g.First().SourceId, g.First().Origin,
                g.ToArray(), g.Any(f => f.SequenceGap || incompleteOperations.ContainsKey(f.LedgerKey + "\u001f" + f.OperationId)) ? "partial" : "complete",
                g.Where(f => f.SequenceGap || incompleteOperations.ContainsKey(f.LedgerKey + "\u001f" + f.OperationId)).Select(f => f.OperationId ?? "*").Distinct().ToArray()));
            return new(DefaultInterpreter.Project(ledgers, time), events.ToArray(), cursor, version,
                incomplete ? "partial" : "complete", activity.Visible(time));
        }
    }
    public void ResetMonitoring()
    { lock (gate) { monitoring = false; activity.Reset(); cursor = null; diagnosticEpoch = null; facts.Clear(); incompleteOperations.Clear(); bytes = 0; version++; } }
    private static void Validate(Fact fact)
    {
        if (fact.Schema != 3 || fact.Id <= 0 || fact.Data is null || fact.Origin is not ("file" or "external")
            || string.IsNullOrWhiteSpace(fact.SourceId) || string.IsNullOrWhiteSpace(fact.SessionId) || fact.Historical)
            throw new InvalidDataException("事实内容与协议不一致。");
    }
    public bool Apply(SseMessage message, DateTimeOffset? receivedAt = null)
    {
        var next = Cursor.Parse(message.Id);
        lock (gate)
        {
            var time = receivedAt ?? DateTimeOffset.UtcNow;
            if (message.Type is "ready" or "gap")
            {
                var boundary = Wire.Read<StreamBoundary>(message.Data);
                if (boundary.Schema != 3 || boundary.Epoch != next.Epoch || boundary.Revision != next.Revision)
                    throw new InvalidDataException("订阅边界与事件位置不一致。");
                if (diagnosticEpoch != next.Epoch) { facts.Clear(); bytes = 0; }
                diagnosticEpoch = next.Epoch;
                activity.Reset(); cursor = next; monitoring = true; incomplete = message.Type == "gap";
                Add(new(time, message.Type, null, message.Type == "gap"
                    ? "实时流存在缺口，已清除显示轮次；从最新边界继续。"
                    : "实时监测已连接；仅后续活动参与显示。"));
                return true;
            }
            if (message.Type != "fact" || cursor is null || next.Epoch != cursor.Epoch || next.Revision != cursor.Revision + 1)
            {
                if (cursor is not null && next.Epoch == cursor.Epoch && next.Revision <= cursor.Revision) return false;
                throw new InvalidDataException("实时流存在间隙，需要重新连接。");
            }
            var fact = Wire.Read<Fact>(message.Data); Validate(fact);
            if (fact.Id != next.Revision) throw new InvalidDataException("事实与事件位置不一致。");
            Retain(fact); incomplete |= fact.SequenceGap;
            if (activity.Tick(time) is { } inferred) Add(inferred);
            var notification = activity.Apply(fact, time);
            cursor = next;
            Add(new(time, "fact", notification?.Session, "", fact, notification is not null, notification?.NotificationOutcome));
            return true;
        }
    }
    // Diagnostic import never changes the live cursor, activity or notifications.
    public bool ImportRecent(RecentFacts recent)
    {
        if (recent.Schema != 3 || string.IsNullOrWhiteSpace(recent.Epoch) || recent.Facts is null)
            throw new InvalidDataException("近期诊断记录格式无效。");
        foreach (var fact in recent.Facts) { Validate(fact); if (fact.Id > recent.Revision) throw new InvalidDataException("无效的近期记录位置。"); }
        lock (gate)
        {
            if (cursor is not null && recent.Epoch != cursor.Epoch) return false;
            if (diagnosticEpoch != recent.Epoch) { facts.Clear(); bytes = 0; }
            diagnosticEpoch = recent.Epoch;
            var ordered = facts.Concat(recent.Facts).DistinctBy(f => f.Id).OrderBy(f => f.Id).ToArray();
            facts.Clear(); bytes = 0;
            foreach (var fact in ordered) Retain(fact);
            version++; return true;
        }
    }
    public void ClearEvents() { lock (gate) { events.Clear(); version++; } }
    private void Add(ClientEvent entry) { events.Enqueue(entry); while (events.Count > 500) events.Dequeue(); version++; }
}
