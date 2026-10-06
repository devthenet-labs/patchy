// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/forge"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/ghsecret"
	"github.com/bitwise-media-group/patchy/internal/intentperm"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

// TestCredentialsKeptBetweenCalls: the production GitHub reads a Forge's
// Secret once for a run of calls with one repository and permission set, not
// once per call, and reads it again once the credential is kept long enough,
// or when the Forge changes.
func TestCredentialsKeptBetweenCalls(t *testing.T) {
	ctx := context.Background()
	forgeObj := &v1alpha1.Forge{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "github"},
		Spec: v1alpha1.ForgeSpec{Provider: v1alpha1.ForgeProviderGitHub,
			SecretRef: v1alpha1.LocalSecretReference{Name: "cred"}},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "cred"},
		Data:       map[string][]byte{ghsecret.KeyToken: []byte("ghp_dev")},
	}
	reads := 0
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(forgeObj, secret).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					reads++
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	clock := &fakeClock{t: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
	g := NewForgeGitHub(forge.NewStore(c), testNS).(*forgeGitHub)
	g.now = clock.Now

	call := func() {
		t.Helper()
		for _, url := range []string{intentRepoURL, intentRepoURL, appRepoURL} {
			if _, _, err := g.client(ctx, url, issuesWrite); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := g.client(ctx, appRepoURL, contentsWrite); err != nil {
			t.Fatal(err)
		}
		if login, err := g.BotLogin(ctx, intentRepoURL); err != nil || login != "" {
			t.Fatalf("BotLogin = %q, %v; a personal access token has no bot", login, err)
		}
	}
	call()
	// One read each for (intents, issues), (app, issues), (app, contents)
	// and the bot login.
	if reads != 4 {
		t.Fatalf("%d Secret reads for the first calls, want 4", reads)
	}
	for range 5 {
		clock.Advance(time.Minute)
		call()
	}
	if reads != 4 {
		t.Errorf("%d Secret reads after repeated calls, want still 4", reads)
	}
	clock.Advance(credentialReread)
	call()
	if reads != 8 {
		t.Errorf("%d Secret reads once the credentials were kept %s, want 8", reads, credentialReread)
	}
	// A changed Forge is read again at once.
	var f v1alpha1.Forge
	if err := c.Get(ctx, client.ObjectKeyFromObject(forgeObj), &f); err != nil {
		t.Fatal(err)
	}
	f.Spec.Orgs = []string{"acme"}
	if err := c.Update(ctx, &f); err != nil {
		t.Fatal(err)
	}
	call()
	if reads != 12 {
		t.Errorf("%d Secret reads after the Forge changed, want 12", reads)
	}
}

// The repositories intent-controller calls a GitHub method with, for
// TestEveryTokenIsInTheTable.
const (
	onIntentRepository   = "the intent repository"
	onAppRepository      = "an application repository"
	onCheckFixRepository = "an application repository, on a check-fix round"
)

// tokenUses declares, for every method of GitHub, the repositories
// intent-controller calls it with; nil for a method that mints no token of
// its own. A method added to GitHub must be declared here.
var tokenUses = map[string][]string{
	// Resolve and BotLogin mint no token; Installed mints the one it is
	// asked for, which is how Ready proves the table.
	"Resolve": nil, "Installed": nil, "BotLogin": nil,
	// authorize (the intent issue's commands) and authorizeIn (a pull
	// request's reviews and comments).
	"Permission": {onIntentRepository, onAppRepository},
	// discover and the intent issue's poll; rateOK before an application
	// repository's pull requests or default branch are read.
	"RateRemaining": {onIntentRepository, onAppRepository},

	"EnsureLabel": {onIntentRepository}, "ListIssues": {onIntentRepository},
	"GetIssue": {onIntentRepository}, "ListIssueEvents": {onIntentRepository},
	"ListIssueComments": {onIntentRepository}, "GetIssueComment": {onIntentRepository},
	"CommentEdited": {onIntentRepository}, "CreateIssueComment": {onIntentRepository},
	"EditIssueComment": {onIntentRepository}, "React": {onIntentRepository},
	"RemoveLabel": {onIntentRepository}, "CloseIssue": {onIntentRepository},

	"DefaultBranch": {onAppRepository}, "HeadSHA": {onAppRepository},
	"CreateCommit": {onAppRepository}, "CreateBranchRef": {onAppRepository},
	"FastForwardRef": {onAppRepository}, "FindPullRequest": {onAppRepository},
	"CreatePullRequest": {onAppRepository}, "GetPullRequest": {onAppRepository},
	"ListPullRequestComments": {onAppRepository}, "GetPullRequestComment": {onAppRepository},
	"PullRequestCommentEdited": {onAppRepository}, "CreatePullRequestComment": {onAppRepository},
	"ReactPullRequestComment": {onAppRepository}, "ListPullRequestReviews": {onAppRepository},
	"ListPullRequestReviewComments": {onAppRepository}, "ReviewEdited": {onAppRepository},
	"ReviewCommentEdited": {onAppRepository}, "ComparePatch": {onAppRepository},
	"RequestReviewers": {onAppRepository},

	"ListCheckRuns": {onCheckFixRepository}, "ListCheckAnnotations": {onCheckFixRepository},
	"ListCommitStatuses": {onCheckFixRepository}, "ListWorkflowJobs": {onCheckFixRepository},
	"GetJobLogTail": {onCheckFixRepository},
}

// TestEveryTokenIsInTheTable is the other half of
// TestProjectValidationMintsTheTable: every token the production GitHub
// mints is a grant of the intentperm table for the repository it is minted
// on, so a Project's Ready, which proves the table, proves every token its
// intents will ask for. Each method is called on each repository tokenUses
// declares, against a Forge whose API answers everything with 404, and the
// tokens it took are read off its credential cache.
//
// One read is outside the table by design, and so outside this test: while an
// intent is open, the pull request it opened in a repository its Project no
// longer lists is still read, read-only (GetPullRequest with pulls read, and
// RateRemaining with issues read for that installation's floor), so its merge
// counts towards the ending (readDepartedPullRequest). The Project has no row
// for that repository to hold it to, nothing is written there, and once
// refused there it is not asked again for departedRetry.
func TestEveryTokenIsInTheTable(t *testing.T) {
	store, spec := tokenFixture(t)

	gh := reflect.TypeFor[GitHub]()
	for i := range gh.NumMethod() {
		m := gh.Method(i)
		on, declared := tokenUses[m.Name]
		if !declared {
			t.Errorf("GitHub.%s is not in tokenUses: declare the repositories intent-controller calls it with", m.Name)
			continue
		}
		if len(on) == 0 {
			if minted := mintedBy(t, store, m, spec.IntentRepository); len(minted) != 0 {
				t.Errorf("GitHub.%s minted %+v; declare the repositories it is called with", m.Name, minted)
			}
			continue
		}
		for _, use := range on {
			url, grants := tableFor(t, spec, use)
			checkTokensGranted(t, m.Name, use, url, grants, mintedBy(t, store, m, url))
		}
	}
	for name := range tokenUses {
		if _, ok := gh.MethodByName(name); !ok {
			t.Errorf("tokenUses declares %s, which GitHub has no method for", name)
		}
	}
}

// TestCreatePullRequestCanReadTheRefs pins the token that opens a pull
// request. Pull requests write alone opens one only in a public repository:
// in a private one GitHub refuses with "not all refs are readable", because
// the token cannot read the head and base it names, so the token reads
// contents too (overdub-12, the first intent built in a private repository).
func TestCreatePullRequestCanReadTheRefs(t *testing.T) {
	store, spec := tokenFixture(t)
	m, ok := reflect.TypeFor[GitHub]().MethodByName("CreatePullRequest")
	if !ok {
		t.Fatal("GitHub has no CreatePullRequest")
	}
	minted := mintedBy(t, store, m, spec.Repositories[0].URL)
	want := ghclient.TokenPerms{PullRequests: ghclient.PermWrite, Contents: ghclient.PermRead}
	if len(minted) != 1 || minted[0].perms != want {
		t.Fatalf("CreatePullRequest minted %+v, want one token with %+v", minted, want)
	}
}

// tokenFixture is a Forge whose API answers everything with 404, its
// credential Secret, the store minting tokens from them, and a Project spec
// with an intent repository and one application repository on that Forge.
func tokenFixture(t *testing.T) (*forge.Store, v1alpha1.ProjectSpec) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	t.Cleanup(srv.Close)
	forgeObj := &v1alpha1.Forge{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "github"},
		Spec: v1alpha1.ForgeSpec{Provider: v1alpha1.ForgeProviderGitHub, BaseURL: srv.URL,
			SecretRef: v1alpha1.LocalSecretReference{Name: "cred"}},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "cred"},
		Data:       map[string][]byte{ghsecret.KeyToken: []byte("ghp_dev")},
	}
	store := forge.NewStore(fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(forgeObj, secret).Build())
	spec := v1alpha1.ProjectSpec{IntentRepository: srv.URL + "/acme/intents",
		Repositories: []v1alpha1.ProjectRepository{{Name: "app", URL: srv.URL + "/acme/app"}}}
	return store, spec
}

// tableFor is the repository URL and the intentperm grants of the
// repository use names in a Project shaped like spec: the intent
// repository's row, or the application repository's, with spec.checks.fix
// set for a check-fix round.
func tableFor(t *testing.T, spec v1alpha1.ProjectSpec, use string) (string, []intentperm.Grant) {
	t.Helper()
	role := intentperm.RoleApp
	switch use {
	case onIntentRepository:
		role = intentperm.RoleIntent
	case onCheckFixRepository:
		spec.Checks.Fix = []string{"test"}
	}
	for _, need := range intentperm.For(&spec) {
		if need.Role == role {
			return need.URL, need.Grants
		}
	}
	t.Fatalf("intentperm.For has no %s row", role)
	return "", nil
}

// mintedBy calls m of a fresh production GitHub over store with url as its
// repository and every other argument zero, and returns the credentials it
// took: the repository and permission set of every token it minted.
func mintedBy(t *testing.T, store *forge.Store, m reflect.Method, url string) []credKey {
	t.Helper()
	g := NewForgeGitHub(store, testNS).(*forgeGitHub)
	if m.Type.NumIn() < 2 || m.Type.In(1).Kind() != reflect.String {
		t.Fatalf("GitHub.%s does not take the repository URL after the context", m.Name)
	}
	args := []reflect.Value{reflect.ValueOf(t.Context()), reflect.ValueOf(url)}
	for i := 2; i < m.Type.NumIn(); i++ {
		args = append(args, reflect.Zero(m.Type.In(i)))
	}
	// The API answers 404, so the call itself fails: only the tokens it
	// took on the way matter.
	reflect.ValueOf(g).MethodByName(m.Name).Call(args)
	g.mu.Lock()
	defer g.mu.Unlock()
	minted := make([]credKey, 0, len(g.creds))
	for k := range g.creds {
		minted = append(minted, k)
	}
	return minted
}

// checkTokensGranted fails unless method, called on url (the repository
// use), minted at least one token, every one of them on url and each a
// grant of the table for it.
func checkTokensGranted(t *testing.T, method, use, url string, grants []intentperm.Grant, minted []credKey) {
	t.Helper()
	_, repo, err := forge.ParseRepoURL(url)
	if err != nil {
		t.Fatal(err)
	}
	if len(minted) == 0 {
		t.Errorf("GitHub.%s on %s minted no token; tokenUses says it reads that repository", method, use)
	}
	for _, k := range minted {
		if k.repo != strings.ToLower(repo.String()) {
			t.Errorf("GitHub.%s on %s minted a token for %s, not %s", method, use, k.repo, repo)
		}
		if !granted(k.perms, grants) {
			t.Errorf("GitHub.%s on %s mints a token with %+v, which the intentperm table does not grant there "+
				"(%v), so a Ready Project does not prove it", method, use, k.perms, grants)
		}
	}
}

// granted reports whether grants cover perms, a token's permission set: each
// permission it asks for is one of grants at the access asked for, or at
// write, which includes read.
func granted(perms ghclient.TokenPerms, grants []intentperm.Grant) bool {
	parts := []ghclient.TokenPerms{
		{Contents: perms.Contents}, {Issues: perms.Issues}, {PullRequests: perms.PullRequests},
		{Checks: perms.Checks}, {Statuses: perms.Statuses}, {Actions: perms.Actions},
	}
	asked := 0
	for _, part := range parts {
		if part == (ghclient.TokenPerms{}) {
			continue
		}
		asked++
		if !grantedOne(part, grants) {
			return false
		}
	}
	return asked > 0
}

// grantedOne reports whether one of grants covers perms, a single
// permission: the same permission at the access asked for, or at write.
func grantedOne(perms ghclient.TokenPerms, grants []intentperm.Grant) bool {
	for _, g := range grants {
		accesses := []string{g.Access}
		if g.Access == intentperm.Write {
			accesses = append(accesses, intentperm.Read)
		}
		for _, access := range accesses {
			if p, err := tokenPerms(intentperm.Grant{Permission: g.Permission, Access: access}); err == nil &&
				p == perms {
				return true
			}
		}
	}
	return false
}
