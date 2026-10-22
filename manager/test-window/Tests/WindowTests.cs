using System.Runtime.ExceptionServices;
using AgentStatus.TestWindow;

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
                        Assert.IsTrue(Descendants(form).OfType<Label>().Any(l=>l.Text.Contains("不会启动服务")));
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

    private static IEnumerable<Control> Descendants(Control root)
    {
        foreach(Control child in root.Controls)
        {
            yield return child;
            foreach(var descendant in Descendants(child))yield return descendant;
        }
    }
}
