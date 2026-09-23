// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import (
	"errors"
	"fmt"
)

// Rejection is a deterministic refusal of a declared image or of the file
// declaring it: the outcome cannot change on retry, so the caller records it
// (Stalled / RunnerImageRejected) instead of backing off. Message is fixed for
// its cause and is shown to humans verbatim.
type Rejection struct {
	// Reason is a short PascalCase label for the cause (NotAllowlisted,
	// Oversized, Unsigned, ...), the value status.runnerImage.rejected
	// records beside the message. The pure checks in this package leave it
	// empty and the caller labels them by stage; the registry-backed
	// resolver sets it, since only it knows which of its checks failed.
	Reason string
	// Message is the human explanation, shown verbatim.
	Message string
}

// Error implements error.
func (r *Rejection) Error() string { return r.Message }

// IsRejection reports whether err is, or wraps, a Rejection.
func IsRejection(err error) bool {
	var r *Rejection
	return errors.As(err, &r)
}

// reject builds a Rejection with a formatted message.
func reject(format string, args ...any) error {
	return &Rejection{Message: fmt.Sprintf(format, args...)}
}
