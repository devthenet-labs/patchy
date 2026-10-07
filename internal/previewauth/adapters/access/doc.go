// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package access is the preview sign-in relay's previewauth.Authorizer: a
// SubjectAccessReview for get on the virtual subresource projects/previews,
// named for the Preview's Project, through web/authz's ProjectReviewer (the
// dashboard's own per-Project reviews, cache included). Viewers are whoever
// the chart's RBAC bindings name; nothing here knows a team or a user.
package access
