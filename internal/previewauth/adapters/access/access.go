// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package access

import (
	"context"
	"fmt"

	"github.com/bitwise-media-group/patchy/internal/previewauth"
	"github.com/bitwise-media-group/patchy/internal/web/auth"
	"github.com/bitwise-media-group/patchy/internal/web/authz"
)

// Reviewer answers previewauth access reviews.
type Reviewer struct {
	Projects *authz.ProjectReviewer
	// Record, when set, is told every decided review's answer.
	Record func(ctx context.Context, allowed bool)
}

var _ previewauth.Authorizer = Reviewer{}

// Allowed reviews get on projects/<subresource> for review's Project. Only
// the previews subresource is ever asked: anything else is an error, so a
// confused caller fails closed.
func (r Reviewer) Allowed(ctx context.Context, review previewauth.AccessReview) (bool, error) {
	if review.Subresource != authz.SubresourcePreviews {
		return false, fmt.Errorf("preview access review for subresource %q", review.Subresource)
	}
	id := auth.Identity{Username: review.Username, Groups: append([]string(nil), review.Groups...), Session: true}
	ok, err := r.Projects.Allowed(ctx, id, review.Project, authz.SubresourcePreviews)
	if err == nil && r.Record != nil {
		r.Record(ctx, ok)
	}
	return ok, err
}
