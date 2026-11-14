// Package reputation answers one question: given a client address, is this
// traffic coming from a hosting provider, a known VPN exit, or a network with
// a bad abuse history?
//
// The data is a plain JSON file (REPUTATION_FILE) so it can be refreshed from
// CI without rebuilding the service. When no file is configured every lookup
// returns "unknown" and the relay falls back to its static hosted-range list.
package reputation

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"sort"
	"strings"
	"sync"
)

// Entry is the intelligence we hold for one network range.
type Entry struct {
	Prefix  netip.Prefix `json:"-"`
	CIDR    string       `json:"cidr"`
	Hosting bool         `json:"hosting,omitempty"`
	VPN     bool         `json:"vpn,omitempty"`
	Tor     bool         `json:"tor,omitempty"`
	Abuser  int          `json:"abuser,omitempty"`
	ASNOrg  string       `json:"asn_org,omitempty"`
}

// Table is an immutable, longest-prefix-match lookup table.
type Table struct {
	mu      sync.RWMutex
	entries []Entry // sorted by prefix bits descending (longest first)
	loaded  bool
	source  string
}

// Load reads a reputation file. A missing path yields an empty table, which is
// a valid (fully permissive) configuration.
func Load(path string) (*Table, error) {
	t := &Table{source: path}
	if path == "" {
		return t, nil
	}
	buf, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reputation: read %s: %w", path, err)
	}
	var file struct {
		Entries []Entry `json:"entries"`
	}
	if err := json.Unmarshal(buf, &file); err != nil {
		return nil, fmt.Errorf("reputation: parse %s: %w", path, err)
	}
	t.entries = make([]Entry, 0, len(file.Entries))
	for _, e := range file.Entries {
		if strings.TrimSpace(e.CIDR) == "" {
			continue
		}
		p, err := netip.ParsePrefix(strings.TrimSpace(e.CIDR))
		if err != nil {
			return nil, fmt.Errorf("reputation: bad cidr %q: %w", e.CIDR, err)
		}
		e.Prefix = p.Masked()
		t.entries = append(t.entries, e)
	}
	t.sort()
	t.loaded = len(t.entries) > 0
	return t, nil
}

func (t *Table) sort() {
	sort.SliceStable(t.entries, func(i, j int) bool {
		return t.entries[i].Prefix.Bits() > t.entries[j].Prefix.Bits()
	})
}

// Lookup returns the most specific entry covering ip.
func (t *Table) Lookup(ip netip.Addr) (Entry, bool) {
	if t == nil || !ip.IsValid() {
		return Entry{}, false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	for i := range t.entries {
		if t.entries[i].Prefix.Contains(ip) {
			return t.entries[i], true
		}
	}
	return Entry{}, false
}

// Size is the number of ranges loaded.
func (t *Table) Size() int {
	if t == nil {
		return 0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.entries)
}

// Loaded reports whether real data was found.
func (t *Table) Loaded() bool {
	if t == nil {
		return false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.loaded
}
