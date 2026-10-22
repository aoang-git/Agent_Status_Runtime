using AgentStatus.TestWindow.Core;

namespace AgentStatus.TestWindow;

internal static class Program
{
    [STAThread]
    private static void Main(string[] args)
    {
        ApplicationConfiguration.Initialize();
        string? directory = null;
        if (args.Length != 0)
        {
            if (args.Length != 2 || args[0] != "--data-dir" || string.IsNullOrWhiteSpace(args[1]))
            {
                MessageBox.Show("用法：agent-status-test.exe [--data-dir 数据目录]", "启动参数", MessageBoxButtons.OK, MessageBoxIcon.Information);
                return;
            }
            directory = args[1];
        }
        try { Application.Run(new MainForm(RuntimeConnection.DiscoverDirectory(directory))); }
        catch (Exception error) when (error is ArgumentException or NotSupportedException or PathTooLongException)
        {
            MessageBox.Show("无法使用此数据目录，请检查路径。", "启动失败", MessageBoxButtons.OK, MessageBoxIcon.Error);
        }
    }
}
