using System.Diagnostics;
using System.Text.Json;
using System.Text.Json.Serialization;

namespace DoubleTake.Tray;

internal sealed class Receiver
{
    public string Name { get; set; } = "";
    public string IP { get; set; } = "";
    public int Port { get; set; } = 7000;
    [JsonExtensionData] public Dictionary<string, JsonElement>? Advertisement { get; set; }
    public override string ToString() => $"{Name} — {IP}:{Port}";
}

internal sealed record BackendEvent(string Type, string Message, ulong RequestId, bool SessionReady);

internal sealed class BackendSession : IDisposable
{
    internal static readonly string StateDirectory = Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData), "DoubleTake", "state");
    internal static readonly string LogPath = Path.Combine(StateDirectory, "doubletake.log");
    private static readonly object LogLock = new();
    private readonly Process process;
    private readonly ProcessJob job;
    private readonly SemaphoreSlim writes = new(1, 1);
    private volatile bool stopping;
    private Task? stopTask;
    private int disposed;
    internal event Action<BackendEvent>? EventReceived;
    internal bool StopRequested => stopping;

    internal BackendSession(Receiver receiver, bool forcePair)
    {
        var start = StartInfo("-ui", "-target", receiver.IP, "-port", receiver.Port.ToString(),
            "-device-json", JsonSerializer.Serialize(receiver), "-cred-backend", "keyring", "-creds", Path.Combine(StateDirectory, "credentials.json"),
            "-video-codec", "h264", "-full-hd", "-fps", "60", "-bitrate", "10000");
        // A tray started by an already-running Explorer can inherit an old environment.
        string? nvencDevice = Environment.GetEnvironmentVariable("DOUBLETAKE_NVENC_DEVICE", EnvironmentVariableTarget.User)
            ?? Environment.GetEnvironmentVariable("DOUBLETAKE_NVENC_DEVICE");
        if (!string.IsNullOrWhiteSpace(nvencDevice))
        {
            start.ArgumentList.Add("-hwaccel");
            start.ArgumentList.Add("nvenc");
            start.ArgumentList.Add("-nvenc-device");
            start.ArgumentList.Add(nvencDevice.Trim());
        }
        if (forcePair) start.ArgumentList.Add("-pair");
        job = new ProcessJob();
        process = new Process { StartInfo = start };
    }

    internal static ProcessStartInfo StartInfo(params string[] arguments)
    {
        string executable = Path.Combine(AppContext.BaseDirectory, "doubletake.exe");
        if (!File.Exists(executable)) throw new FileNotFoundException("The streaming backend doubletake.exe must be beside DoubleTake.Tray.exe.", executable);
        Directory.CreateDirectory(StateDirectory);
        var start = new ProcessStartInfo(executable)
        {
            UseShellExecute = false, CreateNoWindow = true, WindowStyle = ProcessWindowStyle.Hidden,
            RedirectStandardOutput = true, RedirectStandardError = true, RedirectStandardInput = true,
            WorkingDirectory = AppContext.BaseDirectory
        };
        foreach (string argument in arguments) start.ArgumentList.Add(argument);
        // The tray owns credential entry. Do not inherit a credential from a
        // launcher's environment or pass it on the command line.
        start.Environment.Remove("DOUBLETAKE_CODE");
        var candidates = new[]
        {
            start.Environment.TryGetValue("GSTREAMER_1_0_ROOT_MSVC_X86_64", out var root) && !string.IsNullOrWhiteSpace(root) ? Path.Combine(root, "bin") : "",
            Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData), "Programs", "gstreamer", "1.0", "msvc_x86_64", "bin"),
            Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.ProgramFiles), "gstreamer", "1.0", "msvc_x86_64", "bin"),
            @"C:\gstreamer\1.0\msvc_x86_64\bin"
        };
        string? runtime = candidates.FirstOrDefault(directory => File.Exists(Path.Combine(directory, "gst-launch-1.0.exe")));
        if (runtime != null)
        {
            start.Environment.TryGetValue("PATH", out var path);
            start.Environment["PATH"] = runtime + Path.PathSeparator + path;
        }
        return start;
    }

    internal static void Log(string message)
    {
        lock (LogLock)
        {
            Directory.CreateDirectory(StateDirectory);
            // Keep a bounded local diagnostic file, never stdin or credentials.
            if (File.Exists(LogPath) && new FileInfo(LogPath).Length > 4 * 1024 * 1024)
                File.Move(LogPath, LogPath + ".previous", true);
            File.AppendAllText(LogPath, $"{DateTimeOffset.Now:O} {message}{Environment.NewLine}");
        }
    }

    internal static async Task<Receiver[]> DiscoverAsync(CancellationToken cancellation)
    {
        using var process = new Process { StartInfo = StartInfo("-discover-json") };
        using var job = new ProcessJob();
        if (!process.Start()) throw new IOException("Discovery backend did not start.");
        try
        {
            job.Assign(process);
            Task<string> output = process.StandardOutput.ReadToEndAsync(cancellation);
            Task<string> diagnostics = process.StandardError.ReadToEndAsync(cancellation);
            await process.WaitForExitAsync(cancellation);
            string stderr = await diagnostics;
            if (!string.IsNullOrWhiteSpace(stderr)) Log(stderr);
            if (process.ExitCode != 0) throw new IOException("AirPlay discovery failed. " + stderr.Trim());
            return JsonSerializer.Deserialize<Receiver[]>(await output) ?? [];
        }
        finally
        {
            if (!process.HasExited) process.Kill(entireProcessTree: true);
        }
    }

    internal async Task<int> RunAsync()
    {
        if (!process.Start()) throw new IOException("Streaming backend did not start.");
        try
        {
            job.Assign(process);
            Task diagnostics = ReadDiagnosticsAsync();
            Task events = ReadEventsAsync();
            await SendAsync(new { type = "start" });
            if (stopping) await SendAsync(new { type = "stop" });
            await process.WaitForExitAsync();
            await Task.WhenAll(diagnostics, events);
            return process.ExitCode;
        }
        finally
        {
            if (!process.HasExited) process.Kill(entireProcessTree: true);
        }
    }

    private async Task ReadDiagnosticsAsync()
    {
        while (await process.StandardError.ReadLineAsync() is string line)
        {
            try { Log(line); }
            catch (IOException) { EventReceived?.Invoke(new("warning", "The local diagnostic log could not be written.", 0, false)); }
            catch (UnauthorizedAccessException) { EventReceived?.Invoke(new("warning", "Access to the local diagnostic log was denied.", 0, false)); }
        }
    }

    private async Task ReadEventsAsync()
    {
        try
        {
            while (await process.StandardOutput.ReadLineAsync() is string line)
            {
                using var document = JsonDocument.Parse(line);
                var item = document.RootElement;
                string type = item.GetProperty("type").GetString() ?? "";
                string message = item.TryGetProperty("message", out var text) ? text.GetString() ?? "" : "";
                ulong requestId = item.TryGetProperty("request_id", out var id) ? id.GetUInt64() : 0;
                bool ready = item.TryGetProperty("session_ready", out var state) && state.GetBoolean();
                EventReceived?.Invoke(new(type, message, requestId, ready));
            }
        }
        catch (Exception error) when (error is JsonException or InvalidOperationException or KeyNotFoundException or FormatException)
        {
            EventReceived?.Invoke(new("error", "The backend returned an invalid status message.", 0, false));
            await StopAsync();
        }
    }

    internal Task SendCredentialAsync(ulong requestId, string value) => SendAsync(new { type = "credential", request_id = requestId, value });

    private async Task SendAsync(object command)
    {
        await writes.WaitAsync();
        try
        {
            if (process.HasExited) throw new IOException("The streaming backend has exited.");
            await process.StandardInput.WriteLineAsync(JsonSerializer.Serialize(command));
            await process.StandardInput.FlushAsync();
        }
        finally { writes.Release(); }
    }

    internal Task StopAsync()
    {
        stopping = true;
        return stopTask ??= StopCoreAsync();
    }

    private async Task StopCoreAsync()
    {
        try
        {
            if (process.HasExited) return;
            await SendAsync(new { type = "stop" });
        }
        catch (Exception error) when (error is IOException or InvalidOperationException) { }
        try
        {
            using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(5));
            await process.WaitForExitAsync(timeout.Token);
        }
        catch (OperationCanceledException)
        {
            // The already-owned job kills the backend and every descendant,
            // without a HasExited/Kill race against normal receiver teardown.
            job.Dispose();
            await process.WaitForExitAsync();
        }
        catch (InvalidOperationException) { }
    }

    internal void EmergencyStop()
    {
        stopping = true;
        job.Dispose();
    }

    public void Dispose()
    {
        if (Interlocked.Exchange(ref disposed, 1) != 0) return;
        job.Dispose();
        process.Dispose();
        writes.Dispose();
    }
}
