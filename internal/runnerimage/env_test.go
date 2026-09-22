// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import (
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	"github.com/bitwise-media-group/patchy/internal/provider"
)

const envSuffix = "; `PATCHY_*`, `ANTHROPIC_*`, `CLAUDE_*`, proxy and patchy-reserved variables cannot come " +
	"from the image"

func TestCheckEnv(t *testing.T) {
	jobReserved := map[string]bool{"HOME": true, "GITHUB_TOKEN": true}
	cases := []struct {
		name    string
		env     []string
		extra   map[string]bool
		wantErr string
	}{
		{"benign toolchain env", []string{"PATH=/usr/local/go/bin:/usr/bin", "GOPATH=/go", "GOFLAGS=-mod=vendor",
			"LANG=C.UTF-8", "HOME=/root"}, nil, ""},
		{"nil env", nil, nil, ""},
		{"a job-reserved name is rejected only when supplied", []string{"HOME=/root"}, jobReserved,
			"image ENV sets `HOME`" + envSuffix},
		{"a credential channel", []string{"GITHUB_TOKEN=x"}, jobReserved, "image ENV sets `GITHUB_TOKEN`" + envSuffix},
		{"PATCHY_ prefix (fixture config)", []string{"PATCHY_CHANGESET_MAX_BYTES=1"}, nil,
			"image ENV sets `PATCHY_CHANGESET_MAX_BYTES`" + envSuffix},
		{"ANTHROPIC_ prefix", []string{"ANTHROPIC_MODEL=x"}, nil, "image ENV sets `ANTHROPIC_MODEL`" + envSuffix},
		{"CLAUDE_ prefix", []string{"CLAUDE_CONFIG_DIR=/x"}, nil, "image ENV sets `CLAUDE_CONFIG_DIR`" + envSuffix},
		{"CLAUDE_CODE_ prefix", []string{"CLAUDE_CODE_USE_BEDROCK=1"}, nil,
			"image ENV sets `CLAUDE_CODE_USE_BEDROCK`" + envSuffix},
		{"gateway name without a reserved prefix", []string{"AWS_REGION=us-east-1"}, nil,
			"image ENV sets `AWS_REGION`" + envSuffix},
		{"gateway name CLOUD_ML_REGION", []string{"CLOUD_ML_REGION=us-east5"}, nil,
			"image ENV sets `CLOUD_ML_REGION`" + envSuffix},
		{"upper-case proxy", []string{"HTTPS_PROXY=http://p:3128"}, nil, "image ENV sets `HTTPS_PROXY`" + envSuffix},
		{"lower-case proxy", []string{"http_proxy=http://p:3128"}, nil, "image ENV sets `http_proxy`" + envSuffix},
		{"mixed-case proxy", []string{"No_Proxy=localhost"}, nil, "image ENV sets `No_Proxy`" + envSuffix},
		{"all_proxy", []string{"all_proxy=socks5://p"}, nil, "image ENV sets `all_proxy`" + envSuffix},
		{"a name without a value", []string{"PATCHY_FOO"}, nil, "image ENV sets `PATCHY_FOO`" + envSuffix},
		{"offenders are sorted and deduplicated", []string{"http_proxy=a", "PATCHY_X=1", "HTTPS_PROXY=b", "PATCHY_X=2",
			"GOPATH=/go"}, nil, "image ENV sets `HTTPS_PROXY`, `PATCHY_X`, `http_proxy`" + envSuffix},
		{"a prefix must be a prefix", []string{"MY_PATCHY_VAR=1", "XANTHROPIC_=1", "PROXY=1", "HTTP_PROXY_LIST=1"}, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckEnv(c.env, c.extra)
			if c.wantErr == "" {
				if err != nil {
					t.Errorf("CheckEnv(%v) = %v, want nil", c.env, err)
				}
				return
			}
			if err == nil || err.Error() != c.wantErr {
				t.Errorf("CheckEnv(%v) =\n  %v\nwant\n  %q", c.env, err, c.wantErr)
			}
			if !IsRejection(err) {
				t.Errorf("error is not a Rejection: %T", err)
			}
		})
	}
}

func TestReservedEnvNameCoversEveryGatewayName(t *testing.T) {
	for _, name := range provider.GatewayEnvNames {
		if !ReservedEnvName(name, nil) {
			t.Errorf("gateway name %s is not reserved", name)
		}
	}
}

func genEnvName(r *rand.Rand) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ_0123456789"
	n := 1 + r.Intn(12)
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[r.Intn(len(alphabet))]
	}
	return string(b)
}

func TestReservedEnvPrefixProperty(t *testing.T) {
	prefixes := []string{"PATCHY_", "ANTHROPIC_", "CLAUDE_", "CLAUDE_CODE_"}
	cfg := &quick.Config{
		MaxCount: 1000,
		Rand:     rand.New(rand.NewSource(20260922)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			args[0] = reflect.ValueOf(prefixes[r.Intn(len(prefixes))] + genEnvName(r))
			args[1] = reflect.ValueOf(genEnvName(r))
		},
	}
	// Any name under a reserved prefix is rejected however it is spelled
	// past the prefix, and a random name is reserved only by one of the
	// stated rules — never by accident.
	rule := func(prefixed, random string) bool {
		if CheckEnv([]string{prefixed + "=1"}, nil) == nil {
			return false
		}
		reserved := ReservedEnvName(random, nil)
		byRule := slices.Contains(provider.GatewayEnvNames, random) ||
			slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(random, p) }) ||
			slices.Contains(proxyEnv, strings.ToUpper(random))
		return reserved == byRule
	}
	if err := quick.Check(rule, cfg); err != nil {
		t.Error(err)
	}
}
