package airplay

import (
	"strings"
	"testing"
	"time"
)

const netshGerman = "\r\nEs ist 1 Schnittstelle auf dem System vorhanden:\r\n\r\n" +
	"    Name                   : WLAN\r\n" +
	"    Beschreibung           : Intel(R) Wi-Fi 6E AX211 160MHz\r\n" +
	"    GUID                   : 3F9A7C1E-1111-2222-3333-444455556666\r\n" +
	"    Physische Adresse      : aa:bb:cc:dd:ee:ff\r\n" +
	"    Schnittstellentyp      : Prim\x84r\r\n" +
	"    Status                 : Verbunden\r\n" +
	"    SSID                   : HeimNetz\r\n" +
	"    BSSID                  : 11:22:33:44:55:66\r\n" +
	"    Netzwerktyp            : Infrastruktur\r\n" +
	"    Funktyp                : 802.11ax\r\n" +
	"    Authentifizierung      : WPA3-Personal\r\n" +
	"    Verschl\x81sselung       : CCMP\r\n" +
	"    Verbindungsmodus       : Automatische Verbindung\r\n" +
	"    Band                   : 5 GHz\r\n" +
	"    Kanal                  : 36\r\n" +
	"    Empfangsrate (MBit/s)  : 1201\r\n" +
	"    \x9Abertragungsrate (MBit/s) : 960,7\r\n" +
	"    Signal                 : 94%\r\n" +
	"    Profil                 : HeimNetz\r\n" +
	"\r\n    Status der gehosteten Netzwerke : Nicht verf\x81gbar\r\n"

const netshEnglish = `
There is 1 interface on the system:

    Name                   : Wi-Fi
    Description            : Intel(R) Wi-Fi 6 AX201 160MHz
    GUID                   : 11111111-2222-3333-4444-555555555555
    Physical address       : aa-bb-cc-dd-ee-ff
    State                  : connected
    SSID                   : HomeNet
    BSSID                  : 11-22-33-44-55-66
    Network type           : Infrastructure
    Radio type             : 802.11ac
    Channel                : 44
    Receive rate (Mbps)    : 866.7
    Transmit rate (Mbps)   : 780
    Signal                 : 71%
`

func TestParseNetshWlanInterfacesGerman(t *testing.T) {
	list := parseNetshWlanInterfaces(netshGerman)
	if len(list) != 1 {
		t.Fatalf("got %d interfaces, want 1: %+v", len(list), list)
	}
	w := list[0]
	if w.Name != "WLAN" || w.GUID != "3f9a7c1e-1111-2222-3333-444455556666" {
		t.Errorf("name/guid = %q/%q", w.Name, w.GUID)
	}
	if w.SSID != "HeimNetz" || w.BSSID != "11:22:33:44:55:66" {
		t.Errorf("ssid/bssid = %q/%q", w.SSID, w.BSSID)
	}
	if w.RadioType != "802.11ax" || w.Band != "5GHz" && w.Band != "5 GHz" {
		t.Errorf("phy/band = %q/%q", w.RadioType, w.Band)
	}
	if !w.HaveChannel || w.Channel != 36 {
		t.Errorf("channel = %d (have %v)", w.Channel, w.HaveChannel)
	}
	if !w.HaveSignal || w.SignalPct != 94 {
		t.Errorf("signal = %d", w.SignalPct)
	}
	if !w.HaveRates || w.RxMbps != 1201 || w.TxMbps != 960.7 {
		t.Errorf("rates rx/tx = %v/%v", w.RxMbps, w.TxMbps)
	}
	if w.State != "Verbunden" {
		t.Errorf("state = %q", w.State)
	}
}

func TestParseNetshWlanInterfacesEnglishAndRedaction(t *testing.T) {
	list := parseNetshWlanInterfaces(netshEnglish)
	if len(list) != 1 {
		t.Fatalf("got %d interfaces", len(list))
	}
	w := list[0]
	if w.RxMbps != 866.7 || w.TxMbps != 780 || w.Channel != 44 || w.SignalPct != 71 || w.RadioType != "802.11ac" {
		t.Errorf("unexpected parse: %+v", w)
	}
	f := sysWifiFields(w)
	for k, v := range f {
		if s, ok := v.(string); ok && (strings.Contains(s, "HomeNet") || strings.Contains(strings.ToLower(s), "22:33")) {
			t.Errorf("field %s leaks raw identifier: %q", k, s)
		}
	}
	if f["ssid_hash"] == "" || f["ssid_hash"] == "HomeNet" || f["bssid_hash"] == "" {
		t.Errorf("hashes missing: %v", f)
	}
	if f["band_guess"] != "5 GHz" {
		t.Errorf("band_guess = %v", f["band_guess"])
	}
}

func TestParseNetshNoInterface(t *testing.T) {
	if got := parseNetshWlanInterfaces("Der Dienst f\x81r die automatische WLAN-Konfiguration (wlansvc) wird nicht ausgef\x81hrt.\r\n"); len(got) != 0 {
		t.Fatalf("expected no interfaces, got %+v", got)
	}
	if got := parseNetshWlanInterfaces(""); len(got) != 0 {
		t.Fatalf("expected no interfaces, got %+v", got)
	}
}

func TestParseNvidiaSmi(t *testing.T) {
	f, ok := parseNvidiaSmi("37, 1210, 8188, 1680, 52, P2\r\n")
	if !ok || f["gpu_util_pct"] != 37.0 || f["gpu_mem_used_mb"] != 1210.0 || f["gpu_pstate"] != "P2" {
		t.Fatalf("parse = %v ok=%v", f, ok)
	}
	if _, ok := parseNvidiaSmi("NVIDIA-SMI has failed"); ok {
		t.Fatal("garbage should not parse")
	}
	if _, ok := parseNvidiaSmi(""); ok {
		t.Fatal("empty should not parse")
	}
}

func TestSysPercentile(t *testing.T) {
	s := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	cases := []struct {
		p    float64
		want int64
	}{{0, 1}, {50, 5}, {90, 9}, {95, 10}, {99, 10}, {100, 10}}
	for _, c := range cases {
		if got := sysPercentile(s, c.p); got != c.want {
			t.Errorf("p%v = %d, want %d", c.p, got, c.want)
		}
	}
	if sysPercentile(nil, 50) != 0 {
		t.Error("empty slice must give 0")
	}
	if sysPercentile([]int64{7}, 95) != 7 {
		t.Error("single value")
	}
}

func TestSysRateLimiter(t *testing.T) {
	l := newSysRateLimiter(20)
	t0 := time.Unix(1000, 0)
	allowed := 0
	for i := 0; i < 100; i++ {
		if l.allow(t0) {
			allowed++
		}
	}
	if allowed != 20 {
		t.Fatalf("burst allowed %d, want 20", allowed)
	}
	if n := l.takeSuppressed(); n != 80 {
		t.Fatalf("suppressed %d, want 80", n)
	}
	if l.takeSuppressed() != 0 {
		t.Fatal("suppressed counter must reset")
	}
	// Half a second later 10 tokens have refilled.
	t1 := t0.Add(500 * time.Millisecond)
	allowed = 0
	for i := 0; i < 50; i++ {
		if l.allow(t1) {
			allowed++
		}
	}
	if allowed != 10 {
		t.Fatalf("after refill allowed %d, want 10", allowed)
	}
}

func TestSysPingWindowSummary(t *testing.T) {
	var w sysPingWindow
	for _, ms := range []int{2, 4, 3, 80, 5} {
		w.add(time.Duration(ms)*time.Millisecond, true, "")
	}
	w.add(0, false, "timeout")
	f := w.summary()
	if f["replies_iv"] != 5 || f["lost_iv"] != 1 || f["slow_iv"] != 1 {
		t.Errorf("counts = %v", f)
	}
	if f["min_us"] != int64(2000) || f["max_us"] != int64(80000) || f["p50_us"] != int64(4000) || f["p95_us"] != int64(80000) {
		t.Errorf("percentiles = %v", f)
	}
	if f["sent_total"] != uint64(6) || f["lost_total"] != uint64(1) {
		t.Errorf("totals = %v", f)
	}
	// Interval counters reset, totals persist.
	f = w.summary()
	if f["replies_iv"] != 0 || f["lost_iv"] != 0 || f["sent_total"] != uint64(6) {
		t.Errorf("after reset = %v", f)
	}
	if _, ok := f["min_us"]; ok {
		t.Errorf("empty window must not report min_us: %v", f)
	}
}

func TestSysClockWatch(t *testing.T) {
	var c sysClockWatch
	w0 := time.Unix(5000, 0)
	if _, _, ok := c.observe(w0, 10*time.Second); ok {
		t.Fatal("first observation has no delta")
	}
	el, step, ok := c.observe(w0.Add(time.Second), 11*time.Second)
	if !ok || el != time.Second || step != 0 {
		t.Fatalf("steady: %v %v %v", el, step, ok)
	}
	// Wall clock jumps 40 ms forward while the monotonic clock advanced 1 s.
	el, step, _ = c.observe(w0.Add(2*time.Second+40*time.Millisecond), 12*time.Second)
	if el != time.Second || step != 40*time.Millisecond {
		t.Fatalf("step: %v %v", el, step)
	}
}

func TestSysHelpers(t *testing.T) {
	cases := map[string]string{
		"192.0.2.10:7000": "192.0.2.10",
		"192.0.2.10":      "192.0.2.10",
		"[fe80::1]:7000":  "fe80::1",
		"[fe80::1]":       "fe80::1",
		" tv.local:7000 ": "tv.local",
	}
	for in, want := range cases {
		if got := sysReceiverHost(in); got != want {
			t.Errorf("sysReceiverHost(%q) = %q, want %q", in, got, want)
		}
	}
	if got := sysMACOUI([]byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}); got != "aa:bb:cc" {
		t.Errorf("oui = %q", got)
	}
	if sysMACOUI([]byte{1, 2}) != "" {
		t.Error("short mac must give empty OUI")
	}
	if sysRedact("") != "" || sysRedact("a") == sysRedact("b") || sysRedact("a") != sysRedact("a") || len(sysRedact("a")) != 12 {
		t.Error("sysRedact must be stable, distinct and short")
	}
	if got := sysPdhInstanceName("Intel(R) Wi-Fi 6E AX211 160MHz #2"); got != "intel[r] wi-fi 6e ax211 160mhz _2" {
		t.Errorf("pdh instance = %q", got)
	}
	if sysBandFromChannel(6) != "2.4 GHz" || sysBandFromChannel(100) != "5 GHz" {
		t.Error("band guess")
	}
}

func TestSysUnavailableOncePerItem(t *testing.T) {
	u := newSysUnavailable()
	u.report("x", nil)
	u.report("x", nil)
	if len(u.seen) != 1 {
		t.Fatalf("seen = %v", u.seen)
	}
}
