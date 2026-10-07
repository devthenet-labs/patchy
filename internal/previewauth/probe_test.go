// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"errors"
	"math/rand"
	"net/url"
	"reflect"
	"testing"
	"testing/quick"
)

func probeLocation(issuer string, slot int, label string, edit func(url.Values)) string {
	q := url.Values{
		"client_id":     {ClientID(slot)},
		"redirect_uri":  {"https://" + label + "." + testSuffix + CallbackPath},
		"response_type": {"code"},
		"scope":         {"openid"},
		"state":         {"alb-state"},
	}
	if edit != nil {
		edit(q)
	}
	return issuer + "/authorize?" + q.Encode()
}

func TestJudgeProbe(t *testing.T) {
	cb := newCallbacks(t)
	good := probeLocation(testIssuer, 1, "intent-7", nil)
	tests := []struct {
		name     string
		status   int
		location string
		ok       bool
	}{
		{"the ALB redirect", 302, good, true},
		{"the app answers", 200, "", false},
		{"a 401", 401, "", false},
		{"a 301 redirect", 301, good, false},
		{"another slot's client", 302, probeLocation(testIssuer, 0, "intent-7", nil), false},
		{"another host's callback", 302, probeLocation(testIssuer, 1, "intent-8", nil), false},
		{"another issuer", 302, probeLocation("https://evil.example.com", 1, "intent-7", nil), false},
		{"issuer lookalike", 302, probeLocation(testIssuer+".evil", 1, "intent-7", nil), false},
		{"another path", 302, testIssuer + "/authorize/x?" + url.Values{"client_id": {ClientID(1)}}.Encode(), false},
		{"repeated client", 302, probeLocation(testIssuer, 1, "intent-7", func(q url.Values) {
			q.Add("client_id", ClientID(0))
		}), false},
		{"no response type", 302, probeLocation(testIssuer, 1, "intent-7", func(q url.Values) {
			q.Del("response_type")
		}), false},
		{"userinfo in location", 302, "https://u@preview-auth.example.com/authorize?client_id=patchy-preview-s1", false},
		{"unparseable", 302, "%zz", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := JudgeProbe(tt.status, tt.location, testIssuer, 1, "intent-7", cb)
			if (err == nil) != tt.ok {
				t.Fatalf("JudgeProbe = %v, want ok %v", err, tt.ok)
			}
			if err != nil && !errors.Is(err, ErrUnprotected) {
				t.Fatalf("error %v is not ErrUnprotected", err)
			}
		})
	}
}

// TestJudgeProbeOnlyItsOwnSlotAndLabel: a redirect for any (slot, label)
// passes for exactly that pair and for no other.
func TestJudgeProbeOnlyItsOwnSlotAndLabel(t *testing.T) {
	cb := newCallbacks(t)
	type pair struct {
		Slot  int
		Label string
	}
	gen := func(vals []reflect.Value, r *rand.Rand) {
		vals[0] = reflect.ValueOf(pair{r.Intn(MaxSlots), randLabel(r)})
		switch r.Intn(3) {
		case 0:
			vals[1] = vals[0]
		case 1:
			vals[1] = reflect.ValueOf(pair{r.Intn(MaxSlots), vals[0].Interface().(pair).Label})
		default:
			vals[1] = reflect.ValueOf(pair{r.Intn(MaxSlots), randLabel(r)})
		}
	}
	prop := func(issued, asked pair) bool {
		loc := probeLocation(testIssuer, issued.Slot, issued.Label, nil)
		err := JudgeProbe(302, loc, testIssuer, asked.Slot, asked.Label, cb)
		return (err == nil) == (issued == asked)
	}
	cfg := quickConfig(2000)
	cfg.Values = gen
	if err := quick.Check(prop, cfg); err != nil {
		t.Fatal(err)
	}
}
