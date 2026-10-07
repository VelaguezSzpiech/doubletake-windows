//go:build windows

package airplay

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	sysNtdll                      = windows.NewLazySystemDLL("ntdll.dll")
	procNtQueryTimerResolution    = sysNtdll.NewProc("NtQueryTimerResolution")
	sysPowrprof                   = windows.NewLazySystemDLL("powrprof.dll")
	procPowerGetActiveScheme      = sysPowrprof.NewProc("PowerGetActiveScheme")
	procPowerReadFriendlyName     = sysPowrprof.NewProc("PowerReadFriendlyName")
	procPowerGetEffectiveOverlay  = sysPowrprof.NewProc("PowerGetEffectiveOverlayScheme")
	sysKernel32                   = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx      = sysKernel32.NewProc("GlobalMemoryStatusEx")
	procK32GetProcessMemoryInfo   = sysKernel32.NewProc("K32GetProcessMemoryInfo")
	procGetProcessHandleCount     = sysKernel32.NewProc("GetProcessHandleCount")
	procGetSystemPowerStatus      = sysKernel32.NewProc("GetSystemPowerStatus")
	procQueryPerformanceCounter   = sysKernel32.NewProc("QueryPerformanceCounter")
	procQueryPerformanceFrequency = sysKernel32.NewProc("QueryPerformanceFrequency")
	sysIphlpapi                   = windows.NewLazySystemDLL("iphlpapi.dll")
	procIcmpCreateFile            = sysIphlpapi.NewProc("IcmpCreateFile")
	procIcmpSendEcho              = sysIphlpapi.NewProc("IcmpSendEcho")
	procIcmpCloseHandle           = sysIphlpapi.NewProc("IcmpCloseHandle")
	sysPdhDLL                     = windows.NewLazySystemDLL("pdh.dll")
	procPdhOpenQuery              = sysPdhDLL.NewProc("PdhOpenQueryW")
	procPdhAddEnglishCounter      = sysPdhDLL.NewProc("PdhAddEnglishCounterW")
	procPdhCollectQueryData       = sysPdhDLL.NewProc("PdhCollectQueryData")
	procPdhGetFormattedValue      = sysPdhDLL.NewProc("PdhGetFormattedCounterValue")
	procPdhGetFormattedArray      = sysPdhDLL.NewProc("PdhGetFormattedCounterArrayW")
	procPdhCloseQuery             = sysPdhDLL.NewProc("PdhCloseQuery")
	sysWlanapi                    = windows.NewLazySystemDLL("wlanapi.dll")
	procWlanOpenHandle            = sysWlanapi.NewProc("WlanOpenHandle")
	procWlanCloseHandle           = sysWlanapi.NewProc("WlanCloseHandle")
	procWlanEnumInterfaces        = sysWlanapi.NewProc("WlanEnumInterfaces")
	procWlanQueryInterface        = sysWlanapi.NewProc("WlanQueryInterface")
	procWlanFreeMemory            = sysWlanapi.NewProc("WlanFreeMemory")
	sysOle32                      = windows.NewLazySystemDLL("ole32.dll")
	procCoCreateInstance          = sysOle32.NewProc("CoCreateInstance")
	procPropVariantClear          = sysOle32.NewProc("PropVariantClear")
)

func sysWallNow() time.Time {
	var ft windows.Filetime
	windows.GetSystemTimePreciseAsFileTime(&ft)
	return time.Unix(0, ft.Nanoseconds())
}

// sysWinState is shared between the Windows sampler goroutines.
type sysWinState struct {
	*sysDiag
	nvidiaSmi string

	mu        sync.Mutex
	gpu       map[string]any
	gpuAt     time.Time
	netshWifi *sysWifiInfo
	netshAt   time.Time

	// Set by the health loop: the routing NIC is Wi-Fi and the WLAN API
	// cannot describe it, so the slow loop polls netsh instead.
	needNetsh atomic.Bool
	wifiGUID  atomic.Value // string, lower-case, no braces
}

func startSystemDiagPlatform(ctx context.Context, s *sysDiag, run func(string, func(context.Context))) {
	w := &sysWinState{sysDiag: s}
	if p, err := exec.LookPath("nvidia-smi"); err == nil {
		w.nvidiaSmi = p
	}
	w.wifiGUID.Store("")
	run("health", w.healthLoop)
	run("slow", w.slowLoop)
	run("audio", w.audioLoop)
}

// ---------------------------------------------------------------- commands

func sysRunCommand(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, sysCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return string(out), fmt.Errorf("%s: %w", filepath.Base(name), ctx.Err())
	}
	return string(out), err
}

func sysNetshWifi(ctx context.Context, guid string) (*sysWifiInfo, error) {
	out, err := sysRunCommand(ctx, "netsh", "wlan", "show", "interfaces")
	list := parseNetshWlanInterfaces(out)
	if len(list) == 0 {
		if err == nil {
			err = errors.New("netsh reported no WLAN interface")
		}
		return nil, err
	}
	for i := range list {
		if guid != "" && list[i].GUID == guid {
			return &list[i], nil
		}
	}
	return &list[0], nil
}

// slowLoop runs the subprocess probes, one command at a time, every
// sysSlowInterval with a sysCommandTimeout each.
func (w *sysWinState) slowLoop(ctx context.Context) {
	ticker := time.NewTicker(sysSlowInterval)
	defer ticker.Stop()
	for {
		if w.nvidiaSmi != "" {
			out, err := sysRunCommand(ctx, w.nvidiaSmi,
				"--query-gpu=utilization.gpu,memory.used,memory.total,clocks.gr,temperature.gpu,pstate",
				"--format=csv,noheader,nounits")
			if f, ok := parseNvidiaSmi(out); ok && err == nil {
				w.mu.Lock()
				w.gpu, w.gpuAt = f, time.Now()
				w.mu.Unlock()
			} else if ctx.Err() == nil {
				w.unavail.report("gpu.nvidia_smi", fmt.Errorf("query failed: %v", err))
			}
		}
		if w.needNetsh.Load() && ctx.Err() == nil {
			guid, _ := w.wifiGUID.Load().(string)
			info, err := sysNetshWifi(ctx, guid)
			if err != nil {
				w.unavail.report("wifi.netsh", err)
			} else {
				w.mu.Lock()
				w.netshWifi, w.netshAt = info, time.Now()
				w.mu.Unlock()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ---------------------------------------------------------------- host facts

func sysTimerResolution() (coarse, fine, cur uint32, err error) {
	if err = procNtQueryTimerResolution.Find(); err != nil {
		return
	}
	r, _, _ := procNtQueryTimerResolution.Call(
		uintptr(unsafe.Pointer(&coarse)), uintptr(unsafe.Pointer(&fine)), uintptr(unsafe.Pointer(&cur)))
	if r != 0 {
		err = fmt.Errorf("NtQueryTimerResolution NTSTATUS 0x%x", r)
	}
	return
}

func sysPowerPlan() (guid, name string, err error) {
	if err = procPowerGetActiveScheme.Find(); err != nil {
		return
	}
	var p *windows.GUID
	if r, _, _ := procPowerGetActiveScheme.Call(0, uintptr(unsafe.Pointer(&p))); r != 0 || p == nil {
		return "", "", fmt.Errorf("PowerGetActiveScheme: %w", syscall.Errno(r))
	}
	defer windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(p))))
	guid = strings.ToLower(strings.Trim(p.String(), "{}"))
	if procPowerReadFriendlyName.Find() == nil {
		var size uint32
		procPowerReadFriendlyName.Call(0, uintptr(unsafe.Pointer(p)), 0, 0, 0, uintptr(unsafe.Pointer(&size)))
		if size > 0 && size < 4096 {
			buf := make([]uint16, size/2+1)
			if r, _, _ := procPowerReadFriendlyName.Call(0, uintptr(unsafe.Pointer(p)), 0, 0,
				uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r == 0 {
				name = windows.UTF16ToString(buf)
			}
		}
	}
	return guid, name, nil
}

// sysPowerOverlay returns the Windows 10/11 power mode slider overlay GUID
// (best performance / balanced / best efficiency).
func sysPowerOverlay() (string, bool) {
	if procPowerGetEffectiveOverlay.Find() != nil {
		return "", false
	}
	var g windows.GUID
	if r, _, _ := procPowerGetEffectiveOverlay.Call(uintptr(unsafe.Pointer(&g))); r != 0 {
		return "", false
	}
	return strings.ToLower(strings.Trim(g.String(), "{}")), true
}

type sysPowerStatus struct {
	ACLineStatus        byte
	BatteryFlag         byte
	BatteryLifePercent  byte
	SystemStatusFlag    byte
	BatteryLifeTime     uint32
	BatteryFullLifeTime uint32
}

func sysPowerSource() (ac int, saver bool, battery int, ok bool) {
	if procGetSystemPowerStatus.Find() != nil {
		return 0, false, 0, false
	}
	var ps sysPowerStatus
	if r, _, _ := procGetSystemPowerStatus.Call(uintptr(unsafe.Pointer(&ps))); r == 0 {
		return 0, false, 0, false
	}
	return int(ps.ACLineStatus), ps.SystemStatusFlag == 1, int(ps.BatteryLifePercent), true
}

type sysMemoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func sysGlobalMemory() (sysMemoryStatusEx, error) {
	m := sysMemoryStatusEx{Length: uint32(unsafe.Sizeof(sysMemoryStatusEx{}))}
	if err := procGlobalMemoryStatusEx.Find(); err != nil {
		return m, err
	}
	if r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return m, err
	}
	return m, nil
}

func sysRegString(root registry.Key, path, name string) string {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	if s, _, err := k.GetStringValue(name); err == nil {
		return strings.TrimSpace(s)
	}
	if n, _, err := k.GetIntegerValue(name); err == nil {
		return fmt.Sprint(n)
	}
	return ""
}

func sysRegInt(root registry.Key, path, name string) (uint64, bool) {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		return 0, false
	}
	defer k.Close()
	n, _, err := k.GetIntegerValue(name)
	return n, err == nil
}

var sysPriorityNames = map[uint32]string{
	windows.IDLE_PRIORITY_CLASS:         "idle",
	windows.BELOW_NORMAL_PRIORITY_CLASS: "below_normal",
	windows.NORMAL_PRIORITY_CLASS:       "normal",
	windows.ABOVE_NORMAL_PRIORITY_CLASS: "above_normal",
	windows.HIGH_PRIORITY_CLASS:         "high",
	windows.REALTIME_PRIORITY_CLASS:     "realtime",
}

// ---------------------------------------------------------------- NICs

type sysNIC struct {
	index  uint32
	guid   string
	name   string
	desc   string
	ifType uint32
	up     bool
	txMbps float64
	rxMbps float64
	oui    string
	mtu    uint32
}

func sysIfTypeName(t uint32) string {
	switch t {
	case windows.IF_TYPE_ETHERNET_CSMACD:
		return "ethernet"
	case windows.IF_TYPE_IEEE80211:
		return "wifi"
	case windows.IF_TYPE_SOFTWARE_LOOPBACK:
		return "loopback"
	case windows.IF_TYPE_TUNNEL:
		return "tunnel"
	case windows.IF_TYPE_PPP:
		return "ppp"
	case 243, 244:
		return "wwan"
	default:
		return fmt.Sprintf("type_%d", t)
	}
}

func sysLinkMbps(bps uint64) float64 {
	if bps == 0 || bps == math.MaxUint64 {
		return 0
	}
	return sysRound(float64(bps)/1e6, 1)
}

func sysAdapters() ([]sysNIC, error) {
	size := uint32(16 * 1024)
	var buf []byte
	for i := 0; i < 4; i++ {
		buf = make([]byte, size)
		err := windows.GetAdaptersAddresses(windows.AF_UNSPEC,
			windows.GAA_FLAG_SKIP_ANYCAST|windows.GAA_FLAG_SKIP_MULTICAST|windows.GAA_FLAG_SKIP_DNS_SERVER,
			0, (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0])), &size)
		if err == nil {
			break
		}
		if err != windows.ERROR_BUFFER_OVERFLOW {
			return nil, err
		}
		buf = nil
	}
	if buf == nil {
		return nil, errors.New("GetAdaptersAddresses: buffer overflow")
	}
	var list []sysNIC
	for a := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0])); a != nil; a = a.Next {
		if a.IfType == windows.IF_TYPE_SOFTWARE_LOOPBACK {
			continue
		}
		n := sysNIC{
			index:  a.IfIndex,
			guid:   strings.ToLower(strings.Trim(windows.BytePtrToString(a.AdapterName), "{}")),
			name:   windows.UTF16PtrToString(a.FriendlyName),
			desc:   windows.UTF16PtrToString(a.Description),
			ifType: a.IfType,
			up:     a.OperStatus == windows.IfOperStatusUp,
			txMbps: sysLinkMbps(a.TransmitLinkSpeed),
			rxMbps: sysLinkMbps(a.ReceiveLinkSpeed),
			mtu:    a.Mtu,
		}
		if a.PhysicalAddressLength >= 3 {
			n.oui = sysMACOUI(a.PhysicalAddress[:a.PhysicalAddressLength])
		}
		list = append(list, n)
	}
	return list, nil
}

func sysRouteIfIndex(ip net.IP) (uint32, error) {
	v4 := ip.To4()
	if v4 == nil {
		return 0, errors.New("not IPv4")
	}
	sa := &windows.SockaddrInet4{}
	copy(sa.Addr[:], v4)
	var idx uint32
	err := windows.GetBestInterfaceEx(sa, &idx)
	return idx, err
}

func sysNICFields(n sysNIC, route bool) map[string]any {
	f := map[string]any{
		"name":         n.name,
		"desc":         n.desc,
		"type":         sysIfTypeName(n.ifType),
		"up":           n.up,
		"link_tx_mbps": n.txMbps,
		"link_rx_mbps": n.rxMbps,
		"mtu":          n.mtu,
	}
	if n.oui != "" {
		f["mac_oui"] = n.oui
	}
	if route {
		f["routes_to_receiver"] = true
	}
	return f
}

// ---------------------------------------------------------------- PDH

const (
	sysPdhFmtDouble  = 0x00000200
	sysPdhFmtNoCap   = 0x00008000
	sysPdhMoreData   = 0x800007D2
	sysPdhItemSize   = 24 // PDH_FMT_COUNTERVALUE_ITEM_W: name ptr + 8-aligned {CStatus, double}
	sysPdhValueBytes = 16 // PDH_FMT_COUNTERVALUE
)

type sysPdh struct {
	q        uintptr
	counters map[string]uintptr
	unavail  *sysUnavailable
}

func newSysPdh(un *sysUnavailable) (*sysPdh, error) {
	if err := procPdhOpenQuery.Find(); err != nil {
		return nil, err
	}
	p := &sysPdh{counters: map[string]uintptr{}, unavail: un}
	if r, _, _ := procPdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&p.q))); r != 0 {
		return nil, fmt.Errorf("PdhOpenQuery 0x%x", r)
	}
	return p, nil
}

func (p *sysPdh) add(path string) {
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	var c uintptr
	if r, _, _ := procPdhAddEnglishCounter.Call(p.q, uintptr(unsafe.Pointer(ptr)), 0, uintptr(unsafe.Pointer(&c))); r != 0 {
		p.unavail.report("pdh:"+path, fmt.Errorf("PdhAddEnglishCounter 0x%x", r))
		return
	}
	p.counters[path] = c
}

func (p *sysPdh) collect() error {
	if r, _, _ := procPdhCollectQueryData.Call(p.q); r != 0 {
		return fmt.Errorf("PdhCollectQueryData 0x%x", r)
	}
	return nil
}

func (p *sysPdh) value(path string) (float64, bool) {
	c, ok := p.counters[path]
	if !ok {
		return 0, false
	}
	var v [sysPdhValueBytes / 8]uint64
	r, _, _ := procPdhGetFormattedValue.Call(c, sysPdhFmtDouble|sysPdhFmtNoCap, 0, uintptr(unsafe.Pointer(&v[0])))
	if r != 0 {
		return 0, false
	}
	if status := uint32(v[0]); status > 1 {
		return 0, false
	}
	return math.Float64frombits(v[1]), true
}

func (p *sysPdh) array(path string) map[string]float64 {
	c, ok := p.counters[path]
	if !ok {
		return nil
	}
	var size, count uint32
	r, _, _ := procPdhGetFormattedArray.Call(c, sysPdhFmtDouble|sysPdhFmtNoCap,
		uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), 0)
	if uint32(r) != sysPdhMoreData || size == 0 {
		return nil
	}
	buf := make([]uint64, (size+7)/8)
	r, _, _ = procPdhGetFormattedArray.Call(c, sysPdhFmtDouble|sysPdhFmtNoCap,
		uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&buf[0])))
	if r != 0 {
		return nil
	}
	out := make(map[string]float64, count)
	base := unsafe.Pointer(&buf[0])
	for i := uint32(0); i < count; i++ {
		item := unsafe.Add(base, uintptr(i)*sysPdhItemSize)
		name := windows.UTF16PtrToString(*(**uint16)(item))
		status := *(*uint32)(unsafe.Add(item, 8))
		if status > 1 {
			continue
		}
		out[name] = *(*float64)(unsafe.Add(item, 16))
	}
	runtime.KeepAlive(buf)
	return out
}

func (p *sysPdh) close() {
	if p != nil && p.q != 0 {
		procPdhCloseQuery.Call(p.q)
	}
}

type sysPdhSingle struct {
	key    string
	path   string
	scale  float64
	places int
}

var sysPdhSingles = []sysPdhSingle{
	{"cpu_pct", `\Processor(_Total)\% Processor Time`, 1, 1},
	{"dpc_pct", `\Processor(_Total)\% DPC Time`, 1, 2},
	{"interrupt_pct", `\Processor(_Total)\% Interrupt Time`, 1, 2},
	{"dpcs_queued_per_s", `\Processor(_Total)\DPCs Queued/sec`, 1, 0},
	{"interrupts_per_s", `\Processor(_Total)\Interrupts/sec`, 1, 0},
	{"ctx_switches_per_s", `\System\Context Switches/sec`, 1, 0},
	{"proc_queue_len", `\System\Processor Queue Length`, 1, 0},
	{"cpu_perf_pct", `\Processor Information(_Total)\% Processor Performance`, 1, 1},
	{"cpu_freq_mhz", `\Processor Information(_Total)\Processor Frequency`, 1, 0},
	{"cpu_max_freq_pct", `\Processor Information(_Total)\% of Maximum Frequency`, 1, 1},
	{"mem_avail_mb", `\Memory\Available MBytes`, 1, 0},
	{"commit_mb", `\Memory\Committed Bytes`, 1.0 / (1 << 20), 0},
	{"commit_limit_mb", `\Memory\Commit Limit`, 1.0 / (1 << 20), 0},
	{"commit_pct", `\Memory\% Committed Bytes In Use`, 1, 1},
	{"page_faults_per_s", `\Memory\Page Faults/sec`, 1, 0},
	{"hard_faults_per_s", `\Memory\Pages Input/sec`, 1, 1},
	{"disk_queue_len", `\PhysicalDisk(_Total)\Current Disk Queue Length`, 1, 0},
	{"tcp_retrans_per_s", `\TCPv4\Segments Retransmitted/sec`, 1, 2},
	{"tcp_segments_per_s", `\TCPv4\Segments/sec`, 1, 0},
	{"udp_sent_per_s", `\UDPv4\Datagrams Sent/sec`, 1, 0},
	{"udp_no_port_per_s", `\UDPv4\Datagrams No Port/sec`, 1, 2},
	{"udp_rx_errors_total", `\UDPv4\Datagrams Received Errors`, 1, 0},
}

const (
	sysPdhCore    = `\Processor(*)\% Processor Time`
	sysPdhCoreDPC = `\Processor(*)\% DPC Time`
)

type sysPdhNet struct {
	key     string
	counter string
	scale   float64
	total   bool // raw cumulative counter: also report the interval delta
}

var sysPdhNets = []sysPdhNet{
	{"nic_tx_kb_per_s", "Bytes Sent/sec", 1.0 / 1024, false},
	{"nic_rx_kb_per_s", "Bytes Received/sec", 1.0 / 1024, false},
	{"nic_tx_pkts_per_s", "Packets Sent/sec", 1, false},
	{"nic_rx_pkts_per_s", "Packets Received/sec", 1, false},
	{"nic_out_queue_len", "Output Queue Length", 1, false},
	{"nic_rx_errors", "Packets Received Errors", 1, true},
	{"nic_tx_errors", "Packets Outbound Errors", 1, true},
	{"nic_rx_discards", "Packets Received Discarded", 1, true},
	{"nic_tx_discards", "Packets Outbound Discarded", 1, true},
}

func sysPdhNetPath(counter string) string { return `\Network Interface(*)\` + counter }

// ---------------------------------------------------------------- processes

type sysProcMemCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
	PrivateUsage               uintptr
}

type sysProcPrev struct {
	created uint64
	cpu     uint64 // 100 ns
	faults  uint32
	at      time.Time
}

type sysProcTracker struct {
	self    uint32
	selfExe string
	prev    map[uint32]sysProcPrev
	unavail *sysUnavailable
}

func sysProcWatched(name string, pid, ppid, self uint32, selfExe string) bool {
	n := strings.ToLower(name)
	return pid == self || ppid == self || n == selfExe ||
		n == "doubletake.exe" || n == "doubletake.tray.exe" ||
		strings.HasPrefix(n, "gst-launch-1.0") || n == "audiodg.exe"
}

type sysProcEntry struct {
	pid, ppid, threads uint32
	name               string
}

func sysProcessList() ([]sysProcEntry, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	var list []sysProcEntry
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		list = append(list, sysProcEntry{
			pid: e.ProcessID, ppid: e.ParentProcessID, threads: e.Threads,
			name: windows.UTF16ToString(e.ExeFile[:]),
		})
	}
	return list, nil
}

func (t *sysProcTracker) sample(now time.Time, procs []sysProcEntry, f map[string]any) {
	ncpu := float64(runtime.NumCPU())
	var out []map[string]any
	var ourCPU float64
	seen := map[uint32]bool{}
	for _, p := range procs {
		if !sysProcWatched(p.name, p.pid, p.ppid, t.self, t.selfExe) {
			continue
		}
		seen[p.pid] = true
		rec := map[string]any{"name": p.name, "pid": p.pid, "threads": p.threads}
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, p.pid)
		if err != nil {
			t.unavail.report("proc.open:"+strings.ToLower(p.name), err)
			out = append(out, rec)
			continue
		}
		var c, e, k, u windows.Filetime
		if windows.GetProcessTimes(h, &c, &e, &k, &u) == nil {
			cpu := uint64(k.Nanoseconds()/100) + uint64(u.Nanoseconds()/100)
			created := uint64(c.Nanoseconds())
			if prev, ok := t.prev[p.pid]; ok && prev.created == created && now.After(prev.at) && cpu >= prev.cpu {
				wall := float64(now.Sub(prev.at)) / 100
				pct := float64(cpu-prev.cpu) / wall / ncpu * 100
				rec["cpu_pct"] = sysRound(pct, 2)
				if !strings.EqualFold(p.name, "audiodg.exe") {
					ourCPU += pct
				}
			}
			pv := t.prev[p.pid]
			pv.created, pv.cpu = created, cpu
			t.prev[p.pid] = pv
		}
		var mc sysProcMemCounters
		mc.CB = uint32(unsafe.Sizeof(mc))
		if r, _, _ := procK32GetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&mc)), uintptr(mc.CB)); r != 0 {
			rec["ws_mb"] = sysRound(float64(mc.WorkingSetSize)/(1<<20), 1)
			rec["private_mb"] = sysRound(float64(mc.PrivateUsage)/(1<<20), 1)
			pv := t.prev[p.pid]
			if pv.faults != 0 && mc.PageFaultCount >= pv.faults {
				rec["page_faults_iv"] = mc.PageFaultCount - pv.faults
			}
			pv.faults = mc.PageFaultCount
			t.prev[p.pid] = pv
		}
		var handles uint32
		if r, _, _ := procGetProcessHandleCount.Call(uintptr(h), uintptr(unsafe.Pointer(&handles))); r != 0 {
			rec["handles"] = handles
		}
		windows.CloseHandle(h)
		pv := t.prev[p.pid]
		pv.at = now
		t.prev[p.pid] = pv
		out = append(out, rec)
	}
	for pid := range t.prev {
		if !seen[pid] {
			delete(t.prev, pid)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["pid"].(uint32) < out[j]["pid"].(uint32) })
	f["procs"] = out
	f["our_cpu_pct"] = sysRound(ourCPU, 2)
}

// ---------------------------------------------------------------- WLAN API

const (
	sysWlanOpCurrentConnection = 7
	sysWlanOpChannel           = 8
	sysWlanOpBackgroundScan    = 2
	sysWlanOpMediaStreaming    = 3
	sysWlanOpRSSI              = 0x10000102
)

type sysWlanConnAttrs struct {
	State      uint32
	Mode       uint32
	Profile    [256]uint16
	SSIDLen    uint32
	SSID       [32]byte
	BssType    uint32
	BSSID      [6]byte
	PhyType    uint32
	PhyIndex   uint32
	Signal     uint32
	RxRateKbps uint32
	TxRateKbps uint32
	SecEnabled int32
	OneX       int32
	Auth       uint32
	Cipher     uint32
}

var sysPhyNames = map[uint32]string{
	1: "fhss", 2: "dsss", 3: "irbaseband", 4: "802.11a", 5: "802.11b", 6: "802.11g",
	7: "802.11n", 8: "802.11ac", 9: "802.11ad", 10: "802.11ax", 11: "802.11be",
}

type sysWlan struct{ h uintptr }

func openSysWlan() (*sysWlan, error) {
	if err := procWlanOpenHandle.Find(); err != nil {
		return nil, err
	}
	var neg uint32
	w := &sysWlan{}
	if r, _, _ := procWlanOpenHandle.Call(2, 0, uintptr(unsafe.Pointer(&neg)), uintptr(unsafe.Pointer(&w.h))); r != 0 {
		return nil, fmt.Errorf("WlanOpenHandle: %w", syscall.Errno(r))
	}
	return w, nil
}

func (w *sysWlan) close() {
	if w != nil && w.h != 0 {
		procWlanCloseHandle.Call(w.h, 0)
	}
}

func (w *sysWlan) queryRaw(guid *windows.GUID, op uint32) (unsafe.Pointer, uint32, error) {
	var size uint32
	var data unsafe.Pointer
	r, _, _ := procWlanQueryInterface.Call(w.h, uintptr(unsafe.Pointer(guid)), uintptr(op), 0,
		uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&data)), 0)
	if r != 0 {
		return nil, 0, syscall.Errno(r)
	}
	return data, size, nil
}

func (w *sysWlan) queryU32(guid *windows.GUID, op uint32) (uint32, bool) {
	data, size, err := w.queryRaw(guid, op)
	if err != nil || data == nil {
		return 0, false
	}
	defer procWlanFreeMemory.Call(uintptr(data))
	if size < 4 {
		return 0, false
	}
	return *(*uint32)(data), true
}

// sample returns the association of the interface with the given GUID
// (lower-case, no braces) plus extra fields that netsh does not report.
func (w *sysWlan) sample(guidStr string) (sysWifiInfo, map[string]any, error) {
	var info sysWifiInfo
	guid, err := windows.GUIDFromString("{" + guidStr + "}")
	if err != nil {
		return info, nil, err
	}
	data, size, err := w.queryRaw(&guid, sysWlanOpCurrentConnection)
	if err != nil {
		return info, nil, fmt.Errorf("current_connection: %w", err)
	}
	if data == nil || uintptr(size) < unsafe.Sizeof(sysWlanConnAttrs{}) {
		if data != nil {
			procWlanFreeMemory.Call(uintptr(data))
		}
		return info, nil, errors.New("current_connection: short result")
	}
	a := *(*sysWlanConnAttrs)(data)
	procWlanFreeMemory.Call(uintptr(data))
	info.GUID = guidStr
	if a.State == 1 {
		info.State = "connected"
	} else {
		info.State = fmt.Sprintf("state_%d", a.State)
	}
	if n := int(a.SSIDLen); n > 0 && n <= len(a.SSID) {
		info.SSID = string(a.SSID[:n])
	}
	info.BSSID = net.HardwareAddr(a.BSSID[:]).String()
	info.RadioType = sysPhyNames[a.PhyType]
	info.SignalPct, info.HaveSignal = int(a.Signal), true
	info.RxMbps = sysRound(float64(a.RxRateKbps)/1000, 1)
	info.TxMbps = sysRound(float64(a.TxRateKbps)/1000, 1)
	info.HaveRates = true
	if ch, ok := w.queryU32(&guid, sysWlanOpChannel); ok {
		info.Channel, info.HaveChannel = int(ch), true
	}
	extra := map[string]any{}
	if v, ok := w.queryU32(&guid, sysWlanOpRSSI); ok {
		extra["rssi_dbm"] = int32(v)
	}
	if v, ok := w.queryU32(&guid, sysWlanOpBackgroundScan); ok {
		extra["background_scan"] = v != 0
	}
	if v, ok := w.queryU32(&guid, sysWlanOpMediaStreaming); ok {
		extra["media_streaming_mode"] = v != 0
	}
	return info, extra, nil
}

// ---------------------------------------------------------------- ICMP

var sysQPCFreq = func() int64 {
	var f int64
	if r, _, _ := procQueryPerformanceFrequency.Call(uintptr(unsafe.Pointer(&f))); r == 0 || f <= 0 {
		return 1
	}
	return f
}()

func sysQPCDuration(ticks int64) time.Duration {
	if ticks < 0 {
		ticks = 0
	}
	return time.Duration(float64(ticks) * float64(time.Second) / float64(sysQPCFreq))
}

type sysIcmp struct {
	h     uintptr
	dst   uint32
	req   [sysPingPayloadSize]byte
	reply [32]uint64 // ICMP_ECHO_REPLY + payload + ICMP error room, 8-aligned
}

func newSysPinger(ip net.IP) (sysPinger, error) {
	v4 := ip.To4()
	if v4 == nil {
		return nil, errors.New("not IPv4")
	}
	if err := procIcmpCreateFile.Find(); err != nil {
		return nil, err
	}
	h, _, err := procIcmpCreateFile.Call()
	if h == 0 || h == uintptr(windows.InvalidHandle) {
		return nil, fmt.Errorf("IcmpCreateFile: %v", err)
	}
	p := &sysIcmp{h: h}
	// IPAddr is the address in network byte order as laid out in memory.
	p.dst = *(*uint32)(unsafe.Pointer(&v4[0]))
	copy(p.req[:], "doubletake-sysdiag-ping-payload!")
	return p, nil
}

func (p *sysIcmp) ping() (time.Duration, bool, string) {
	// Go's runtime clock is quantized to the system timer resolution (0.5 ms
	// here), so measure the RTT with the performance counter.
	var q0, q1 int64
	procQueryPerformanceCounter.Call(uintptr(unsafe.Pointer(&q0)))
	n, _, err := procIcmpSendEcho.Call(p.h, uintptr(p.dst),
		uintptr(unsafe.Pointer(&p.req[0])), uintptr(len(p.req)), 0,
		uintptr(unsafe.Pointer(&p.reply[0])), unsafe.Sizeof(p.reply), uintptr(sysPingTimeout.Milliseconds()))
	procQueryPerformanceCounter.Call(uintptr(unsafe.Pointer(&q1)))
	rtt := sysQPCDuration(q1 - q0)
	if n == 0 {
		var errno syscall.Errno
		if errors.As(err, &errno) {
			if errno == 11010 { // IP_REQ_TIMED_OUT
				return rtt, false, "timeout"
			}
			return rtt, false, fmt.Sprintf("error_%d", uint32(errno))
		}
		return rtt, false, "error"
	}
	// ICMP_ECHO_REPLY: Address uint32, Status uint32, RoundTripTime uint32 ...
	if status := uint32(p.reply[0] >> 32); status != 0 {
		return rtt, false, fmt.Sprintf("ip_status_%d", status)
	}
	return rtt, true, ""
}

func (p *sysIcmp) close() {
	if p.h != 0 {
		procIcmpCloseHandle.Call(p.h)
	}
}

// ---------------------------------------------------------------- health loop

type sysHealthState struct {
	pdh        *sysPdh
	procs      sysProcTracker
	wlan       *sysWlan
	wlanErr    error
	receiverIP net.IP

	routeIdx     uint32
	routeGUID    string
	routeDesc    string
	routeWifi    bool
	links        map[uint32][2]float64
	ups          map[uint32]bool
	planGUID     string
	overlay      string
	ac           int
	saver        bool
	timerCur     uint32
	bssidHash    string
	channel      int
	wifiBand     string
	netPrev      map[string]float64
	udpErrPrev   float64
	haveUDPPrev  bool
	lastGPUEmit  time.Time
	lastNetshUse time.Time
}

func (w *sysWinState) healthLoop(ctx context.Context) {
	st := &sysHealthState{
		links:   map[uint32][2]float64{},
		ups:     map[uint32]bool{},
		netPrev: map[string]float64{},
		ac:      -1,
	}
	st.procs = sysProcTracker{
		self:    uint32(os.Getpid()),
		prev:    map[uint32]sysProcPrev{},
		unavail: w.unavail,
	}
	if exe, err := os.Executable(); err == nil {
		st.procs.selfExe = strings.ToLower(filepath.Base(exe))
	}
	if ip, err := sysResolveIPv4(ctx, w.receiver); err == nil {
		st.receiverIP = ip
	} else {
		w.unavail.report("net.route", err)
	}

	w.emitStart(ctx, st)

	if pdh, err := newSysPdh(w.unavail); err != nil {
		w.unavail.report("pdh", err)
	} else {
		st.pdh = pdh
		defer pdh.close()
		for _, c := range sysPdhSingles {
			pdh.add(c.path)
		}
		pdh.add(sysPdhCore)
		pdh.add(sysPdhCoreDPC)
		for _, c := range sysPdhNets {
			pdh.add(sysPdhNetPath(c.counter))
		}
		if err := pdh.collect(); err != nil {
			w.unavail.report("pdh.collect", err)
		}
	}
	// st.wlan may be opened later by sampleWifi, so close whatever is current.
	defer func() { st.wlan.close() }()

	ticker := time.NewTicker(sysHealthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		w.emitHealth(ctx, st)
	}
}

// emitStart collects the one-time host facts and the initial state that the
// health loop later diffs against.
func (w *sysWinState) emitStart(ctx context.Context, st *sysHealthState) {
	f := map[string]any{
		"go_version": runtime.Version(),
		"gomaxprocs": runtime.GOMAXPROCS(0),
		"cpu_cores":  runtime.NumCPU(),
		"receiver":   w.receiver,
	}
	const ntKey = `SOFTWARE\Microsoft\Windows NT\CurrentVersion`
	v := windows.RtlGetVersion()
	f["os_build"] = fmt.Sprintf("%d.%d.%d", v.MajorVersion, v.MinorVersion, v.BuildNumber)
	if ubr, ok := sysRegInt(registry.LOCAL_MACHINE, ntKey, "UBR"); ok {
		f["os_build"] = fmt.Sprintf("%d.%d.%d.%d", v.MajorVersion, v.MinorVersion, v.BuildNumber, ubr)
	}
	f["os_product"] = sysRegString(registry.LOCAL_MACHINE, ntKey, "ProductName")
	f["os_display_version"] = sysRegString(registry.LOCAL_MACHINE, ntKey, "DisplayVersion")
	const cpuKey = `HARDWARE\DESCRIPTION\System\CentralProcessor\0`
	f["cpu_model"] = sysRegString(registry.LOCAL_MACHINE, cpuKey, "ProcessorNameString")
	if mhz, ok := sysRegInt(registry.LOCAL_MACHINE, cpuKey, "~MHz"); ok {
		f["cpu_nominal_mhz"] = mhz
	}
	if m, err := sysGlobalMemory(); err == nil {
		f["ram_mb"] = m.TotalPhys >> 20
		f["ram_avail_mb"] = m.AvailPhys >> 20
		f["commit_limit_mb"] = m.TotalPageFile >> 20
	} else {
		w.unavail.report("sys.memory", err)
	}
	if guid, name, err := sysPowerPlan(); err == nil {
		f["power_plan_guid"], f["power_plan"] = guid, name
		st.planGUID = guid
	} else {
		w.unavail.report("sys.power_plan", err)
	}
	if ov, ok := sysPowerOverlay(); ok {
		f["power_mode_overlay_guid"] = ov
		st.overlay = ov
	}
	if ac, saver, batt, ok := sysPowerSource(); ok {
		f["ac_line"], f["battery_saver"], f["battery_pct"] = ac, saver, batt
		st.ac, st.saver = ac, saver
	}
	if coarse, fine, cur, err := sysTimerResolution(); err == nil {
		f["timer_res_current_us"] = sysRound(float64(cur)/10, 1)
		f["timer_res_finest_us"] = sysRound(float64(fine)/10, 1)
		f["timer_res_coarsest_us"] = sysRound(float64(coarse)/10, 1)
		st.timerCur = cur
	} else {
		w.unavail.report("sys.timer_resolution", err)
	}
	// By code inspection: doubletake never calls timeBeginPeriod, and the Go
	// runtime uses high-resolution waitable timers instead on Windows 10 1803+.
	// The current resolution above is system-wide (any process can raise it).
	f["process_raised_timer"] = false
	if pc, err := windows.GetPriorityClass(windows.CurrentProcess()); err == nil {
		f["priority_class"] = sysPriorityNames[pc]
	}

	nics, err := sysAdapters()
	if err != nil {
		w.unavail.report("net.adapters", err)
	}
	if st.receiverIP != nil {
		if idx, err := sysRouteIfIndex(st.receiverIP); err == nil {
			st.routeIdx = idx
		} else {
			w.unavail.report("net.route", err)
		}
	}
	var list []map[string]any
	for _, n := range nics {
		route := n.index == st.routeIdx && st.routeIdx != 0
		list = append(list, sysNICFields(n, route))
		st.links[n.index] = [2]float64{n.txMbps, n.rxMbps}
		st.ups[n.index] = n.up
		if route {
			st.routeGUID, st.routeDesc, st.routeWifi = n.guid, n.desc, n.ifType == windows.IF_TYPE_IEEE80211
			f["route_nic"] = n.name
			f["route_nic_type"] = sysIfTypeName(n.ifType)
			f["route_link_tx_mbps"], f["route_link_rx_mbps"] = n.txMbps, n.rxMbps
		}
	}
	f["nics"] = list
	f["route_is_wifi"] = st.routeWifi
	if st.routeWifi {
		f["wifi"] = w.startWifi(ctx, st)
	}
	diagEmit("sys.start", f)
}

// startWifi samples the routing Wi-Fi association once via the WLAN API and
// netsh (netsh adds the band, which the WLAN API cannot report).
func (w *sysWinState) startWifi(ctx context.Context, st *sysHealthState) map[string]any {
	w.wifiGUID.Store(st.routeGUID)
	var info sysWifiInfo
	var extra map[string]any
	st.wlan, st.wlanErr = openSysWlan()
	if st.wlanErr == nil {
		info, extra, st.wlanErr = st.wlan.sample(st.routeGUID)
	}
	if st.wlanErr != nil {
		w.unavail.report("wifi.wlanapi", st.wlanErr)
		w.needNetsh.Store(true)
	}
	if n, err := sysNetshWifi(ctx, st.routeGUID); err == nil {
		st.wifiBand = n.Band
		if st.wlanErr != nil {
			info = *n
		} else if info.Band == "" {
			info.Band = n.Band
		}
	} else {
		w.unavail.report("wifi.netsh", err)
	}
	f := sysWifiFields(info)
	for k, v := range extra {
		f[k] = v
	}
	st.bssidHash, st.channel = sysRedact(info.BSSID), info.Channel
	return f
}

func (w *sysWinState) emitHealth(ctx context.Context, st *sysHealthState) {
	now := time.Now()
	f := map[string]any{}

	if st.pdh != nil {
		if err := st.pdh.collect(); err != nil {
			w.unavail.report("pdh.collect", err)
		}
		for _, c := range sysPdhSingles {
			if v, ok := st.pdh.value(c.path); ok {
				f[c.key] = sysRound(v*c.scale, c.places)
				if c.key == "udp_rx_errors_total" {
					if st.haveUDPPrev && v >= st.udpErrPrev {
						f["udp_rx_errors_iv"] = v - st.udpErrPrev
					}
					st.udpErrPrev, st.haveUDPPrev = v, true
				}
			}
		}
		sysMaxInstance(st.pdh.array(sysPdhCore), f, "core_max_pct", "core_max_id")
		sysMaxInstance(st.pdh.array(sysPdhCoreDPC), f, "core_dpc_max_pct", "core_dpc_max_id")
		w.nicCounters(st, f)
	}

	if procs, err := sysProcessList(); err == nil {
		st.procs.sample(now, procs, f)
	} else {
		w.unavail.report("proc.snapshot", err)
	}

	if m, err := sysGlobalMemory(); err == nil {
		f["mem_load_pct"] = m.MemoryLoad
	}

	w.checkStateChanges(st, f)
	w.sampleWifi(st, f)
	w.sched.take(f)

	w.mu.Lock()
	if w.gpu != nil && now.Sub(w.gpuAt) < 2*sysSlowInterval {
		for k, v := range w.gpu {
			f[k] = v
		}
		f["gpu_age_ms"] = now.Sub(w.gpuAt).Milliseconds()
	}
	w.mu.Unlock()
	diagEmit("sys.health", f)
}

func sysMaxInstance(vals map[string]float64, f map[string]any, valKey, idKey string) {
	best, bestName := -1.0, ""
	for name, v := range vals {
		if name == "_Total" {
			continue
		}
		if v > best {
			best, bestName = v, name
		}
	}
	if bestName != "" {
		f[valKey] = sysRound(best, 1)
		f[idKey] = bestName
	}
}

func (w *sysWinState) nicCounters(st *sysHealthState, f map[string]any) {
	if st.routeDesc == "" {
		return
	}
	want := sysPdhInstanceName(st.routeDesc)
	matched := false
	for _, c := range sysPdhNets {
		vals := st.pdh.array(sysPdhNetPath(c.counter))
		for inst, v := range vals {
			if strings.ToLower(inst) != want {
				continue
			}
			matched = true
			v *= c.scale
			if c.total {
				f[c.key+"_total"] = v
				if prev, ok := st.netPrev[c.key]; ok && v >= prev {
					f[c.key+"_iv"] = v - prev
				}
				st.netPrev[c.key] = v
			} else {
				f[c.key] = sysRound(v, 1)
			}
		}
	}
	if !matched {
		w.unavail.report("pdh.network_interface", fmt.Errorf("no PDH instance for routing NIC %q", st.routeDesc))
	}
}

// checkStateChanges emits sys.event for power, timer, route and link changes.
func (w *sysWinState) checkStateChanges(st *sysHealthState, f map[string]any) {
	if guid, name, err := sysPowerPlan(); err == nil && guid != st.planGUID {
		diagEmit("sys.event", map[string]any{"event": "power_plan_change", "from_guid": st.planGUID, "to_guid": guid, "to_name": name})
		st.planGUID = guid
	}
	if ov, ok := sysPowerOverlay(); ok && ov != st.overlay {
		diagEmit("sys.event", map[string]any{"event": "power_mode_change", "from_guid": st.overlay, "to_guid": ov})
		st.overlay = ov
	}
	if ac, saver, batt, ok := sysPowerSource(); ok && (ac != st.ac || saver != st.saver) {
		diagEmit("sys.event", map[string]any{"event": "power_source_change", "ac_line": ac, "battery_saver": saver, "battery_pct": batt})
		st.ac, st.saver = ac, saver
	}
	if _, _, cur, err := sysTimerResolution(); err == nil {
		f["timer_res_current_us"] = sysRound(float64(cur)/10, 1)
		if cur != st.timerCur {
			diagEmit("sys.event", map[string]any{
				"event":   "timer_resolution_change",
				"from_us": sysRound(float64(st.timerCur)/10, 1), "to_us": sysRound(float64(cur)/10, 1),
			})
			st.timerCur = cur
		}
	}

	nics, err := sysAdapters()
	if err != nil {
		w.unavail.report("net.adapters", err)
		return
	}
	if st.receiverIP != nil {
		if idx, err := sysRouteIfIndex(st.receiverIP); err == nil && idx != st.routeIdx {
			ev := map[string]any{"event": "route_nic_change", "from_ifindex": st.routeIdx, "to_ifindex": idx}
			for _, n := range nics {
				if n.index == idx {
					ev["to_nic"], ev["to_type"] = n.name, sysIfTypeName(n.ifType)
					st.routeGUID, st.routeDesc, st.routeWifi = n.guid, n.desc, n.ifType == windows.IF_TYPE_IEEE80211
				}
			}
			diagEmit("sys.event", ev)
			st.routeIdx = idx
			w.wifiGUID.Store(st.routeGUID)
			if !st.routeWifi {
				w.needNetsh.Store(false)
			}
		}
	}
	for _, n := range nics {
		cur := [2]float64{n.txMbps, n.rxMbps}
		prev, known := st.links[n.index]
		if known && prev != cur && (n.ifType != windows.IF_TYPE_IEEE80211 || n.index != st.routeIdx) {
			// Wi-Fi link rate changes constantly; the routing Wi-Fi NIC's
			// rates are reported in sys.health instead of as events.
			diagEmit("sys.event", map[string]any{
				"event": "link_speed_change", "nic": n.name,
				"from_tx_mbps": prev[0], "to_tx_mbps": cur[0], "from_rx_mbps": prev[1], "to_rx_mbps": cur[1],
			})
		}
		if up, ok := st.ups[n.index]; ok && up != n.up {
			diagEmit("sys.event", map[string]any{"event": "link_state_change", "nic": n.name, "up": n.up})
		}
		st.links[n.index], st.ups[n.index] = cur, n.up
		if n.index == st.routeIdx {
			f["route_link_tx_mbps"], f["route_link_rx_mbps"] = n.txMbps, n.rxMbps
			f["route_up"] = n.up
		}
	}
}

func (w *sysWinState) sampleWifi(st *sysHealthState, f map[string]any) {
	if !st.routeWifi {
		return
	}
	var info sysWifiInfo
	var extra map[string]any
	have := false
	if st.wlan == nil && st.wlanErr == nil {
		st.wlan, st.wlanErr = openSysWlan()
	}
	if st.wlan != nil {
		var err error
		info, extra, err = st.wlan.sample(st.routeGUID)
		if err == nil {
			have = true
			w.needNetsh.Store(false)
		} else {
			w.unavail.report("wifi.wlanapi", err)
			w.needNetsh.Store(true)
		}
	} else {
		w.unavail.report("wifi.wlanapi", st.wlanErr)
		w.needNetsh.Store(true)
	}
	if !have {
		w.mu.Lock()
		if w.netshWifi != nil && time.Since(w.netshAt) < 2*sysSlowInterval {
			info, have = *w.netshWifi, true
			f["wifi_source"] = "netsh"
			f["wifi_age_ms"] = time.Since(w.netshAt).Milliseconds()
		}
		w.mu.Unlock()
	}
	if !have {
		return
	}
	if info.Band == "" {
		info.Band = st.wifiBand
	}
	for k, v := range sysWifiFields(info) {
		f["wifi_"+k] = v
	}
	for k, v := range extra {
		f["wifi_"+k] = v
	}
	if h := sysRedact(info.BSSID); h != "" && h != st.bssidHash {
		diagEmit("sys.event", map[string]any{"event": "wifi_roam", "from_bssid_hash": st.bssidHash, "to_bssid_hash": h, "channel": info.Channel, "signal_pct": info.SignalPct})
		st.bssidHash = h
	}
	if info.HaveChannel && info.Channel != st.channel {
		if st.channel != 0 {
			diagEmit("sys.event", map[string]any{"event": "wifi_channel_change", "from": st.channel, "to": info.Channel})
		}
		st.channel = info.Channel
	}
}

// ---------------------------------------------------------------- audio (COM)

var (
	sysCLSIDMMDeviceEnumerator = sysMustGUID("{BCDE0395-E52F-467C-8E3D-C4579291692E}")
	sysIIDMMDeviceEnumerator   = sysMustGUID("{A95664D2-9614-4F35-A746-DE8DB63617E6}")
	sysIIDAudioClient          = sysMustGUID("{1CB9AD4C-DBFA-4C32-B178-C2F568A703B2}")
	sysIIDAudioEndpointVolume  = sysMustGUID("{5CDF2C82-841E-4546-9722-0CF74078229A}")
	sysIIDAudioSessionManager2 = sysMustGUID("{77AA99A0-1BD6-484F-8BC7-2C654C9A9B6F}")
	sysIIDAudioSessionControl2 = sysMustGUID("{BFB7FF88-7239-4FC9-8FA2-07C950BE9C6D}")

	sysPKeyDeviceFriendlyName    = sysPropKey{sysMustGUID("{A45C254E-DF1C-4EFD-8020-67D146A850E0}"), 14}
	sysPKeyInterfaceFriendlyName = sysPropKey{sysMustGUID("{026E516E-B814-414B-83CD-856D6FEF4822}"), 2}
	sysPKeyDisableSysFx          = sysPropKey{sysMustGUID("{1DA5D803-D492-4EDD-8C23-E0C0FFEE7F0E}"), 5}
)

const (
	sysCLSCTXAll         = 0x17
	sysVTLPWSTR          = 31
	sysVTUI4             = 19
	sysVTBool            = 11
	sysVTI4              = 3
	sysAudioPollInterval = 5 * time.Second
)

func sysMustGUID(s string) windows.GUID {
	g, err := windows.GUIDFromString(s)
	if err != nil {
		panic(err)
	}
	return g
}

type sysPropKey struct {
	Fmtid windows.GUID
	Pid   uint32
}

type sysPropVariant struct {
	VT       uint16
	R1       uint16
	R2       uint16
	R3       uint16
	Val      unsafe.Pointer
	Reserved unsafe.Pointer
}

// sysCOM calls vtable slot method of a COM object and returns the HRESULT.
func sysCOM(obj unsafe.Pointer, method int, args ...uintptr) uint32 {
	vtbl := *(*unsafe.Pointer)(obj)
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(method)*unsafe.Sizeof(uintptr(0))))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(obj)}, args...)...)
	return uint32(r)
}

func sysRelease(obj unsafe.Pointer) {
	if obj != nil {
		sysCOM(obj, 2)
	}
}

func sysHRESULT(hr uint32) error { return fmt.Errorf("HRESULT 0x%08x", hr) }

func sysPropString(store unsafe.Pointer, key *sysPropKey) (string, bool) {
	var pv sysPropVariant
	if hr := sysCOM(store, 5, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&pv))); hr != 0 {
		return "", false
	}
	defer procPropVariantClear.Call(uintptr(unsafe.Pointer(&pv)))
	if pv.VT != sysVTLPWSTR || pv.Val == nil {
		return "", false
	}
	return windows.UTF16PtrToString((*uint16)(pv.Val)), true
}

func sysPropUint(store unsafe.Pointer, key *sysPropKey) (uint32, bool) {
	var pv sysPropVariant
	if hr := sysCOM(store, 5, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&pv))); hr != 0 {
		return 0, false
	}
	defer procPropVariantClear.Call(uintptr(unsafe.Pointer(&pv)))
	switch pv.VT {
	case sysVTUI4, sysVTI4:
		return *(*uint32)(unsafe.Pointer(&pv.Val)), true
	case sysVTBool:
		if *(*int16)(unsafe.Pointer(&pv.Val)) != 0 {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

type sysEndpoint struct {
	fields      map[string]any
	fingerprint string
}

// sysReadEndpoint describes the default render (console) endpoint. Must run
// on a thread with COM initialized.
func sysReadEndpoint(names map[uint32]string, drivers []map[string]any) (*sysEndpoint, []string, error) {
	var missing []string
	var enum unsafe.Pointer
	if hr, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&sysCLSIDMMDeviceEnumerator)), 0, sysCLSCTXAll,
		uintptr(unsafe.Pointer(&sysIIDMMDeviceEnumerator)), uintptr(unsafe.Pointer(&enum))); hr != 0 {
		return nil, nil, fmt.Errorf("CoCreateInstance(MMDeviceEnumerator): %w", sysHRESULT(uint32(hr)))
	}
	defer sysRelease(enum)
	var dev unsafe.Pointer
	if hr := sysCOM(enum, 4, 0 /* eRender */, 0 /* eConsole */, uintptr(unsafe.Pointer(&dev))); hr != 0 {
		return nil, nil, fmt.Errorf("GetDefaultAudioEndpoint: %w", sysHRESULT(hr))
	}
	defer sysRelease(dev)

	f := map[string]any{}
	var idPtr *uint16
	if hr := sysCOM(dev, 5, uintptr(unsafe.Pointer(&idPtr))); hr == 0 && idPtr != nil {
		f["endpoint_id"] = windows.UTF16PtrToString(idPtr)
		windows.CoTaskMemFree(unsafe.Pointer(idPtr))
	}
	var store unsafe.Pointer
	if hr := sysCOM(dev, 4, 0 /* STGM_READ */, uintptr(unsafe.Pointer(&store))); hr == 0 {
		if s, ok := sysPropString(store, &sysPKeyDeviceFriendlyName); ok {
			f["name"] = s
		}
		if s, ok := sysPropString(store, &sysPKeyInterfaceFriendlyName); ok {
			f["adapter"] = s
			for _, d := range drivers {
				if strings.EqualFold(fmt.Sprint(d["desc"]), s) {
					f["driver_provider"], f["driver_version"], f["driver_date"] = d["provider"], d["version"], d["date"]
				}
			}
		}
		if v, ok := sysPropUint(store, &sysPKeyDisableSysFx); ok {
			f["enhancements_disabled"] = v != 0
		} else {
			missing = append(missing, "audio.endpoint.enhancements")
		}
		sysRelease(store)
	} else {
		missing = append(missing, "audio.endpoint.properties")
	}

	var client unsafe.Pointer
	if hr := sysCOM(dev, 3, uintptr(unsafe.Pointer(&sysIIDAudioClient)), sysCLSCTXAll, 0, uintptr(unsafe.Pointer(&client))); hr == 0 {
		var wfx unsafe.Pointer
		if hr := sysCOM(client, 8, uintptr(unsafe.Pointer(&wfx))); hr == 0 && wfx != nil {
			tag := *(*uint16)(wfx)
			f["mix_channels"] = *(*uint16)(unsafe.Add(wfx, 2))
			f["mix_sample_rate"] = *(*uint32)(unsafe.Add(wfx, 4))
			f["mix_bits"] = *(*uint16)(unsafe.Add(wfx, 14))
			f["mix_format_tag"] = fmt.Sprintf("0x%04x", tag)
			if tag == 0xFFFE && *(*uint16)(unsafe.Add(wfx, 16)) >= 22 {
				f["mix_valid_bits"] = *(*uint16)(unsafe.Add(wfx, 18))
				f["mix_channel_mask"] = fmt.Sprintf("0x%x", *(*uint32)(unsafe.Add(wfx, 20)))
				sub := *(*windows.GUID)(unsafe.Add(wfx, 24))
				switch sub.Data1 {
				case 3:
					f["mix_subformat"] = "float"
				case 1:
					f["mix_subformat"] = "pcm"
				default:
					f["mix_subformat"] = sub.String()
				}
			}
			windows.CoTaskMemFree(wfx)
		} else {
			missing = append(missing, "audio.endpoint.mix_format")
		}
		var def, min int64
		if hr := sysCOM(client, 9, uintptr(unsafe.Pointer(&def)), uintptr(unsafe.Pointer(&min))); hr == 0 {
			f["device_period_default_us"] = def / 10
			f["device_period_min_us"] = min / 10
		}
		sysRelease(client)
	} else {
		missing = append(missing, "audio.endpoint.audio_client")
	}

	var vol unsafe.Pointer
	if hr := sysCOM(dev, 3, uintptr(unsafe.Pointer(&sysIIDAudioEndpointVolume)), sysCLSCTXAll, 0, uintptr(unsafe.Pointer(&vol))); hr == 0 {
		var level float32
		if sysCOM(vol, 9, uintptr(unsafe.Pointer(&level))) == 0 {
			f["volume_scalar"] = sysRound(float64(level), 3)
		}
		var mute int32
		if sysCOM(vol, 15, uintptr(unsafe.Pointer(&mute))) == 0 {
			f["mute"] = mute != 0
		}
		sysRelease(vol)
	} else {
		missing = append(missing, "audio.endpoint.volume")
	}

	var mgr unsafe.Pointer
	if hr := sysCOM(dev, 3, uintptr(unsafe.Pointer(&sysIIDAudioSessionManager2)), sysCLSCTXAll, 0, uintptr(unsafe.Pointer(&mgr))); hr == 0 {
		active, inactive := sysAudioSessions(mgr, names)
		f["sessions_active"] = active
		f["sessions_inactive_count"] = inactive
		sysRelease(mgr)
	} else {
		missing = append(missing, "audio.endpoint.sessions")
	}

	// Fingerprint ignores nothing that matters for a change event; volume is
	// rounded so a slider drag yields one event per settled value.
	fp := fmt.Sprintf("%v|%v|%v|%v|%v|%v|%v|%v", f["endpoint_id"], f["mix_sample_rate"], f["mix_channels"],
		f["mix_bits"], f["volume_scalar"], f["mute"], f["sessions_active"], f["enhancements_disabled"])
	return &sysEndpoint{fields: f, fingerprint: fp}, missing, nil
}

func sysAudioSessions(mgr unsafe.Pointer, names map[uint32]string) ([]string, int) {
	var senum unsafe.Pointer
	if sysCOM(mgr, 5, uintptr(unsafe.Pointer(&senum))) != 0 || senum == nil {
		return nil, 0
	}
	defer sysRelease(senum)
	var count int32
	if sysCOM(senum, 3, uintptr(unsafe.Pointer(&count))) != 0 {
		return nil, 0
	}
	active := []string{}
	inactive := 0
	for i := int32(0); i < count && i < 256; i++ {
		var ctl unsafe.Pointer
		if sysCOM(senum, 4, uintptr(i), uintptr(unsafe.Pointer(&ctl))) != 0 || ctl == nil {
			continue
		}
		var state uint32
		sysCOM(ctl, 3, uintptr(unsafe.Pointer(&state)))
		name := "unknown"
		var ctl2 unsafe.Pointer
		if sysCOM(ctl, 0, uintptr(unsafe.Pointer(&sysIIDAudioSessionControl2)), uintptr(unsafe.Pointer(&ctl2))) == 0 && ctl2 != nil {
			if sysCOM(ctl2, 15) == 0 {
				name = "system_sounds"
			} else {
				var pid uint32
				hr := sysCOM(ctl2, 14, uintptr(unsafe.Pointer(&pid)))
				switch {
				case hr == 0x08890005: // AUDCLNT_S_NO_SINGLE_PROCESS
					name = "multi_process"
				case hr == 0 && names[pid] != "":
					name = names[pid]
				case hr == 0:
					name = fmt.Sprintf("pid_%d", pid)
				}
			}
			sysRelease(ctl2)
		}
		sysRelease(ctl)
		if state == 1 { // AudioSessionStateActive
			active = append(active, name)
		} else {
			inactive++
		}
	}
	sort.Strings(active)
	return active, inactive
}

func sysServiceState(name string) string {
	mgr, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return "unknown: " + err.Error()
	}
	defer windows.CloseServiceHandle(mgr)
	n, _ := windows.UTF16PtrFromString(name)
	svc, err := windows.OpenService(mgr, n, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return "unknown: " + err.Error()
	}
	defer windows.CloseServiceHandle(svc)
	var st windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(svc, &st); err != nil {
		return "unknown: " + err.Error()
	}
	switch st.CurrentState {
	case windows.SERVICE_RUNNING:
		return "running"
	case windows.SERVICE_STOPPED:
		return "stopped"
	case windows.SERVICE_START_PENDING:
		return "start_pending"
	case windows.SERVICE_STOP_PENDING:
		return "stop_pending"
	case windows.SERVICE_PAUSED:
		return "paused"
	default:
		return fmt.Sprintf("state_%d", st.CurrentState)
	}
}

// sysAudioDrivers lists the drivers of the MEDIA device class.
func sysAudioDrivers() []map[string]any {
	const classKey = `SYSTEM\CurrentControlSet\Control\Class\{4d36e96c-e325-11ce-bfc1-08002be10318}`
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, classKey, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil
	}
	defer k.Close()
	subs, _ := k.ReadSubKeyNames(-1)
	var out []map[string]any
	seen := map[string]bool{}
	for _, s := range subs {
		if len(s) != 4 {
			continue
		}
		path := classKey + `\` + s
		desc := sysRegString(registry.LOCAL_MACHINE, path, "DriverDesc")
		if desc == "" {
			continue
		}
		ver := sysRegString(registry.LOCAL_MACHINE, path, "DriverVersion")
		if seen[desc+"|"+ver] {
			continue
		}
		seen[desc+"|"+ver] = true
		out = append(out, map[string]any{
			"desc":     desc,
			"provider": sysRegString(registry.LOCAL_MACHINE, path, "ProviderName"),
			"version":  ver,
			"date":     sysRegString(registry.LOCAL_MACHINE, path, "DriverDate"),
		})
	}
	return out
}

func (w *sysWinState) emitAudioStack(drivers []map[string]any) {
	f := map[string]any{
		"audiosrv":               sysServiceState("Audiosrv"),
		"audio_endpoint_builder": sysServiceState("AudioEndpointBuilder"),
		"audio_drivers":          drivers,
	}
	const mmcss = `SOFTWARE\Microsoft\Windows NT\CurrentVersion\Multimedia\SystemProfile`
	if v, ok := sysRegInt(registry.LOCAL_MACHINE, mmcss, "NetworkThrottlingIndex"); ok {
		f["mmcss_network_throttling_index"] = v
	}
	if v, ok := sysRegInt(registry.LOCAL_MACHINE, mmcss, "SystemResponsiveness"); ok {
		f["mmcss_system_responsiveness_pct"] = v
	}
	diagEmit("sys.audio_stack", f)
	w.unavail.report("audio.spatial_sound", errors.New("no registry or public API read implemented"))
	w.unavail.report("audio.exclusive_mode_in_use", errors.New("no public API reports exclusive-mode streams"))
}

// audioLoop polls the default render endpoint on a COM-initialized thread
// and emits audio.endpoint on start and whenever its fingerprint changes.
func (w *sysWinState) audioLoop(ctx context.Context) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED); err != nil && err != syscall.Errno(1) /* S_FALSE */ {
		w.unavail.report("audio.endpoint", fmt.Errorf("CoInitializeEx: %w", err))
		return
	}
	defer windows.CoUninitialize()
	if procCoCreateInstance.Find() != nil || procPropVariantClear.Find() != nil {
		w.unavail.report("audio.endpoint", errors.New("ole32 procs missing"))
		return
	}
	drivers := sysAudioDrivers()
	w.emitAudioStack(drivers)

	last, lastID := "", ""
	ticker := time.NewTicker(sysAudioPollInterval)
	defer ticker.Stop()
	for {
		names := map[uint32]string{}
		if procs, err := sysProcessList(); err == nil {
			for _, p := range procs {
				names[p.pid] = p.name
			}
		}
		ep, missing, err := sysReadEndpoint(names, drivers)
		if err != nil {
			w.unavail.report("audio.endpoint", err)
		} else {
			for _, m := range missing {
				w.unavail.report(m, errors.New("not reported by the endpoint or COM call failed"))
			}
			id := fmt.Sprint(ep.fields["endpoint_id"])
			if lastID != "" && id != lastID {
				diagEmit("sys.event", map[string]any{"event": "default_audio_endpoint_change", "to_name": ep.fields["name"]})
			}
			lastID = id
			if ep.fingerprint != last {
				if last != "" {
					ep.fields["changed"] = true
				}
				diagEmit("audio.endpoint", ep.fields)
				last = ep.fingerprint
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
