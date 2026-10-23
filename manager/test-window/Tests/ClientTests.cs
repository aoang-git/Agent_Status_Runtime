using System.Net;
using System.Text;
using System.Text.Json;
using AgentStatus.TestWindow.Core;

namespace Tests;

[TestClass]
public sealed class ClientTests
{
    private static string Json(object value) => JsonSerializer.Serialize(value, Wire.Json);
    private static Fact Sample(long id=1, string kind="input.observed", string origin="file") => new()
    { Id=id, ProducerId="p", EventId=id.ToString(), Agent="codex", SessionNamespace="local", SessionId="one", OperationId="op", Kind=kind, SourceId="source", Origin=origin, ReceivedAt=DateTimeOffset.UtcNow, OccurredAt=DateTimeOffset.UtcNow };
    private static EvidenceSession Ledger(params Fact[] facts) => new("codex","local","one","source","file",facts);
    private static SseMessage Snapshot(string epoch="a",long revision=0,string type="ready",params Fact[] facts) =>
        new($"{epoch}:{revision}",type=="reset"?"gap":type,Json(new StreamBoundary(3,epoch,revision)));
    private static SseMessage Change(long id, Fact? fact=null) => new($"a:{id}","fact",Json(fact??Sample(id)));

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
            await File.WriteAllTextAsync(path, Json(new { url, token = "SECRET", epoch = "a", pid = 123 }));
            await Assert.ThrowsAsync<InvalidDataException>(() => RuntimeConnection.LoadAsync(directory.Path, default));
        }
        await File.WriteAllTextAsync(path, Json(new { url = "http://127.0.0.1:9120", token = "SECRET", epoch = "a", pid = 123 }));
        var connection = await RuntimeConnection.LoadAsync(directory.Path, default);
        Assert.DoesNotContain("SECRET", connection.ToString());
    }

    [TestMethod]
    public void DiscoveryPrefersInstalledDirectoryAndOnlyFallsBackToCurrentProjectConnection()
    {
        using var root = new TempDirectory();
        var bin = System.IO.Path.Combine(root.Path, "bin");
        var project = System.IO.Path.Combine(root.Path, ".runtime", "live");
        var installed = System.IO.Path.Combine(root.Path, "AgentStatusRuntime");
        Assert.AreEqual(installed, RuntimeConnection.DiscoverDirectory(null, bin, root.Path));
        Directory.CreateDirectory(System.IO.Path.Combine(project, "private"));
        var projectConnection = System.IO.Path.Combine(project, "private", "connection.json");
        File.WriteAllText(projectConnection, Json(new { url = "http://127.0.0.1:9120", token = "SECRET" }));
        Assert.AreEqual(installed, RuntimeConnection.DiscoverDirectory(null, bin, root.Path), "旧项目连接文件不得遮住用户运行目录。");
        File.WriteAllText(projectConnection, Json(new { url = "http://127.0.0.1:9120", token = "SECRET", epoch = "a", pid = 123 }));
        Assert.AreEqual(project, RuntimeConnection.DiscoverDirectory(null, bin, root.Path));
        Directory.CreateDirectory(System.IO.Path.Combine(installed, "private"));
        File.WriteAllText(System.IO.Path.Combine(installed, "private", "connection.json"), Json(new { url = "http://127.0.0.1:9121", token = "SECRET", epoch = "b", pid = 456 }));
        Assert.AreEqual(installed, RuntimeConnection.DiscoverDirectory(null, bin, root.Path), "用户安装的服务应优先于开发目录。");
        File.WriteAllText(projectConnection, "{broken");
        Assert.AreEqual(installed, RuntimeConnection.DiscoverDirectory(null, bin, root.Path));
        Assert.AreEqual(root.Path, RuntimeConnection.DiscoverDirectory(root.Path, bin, root.Path));
    }

    [TestMethod]
    public async Task OldConnectionFileIsReportedAsIncompatibleInsteadOfDamaged()
    {
        using var root = new TempDirectory();
        Directory.CreateDirectory(System.IO.Path.Combine(root.Path, "private"));
        var path = System.IO.Path.Combine(root.Path, "private", "connection.json");
        await File.WriteAllTextAsync(path, Json(new { url = "http://127.0.0.1:9120", token = "SECRET" }));
        var error = await Assert.ThrowsAsync<InvalidDataException>(() => RuntimeConnection.LoadAsync(root.Path, default));
        StringAssert.Contains(error.Message, "服务实例标识");
        StringAssert.Contains(error.Message, "选择");
        Assert.DoesNotContain("损坏", error.Message);
        Assert.DoesNotContain("SECRET", error.Message);
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
        var error = await Assert.ThrowsAsync<HttpRequestException>(() => client.GetAsync<RuntimeInfo>(new(new("http://127.0.0.1:9120"), "SECRET"), "/v3/info", default));
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
    public void SnapshotsResetsAndDuplicateFactsDoNotInventNotifications()
    {
        var state=new MonitorState();
        state.Apply(Snapshot(revision:1,facts:[Sample()]));
        Assert.IsNull(state.View().Activity);
        var ended=Sample(2,"lifecycle.observed") with {Data=new(){Phase="ended",Scope="agent_turn",Outcome="completed"}};
        Assert.IsTrue(state.Apply(Change(2,ended)));
        Assert.IsFalse(state.Apply(Change(2,ended)));
        Assert.HasCount(1,state.View().Events.Where(e=>e.IsNotification).ToArray());
        state.ClearEvents();
        state.Apply(Change(3,ended with {Id=3}));
        Assert.HasCount(0,state.View().Events.Where(e=>e.IsNotification).ToArray());
        state.Apply(Snapshot("b",8,"reset",ended with {Id=8,Historical=true}));
        Assert.HasCount(0,state.View().Events.Where(e=>e.IsNotification).ToArray());
        Assert.AreEqual("b:8",state.View().Cursor?.ToString());
    }

    [TestMethod]
    public void GapsAndMismatchedEpochsKeepTheLastSnapshot()
    {
        var state=new MonitorState();state.Apply(Snapshot(revision:1,facts:[Sample()]));
        Assert.Throws<InvalidDataException>(()=>state.Apply(Change(3)));
        Assert.Throws<InvalidDataException>(()=>state.Apply(Change(2) with {Id="wrong:2"}));
        Assert.AreEqual("a:1",state.View().Cursor?.ToString());
        Assert.HasCount(0,state.View().Sessions);
    }

    [TestMethod]
    public void EventHistoryIsBoundedAndClearPreservesSnapshotAndCursor()
    {
        var state=new MonitorState();state.Apply(Snapshot());
        for(int id=1;id<=700;id++)state.Apply(Change(id));
        Assert.HasCount(500,state.View().Events);state.ClearEvents();
        Assert.HasCount(0,state.View().Events);Assert.HasCount(1,state.View().Sessions);
        Assert.AreEqual(700L,state.View().Cursor?.Revision);
    }

    [TestMethod]
    public void RecentIsDiagnosticOnlyAndDoesNotConsumeLiveEvents()
    {
        var state = new MonitorState();
        var seed = Sample(); state.ImportRecent(new(3,"a",1,[seed]));
        state.Apply(Snapshot()); Assert.HasCount(1, state.View().Sessions);
        var end = Sample(1, "lifecycle.observed") with { Data = new() { Phase = "ended", Scope = "agent_turn", Outcome = "completed" } };
        state.ImportRecent(new(3,"a",1,[end]));
        Assert.IsNull(state.View().Activity); Assert.AreEqual(0L,state.View().Cursor?.Revision);
        Assert.HasCount(0,state.View().Events.Where(e=>e.IsNotification).ToArray());
        state.Apply(Change(1,end)); Assert.HasCount(1,state.View().Events.Where(e=>e.IsNotification).ToArray());
        state.Apply(Snapshot(revision:4,type:"gap")); Assert.IsNull(state.View().Activity);
    }

    [TestMethod]
    public async Task OversizedUnterminatedSseLineIsBounded()
    {
        using var stream=new MemoryStream(Encoding.UTF8.GetBytes("data: "+new string('x',8*1024*1024)));
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
        await File.WriteAllTextAsync(path, Json(new { url = "http://127.0.0.1:9120", token = "old-token", epoch = "a", pid = 123 }));
        using var stop = new CancellationTokenSource(TimeSpan.FromSeconds(8));
        int streams = 0; bool newTokenSeen = false;
        using var client = new RuntimeClient(new FakeHandler(async (request, token) =>
        {
            var route = request.RequestUri!.AbsolutePath;
            if (route == "/v3/info")
            {
                if (request.Headers.Authorization?.Parameter == "new-token") newTokenSeen = true;
                return JsonResponse(new RuntimeInfo("sample", 3, "facts", "a"));
            }
            if (route == "/v3/health") return JsonResponse(new Health([], [], "polling"));
            if (route == "/v3/capabilities") return JsonResponse(Array.Empty<Capability>());
            if (route == "/v3/recent") return JsonResponse(new RecentFacts(3,"a",0,[]));
            Assert.IsFalse(request.Headers.Contains("Last-Event-ID"), "每次连接应建立新基线，不恢复断线期间的事实流。");
            if (Interlocked.Increment(ref streams) == 1)
            {
                await File.WriteAllTextAsync(path, Json(new { url = "http://127.0.0.1:9120", token = "new-token", epoch = "a", pid = 123 }), token);
                return new(HttpStatusCode.Unauthorized);
            }
            var snapshot = Snapshot(revision:1,facts: [Sample()]);
            return new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent($"id: {snapshot.Id}\nevent: ready\ndata: {snapshot.Data}\n\n", Encoding.UTF8, "text/event-stream")
            };
        }));
        var state = new MonitorState();
        state.Apply(Snapshot());
        using var controller = new MonitorController(state, client);
        controller.Changed += () => { if (controller.State.View().Cursor?.Revision == 1) stop.Cancel(); };
        await controller.RunAsync(root.Path, stop.Token);
        Assert.IsTrue(newTokenSeen);
        Assert.AreEqual(2, streams);
        Assert.IsNull(controller.State.View().Activity);
        CollectionAssert.AreEqual(new[] { 1D, 2D, 5D, 10D, 10D }, Enumerable.Range(1, 5).Select(n => MonitorController.RetryDelay(n).TotalSeconds).ToArray());
    }


    [TestMethod]
    public void InterleavedRecentImportExpiresOldRecordsAndDoesNotPoisonNewRounds()
    {
        var time = DateTimeOffset.UtcNow;
        var state = new MonitorState(); state.Apply(Snapshot(revision:1));
        var active = Sample(2) with { ReceivedAt = time, Agent = "pi" };
        state.Apply(Change(2, active), time);
        var old = Sample(0) with { Id = 1, ReceivedAt = time.AddMinutes(-20), SessionId = "old" };
        state.ImportRecent(new(3,"a",2,[old]));
        Assert.IsFalse(state.View(time).Sessions.Any(s => s.NativeSessionId == "old"));
        state.Apply(Change(3, active with { Id = 3, OperationId = "next" }), time.AddSeconds(1));
        state.Apply(Change(4, active with { Id = 4, OperationId = "next", Kind = "message.observed", Data = new() { Role = "assistant", StopReason = "stop" } }), time.AddSeconds(2));
        Assert.AreEqual("idle", state.View(time.AddSeconds(5)).Sessions.Single().State);
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
        // Windows can briefly retain file handles after the fixture process
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
