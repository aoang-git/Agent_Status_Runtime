using AgentStatus.TestWindow;
using AgentStatus.TestWindow.Core;

namespace Tests;

[TestClass]
public sealed class EventDisplayTests
{
    [TestMethod]
    public void RawLifecycleAndExternalNotificationHaveDistinctLabels()
    {
        var fact = new Fact { Kind="lifecycle.observed", Origin="external", OperationId="op", ReportedBasis="native_event",
            Data=new(){Phase="ended",Scope="agent_turn",Outcome="completed"} };
        var row = new ClientEvent(DateTimeOffset.UtcNow,"fact",null,"",fact,true,"completed");
        Assert.AreEqual("任务完成（外部报告）",Display.EventState(row));
        Assert.AreEqual("结束 / agent_turn",Display.EventState(row with { IsNotification=false }));
        Assert.AreEqual("外部声明 / native_event",Display.Basis(fact));
        StringAssert.Contains(Display.EventDescription(row),"本次任务结束通知");
        Assert.AreEqual("任务失败",Display.EventState(row with { Fact=fact with {Origin="file"},NotificationOutcome="failed" }));
        Assert.AreEqual("—",Display.EventState(row with { Type="snapshot",Fact=null,IsNotification=false }));
    }

    [TestMethod]
    public void InferredIdleAndExplicitOutcomeRemainDifferent()
    {
        Assert.AreEqual("空闲（推断）",Display.SessionState(new(){State="idle",Confidence="inferred"}));
        Assert.AreEqual("空闲",Display.SessionState(new(){State="idle",Confidence="observed",Outcome="completed"}));
        StringAssert.Contains(Display.SessionOutcome(new(){Outcome="completed",Origin="external"}),"外部报告");
        Assert.AreEqual("任务完成（外部报告）",Display.SessionOutcome(new(){Outcome="completed",Origin="external"}));
        StringAssert.Contains(Display.FactDetail(new(DateTimeOffset.UtcNow,"fact",null,"",new(){ProducerId="source",OperationId="op",NativeType="native"})),"producer_id：source");
    }
}
