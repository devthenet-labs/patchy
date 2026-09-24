// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"errors"
	"fmt"
)

// Slug returns the App's slug (GET /app, authenticated as the App itself),
// read once and cached: an App ID's slug does not change under a running
// process.
func (a *App) Slug(ctx context.Context) (string, error) {
	a.mu.Lock()
	slug := a.slug
	a.mu.Unlock()
	if slug != "" {
		return slug, nil
	}
	app, _, err := a.gh.Apps.Get(ctx, "")
	if err != nil {
		return "", fmt.Errorf("ghclient: get app: %w", err)
	}
	slug = app.GetSlug()
	if slug == "" {
		return "", errors.New("ghclient: get app: GitHub reported no slug")
	}
	a.mu.Lock()
	a.slug = slug
	a.mu.Unlock()
	return slug, nil
}

// BotLogin returns the login of the App's bot user, "<slug>[bot]": the
// actor GitHub records, with type "Bot", on everything the App's
// installation tokens do. Label events carry no performed_via_github_app,
// so this login is how the App recognises its own events.
func (a *App) BotLogin(ctx context.Context) (string, error) {
	slug, err := a.Slug(ctx)
	if err != nil {
		return "", err
	}
	return slug + "[bot]", nil
}
