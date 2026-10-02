using System.ComponentModel;
using System.Drawing;
using System.Drawing.Drawing2D;
using System.Runtime.InteropServices;
using System.Security.Principal;
using System.Windows;
using System.Windows.Input;
using System.Windows.Interop;
using System.Windows.Threading;
using Microsoft.Win32;
using Forms = System.Windows.Forms;
using WpfApplication = System.Windows.Application;

namespace DoubleTake.Tray;

internal sealed class TrayHost : IDisposable
{
    private readonly FlyoutWindow window;
    private readonly Forms.NotifyIcon tray;
    private readonly Forms.ContextMenuStrip menu;
    private readonly Forms.ToolStripMenuItem connect;
    private readonly Forms.ToolStripMenuItem disconnect;
    private readonly DispatcherTimer dismissal;
    private readonly EventWaitHandle? activationEvent;
    private readonly RegisteredWaitHandle? activationWait;
    private Icon icon;
    private bool disposed;
    private bool quitting;
    private bool quitComplete;
    private long lastOutsideDismissal;

    internal static bool TryAcquireInstance(out EventWaitHandle activationEvent)
    {
        // Session-local, per-user kernel event: no listening ports, receiver data or credentials.
        using WindowsIdentity identity = WindowsIdentity.GetCurrent();
        string user = identity.User?.Value ?? Environment.UserName;
        activationEvent = new EventWaitHandle(false, EventResetMode.AutoReset,
            @"Local\DoubleTake.Tray.Activate." + user, out bool created);
        if (created) return true;
        activationEvent.Set();
        activationEvent.Dispose();
        return false;
    }

    internal TrayHost(FlyoutWindow window, EventWaitHandle? activationEvent = null)
    {
        this.window = window;
        this.activationEvent = activationEvent;
        menu = new Forms.ContextMenuStrip();
        menu.Items.Add("Open DoubleTake", null, (_, _) => ShowFlyout());
        connect = new Forms.ToolStripMenuItem("Connect", null, async (_, _) =>
        {
            ShowFlyout();
            await window.ConnectAsync();
        });
        disconnect = new Forms.ToolStripMenuItem("Disconnect", null, async (_, _) => await window.DisconnectAsync());
        menu.Items.Add(connect);
        menu.Items.Add(disconnect);
        menu.Items.Add("Open local log", null, (_, _) => window.OpenLog());
        menu.Items.Add(new Forms.ToolStripSeparator());
        menu.Items.Add("Quit DoubleTake", null, async (_, _) => await QuitAsync());
        menu.Opening += OnMenuOpening;
        menu.Closed += OnMenuClosed;
        icon = CreateIcon();
        tray = new Forms.NotifyIcon { Icon = icon, Text = "DoubleTake — disconnected", ContextMenuStrip = menu, Visible = true };
        tray.MouseClick += OnTrayClick;
        tray.MouseDown += OnTrayMouseDown;
        tray.BalloonTipClicked += OnBalloonClicked;
        dismissal = new DispatcherTimer(DispatcherPriority.Background, window.Dispatcher)
        {
            Interval = TimeSpan.FromMilliseconds(160)
        };
        dismissal.Tick += OnDismissalTick;
        window.Deactivated += OnDeactivated;
        window.PreviewKeyDown += OnKeyDown;
        window.SizeChanged += OnSizeChanged;
        window.Closing += OnClosing;
        window.SourceInitialized += OnSourceInitialized;
        window.RevealRequested += ShowFlyout;
        window.StatusChanged += OnStatusChanged;
        window.QuitRequested += OnQuitRequested;
        SystemEvents.UserPreferenceChanged += OnPreferenceChanged;
        SystemParameters.StaticPropertyChanged += OnSystemParameterChanged;
        if (WpfApplication.Current != null)
        {
            WpfApplication.Current.SessionEnding += OnSessionEnding;
            WpfApplication.Current.Exit += OnApplicationExit;
        }
        window.ApplyTheme(FlyoutTheme.GetPalette());
        UpdateMenu();
        if (activationEvent != null)
            activationWait = ThreadPool.RegisterWaitForSingleObject(activationEvent, (_, _) =>
                Dispatch(ShowFlyout), null, Timeout.Infinite, false);
    }

    internal void ShowFlyout()
    {
        if (disposed || quitting) return;
        dismissal.Stop();
        if (!window.IsVisible) window.Show();
        FlyoutChrome.Apply(window);
        FlyoutChrome.Position(window);
        window.Activate();
        SetForegroundWindow(new WindowInteropHelper(window).Handle);
    }

    internal void ToggleFlyout()
    {
        if (disposed || quitting) return;
        if (window.IsVisible)
        {
            if (window.IsInteractionPinned) ShowFlyout();
            else { dismissal.Stop(); window.Hide(); }
        }
        else
        {
            // Activation loss arrives before NotifyIcon.MouseClick for the very same click.
            // Suppress that one click rather than reopening the panel we have just dismissed.
            if (lastOutsideDismissal != 0 && Environment.TickCount64 - lastOutsideDismissal < 300) return;
            ShowFlyout();
        }
    }
    private void OnTrayClick(object? sender, Forms.MouseEventArgs e)
    {
        if (e.Button != Forms.MouseButtons.Left) return;
        FlyoutChrome.RememberAnchor();
        ToggleFlyout();
    }

    private void OnTrayMouseDown(object? sender, Forms.MouseEventArgs e)
    {
        FlyoutChrome.RememberAnchor();
        if (e.Button == Forms.MouseButtons.Right) dismissal.Stop();
    }

    private void OnBalloonClicked(object? sender, EventArgs e) => ShowFlyout();
    private void OnSourceInitialized(object? sender, EventArgs e) => FlyoutChrome.Apply(window);
    private void OnSizeChanged(object sender, SizeChangedEventArgs e) => FlyoutChrome.Position(window);
    private void OnDeactivated(object? sender, EventArgs e)
    {
        if (!quitting && !window.IsInteractionPinned) dismissal.Start();
    }

    private void OnDismissalTick(object? sender, EventArgs e)
    {
        dismissal.Stop();
        if (disposed || quitting || !window.IsVisible || window.IsActive || window.IsInteractionPinned || menu.Visible) return;
        IntPtr foreground = GetForegroundWindow();
        IntPtr handle = new WindowInteropHelper(window).Handle;
        // Owned native dialogs and WPF popup menus keep their flyout alive.
        if (foreground == handle || (foreground != IntPtr.Zero && GetWindow(foreground, 4) == handle)) return;
        if (Mouse.Captured != null || Keyboard.FocusedElement is System.Windows.Controls.ContextMenu) return;
        lastOutsideDismissal = Environment.TickCount64;
        window.Hide();
    }

    private void OnKeyDown(object sender, System.Windows.Input.KeyEventArgs e)
    {
        if (e.Key != Key.Escape || window.IsInteractionPinned || menu.Visible) return;
        e.Handled = true;
        dismissal.Stop();
        window.Hide();
    }

    private void OnMenuOpening(object? sender, CancelEventArgs e)
    {
        dismissal.Stop();
        UpdateMenu();
        e.Cancel = quitting;
    }
    private void OnMenuClosed(object? sender, Forms.ToolStripDropDownClosedEventArgs e)
    {
        if (window.IsVisible && !window.IsActive && !window.IsInteractionPinned) dismissal.Start();
    }
    private void UpdateMenu()
    {
        connect.Enabled = !quitting && !window.IsConnected && !window.IsInteractionPinned;
        disconnect.Enabled = !quitting && (window.IsConnected || window.IsInteractionPinned);
    }
    private void OnStatusChanged(string status, bool connected)
    {
        UpdateMenu();
        // NotifyIcon tooltips have a fixed native limit. Avoid leaking verbose backend messages.
        tray.Text = window.IsConnected ? "DoubleTake — screen and audio connected"
            : window.IsInteractionPinned ? "DoubleTake — pairing" : "DoubleTake — disconnected";
    }
    private void OnClosing(object? sender, CancelEventArgs e)
    {
        if (quitComplete) return;
        e.Cancel = true;
        if (!window.IsInteractionPinned) window.Hide();
    }
    private void OnQuitRequested() => _ = QuitAsync();
    private async Task QuitAsync()
    {
        if (quitting || disposed) return;
        quitting = true;
        dismissal.Stop();
        UpdateMenu();
        menu.Enabled = false;
        // Keep the process/icon alive until its owned streaming process has stopped.
        try
        {
            await window.ShutdownAsync();
            quitComplete = true;
            Dispose();
            window.Close();
            WpfApplication.Current?.Shutdown();
        }
        catch (Exception error)
        {
            quitting = false;
            menu.Enabled = true;
            UpdateMenu();
            ShowFlyout();
            System.Windows.MessageBox.Show(window, "DoubleTake could not stop sharing. " + error.Message,
                "DoubleTake", MessageBoxButton.OK, MessageBoxImage.Error);
        }
    }

    private void OnPreferenceChanged(object sender, UserPreferenceChangedEventArgs e) => Dispatch(UpdateTheme);
    private void OnSystemParameterChanged(object? sender, PropertyChangedEventArgs e) => Dispatch(UpdateTheme);
    private void UpdateTheme()
    {
        if (disposed) return;
        window.ApplyTheme(FlyoutTheme.GetPalette());
        if (new WindowInteropHelper(window).Handle != IntPtr.Zero) FlyoutChrome.Apply(window);
        Icon next = CreateIcon();
        tray.Icon = next;
        icon.Dispose();
        icon = next;
        FlyoutChrome.Position(window);
    }
    private void Dispatch(Action action)
    {
        if (!disposed && !window.Dispatcher.HasShutdownStarted)
            window.Dispatcher.BeginInvoke(action);
    }
    private void OnSessionEnding(object sender, SessionEndingCancelEventArgs e) => _ = QuitAsync();
    private void OnApplicationExit(object sender, ExitEventArgs e) => Dispose();

    private static Icon CreateIcon()
    {
        using var bitmap = new Bitmap(32, 32);
        using Graphics graphics = Graphics.FromImage(bitmap);
        graphics.SmoothingMode = SmoothingMode.AntiAlias;
        graphics.Clear(Color.Transparent);
        using var pen = new Pen(FlyoutTheme.GetTrayGlyphColor(), 2.4f)
        {
            StartCap = LineCap.Round, EndCap = LineCap.Round, LineJoin = LineJoin.Round
        };
        // Quiet screen/cast mark; no generic app logo or blurred raster asset.
        graphics.DrawLines(pen, [new PointF(5, 13), new PointF(5, 6), new PointF(27, 6), new PointF(27, 23), new PointF(20, 23)]);
        graphics.DrawArc(pen, -2.5f, 21.5f, 12, 12, 270, 90);
        graphics.DrawArc(pen, -7.5f, 16.5f, 22, 22, 270, 90);
        using var dot = new SolidBrush(pen.Color);
        graphics.FillEllipse(dot, 2, 26, 3, 3);
        IntPtr nativeIcon = bitmap.GetHicon();
        try
        {
            using Icon temporary = Icon.FromHandle(nativeIcon);
            return (Icon)temporary.Clone();
        }
        finally { DestroyIcon(nativeIcon); }
    }

    public void Dispose()
    {
        if (disposed) return;
        disposed = true;
        activationWait?.Unregister(null);
        activationEvent?.Dispose();
        dismissal.Stop();
        dismissal.Tick -= OnDismissalTick;
        window.Deactivated -= OnDeactivated;
        window.PreviewKeyDown -= OnKeyDown;
        window.SizeChanged -= OnSizeChanged;
        window.Closing -= OnClosing;
        window.SourceInitialized -= OnSourceInitialized;
        window.RevealRequested -= ShowFlyout;
        window.StatusChanged -= OnStatusChanged;
        window.QuitRequested -= OnQuitRequested;
        SystemEvents.UserPreferenceChanged -= OnPreferenceChanged;
        SystemParameters.StaticPropertyChanged -= OnSystemParameterChanged;
        if (WpfApplication.Current != null)
        {
            WpfApplication.Current.SessionEnding -= OnSessionEnding;
            WpfApplication.Current.Exit -= OnApplicationExit;
        }
        tray.Visible = false;
        tray.MouseClick -= OnTrayClick;
        tray.MouseDown -= OnTrayMouseDown;
        tray.BalloonTipClicked -= OnBalloonClicked;
        tray.Dispose();
        menu.Opening -= OnMenuOpening;
        menu.Closed -= OnMenuClosed;
        menu.Dispose();
        icon.Dispose();
    }

    [DllImport("user32.dll")]
    [return: MarshalAs(UnmanagedType.Bool)]
    private static extern bool DestroyIcon(IntPtr icon);
    [DllImport("user32.dll")]
    [return: MarshalAs(UnmanagedType.Bool)]
    private static extern bool SetForegroundWindow(IntPtr window);
    [DllImport("user32.dll")]
    private static extern IntPtr GetForegroundWindow();
    [DllImport("user32.dll")]
    private static extern IntPtr GetWindow(IntPtr window, uint command);
}
