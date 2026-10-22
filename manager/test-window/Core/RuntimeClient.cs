using System.Net;
using System.Net.Http.Headers;
using System.Text;

namespace AgentStatus.TestWindow.Core;

public sealed class RuntimeClient : IDisposable
{
    private readonly HttpClient http;

    public RuntimeClient(HttpMessageHandler? handler = null)
    {
        http = new HttpClient(handler ?? new SocketsHttpHandler
        {
            UseProxy = false, AllowAutoRedirect = false, ConnectTimeout = TimeSpan.FromSeconds(5)
        }) { Timeout = Timeout.InfiniteTimeSpan };
    }

    private static HttpRequestMessage Request(RuntimeConnection connection, string route)
    {
        var request = new HttpRequestMessage(HttpMethod.Get, new Uri(connection.Endpoint, route));
        request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", connection.Token);
        return request;
    }

    private static void EnsureSuccess(HttpResponseMessage response)
    {
        if (response.StatusCode == HttpStatusCode.Unauthorized)
            throw new HttpRequestException("服务拒绝凭据（HTTP 401），将重新读取连接文件。", null, response.StatusCode);
        if (!response.IsSuccessStatusCode)
            throw new HttpRequestException($"服务返回 HTTP {(int)response.StatusCode}。", null, response.StatusCode);
    }

    public async Task<T> GetAsync<T>(RuntimeConnection connection, string route, CancellationToken cancellationToken)
    {
        using var timeout = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
        timeout.CancelAfter(TimeSpan.FromSeconds(5));
        using var request = Request(connection, route);
        using var response = await http.SendAsync(request, HttpCompletionOption.ResponseHeadersRead, timeout.Token);
        EnsureSuccess(response);
        return Wire.Read<T>(await LimitedBody(response, timeout.Token));
    }

    private static async Task<string> LimitedBody(HttpResponseMessage response, CancellationToken token)
    {
        await using var stream = await response.Content.ReadAsStreamAsync(token);
        using var body = new MemoryStream();
        byte[] buffer = new byte[8192];
        int read;
        while ((read = await stream.ReadAsync(buffer, token)) > 0)
        {
            if (body.Length + read > 8 * 1024 * 1024) throw new InvalidDataException("服务响应超过客户端读取上限。");
            body.Write(buffer, 0, read);
        }
        return Encoding.UTF8.GetString(body.ToArray());
    }

    public async Task SubscribeAsync(RuntimeConnection connection, Cursor? cursor, Func<SseMessage, Task> onMessage,
        Action onReceive, CancellationToken cancellationToken)
    {
        using var request = Request(connection, "/v2/events");
        if (cursor is not null) request.Headers.Add("Last-Event-ID", cursor.ToString());
        using var headersTimeout = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
        // When resuming at the latest cursor the Runtime flushes its first
        // response on the 15-second heartbeat, not immediately.
        headersTimeout.CancelAfter(TimeSpan.FromSeconds(25));
        using var response = await http.SendAsync(request, HttpCompletionOption.ResponseHeadersRead, headersTimeout.Token);
        EnsureSuccess(response);
        if (response.Content.Headers.ContentType?.MediaType != "text/event-stream")
            throw new InvalidDataException("服务未返回 SSE 事件流。");
        await using var stream = await response.Content.ReadAsStreamAsync(cancellationToken);
        await foreach (var message in SseReader.ReadAsync(stream, onReceive, cancellationToken))
            await onMessage(message);
        throw new IOException("事件连接已关闭。");
    }

    public void Dispose() => http.Dispose();
}
