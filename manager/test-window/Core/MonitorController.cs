using System.Text.Json;

namespace AgentStatus.TestWindow.Core;

public sealed record ConnectionStatus(string Phase, string Message, string? Address = null);
public sealed record ControllerView(ConnectionStatus Connection, RuntimeInfo? Info, Health? Health,
    Capability[] Capabilities, DateTimeOffset? LastReceivedAt);

public sealed class MonitorController(MonitorState state, RuntimeClient? client = null) : IDisposable
{
    private readonly object gate = new();
    private readonly RuntimeClient client = client ?? new();
    private ConnectionStatus connection = new("disconnected", "尚未连接；窗口不会启动或停止服务。");
    private RuntimeInfo? info;
    private Health? health;
    private Capability[] capabilities = [];
    private DateTimeOffset? lastReceived;

    public MonitorState State { get; } = state;
    public event Action? Changed;

    public ControllerView View()
    {
        lock (gate) return new(connection, info, health, capabilities, lastReceived);
    }

    public static TimeSpan RetryDelay(int failureCount) => TimeSpan.FromSeconds(failureCount switch
    { <= 1 => 1, 2 => 2, 3 => 5, _ => 10 });

    public async Task RunAsync(string directory, CancellationToken cancellationToken)
    {
        int failures = 0;
        while (!cancellationToken.IsCancellationRequested)
        {
            State.ResetMonitoring();
            SetConnection(new("connecting", "正在读取连接信息并检查服务…"));
            using var attempt = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
            Task? stream = null, diagnostics = null;
            try
            {
                var endpoint = await RuntimeConnection.LoadAsync(directory, cancellationToken);
                SetConnection(new("connecting", "正在连接事件流…", endpoint.Endpoint.ToString()));
                var runtimeInfo = await client.GetAsync<RuntimeInfo>(endpoint, "/v2/info", cancellationToken);
                if (runtimeInfo.Schema != 2 || string.IsNullOrWhiteSpace(runtimeInfo.Epoch))
                    throw new InvalidDataException("不兼容的 Runtime 协议版本。");
                lock (gate) info = runtimeInfo;
                await RefreshDiagnostics(endpoint, attempt.Token);
                diagnostics = PollDiagnostics(endpoint, attempt.Token);
                stream = client.SubscribeAsync(endpoint, null, message =>
                {
                    if (State.Apply(message)) Changed?.Invoke();
                    failures = 0;
                    return Task.CompletedTask;
                }, () =>
                {
                    lock (gate)
                    {
                        lastReceived = DateTimeOffset.UtcNow;
                        connection = new("connected", "事实监测 · default-v1 客户端解释", endpoint.Endpoint.ToString());
                    }
                    Changed?.Invoke();
                }, attempt.Token);
                await await Task.WhenAny(stream, diagnostics);
            }
            catch (OperationCanceledException) when (cancellationToken.IsCancellationRequested) { break; }
            catch (Exception error) when (error is IOException or HttpRequestException or JsonException
                or InvalidDataException or UnauthorizedAccessException or OperationCanceledException or TimeoutException)
            {
                failures++;
                var delay = RetryDelay(failures);
                var address = View().Connection.Address;
                SetConnection(new("disconnected", $"{Explain(error)}  {delay.TotalSeconds:0} 秒后重试；不会启动服务。", address));
            }
            finally
            {
                State.ResetMonitoring();
                attempt.Cancel();
                foreach (var task in new[] { stream, diagnostics })
                {
                    if (task is null) continue;
                    try { await task; }
                    catch (Exception error) when (error is IOException or HttpRequestException or JsonException
                        or InvalidDataException or OperationCanceledException or TimeoutException) { }
                }
            }
            try { await Task.Delay(RetryDelay(failures), cancellationToken); }
            catch (OperationCanceledException) when (cancellationToken.IsCancellationRequested) { break; }
        }
    }

    private async Task RefreshDiagnostics(RuntimeConnection endpoint, CancellationToken token)
    {
        var healthTask = client.GetAsync<Health>(endpoint, "/v2/health", token);
        var capabilitiesTask = client.GetAsync<Capability[]>(endpoint, "/v2/capabilities", token);
        var snapshotTask = client.GetAsync<RuntimeSnapshot>(endpoint, "/v2/snapshot", token);
        await Task.WhenAll(healthTask, capabilitiesTask, snapshotTask);
        if (healthTask.Result.Roots is null || healthTask.Result.Sources is null ||
            healthTask.Result.Sources.Any(source => source is null) || capabilitiesTask.Result.Any(capability => capability is null))
            throw new InvalidDataException("采集诊断数据不完整。");
        State.Reconcile(snapshotTask.Result);
        lock (gate) { health = healthTask.Result; capabilities = capabilitiesTask.Result; }
        Changed?.Invoke();
    }

    private async Task PollDiagnostics(RuntimeConnection endpoint, CancellationToken token)
    {
        using var timer = new PeriodicTimer(TimeSpan.FromSeconds(5));
        while (await timer.WaitForNextTickAsync(token)) await RefreshDiagnostics(endpoint, token);
    }

    private void SetConnection(ConnectionStatus status)
    {
        lock (gate) connection = status with { Address = status.Address ?? connection.Address };
        Changed?.Invoke();
    }

    private static string Explain(Exception error) => error switch
    {
        FileNotFoundException => "未找到连接文件；请先独立启动 Runtime 或重新选择数据目录。",
        UnauthorizedAccessException => "当前用户无法读取连接文件。",
        JsonException => "服务数据或连接文件格式损坏。",
        InvalidDataException => error.Message,
        HttpRequestException { StatusCode: System.Net.HttpStatusCode.Unauthorized } => "凭据已失效（HTTP 401），将重新读取连接文件。",
        HttpRequestException => "服务不可达或返回错误；最后快照已保留。",
        TimeoutException or OperationCanceledException => "服务响应或心跳超时；最后快照已保留。",
        _ => "连接已断开或文件暂不可读；最后快照已保留。"
    };

    public void Dispose() => client.Dispose();
}
