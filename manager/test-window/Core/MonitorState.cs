namespace AgentStatus.TestWindow.Core;

public sealed class MonitorState
{
    private readonly object gate = new();
    private readonly Dictionary<string, EvidenceSession> ledgers = new();
    private readonly Queue<ClientEvent> events = new();
    private readonly HashSet<string> endings = new();
    private readonly Queue<string> endingOrder = new();
    private readonly LatestActivity activity = new();
    private bool monitoring;
    private Cursor? cursor;
    private long viewRevision, version;
    private string snapshotCoverage = "complete";
    private bool streamGap;

    public StateView View(DateTimeOffset? now = null)
    {
        lock (gate)
        {
            var time = now ?? DateTimeOffset.UtcNow;
            if (monitoring && activity.Tick(time) is { } notification) Add(notification);
            return new(DefaultInterpreter.Project(ledgers.Values, time), events.ToArray(), cursor, version,
                streamGap || snapshotCoverage != "complete" || ledgers.Values.Any(s=>s.Coverage!="complete") ? "partial" : "complete", activity.Visible(time));
        }
    }

    public void ResetMonitoring()
    { lock (gate) { monitoring = false; activity.Reset(); version++; } }

    private static void Validate(RuntimeSnapshot snapshot)
    {
        if (snapshot.Schema != 2 || string.IsNullOrWhiteSpace(snapshot.Epoch) || snapshot.Revision < 0 || snapshot.Sessions is null
            || snapshot.Sessions.Any(s => s is null || string.IsNullOrWhiteSpace(s.SessionId) || string.IsNullOrWhiteSpace(s.SourceId)
                || s.Facts is null || s.Facts.Any(f => f is null || f.Data is null || f.Schema != 2 || f.Id <= 0 || f.Id > snapshot.Revision)))
            throw new InvalidDataException("不兼容的事实快照。");
    }

    private void Replace(RuntimeSnapshot snapshot)
    {
        ledgers.Clear();
        foreach (var ledger in snapshot.Sessions) ledgers[ledger.Key] = ledger;
        snapshotCoverage = snapshot.Coverage;
        viewRevision = snapshot.Revision; version++;
    }

    private bool RememberEnd(Fact f)
    {
        string key = DefaultInterpreter.EndKey(f);
        if (!endings.Add(key)) return false;
        endingOrder.Enqueue(key);
        while (endingOrder.Count > 32768) endings.Remove(endingOrder.Dequeue());
        return true;
    }

    public bool Apply(SseMessage message, DateTimeOffset? receivedAt = null)
    {
        var next = Cursor.Parse(message.Id);
        lock (gate)
        {
            var time = receivedAt ?? DateTimeOffset.UtcNow;
            if (message.Type is "snapshot" or "reset")
            {
                var snapshot = Wire.Read<RuntimeSnapshot>(message.Data); Validate(snapshot);
                if (snapshot.Epoch != next.Epoch || snapshot.Revision != next.Revision) throw new InvalidDataException("快照与游标不一致。");
                if (monitoring && cursor is not null && next.Epoch == cursor.Epoch && next.Revision <= cursor.Revision) return false;
                if (cursor?.Epoch != next.Epoch) { endings.Clear(); endingOrder.Clear(); viewRevision = 0; }
                if (next.Revision >= viewRevision) Replace(snapshot);
                foreach (var fact in snapshot.Sessions.SelectMany(s => s.Facts)) if (DefaultInterpreter.EndOutcome(fact) is not null) RememberEnd(fact);
                cursor = next;
                activity.Reset(); monitoring = true;
                if (message.Type == "reset") streamGap = true;
                Add(new(DateTimeOffset.UtcNow, message.Type, null, message.Type == "reset"
                    ? "事实流已重置，存在无法重放的区间；快照不生成完成通知。" : $"建立事实快照：{snapshot.Sessions.Length} 个来源会话（不产生完成通知）。"));
                return true;
            }
            if (message.Type != "fact" || cursor is null || next.Epoch != cursor.Epoch || next.Revision != cursor.Revision + 1)
            {
                if (cursor is not null && next.Epoch == cursor.Epoch && next.Revision <= cursor.Revision) return false;
                throw new InvalidDataException("事实流存在间隙，需要重新获取快照。");
            }
            var factUpdate = Wire.Read<Fact>(message.Data);
            if (factUpdate.Schema != 2 || factUpdate.Id != next.Revision || factUpdate.Data is null || factUpdate.Origin is not ("file" or "external")
                || string.IsNullOrWhiteSpace(factUpdate.SourceId) || string.IsNullOrWhiteSpace(factUpdate.SessionId))
                throw new InvalidDataException("事实内容与协议不一致。");
            if (next.Revision > viewRevision) ApplyFact(factUpdate);
            if (factUpdate.SequenceGap) streamGap = true;
            string? end = DefaultInterpreter.EndOutcome(factUpdate);
            bool uniqueEnd = end is null || RememberEnd(factUpdate);
            ClientEvent? notification = null;
            if (monitoring && !factUpdate.Historical)
            {
                if (activity.Tick(time) is { } inferred) Add(inferred);
                if (uniqueEnd) notification = activity.Apply(factUpdate, time);
            }
            cursor = next; viewRevision = Math.Max(viewRevision, next.Revision);
            Add(new(time, "fact", notification?.Session, "", factUpdate, notification is not null, notification?.NotificationOutcome));
            return true;
        }
    }

    private void ApplyFact(Fact fact)
    {
        ledgers.TryGetValue(fact.LedgerKey, out var prior);
        var facts = (prior?.Facts ?? []).Append(fact).ToArray();
        string coverage = fact.SequenceGap ? "partial" : prior?.Coverage ?? "complete";
        var incomplete=(prior?.IncompleteOperations??[]).ToHashSet();
        if(fact.SequenceGap)incomplete.Add(string.IsNullOrEmpty(fact.OperationId)?"*":fact.OperationId);
        var operations = facts.Reverse().Where(f => f.Kind is not ("source.heartbeat" or "source.health" or "session.observed")).Select(f => f.OperationId).Where(id => !string.IsNullOrEmpty(id)).Distinct().ToArray();
        if (operations.Length > 32) coverage = "partial";
        var allowed = operations.Take(32).ToHashSet();
        var latest = new Dictionary<string,Fact>();
        foreach (var f in facts.Where(f => string.IsNullOrEmpty(f.OperationId) || allowed.Contains(f.OperationId)))
        {
            string key = string.Join("\u001f",f.OperationId,f.Kind,f.Data.Scope);
            if (f.Kind is "tool.observed" or "approval.observed")
            {
                string pair = key+"\u001fpair\u001f"+(f.Data.CallId??f.Data.RequestId);
                if (f.Data.Phase is "finished" or "resolved") latest.Remove(pair); else latest[pair]=f;
                key+="\u001flast";
            }
            else if(f.Kind=="lifecycle.observed") key+="\u001f"+f.Data.Phase;
            else if(f.Kind=="source.heartbeat") key="source.heartbeat";
            latest[key]=f;
        }
        facts=latest.Values.DistinctBy(f=>f.Id).OrderBy(f=>f.Id).ToArray();
        if (facts.Length > 2048)
        { foreach(var lost in facts[..^2048])incomplete.Add(string.IsNullOrEmpty(lost.OperationId)?"*":lost.OperationId); facts = facts[^2048..]; coverage = "partial"; }
        if(incomplete.Count>33)incomplete=["*"];
        ledgers[fact.LedgerKey] = new(fact.Agent, fact.SessionNamespace, fact.SessionId, fact.SourceId, fact.Origin, facts, coverage,incomplete.ToArray());
    }

    public bool Reconcile(RuntimeSnapshot snapshot)
    {
        Validate(snapshot);
        lock (gate)
        {
            if (cursor is null || snapshot.Epoch != cursor.Epoch || snapshot.Revision < viewRevision) return false;
            Replace(snapshot); return true;
        }
    }
    public void ClearEvents() { lock (gate) { events.Clear(); version++; } }
    private void Add(ClientEvent entry) { events.Enqueue(entry); while (events.Count > 500) events.Dequeue(); version++; }
}
