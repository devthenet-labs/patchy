// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpapi

import (
	"html/template"
	"net/http"
)

// page is every HTML answer: a title, a sentence, and on the home page a
// sign-out button. No script, no style, no external resource.
var page = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<meta name="robots" content="noindex"><title>{{.Title}}</title></head>
<body><h1>{{.Title}}</h1><p>{{.Message}}</p>
{{- if .SignOut}}<form method="post" action="/logout"><button type="submit">Sign out of previews</button></form>{{end}}
</body></html>
`))

type pageData struct {
	Title, Message string
	SignOut        bool
}

func renderPage(w http.ResponseWriter, status int, d pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = page.Execute(w, d)
}

// The pages' wording. None of them names a Preview, a Project or the
// viewer.
var (
	pageBadRequest = pageData{Title: "Sign-in request not valid",
		Message: "This sign-in request is not one patchy's preview sign-in can answer. Open the preview link again."}
	pageNoPreview = pageData{Title: "No live preview",
		Message: "There is no live preview at this address. It may have expired or been replaced."}
	pageDenied = pageData{Title: "Not a preview viewer",
		Message: "You are signed in, but you are not a preview viewer for this project. Ask an operator to add you."}
	pageUnavailable = pageData{Title: "Try again shortly",
		Message: "Preview sign-in is temporarily unavailable. Try again in a moment."}
	pageSignInExpired = pageData{Title: "Sign-in expired",
		Message: "This sign-in was not started in this browser or took too long. Open the preview link again."}
	pageUpstreamRefused = pageData{Title: "Sign-in refused",
		Message: "The identity provider did not sign you in. Open the preview link to try again."}
	pageUpstreamFailed = pageData{Title: "Sign-in failed",
		Message: "Signing you in with the identity provider failed. Open the preview link to try again."}
	pageTooManyGroups = pageData{Title: "Too many groups",
		Message: "Your account belongs to more groups than a preview sign-in session can hold. " +
			"Ask an operator to narrow the groups the identity provider sends."}
	pageNotFound = pageData{Title: "Not found", Message: "There is nothing here."}
	pageHome     = pageData{Title: "patchy preview sign-in",
		Message: "This service signs viewers in to patchy previews.", SignOut: true}
	pageSignedOut = pageData{Title: "Signed out",
		Message: "You are signed out of preview sign-in. Previews you already opened stay open until their " +
			"own session ends."}
	pageForbidden   = pageData{Title: "Refused", Message: "This request must come from this page."}
	pageRateLimited = pageData{Title: "Too many requests", Message: "Slow down and try again in a moment."}
	pageNotAllowed  = pageData{Title: "Method not allowed", Message: "Only GET and POST are served."}
)
