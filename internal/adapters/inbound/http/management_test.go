package httpadapter

import (
	"net/http/httptest"
	"net/netip"
	"strconv"
	"testing"
	"time"
)

func TestLoginLimiterBoundsAndAccounts(t *testing.T) {
	l := &loginLimiter{entries: map[string]loginWindow{}}
	now := time.Now()
	for range 5 {
		if !l.allow("127.0.0.1", "Operator@example.com", now) {
			t.Fatal("early throttle")
		}
	}
	if l.allow("127.0.0.2", " operator@EXAMPLE.com ", now) {
		t.Fatal("account throttle bypass via IP/case")
	}
	if !l.allow("127.0.0.2", "operator@example.com", now.Add(16*time.Minute)) {
		t.Fatal("window did not expire")
	}
	for i := 0; i < 5000; i++ {
		_ = l.allow(netip.AddrFrom4([4]byte{10, byte(i >> 8), byte(i), 1}).String(), "unknown-"+strconv.Itoa(i), now)
	}
	if len(l.entries) > 4096 {
		t.Fatal("unbounded rate limiter")
	}
}
func TestManagementProxyTrust(t *testing.T) {
	h := &ManagementHandler{opts: ManagementOptions{TrustedProxyCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}}
	r := httptest.NewRequest("POST", "http://gateway/admin/v1/auth/login", nil)
	r.RemoteAddr = "192.0.2.1:1234"
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	if got := h.remoteIP(r); got != "192.0.2.1" {
		t.Fatal("untrusted forwarded header accepted")
	}
	r.RemoteAddr = "10.0.0.1:1234"
	if got := h.remoteIP(r); got != "198.51.100.1" {
		t.Fatal("trusted proxy not resolved")
	}
	r.Header.Set("X-Forwarded-For", "malformed")
	if got := h.remoteIP(r); got != "10.0.0.1" {
		t.Fatal("malformed proxy address accepted")
	}
}
