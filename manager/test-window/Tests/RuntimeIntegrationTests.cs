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
            await Until(()=>controller.State.View().Sessions.Length==1,stop.Token);
            Assert.AreEqual("working",controller.State.View().Sessions[0].State);
            Assert.HasCount(0,controller.State.View().Events.Where(e=>e.IsNotification).ToArray());
            await File.AppendAllTextAsync(log,Record("task_complete"),stop.Token);
            await Until(()=>controller.State.View().Sessions[0].Outcome=="completed" && controller.State.View().Events.Any(e=>e.IsNotification),stop.Token);
            Assert.HasCount(1,controller.State.View().Events.Where(e=>e.IsNotification).ToArray());
            Assert.AreEqual("idle",controller.State.View().Sessions[0].State);

            var report=JsonSerializer.Serialize(new {schema=2,producer_id="integration-test",event_id="stable-end",agent="custom-agent",session_id="external-session",operation_id="run-1",kind="state.reported",data=new {state="completed"}});
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
            Assert.AreEqual("self_report",external.ReportedBasis);
            await SendReport();
            Assert.HasCount(2,controller.State.View().Events.Where(e=>e.IsNotification).ToArray());
            using var reader=new RuntimeClient();
            var connection=await RuntimeConnection.LoadAsync(directory,stop.Token);
            var snapshot=await reader.GetAsync<RuntimeSnapshot>(connection,"/v2/snapshot",stop.Token);
            Assert.IsTrue(snapshot.Sessions.Any(s=>s.Agent=="custom-agent" && s.Facts.Any(f=>f.EventId=="stable-end")),"外部事实必须写入持久化快照。");
        }
        finally
        {
            stop.Cancel();
            if(worker is not null)await worker;
            controller?.Dispose();
            if(!process.HasExited){process.Kill();await process.WaitForExitAsync();}
        }
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
