// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghapp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// NewState is a fresh state value: 32 random bytes, hex. GitHub carries it
// back on the redirect, which is how the callback knows the code it is
// handed comes from the flow this process started.
func NewState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("state: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// codePattern bounds what a manifest code may be before it reaches a URL
// path: GitHub's are short hex-like tokens.
var codePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// validCode reports whether code is shaped like a manifest code.
func validCode(code string) bool { return codePattern.MatchString(code) }

// startPage posts the manifest to GitHub as soon as it loads: the browser
// half of the manifest flow. html/template escapes the manifest into the
// attribute and vets the action URL.
var startPage = template.Must(template.New("start").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="referrer" content="no-referrer">
<title>Create the patchy GitHub App</title>
</head>
<body>
<form id="manifest" method="post" action="{{.Action}}">
<input type="hidden" name="manifest" value="{{.Manifest}}">
<p>Sending the App manifest to GitHub. If nothing happens, <button type="submit">continue to GitHub</button>.</p>
</form>
<script>document.getElementById("manifest").submit()</script>
</body>
</html>
`))

// StartPage renders the page that posts manifestJSON to createURL.
func StartPage(createURL string, manifestJSON []byte) ([]byte, error) {
	var b bytes.Buffer
	if err := startPage.Execute(&b, struct {
		Action   string
		Manifest string
	}{createURL, string(manifestJSON)}); err != nil {
		return nil, fmt.Errorf("start page: %w", err)
	}
	return b.Bytes(), nil
}

// donePage is what the browser shows once the code is taken.
const donePage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>patchy</title></head>
<body><p>GitHub created the App. Return to your terminal: patchy is collecting its credentials.</p></body></html>
`

// startServedAgain answers every request for the start page after the
// first: the page carries the state, so whoever fetched it first could
// answer the flow.
const startServedAgain = "patchy served this page once already. If you did not open it before, in another tab or " +
	"browser, another program on this computer read it: stop patchy in the terminal (Ctrl-C), do not create the " +
	"App, and run patchy again."

// ErrNoCode reports a wait that ended without a code: GitHub never sent the
// browser back before the deadline, the caller gave up, or the terminal a
// code was to be pasted into closed.
var ErrNoCode = errors.New("no code from GitHub")

// Callback is the one-shot loopback server the browser flow runs on. It
// serves the start page at an unguessable path (StartURL) and takes the
// code GitHub sends the browser back with at /callback. The start page
// carries the state, so another local process that could fetch it could
// forge the callback; the random path keeps it from the port alone, and the
// page is served once only, so a process that read the path (from the
// browser opener's arguments) and fetched it first leaves the person's own
// browser with an error page instead of a flow that quietly goes on. It
// listens on 127.0.0.1 only and answers only requests addressed to that
// exact host and port, so a web page that rebinds a DNS name to the
// loopback address cannot reach it. A callback whose state differs from
// the one it was started with is refused and the wait goes on; the first
// that matches is the only code it takes, and every later callback is told
// so.
type Callback struct {
	ln   net.Listener
	srv  *http.Server
	host string
	// start is the start page's path: "/" and 32 random hex characters.
	start string

	state string
	page  []byte
	codes chan string

	mu sync.Mutex
	// served is set once the start page has been sent.
	served bool
	taken  bool
}

// Listen opens the callback's listener on a random loopback port. The
// manifest needs RedirectURL before the page can be rendered, so serving
// starts separately (Serve).
func Listen() (*Callback, error) {
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return nil, fmt.Errorf("start page path: %w", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen on the loopback address: %w", err)
	}
	return &Callback{ln: ln, host: ln.Addr().String(), start: "/" + hex.EncodeToString(token),
		codes: make(chan string, 1)}, nil
}

// StartURL is the page the browser opens to begin.
func (c *Callback) StartURL() string { return "http://" + c.host + c.start }

// RedirectURL is the manifest's redirect_url.
func (c *Callback) RedirectURL() string { return "http://" + c.host + "/callback" }

// Serve starts answering: page at the start path, once, and codes carrying
// state at /callback.
func (c *Callback) Serve(state string, page []byte) {
	c.state, c.page = state, page
	c.srv = &http.Server{Handler: c, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = c.srv.Serve(c.ln) }()
}

// Wait returns the code, once a callback brings one with the right state,
// or ErrNoCode when ctx ends first. Either way the server is shut down: it
// takes one code, ever.
func (c *Callback) Wait(ctx context.Context) (string, error) {
	defer c.Close()
	select {
	case code := <-c.codes:
		return code, nil
	case <-ctx.Done():
		return "", fmt.Errorf("%w: %w", ErrNoCode, context.Cause(ctx))
	}
}

// Close stops the server, letting a response in flight finish first.
func (c *Callback) Close() {
	if c.srv == nil {
		_ = c.ln.Close()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.srv.Shutdown(ctx)
}

// ServeHTTP answers the start page and the callback.
func (c *Callback) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	if r.Host != c.host {
		http.Error(w, "misdirected request", http.StatusMisdirectedRequest)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch r.URL.Path {
	case c.start:
		c.mu.Lock()
		served := c.served
		c.served = true
		c.mu.Unlock()
		if served {
			http.Error(w, startServedAgain, http.StatusGone)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(c.page)
	case "/callback":
		c.callback(w, r)
	default:
		http.NotFound(w, r)
	}
}

// callback takes the code from GitHub's redirect.
func (c *Callback) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(c.state)) != 1 {
		http.Error(w, "this callback does not carry the state patchy started the flow with; it is ignored",
			http.StatusBadRequest)
		return
	}
	code := q.Get("code")
	if !validCode(code) {
		http.Error(w, "the callback carries no valid code", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	taken := c.taken
	c.taken = true
	c.mu.Unlock()
	if taken {
		http.Error(w, "a code was already received; this one is ignored", http.StatusGone)
		return
	}
	c.codes <- code
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(donePage))
}

// ParseCode takes the code out of what a person pasted: the address GitHub
// sent the browser to (its state must then be this flow's), or the bare
// code, which bare reports. Nothing ties a bare code to this flow: it is
// whatever was typed, as long as it is shaped like a code.
func ParseCode(input, state string) (code string, bare bool, err error) {
	input = strings.TrimSpace(input)
	if !strings.HasPrefix(input, "https://") && !strings.HasPrefix(input, "http://") {
		if validCode(input) {
			return input, true, nil
		}
		if !strings.Contains(input, "?") {
			return "", false, errors.New("that is neither the address GitHub sent you to nor a code")
		}
		// An address copied without its scheme.
		input = "https://" + input
	}
	u, err := url.Parse(input)
	if err != nil {
		return "", false, fmt.Errorf("the address: %w", err)
	}
	q := u.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
		return "", false, errors.New("the address does not carry the state this run started the flow with: " +
			"it belongs to another attempt")
	}
	if code := q.Get("code"); validCode(code) {
		return code, false, nil
	}
	return "", false, errors.New("the address carries no code")
}
