package airplay

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"howett.net/plist"
)

// Control-plane diagnostics: every RTSP/HTTP exchange on the control
// connection, every event-channel message and connection lifecycle events, as
// [DIAG] records (see audio_diag.go for the record format).
//
// kind values:
//
//	rtsp.exchange   one request/response pair on the control connection
//	event.rx        one receiver->sender request on the event channel
//	event.tx        our response on the event channel
//	event.closed    the event channel ended (error, if any)
//	conn.connect    a TCP connect attempt (control or event channel)
//	conn.close      the control connection was closed by us
//
// Redaction: request/response bodies of pairing, FairPlay and auth endpoints
// are never decoded. Plist binary data values are always replaced by
// "<redacted:N bytes>". Values whose field or header name is key-like (see
// diagSecretName) and long hex/base64-looking strings are replaced the same
// way. Unrecognized binary bodies are never dumped.

const diagBodyLimit = 16 * 1024

// diagSecretTokens are name tokens (camelCase/snake/kebab split, lowercased)
// which mark a field as secret.
var diagSecretTokens = map[string]bool{
	"key": true, "keys": true, "ekey": true, "eiv": true, "iv": true, "shk": true,
	"pk": true, "pv": true, "epk": true, "sk": true, "secret": true, "secrets": true,
	"password": true, "passwd": true, "passcode": true, "pwd": true, "pin": true,
	"token": true, "tokens": true, "credential": true, "credentials": true,
	"auth": true, "authorization": true, "authenticate": true, "signature": true,
	"sig": true, "salt": true, "proof": true, "fairplay": true, "fp": true,
	"fpdata": true, "challenge": true, "cookie": true, "nonce": true, "seed": true,
	"private": true, "cert": true, "certificate": true,
}

// diagNumericSecretTokens may hold a secret even as a number.
var diagNumericSecretTokens = map[string]bool{"pin": true, "password": true, "passwd": true, "passcode": true, "pwd": true}

// diagSecretHeaders are redacted regardless of token matching.
var diagSecretHeaders = map[string]bool{
	"authorization": true, "proxy-authorization": true, "www-authenticate": true,
	"apple-challenge": true, "apple-response": true, "x-apple-session-id": true,
	"cookie": true, "set-cookie": true,
}

func diagRedacted(n int) string { return fmt.Sprintf("<redacted:%d bytes>", n) }

// diagNameTokens splits a field name into lowercase tokens at non-alphanumeric
// separators and camelCase boundaries ("aesIV" -> aes, iv; "ekey" -> ekey).
func diagNameTokens(name string) []string {
	var tokens []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			tokens = append(tokens, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	runes := []rune(name)
	for i, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if unicode.IsUpper(r) && len(cur) > 0 {
			prev := runes[i-1]
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return tokens
}

// diagSecretName reports whether a field/header name is key-like.
func diagSecretName(name string) bool {
	lower := strings.ToLower(name)
	if diagSecretHeaders[lower] {
		return true
	}
	for _, token := range diagNameTokens(name) {
		if diagSecretTokens[token] || diagSecretCompound(token) {
			return true
		}
	}
	compact := strings.Join(diagNameTokens(name), "")
	for _, part := range diagSecretSubstrings {
		if strings.Contains(compact, part) {
			return true
		}
	}
	return false
}

var diagSecretSubstrings = []string{"fairplay", "password", "passwd", "secret", "token", "credential"}

// diagSecretCompound catches run-together names the camelCase split cannot
// separate ("rsaaeskey", "fairplaydata", "streamkey").
func diagSecretCompound(token string) bool {
	if strings.HasSuffix(token, "key") || strings.HasSuffix(token, "keys") {
		return true
	}
	for _, part := range diagSecretSubstrings {
		if strings.Contains(token, part) {
			return true
		}
	}
	return false
}

func diagNumericSecretName(name string) bool {
	for _, token := range diagNameTokens(name) {
		if diagNumericSecretTokens[token] {
			return true
		}
	}
	return false
}

// diagLooksLikeKeyMaterial flags long unbroken hex/base64 runs. UUIDs (with
// dashes) and ordinary text (with spaces/punctuation) are left readable.
func diagLooksLikeKeyMaterial(s string) bool {
	if len(s) < 32 {
		return false
	}
	hexOnly := true
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		case r >= 'g' && r <= 'z', r >= 'G' && r <= 'Z', r == '+', r == '/', r == '=':
			hexOnly = false
		default:
			return false
		}
	}
	return hexOnly || len(s) >= 40
}

// diagSanitizeValue converts a decoded plist value into a JSON-safe value with
// secrets and receiver identity removed. name is the enclosing field name (""
// for array elements, which inherit their parent's name).
func diagSanitizeValue(name string, v any, depth int) any {
	if depth > 16 {
		return "<depth-limit>"
	}
	secret := name != "" && diagSecretName(name)
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		return diagRedacted(len(x))
	case string:
		if secret || diagLooksLikeKeyMaterial(x) {
			return diagRedacted(len(x))
		}
		return x
	case bool:
		return x
	case uint64, int64, uint32, int32, int, uint, uint16, int16, uint8, int8:
		if name != "" && diagNumericSecretName(name) {
			return diagRedacted(8)
		}
		return x
	case float64:
		if name != "" && diagNumericSecretName(name) {
			return diagRedacted(8)
		}
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return fmt.Sprint(x)
		}
		return x
	case float32:
		return diagSanitizeValue(name, float64(x), depth)
	case time.Time:
		return x.Format(time.RFC3339Nano)
	case plist.UID:
		return fmt.Sprintf("uid:%d", uint64(x))
	case map[string]any:
		if secret {
			return fmt.Sprintf("<redacted:dict %d fields>", len(x))
		}
		if name == "info" {
			x = diagInfoSubset(x)
		}
		out := make(map[string]any, len(x))
		for k, val := range x {
			field := k
			if diagLooksLikeKeyMaterial(k) {
				field = fmt.Sprintf("<redacted-name:%d>", len(k))
			}
			out[field] = diagSanitizeValue(k, val, depth+1)
		}
		return out
	case []any:
		if secret {
			return fmt.Sprintf("<redacted:array %d items>", len(x))
		}
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = diagSanitizeValue(name, val, depth+1)
		}
		return out
	default:
		return fmt.Sprintf("<%T>", v)
	}
}

// diagInfoFields are the receiver info fields worth recording, whether returned
// directly by /info or nested in another response. Info carries device identity
// and extension fields with arbitrary names, so it is logged by allowlist.
var diagInfoFields = []string{
	"features", "statusFlags", "model", "sourceVersion", "protocolVersion", "vv",
	"audioLatencies", "audioFormats", "keepAliveSendStatsAsBody", "keepAliveLowPower",
	"initialVolume", "displays", "PTPInfo", "supportedFormats", "receiverHDRCapability",
}

func diagIsPairingPath(uri string) bool {
	lower := strings.ToLower(uri)
	for _, marker := range []string{"pair", "fp-setup", "auth-setup", "/auth", "verify", "/pin"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func diagIsText(body []byte) bool {
	if !utf8.Valid(body) {
		return false
	}
	for _, r := range string(body) {
		if r < 0x20 && r != '\r' && r != '\n' && r != '\t' {
			return false
		}
	}
	return true
}

// diagRedactTextLines keeps text bodies (text/parameters, SDP) verbatim except
// for lines whose name is key-like ("a=rsaaeskey:...", "password: ...").
func diagRedactTextLines(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		name := line
		if strings.HasPrefix(name, "a=") {
			name = name[2:]
		}
		idx := strings.IndexAny(name, ":=")
		if idx <= 0 {
			if diagLooksLikeKeyMaterial(strings.TrimSpace(line)) {
				lines[i] = diagRedacted(len(line))
			}
			continue
		}
		value := name[idx+1:]
		if diagSecretName(strings.TrimSpace(name[:idx])) || diagLooksLikeKeyMaterial(strings.TrimSpace(value)) {
			cut := len(line) - len(value)
			lines[i] = line[:cut] + diagRedacted(len(strings.TrimRight(value, "\r")))
		}
	}
	return strings.Join(lines, "\n")
}

// diagDescribeBody returns a JSON-safe, redacted representation of a message
// body plus whether it was truncated to diagBodyLimit.
func diagDescribeBody(uri, contentType string, body []byte) (any, bool) {
	if len(body) == 0 {
		return nil, false
	}
	if diagIsPairingPath(uri) {
		return diagRedacted(len(body)), false
	}
	lowerType := strings.ToLower(contentType)
	isPlist := strings.Contains(lowerType, "plist") || strings.HasPrefix(string(body[:min(len(body), 8)]), "bplist") ||
		strings.HasPrefix(string(body[:min(len(body), 6)]), "<?xml") || strings.HasPrefix(string(body[:min(len(body), 6)]), "<plist")
	if isPlist {
		var decoded any
		if _, err := plist.Unmarshal(body, &decoded); err == nil {
			if dict, ok := decoded.(map[string]any); ok && strings.HasSuffix(strings.ToLower(strings.SplitN(uri, "?", 2)[0]), "/info") {
				decoded = diagInfoSubset(dict)
			}
			value := diagSanitizeValue("", decoded, 0)
			encoded, err := json.Marshal(value)
			if err != nil {
				return fmt.Sprintf("<unencodable plist: %v>", err), false
			}
			if len(encoded) > diagBodyLimit {
				return string(encoded[:diagBodyLimit]), true
			}
			return value, false
		}
	}
	if diagIsText(body) {
		truncated := false
		text := body
		if len(text) > diagBodyLimit {
			text, truncated = text[:diagBodyLimit], true
		}
		return diagRedactTextLines(string(text)), truncated
	}
	// Unknown binary may be key material: record only its size.
	return diagRedacted(len(body)), false
}

func diagInfoSubset(dict map[string]any) map[string]any {
	out := map[string]any{}
	for _, field := range diagInfoFields {
		if v, ok := dict[field]; ok {
			out[field] = v
		}
	}
	out["_other_fields_n"] = uint64(len(dict) - (len(out)))
	return out
}

// diagHeaders returns a redacted copy of a header map.
func diagHeaders(headers map[string]string) map[string]string {
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		if diagSecretName(k) || diagLooksLikeKeyMaterial(v) {
			out[k] = diagRedacted(len(v))
		} else {
			out[k] = v
		}
	}
	return out
}

// diagParseHeaderBlock parses the header block of a serialized request (the
// part before the blank line), skipping the start line.
func diagParseHeaderBlock(data []byte) (startLine string, headers map[string]string) {
	headers = map[string]string{}
	block := string(data)
	if idx := strings.Index(block, "\r\n\r\n"); idx >= 0 {
		block = block[:idx]
	}
	lines := strings.Split(block, "\r\n")
	if len(lines) > 0 {
		startLine = lines[0]
	}
	for _, line := range lines[1:] {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return startLine, headers
}

// diagHexPrefix returns hex of at most n leading bytes.
func diagHexPrefix(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return hex.EncodeToString(b)
}

// diagRateLimiter allows at most limit records per one-second window and
// counts what it suppressed so the next allowed record can report it.
type diagRateLimiter struct {
	mu          sync.Mutex
	limit       int
	windowStart time.Time
	n           int
	suppressed  uint64
}

// allow returns whether to emit now and, when allowed, how many records were
// suppressed since the previous allowed one.
func (l *diagRateLimiter) allow(now time.Time) (bool, uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.windowStart.IsZero() || now.Sub(l.windowStart) >= time.Second || now.Before(l.windowStart) {
		l.windowStart, l.n = now, 0
	}
	if l.n >= l.limit {
		l.suppressed++
		return false, 0
	}
	l.n++
	suppressed := l.suppressed
	l.suppressed = 0
	return true, suppressed
}

// pending returns and clears the suppressed count (for periodic summaries).
func (l *diagRateLimiter) pending() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.suppressed
	l.suppressed = 0
	return s
}

// controlExchangeDiag captures one control-connection exchange.
type controlExchangeDiag struct {
	proto        string // "rtsp", "http" or "raw"
	method, uri  string
	cseq         uint64
	encrypted    bool
	request      []byte // plaintext serialized request (headers + body)
	reqBody      []byte
	reqType      string
	wireBytes    int
	started      time.Time
	wrote        time.Time
	respBody     []byte
	respHeaders  map[string]string
	status       int
	err          error
	sincePrevURI time.Duration
}

func (e *controlExchangeDiag) emit() {
	now := time.Now()
	_, reqHeaders := diagParseHeaderBlock(e.request)
	rec := map[string]any{
		"conn": "control", "proto": e.proto, "method": e.method, "uri": e.uri, "cseq": e.cseq,
		"encrypted": e.encrypted, "req_headers": diagHeaders(reqHeaders), "req_body_bytes": len(e.reqBody),
		"req_wire_bytes": e.wireBytes, "rtt_us": micros(now.Sub(e.started)),
		"status": e.status, "resp_body_bytes": len(e.respBody),
	}
	if !e.wrote.IsZero() {
		rec["write_us"] = micros(e.wrote.Sub(e.started))
	}
	if e.sincePrevURI > 0 {
		rec["since_prev_same_uri_ms"] = e.sincePrevURI.Milliseconds()
	}
	truncated := false
	if body, cut := diagDescribeBody(e.uri, e.reqType, e.reqBody); body != nil {
		rec["req_body"] = body
		truncated = truncated || cut
	}
	if e.respHeaders != nil {
		rec["resp_headers"] = diagHeaders(e.respHeaders)
	}
	if body, cut := diagDescribeBody(e.uri, e.respHeaders["content-type"], e.respBody); body != nil {
		rec["resp_body"] = body
		truncated = truncated || cut
	}
	if truncated {
		rec["truncated"] = true
	}
	if e.err != nil {
		rec["error"] = e.err.Error()
		rec["timeout"] = diagIsTimeout(e.err)
	}
	diagEmit("rtsp.exchange", rec)
}

func diagIsTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
