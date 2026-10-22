// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package tailnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
)

// MaxStatus bounds the client's status: a tailnet of thousands of peers
// is a few megabytes.
const MaxStatus = 16 << 20

// Running is the client's BackendState once it is up and logged in.
const Running = "Running"

// tailnetV4 is where Tailscale gives devices their IPv4 addresses.
var tailnetV4 = netip.MustParsePrefix("100.64.0.0/10")

var (
	labelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	tagPattern   = regexp.MustCompile(`^tag:[A-Za-z0-9][A-Za-z0-9-]{0,127}$`)
	statePattern = regexp.MustCompile(`^[A-Za-z]{1,32}$`)
)

// Device is one node of the tailnet, as much of it as validates.
type Device struct {
	// Name is the first label of DNSName: the device's name on the
	// tailnet. HostName is the name it asked for, which differs when
	// another device already held it.
	Name, DNSName, HostName string
	// IPv4 are its tailnet IPv4 addresses, sorted.
	IPv4 []string
	Tags []string
	// Online is the client's view: whether the device is connected now.
	Online bool
}

// Status is what the client reports: its own state and every device it
// can see, itself included.
type Status struct {
	BackendState string
	Devices      []Device
}

// ErrStatus reports a status that is not the client's JSON.
var ErrStatus = errors.New("tailscale status")

// rawPeer is the part of `tailscale status --json`'s PeerStatus read.
type rawPeer struct {
	HostName     string   `json:"HostName"`
	DNSName      string   `json:"DNSName"`
	TailscaleIPs []string `json:"TailscaleIPs"`
	Tags         []string `json:"Tags"`
	Online       bool     `json:"Online"`
}

// ParseStatus reads `tailscale status --json`. Every value is validated
// and anything that does not is dropped, never quoted.
func ParseStatus(raw []byte) (Status, error) {
	if len(raw) > MaxStatus {
		return Status{}, fmt.Errorf("%w: more than %d bytes", ErrStatus, MaxStatus)
	}
	var st struct {
		BackendState string             `json:"BackendState"`
		Self         *rawPeer           `json:"Self"`
		Peer         map[string]rawPeer `json:"Peer"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return Status{}, fmt.Errorf("%w: not the client's JSON status", ErrStatus)
	}
	out := Status{BackendState: "unknown"}
	if statePattern.MatchString(st.BackendState) {
		out.BackendState = st.BackendState
	}
	if st.Self != nil {
		out.Devices = append(out.Devices, device(*st.Self))
	}
	for _, p := range st.Peer {
		out.Devices = append(out.Devices, device(p))
	}
	slices.SortFunc(out.Devices, func(a, b Device) int {
		return strings.Compare(a.DNSName+"\x00"+a.HostName, b.DNSName+"\x00"+b.HostName)
	})
	return out, nil
}

// device keeps what validates of a peer.
func device(p rawPeer) Device {
	d := Device{Online: p.Online}
	if name := strings.TrimSuffix(strings.ToLower(p.DNSName), "."); dnsName(name) {
		d.DNSName = name
		d.Name, _, _ = strings.Cut(name, ".")
	}
	if h := strings.ToLower(p.HostName); labelPattern.MatchString(h) {
		d.HostName = h
	}
	for _, s := range p.TailscaleIPs {
		if ip, err := netip.ParseAddr(s); err == nil && ip.Is4() && tailnetV4.Contains(ip) {
			d.IPv4 = append(d.IPv4, ip.String())
		}
	}
	slices.Sort(d.IPv4)
	d.IPv4 = slices.Compact(d.IPv4)
	for _, t := range p.Tags {
		if tagPattern.MatchString(t) {
			d.Tags = append(d.Tags, t)
		}
	}
	return d
}

// dnsName reports a lowercase DNS name of at least two labels.
func dnsName(s string) bool {
	if len(s) > 253 || !strings.Contains(s, ".") {
		return false
	}
	for l := range strings.SplitSeq(s, ".") {
		if !labelPattern.MatchString(l) {
			return false
		}
	}
	return true
}

// Match is what Find concluded.
type Match struct {
	// Device is the one device found, when Found.
	Device Device
	Found  bool
	// Why says, when not Found, why not: printable, every name in it
	// validated.
	Why string
}

// Find picks the device named hostname (its tailnet name, or the name it
// asked for) that carries every tag in tags and has a tailnet IPv4
// address, as a Running client sees the tailnet: found only when exactly
// one does.
func Find(st Status, hostname string, tags []string) Match {
	hostname = strings.ToLower(hostname)
	want := strings.Join(tags, ", ")
	if st.BackendState != Running {
		return Match{Why: fmt.Sprintf("this machine's Tailscale client is %s, not %s: it sees no tailnet",
			st.BackendState, Running)}
	}
	tagged := func(d Device) bool {
		return len(d.IPv4) > 0 && !slices.ContainsFunc(tags, func(t string) bool { return !slices.Contains(d.Tags, t) })
	}
	var found, renamed, untagged []Device
	for _, d := range st.Devices {
		switch {
		case !tagged(d):
			// Only the device's own tailnet name: another machine may
			// merely have asked for the same one.
			if d.Name == hostname && len(d.IPv4) > 0 {
				untagged = append(untagged, d)
			}
		case d.Name == hostname || d.HostName == hostname:
			found = append(found, d)
		case strings.HasPrefix(d.Name, hostname+"-") && digits(d.Name[len(hostname)+1:]):
			renamed = append(renamed, d)
		}
	}
	switch {
	case len(found) == 1:
		return Match{Device: found[0], Found: true}
	case len(found) > 1:
		return Match{Why: fmt.Sprintf("%d devices named %s carry %s (%s): delete the stale ones in the admin "+
			"console", len(found), hostname, want, names(found))}
	case len(renamed) > 0:
		return Match{Why: fmt.Sprintf("no device is named %s, but %s carr%s %s: an older %s still holds the "+
			"name, so delete it in the admin console and the operator's proxy takes the name back once it "+
			"restarts", hostname, names(renamed), pluralVerb(len(renamed)), want, hostname)}
	case len(untagged) > 0:
		return Match{Why: fmt.Sprintf("%s does not carry %s: %s", names(untagged), want, tagPolicyHint(want))}
	}
	return Match{Why: fmt.Sprintf("no device named %s carrying %s is on this machine's tailnet: the operator "+
		"creates it once the gateway's Service is installed, and %s", hostname, want, tagPolicyHint(want))}
}

// tagPolicyHint is what the operator needs before it can tag a device:
// the tailnet policy must name the operator's own tag as an owner of each.
// A renamed tag fails here too, until the policy is changed to match.
func tagPolicyHint(want string) string {
	return fmt.Sprintf("the operator can apply %s only while the tailnet policy's \"tagOwners\" lets its own tag "+
		"(%s by default) own it, as step 6 of the NEXT-STEPS.md maul setup previews writes shows", want, operatorTag)
}

// operatorTag is the tag the Tailscale operator's own device carries
// unless its chart says otherwise.
const operatorTag = "tag:k8s-operator"

func digits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

func names(ds []Device) string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.DNSName
		if out[i] == "" {
			out[i] = d.HostName
		}
	}
	return strings.Join(out, ", ")
}

func pluralVerb(n int) string {
	if n == 1 {
		return "ies"
	}
	return "y"
}

// StatusFunc reads a client's raw status: Local, or a test's stand-in.
type StatusFunc func(context.Context) ([]byte, error)

// Lookup asks status for the tailnet and Finds hostname's device in it; a
// client that cannot be asked, or answers nonsense, is a Match not found
// that says so.
func Lookup(ctx context.Context, status StatusFunc, hostname string, tags []string) Match {
	if status == nil {
		return Match{Why: "no Tailscale client to ask"}
	}
	raw, err := status(ctx)
	if err != nil {
		return Match{Why: fmt.Sprintf("this machine's Tailscale client could not be asked (%v)", err)}
	}
	st, err := ParseStatus(raw)
	if err != nil {
		return Match{Why: err.Error()}
	}
	return Find(st, hostname, tags)
}
