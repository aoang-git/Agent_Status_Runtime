using System.Diagnostics;
using System.Text.Json;
using AgentStatus.TestWindow.Core;

namespace Tests;

[TestClass]
public sealed class RuntimeIntegrationTests
{
    [TestMethod]
    public async Task FileAppendFlowsThroughRealRuntimeIntoClientState()
    {
        var repo = FindRepository();
        var executable = Environment.GetEnvironmentVariable("AGENT_STATUS_TEST_RUNTIME") ?? Path.Combine(repo,"bin","agent-statusd.exe");
        if (!File.Exists(executable)) Assert.Inconclusive("先构建 Runtime，才能运行文件到客户端的集成测试。");
        using var root = new TempDirectory();
        var logs = Path.Combine(root.Path,"sessions"); Directory.CreateDirectory(logs);
        var directory = Path.Combine(root.Path,"runtime");
        var log = Path.Combine(logs,"rollout.jsonl");
        await File.WriteAllTextAsync(log,"{\"type\":\"session_meta\",\"payload\":{\"id\":\"window-test\",\"source\":\"cli\"}}\n" + Record("task_started"));
        var start = new ProcessStartInfo(executable) { UseShellExecute=false,CreateNoWindow=true,RedirectStandardError=true };
        foreach(var argument in new[]{"serve","--data-dir",directory,"--listen","127.0.0.1:0","--root","codex="+logs,"--interval","250ms"}) start.ArgumentList.Add(argument);
        using var process = Process.Start(start) ?? throw new AssertFailedException("临时 Runtime 未启动。");
        using var stop = new CancellationTokenSource(TimeSpan.FromSeconds(15));
        Task? worker=null; MonitorController? controller=null;
        try
        {
            await Until(()=>File.Exists(Path.Combine(directory,"private","connection.json")),stop.Token);
            controller=new(new());
            worker=controller.RunAsync(directory,stop.Token);
            await Until(()=>controller.State.View().Cursor is not null,stop.Token);
            Assert.IsNull(controller.State.View().Activity);
            await File.AppendAllTextAsync(log,Record("task_started"),stop.Token);
            await Until(()=>controller.State.View().Activity?.State=="working",stop.Token);
            Assert.AreEqual("working",controller.State.View().Sessions[0].State);
            Assert.HasCount(0,controller.State.View().Events.Where(e=>e.IsNotification).ToArray());
            await File.AppendAllTextAsync(log,Record("task_complete"),stop.Token);
            await Until(()=>controller.State.View().Sessions[0].Outcome=="completed" && controller.State.View().Events.Any(e=>e.IsNotification),stop.Token);
            Assert.HasCount(1,controller.State.View().Events.Where(e=>e.IsNotification).ToArray());
            Assert.AreEqual("idle",controller.State.View().Sessions[0].State);

            var report=JsonSerializer.Serialize(new {schema=3,producer_id="integration-test",event_id="stable-end",agent="custom-agent",session_id="external-session",operation_id="run-1",state="completed",reported_basis="native_event"});
            async Task SendReport()
            {
                var sender=new ProcessStartInfo(executable){UseShellExecute=false,CreateNoWindow=true,RedirectStandardInput=true,RedirectStandardOutput=true,RedirectStandardError=true};
                foreach(var argument in new[]{"report","--data-dir",directory})sender.ArgumentList.Add(argument);
                using var child=Process.Start(sender)!;
                await child.StandardInput.WriteAsync(report);child.StandardInput.Close();
                var output=await child.StandardOutput.ReadToEndAsync(stop.Token);
                var error=await child.StandardError.ReadToEndAsync(stop.Token);
                await child.WaitForExitAsync(stop.Token);
                Assert.AreEqual(0,child.ExitCode,error);
                using var ack=JsonDocument.Parse(output);
                Assert.IsGreaterThan(0L,ack.RootElement.GetProperty("id").GetInt64());
            }
            await SendReport();
            await Until(()=>controller.State.View().Sessions.Any(s=>s.Agent=="custom-agent" && s.Outcome=="completed") && controller.State.View().Events.Count(e=>e.IsNotification)==2,stop.Token);
            var external=controller.State.View().Sessions.Single(s=>s.Agent=="custom-agent");
            Assert.AreEqual("external",external.Origin);Assert.AreEqual("external",external.Confidence);
            Assert.AreEqual("native_event",external.ReportedBasis);
            await SendReport();
            Assert.HasCount(2,controller.State.View().Events.Where(e=>e.IsNotification).ToArray());
            using var reader=new RuntimeClient();
            var connection=await RuntimeConnection.LoadAsync(directory,stop.Token);
            var snapshot=await reader.GetAsync<RecentFacts>(connection,"/v3/recent",stop.Token);
            Assert.IsTrue(snapshot.Facts.Any(f=>f.Agent=="custom-agent" && f.EventId=="stable-end"),"外部状态应进入当前运行的近期诊断队列。");
        }
        finally
        {
            stop.Cancel();
            if(worker is not null)await worker;
            controller?.Dispose();
            if(!process.HasExited){process.Kill();await process.WaitForExitAsync();}
        }
    }


    [TestMethod]
    public async Task DefaultPortConflictFallsBackButExplicitPortFails()
    {
        var repo = FindRepository(); var executable = Path.Combine(repo, "bin", "agent-statusd.exe");
        using var root = new TempDirectory();
        System.Net.Sockets.TcpListener? occupied = new(System.Net.IPAddress.Loopback, 9120);
        try { occupied.Start(); } catch (System.Net.Sockets.SocketException) { occupied.Stop(); occupied = null; }
        using var stop = new CancellationTokenSource(TimeSpan.FromSeconds(15));
        var logs = Path.Combine(root.Path, "logs"); Directory.CreateDirectory(logs);
        Process Launch(string directory, bool explicitPort)
        {
            var start = new ProcessStartInfo(executable) { UseShellExecute = false, CreateNoWindow = true, RedirectStandardError = true };
            foreach (var arg in new[] { "serve", "--data-dir", directory, "--root", "codex=" + logs }) start.ArgumentList.Add(arg);
            if (explicitPort) { start.ArgumentList.Add("--listen"); start.ArgumentList.Add("127.0.0.1:9120"); }
            return Process.Start(start)!;
        }
        var directory = Path.Combine(root.Path, "fallback");
        using var process = Launch(directory, false);
        try
        {
            await Until(() => File.Exists(Path.Combine(directory, "private", "connection.json")), stop.Token);
            var connection = await RuntimeConnection.LoadAsync(directory, stop.Token);
            Assert.AreNotEqual(9120, connection.Endpoint.Port);
            using var reader = new RuntimeClient();
            Assert.AreEqual(3, (await reader.GetAsync<RuntimeInfo>(connection, "/v3/info", stop.Token)).Schema);
            using var explicitProcess = Launch(Path.Combine(root.Path, "explicit"), true);
            await explicitProcess.WaitForExitAsync(stop.Token);
            Assert.AreNotEqual(0, explicitProcess.ExitCode);
        }
        finally { if (!process.HasExited) { process.Kill(); await process.WaitForExitAsync(); } occupied?.Stop(); }
    }

    private static string Record(string type)=>JsonSerializer.Serialize(new{type="event_msg",timestamp=DateTimeOffset.UtcNow.ToString("O"),payload=new{type,turn_id="turn-test"}})+"\n";
    private static async Task Until(Func<bool> condition,CancellationToken token)
    {while(!condition()) await Task.Delay(25,token);}
    private static string FindRepository()
    {
        for(var parent=new DirectoryInfo(AppContext.BaseDirectory);parent is not null;parent=parent.Parent)
            if(File.Exists(Path.Combine(parent.FullName,"runtime","go.mod")))return parent.FullName;
        throw new AssertFailedException("找不到项目根目录。");
    }
}
