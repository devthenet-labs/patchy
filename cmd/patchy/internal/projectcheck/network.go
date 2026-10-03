// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectcheck

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
)

// Resolver looks host names up; *net.Resolver is one.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// TLSDialer completes a TLS handshake with host on port 443, verifying its
// certificate chain against the system's roots and its name against host,
// and returns the connection's state.
type TLSDialer func(ctx context.Context, host string) (tls.ConnectionState, error)

// DialTLS is the TLSDialer the CLI uses: a plain handshake, nothing sent.
func DialTLS(ctx context.Context, host string) (tls.ConnectionState, error) {
	d := tls.Dialer{Config: &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, "443"))
	if err != nil {
		return tls.ConnectionState{}, err
	}
	defer conn.Close() //nolint:errcheck // nothing was written; the handshake is all that was wanted
	tc, ok := conn.(*tls.Conn)
	if !ok {
		return tls.ConnectionState{}, errors.New("not a TLS connection")
	}
	return tc.ConnectionState(), nil
}

// timedOut reports a dial that got no answer in time, which is how a load
// balancer that admits only some source addresses looks from any other.
func timedOut(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
