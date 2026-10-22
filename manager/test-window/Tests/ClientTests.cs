using System.Net;
using System.Text;
using System.Text.Json;
using AgentStatus.TestWindow.Core;

namespace Tests;

[TestClass]
public sealed class ClientTests
{
    private static string Json(object value) => JsonSerializer.Serialize(value, Wire.Json);
    private static Session Sample(string id = "one", string state = "working") => new()
    { SessionId = id, NativeSessionId = id, Agent = "codex", State = state, UpdatedAt = DateTimeOffset.UtcNow };
    private static SseMessage Snapshot(string epoch = "a", long revision = 0, string type = "snapshot", params Session[] sessions) =>
        new($"{epoch}:{revision}", type, Json(new RuntimeSnapshot(1, epoch, revision, sessions)));
    private static SseMessage Change(long id, Session session, string type = "state.changed") =>
        new($"a:{id}", type, Json(new RuntimeEvent(id, type, session)));

    [TestMethod]
    public async Task MissingDamagedAndNonLocalConnectionFilesAreRejected()
    {
        using var directory = new TempDirectory();
        await Assert.ThrowsAsync<FileNotFoundException>(() => RuntimeConnection.LoadAsync(directory.Path, default));
        Directory.CreateDirectory(System.IO.Path.Combine(directory.Path, "private"));
        var path = System.IO.Path.Combine(directory.Path, "private", "connection.json");
        await File.WriteAllTextAsync(path, "{secret-broken");
        var damaged = await Assert.ThrowsAsync<InvalidDataException>(() => RuntimeConnection.LoadAsync(directory.Path, default));
        Assert.DoesNotContain("secret-broken", damaged.Message);
        foreach (var url in new[] { "https://127.0.0.1:9120", "http://example.com", "http://192.168.1.10", "http://user@127.0.0.1", "http://127.0.0.1/?token=bad" })
        {
            await File.WriteAllTextAsync(path, Json(new { url, token = "SECRET" }));
            await Assert.ThrowsAsync<InvalidDataException>(() => RuntimeConnection.LoadAsync(directory.Path, default));
        }
        await File.WriteAllTextAsync(path, Json(new { url = "http://127.0.0.1:9120", token = "SECRET" }));
        var connection = await RuntimeConnection.LoadAsync(directory.Path, default);
        Assert.DoesNotContain("SECRET", connection.ToString());
    }

    [TestMethod]
    public void DiscoveryHonorsExplicitDirectoryAndProjectBeforeUserDefault()
    {
        using var root = new TempDirectory();
        var bin = System.IO.Path.Combine(root.Path, "bin");
        var project = System.IO.Path.Combine(root.Path, ".runtime", "live");
        Assert.AreEqual(System.IO.Path.Combine(root.Path, "AgentStatusRuntime"), RuntimeConnection.DiscoverDirectory(null, bin, root.Path));
        Directory.CreateDirectory(System.IO.Path.Combine(project, "private"));
        File.WriteAllText(System.IO.Path.Combine(project, "private", "connection.json"), "{}");
        Assert.AreEqual(project, RuntimeConnection.DiscoverDirectory(null, bin, root.Path));
        Assert.AreEqual(root.Path, RuntimeConnection.DiscoverDirectory(root.Path, bin, root.Path));
    }

    [TestMethod]
    public async Task UnauthorizedResponseIsReportedWithoutCredentials()
    {
        using var client = new RuntimeClient(new FakeHandler((request, _) =>
        {
            Assert.AreEqual("Bearer", request.Headers.Authorization?.Scheme);
            Assert.AreEqual("SECRET", request.Headers.Authorization?.Parameter);
            return Task.FromResult(new HttpResponseMessage(HttpStatusCode.Unauthorized));
        }));
        var error = await Assert.ThrowsAsync<HttpRequestException>(() => client.GetAsync<RuntimeInfo>(new(new("http://127.0.0.1:9120"), "SECRET"), "/v1/info", default));
        Assert.AreEqual(HttpStatusCode.Unauthorized, error.StatusCode);
        Assert.DoesNotContain("SECRET", error.Message);
    }

    [TestMethod]
    public async Task SegmentedUtf8MultilineDataAndHeartbeatAreParsed()
    {
        const string wire = ": heartbeat\r\n\r\nid: a:1\r\nevent: state.changed\r\ndata: 中文\r\ndata: 第二行\r\n\r\n";
        using var stream = new ChunkStream(Encoding.UTF8.GetBytes(wire), 1);
        var messages = new List<SseMessage>(); int received = 0;
        await foreach (var message in SseReader.ReadAsync(stream, () => received++, default)) messages.Add(message);
        Assert.HasCount(1, messages);
        Assert.AreEqual("a:1", messages[0].Id);
        Assert.AreEqual("中文\n第二行", messages[0].Data);
        Assert.IsGreaterThan(0, received);
    }

    [TestMethod]
    public void SnapshotsResetsAndDeduplicationDoNotInventTurnEndEvents()
    {
        var state = new MonitorState();
        state.Apply(Snapshot(sessions: [Sample()]));
        Assert.IsFalse(state.Apply(Snapshot(sessions: [Sample()])));
        Assert.HasCount(1, state.View().Events);
        Assert.IsTrue(state.Apply(Change(1, Sample(state: "idle"), "turn.ended")));
        Assert.IsFalse(state.Apply(Change(1, Sample())));
        state.Apply(Snapshot("b", 8, "reset", Sample("new")));
        Assert.IsFalse(state.Apply(Snapshot("b", 8, "reset", Sample("new"))));
        Assert.HasCount(1, state.View().Sessions);
        Assert.AreEqual("new", state.View().Sessions[0].SessionId);
        Assert.AreEqual("b:8", state.View().Cursor?.ToString());
        Assert.HasCount(1, state.View().Events.Where(e => e.Type == "turn.ended").ToArray());
        Assert.AreEqual("reset", state.View().Events[^1].Type);
    }

    [TestMethod]
    public void GapsAndMismatchedEpochsRequestFreshSnapshots()
    {
        var state = new MonitorState(); state.Apply(Snapshot(sessions: [Sample()]));
        Assert.Throws<InvalidDataException>(() => state.Apply(Change(2, Sample())));
        Assert.Throws<InvalidDataException>(() => state.Apply(Change(1, Sample()) with { Id = "wrong:1" }));
        Assert.AreEqual("a:0", state.View().Cursor?.ToString());
        Assert.HasCount(1, state.View().Sessions);
    }

    [TestMethod]
    public void EventHistoryIsBoundedAndClearDoesNotLoseSnapshotOrCursor()
    {
        var state = new MonitorState(); state.Apply(Snapshot(sessions: [Sample()]));
        for (int id = 1; id <= 700; id++) state.Apply(Change(id, Sample()));
        Assert.HasCount(500, state.View().Events);
        state.ClearEvents();
        Assert.HasCount(0, state.View().Events);
        Assert.HasCount(1, state.View().Sessions);
        Assert.AreEqual(700L, state.View().Cursor?.Revision);
    }

    [TestMethod]
    public void ReconciliationUpdatesFreshnessAndRemovesSupersededSourcesWithoutConsumingEvents()
    {
        var state = new MonitorState();
        state.Apply(Snapshot(sessions: [Sample("old"),Sample("same")]));
        state.Apply(Change(1,Sample("same") with { TurnId="new",SourceHealth="healthy" }));
        state.Apply(Change(2,Sample("same") with { SourceHealth="superseded" },"source.degraded"));
        Assert.AreEqual("healthy",state.View().Sessions.Single(s=>s.SessionId=="same").SourceHealth);
        var count=state.View().Events.Length;
        var version=state.View().Version;
        Assert.IsTrue(state.Reconcile(new(1,"a",4,[Sample("same","idle") with {Freshness="stale"}])));
        Assert.HasCount(1,state.View().Sessions);
        Assert.AreEqual("stale",state.View().Sessions[0].Freshness);
        Assert.AreEqual(2L,state.View().Cursor?.Revision);
        Assert.IsGreaterThan(version,state.View().Version);
        Assert.IsFalse(state.Reconcile(new(1,"a",1,[Sample("old")])));
        state.Apply(Change(3,Sample("old"),"turn.ended"));
        Assert.HasCount(1,state.View().Sessions);
        Assert.HasCount(count+1,state.View().Events);
        Assert.AreEqual("turn.ended",state.View().Events[^1].Type);
    }

    [TestMethod]
    public async Task OversizedUnterminatedSseLineIsBounded()
    {
        using var stream=new MemoryStream(Encoding.UTF8.GetBytes("data: "+new string('x',1024*1024)));
        await Assert.ThrowsAsync<InvalidDataException>(async()=>
        {await foreach(var _ in SseReader.ReadAsync(stream,()=>{},default)){} });
    }

    [TestMethod]
    public async Task IncompleteFrameDoesNotAdvanceStateAndIdleReadIsCancelled()
    {
        using var partial = new MemoryStream(Encoding.UTF8.GetBytes("id: a:1\nevent: state.changed\ndata: {}"));
        int count = 0;
        await foreach (var _ in SseReader.ReadAsync(partial, () => { }, default)) count++;
        Assert.AreEqual(0, count);
        using var blocked = new BlockingStream();
        await Assert.ThrowsAsync<TimeoutException>(async () =>
        {
            await foreach (var _ in SseReader.ReadAsync(blocked, () => { }, default, TimeSpan.FromMilliseconds(50))) { }
        });
    }

    [TestMethod]
    public async Task ControllerRereadsTokenAfterDisconnectAndCancellationStopsRetries()
    {
        using var root = new TempDirectory();
        var path = System.IO.Path.Combine(root.Path, "private", "connection.json");
        Directory.CreateDirectory(System.IO.Path.GetDirectoryName(path)!);
        await File.WriteAllTextAsync(path, Json(new { url = "http://127.0.0.1:9120", token = "old-token" }));
        using var stop = new CancellationTokenSource(TimeSpan.FromSeconds(8));
        int streams = 0; bool newTokenSeen = false;
        using var client = new RuntimeClient(new FakeHandler(async (request, token) =>
        {
            var route = request.RequestUri!.AbsolutePath;
            if (route == "/v1/info")
            {
                if (request.Headers.Authorization?.Parameter == "new-token") newTokenSeen = true;
                return JsonResponse(new RuntimeInfo("sample", 1, "passive", "a"));
            }
            if (route == "/v1/health") return JsonResponse(new Health([], [], "polling"));
            if (route == "/v1/capabilities") return JsonResponse(Array.Empty<Capability>());
            if (route == "/v1/status") return JsonResponse(new RuntimeSnapshot(1,"a",0,[Sample()]));
            if (Interlocked.Increment(ref streams) == 1)
            {
                await File.WriteAllTextAsync(path, Json(new { url = "http://127.0.0.1:9120", token = "new-token" }), token);
                return new(HttpStatusCode.Unauthorized);
            }
            var snapshot = Snapshot(sessions: [Sample()]);
            return new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent($"id: {snapshot.Id}\nevent: snapshot\ndata: {snapshot.Data}\n\n", Encoding.UTF8, "text/event-stream")
            };
        }));
        using var controller = new MonitorController(new(), client);
        controller.Changed += () => { if (controller.State.View().Sessions.Length == 1) stop.Cancel(); };
        await controller.RunAsync(root.Path, stop.Token);
        Assert.IsTrue(newTokenSeen);
        Assert.AreEqual(2, streams);
        Assert.HasCount(1, controller.State.View().Sessions);
        CollectionAssert.AreEqual(new[] { 1D, 2D, 5D, 10D, 10D }, Enumerable.Range(1, 5).Select(n => MonitorController.RetryDelay(n).TotalSeconds).ToArray());
    }

    internal static HttpResponseMessage JsonResponse(object body) => new(HttpStatusCode.OK)
    { Content = new StringContent(Json(body), Encoding.UTF8, "application/json") };
}

internal sealed class TempDirectory : IDisposable
{
    public string Path { get; } = System.IO.Path.Combine(System.IO.Path.GetTempPath(), "agent-status-window-" + Guid.NewGuid().ToString("N"));
    public TempDirectory() => Directory.CreateDirectory(Path);
    public void Dispose()
    {
        // Windows can briefly retain SQLite mappings after the fixture process
        // exits. Retry only cleanup of this uniquely owned temporary directory.
        var expectedPrefix=System.IO.Path.Combine(System.IO.Path.GetTempPath(),"agent-status-window-");
        if(!System.IO.Path.GetFullPath(Path).StartsWith(expectedPrefix,StringComparison.OrdinalIgnoreCase))
            throw new InvalidOperationException("Unexpected test directory.");
        for(int attempt=0;;attempt++)
        {
            try{Directory.Delete(Path,true);return;}
            catch(IOException) when(attempt<9){Thread.Sleep(50);}
        }
    }
}

internal sealed class FakeHandler(Func<HttpRequestMessage, CancellationToken, Task<HttpResponseMessage>> action) : HttpMessageHandler
{
    protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken) => action(request, cancellationToken);
}

internal sealed class ChunkStream(byte[] data, int chunkSize) : MemoryStream(data)
{
    public override ValueTask<int> ReadAsync(Memory<byte> buffer, CancellationToken cancellationToken = default) => base.ReadAsync(buffer[..Math.Min(buffer.Length, chunkSize)], cancellationToken);
}

internal sealed class BlockingStream : Stream
{
    public override bool CanRead => true;
    public override bool CanSeek => false;
    public override bool CanWrite => false;
    public override long Length => throw new NotSupportedException();
    public override long Position { get => 0; set => throw new NotSupportedException(); }
    public override async ValueTask<int> ReadAsync(Memory<byte> buffer, CancellationToken cancellationToken = default)
    { await Task.Delay(Timeout.Infinite, cancellationToken); return 0; }
    public override int Read(byte[] buffer, int offset, int count) => throw new NotSupportedException();
    public override void Flush() => throw new NotSupportedException();
    public override long Seek(long offset, SeekOrigin origin) => throw new NotSupportedException();
    public override void SetLength(long value) => throw new NotSupportedException();
    public override void Write(byte[] buffer, int offset, int count) => throw new NotSupportedException();
}
