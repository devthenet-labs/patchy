// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakegithub

import (
	"net/http"
	"time"
)

// defaultRateRemaining is the core budget the fake reports until a test sets
// another: an installation's full hour, well above any poller's floor.
const defaultRateRemaining = 5000

// rateLimit answers GET /rate_limit, which GitHub serves to any credential
// without spending it: the core REST budget left in the current window.
func (s *Server) rateLimit(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	remaining := s.rateRemaining
	s.mu.Unlock()
	core := map[string]any{
		"limit":     defaultRateRemaining,
		"used":      max(0, defaultRateRemaining-remaining),
		"remaining": remaining,
		"reset":     s.now().Add(time.Hour).Unix(),
	}
	writeJSON(w, map[string]any{"resources": map[string]any{"core": core}, "rate": core})
}

// SetRateRemaining sets the core budget GET /rate_limit reports, as an
// installation that has spent the rest of its hour.
func (s *Server) SetRateRemaining(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rateRemaining = n
}
