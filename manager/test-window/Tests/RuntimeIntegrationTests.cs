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
        var executable = Path.Combine(repo,"bin","agent-statusd.exe");
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
            Assert.HasCount(0,controller.State.View().Events.Where(e=>e.Type=="turn.ended").ToArray());
            await File.AppendAllTextAsync(log,Record("task_complete"),stop.Token);
            await Until(()=>controller.State.View().Sessions[0].Outcome=="completed",stop.Token);
            Assert.HasCount(1,controller.State.View().Events.Where(e=>e.Type=="turn.ended").ToArray());
            Assert.AreEqual("idle",controller.State.View().Sessions[0].State);
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
