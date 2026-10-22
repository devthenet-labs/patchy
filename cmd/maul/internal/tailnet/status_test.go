// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package tailnet

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
)

// seed makes every property here deterministic.
const seed = 20261018

func quickConfig(n int) *quick.Config {
	return &quick.Config{MaxCount: n, Rand: rand.New(rand.NewSource(seed))}
}

// statusJSON is `tailscale status --json` as a 1.104 client prints it,
// trimmed to what matters: this machine, the operator's own device, the
// previews' device and a laptop that happens to share its name.
const statusJSON = `{
  "Version": "1.104.1",
  "BackendState": "Running",
  "Self": {"HostName": "desk", "DNSName": "desk.tail1234.ts.net.",
    "TailscaleIPs": ["100.101.1.1", "fd7a:115c:a1e0::1"], "Online": true},
  "MagicDNSSuffix": "tail1234.ts.net",
  "Peer": {
    "nodekey:01": {"HostName": "tailscale-operator", "DNSName": "tailscale-operator.tail1234.ts.net.",
      "TailscaleIPs": ["100.101.2.2"], "Tags": ["tag:k8s-operator"], "Online": true},
    "nodekey:02": {"HostName": "maul-previews", "DNSName": "maul-previews.tail1234.ts.net.",
      "TailscaleIPs": ["100.101.12.58", "fd7a:115c:a1e0::3a"], "Tags": ["tag:maul-previews"], "Online": true},
    "nodekey:03": {"HostName": "maul-previews", "DNSName": "maul-previews-laptop.tail1234.ts.net.",
      "TailscaleIPs": ["100.101.3.3"], "Online": false}
  }
}`

var previewTags = []string{"tag:maul-previews"}

func TestFindThePreviewsDevice(t *testing.T) {
	st, err := ParseStatus([]byte(statusJSON))
	if err != nil {
		t.Fatal(err)
	}
	m := Find(st, "maul-previews", previewTags)
	if !m.Found || m.Device.DNSName != "maul-previews.tail1234.ts.net" ||
		!slices.Equal(m.Device.IPv4, []string{"100.101.12.58"}) {
		t.Fatalf("Find = %+v", m)
	}
	// The laptop asked for the same name but carries no tag: never it.
	if m := Find(st, "maul-previews", []string{"tag:other"}); m.Found ||
		!strings.Contains(m.Why, "maul-previews.tail1234.ts.net does not carry tag:other") {
		t.Errorf("another tag: %+v", m)
	}
}

func TestFindWhyNot(t *testing.T) {
	stopped := strings.Replace(statusJSON, `"Running"`, `"NeedsLogin"`, 1)
	renamed := strings.Replace(statusJSON, `"maul-previews.tail1234.ts.net."`,
		`"maul-previews-1.tail1234.ts.net."`, 1)
	renamed = strings.Replace(renamed, `"HostName": "maul-previews", "DNSName": "maul-previews-1`,
		`"HostName": "maul-previews-1", "DNSName": "maul-previews-1`, 1)
	twice := strings.Replace(statusJSON, `"TailscaleIPs": ["100.101.3.3"], "Online": false`,
		`"TailscaleIPs": ["100.101.3.3"], "Tags": ["tag:maul-previews"], "Online": false`, 1)
	// The operator's device under the old tag, as a renamed tag leaves it
	// until the tailnet policy lets the operator own the new one.
	oldTag := strings.Replace(statusJSON, `"Tags": ["tag:maul-previews"]`, `"Tags": ["tag:patchy-previews"]`, 1)
	gone := strings.Replace(oldTag, `"maul-previews.tail1234.ts.net."`, `"elsewhere.tail1234.ts.net."`, 1)
	hostile := strings.Replace(statusJSON, `"NeedsLogin"`, `"Running"`, 1)
	hostile = strings.Replace(hostile, `"Running"`, `"Run\u001b[31mning"`, 1)
	for name, tc := range map[string]struct{ raw, want string }{
		"not running": {stopped, "this machine's Tailscale client is NeedsLogin, not Running"},
		"renamed":     {renamed, "an older maul-previews still holds the name"},
		"two of them": {twice, "2 devices named maul-previews"},
		"other tag": {oldTag, "maul-previews.tail1234.ts.net does not carry tag:maul-previews: the operator can " +
			"apply tag:maul-previews only while the tailnet policy's \"tagOwners\" lets its own tag " +
			"(tag:k8s-operator by default) own it"},
		"no device": {gone, "no device named maul-previews carrying tag:maul-previews is on this machine's " +
			"tailnet: the operator creates it once the gateway's Service is installed, and the operator can " +
			"apply tag:maul-previews only while the tailnet policy's \"tagOwners\""},
		"control chars": {hostile, "this machine's Tailscale client is unknown"},
	} {
		st, err := ParseStatus([]byte(tc.raw))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if m := Find(st, "maul-previews", previewTags); m.Found || !strings.Contains(m.Why, tc.want) {
			t.Errorf("%s: %+v, want %q", name, m, tc.want)
		}
	}
	for _, raw := range []string{"", "not json", `["a"]`, strings.Repeat(" ", MaxStatus+1)} {
		if _, err := ParseStatus([]byte(raw)); err == nil {
			t.Errorf("ParseStatus(%.20q) succeeded", raw)
		}
	}
}

// TestParseStatusKeepsOnlyWhatValidates: a hostile client's names, tags
// and addresses never come through as they were.
func TestParseStatusKeepsOnlyWhatValidates(t *testing.T) {
	raw := `{"BackendState": "Running", "Peer": {"k": {"HostName": "x\u001b]8;;evil", "DNSName": "a b.ts.net.",
		"TailscaleIPs": ["8.8.8.8", "100.64.0.1", "100.64.0.1", "fd7a:115c:a1e0::1", "garbage"],
		"Tags": ["tag:ok", "tag:\u202ebad", "nottag"]}}}`
	st, err := ParseStatus([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	d := st.Devices[0]
	if d.HostName != "" || d.DNSName != "" || d.Name != "" || !slices.Equal(d.IPv4, []string{"100.64.0.1"}) ||
		!slices.Equal(d.Tags, []string{"tag:ok"}) {
		t.Errorf("device = %+v", d)
	}
}

// randStatus is a status of a few devices drawn from small pools, so names
// and tags collide often.
func randStatus(r *rand.Rand) Status {
	names := []string{"maul-previews", "maul-previews-1", "maul-previews-x", "desk", "MAUL-PREVIEWS"}
	tags := []string{"tag:maul-previews", "tag:k8s", "tag:other"}
	ips := []string{"100.101.12.58", "100.64.0.1", "8.8.8.8", "10.0.0.1", "fd7a:115c:a1e0::1"}
	states := []string{Running, Running, Running, "Stopped", "NeedsLogin"}
	type peer struct {
		HostName, DNSName string
		TailscaleIPs      []string
		Tags              []string
	}
	doc := struct {
		BackendState string
		Peer         map[string]peer
	}{BackendState: states[r.Intn(len(states))], Peer: map[string]peer{}}
	for i := range r.Intn(5) {
		p := peer{HostName: names[r.Intn(len(names))], DNSName: names[r.Intn(len(names))] + ".tail1234.ts.net."}
		for range r.Intn(3) {
			p.TailscaleIPs = append(p.TailscaleIPs, ips[r.Intn(len(ips))])
		}
		for range r.Intn(3) {
			p.Tags = append(p.Tags, tags[r.Intn(len(tags))])
		}
		doc.Peer[fmt.Sprint("nodekey:", i)] = p
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	st, err := ParseStatus(raw)
	if err != nil {
		panic(err)
	}
	return st
}

// qualifies is the property's own statement of the device Find looks for.
func qualifies(d Device, hostname string, tags []string) bool {
	if d.Name != hostname && d.HostName != hostname || len(d.IPv4) == 0 {
		return false
	}
	for _, tg := range tags {
		if !slices.Contains(d.Tags, tg) {
			return false
		}
	}
	return true
}

// TestFindProperties (seeded): Find is exactly "the one Running device
// named hostname that carries every tag and a tailnet IPv4 address": a
// device found is such a device and the only one, every address it gives
// is a tailnet IPv4 address, and when exactly one such device exists it is
// found; otherwise there is a reason.
func TestFindProperties(t *testing.T) {
	cfg := quickConfig(3000)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		args[0] = reflect.ValueOf(randStatus(r))
		args[1] = reflect.ValueOf([]string{"maul-previews", "desk"}[r.Intn(2)])
		args[2] = reflect.ValueOf([][]string{{"tag:maul-previews"}, {"tag:k8s", "tag:maul-previews"}}[r.Intn(2)])
	}
	prop := func(st Status, hostname string, tags []string) bool {
		qualifies := func(d Device) bool { return qualifies(d, hostname, tags) }
		n := 0
		for _, d := range st.Devices {
			if qualifies(d) {
				n++
			}
		}
		m := Find(st, hostname, tags)
		switch {
		case m.Found && (st.BackendState != Running || !qualifies(m.Device) || n != 1):
			t.Logf("found %+v (running %s, %d qualify)", m.Device, st.BackendState, n)
			return false
		case m.Found && slices.ContainsFunc(m.Device.IPv4, func(s string) bool {
			ip, err := netip.ParseAddr(s)
			return err != nil || !tailnetV4.Contains(ip)
		}):
			t.Logf("found %+v with an address outside the tailnet", m.Device)
			return false
		case !m.Found && st.BackendState == Running && n == 1:
			t.Logf("missed the one device: %s", m.Why)
			return false
		case !m.Found && m.Why == "":
			t.Log("not found, and no reason")
			return false
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

func TestLimitedBuffer(t *testing.T) {
	b := &limitedBuffer{max: 4}
	if n, err := b.Write([]byte("abc")); n != 3 || err != nil || b.over {
		t.Fatalf("first write: %d %v %v", n, err, b.over)
	}
	if n, err := b.Write([]byte("def")); n != 3 || err != nil || !b.over || b.String() != "abcd" {
		t.Errorf("second write: %d %v %v %q", n, err, b.over, b.String())
	}
}

// TestLimitedBufferIsNotAReaderFrom: io.Copy must go through Write, so the
// limit holds when exec copies a client's stdout into the buffer.
func TestLimitedBufferIsNotAReaderFrom(t *testing.T) {
	b := &limitedBuffer{max: 4}
	if _, ok := any(b).(io.ReaderFrom); ok {
		t.Fatal("limitedBuffer is an io.ReaderFrom, so io.Copy bypasses its limit")
	}
	if _, err := io.Copy(b, strings.NewReader("abcdefgh")); err != nil || !b.over || b.String() != "abcd" {
		t.Errorf("io.Copy: %v %v %q", err, b.over, b.String())
	}
}
