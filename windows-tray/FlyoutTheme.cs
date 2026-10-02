using System.Runtime.InteropServices;
using System.Windows;
using System.Windows.Media;
using Microsoft.Win32;

namespace DoubleTake.Tray;

internal sealed record FlyoutPalette(
    bool IsDark,
    bool HighContrast,
    bool TransparencyEnabled,
    Color Surface,
    Color Card,
    Color Border,
    Color Text,
    Color SecondaryText,
    Color Accent,
    Color AccentText);

internal static class FlyoutTheme
{
    internal static System.Drawing.Color GetTrayGlyphColor()
    {
        if (SystemParameters.HighContrast)
        {
            Color text = SystemColors.WindowTextColor;
            return System.Drawing.Color.FromArgb(text.R, text.G, text.B);
        }
        return ReadPreference("SystemUsesLightTheme", 1) == 0
            ? System.Drawing.Color.FromArgb(245, 245, 245)
            : System.Drawing.Color.FromArgb(35, 35, 35);
    }

    internal static FlyoutPalette GetPalette()
    {
        bool highContrast = SystemParameters.HighContrast;
        bool dark = ReadPreference("AppsUseLightTheme", 1) == 0;
        bool transparent = !highContrast && ReadPreference("EnableTransparency", 0) != 0
            && OperatingSystem.IsWindowsVersionAtLeast(10, 0, 22621);
        if (highContrast)
        {
            return new FlyoutPalette(dark, true, false,
                SystemColors.WindowColor, SystemColors.WindowColor,
                SystemColors.WindowTextColor, SystemColors.WindowTextColor,
                SystemColors.WindowTextColor, SystemColors.HighlightColor,
                SystemColors.HighlightTextColor);
        }

        Color accent = Color.FromRgb(0, 103, 192);
        if (DwmGetColorizationColor(out uint argb, out _) == 0)
            accent = Color.FromRgb((byte)(argb >> 16), (byte)(argb >> 8), (byte)argb);
        Color accentText = ContrastText(accent);
        return dark
            ? new FlyoutPalette(true, false, transparent,
                Color.FromArgb(transparent ? (byte)235 : (byte)255, 37, 36, 34),
                Color.FromRgb(46, 45, 43), Color.FromRgb(67, 65, 62),
                Color.FromRgb(246, 244, 241), Color.FromRgb(182, 180, 176), accent, accentText)
            : new FlyoutPalette(false, false, transparent,
                Color.FromArgb(transparent ? (byte)240 : (byte)255, 249, 248, 246),
                Color.FromRgb(255, 255, 255), Color.FromRgb(222, 220, 216),
                Color.FromRgb(31, 30, 29), Color.FromRgb(100, 98, 95), accent, accentText);
    }

    // Registry values are optional: unsupported/locked-down systems get a solid light panel.
    private static int ReadPreference(string name, int fallback)
    {
        try
        {
            using RegistryKey? key = Registry.CurrentUser.OpenSubKey(
                @"Software\Microsoft\Windows\CurrentVersion\Themes\Personalize");
            return key?.GetValue(name) is int value ? value : fallback;
        }
        catch (System.Security.SecurityException) { return fallback; }
        catch (UnauthorizedAccessException) { return fallback; }
        catch (IOException) { return fallback; }
    }

    private static Color ContrastText(Color color)
    {
        static double Linear(byte channel)
        {
            double value = channel / 255d;
            return value <= .04045 ? value / 12.92 : Math.Pow((value + .055) / 1.055, 2.4);
        }
        double luminance = .2126 * Linear(color.R) + .7152 * Linear(color.G) + .0722 * Linear(color.B);
        return luminance > .179 ? Colors.Black : Colors.White;
    }

    [DllImport("dwmapi.dll")]
    private static extern int DwmGetColorizationColor(out uint color, [MarshalAs(UnmanagedType.Bool)] out bool opaqueBlend);
}
