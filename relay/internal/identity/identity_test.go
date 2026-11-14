package identity

import (
	"net/netip"
	"testing"
)

func testHash(kind, value string) string { return kind + ":" + value }

func TestCanonicalIPv4Mapped(t *testing.T) {
	a := netip.MustParseAddr("::ffff:203.0.113.9")
	got, err := CanonicalIP(a, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.String() != "203.0.113.9" {
		t.Fatalf("expected unmapped ipv4, got %s", got.String())
	}
}

func TestCanonicalIPRejects(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.1.2.3", "fe80::1", "0.0.0.0", "ff02::1"} {
		if _, err := CanonicalIP(netip.MustParseAddr(raw), false); err == nil {
			t.Fatalf("expected %s to be rejected", raw)
		}
	}
}

func TestLoopbackAllowedInDev(t *testing.T) {
	if _, err := CanonicalIP(netip.MustParseAddr("127.0.0.1"), true); err != nil {
		t.Fatalf("loopback should pass when allowed: %v", err)
	}
}

func TestClientIPIgnoresUntrustedChain(t *testing.T) {
	remote := netip.MustParseAddr("198.51.100.7")
	headers := map[string][]string{
		HeaderForwardedFor: {"1.2.3.4, 203.0.113.9"},
	}
	// With our trusted proxy appended last, the client is the last hop.
	got, hops, err := ClientIP(remote, headers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.String() != "198.51.100.7" {
		t.Fatalf("expected socket address to win, got %s", got.String())
	}
	if hops != 2 {
		t.Fatalf("expected 2 hops, got %d", hops)
	}
}

func TestNetworkMasks(t *testing.T) {
	if got := Network(netip.MustParseAddr("203.0.113.77")); got.String() != "203.0.113.0/24" {
		t.Fatalf("v4 mask wrong: %s", got.String())
	}
	if got := Network(netip.MustParseAddr("2001:db8:1234:5678::1")); got.String() != "2001:db8:1234:5678::/64" {
		t.Fatalf("v6 mask wrong: %s", got.String())
	}
}

func TestNormalizeIdentifier(t *testing.T) {
	good := []string{"4c4c4544-0044-3510-8054-b4c04f4e4e32", "0123456789abcdef0123456789abcdef"}
	for _, v := range good {
		if _, err := NormalizeIdentifier("machine_id", v, 16, 128); err != nil {
			t.Fatalf("%s should be valid: %v", v, err)
		}
	}
	bad := []string{"", "0", "unknown", "00000000-0000-0000-0000-000000000000", "has spaces", "short"}
	for _, v := range bad {
		if _, err := NormalizeIdentifier("machine_id", v, 16, 128); err == nil {
			t.Fatalf("%q should be rejected", v)
		}
	}
}

func TestFromRequestBuildsThreeMarkersInOrder(t *testing.T) {
	h := func(kind, value string) string { return kind + ":" + value }
	dev := Device{
		MachineID:  "4c4c4544-0044-3510-8054-b4c04f4e4e32",
		DiskUUID:   "9f1b2c3d4e5f60718293a4b5c6d7e8f9",
		ClockSkewS: 0,
	}
	id, err := FromRequest(netip.MustParseAddr("198.51.100.7"), map[string][]string{}, dev, h, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(id.Markers) != 3 {
		t.Fatalf("expected 3 markers, got %d", len(id.Markers))
	}
	want := []string{MarkerIP, MarkerMachineID, MarkerDiskUUID}
	for i, m := range id.Markers {
		if m.Type != want[i] {
			t.Fatalf("marker %d: expected %s got %s", i, want[i], m.Type)
		}
	}
}

func TestPrivacyBlocksHighScore(t *testing.T) {
	p := Privacy{}
	p.add(60, "test")
	if !p.Blocked() {
		t.Fatal("score 60 should be blocked")
	}
	if (Privacy{IsVPN: true, Score: 20}).Blocked() {
		t.Fatal("low score should not be blocked")
	}
}

func TestLooksHosted(t *testing.T) {
	if !LooksHosted(netip.MustParseAddr("3.5.140.2")) {
		t.Fatal("3.x should look hosted")
	}
	if LooksHosted(netip.MustParseAddr("203.0.113.2")) {
		t.Fatal("documentation range should not look hosted")
	}
}
