using System.Net;
using System.Text.Json;

namespace AgentStatus.TestWindow.Core;

// The credential is deliberately excluded from ToString and every UI model.
public sealed class RuntimeConnection(Uri endpoint, string token, string epoch = "", int pid = 0)
{
    public Uri Endpoint { get; } = endpoint;
    public string Epoch { get; } = epoch;
    public int Pid { get; } = pid;
    internal string Token { get; } = token;
    public override string ToString() => Endpoint.ToString();

    public static string DiscoverDirectory(string? explicitDirectory, string? executableDirectory = null, string? configDirectory = null)
    {
        if (!string.IsNullOrWhiteSpace(explicitDirectory)) return Path.GetFullPath(explicitDirectory);
        var installed = Path.Combine(configDirectory ?? Environment.GetFolderPath(Environment.SpecialFolder.ApplicationData), "AgentStatusRuntime");
        if (File.Exists(Path.Combine(installed, "private", "connection.json"))) return installed;
        var project = Path.GetFullPath(Path.Combine(executableDirectory ?? AppContext.BaseDirectory, "..", ".runtime", "live"));
        try
        {
            var path = Path.Combine(project, "private", "connection.json");
            if (File.Exists(path) && new FileInfo(path).Length <= 16 * 1024)
            {
                Parse(File.ReadAllText(path));
                return project;
            }
        }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException or InvalidDataException) { }
        return installed;
    }

    public static async Task<RuntimeConnection> LoadAsync(string directory, CancellationToken cancellationToken)
    {
        var path = Path.Combine(directory, "private", "connection.json");
        if (!File.Exists(path)) throw new FileNotFoundException("未找到连接文件。请先在独立终端启动 Runtime，或选择正确的数据目录。");
        var info = new FileInfo(path);
        if (info.Length > 16 * 1024) throw new InvalidDataException("连接文件过大。");
        return Parse(await File.ReadAllTextAsync(path, cancellationToken));
    }

    private static RuntimeConnection Parse(string json)
    {
        try
        {
            using var document = JsonDocument.Parse(json);
            var root = document.RootElement;
            if (root.ValueKind != JsonValueKind.Object) throw new InvalidDataException("连接文件字段类型不正确。");
            if (!root.TryGetProperty("epoch", out var epochField) || !root.TryGetProperty("pid", out var pidField))
                throw new InvalidDataException("连接文件缺少新版服务实例标识；请重新选择已安装服务的数据目录，或用新版 Runtime 启动此目录。");
            var url = root.GetProperty("url").GetString();
            var token = root.GetProperty("token").GetString();
            var epoch = epochField.GetString();
            var pid = pidField.GetInt32();
            if (string.IsNullOrWhiteSpace(epoch) || epoch.Length > 256 || pid <= 0) throw new InvalidDataException("连接文件缺少服务实例标识。");
            if (!Uri.TryCreate(url, UriKind.Absolute, out var endpoint) || endpoint.Scheme != "http" ||
                !IPAddress.TryParse(endpoint.DnsSafeHost, out var ip) || !IPAddress.IsLoopback(ip) ||
                endpoint.UserInfo != "" || endpoint.AbsolutePath != "/" || endpoint.Query != "" || endpoint.Fragment != "")
                throw new InvalidDataException("连接地址必须是本机 HTTP loopback 地址。");
            if (string.IsNullOrWhiteSpace(token) || token.Length > 4096 || token.Any(char.IsControl))
                throw new InvalidDataException("连接文件缺少有效凭据。");
            return new(endpoint, token, epoch, pid);
        }
        catch (JsonException)
        {
            throw new InvalidDataException("连接文件不是有效 JSON，可能已损坏。");
        }
        catch (Exception error) when (error is KeyNotFoundException or InvalidOperationException or FormatException)
        {
            throw new InvalidDataException("连接文件字段缺失或类型不正确；请选择新版 Runtime 的数据目录。");
        }
    }
}
