package web

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientLimiterSeparatesClients(t *testing.T) {
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	c := newClientLimiter(3, time.Second)
	for i := 0; i < 3; i++ {
		if !c.allow("10.0.0.1", now) {
			t.Fatalf("request %d of the first client refused", i+1)
		}
	}
	if c.allow("10.0.0.1", now) {
		t.Fatal("first client not limited after its burst")
	}
	if !c.allow("10.0.0.2", now) {
		t.Fatal("second client limited by the first one")
	}
	if !c.allow("10.0.0.1", now.Add(time.Second)) {
		t.Fatal("first client not refilled after one interval")
	}
}

func TestClientLimiterPrune(t *testing.T) {
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	c := newClientLimiter(2, time.Second)
	c.allow("a", now)
	c.allow("b", now)
	c.allow("b", now.Add(time.Second))
	if n := c.prune(now.Add(time.Second)); n != 1 {
		t.Fatalf("pruned %d buckets, want 1 (only a is full again)", n)
	}
	if n := c.prune(now.Add(10 * time.Second)); n != 1 {
		t.Fatalf("pruned %d buckets, want 1", n)
	}
	if len(c.buckets) != 0 {
		t.Fatalf("%d buckets left", len(c.buckets))
	}
}

func TestClientIP(t *testing.T) {
	cases := []struct {
		name, remote string
		xff          []string
		want         string
	}{
		{"direct public client", "203.0.113.7:5000", nil, "203.0.113.7"},
		{"public client cannot spoof", "203.0.113.7:5000", []string{"198.51.100.1"}, "203.0.113.7"},
		{"behind proxy", "172.18.0.3:4000", []string{"198.51.100.1"}, "198.51.100.1"},
		{"proxy appends last entry", "172.18.0.3:4000", []string{"1.2.3.4, 198.51.100.1"}, "198.51.100.1"},
		{"last header wins", "127.0.0.1:4000", []string{"1.2.3.4", "198.51.100.2"}, "198.51.100.2"},
		{"proxy without header", "172.18.0.3:4000", nil, "172.18.0.3"},
		{"garbage header", "172.18.0.3:4000", []string{"nonsense"}, "172.18.0.3"},
		{"ipv6 behind proxy", "[::1]:4000", []string{"2001:db8::1"}, "2001:db8::1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/auth/start", nil)
			r.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := clientIP(r); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}
