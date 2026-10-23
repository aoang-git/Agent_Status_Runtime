using System.Text.Json;
using AgentStatus.TestWindow;
using AgentStatus.TestWindow.Core;

namespace Tests;

[TestClass]
public sealed class LatestActivityTests
{
    private static readonly DateTimeOffset Start = new(2026, 10, 23, 0, 0, 0, TimeSpan.Zero);
    private static string Json(object value) => JsonSerializer.Serialize(value, Wire.Json);
    private static Fact F(long id, string kind = "input.observed", FactData? data = null, string agent = "codex", string op = "op") => new()
    {
        Id = id, ProducerId = "test", EventId = id.ToString(), Agent = agent, SessionNamespace = "local",
        SessionId = "session", SourceId = "source", Origin = "file", OperationId = op,
        ReceivedAt = Start, OccurredAt = Start, Kind = kind, Data = data ?? new()
    };
    private static SseMessage Snapshot(long revision = 0, string type = "ready", params Fact[] facts) =>
        new($"a:{revision}", type == "reset" ? "gap" : type, Json(new StreamBoundary(3, "a", revision)));
    private static MonitorState Empty()
    {
        var state = new MonitorState(); state.Apply(Snapshot(), Start); return state;
    }
    private static void Apply(MonitorState state, Fact fact, int seconds) => state.Apply(new($"a:{fact.Id}", "fact", Json(fact)), Start.AddSeconds(seconds));
    private static Session? Current(MonitorState state, int seconds) => state.View(Start.AddSeconds(seconds)).Activity;
    private static Fact End(long id, string outcome = "completed", string agent = "codex") =>
        F(id, "lifecycle.observed", new() { Phase = "ended", Scope = "agent_turn", Outcome = outcome }, agent);

    [TestMethod]
    public void StartupSnapshotAndDiagnosticReconciliationNeverRestoreTopActivity()
    {
        foreach (var fact in new[] { F(1), End(1) })
        {
            var state = new MonitorState(); state.Apply(Snapshot(1, facts: [fact]), Start);
            Assert.IsNull(Current(state, 1));
            Assert.IsFalse(state.View(Start).Events.Any(e => e.IsNotification));
            state.ImportRecent(new(3, "a", 2, [F(2)]));
            Assert.IsNull(Current(state, 2));
        }
    }

    [TestMethod]
    public void WholeRoundClockSurvivesToolsAndMessagesAndEndNoticeExpiresAfterTenSeconds()
    {
        var state = Empty(); Apply(state, F(1), 0);
        Apply(state, F(2, "tool.observed", new() { Phase = "requested", CallId = "tool" }), 20);
        Assert.AreEqual("Codex：工具活动 · 30s", Display.Activity(Current(state, 30), Start.AddSeconds(30)));
        Apply(state, F(3, "tool.observed", new() { Phase = "finished", CallId = "tool" }), 40);
        Apply(state, F(4, "message.observed", new() { Role = "assistant" }), 60);
        Assert.AreEqual(Start, Current(state, 90)!.StartedAt);
        Apply(state, End(5), 100);
        Assert.AreEqual("Codex：任务完成 · 1m40s", Display.Activity(Current(state, 109), Start.AddSeconds(109)));
        Assert.IsNull(Current(state, 110));
        Apply(state, End(6), 120);
        Assert.IsNull(Current(state, 120), "重复结束不能重新显示通知。");
        Assert.HasCount(1, state.View(Start.AddSeconds(120)).Events.Where(e => e.IsNotification).ToArray());
    }

    [TestMethod]
    public void NewestSourceWinsAndNeverRestoresAnOlderActiveSource()
    {
        var state = Empty(); Apply(state, F(1), 0);
        Apply(state, F(2, agent: "pi"), 10);
        Assert.AreEqual("pi", Current(state, 11)!.Agent);
        Assert.AreEqual(Start.AddSeconds(10), Current(state, 11)!.StartedAt);
        Apply(state, End(3, agent: "pi"), 20);
        Assert.IsNull(Current(state, 30));
        Apply(state, F(4, "message.observed", new() { Role = "assistant" }), 40);
        Assert.AreEqual("codex", Current(state, 41)!.Agent);
        Assert.AreEqual(Start.AddSeconds(40), Current(state, 41)!.StartedAt);
        Apply(state, End(5), 50);
        Apply(state, F(6, agent: "pi", op: "new"), 51);
        Assert.AreEqual("pi", Current(state, 51)!.Agent, "新活动立即替换结束通知。");
    }

    [TestMethod]
    public void FiveMinuteTimeoutIsFinalAndLaterActivityStartsANewRound()
    {
        var state = Empty(); Apply(state, F(1), 0);
        Apply(state, F(2, "tool.observed", new() { Phase = "requested", CallId = "tool" }), 30);
        Apply(state, F(3, "source.heartbeat"), 300);
        Assert.IsNull(Current(state, 329)!.EndedAt);
        var view = state.View(Start.AddSeconds(330));
        Assert.AreEqual(Start.AddSeconds(330), view.Activity!.EndedAt);
        Assert.IsNull(view.Activity.Outcome);
        Assert.AreEqual("timeout", view.Events.Single(e => e.IsNotification).NotificationOutcome);
        Assert.IsNull(Current(state, 340));
        Apply(state, F(4, "message.observed", new() { Role = "assistant" }), 400);
        Assert.AreEqual(Start.AddSeconds(400), Current(state, 410)!.StartedAt);
        Apply(state, End(5), 420);
        Assert.HasCount(2, state.View(Start.AddSeconds(420)).Events.Where(e => e.IsNotification).ToArray());
    }

    [TestMethod]
    public void ApprovalUsesThirtyMinutesAndResolutionRestoresFiveMinuteTimeout()
    {
        var state = Empty(); Apply(state, F(1), 0);
        Apply(state, F(2, "approval.observed", new() { RequestId = "r", Phase = "requested" }), 10);
        Assert.AreEqual("waiting_confirmation", Current(state, 1000)!.State);
        Assert.IsNull(Current(state, 1809)!.EndedAt);
        Assert.AreEqual(Start.AddSeconds(1810), Current(state, 1810)!.EndedAt);
        state = Empty(); Apply(state, F(1), 0);
        Apply(state, F(2, "approval.observed", new() { RequestId = "r", Phase = "requested" }), 10);
        Apply(state, F(3, "approval.observed", new() { RequestId = "r", Phase = "resolved" }), 100);
        Assert.IsNull(Current(state, 399)!.EndedAt);
        Assert.IsNotNull(Current(state, 400)!.EndedAt);
    }

    [TestMethod]
    public void NormalMessageEndWaitsThreeSecondsAndNewActivityCancelsTheCandidate()
    {
        var state = Empty(); Apply(state, F(1, agent: "pi"), 0);
        var stop = F(2, "message.observed", new() { Role = "assistant", StopReason = "stop", HasTools = false }, "pi");
        Apply(state, stop, 10);
        Assert.IsNull(Current(state, 12)!.EndedAt);
        var view = state.View(Start.AddSeconds(13));
        Assert.AreEqual(Start.AddSeconds(10), view.Activity!.EndedAt);
        Assert.AreEqual("inferred", view.Events.Single(e => e.IsNotification).NotificationOutcome);
        Assert.IsNull(Current(state, 23));
        Apply(state, F(3, agent: "pi"), 30);
        Assert.AreEqual(Start.AddSeconds(30), Current(state, 31)!.StartedAt);
        Apply(state, stop with { Id = 4 }, 40);
        Apply(state, F(5, "tool.observed", new() { Phase = "requested", CallId = "tool" }, "pi"), 42);
        Assert.AreEqual("tool_activity", Current(state, 43)!.State);
        Assert.IsNull(Current(state, 43)!.EndedAt);
    }

    [TestMethod]
    public void ErrorAndUnknownEndWaitForTimeoutButExplicitInterruptionEndsImmediately()
    {
        var state = Empty(); Apply(state, F(1), 0);
        Apply(state, End(2, "unknown"), 30);
        Assert.AreEqual("unknown", Current(state, 31)!.State);
        Assert.IsNull(Current(state, 299)!.EndedAt);
        Assert.IsNotNull(Current(state, 300)!.EndedAt);
        state = Empty(); Apply(state, F(1), 0); Apply(state, End(2, "interrupted"), 30);
        Assert.AreEqual("interrupted", Current(state, 30)!.Outcome);
        Assert.IsNull(Current(state, 40));
    }

    [TestMethod]
    public void DisconnectResetAndHistoryNeverRepopulateTheTopDisplay()
    {
        var state = Empty(); Apply(state, F(1), 0);
        state.ResetMonitoring(); Assert.IsNull(Current(state, 10));
        state.Apply(Snapshot(1, facts: [F(1)]), Start.AddSeconds(20));
        Assert.IsNull(Current(state, 20));
        Assert.Throws<InvalidDataException>(() => Apply(state, F(2) with { Historical = true }, 30));
        state.Apply(Snapshot(2), Start.AddSeconds(30));
        Assert.IsNull(Current(state, 30));
        Apply(state, F(3), 40); Assert.IsNotNull(Current(state, 40));
        state.Apply(Snapshot(4, "reset", F(4)), Start.AddSeconds(50));
        Assert.IsNull(Current(state, 50));
        Assert.IsFalse(state.View(Start.AddHours(1)).Events.Any(e => e.IsNotification));
    }

    [TestMethod]
    public void ANewFactAfterTheDeadlineStartsANewRoundEvenWithoutAnInterveningRender()
    {
        var state = Empty(); Apply(state, F(1), 0);
        Apply(state, F(2, "message.observed", new() { Role = "assistant" }), 301);
        Assert.AreEqual(Start.AddSeconds(301), Current(state, 302)!.StartedAt);
        Assert.IsNull(Current(state, 302)!.EndedAt);
        Assert.AreEqual("timeout", state.View(Start.AddSeconds(302)).Events.Single(e => e.IsNotification).NotificationOutcome);
        Apply(state, F(3, op: "next"), 310);
        Assert.AreEqual(Start.AddSeconds(310), Current(state, 311)!.StartedAt);
    }

    [TestMethod]
    public void LatestStandaloneEndOrIdleReplacesWorkingAndDiagnosticsCannotRenewANotice()
    {
        var state = Empty(); Apply(state, F(1), 0);
        Apply(state, End(2, agent: "pi"), 10);
        Assert.AreEqual("pi", Current(state, 11)!.Agent);
        Assert.AreEqual("completed", Current(state, 11)!.Outcome);
        Assert.IsNull(Current(state, 11)!.StartedAt, "没有观察到开始，不恢复历史时长。");
        Apply(state, F(3, "source.heartbeat"), 15);
        state.ImportRecent(new(3, "a", 3, []));
        Assert.IsNull(Current(state, 20));
        Apply(state, F(4), 30);
        Apply(state, F(5, "state.reported", new() { State = "idle" }, agent: "pi"), 31);
        Assert.IsNull(Current(state, 31));
    }
    [TestMethod]
    public void PluginStatesAndFileFactsUseStrictLatestOrderWithoutResettingClock()
    {
        var state = Empty(); Apply(state, F(1), 0);
        Apply(state, F(2, "tool.observed", new() { Phase = "requested", CallId = "pending" }), 10);
        Apply(state, F(3, "state.reported", new() { State = "working" }) with { Origin = "external", ReportedBasis = "native_event" }, 20);
        Assert.AreEqual("working", Current(state, 20)!.State);
        Assert.AreEqual(Start, Current(state, 20)!.StartedAt);
        Apply(state, F(4, "state.reported", new() { State = "waiting_confirmation" }) with { Origin = "external" }, 30);
        Apply(state, F(5, "message.observed", new() { Role = "assistant" }), 40);
        Assert.AreEqual("working", Current(state, 40)!.State);
        Apply(state, F(6, "state.reported", new() { State = "completed" }) with { Origin = "external" }, 50);
        Apply(state, F(7, "message.observed", new() { Role = "assistant" }), 51);
        Assert.AreEqual(Start.AddSeconds(51), Current(state, 51)!.StartedAt);
    }

    [TestMethod]
    public void UnknownDoesNotRenewTimeoutAndSequenceGapDisablesEarlyInference()
    {
        var state = Empty(); Apply(state, F(1, agent: "pi"), 0);
        Apply(state, F(2, "state.reported", new() { State = "unknown" }, "pi", op: "") with { Origin = "external" }, 200);
        Assert.IsNotNull(Current(state, 300)!.EndedAt);
        state = Empty(); Apply(state, F(1, agent: "pi") with { SequenceGap = true }, 0);
        Apply(state, F(2, "message.observed", new() { Role = "assistant", StopReason = "stop", HasTools = false }, "pi"), 10);
        Assert.IsNull(Current(state, 14)!.EndedAt);
        state = Empty(); Apply(state, F(1, agent: "pi"), 0);
        Apply(state, F(2, "message.observed", new() { Role = "assistant", StopReason = "stop" }, "pi"), 10);
        Apply(state, F(3, "source.health", new() { Health = "degraded" }, "pi"), 11);
        Assert.IsNull(Current(state, 14)!.EndedAt);
    }
    [TestMethod]
    public void NewEndOutcomeUsesLatestReportWithoutRestartingTaskClock()
    {
        var state = Empty(); Apply(state, F(1), 0); Apply(state, End(2), 10);
        Apply(state, End(3, "interrupted"), 11);
        Assert.AreEqual("interrupted", Current(state, 11)!.Outcome);
        Assert.AreEqual(Start, Current(state, 11)!.StartedAt);
        Assert.AreEqual(Start.AddSeconds(10), Current(state, 11)!.EndedAt);
    }
}
