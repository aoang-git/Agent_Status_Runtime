using System.Runtime.CompilerServices;
using System.Text;

namespace AgentStatus.TestWindow.Core;

public static class SseReader
{
    public static async IAsyncEnumerable<SseMessage> ReadAsync(Stream stream, Action onReceive,
        [EnumeratorCancellation] CancellationToken cancellationToken, TimeSpan? idleTimeout = null)
    {
        using var reader = new StreamReader(stream, Encoding.UTF8, true, 4096, leaveOpen: true);
        using var idle = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
        var timeout = idleTimeout ?? TimeSpan.FromSeconds(45);
        string id = "", type = "message";
        var data = new StringBuilder();
        int frameSize = 0;
        var lineBuffer = new StringBuilder();
        char[] buffer = new char[4096];
        bool skipLf = false;
        while (true)
        {
            idle.CancelAfter(timeout);
            int count;
            try { count = await reader.ReadAsync(buffer.AsMemory(), idle.Token); }
            catch (OperationCanceledException) when (!cancellationToken.IsCancellationRequested)
            { throw new TimeoutException("45 秒未收到事件或心跳。"); }
            if (count == 0) yield break;
            onReceive();
            for (int index = 0; index < count; index++)
            {
                char character = buffer[index];
                if (skipLf && character == '\n') { skipLf = false; continue; }
                skipLf = character == '\r';
                if (++frameSize > 1024 * 1024) throw new InvalidDataException("事件帧超过客户端读取上限。");
                if (character is not ('\r' or '\n')) { lineBuffer.Append(character); continue; }
                string line = lineBuffer.ToString(); lineBuffer.Clear();
                if (line.Length == 0)
                {
                    if (data.Length > 0) yield return new(id, type, data.ToString(0, data.Length - 1));
                    id = ""; type = "message"; data.Clear(); frameSize = 0;
                    continue;
                }
                if (line.StartsWith(':')) continue;
                int colon = line.IndexOf(':');
                string field = colon < 0 ? line : line[..colon];
                string value = colon < 0 ? "" : line[(colon + 1)..];
                if (value.StartsWith(' ')) value = value[1..];
                switch (field)
                {
                    case "id": id = value; break;
                    case "event": type = value; break;
                    case "data": data.Append(value).Append('\n'); break;
                }
            }
        }
    }
}
