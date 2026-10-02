using System.Runtime.InteropServices;
using System.Windows;
using System.Windows.Interop;
using System.Windows.Media;

namespace DoubleTake.Tray;

internal static class FlyoutChrome
{
    private static NativePoint? anchor;
    private static bool positioning;

    internal static void RememberAnchor()
    {
        if (GetCursorPos(out NativePoint point)) anchor = point;
    }

    internal static void Apply(Window window)
    {
        IntPtr handle = new WindowInteropHelper(window).EnsureHandle();
        int extendedStyle = GetWindowLong(handle, -20);
        SetWindowLong(handle, -20, (extendedStyle | 0x80) & ~0x40000); // Tool window, never an app/taskbar window.
        FlyoutPalette palette = FlyoutTheme.GetPalette();
        int dark = palette.IsDark && !palette.HighContrast ? 1 : 0;
        DwmSetWindowAttribute(handle, 20, ref dark, sizeof(int));
        int corner = palette.HighContrast ? 1 : 2; // DWMWCP_DONOTROUND / ROUND.
        DwmSetWindowAttribute(handle, 33, ref corner, sizeof(int));
        bool backdrop = palette.TransparencyEnabled && !palette.HighContrast;
        int material = backdrop ? 3 : 1; // DWMSBT_TRANSIENTWINDOW / NONE (Windows 11 22H2+).
        if (OperatingSystem.IsWindowsVersionAtLeast(10, 0, 22621))
            backdrop &= DwmSetWindowAttribute(handle, 38, ref material, sizeof(int)) == 0;
        else backdrop = false;
        Margins margins = backdrop ? new Margins(-1, -1, -1, -1) : new Margins(0, 0, 0, 0);
        DwmExtendFrameIntoClientArea(handle, ref margins);
        HwndSource? source = HwndSource.FromHwnd(handle);
        if (source?.CompositionTarget != null)
            source.CompositionTarget.BackgroundColor = backdrop ? Colors.Transparent
                : Color.FromRgb(palette.Surface.R, palette.Surface.G, palette.Surface.B);
        // Unsupported DWM attributes are deliberately harmless; WPF retains its solid surface.
    }

    internal static void Position(Window window)
    {
        if (positioning || !window.IsVisible) return;
        positioning = true;
        try
        {
            IntPtr handle = new WindowInteropHelper(window).EnsureHandle();
            bool rememberedAnchor = anchor.HasValue;
            NativePoint point = anchor.GetValueOrDefault();
            IntPtr monitor = IntPtr.Zero;
            NativeRect taskbar = default;
            bool taskbarAnchor = !rememberedAnchor && TryGetTaskbarAnchor(out point, out monitor, out taskbar);
            if (!taskbarAnchor) monitor = MonitorFromPoint(point, rememberedAnchor ? 2u : 1u);
            MonitorInfo info = new() { Size = Marshal.SizeOf<MonitorInfo>() };
            if (!GetMonitorInfo(monitor, ref info)) return;
            if (!rememberedAnchor && !taskbarAnchor)
                point = new NativePoint { X = info.Work.Right, Y = info.Work.Bottom };
            uint dpi = 96;
            if (OperatingSystem.IsWindowsVersionAtLeast(6, 3)
                && GetDpiForMonitor(monitor, 0, out uint monitorDpi, out _) == 0) dpi = monitorDpi;
            double scale = dpi / 96d;
            window.MaxHeight = Math.Max(1, (info.Work.Bottom - info.Work.Top) / scale - 24);
            window.MaxWidth = Math.Max(1, (info.Work.Right - info.Work.Left) / scale - 24);
            int width = (int)Math.Ceiling(Math.Min(window.ActualWidth > 0 ? window.ActualWidth : window.Width, window.MaxWidth) * scale);
            int height = (int)Math.Ceiling(Math.Min(window.ActualHeight > 0 ? window.ActualHeight : window.Height, window.MaxHeight) * scale);
            int gap = (int)Math.Round(12 * scale);
            NativeRect work = info.Work;
            NativeRect screen = info.Monitor;
            // Keep real tray clicks on their monitor (including overflow trays). Without a click,
            // use the primary taskbar's notification end, never the current desktop cursor.
            int left = point.X - width / 2;
            int top = point.Y - height - gap;
            uint edge = taskbarAnchor ? GetTaskbarEdge(taskbar, screen)
                : work.Left > screen.Left ? 0u : work.Top > screen.Top ? 1u
                : work.Right < screen.Right ? 2u : 3u;
            bool fullWorkArea = work.Left == screen.Left && work.Top == screen.Top
                && work.Right == screen.Right && work.Bottom == screen.Bottom;
            if (!taskbarAnchor && fullWorkArea && OperatingSystem.IsWindowsVersionAtLeast(6, 2))
            {
                // Auto-hide taskbars do not reserve a work-area strip. Ask the shell per monitor.
                AppBarData bar = new() { Size = (uint)Marshal.SizeOf<AppBarData>(), Rect = screen };
                for (uint candidate = 0; candidate < 4; candidate++)
                {
                    bar.Edge = candidate;
                    if (SHAppBarMessage(0xB, ref bar) != UIntPtr.Zero) { edge = candidate; break; }
                }
            }
            if (edge == 3)
            {
                top = work.Bottom - height - gap;
                left = point.X - width + (int)(24 * scale);
            }
            else if (edge == 1) top = work.Top + gap;
            else if (edge == 0) { left = work.Left + gap; top = point.Y - height / 2; }
            else { left = work.Right - width - gap; top = point.Y - height / 2; }
            left = Math.Clamp(left, work.Left + gap, Math.Max(work.Left + gap, work.Right - width - gap));
            top = Math.Clamp(top, work.Top + gap, Math.Max(work.Top + gap, work.Bottom - height - gap));
            SetWindowPos(handle, IntPtr.Zero, left, top, 0, 0, 0x15); // NOSIZE | NOZORDER | NOACTIVATE.
        }
        finally { positioning = false; }
    }

    private static bool TryGetTaskbarAnchor(out NativePoint point, out IntPtr monitor, out NativeRect taskbar)
    {
        point = default;
        monitor = IntPtr.Zero;
        taskbar = default;
        IntPtr window = FindWindow("Shell_TrayWnd", null);
        if (window == IntPtr.Zero || !GetWindowRect(window, out taskbar)
            || taskbar.Right <= taskbar.Left || taskbar.Bottom <= taskbar.Top) return false;
        monitor = MonitorFromWindow(window, 1); // MONITOR_DEFAULTTOPRIMARY.
        if (monitor == IntPtr.Zero) return false;
        point = new NativePoint { X = taskbar.Right, Y = taskbar.Bottom };
        return true;
    }

    private static uint GetTaskbarEdge(NativeRect taskbar, NativeRect screen)
    {
        if (taskbar.Right - taskbar.Left >= taskbar.Bottom - taskbar.Top)
            return taskbar.Top - screen.Top < screen.Bottom - taskbar.Bottom ? 1u : 3u;
        return taskbar.Left - screen.Left < screen.Right - taskbar.Right ? 0u : 2u;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct NativePoint { internal int X, Y; }
    [StructLayout(LayoutKind.Sequential)]
    private struct NativeRect { internal int Left, Top, Right, Bottom; }
    [StructLayout(LayoutKind.Sequential)]
    private struct MonitorInfo { internal int Size; internal NativeRect Monitor, Work; internal uint Flags; }
    [StructLayout(LayoutKind.Sequential)]
    private struct AppBarData
    {
        internal uint Size;
        internal IntPtr Window;
        internal uint CallbackMessage, Edge;
        internal NativeRect Rect;
        internal IntPtr Parameter;
    }
    [StructLayout(LayoutKind.Sequential)]
    private readonly struct Margins
    {
        internal readonly int Left, Right, Top, Bottom;
        internal Margins(int left, int right, int top, int bottom) => (Left, Right, Top, Bottom) = (left, right, top, bottom);
    }
    [DllImport("user32.dll")]
    [return: MarshalAs(UnmanagedType.Bool)]
    private static extern bool GetCursorPos(out NativePoint point);
    [DllImport("user32.dll")]
    private static extern IntPtr MonitorFromPoint(NativePoint point, uint flags);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)]
    private static extern IntPtr FindWindow(string className, string? windowName);
    [DllImport("user32.dll")]
    private static extern IntPtr MonitorFromWindow(IntPtr window, uint flags);
    [DllImport("user32.dll")]
    [return: MarshalAs(UnmanagedType.Bool)]
    private static extern bool GetWindowRect(IntPtr window, out NativeRect rect);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)]
    [return: MarshalAs(UnmanagedType.Bool)]
    private static extern bool GetMonitorInfo(IntPtr monitor, ref MonitorInfo info);
    [DllImport("shell32.dll")]
    private static extern UIntPtr SHAppBarMessage(uint message, ref AppBarData data);
    [DllImport("shcore.dll")]
    private static extern int GetDpiForMonitor(IntPtr monitor, int type, out uint dpiX, out uint dpiY);
    [DllImport("user32.dll", EntryPoint = "GetWindowLongW")]
    private static extern int GetWindowLong(IntPtr window, int index);
    [DllImport("user32.dll", EntryPoint = "SetWindowLongW")]
    private static extern int SetWindowLong(IntPtr window, int index, int value);
    [DllImport("user32.dll")]
    [return: MarshalAs(UnmanagedType.Bool)]
    private static extern bool SetWindowPos(IntPtr window, IntPtr after, int x, int y, int width, int height, uint flags);
    [DllImport("dwmapi.dll")]
    private static extern int DwmSetWindowAttribute(IntPtr window, int attribute, ref int value, int size);
    [DllImport("dwmapi.dll")]
    private static extern int DwmExtendFrameIntoClientArea(IntPtr window, ref Margins margins);
}
