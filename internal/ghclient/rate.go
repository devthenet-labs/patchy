// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"errors"
	"fmt"
)

// RateRemaining returns how many core REST requests the credential has left
// in its current rate-limit window (GET /rate_limit, which itself costs
// nothing). For an installation token that is the installation's budget,
// shared by every token minted from it, which is what a poller backing off
// before it starves other work needs to read. GitHub's figures are not
// consistent from one response to the next, so the value is a coarse guard,
// never an exact budget.
func (c *Client) RateRemaining(ctx context.Context) (int, error) {
	limits, _, err := c.gh.RateLimit.Get(ctx)
	if err != nil {
		return 0, fmt.Errorf("ghclient: rate limit: %w", err)
	}
	if limits.GetCore() == nil {
		return 0, errors.New("ghclient: rate limit: GitHub reported no core limit")
	}
	return limits.GetCore().Remaining, nil
}
