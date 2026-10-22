using AgentStatus.TestWindow;
using AgentStatus.TestWindow.Core;

namespace Tests;

[TestClass]
public sealed class ActivityTests
{
    private static readonly DateTimeOffset Now = new(2026, 10, 22, 15, 0, 0, TimeSpan.Zero);
    private static Session Sample(string agent, string state, int seconds) => new()
    { Agent = agent, State = state, Freshness = "fresh", SourceHealth = "healthy", UpdatedAt = Now.AddSeconds(-seconds) };

    [TestMethod]
    public void LatestActiveAgentWinsOverNewerIdleAndTimerAdvancesWithoutEvents()
    {
        Session[] sessions = [Sample("claude", "working", 40), Sample("codex", "working", 26), Sample("pi", "idle", 1)];
        Assert.AreEqual("Codex：工作中...26s", Display.Activity(sessions, Now));
        Assert.AreEqual("Codex：工作中...27s", Display.Activity(sessions, Now.AddSeconds(1)));
        Assert.AreEqual("无活动：空闲...1m10s", Display.Activity([Sample("codex", "idle", 70)], Now));
        Assert.AreEqual("Pi：工具活动...5s", Display.Activity([Sample("pi", "tool_activity", 5)], Now));
    }

    [TestMethod]
    public void StaleUnavailableAndUnknownEvidenceNeverClaimsActiveOrCompleted()
    {
        var stale = Sample("codex", "working", 200) with { Freshness = "stale" };
        Assert.AreEqual("无活动：未知（证据过期）...3m20s", Display.Activity([stale], Now));
        Assert.AreEqual("无活动：未知...未提供", Display.Activity([], Now));
        Assert.AreEqual("无活动：未知...未提供", Display.Activity([stale with { SourceHealth = "unavailable" }], Now));
        Assert.AreEqual("DeepSeek：等待确认（推断）...1m0s", Display.Activity([Sample("deepseek", "waiting_confirmation", 60)], Now));
        Assert.AreEqual("无活动：未知...0s", Display.Activity([Sample("pi", "unknown", -10)], Now));
        Assert.AreEqual("无活动：空闲...1h1m1s", Display.Activity([Sample("codex", "idle", 3661)], Now));
    }
}
