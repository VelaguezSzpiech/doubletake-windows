package airplay

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// GStreamer child-process diagnostics.
//
// Every gst-launch-1.0 child (the video encoder and the PCM audio capture
// pipeline) is reported through [DIAG] records:
//
//	gst.start            pipeline name, pid, exact argument string, start count
//	gst.start_error      the child could not be started
//	gst.exit             exit code, runtime and stdout/stderr totals
//	gst.stats            every gstStatsInterval: stdout bytes per second (when
//	                     the reader is counted), CPU time and priority class
//	gst.stderr           every stderr line (rate-limited; repeats folded)
//	video.encoder_change a video encoder pipeline started after an earlier one
//
// gst-inspect-1.0 probes are not reported. Setting DOUBLETAKE_GST_DEBUG passes
// its value to the child as GST_DEBUG and writes GStreamer's own debug log to a
// per-process file in the DoubleTake state directory. Unset, the child
// environment is unchanged.
const (
	gstStatsInterval      = 5 * time.Second
	gstStderrLinesPerSec  = 20
	gstStderrRepeatReport = 10 * time.Second
	gstStderrMaxLine      = 1024
	gstDebugEnv           = "DOUBLETAKE_GST_DEBUG"
)

type gstUsage struct {
	user, kernel  time.Duration
	priorityClass uint32
}

type gstProc struct {
	name     string
	pid      int
	started  time.Time
	pipeline string

	stdoutCounted atomic.Bool
	stdoutBytes   atomic.Uint64
	stderrLines   atomic.Uint64

	// Owned by gstStatsLoop.
	lastStdout  uint64
	lastSample  time.Time
	windowStart time.Time
	lastUsage   gstUsage
	usageOK     bool
	perSecond   []uint64
}

var gstProcs struct {
	sync.Mutex
	live        map[*exec.Cmd]*gstProc
	latest      map[string]*gstProc
	starts      map[string]uint64
	lastEncoder map[string]any
	ticking     bool
}

// startGStreamerCommand starts a supervised GStreamer child. gst-launch-1.0
// children are additionally registered for diagnostics; the returned channel
// carries the same single Wait result as the platform implementation.
func startGStreamerCommand(cmd *exec.Cmd) (<-chan error, error) {
	name := gstPipelineName(cmd.Args)
	if name == "" {
		return startGStreamerCommandPlatform(cmd)
	}
	debugLevel, debugFile := applyGstDebugEnv(cmd, name)
	launchStart := time.Now()
	wait, err := startGStreamerCommandPlatform(cmd)
	if err != nil {
		diagEmit("gst.start_error", map[string]any{"pipeline": name, "error": err.Error(), "pipeline_args": gstArgsString(cmd.Args)})
		return nil, err
	}
	started := time.Now()
	p := &gstProc{name: name, pid: cmd.Process.Pid, started: started, pipeline: gstArgsString(cmd.Args)}
	encoder := map[string]any(nil)
	if name == "video" {
		encoder = gstEncoderSummary(cmd.Args)
	}

	gstProcs.Lock()
	if gstProcs.live == nil {
		gstProcs.live = map[*exec.Cmd]*gstProc{}
		gstProcs.latest = map[string]*gstProc{}
		gstProcs.starts = map[string]uint64{}
	}
	gstProcs.live[cmd] = p
	gstProcs.latest[name] = p
	gstProcs.starts[name]++
	starts := gstProcs.starts[name]
	previousEncoder := gstProcs.lastEncoder
	if encoder != nil {
		gstProcs.lastEncoder = encoder
	}
	startTicker := !gstProcs.ticking
	gstProcs.ticking = true
	gstProcs.Unlock()
	if startTicker {
		go gstStatsLoop()
	}

	rec := map[string]any{
		"pipeline":      name,
		"pid":           p.pid,
		"starts_total":  starts,
		"restart":       starts > 1,
		"launch_us":     micros(started.Sub(launchStart)),
		"pipeline_args": p.pipeline,
	}
	if encoder != nil {
		rec["encoder"] = encoder
	}
	if debugLevel != "" {
		rec["gst_debug"] = debugLevel
		rec["gst_debug_file"] = debugFile
	}
	diagEmit("gst.start", rec)
	if encoder != nil && previousEncoder != nil {
		diagEmit("video.encoder_change", map[string]any{
			"restart":          true,
			"starts_total":     starts,
			"previous_encoder": previousEncoder,
			"encoder":          encoder,
			"bitrate_changed":  fmt.Sprint(previousEncoder["bitrate"]) != fmt.Sprint(encoder["bitrate"]),
		})
	}

	out := make(chan error, 1)
	go func() {
		waitErr, ok := <-wait
		rec := map[string]any{
			"pipeline":           name,
			"pid":                p.pid,
			"runtime_s":          math.Round(time.Since(started).Seconds()*10) / 10,
			"stdout_kb_total":    p.stdoutBytes.Load() / 1024,
			"stdout_counted":     p.stdoutCounted.Load(),
			"stderr_lines_total": p.stderrLines.Load(),
		}
		if cmd.ProcessState != nil {
			code := cmd.ProcessState.ExitCode()
			rec["exit_code"] = code
			rec["exit_code_hex"] = fmt.Sprintf("0x%08x", uint32(code))
		}
		if waitErr != nil {
			rec["error"] = waitErr.Error()
		}
		gstProcs.Lock()
		delete(gstProcs.live, cmd)
		gstProcs.Unlock()
		diagEmit("gst.exit", rec)
		if ok {
			out <- waitErr
		}
		close(out)
	}()
	return out, nil
}

// gstPipelineName classifies a GStreamer command. Only gst-launch-1.0 children
// are instrumented; the result is "" for probes and unrelated commands.
func gstPipelineName(args []string) string {
	if len(args) == 0 {
		return ""
	}
	base := strings.ToLower(filepath.Base(args[0]))
	base = strings.TrimSuffix(base, ".exe")
	if base != "gst-launch-1.0" {
		return ""
	}
	for _, arg := range args[1:] {
		switch {
		case strings.HasPrefix(arg, "audio/x-raw"), arg == "wasapi2src", arg == "audiotestsrc", arg == "pulsesrc", arg == "rtpL16pay":
			return "audio"
		}
	}
	return "video"
}

func gstArgsString(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return strings.Join(append([]string{filepath.Base(args[0])}, args[1:]...), " ")
}

// gstEncoderSummary extracts the encoder element and its properties from a
// video pipeline so bitrate and GOP settings are visible without parsing.
func gstEncoderSummary(args []string) map[string]any {
	for i, arg := range args {
		if i == 0 || args[i-1] != "!" || !strings.HasSuffix(arg, "enc") {
			continue
		}
		summary := map[string]any{"element": arg}
		var props []string
		for _, prop := range args[i+1:] {
			if prop == "!" {
				break
			}
			props = append(props, prop)
			key, value, found := strings.Cut(prop, "=")
			if !found {
				continue
			}
			switch key {
			case "bitrate", "max-bitrate", "gop-size", "key-int-max", "idr-period", "rc-mode", "preset", "tune", "speed-preset", "bframes", "zerolatency", "rate-control":
				summary[key] = value
			}
		}
		summary["props"] = strings.Join(props, " ")
		return summary
	}
	return nil
}

// latestVideoEncoder returns the encoder summary of the most recent video child.
func latestVideoEncoder() map[string]any {
	gstProcs.Lock()
	defer gstProcs.Unlock()
	return gstProcs.lastEncoder
}

// gstCountStdout counts bytes the parent reads from a registered child's stdout.
// It adds one atomic increment per Read and does not buffer or copy.
func gstCountStdout(cmd *exec.Cmd, r io.ReadCloser) io.ReadCloser {
	gstProcs.Lock()
	p := gstProcs.live[cmd]
	gstProcs.Unlock()
	if p == nil || r == nil {
		return r
	}
	p.stdoutCounted.Store(true)
	return &gstCountingReader{ReadCloser: r, n: &p.stdoutBytes}
}

type gstCountingReader struct {
	io.ReadCloser
	n *atomic.Uint64
}

func (r *gstCountingReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.n.Add(uint64(n))
	}
	return n, err
}

func gstStatsLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for now := range ticker.C {
		gstProcs.Lock()
		if len(gstProcs.live) == 0 {
			gstProcs.ticking = false
			gstProcs.Unlock()
			return
		}
		procs := make([]*gstProc, 0, len(gstProcs.live))
		for _, p := range gstProcs.live {
			procs = append(procs, p)
		}
		gstProcs.Unlock()
		for _, p := range procs {
			p.sample(now)
		}
	}
}

func (p *gstProc) sample(now time.Time) {
	if p.windowStart.IsZero() {
		p.windowStart, p.lastSample = p.started, p.started
		p.lastUsage, p.usageOK = gstProcessUsage(p.pid)
	}
	total := p.stdoutBytes.Load()
	p.perSecond = append(p.perSecond, total-p.lastStdout)
	p.lastStdout = total
	p.lastSample = now
	if now.Sub(p.windowStart) < gstStatsInterval-500*time.Millisecond {
		return
	}
	elapsed := now.Sub(p.windowStart)
	rec := map[string]any{
		"pipeline":           p.name,
		"pid":                p.pid,
		"uptime_s":           math.Round(now.Sub(p.started).Seconds()*10) / 10,
		"interval_s":         math.Round(elapsed.Seconds()*10) / 10,
		"stdout_counted":     p.stdoutCounted.Load(),
		"stderr_lines_total": p.stderrLines.Load(),
	}
	if p.stdoutCounted.Load() {
		var sum uint64
		for _, n := range p.perSecond {
			sum += n
		}
		rec["stdout_bytes_per_s"] = append([]uint64(nil), p.perSecond...)
		rec["stdout_kb_total"] = total / 1024
		if elapsed > 0 {
			rec["stdout_kbps_iv"] = math.Round(float64(sum)*8/elapsed.Seconds()/1000*10) / 10
		}
	}
	if usage, ok := gstProcessUsage(p.pid); ok {
		rec["cpu_user_ms_total"] = usage.user.Milliseconds()
		rec["cpu_kernel_ms_total"] = usage.kernel.Milliseconds()
		if usage.priorityClass != 0 {
			rec["priority_class"] = fmt.Sprintf("0x%x", usage.priorityClass)
		}
		if p.usageOK && elapsed > 0 {
			busy := (usage.user - p.lastUsage.user) + (usage.kernel - p.lastUsage.kernel)
			rec["cpu_pct_iv"] = math.Round(float64(busy)/float64(elapsed)*1000) / 10
		}
		p.lastUsage, p.usageOK = usage, true
	}
	p.perSecond = p.perSecond[:0]
	p.windowStart = now
	diagEmit("gst.stats", rec)
}

// applyGstDebugEnv enables GStreamer's own debug log for the child when
// DOUBLETAKE_GST_DEBUG is set. The default environment is left untouched.
func applyGstDebugEnv(cmd *exec.Cmd, name string) (level, file string) {
	level = strings.TrimSpace(os.Getenv(gstDebugEnv))
	if level == "" {
		return "", ""
	}
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	env = append(env, "GST_DEBUG="+level, "GST_DEBUG_NO_COLOR=1")
	if dir := doubleTakeStateDir(); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err == nil {
			file = filepath.Join(dir, fmt.Sprintf("gst-%s-%s.log", name, time.Now().Format("20060102-150405.000")))
			env = append(env, "GST_DEBUG_FILE="+file)
		}
	}
	cmd.Env = env
	return level, file
}

// doubleTakeStateDir mirrors the tray's state directory on Windows
// (%LOCALAPPDATA%\DoubleTake\state) and the XDG state directory elsewhere.
func doubleTakeStateDir() string {
	if runtime.GOOS == "windows" {
		if base := os.Getenv("LOCALAPPDATA"); base != "" {
			return filepath.Join(base, "DoubleTake", "state")
		}
		return ""
	}
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		return filepath.Join(base, "doubletake")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "state", "doubletake")
	}
	return ""
}

func gstPipelineForPrefix(prefix string) string {
	switch prefix {
	case "GST":
		return "video"
	case "AUDIO-GST":
		return "audio"
	}
	return strings.ToLower(prefix)
}

// gstStderrLimiter bounds gst.stderr records: identical consecutive lines are
// folded into a repeat count, and distinct lines are limited per second with
// the number suppressed carried by the next emitted record.
type gstStderrLimiter struct {
	windowStart time.Time
	inWindow    int
	suppressed  int
	lastLine    string
	repeats     int
	repeatSince time.Time
}

func (l *gstStderrLimiter) observe(now time.Time, line string) []map[string]any {
	var out []map[string]any
	if line == l.lastLine && l.lastLine != "" {
		l.repeats++
		if now.Sub(l.repeatSince) >= gstStderrRepeatReport {
			out = append(out, map[string]any{"line": line, "repeat_n": l.repeats})
			l.repeats = 0
			l.repeatSince = now
		}
		return out
	}
	if l.repeats > 0 {
		out = append(out, map[string]any{"line": l.lastLine, "repeat_n": l.repeats, "repeat_end": true})
		l.repeats = 0
	}
	l.lastLine = line
	l.repeatSince = now
	if now.Sub(l.windowStart) >= time.Second {
		l.windowStart = now
		l.inWindow = 0
	}
	if l.inWindow >= gstStderrLinesPerSec {
		l.suppressed++
		return out
	}
	l.inWindow++
	rec := map[string]any{"line": line}
	if l.suppressed > 0 {
		rec["suppressed_n"] = l.suppressed
		l.suppressed = 0
	}
	return append(out, rec)
}

func (l *gstStderrLimiter) flush() []map[string]any {
	var out []map[string]any
	if l.repeats > 0 {
		out = append(out, map[string]any{"line": l.lastLine, "repeat_n": l.repeats, "repeat_end": true})
		l.repeats = 0
	}
	if l.suppressed > 0 {
		out = append(out, map[string]any{"line": "", "suppressed_n": l.suppressed})
		l.suppressed = 0
	}
	return out
}

func logStderr(prefix string, r io.Reader) {
	if r == nil {
		return
	}
	pipeline := gstPipelineForPrefix(prefix)
	gstProcs.Lock()
	proc := gstProcs.latest[pipeline]
	gstProcs.Unlock()
	emit := func(rec map[string]any) {
		rec["pipeline"] = pipeline
		if proc != nil {
			rec["pid"] = proc.pid
		}
		diagEmit("gst.stderr", rec)
	}
	var limiter gstStderrLimiter
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		dbg("[%s] %s", prefix, line)
		if proc != nil {
			proc.stderrLines.Add(1)
		}
		if len(line) > gstStderrMaxLine {
			line = line[:gstStderrMaxLine] + "..."
		}
		for _, rec := range limiter.observe(time.Now(), line) {
			emit(rec)
		}
	}
	for _, rec := range limiter.flush() {
		emit(rec)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
		dbg("[%s] stderr read error: %v", prefix, err)
		emit(map[string]any{"line": "", "read_error": err.Error()})
	}
}
