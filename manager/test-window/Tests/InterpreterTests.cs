using System.Text.Json;
using AgentStatus.TestWindow.Core;

namespace Tests;

[TestClass]
public sealed class InterpreterTests
{
    [TestMethod]
    public void PublishedRuleFixturesMatchTheDefaultInterpreter()
    {
        using var document=JsonDocument.Parse(File.ReadAllText(Path.Combine(AppContext.BaseDirectory,"protocol","interpretation.json")));
        var now=document.RootElement.GetProperty("now").GetDateTimeOffset();
        Assert.AreEqual(DefaultInterpreter.Version,document.RootElement.GetProperty("rule_version").GetString());
        foreach(var row in document.RootElement.GetProperty("cases").EnumerateArray())
        {
            var facts=Wire.Read<Fact[]>(row.GetProperty("facts").GetRawText());
            var actual=DefaultInterpreter.Project([Ledger(facts)],now).Single();
            Assert.AreEqual(row.GetProperty("state").GetString(),actual.State,row.GetProperty("name").GetString());
            Assert.AreEqual(row.GetProperty("confidence").GetString(),actual.Confidence);
            Assert.AreEqual(row.TryGetProperty("outcome",out var outcome)?outcome.GetString():null,actual.Outcome);
        }
    }

    private static readonly DateTimeOffset Now = new(2026,10,23,0,0,0,TimeSpan.Zero);
    private static Fact F(long id,string kind,FactData? data=null,string op="a",string agent="pi",string origin="file") => new()
    {Id=id,ProducerId="test",EventId=id.ToString(),Agent=agent,SessionNamespace="local",SessionId="session",OperationId=op,Kind=kind,Data=data??new(),Origin=origin,SourceId=origin,ReceivedAt=Now,OccurredAt=Now};
    private static EvidenceSession Ledger(params Fact[] facts) => new(facts[0].Agent,"local","session",facts[0].SourceId,facts[0].Origin,facts);
    private static Session Project(params Fact[] facts) => DefaultInterpreter.Project([Ledger(facts)],Now).Single();

    [TestMethod]
    public void ConcurrentToolsAndApprovalsRemainIndependent()
    {
        var facts=new[]{F(1,"input.observed"),F(2,"tool.observed",new(){CallId="one",Phase="requested"}),
            F(3,"tool.observed",new(){CallId="two",Phase="requested"}),F(4,"tool.observed",new(){CallId="one",Phase="finished"}),
            F(5,"message.observed",new(){Role="assistant",StopReason="stop",HasTools=false})};
        var session=Project(facts);Assert.AreEqual("tool_activity",session.State);Assert.IsTrue(session.PendingTools!.ContainsKey("two"));
        session=Project([..facts,F(6,"approval.observed",new(){RequestId="r1",Phase="requested"}),F(7,"approval.observed",new(){RequestId="r2",Phase="requested"}),F(8,"approval.observed",new(){RequestId="r1",Phase="resolved"})]);
        Assert.AreEqual("waiting_confirmation",session.State);Assert.HasCount(1,session.PendingApprovals!);
    }

    [TestMethod]
    public void NormalMessagesInferIdleButTruncationErrorsAndPartialCoverageDoNot()
    {
        foreach(var agent in new[]{"pi","claude"})
        {
            var message=F(2,"message.observed",new(){Role="assistant",StopReason=agent=="pi"?"stop":"end_turn",HasTools=false},agent:agent);
            var input=F(1,"input.observed",agent:agent);
            var session=Project(input,message);Assert.AreEqual("idle",session.State);Assert.AreEqual("inferred",session.Confidence);Assert.IsNull(session.Outcome);
            Assert.AreEqual("unknown",Project(input,message with {Data=message.Data with {StopReason="max_tokens"}}).State);
            Assert.AreEqual("unknown",Project(input,message with {Data=message.Data with {Outcome="failed"}}).State);
            session=DefaultInterpreter.Project([Ledger(input,message) with {Coverage="partial"}],Now).Single();
            Assert.AreNotEqual("idle",session.State);
            session=DefaultInterpreter.Project([Ledger(input,message) with {Coverage="partial",IncompleteOperations=[]}],Now).Single();
            Assert.AreEqual("idle",session.State,"历史任务裁剪不能永久阻止当前完整任务的空闲推断。");
        }
    }

    [TestMethod]
    public void OldEndAndHeartbeatDoNotReplaceTheCurrentOperationOrRenewEvidence()
    {
        var started=F(2,"lifecycle.observed",new(){Phase="started",Scope="agent_turn"},op:"b",agent:"codex");
        var session=Project(F(1,"input.observed",agent:"codex"),started,F(3,"lifecycle.observed",new(){Phase="ended",Scope="agent_turn",Outcome="completed"},agent:"codex"));
        Assert.AreEqual("b",session.TurnId);Assert.AreEqual("working",session.State);Assert.IsNull(session.Outcome);
        var heartbeat=F(4,"source.heartbeat",op:"b",agent:"codex") with {ReceivedAt=Now.AddMinutes(3),OccurredAt=Now.AddMinutes(3)};
        session=DefaultInterpreter.Project([Ledger(started,heartbeat)],Now.AddMinutes(3)).Single();
        Assert.AreEqual("unknown",session.State);Assert.AreEqual("stale",session.Freshness);Assert.AreEqual(Now,session.UpdatedAt);
    }

    [TestMethod]
    public void ExternalClaimsFutureTimesAndSessionNamespacesStayDistinct()
    {
        var external=F(1,"state.reported",new(){State="working"},origin:"external") with {ReportedBasis="native_event",OccurredAt=Now.AddYears(1)};
        var session=DefaultInterpreter.Project([Ledger(external)],Now.AddMinutes(3)).Single();
        Assert.AreEqual("unknown",session.State);Assert.AreEqual("external",session.Origin);
        var end=external with {Id=2,Data=new(){State="completed"}};
        session=Project(external,end);Assert.AreEqual("external",session.Confidence);Assert.AreEqual("completed",session.Outcome);
        Assert.HasCount(2,DefaultInterpreter.Project([Ledger(external),Ledger(external) with {SessionNamespace="other"}],Now));
    }

    [TestMethod]
    public void CompletionOnlySourcesAdvanceAndBranchChangesDoNotResurrectOldWork()
    {
        var first=F(1,"state.reported",new(){State="completed"},origin:"external");
        var second=first with {Id=2,OperationId="b",ReceivedAt=Now.AddSeconds(1)};
        var session=Project(first,second);
        Assert.AreEqual("b",session.TurnId);Assert.AreEqual(Now.AddSeconds(1),session.UpdatedAt);
        session=Project(F(1,"input.observed"),F(2,"tool.observed",new(){CallId="old",Phase="requested"}),
            F(3,"session.observed",new(){BranchChanged=true},op:""),F(4,"message.observed",new(){Role="assistant",StopReason="stop"},op:""));
        Assert.AreEqual("unknown",session.State);Assert.IsNull(session.TurnId);
        session=Project(F(1,"lifecycle.observed",new(){Phase="started",Scope="agent_turn"}),
            F(2,"lifecycle.observed",new(){Phase="ended",Scope="agent_turn",Outcome="unknown"}));
        Assert.AreEqual("unknown",session.State);Assert.IsNull(session.Outcome);
    }

    [TestMethod]
    public void MixedSourcesDeduplicateOnlyExactlyMatchingOperationAndScope()
    {
        var state=new MonitorState();
        string Json(object v)=>JsonSerializer.Serialize(v,Wire.Json);
        state.Apply(new("a:0","snapshot",Json(new RuntimeSnapshot(2,"a",0,[]))));
        var first=F(1,"lifecycle.observed",new(){Phase="ended",Scope="agent_turn",Outcome="completed"},agent:"codex");
        var duplicate=first with {Id=2,Origin="external",SourceId="external"};
        foreach(var f in new[]{first,duplicate})state.Apply(new($"a:{f.Id}","fact",Json(f)));
        Assert.HasCount(1,state.View(Now).Events.Where(e=>e.IsNotification).ToArray());
        Assert.HasCount(1,state.View(Now).Sessions);
        state.ClearEvents();
        var distinct=duplicate with {Id=3,Data=duplicate.Data with {Scope="agent_run"}};
        state.Apply(new("a:3","fact",Json(distinct)));
        Assert.HasCount(1,state.View(Now).Events.Where(e=>e.IsNotification).ToArray());
    }
}
