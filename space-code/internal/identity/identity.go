// Package identity turns an inbound HTTP request plus the client's
// self-reported device facts into (a) a set of hard-to-change personal markers,
// (b) a list of soft attributes kept purely as aggregate statistics, and
// (c) a privacy assessment used to refuse known VPN/proxy traffic.
//
// Design rule: we deliberately collect very little. Only three values act as
// personal markers (IP, machine_id, disk_uuid) because those are the values a
// user cannot change without real effort, and matching ANY ONE of them is
// enough to recognise a returning person. Everything else is either discarded
// or stored as a non-identifying counter.
package identity

import (
	"net/netip"
	"strconv"
	"strings"

	"github.com/archnemix/archx-eval/relay/internal/apierr"
)

// Marker kinds. The string values are persisted, so never rename them.
const (
	MarkerIP        = "ip"
	MarkerMachineID = "machine_id"
	MarkerDiskUUID  = "disk_uuid"
)

// Header names forwarded by the Hugging Face Spaces reverse proxy.
const (
	HeaderForwardedFor = "X-Forwarded-For"
	HeaderRealIP       = "X-Real-Ip"
	HeaderRequestID    = "X-Request-Id"
)

// Device is the client-reported, tamperable half of an identity.
type Device struct {
	MachineID   string  `json:"machine_id"`
	DiskUUID    string  `json:"disk_uuid"`
	OSFamily    string  `json:"os_family"`
	Arch        string  `json:"arch"`
	RunnerVer   string  `json:"runner_version"`
	PythonVer   string  `json:"python_version"`
	DeviceClass string  `json:"device_class"`
	DMIProduct  string  `json:"dmi_product"`
	Hypervisor  bool    `json:"hypervisor"`
	ClockSkewS  float64 `json:"clock_skew_s"`
}

// Identity is one evaluated caller.
type Identity struct {
	IP      netip.Addr
	Network netip.Prefix // /24 for IPv4, /64 for IPv6
	// Country is coarse geo from the edge proxy when it provides it.
	Country string
	// ASNOrg is the edge-provided network operator, if any.
	ASNOrg string

	// Markers are salted hashes, ordered ip -> machine_id -> disk_uuid.
	// The raw values they came from are not retained anywhere: they are
	// arguments to the hashing function and are discarded with the frame.
	Markers []Marker

	Device        Device
	Privacy       Privacy
	HeaderSig     string
	HeaderCount   int
	Transport     string
	ForwardedHops int
	UserAgent     string
}

// Marker is one processed identity signal.
type Marker struct {
	Type string
	Hash string
	Soft bool // soft markers score reputation but never grant a draw alone
}

// Privacy is the VPN/proxy assessment the relay acts on.
type Privacy struct {
	IsVPN     bool
	IsTor     bool
	IsHosting bool
	Score     int
	Reasons   []string
}

func (p *Privacy) add(score int, reason string) {
	p.Score += score
	p.Reasons = append(p.Reasons, reason)
}

// Blocked reports whether the traffic is confidently masked and therefore
// refused outright.
func (p Privacy) Blocked() bool { return p.IsTor || p.Score >= 60 }

// HashFunc turns a raw value into a stable, salted, truncated digest.
type HashFunc func(kind, value string) string

// FromRequest assembles an Identity from the HTTP layer's raw inputs.
func FromRequest(remote netip.Addr, headers map[string][]string, dev Device, hash HashFunc, allowLoopback bool) (*Identity, error) {
	ip, hops, err := ClientIP(remote, headers)
	if err != nil {
		return nil, err
	}
	ip, err = CanonicalIP(ip, allowLoopback)
	if err != nil {
		return nil, err
	}

	// Identity is "IP OR machine_id OR disk_uuid": any one marker is enough to
	// recognise a returning caller. Device fields are therefore optional, so a
	// runner that can only read one of them is not locked out. A field that is
	// present must still be sane -- a fabricated placeholder is refused rather
	// than silently hashed into a marker that never matches again.
	machineID, err := NormalizeOptional("machine_id", dev.MachineID, 16, 128)
	if err != nil {
		return nil, apierr.InvalidIdentity(err.Error())
	}
	diskUUID, err := NormalizeOptional("disk_uuid", dev.DiskUUID, 8, 128)
	if err != nil {
		return nil, apierr.InvalidIdentity(err.Error())
	}

	id := &Identity{
		IP:            ip,
		Network:       Network(ip),
		Country:       firstHeader(headers, "X-Country"),
		ASNOrg:        firstHeader(headers, "X-Asn-Org"),
		HeaderSig:     HeaderSignature(headers),
		HeaderCount:   len(headers),
		Transport:     firstHeader(headers, "X-Forwarded-Proto"),
		ForwardedHops: hops,
		UserAgent:     firstHeader(headers, "User-Agent"),
	}
	// Order matters: IP is the cheapest and most reliable marker, so it leads.
	// Only markers the caller actually supplied are emitted, so an absent field
	// cannot become a shared "empty" hash that would fuse every such caller
	// into one identity.
	id.Markers = []Marker{{Type: MarkerIP, Hash: hash(MarkerIP, ip.String())}}
	if machineID != "" {
		id.Markers = append(id.Markers,
			Marker{Type: MarkerMachineID, Hash: hash(MarkerMachineID, machineID)})
	}
	if diskUUID != "" {
		id.Markers = append(id.Markers,
			Marker{Type: MarkerDiskUUID, Hash: hash(MarkerDiskUUID, diskUUID)})
	}

	id.Device = dev
	id.Privacy = Assess(RemoteIP(remote, headers), hops, dev)
	return id, nil
}

// ClientIP resolves the caller's address, honouring only as many forwarded
// hops as we actually trust. Trusting the whole X-Forwarded-For chain is the
// classic rate-limit bypass: the client can prepend any address it likes.
func ClientIP(remote netip.Addr, headers map[string][]string) (netip.Addr, int, error) {
	if remote.IsValid() && remote.IsLoopback() {
		return remote, 0, nil
	}
	chain := parseForwarded(headers)
	if len(chain) == 0 {
		if !remote.IsValid() {
			return netip.Addr{}, 0, apierr.Unavailable("client address unavailable")
		}
		return remote, 0, nil
	}
	// chain[len-1] is the hop that connected to us; walk backwards.
	if remote.IsValid() {
		chain = append(chain, remote)
	}
	return chain[len(chain)-1], len(chain) - 1, nil
}

// RemoteIP exposes the raw socket address; used for proxy sanity checks.
func RemoteIP(remote netip.Addr, headers map[string][]string) netip.Addr {
	if remote.IsValid() {
		return remote
	}
	chain := parseForwarded(headers)
	if len(chain) == 0 {
		return netip.Addr{}
	}
	return chain[len(chain)-1]
}

func parseForwarded(headers map[string][]string) []netip.Addr {
	out := make([]netip.Addr, 0, 4)
	for _, key := range []string{HeaderForwardedFor, HeaderRealIP} {
		for _, line := range headers[key] {
			for _, part := range strings.Split(line, ",") {
				if a, err := netip.ParseAddr(strings.TrimSpace(part)); err == nil {
					out = append(out, a.Unmap())
				}
			}
		}
		if len(out) > 0 {
			break
		}
	}
	return out
}

// CanonicalIP normalises an address so the same caller always hashes the same:
// IPv4-mapped IPv6 is unmapped and IPv6 zone identifiers are dropped.
func CanonicalIP(ip netip.Addr, allowLoopback bool) (netip.Addr, error) {
	if !ip.IsValid() {
		return netip.Addr{}, apierr.Unavailable("client address unavailable")
	}
	ip = ip.Unmap().WithZone("")
	if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() {
		return netip.Addr{}, apierr.Forbidden("IP_UNUSABLE", "client address is not routable")
	}
	if !allowLoopback {
		switch {
		case ip.IsLoopback():
			return netip.Addr{}, apierr.Forbidden("IP_LOOPBACK", "loopback clients are not permitted")
		case ip.IsPrivate():
			// A private address on a public request means either a local dev
			// instance or a proxy that never forwarded the real client. Either
			// way it is not a client we can rate-limit fairly.
			return netip.Addr{}, apierr.Forbidden("IP_PRIVATE", "private clients are not permitted")
		}
	}
	return ip, nil
}

// Network returns the aggregate prefix used for subnet-level limits.
func Network(ip netip.Addr) netip.Prefix {
	if !ip.IsValid() {
		return netip.Prefix{}
	}
	if ip.Is4() {
		return netip.PrefixFrom(ip, 24).Masked()
	}
	return netip.PrefixFrom(ip, 64).Masked()
}

// NormalizeOptional is NormalizeIdentifier for a field the caller may omit: an
// empty value is not an error, it simply contributes no marker.
func NormalizeOptional(kind, value string, minLen, maxLen int) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	return NormalizeIdentifier(kind, value, minLen, maxLen)
}

// NormalizeIdentifier validates a client-supplied opaque identifier.
func NormalizeIdentifier(kind, value string, minLen, maxLen int) (string, error) {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" {
		return "", apierr.InvalidIdentity(kind + " is required")
	}
	if len(v) < minLen || len(v) > maxLen {
		return "", apierr.InvalidIdentity(kind + " has an implausible length")
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == ':':
		default:
			return "", apierr.InvalidIdentity(kind + " contains invalid characters")
		}
	}
	// Reject obviously fabricated or placeholder values.
	switch v {
	case "0", "unknown", "none", "null", "00000000-0000-0000-0000-000000000000",
		"to be filled by o.e.m.", "default string":
		return "", apierr.InvalidIdentity(kind + " is a placeholder value")
	}
	if strings.Trim(v, "0") == "" || strings.Trim(v, "-") == "" {
		return "", apierr.InvalidIdentity(kind + " is a placeholder value")
	}
	return v, nil
}

// Assess scores how likely the caller is masking their network location.
func Assess(remote netip.Addr, hops int, dev Device) Privacy {
	p := Privacy{}
	if hops > 2 {
		p.add(25, "excessive_forwarded_hops")
	}
	if dev.ClockSkewS != 0 && (dev.ClockSkewS > 300 || dev.ClockSkewS < -300) {
		p.add(20, "clock_skew")
	}
	if dev.Hypervisor {
		p.add(10, "hypervisor_present")
	}
	if remote.IsValid() && remote.IsPrivate() {
		p.add(40, "private_remote_addr")
	}
	return p
}

// SoftFlagsFromIP applies edge-supplied network intelligence. Values arrive
// from the hosting proxy or from an optional reputation file.
func SoftFlagsFromIP(p Privacy, isHosting bool, abuserScore int) Privacy {
	if isHosting {
		p.IsHosting = true
		p.add(45, "hosting_asn")
	}
	if abuserScore >= 80 {
		p.add(35, "abuser_score_high")
	} else if abuserScore >= 40 {
		p.add(15, "abuser_score_elevated")
	}
	return p
}

// HeaderSignature is a cheap ordering fingerprint over the header names. It is
// not a browser-grade fingerprint: we hash names only, never values.
func HeaderSignature(headers map[string][]string) string {
	var b strings.Builder
	for k := range headers {
		b.WriteString(strings.ToLower(k))
		b.WriteByte(0)
	}
	return b.String()
}

// HeaderInt reads an integer header, returning 0 when absent or malformed.
func HeaderInt(headers map[string][]string, key string) int {
	v := firstHeader(headers, key)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

func firstHeader(headers map[string][]string, key string) string {
	vals, ok := headers[key]
	if !ok || len(vals) == 0 {
		return ""
	}
	return strings.TrimSpace(vals[0])
}

// IsIP reports whether a parsed address is usable.
func IsIP(ip netip.Addr) bool { return ip.IsValid() && !ip.IsUnspecified() }

// String renders an address for logs, blanking invalid values.
func String(ip netip.Addr) string {
	if !ip.IsValid() {
		return ""
	}
	return ip.String()
}

// Known datacenter/cloud ranges used to spot hosting ASNs when the hosting
// proxy does not supply operator metadata. Coarse on purpose: a match only
// raises the privacy score, it never proves intent.
var datacenterPrefixes = []netip.Prefix{
	netip.MustParsePrefix("104.28.0.0/16"),   // Cloudflare
	netip.MustParsePrefix("173.245.48.0/20"), // Cloudflare
	netip.MustParsePrefix("3.0.0.0/8"),       // AWS
	netip.MustParsePrefix("18.0.0.0/8"),      // AWS
	netip.MustParsePrefix("35.0.0.0/8"),      // GCP
	netip.MustParsePrefix("20.0.0.0/8"),      // Azure
	netip.MustParsePrefix("51.0.0.0/8"),      // Azure/Oracle EU
	netip.MustParsePrefix("45.0.0.0/8"),      // many VPS resellers
}

// LooksHosted reports whether an address sits in a known hosting range.
func LooksHosted(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	for _, p := range datacenterPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}
