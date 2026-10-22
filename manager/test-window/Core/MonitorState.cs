namespace AgentStatus.TestWindow.Core;

public sealed class MonitorState
{
    private readonly object gate = new();
    private readonly Dictionary<string, Session> sessions = new();
    private readonly Queue<ClientEvent> events = new();
    private Cursor? cursor;
    private long viewRevision;
    private long version;

    public StateView View()
    {
        lock (gate) return new(sessions.Values.OrderBy(s => s.Agent).ThenBy(s => s.SessionId).ToArray(),
            events.ToArray(), cursor, version);
    }

    public bool Apply(SseMessage message)
    {
        var next = Cursor.Parse(message.Id);
        lock (gate)
        {
            if (message.Type is "snapshot" or "reset")
            {
                var snapshot = Wire.Read<RuntimeSnapshot>(message.Data);
                if (snapshot.Schema != 1 || snapshot.Epoch != next.Epoch || snapshot.Revision != next.Revision || snapshot.Sessions is null)
                    throw new InvalidDataException("不兼容的状态快照。");
                if (cursor is not null && next.Epoch == cursor.Epoch &&
                    (next.Revision <= cursor.Revision || next.Revision < viewRevision)) return false;
                if (snapshot.Sessions.Any(s => s is null || string.IsNullOrWhiteSpace(s.SessionId)))
                    throw new InvalidDataException("会话缺少标识。");
                sessions.Clear();
                foreach (var session in snapshot.Sessions)
                {
                    sessions[session.SessionId] = session;
                }
                cursor = next;
                viewRevision = next.Revision;
                Add(new(DateTimeOffset.UtcNow, message.Type, null, message.Type == "reset"
                    ? "事件流已重置，缺失区间不可重放；已重新获取快照。"
                    : $"建立快照：{sessions.Count} 个会话（不产生完成通知）。"));
                return true;
            }
            if (cursor is null || next.Epoch != cursor.Epoch)
                throw new InvalidDataException("事件流需要重新获取快照。");
            if (next.Revision <= cursor.Revision) return false;
            if (next.Revision != cursor.Revision + 1)
                throw new InvalidDataException("事件序号存在间隙，需要重新获取快照。");
            var update = Wire.Read<RuntimeEvent>(message.Data);
            if (update.Id != next.Revision || update.Type != message.Type)
                throw new InvalidDataException("事件内容与游标不一致。");
            if (next.Revision > viewRevision && update.Session is { SourceHealth: not "superseded" } changed && !string.IsNullOrWhiteSpace(changed.SessionId))
                sessions[changed.SessionId] = changed;
            cursor = next;
            viewRevision = Math.Max(viewRevision, next.Revision);
            Add(new(DateTimeOffset.UtcNow, message.Type, update.Session, ""));
            return true;
        }
    }

    // The Runtime computes some metadata (idle freshness, source removal) only
    // in snapshots. Reconcile without consuming the SSE cursor or notifications.
    public bool Reconcile(RuntimeSnapshot snapshot)
    {
        lock (gate)
        {
            if (snapshot.Schema != 1 || snapshot.Sessions is null ||
                snapshot.Sessions.Any(s => s is null || string.IsNullOrWhiteSpace(s.SessionId)))
                throw new InvalidDataException("不兼容的状态快照。");
            if (cursor is null || snapshot.Epoch != cursor.Epoch || snapshot.Revision < viewRevision) return false;
            sessions.Clear();
            foreach (var session in snapshot.Sessions) sessions[session.SessionId] = session;
            viewRevision = snapshot.Revision;
            version++;
            return true;
        }
    }

    public void ClearEvents() { lock (gate) { events.Clear(); version++; } }

    private void Add(ClientEvent entry)
    {
        events.Enqueue(entry);
        while (events.Count > 500) events.Dequeue();
        version++;
    }
}
