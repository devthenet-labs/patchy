// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ledger

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bitwise-media-group/patchy/internal/previewauth"
)

// Annotation holds the ledger on the Lease: a JSON object of hex code keys
// to the unix second they can be forgotten at.
const Annotation = "patchy.bitwisemedia.uk/redeemed-codes"

const (
	// MaxEntries caps the ledger. Codes live a minute, so this is far more
	// than any real sign-in rate.
	MaxEntries = 512
	// maxAttempts bounds the compare-and-swap retries.
	maxAttempts = 5
)

// Results reported to Record.
const (
	ResultOK       = "ok"
	ResultReplay   = "replay"
	ResultConflict = "conflict"
	ResultFull     = "full"
	ResultError    = "error"
)

// ErrFull means the ledger holds MaxEntries live codes.
var ErrFull = errors.New("ledger: full")

// Ledger records redeemed codes on one Lease. Client must not be a cached
// client: every read must see the API server's current resourceVersion.
type Ledger struct {
	Client    client.Client
	Namespace string
	Name      string
	Now       func() time.Time
	// Record, when set, is told every Consume's result.
	Record func(ctx context.Context, result string)
}

var _ previewauth.CodeLedger = (*Ledger)(nil)

// Consume records key until exp, or returns previewauth.ErrReplayed when it
// is already recorded. Entries past their expiry are dropped on the way.
func (l *Ledger) Consume(ctx context.Context, key [32]byte, exp time.Time) error {
	result, err := l.consume(ctx, key, exp)
	if l.Record != nil {
		l.Record(ctx, result)
	}
	return err
}

func (l *Ledger) consume(ctx context.Context, key [32]byte, exp time.Time) (string, error) {
	k := hex.EncodeToString(key[:])
	for range maxAttempts {
		var lease coordinationv1.Lease
		if err := l.Client.Get(ctx, client.ObjectKey{Namespace: l.Namespace, Name: l.Name}, &lease); err != nil {
			return ResultError, fmt.Errorf("ledger: get lease %s: %w", l.Name, err)
		}
		entries, err := decode(lease.Annotations[Annotation])
		if err != nil {
			// A corrupt ledger is reset rather than wedging every sign-in.
			// Codes live a minute, so at worst one in flight could redeem
			// twice, and both redemptions still need the slot's secret.
			entries = map[string]int64{}
		}
		now := l.now().Unix()
		for ek, until := range entries {
			if until <= now {
				delete(entries, ek)
			}
		}
		if _, spent := entries[k]; spent {
			return ResultReplay, previewauth.ErrReplayed
		}
		if len(entries) >= MaxEntries {
			return ResultFull, ErrFull
		}
		entries[k] = max(exp.Unix(), now+1)
		raw, err := json.Marshal(entries)
		if err != nil {
			return ResultError, fmt.Errorf("ledger: encode: %w", err)
		}
		if lease.Annotations == nil {
			lease.Annotations = map[string]string{}
		}
		lease.Annotations[Annotation] = string(raw)
		err = l.Client.Update(ctx, &lease)
		if err == nil {
			return ResultOK, nil
		}
		if !apierrors.IsConflict(err) {
			return ResultError, fmt.Errorf("ledger: update lease %s: %w", l.Name, err)
		}
	}
	return ResultConflict, fmt.Errorf("ledger: lease %s changed %d times in a row", l.Name, maxAttempts)
}

func (l *Ledger) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func decode(raw string) (map[string]int64, error) {
	entries := map[string]int64{}
	if raw == "" {
		return entries, nil
	}
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, err
	}
	if entries == nil {
		entries = map[string]int64{}
	}
	return entries, nil
}
