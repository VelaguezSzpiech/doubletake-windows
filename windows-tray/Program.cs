global using System;
global using System.IO;
global using System.Linq;
global using System.Collections.Generic;
global using System.Threading;
global using System.Threading.Tasks;

using WpfApplication = System.Windows.Application;
using WpfMessageBox = System.Windows.MessageBox;

namespace DoubleTake.Tray;

internal static class Program
{
    [STAThread]
    private static void Main()
    {
        if (!TrayHost.TryAcquireInstance(out var activationEvent)) return;
        System.Windows.Forms.Application.SetHighDpiMode(System.Windows.Forms.HighDpiMode.PerMonitorV2);
        var app = new WpfApplication { ShutdownMode = System.Windows.ShutdownMode.OnExplicitShutdown };
        FlyoutWindow? window = null;
        app.DispatcherUnhandledException += (_, e) =>
        {
            WpfMessageBox.Show(e.Exception.Message, "DoubleTake error", System.Windows.MessageBoxButton.OK, System.Windows.MessageBoxImage.Error);
            e.Handled = true;
        };
        app.SessionEnding += (_, _) => window?.EmergencyStop();
        try
        {
            window = new FlyoutWindow();
            app.MainWindow = window;
            using var host = new TrayHost(window, activationEvent);
            host.ShowFlyout();
            app.Run();
        }
        catch (Exception error)
        {
            window?.EmergencyStop();
            activationEvent.Dispose();
            WpfMessageBox.Show(error.Message, "DoubleTake could not start", System.Windows.MessageBoxButton.OK, System.Windows.MessageBoxImage.Error);
        }
    }
}
