using System.Runtime.ExceptionServices;
using System.Reflection;
using System.Text.Json;
using AgentStatus.TestWindow;
using AgentStatus.TestWindow.Core;

namespace Tests;

[TestClass]
[DoNotParallelize]
public sealed class WindowTests
{
    public TestContext TestContext { get; set; } = null!;

    [TestMethod]
    public void OfflineWindowRemainsResponsiveAtMinimumSizeAndScaledLayout()
    {
        using var root=new TempDirectory();
        Exception? failure=null;
        string metrics="";
        var thread=new Thread(()=>
        {
            try
            {
                Application.SetHighDpiMode(HighDpiMode.PerMonitorV2);
                using var form=new MainForm(root.Path);
                using var timer=new System.Windows.Forms.Timer{Interval=100};
                int ticks=0;
                timer.Tick+=(_,_)=>
                {
                    try
                    {
                        ticks++;
                        if(ticks<3)return;
                        metrics=$"initial={form.Size}, dpi={form.DeviceDpi}, baseline={form.AutoScaleDimensions}";
                        Assert.IsGreaterThanOrEqualTo(1000,form.Width);
                        Assert.IsTrue(Descendants(form).OfType<StatusBanner>().Any(l=>l.Text.Contains("不会启动服务")));
                        form.Size=form.MinimumSize;
                        form.PerformLayout();
                        var directory=Descendants(form).OfType<TextBox>().Single(t=>!t.Multiline);
                        Assert.IsGreaterThan(300,directory.Width);
                        form.Scale(new SizeF(1.5f,1.5f));
                        form.PerformLayout();
                        Assert.IsTrue(Descendants(form).OfType<Button>().All(b=>b.Width>60 && b.Height>20));
                        Assert.IsFalse(Directory.Exists(Path.Combine(root.Path,"private")),"窗口不得创建服务或连接文件。");
                    }
                    catch(Exception error){failure=error;}
                    finally{timer.Stop();form.Close();}
                };
                form.Shown+=(_,_)=>timer.Start();
                Application.Run(form);
            }
            catch(Exception error){failure=error;}
        });
        thread.SetApartmentState(ApartmentState.STA);
        thread.Start();
        Assert.IsTrue(thread.Join(TimeSpan.FromSeconds(10)),"窗口未能及时关闭。");
        TestContext.WriteLine(metrics);
        if(failure is not null)ExceptionDispatchInfo.Capture(failure).Throw();
    }

    [TestMethod]
    public void SelectedFactDetailsSurviveSnapshotRefresh()
    {
        using var root=new TempDirectory();
        Exception? failure=null;
        var thread=new Thread(()=>
        {
            try
            {
                using var form=new MainForm(root.Path);
                var controller=(MonitorController)typeof(MainForm).GetField("controller",BindingFlags.Instance|BindingFlags.NonPublic)!.GetValue(form)!;
                var state=controller.State;
                string Json(object value)=>JsonSerializer.Serialize(value,Wire.Json);
                var input=new Fact{Id=1,ProducerId="test",EventId="one",Agent="codex",SessionNamespace="test",SessionId="ui-session",SourceId="test",Origin="file",OperationId="ui-op",Kind="input.observed",ReceivedAt=DateTimeOffset.UtcNow};
                var tool=input with{Id=2,EventId="two",Kind="tool.observed",Data=new(){Phase="requested",CallId="ui-selected-call"}};
                using var timer=new System.Windows.Forms.Timer{Interval=100};
                int ticks=0;
                timer.Tick+=(_,_)=>
                {
                    try
                    {
                        ticks++;
                        if(ticks==3)
                        {
                            state.Apply(new("ui:0","snapshot",Json(new RuntimeSnapshot(2,"ui",0,[]))));
                            state.Apply(new("ui:1","fact",Json(input)));
                            state.Apply(new("ui:2","fact",Json(tool)));
                            Descendants(form).OfType<TabControl>().Single().SelectedIndex=1;
                        }
                        if(ticks==15)
                        {
                            var grid=Descendants(form).OfType<DataGridView>().Single(g=>g.Columns[1].HeaderText=="事实 / 通知");
                            grid.CurrentCell=grid.Rows[2].Cells[0];
                            var detail=Descendants(Descendants(form).OfType<TabControl>().Single().TabPages[1]).OfType<TextBox>().Single();
                            StringAssert.Contains(detail.Text,"ui-selected-call","点击事实后必须显示该行详情。");
                            state.Reconcile(new(2,"ui",2,[new("codex","test","ui-session","test","file",[input,tool])]));
                        }
                        if(ticks==30)
                        {
                            var detail=Descendants(Descendants(form).OfType<TabControl>().Single().TabPages[1]).OfType<TextBox>().Single();
                            StringAssert.Contains(detail.Text,"ui-selected-call","自动快照刷新不得将详情换回初始快照。");
                            timer.Stop();form.Close();
                        }
                    }
                    catch(Exception error){failure=error;timer.Stop();form.Close();}
                };
                form.Shown+=(_,_)=>timer.Start();
                Application.Run(form);
            }
            catch(Exception error){failure=error;}
        });
        thread.SetApartmentState(ApartmentState.STA);thread.Start();
        Assert.IsTrue(thread.Join(TimeSpan.FromSeconds(10)),"窗口测试未结束。");
        if(failure is not null)ExceptionDispatchInfo.Capture(failure).Throw();
    }

    [TestMethod]
    public void StatusTimerUpdatesDoNotTriggerNativeUnbufferedDrawing()
    {
        using var root=new TempDirectory();
        Exception? failure=null;
        var thread=new Thread(()=>
        {
            try
            {
                using var form=new MainForm(root.Path);
                using var timer=new System.Windows.Forms.Timer{Interval=100};
                timer.Tick+=(_,_)=>
                {
                    try
                    {
                        var banner=(Control)typeof(MainForm).GetField("statusLabel",BindingFlags.Instance|BindingFlags.NonPublic)!.GetValue(form)!;
                        banner.BackColor=Color.FromArgb(225,243,233);
                        banner.ForeColor=Color.FromArgb(21,100,65);
                        using var drawing=new NativeDrawingCounter(banner.Parent!);
                        for(int seconds=0;seconds<20;seconds++)banner.Text=$"已连接 | Codex：工作中...{seconds}s";
                        Assert.AreEqual(0,drawing.DrawItems,"计时文本更新不得触发原生 STATIC 的即时擦除/绘制，必须交给缓冲重绘。");
                        form.Update();
                        StringAssert.Contains(banner.Text,"工作中...19s");
                        using var bitmap=new Bitmap(banner.Width,banner.Height);
                        banner.DrawToBitmap(bitmap,banner.ClientRectangle);
                        int background=0, text=0;
                        for(int y=0;y<bitmap.Height;y++)
                            for(int x=0;x<bitmap.Width;x++)
                                if(bitmap.GetPixel(x,y).ToArgb()==banner.BackColor.ToArgb())background++;else text++;
                        Assert.IsGreaterThan(bitmap.Width*bitmap.Height/2,background,"状态背景必须完整绘制。");
                        Assert.IsGreaterThan(10,text,"计时文本必须实际绘制，不能靠停止更新来消除闪烁。");
                    }
                    catch(Exception error){failure=error;}
                    finally{timer.Stop();form.Close();}
                };
                form.Shown+=(_,_)=>timer.Start();
                Application.Run(form);
            }
            catch(Exception error){failure=error;}
        });
        thread.SetApartmentState(ApartmentState.STA);thread.Start();
        Assert.IsTrue(thread.Join(TimeSpan.FromSeconds(10)),"状态栏测试未结束。");
        if(failure is not null)ExceptionDispatchInfo.Capture(failure).Throw();
    }

    [TestMethod]
    public void BannerIgnoresSnapshotResultsAndCoverageAndClearsExpiredLiveNotice()
    {
        using var root = new TempDirectory();
        Exception? failure = null;
        var thread = new Thread(() =>
        {
            try
            {
                using var form = new MainForm(root.Path);
                var flags = BindingFlags.Instance | BindingFlags.NonPublic;
                typeof(MainForm).GetField("closing", flags)!.SetValue(form, true);
                form.Show();
                ((System.Windows.Forms.Timer)typeof(MainForm).GetField("renderTimer", flags)!.GetValue(form)!).Stop();
                Application.DoEvents();
                typeof(MainForm).GetField("closing", flags)!.SetValue(form, false);
                var controller = (MonitorController)typeof(MainForm).GetField("controller", flags)!.GetValue(form)!;
                typeof(MonitorController).GetField("connection", flags)!.SetValue(controller, new ConnectionStatus("connected", "test"));
                string Json(object value) => JsonSerializer.Serialize(value, Wire.Json);
                var now = DateTimeOffset.UtcNow;
                var end = new Fact { Id = 1, Agent = "codex", SessionNamespace = "test", SessionId = "session", SourceId = "test", Origin = "file",
                    OperationId = "op", Kind = "lifecycle.observed", Data = new() { Phase = "ended", Scope = "agent_turn", Outcome = "completed" } };
                controller.State.Apply(new("ui:1", "snapshot", Json(new RuntimeSnapshot(2, "ui", 1,
                    [new("codex", "test", "session", "test", "file", [end], "partial")], "partial"))), now);
                var banner = (Control)typeof(MainForm).GetField("statusLabel", flags)!.GetValue(form)!;
                void Render()
                {
                    typeof(MainForm).GetField("dirty", flags)!.SetValue(form, 1);
                    typeof(MainForm).GetMethod("Render", flags)!.Invoke(form, null);
                }
                Render(); Assert.AreEqual("已连接 | 无活动", banner.Text);
                var input = end with { Id = 2, OperationId = "new", Kind = "input.observed", Data = new() };
                controller.State.Apply(new("ui:2", "fact", Json(input)), now.AddSeconds(-60));
                Render(); StringAssert.Contains(banner.Text, "Codex：工作中 · 1m");
                controller.State.Apply(new("ui:3", "fact", Json(end with { Id = 3, OperationId = "new" })), now.AddSeconds(-11));
                Render(); Assert.AreEqual("已连接 | 无活动", banner.Text);
                Assert.IsFalse(Directory.Exists(Path.Combine(root.Path, "private")));
                form.Close();
            }
            catch (Exception error) { failure = error; }
        });
        thread.SetApartmentState(ApartmentState.STA); thread.Start();
        Assert.IsTrue(thread.Join(TimeSpan.FromSeconds(10)), "状态摘要测试未结束。");
        if (failure is not null) ExceptionDispatchInfo.Capture(failure).Throw();
    }

    private sealed class NativeDrawingCounter : NativeWindow, IDisposable
    {
        public int DrawItems { get; private set; }
        public NativeDrawingCounter(Control parent)=>AssignHandle(parent.Handle);
        protected override void WndProc(ref Message message)
        {
            if(message.Msg==0x002B)DrawItems++; // WM_DRAWITEM: STATIC bypassing WM_PAINT.
            base.WndProc(ref message);
        }
        public void Dispose()=>ReleaseHandle();
    }

    private static IEnumerable<Control> Descendants(Control root)
    {
        foreach(Control child in root.Controls)
        {
            yield return child;
            foreach(var descendant in Descendants(child))yield return descendant;
        }
    }
}
