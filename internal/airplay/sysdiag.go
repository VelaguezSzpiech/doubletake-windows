package airplay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Host, OS and network telemetry for correlating audio disturbances with
// things outside the media path (Wi-Fi jitter, DPC storms, timer changes,
// power plan switches, sleep, leaks). Records go through diagEmit like the
// audio records; every probe runs in its own goroutine with its own timeout so
// a slow or hung OS API never delays the media loop.
//
// kind values:
//
//	sys.start          once: OS, CPU, RAM, power plan, timer resolution, NICs,
//	                   routing NIC, Wi-Fi association (hashed SSID/BSSID)
//	sys.health         every sysHealthInterval: CPU/DPC/interrupt load, memory,
//	                   disk queue, per-process resources, routing-NIC counters,
//	                   TCP/UDP errors, Wi-Fi signal/rates, scheduler lateness, GPU
//	sys.event          state changes (power plan, Wi-Fi roam, link speed, route,
//	                   timer resolution, clock step, default audio endpoint)
//	sys.sampler_gap    the 1 s clock tick arrived more than sysGapThreshold late
//	                   (sleep/resume, whole-process stall)
//	sys.unavailable    a probe failed; emitted once per item, never per tick
//	sys.audio_stack    once: audio services, drivers, MMCSS network throttling
//	audio.endpoint     default render endpoint and active sessions, once and on change
//	net.ping_summary   every sysHealthInterval: ICMP RTT min/p50/p95/max/jitter/loss
//	net.ping_anomaly   one ICMP probe with RTT > sysPingSlow or lost (rate limited)
//
// Privacy: SSIDs and BSSIDs are logged only as truncated SHA-256 hashes, MACs
// only as their OUI, and no IP address other than the receiver's is recorded.
const (
	sysHealthInterval  = 5 * time.Second
	sysSlowInterval    = 15 * time.Second
	sysCommandTimeout  = 3 * time.Second
	sysClockInterval   = time.Second
	sysGapThreshold    = 2 * time.Second
	sysClockStep       = 5 * time.Millisecond
	sysPingInterval    = time.Second
	sysPingTimeout     = time.Second
	sysPingSlow        = 50 * time.Millisecond
	sysAnomalyRate     = 20 // anomaly records per second
	sysSchedInterval   = 10 * time.Millisecond
	sysSchedLate       = 5 * time.Millisecond
	sysStopWait        = 2 * time.Second
	sysPingPayloadSize = 32
)

// StartSystemDiag launches the host/network samplers and returns a stop func
// that cancels them. stop waits at most sysStopWait for the goroutines; a probe
// stuck in a blocking OS call exits on its own once the call returns.
func StartSystemDiag(ctx context.Context, receiverAddr string) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	s := &sysDiag{
		receiver: sysReceiverHost(receiverAddr),
		unavail:  newSysUnavailable(),
	}
	var wg sync.WaitGroup
	run := func(name string, fn func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					s.unavail.report("goroutine."+name, fmt.Errorf("panic: %v", r))
				}
			}()
			fn(ctx)
		}()
	}
	run("clock", s.clockLoop)
	run("sched", s.sched.loop)
	run("ping", s.pingLoop)
	startSystemDiagPlatform(ctx, s, run)

	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(sysStopWait):
			}
		})
	}
}

type sysDiag struct {
	receiver string
	unavail  *sysUnavailable
	sched    sysSchedProbe
}

// sysReceiverHost strips a port and brackets from an address; the result is
// a host name or IP literal.
func sysReceiverHost(addr string) string {
	addr = strings.TrimSpace(addr)
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return strings.Trim(addr, "[]")
}

// sysResolveIPv4 returns the receiver's IPv4 address, resolving names with a
// bounded lookup.
func sysResolveIPv4(ctx context.Context, host string) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			return v4, nil
		}
		return nil, fmt.Errorf("receiver is IPv6; ICMPv4 only")
	}
	ctx, cancel := context.WithTimeout(ctx, sysCommandTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		if v4 := a.To4(); v4 != nil {
			return v4, nil
		}
	}
	return nil, fmt.Errorf("no IPv4 address for receiver")
}

// sysUnavailable emits one sys.unavailable record per item.
type sysUnavailable struct {
	mu   sync.Mutex
	seen map[string]bool
}

func newSysUnavailable() *sysUnavailable {
	return &sysUnavailable{seen: map[string]bool{}}
}

func (u *sysUnavailable) report(item string, err error) {
	u.mu.Lock()
	if u.seen[item] {
		u.mu.Unlock()
		return
	}
	u.seen[item] = true
	u.mu.Unlock()
	fields := map[string]any{"item": item}
	if err != nil {
		fields["error"] = err.Error()
	}
	diagEmit("sys.unavailable", fields)
}

// sysRedact returns a short stable hash for identifiers that must not appear in
// logs (SSID, BSSID) but whose changes matter. Empty stays empty.
func sysRedact(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("doubletake-sysdiag:" + s))
	return hex.EncodeToString(sum[:6])
}

// sysMACOUI keeps only the vendor prefix of a hardware address.
func sysMACOUI(mac []byte) string {
	if len(mac) < 3 {
		return ""
	}
	return fmt.Sprintf("%02x:%02x:%02x", mac[0], mac[1], mac[2])
}

func sysRound(v float64, places int) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	p := math.Pow10(places)
	return math.Round(v*p) / p
}

// sysPercentile returns the nearest-rank percentile of sorted values.
func sysPercentile(sorted []int64, p float64) int64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[n-1]
	}
	rank := int(math.Ceil(p / 100 * float64(n)))
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}

// sysRateLimiter is a token bucket; suppressed counts denied events so the
// next summary can report them.
type sysRateLimiter struct {
	mu         sync.Mutex
	rate       float64
	burst      float64
	tokens     float64
	last       time.Time
	suppressed uint64
}

func newSysRateLimiter(perSecond int) *sysRateLimiter {
	return &sysRateLimiter{rate: float64(perSecond), burst: float64(perSecond), tokens: float64(perSecond)}
}

func (l *sysRateLimiter) allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.last.IsZero() {
		elapsed := now.Sub(l.last).Seconds()
		if elapsed > 0 {
			l.tokens = math.Min(l.burst, l.tokens+elapsed*l.rate)
		}
	}
	l.last = now
	if l.tokens >= 1 {
		l.tokens--
		return true
	}
	l.suppressed++
	return false
}

func (l *sysRateLimiter) takeSuppressed() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.suppressed
	l.suppressed = 0
	return n
}

// sysPingWindow aggregates ICMP results for one summary interval.
type sysPingWindow struct {
	rtts       []int64 // microseconds
	lost       int
	jitterSum  int64
	jitterN    int
	prev       int64
	havePrev   bool
	sentTotal  uint64
	lostTotal  uint64
	slowTotal  uint64
	slowIv     int
	errorsByIv map[string]int
}

func (w *sysPingWindow) add(rtt time.Duration, ok bool, status string) {
	w.sentTotal++
	if !ok {
		w.lost++
		w.lostTotal++
		w.havePrev = false
		if status != "" {
			if w.errorsByIv == nil {
				w.errorsByIv = map[string]int{}
			}
			w.errorsByIv[status]++
		}
		return
	}
	us := rtt.Microseconds()
	w.rtts = append(w.rtts, us)
	if rtt > sysPingSlow {
		w.slowIv++
		w.slowTotal++
	}
	if w.havePrev {
		d := us - w.prev
		if d < 0 {
			d = -d
		}
		w.jitterSum += d
		w.jitterN++
	}
	w.prev, w.havePrev = us, true
}

// summary returns the interval statistics and resets the interval.
func (w *sysPingWindow) summary() map[string]any {
	sorted := append([]int64(nil), w.rtts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	f := map[string]any{
		"replies_iv":   len(sorted),
		"lost_iv":      w.lost,
		"slow_iv":      w.slowIv,
		"sent_total":   w.sentTotal,
		"lost_total":   w.lostTotal,
		"slow_total":   w.slowTotal,
		"slow_over_ms": sysPingSlow.Milliseconds(),
	}
	if len(sorted) > 0 {
		f["min_us"] = sorted[0]
		f["p50_us"] = sysPercentile(sorted, 50)
		f["p95_us"] = sysPercentile(sorted, 95)
		f["max_us"] = sorted[len(sorted)-1]
	}
	if w.jitterN > 0 {
		f["jitter_us"] = w.jitterSum / int64(w.jitterN)
	}
	if len(w.errorsByIv) > 0 {
		f["loss_status_iv"] = w.errorsByIv
	}
	w.rtts = w.rtts[:0]
	w.lost, w.slowIv, w.jitterSum, w.jitterN = 0, 0, 0, 0
	w.errorsByIv = nil
	return f
}

// sysPinger is the platform ICMP echo primitive.
type sysPinger interface {
	// ping sends one echo and returns the measured RTT, whether a reply
	// arrived, and a short status for failures.
	ping() (time.Duration, bool, string)
	close()
}

func (s *sysDiag) pingLoop(ctx context.Context) {
	ip, err := sysResolveIPv4(ctx, s.receiver)
	if err != nil {
		s.unavail.report("net.ping", err)
		return
	}
	p, err := newSysPinger(ip)
	if err != nil {
		s.unavail.report("net.ping", err)
		return
	}
	defer p.close()

	limiter := newSysRateLimiter(sysAnomalyRate)
	var w sysPingWindow
	var seq uint64
	ticker := time.NewTicker(sysPingInterval)
	defer ticker.Stop()
	windowStart := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		seq++
		rtt, ok, status := p.ping()
		now := time.Now()
		w.add(rtt, ok, status)
		if (!ok || rtt > sysPingSlow) && limiter.allow(now) {
			f := map[string]any{"seq": seq, "lost": !ok, "target": ip.String()}
			if ok {
				f["rtt_us"] = rtt.Microseconds()
			} else {
				f["status"] = status
			}
			diagEmit("net.ping_anomaly", f)
		}
		if now.Sub(windowStart) >= sysHealthInterval {
			f := w.summary()
			f["target"] = ip.String()
			f["interval_ms"] = now.Sub(windowStart).Milliseconds()
			if n := limiter.takeSuppressed(); n > 0 {
				f["anomalies_suppressed_iv"] = n
			}
			diagEmit("net.ping_summary", f)
			windowStart = now
		}
	}
}

// sysClockWatch detects sampler gaps (sleep/resume, process stalls) and wall
// clock steps relative to the monotonic clock.
type sysClockWatch struct {
	prevWall time.Time
	prevMono time.Duration
	have     bool
}

// observe returns the monotonic elapsed time since the previous observation
// and the wall-minus-monotonic step over the same span.
func (c *sysClockWatch) observe(wall time.Time, mono time.Duration) (elapsed, step time.Duration, ok bool) {
	if !c.have {
		c.prevWall, c.prevMono, c.have = wall, mono, true
		return 0, 0, false
	}
	elapsed = mono - c.prevMono
	step = wall.Sub(c.prevWall) - elapsed
	c.prevWall, c.prevMono = wall, mono
	return elapsed, step, true
}

func (s *sysDiag) clockLoop(ctx context.Context) {
	var cw sysClockWatch
	cw.observe(sysWallNow(), bootRelativeNow())
	ticker := time.NewTicker(sysClockInterval)
	defer ticker.Stop()
	var stepTotal time.Duration
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		elapsed, step, ok := cw.observe(sysWallNow(), bootRelativeNow())
		if !ok {
			continue
		}
		if elapsed > sysClockInterval+sysGapThreshold {
			diagEmit("sys.sampler_gap", map[string]any{
				"gap_ms":      elapsed.Milliseconds(),
				"expected_ms": sysClockInterval.Milliseconds(),
			})
		}
		if step > sysClockStep || step < -sysClockStep {
			stepTotal += step
			diagEmit("sys.event", map[string]any{
				"event":             "clock_step",
				"step_us":           step.Microseconds(),
				"mono_elapsed_ms":   elapsed.Milliseconds(),
				"step_sum_total_us": stepTotal.Microseconds(),
			})
		}
	}
}

// sysSchedProbe measures how late a short sleep wakes up: a host-wide
// scheduling or DPC stall shows here even when the audio loop is idle.
type sysSchedProbe struct {
	mu   sync.Mutex
	late []int64 // microseconds
	over int
}

func (p *sysSchedProbe) loop(ctx context.Context) {
	timer := time.NewTimer(sysSchedInterval)
	defer timer.Stop()
	for {
		start := time.Now()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		late := time.Since(start) - sysSchedInterval
		if late < 0 {
			late = 0
		}
		p.mu.Lock()
		if len(p.late) < 4096 {
			p.late = append(p.late, late.Microseconds())
		}
		if late > sysSchedLate {
			p.over++
		}
		p.mu.Unlock()
		timer.Reset(sysSchedInterval)
	}
}

func (p *sysSchedProbe) take(f map[string]any) {
	p.mu.Lock()
	vals := p.late
	over := p.over
	p.late = make([]int64, 0, 512)
	p.over = 0
	p.mu.Unlock()
	if len(vals) == 0 {
		return
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	f["sched_late_p50_us"] = sysPercentile(vals, 50)
	f["sched_late_p99_us"] = sysPercentile(vals, 99)
	f["sched_late_max_us"] = vals[len(vals)-1]
	f["sched_late_over_5ms_iv"] = over
	f["sched_samples_iv"] = len(vals)
}

// sysWifiInfo is one Wi-Fi interface as reported by netsh or the WLAN API.
// SSID and BSSID are raw here and must be passed through sysRedact before
// they reach a record.
type sysWifiInfo struct {
	Name        string
	Description string
	GUID        string
	State       string
	SSID        string
	BSSID       string
	Band        string
	Channel     int
	RadioType   string
	SignalPct   int
	RxMbps      float64
	TxMbps      float64
	HaveSignal  bool
	HaveRates   bool
	HaveChannel bool
}

var (
	sysMACRe     = regexp.MustCompile(`^[0-9a-fA-F]{2}([:-][0-9a-fA-F]{2}){5}$`)
	sysGUIDRe    = regexp.MustCompile(`^\{?[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\}?$`)
	sysSignalRe  = regexp.MustCompile(`^(\d{1,3})\s*%$`)
	sysBandRe    = regexp.MustCompile(`(?i)^\d+(?:[.,]\d+)?\s*GHz$`)
	sysRadioRe   = regexp.MustCompile(`(?i)^802\.11\s*[a-z0-9]+$`)
	sysRateKeyRe = regexp.MustCompile(`(?i)\((mbps|mbit/s|mb/s|mbit/s\.?)\)`)
	sysNumberRe  = regexp.MustCompile(`^\d+(?:[.,]\d+)?$`)
)

// parseNetshWlanInterfaces parses `netsh wlan show interfaces` output. Labels
// are localized (and on German Windows arrive in the OEM code page, so
// umlauts may be mangled), so each field is recognized by value shape first
// and by a set of known labels second:
//   - GUID, BSSID, band, radio type and signal have distinctive value shapes;
//   - the two rate lines carry a "(Mbps)"/"(MBit/s)" unit and are always
//     ordered receive then transmit;
//   - SSID, channel, state and name use known English/German labels.
func parseNetshWlanInterfaces(out string) []sysWifiInfo {
	var list []sysWifiInfo
	var cur *sysWifiInfo
	rateLines := 0
	flush := func() {
		if cur != nil && (cur.GUID != "" || cur.Name != "" || cur.HaveSignal || cur.SSID != "") {
			list = append(list, *cur)
		}
		cur = nil
	}
	for _, raw := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		line := strings.TrimSpace(raw)
		idx := strings.Index(line, " : ")
		var key, val string
		switch {
		case idx > 0:
			key, val = strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx+3:])
		case strings.HasSuffix(line, " :"):
			key = strings.TrimSpace(strings.TrimSuffix(line, ":"))
		default:
			continue
		}
		lkey := strings.ToLower(key)
		if lkey == "name" {
			flush()
			cur = &sysWifiInfo{Name: val}
			rateLines = 0
			continue
		}
		if cur == nil {
			cur = &sysWifiInfo{}
			rateLines = 0
		}
		switch {
		case lkey == "ssid":
			cur.SSID = val
		case strings.Contains(lkey, "bssid") && sysMACRe.MatchString(val):
			cur.BSSID = strings.ToLower(strings.ReplaceAll(val, "-", ":"))
		case sysMACRe.MatchString(val):
			// Physical address of the local adapter: never recorded.
		case sysGUIDRe.MatchString(val):
			cur.GUID = strings.ToLower(strings.Trim(val, "{}"))
		case sysSignalRe.MatchString(val):
			m := sysSignalRe.FindStringSubmatch(val)
			cur.SignalPct, _ = strconv.Atoi(m[1])
			cur.HaveSignal = true
		case sysBandRe.MatchString(val):
			cur.Band = strings.ReplaceAll(val, ",", ".")
		case sysRadioRe.MatchString(val):
			cur.RadioType = strings.ReplaceAll(val, " ", "")
		case sysRateKeyRe.MatchString(key) && sysNumberRe.MatchString(val):
			v, _ := strconv.ParseFloat(strings.ReplaceAll(val, ",", "."), 64)
			isRx := strings.Contains(lkey, "receive") || strings.Contains(lkey, "empfang")
			isTx := strings.Contains(lkey, "transmit") || strings.Contains(lkey, "bertragung") || strings.Contains(lkey, "sende")
			if !isRx && !isTx {
				isRx = rateLines == 0
				isTx = !isRx
			}
			if isRx {
				cur.RxMbps = v
			} else {
				cur.TxMbps = v
			}
			cur.HaveRates = true
			rateLines++
		case lkey == "channel" || lkey == "kanal" || lkey == "canal" || lkey == "canale":
			if n, err := strconv.Atoi(val); err == nil {
				cur.Channel, cur.HaveChannel = n, true
			}
		case lkey == "state" || lkey == "status" || lkey == "zustand":
			cur.State = val
		case lkey == "description" || lkey == "beschreibung":
			cur.Description = val
		}
	}
	flush()
	return list
}

// sysWifiFields renders the redacted Wi-Fi view used in records.
func sysWifiFields(w sysWifiInfo) map[string]any {
	f := map[string]any{}
	if w.SSID != "" {
		f["ssid_hash"] = sysRedact(w.SSID)
	}
	if w.BSSID != "" {
		f["bssid_hash"] = sysRedact(w.BSSID)
	}
	if w.Band != "" {
		f["band"] = w.Band
	}
	if w.HaveChannel {
		f["channel"] = w.Channel
		if w.Band == "" {
			f["band_guess"] = sysBandFromChannel(w.Channel)
		}
	}
	if w.RadioType != "" {
		f["phy"] = w.RadioType
	}
	if w.State != "" {
		f["state"] = w.State
	}
	if w.HaveSignal {
		f["signal_pct"] = w.SignalPct
	}
	if w.HaveRates {
		f["rx_rate_mbps"] = w.RxMbps
		f["tx_rate_mbps"] = w.TxMbps
	}
	return f
}

// sysBandFromChannel is a guess only: 6 GHz channel numbers overlap 2.4/5 GHz.
func sysBandFromChannel(ch int) string {
	switch {
	case ch >= 1 && ch <= 14:
		return "2.4 GHz"
	case ch >= 32 && ch <= 177:
		return "5 GHz"
	default:
		return "unknown"
	}
}

// parseNvidiaSmi parses one line of
// nvidia-smi --query-gpu=utilization.gpu,memory.used,memory.total,clocks.gr,temperature.gpu,pstate --format=csv,noheader,nounits
func parseNvidiaSmi(out string) (map[string]any, bool) {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])
	parts := strings.Split(line, ",")
	if len(parts) < 5 {
		return nil, false
	}
	num := func(s string) (float64, bool) {
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		return v, err == nil
	}
	f := map[string]any{}
	keys := []string{"gpu_util_pct", "gpu_mem_used_mb", "gpu_mem_total_mb", "gpu_clock_mhz", "gpu_temp_c"}
	found := false
	for i, k := range keys {
		if v, ok := num(parts[i]); ok {
			f[k] = v
			found = true
		}
	}
	if len(parts) > 5 {
		f["gpu_pstate"] = strings.TrimSpace(parts[5])
	}
	return f, found
}

// sysPdhInstanceName maps an adapter description to its PDH "Network
// Interface" instance name, which replaces ( ) # / \ characters.
func sysPdhInstanceName(desc string) string {
	r := strings.NewReplacer("(", "[", ")", "]", "#", "_", "/", "_", `\`, "_")
	return strings.ToLower(r.Replace(desc))
}
