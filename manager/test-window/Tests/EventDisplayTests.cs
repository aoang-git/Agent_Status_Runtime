using AgentStatus.TestWindow;
using AgentStatus.TestWindow.Core;

namespace Tests;

[TestClass]
public sealed class EventDisplayTests
{
    [TestMethod]
    public void TerminalNotificationIsDistinctFromIdleStateWithTheSameOutcome()
    {
        var session = new Session { State = "idle", Outcome = "completed" };
        var ended = new ClientEvent(DateTimeOffset.UtcNow, "turn.ended", session, "");
        var idle = ended with { Type = "state.changed" };
        Assert.AreEqual("任务结束通知", Display.Event(ended.Type));
        Assert.AreEqual("任务完成", Display.EventState(ended));
        Assert.AreEqual("空闲", Display.EventState(idle));
        StringAssert.Contains(Display.EventDescription(idle), "不是新的完成通知");
        Assert.AreEqual("任务中断", Display.EventState(ended with { Session = session with { Outcome = "interrupted" } }));
        Assert.AreEqual("任务失败", Display.EventState(ended with { Session = session with { Outcome = "failed" } }));
        Assert.AreEqual("结束（结果未知）", Display.EventState(ended with { Session = session with { Outcome = "unknown" } }));
        Assert.AreEqual("—", Display.EventState(ended with { Type = "snapshot", Session = null }));
        Assert.AreEqual("idle", session.State);
    }

    [TestMethod]
    public void PiMessageEndAndStalenessExplainUnknownWithoutInventingCompletion()
    {
        var candidate = new ClientEvent(DateTimeOffset.UtcNow, "state.changed",
            new Session { Agent = "pi", State = "unknown", Reason = "message_end_without_settled_event" }, "");
        Assert.AreEqual("未知", Display.EventState(candidate));
        Assert.AreEqual("消息已结束，尚无整轮稳定结束证据", Display.EventDescription(candidate));
        Assert.AreEqual("活动证据已过期，无法确认当前状态",
            Display.EventDescription(candidate with { Session = candidate.Session! with { Reason = "stale_evidence" } }));
    }
}
