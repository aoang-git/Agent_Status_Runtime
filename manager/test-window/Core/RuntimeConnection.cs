using System.Net;
using System.Text.Json;

namespace AgentStatus.TestWindow.Core;

// The credential is deliberately excluded from ToString and every UI model.
public sealed class RuntimeConnection(Uri endpoint, string token)
{
    public Uri Endpoint { get; } = endpoint;
    internal string Token { get; } = token;
    public override string ToString() => Endpoint.ToString();

    public static string DiscoverDirectory(string? explicitDirectory, string? executableDirectory = null, string? configDirectory = null)
    {
        if (!string.IsNullOrWhiteSpace(explicitDirectory)) return Path.GetFullPath(explicitDirectory);
        var project = Path.GetFullPath(Path.Combine(executableDirectory ?? AppContext.BaseDirectory, "..", ".runtime", "live"));
        if (File.Exists(Path.Combine(project, "private", "connection.json"))) return project;
        return Path.Combine(configDirectory ?? Environment.GetFolderPath(Environment.SpecialFolder.ApplicationData), "AgentStatusRuntime");
    }

    public static async Task<RuntimeConnection> LoadAsync(string directory, CancellationToken cancellationToken)
    {
        var path = Path.Combine(directory, "private", "connection.json");
        if (!File.Exists(path)) throw new FileNotFoundException("未找到连接文件。请先在独立终端启动 Runtime，或选择正确的数据目录。");
        var info = new FileInfo(path);
        if (info.Length > 16 * 1024) throw new InvalidDataException("连接文件过大。");
        try
        {
            using var document = JsonDocument.Parse(await File.ReadAllTextAsync(path, cancellationToken));
            var root = document.RootElement;
            var url = root.GetProperty("url").GetString();
            var token = root.GetProperty("token").GetString();
            if (!Uri.TryCreate(url, UriKind.Absolute, out var endpoint) || endpoint.Scheme != "http" ||
                !IPAddress.TryParse(endpoint.DnsSafeHost, out var ip) || !IPAddress.IsLoopback(ip) ||
                endpoint.UserInfo != "" || endpoint.AbsolutePath != "/" || endpoint.Query != "" || endpoint.Fragment != "")
                throw new InvalidDataException("连接地址必须是本机 HTTP loopback 地址。");
            if (string.IsNullOrWhiteSpace(token) || token.Length > 4096 || token.Any(char.IsControl))
                throw new InvalidDataException("连接文件缺少有效凭据。");
            return new(endpoint, token);
        }
        catch (Exception error) when (error is JsonException or KeyNotFoundException or InvalidOperationException)
        {
            throw new InvalidDataException("连接文件格式损坏。");
        }
    }
}
