using AgentStatus.TestWindow.Core;

namespace AgentStatus.TestWindow;

public sealed class MainForm : Form
{
    private readonly TextBox directoryBox = new() { ReadOnly = true, Dock = DockStyle.Fill };
    private readonly Label endpointLabel = new() { AutoSize = true };
    private readonly Label statusLabel = new() { Dock = DockStyle.Fill, TextAlign = ContentAlignment.MiddleLeft, Padding = new(12, 5, 12, 5) };
    private readonly Label lastReceivedLabel = new() { AutoSize = true };
    private readonly Label countLabel = new() { AutoSize = true, Padding = new(12, 6, 0, 0) };
    private readonly Label watcherLabel = new() { Dock = DockStyle.Fill, TextAlign = ContentAlignment.MiddleLeft };
    private readonly ComboBox agentFilter = new() { DropDownStyle = ComboBoxStyle.DropDownList, Width = 140 };
    private readonly ComboBox stateFilter = new() { DropDownStyle = ComboBoxStyle.DropDownList, Width = 170 };
    private readonly CheckBox pauseScroll = new() { Text = "暂停自动滚动", AutoSize = true, Padding = new(10, 5, 0, 0) };
    private readonly TextBox detailBox = new() { Multiline = true, ReadOnly = true, Dock = DockStyle.Fill, ScrollBars = ScrollBars.Both, WordWrap = false, BackColor = Color.White };
    private readonly DataGridView sessionsGrid = Grid();
    private readonly DataGridView eventsGrid = Grid();
    private readonly DataGridView rootsGrid = Grid();
    private readonly DataGridView sourcesGrid = Grid();
    private readonly DataGridView capabilitiesGrid = Grid();
    private readonly System.Windows.Forms.Timer renderTimer = new() { Interval = 250 };
    private MonitorController controller;
    private CancellationTokenSource? lifetime;
    private Task? worker;
    private int dirty = 1;
    private bool updatingRows;
    private bool closing;
    private string? lastCursor;
    private int lastEventCount = -1;
    private long lastViewVersion = -1;
    private long lastRenderSecond = -1;
    private Health? lastHealth;
    private string directory;

    public MainForm(string dataDirectory)
    {
        SuspendLayout();
        AutoScaleDimensions = new SizeF(96F, 96F);
        AutoScaleMode = AutoScaleMode.Dpi;
        directory = dataDirectory;
        controller = new(new MonitorState());
        Text = "Agent Status · 测试窗口";
        Size = new(1200, 800);
        MinimumSize = new(1000, 650);
        StartPosition = FormStartPosition.CenterScreen;
        Font = new("Microsoft YaHei UI", 9F);
        BackColor = Color.FromArgb(245, 247, 250);
        var layout = new TableLayoutPanel { Dock = DockStyle.Fill, ColumnCount = 1, RowCount = 5, Padding = new(16), BackColor = BackColor };
        layout.ColumnStyles.Add(new(SizeType.Percent, 100));
        layout.RowStyles.Add(new(SizeType.Absolute, 68));
        layout.RowStyles.Add(new(SizeType.Absolute, 84));
        layout.RowStyles.Add(new(SizeType.Absolute, 48));
        layout.RowStyles.Add(new(SizeType.Percent, 100));
        layout.RowStyles.Add(new(SizeType.Absolute, 28));
        Controls.Add(layout);
        var heading = new TableLayoutPanel { Dock = DockStyle.Fill, RowCount = 2, ColumnCount = 1, Margin = Padding.Empty };
        heading.Controls.Add(new Label { Text = "Agent Status Runtime", Font = new(Font.FontFamily, 18F, FontStyle.Bold), AutoSize = true, ForeColor = Color.FromArgb(24, 39, 64) });
        heading.Controls.Add(new Label { Text = "真实会话与采集诊断  /  Windows 测试客户端", AutoSize = true, ForeColor = Color.FromArgb(92, 104, 122) });
        layout.Controls.Add(heading, 0, 0);
        layout.Controls.Add(ConnectionBar(), 0, 1);
        layout.Controls.Add(statusLabel, 0, 2);
        var tabs = new TabControl { Dock = DockStyle.Fill, Padding = new(18, 7) };
        tabs.TabPages.Add(SessionsPage());
        tabs.TabPages.Add(EventsPage());
        tabs.TabPages.Add(DiagnosticsPage());
        layout.Controls.Add(tabs, 0, 3);
        layout.Controls.Add(new Label { AutoSize = true, Padding = new(0, 6, 0, 0), ForeColor = Color.FromArgb(92, 104, 122), Text = "只读客户端 · 不启动或停止 Runtime · 文件证据不足时保留未知 · 时间显示为本机时区" }, 0, 4);
        sessionsGrid.SelectionChanged += (_, _) => UpdateDetail();
        agentFilter.SelectedIndexChanged += (_, _) => RenderSessions(controller.State.View());
        stateFilter.SelectedIndexChanged += (_, _) => RenderSessions(controller.State.View());
        renderTimer.Tick += (_, _) => Render();
        Shown += async (_, _) =>
        {
            var area = Screen.FromControl(this).WorkingArea;
            if (Width > area.Width || Height > area.Height) Bounds = area;
            renderTimer.Start();
            await Reconnect(false);
        };
        FormClosing += (_, _) =>
        {
            closing = true;
            renderTimer.Stop();
            controller.Changed -= MarkDirty;
            lifetime?.Cancel();
        };
        FormClosed += (_, _) =>
        {
            renderTimer.Dispose();
            var active = controller;
            var cancellation = lifetime;
            _ = (worker ?? Task.CompletedTask).ContinueWith(_ => { active.Dispose(); cancellation?.Dispose(); }, TaskScheduler.Default);
        };
        directoryBox.Text = directory;
        ResumeLayout(true);
    }

    private Control ConnectionBar()
    {
        var bar = new TableLayoutPanel { Dock = DockStyle.Fill, ColumnCount = 4, RowCount = 2, Margin = Padding.Empty };
        bar.ColumnStyles.Add(new(SizeType.Absolute, 74));
        bar.ColumnStyles.Add(new(SizeType.Percent, 100));
        bar.ColumnStyles.Add(new(SizeType.Absolute, 112));
        bar.ColumnStyles.Add(new(SizeType.Absolute, 112));
        bar.RowStyles.Add(new(SizeType.Absolute, 36));
        bar.RowStyles.Add(new(SizeType.Absolute, 38));
        bar.Controls.Add(new Label { Text = "数据目录", AutoSize = true, Padding = new(0, 5, 0, 0) }, 0, 0);
        bar.Controls.Add(directoryBox, 1, 0);
        var choose = new Button { Text = "选择目录…", Dock = DockStyle.Fill };
        choose.Click += async (_, _) =>
        {
            using var dialog = new FolderBrowserDialog { Description = "选择 Runtime 数据目录（包含 private 子目录）", UseDescriptionForTitle = true, SelectedPath = directory };
            if (dialog.ShowDialog(this) != DialogResult.OK) return;
            directory = dialog.SelectedPath;
            directoryBox.Text = directory;
            await Reconnect(true);
        };
        var reconnect = new Button { Text = "重新连接", Dock = DockStyle.Fill };
        reconnect.Click += async (_, _) => await Reconnect(false);
        bar.Controls.Add(choose, 2, 0);
        bar.Controls.Add(reconnect, 3, 0);
        var metadata = new FlowLayoutPanel { Dock = DockStyle.Fill, Padding = new(0, 5, 0, 0), WrapContents = false, AutoScroll = true };
        metadata.Controls.Add(endpointLabel);
        metadata.Controls.Add(lastReceivedLabel);
        bar.Controls.Add(metadata, 0, 1);
        bar.SetColumnSpan(metadata, 4);
        return bar;
    }

    private TabPage SessionsPage()
    {
        var page = Page("会话");
        var layout = new TableLayoutPanel { Dock = DockStyle.Fill, RowCount = 2, ColumnCount = 1 };
        layout.RowStyles.Add(new(SizeType.Absolute, 42));
        layout.RowStyles.Add(new(SizeType.Percent, 100));
        var filters = new FlowLayoutPanel { Dock = DockStyle.Fill, WrapContents = false };
        agentFilter.Items.AddRange(["全部 Agent", "codex", "claude", "deepseek", "pi"]);
        stateFilter.Items.AddRange(["全部状态", "工作中", "工具活动", "等待确认（推断）", "空闲", "未知"]);
        agentFilter.SelectedIndex = stateFilter.SelectedIndex = 0;
        filters.Controls.Add(new Label { Text = "Agent", AutoSize = true, Padding = new(0, 6, 0, 0) });
        filters.Controls.Add(agentFilter);
        filters.Controls.Add(new Label { Text = "状态", AutoSize = true, Padding = new(12, 6, 0, 0) });
        filters.Controls.Add(stateFilter);
        filters.Controls.Add(countLabel);
        layout.Controls.Add(filters, 0, 0);
        var split = new SplitContainer { Dock = DockStyle.Fill, Orientation = Orientation.Horizontal, Size = new(1100, 490), SplitterDistance = 280, Panel1MinSize = 150, Panel2MinSize = 100, SplitterWidth = 6 };
        Columns(sessionsGrid, [("Agent", 100), ("入口", 80), ("会话 ID", 190), ("当前状态", 130), ("可信度", 85), ("新鲜度", 75), ("结束结果", 125), ("更新时间", 160), ("来源健康", 85)]);
        split.Panel1.Controls.Add(sessionsGrid);
        split.Panel2.Controls.Add(detailBox);
        layout.Controls.Add(split, 0, 1);
        page.Controls.Add(layout);
        return page;
    }

    private TabPage EventsPage()
    {
        var page = Page("实时事件");
        var layout = new TableLayoutPanel { Dock = DockStyle.Fill, RowCount = 2, ColumnCount = 1 };
        layout.RowStyles.Add(new(SizeType.Absolute, 42));
        layout.RowStyles.Add(new(SizeType.Percent, 100));
        var tools = new FlowLayoutPanel { Dock = DockStyle.Fill, WrapContents = false };
        var clear = new Button { Text = "清空记录", AutoSize = true };
        clear.Click += (_, _) => { controller.State.ClearEvents(); MarkDirty(); };
        tools.Controls.Add(clear);
        tools.Controls.Add(pauseScroll);
        tools.Controls.Add(new Label { Text = "最多保留最近 500 条；时间为客户端接收时间。", AutoSize = true, Padding = new(14, 6, 0, 0) });
        Columns(eventsGrid, [("接收时间", 160), ("事件类型", 160), ("Agent", 100), ("会话 ID", 190), ("当前状态", 130), ("结束结果", 125), ("说明", 300)]);
        layout.Controls.Add(tools, 0, 0);
        layout.Controls.Add(eventsGrid, 0, 1);
        page.Controls.Add(layout);
        return page;
    }

    private TabPage DiagnosticsPage()
    {
        var page = Page("采集诊断");
        var layout = new TableLayoutPanel { Dock = DockStyle.Fill, ColumnCount = 1, RowCount = 4 };
        layout.RowStyles.Add(new(SizeType.Absolute, 32));
        layout.RowStyles.Add(new(SizeType.Percent, 27));
        layout.RowStyles.Add(new(SizeType.Percent, 43));
        layout.RowStyles.Add(new(SizeType.Percent, 30));
        Columns(rootsGrid, [("Agent / 数据根目录", 780), ("可达性", 160)]);
        Columns(sourcesGrid, [("来源 ID", 330), ("Agent", 130), ("健康", 120), ("格式版本", 100), ("降级原因", 400)]);
        Columns(capabilitiesGrid, [("Agent", 100), ("入口", 140), ("格式", 90), ("活动", 110), ("工具", 110), ("人工等待", 160), ("回合结束", 170), ("桌面认证", 120)]);
        layout.Controls.Add(watcherLabel, 0, 0);
        layout.Controls.Add(Group("根目录", rootsGrid), 0, 1);
        layout.Controls.Add(Group("数据来源与降级原因", sourcesGrid), 0, 2);
        layout.Controls.Add(Group("能力边界（文件监测）", capabilitiesGrid), 0, 3);
        page.Controls.Add(layout);
        return page;
    }

    private async Task Reconnect(bool newDirectory)
    {
        if (closing) return;
        // Serialise repeated clicks without blocking the UI thread.
        var oldTask = worker;
        var oldController = controller;
        var oldLifetime = lifetime;
        oldController.Changed -= MarkDirty;
        oldLifetime?.Cancel();
        lifetime = new();
        var mine = lifetime;
        var state = newDirectory ? new MonitorState() : oldController.State;
        controller = new(state);
        var next = controller;
        next.Changed += MarkDirty;
        lastCursor = null; lastEventCount = -1; lastViewVersion = -1; lastHealth = null;
        MarkDirty();
        worker = Task.Run(async () =>
        {
            if (oldTask is not null) await oldTask;
            oldController.Dispose(); oldLifetime?.Dispose();
            if (!mine.IsCancellationRequested) await next.RunAsync(directory, mine.Token);
        });
        await Task.CompletedTask;
    }

    private void MarkDirty() => Interlocked.Exchange(ref dirty, 1);

    private void Render()
    {
        if (closing) return;
        var now = DateTimeOffset.UtcNow;
        if (Interlocked.Exchange(ref dirty, 0) == 0 && now.ToUnixTimeSeconds() == lastRenderSecond) return;
        lastRenderSecond = now.ToUnixTimeSeconds();
        var view = controller.View();
        var data = controller.State.View();
        endpointLabel.Text = $"服务：{Display.Value(view.Connection.Address)}    Runtime：{Display.Value(view.Info?.Version)}    ";
        lastReceivedLabel.Text = $"最后接收：{(view.LastReceivedAt is { } time ? Display.Time(time) : "未提供")}";
        string phase = view.Connection.Phase switch { "connected" => "已连接", "connecting" => "连接中", _ => "未连接" };
        statusLabel.Text = $"{phase} | {(view.Connection.Phase == "connected" ? Display.Activity(data.Sessions, now) : view.Connection.Message)}";
        statusLabel.BackColor = view.Connection.Phase switch { "connected" => Color.FromArgb(225, 243, 233), "connecting" => Color.FromArgb(229, 237, 253), _ => Color.FromArgb(255, 240, 221) };
        statusLabel.ForeColor = view.Connection.Phase == "connected" ? Color.FromArgb(21, 100, 65) : Color.FromArgb(102, 64, 24);
        var cursor = data.Cursor?.ToString();
        if (cursor != lastCursor || data.Events.Length != lastEventCount || data.Version != lastViewVersion)
        {
            RenderSessions(data); RenderEvents(data);
            lastCursor = cursor; lastEventCount = data.Events.Length;
            lastViewVersion = data.Version;
        }
        if (!ReferenceEquals(lastHealth, view.Health))
        {
            RenderDiagnostics(view);
            lastHealth = view.Health;
        }
    }

    private void RenderSessions(StateView view)
    {
        string? selected = (sessionsGrid.CurrentRow?.Tag as Session)?.SessionId;
        int top = Math.Max(0, sessionsGrid.FirstDisplayedScrollingRowIndex);
        updatingRows = true;
        sessionsGrid.Rows.Clear();
        var list = view.Sessions.Where(s => (agentFilter.SelectedIndex <= 0 || s.Agent == agentFilter.SelectedItem?.ToString())
            && (stateFilter.SelectedIndex <= 0 || Display.State(s.State) == stateFilter.SelectedItem?.ToString())).ToArray();
        foreach (var s in list)
        {
            int i = sessionsGrid.Rows.Add(Display.Agent(s.Agent), Display.Surface(s.Surface), s.SessionId, Display.State(s.State),
                Display.Confidence(s.Confidence), Display.Freshness(s.Freshness), Display.Outcome(s.Outcome), Display.Time(s.UpdatedAt), Display.Health(s.SourceHealth));
            var row = sessionsGrid.Rows[i]; row.Tag = s;
            row.Cells[2].ToolTipText = s.SessionId;
            row.Cells[3].Style.ForeColor = row.Cells[3].Style.SelectionForeColor = Display.StateColor(s.State);
            if (s.Outcome == "completed") row.Cells[6].Style.ForeColor = row.Cells[6].Style.SelectionForeColor = Color.FromArgb(21, 100, 65);
        }
        if (sessionsGrid.Rows.Count > 0)
        {
            var restore = sessionsGrid.Rows.Cast<DataGridViewRow>().FirstOrDefault(r => (r.Tag as Session)?.SessionId == selected) ?? sessionsGrid.Rows[0];
            sessionsGrid.CurrentCell = restore.Cells[0];
            sessionsGrid.FirstDisplayedScrollingRowIndex = Math.Min(top, sessionsGrid.Rows.Count - 1);
        }
        updatingRows = false;
        countLabel.Text = $"显示 {list.Length} / 共 {view.Sessions.Length} 个会话";
        UpdateDetail();
    }

    private void UpdateDetail()
    {
        if (updatingRows) return;
        detailBox.Text = sessionsGrid.CurrentRow?.Tag is Session s ? Display.Detail(s) : "选择会话以查看详情。尚未连接时，请先独立启动 Runtime；此窗口不会启动服务。";
    }

    private void RenderEvents(StateView view)
    {
        int top = Math.Max(0, eventsGrid.FirstDisplayedScrollingRowIndex);
        eventsGrid.Rows.Clear();
        foreach (var e in view.Events)
            eventsGrid.Rows.Add(Display.Time(e.ReceivedAt), Display.Event(e.Type), e.Session is { } s ? Display.Agent(s.Agent) : "—",
                e.Session?.SessionId ?? "—", e.Session is { } state ? Display.State(state.State) : "—",
                e.Type == "turn.ended" ? Display.Outcome(e.Session?.Outcome) : "—", e.Summary);
        if (eventsGrid.Rows.Count > 0)
            eventsGrid.FirstDisplayedScrollingRowIndex = pauseScroll.Checked ? Math.Min(top, eventsGrid.Rows.Count - 1) : eventsGrid.Rows.Count - 1;
    }

    private void RenderDiagnostics(ControllerView view)
    {
        rootsGrid.Rows.Clear(); sourcesGrid.Rows.Clear(); capabilitiesGrid.Rows.Clear();
        var health = view.Health;
        watcherLabel.Text = health is null ? "等待采集诊断信息…" : $"文件通知：{Display.Value(health.Watcher)}    来源 {health.Sources.Length} 个 · 降级/不可用 {health.Sources.Count(s => s.Health is "degraded" or "unavailable")} 个";
        if (health is not null)
        {
            foreach (var root in health.Roots) rootsGrid.Rows.Add(root.Key, Display.Health(root.Value));
            foreach (var source in health.Sources)
            {
                int index = sourcesGrid.Rows.Add(source.Id, Display.Agent(source.Agent), Display.Health(source.Health), source.Format?.ToString() ?? "未提供", Display.Value(source.Reason));
                if (source.Health is "degraded" or "unavailable") sourcesGrid.Rows[index].DefaultCellStyle.ForeColor = Color.FromArgb(155, 58, 21);
            }
        }
        foreach (var c in view.Capabilities)
            capabilitiesGrid.Rows.Add(Display.Agent(c.Agent), c.Surfaces is null ? "未提供" : string.Join(" / ", c.Surfaces.Select(Display.Surface)),
                c.Formats is null ? "未提供" : string.Join(", ", c.Formats), Display.Capability(c.Activity), Display.Capability(c.Tools),
                Display.Capability(c.HumanWait), Display.Capability(c.TurnEnd), Display.Capability(c.DesktopCertification ?? ""));
    }

    private static TabPage Page(string title) => new(title) { Padding = new(10), BackColor = Color.White };
    private static GroupBox Group(string title, Control content)
    {
        var box = new GroupBox { Text = title, Dock = DockStyle.Fill, Padding = new(8) };
        box.Controls.Add(content); return box;
    }
    private static DataGridView Grid() => new()
    {
        Dock = DockStyle.Fill, ReadOnly = true, AllowUserToAddRows = false, AllowUserToDeleteRows = false,
        AllowUserToResizeRows = false, RowHeadersVisible = false, MultiSelect = false,
        SelectionMode = DataGridViewSelectionMode.FullRowSelect, BackgroundColor = Color.White,
        BorderStyle = BorderStyle.None, AutoGenerateColumns = false, AutoSizeRowsMode = DataGridViewAutoSizeRowsMode.AllCellsExceptHeaders,
        ColumnHeadersHeightSizeMode = DataGridViewColumnHeadersHeightSizeMode.AutoSize,
        AutoSizeColumnsMode = DataGridViewAutoSizeColumnsMode.Fill,
        ColumnHeadersDefaultCellStyle = new() { WrapMode = DataGridViewTriState.False },
        CellBorderStyle = DataGridViewCellBorderStyle.SingleHorizontal, GridColor = Color.FromArgb(231, 235, 241),
        RowTemplate = { Height = 28 },
        DefaultCellStyle = new() { SelectionBackColor = Color.FromArgb(225, 234, 248), SelectionForeColor = Color.FromArgb(24, 39, 64), Padding = new(4, 4, 4, 4) },
        AlternatingRowsDefaultCellStyle = new() { BackColor = Color.FromArgb(248, 250, 252) }
    };

    private static void Columns(DataGridView grid, (string Title, int Width)[] columns)
    {
        foreach (var column in columns)
            grid.Columns.Add(new DataGridViewTextBoxColumn { HeaderText = column.Title, FillWeight = column.Width, MinimumWidth = 70, SortMode = DataGridViewColumnSortMode.NotSortable });
    }
}
