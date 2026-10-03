// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/bitwise-media-group/patchy/internal/agentrun"
)

// treesSpec is a multi-repository intent's plan Job: the planning
// repository (the web front end) and the API beside it, named the way an
// operator may name a repository.
func treesSpec() Spec {
	spec := testSpec()
	spec.Repo = "devthenet-labs/marigold-web"
	spec.Phase, spec.Kind = "plan", "intent"
	spec.Owner = "marigold-3-plan-r1-web-a1"
	spec.Finding = spec.Owner
	spec.IssueMarkdown = "# Show the API's greeting on the home page\n"
	spec.RepoKey = "web"
	spec.RepoURL = "https://github.com/devthenet-labs/marigold-web"
	spec.Trees = []Tree{{
		Key:            "api",
		URL:            "https://github.com/devthenet-labs/Acme.Web_App",
		ArtifactURL:    "http://patchy-source-controller.patchy.svc.cluster.local:9790/artifacts/feedface.tar.gz",
		ArtifactDigest: "bb22cc33dd44ee55ff6600112233445566778899aabbccddeeff00112233aa11",
		BaseSHA:        "89abcdef0123456789abcdef0123456789abcdef",
	}}
	return spec
}

// TestGoldenPlanTreesJob pins the one Job shape trees change: a brokered
// plan Job whose prepare init fetches, verifies and extracts each tree after
// its own. Nothing else of the pod moves.
func TestGoldenPlanTreesJob(t *testing.T) {
	job := buildJobForTest(t, brokeredConfig(), treesSpec())
	goldenJob(t, "job_plan_trees", job)

	plain := treesSpec()
	plain.Trees, plain.RepoKey, plain.RepoURL = nil, "", ""
	base := buildJobForTest(t, brokeredConfig(), plain)
	prep, basePrep := container(t, job, initContainerName), container(t, base, initContainerName)
	if got, want := prep.Command[2], basePrep.Command[2]+treesScript; got != want {
		t.Errorf("prepare script = %q, want the plain plan's with the trees script after it", got)
	}
	prep.Command, basePrep.Command = nil, nil
	if !reflect.DeepEqual(prep, basePrep) {
		t.Error("the prepare container differs from a plain plan's beyond its script")
	}
	if !reflect.DeepEqual(container(t, job, agentContainerName), container(t, base, agentContainerName)) {
		t.Error("the agent container differs from a plain plan's: trees reach the agent through the workspace alone")
	}
	job.Spec.Template.Spec.InitContainers, base.Spec.Template.Spec.InitContainers = nil, nil
	if !reflect.DeepEqual(job, base) {
		t.Error("the Job differs from a plain plan's beyond the prepare script")
	}
}

// TestTreesSecret: a Job with trees hands its init the fetch list and the
// manifest, one line per tree and per repository, the Job's own first; a
// Job without trees carries neither key.
func TestTreesSecret(t *testing.T) {
	spec := treesSpec()
	spec.Trees = append(spec.Trees, Tree{
		Key: "docs", URL: "https://ghe.example.com/acme/docs",
		ArtifactURL:    "https://artifacts.example/docs.tar.gz",
		ArtifactDigest: strings.Repeat("0", 64), BaseSHA: strings.Repeat("1", 64),
	})
	secret := buildSecret("job", "patchy-agents", spec)
	wantTrees := "api bb22cc33dd44ee55ff6600112233445566778899aabbccddeeff00112233aa11 " +
		"http://patchy-source-controller.patchy.svc.cluster.local:9790/artifacts/feedface.tar.gz\n" +
		"docs " + strings.Repeat("0", 64) + " https://artifacts.example/docs.tar.gz\n"
	if got := string(secret.Data["trees"]); got != wantTrees {
		t.Errorf("trees = %q, want %q", got, wantTrees)
	}
	wantRepos := "web /workspace/repo https://github.com/devthenet-labs/marigold-web\n" +
		"api /workspace/repos/api https://github.com/devthenet-labs/Acme.Web_App\n" +
		"docs /workspace/repos/docs https://ghe.example.com/acme/docs\n"
	if got := string(secret.Data[agentrun.RepositoriesManifest]); got != wantRepos {
		t.Errorf("repositories = %q, want %q", got, wantRepos)
	}

	plain := buildSecret("job", "patchy-agents", testSpec())
	for _, key := range []string{"trees", agentrun.RepositoriesManifest} {
		if _, ok := plain.Data[key]; ok {
			t.Errorf("a Job without trees carries %s", key)
		}
	}
}

// TestTreesRefused: Create refuses every Spec whose trees could reach a pod
// they were never meant for, or a line the init splits into the wrong
// fields, and creates nothing for it.
func TestTreesRefused(t *testing.T) {
	tree := func(mut func(*Tree)) func(*Spec) {
		return func(s *Spec) { mut(&s.Trees[0]) }
	}
	tests := []struct {
		name string
		cfg  func() Config
		mut  func(*Spec)
		want string
	}{
		{"a build Job", brokeredConfig, func(s *Spec) { s.Phase = "build" }, `phase "build"`},
		{"an investigation Job", brokeredConfig, func(s *Spec) { s.Phase = "investigate" }, `phase "investigate"`},
		{"a repository image, honoured", injectedConfig, func(s *Spec) {
			s.RunnerImage, s.RunnerSearchPath = repoImage, repoSearchPath
		}, "default runner image"},
		{"a repository image, switched off", brokeredConfig, func(s *Spec) { s.RunnerImage = repoImage },
			"default runner image"},
		{"too many trees", brokeredConfig, func(s *Spec) {
			for i := range 7 {
				t := s.Trees[0]
				t.Key = fmt.Sprintf("r%d", i)
				s.Trees = append(s.Trees, t)
			}
		}, "8 trees, over 7"},
		{"no key of its own", brokeredConfig, func(s *Spec) { s.RepoKey = "" }, "the Job's own repository key"},
		{"no URL of its own", brokeredConfig, func(s *Spec) { s.RepoURL = "" }, "the Job's own repository URL"},
		{"its own URL with a space", brokeredConfig, func(s *Spec) { s.RepoURL += " x" },
			"the Job's own repository URL"},
		{"an upper-case key", brokeredConfig, tree(func(t *Tree) { t.Key = "API" }), `key "API"`},
		{"a key with a slash", brokeredConfig, tree(func(t *Tree) { t.Key = "../etc" }), `key "../etc"`},
		{"a key past 16 characters", brokeredConfig, tree(func(t *Tree) { t.Key = strings.Repeat("a", 17) }),
			"at most 16"},
		{"an empty key", brokeredConfig, tree(func(t *Tree) { t.Key = "" }), `key ""`},
		{"the Job's own key", brokeredConfig, tree(func(t *Tree) { t.Key = "web" }), `key "web" is used twice`},
		{"a key twice", brokeredConfig, func(s *Spec) { s.Trees = append(s.Trees, s.Trees[0]) },
			`key "api" is used twice`},
		{"a short digest", brokeredConfig, tree(func(t *Tree) { t.ArtifactDigest = "abc" }), "not 64 hex"},
		{"an upper-case digest", brokeredConfig,
			tree(func(t *Tree) { t.ArtifactDigest = strings.ToUpper(t.ArtifactDigest) }), "not 64 hex"},
		{"a prefixed digest", brokeredConfig,
			tree(func(t *Tree) { t.ArtifactDigest = "sha256:" + t.ArtifactDigest[7:] }), "not 64 hex"},
		{"a URL with a space", brokeredConfig, tree(func(t *Tree) { t.URL += " evil" }), "trees[0] URL"},
		{"a URL with a line break", brokeredConfig,
			tree(func(t *Tree) { t.URL += "\nevil 0 http://x" }), "trees[0] URL"},
		{"an http URL", brokeredConfig,
			tree(func(t *Tree) { t.URL = strings.Replace(t.URL, "https", "http", 1) }), "trees[0] URL"},
		{"an artifact URL with a tab", brokeredConfig, tree(func(t *Tree) { t.ArtifactURL += "\tx" }),
			"artifact URL"},
		{"an artifact URL that is an option", brokeredConfig,
			tree(func(t *Tree) { t.ArtifactURL = "-o/etc/passwd" }), "artifact URL"},
		{"an artifact URL with no host", brokeredConfig, tree(func(t *Tree) { t.ArtifactURL = "https://" }),
			"artifact URL"},
		{"a file artifact URL", brokeredConfig, tree(func(t *Tree) { t.ArtifactURL = "file:///etc/passwd" }),
			"artifact URL"},
		{"an artifact URL with a control character", brokeredConfig,
			tree(func(t *Tree) { t.ArtifactURL += "\x00" }), "artifact URL"},
		{"a short commit", brokeredConfig, tree(func(t *Tree) { t.BaseSHA = "89abcdef" }), "not a commit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg()
			cs := fake.NewClientset()
			spec := treesSpec()
			tt.mut(&spec)
			_, _, err := New(cs, cfg, nil).Create(context.Background(), spec)
			if !errors.Is(err, ErrTreesRefused) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Create() error = %v, want ErrTreesRefused naming %q", err, tt.want)
			}
			assertNothingCreated(t, cs, cfg.Namespace)
		})
	}

	// The refused spec, mended, is a Job.
	if _, _, err := New(fake.NewClientset(), brokeredConfig(), nil).Create(context.Background(), treesSpec()); err != nil {
		t.Errorf("Create(treesSpec) = %v, want it created", err)
	}
}

// treeTarball is a tree's artifact as source-controller serves one: a
// gzipped tar of the repository under one top-level directory, as a forge's
// archive endpoint names it.
func treeTarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "acme-repo-0123456/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for k := range files {
			if !yield(k) {
				return
			}
		}
	}) {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: "acme-repo-0123456/" + name, Mode: 0o644,
			Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func hexDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// runTreesScript runs the trees script, as the prepare init runs it after
// its own tree is staged, under set -eu in a real sh with the pod's paths
// moved into a scratch directory, handed spec's Secret. It returns the
// workspace and how the script exited.
func runTreesScript(t *testing.T, spec Spec) (string, []byte, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the init script is POSIX sh")
	}
	for _, tool := range []string{"sh", "curl", "sha256sum", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s on PATH", tool)
		}
	}
	root := t.TempDir()
	ws, input, tmp := filepath.Join(root, "workspace"), filepath.Join(root, "input"), filepath.Join(root, "tmp")
	for _, dir := range []string{filepath.Join(ws, "input"), filepath.Join(ws, "repo"), input, tmp} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for key, body := range buildSecret("job", "patchy-agents", spec).Data {
		if err := os.WriteFile(filepath.Join(input, key), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// One pass over the script's own text, so a replacement is never
	// rewritten by another.
	script := strings.NewReplacer(workspaceDir, ws, inputMount, input, "/tmp/", tmp+"/").Replace(treesScript)
	cmd := exec.Command("sh", "-c", "set -eu\n"+script)
	out, err := cmd.CombinedOutput()
	return ws, out, err
}

// TestTreesScriptExtracts runs the init's trees script against two real
// tarballs over HTTP: each lands, verified, in its own directory as a plain
// tree with no git, the manifest is staged for agent-runner, and nothing is
// left in /tmp. A tree whose bytes do not match its digest ends the init
// before any of it is unpacked.
func TestTreesScriptExtracts(t *testing.T) {
	api := treeTarball(t, map[string]string{"main.go": "package main\n", "api/handler.go": "package api\n"})
	docs := treeTarball(t, map[string]string{"README.md": "# Docs\n"})
	tarballs := map[string][]byte{"/api.tar.gz": api, "/docs.tar.gz": docs}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := tarballs[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	spec := treesSpec()
	spec.Trees = []Tree{
		{Key: "api", URL: "https://github.com/acme/api", ArtifactURL: srv.URL + "/api.tar.gz",
			ArtifactDigest: hexDigest(api), BaseSHA: strings.Repeat("a", 40)},
		{Key: "docs", URL: "https://github.com/acme/docs", ArtifactURL: srv.URL + "/docs.tar.gz",
			ArtifactDigest: hexDigest(docs), BaseSHA: strings.Repeat("b", 40)},
	}
	if err := treesRefusal(spec); err != nil {
		t.Fatalf("the spec is refused: %v", err)
	}
	ws, out, err := runTreesScript(t, spec)
	if err != nil {
		t.Fatalf("trees script: %v\n%s", err, out)
	}
	for path, want := range map[string]string{
		"repos/api/main.go":        "package main\n",
		"repos/api/api/handler.go": "package api\n",
		"repos/docs/README.md":     "# Docs\n",
		"input/" + agentrun.RepositoriesManifest: "web /workspace/repo https://github.com/devthenet-labs/marigold-web\n" +
			"api /workspace/repos/api https://github.com/acme/api\n" +
			"docs /workspace/repos/docs https://github.com/acme/docs\n",
	} {
		got, err := os.ReadFile(filepath.Join(ws, path))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q (%v), want %q", path, got, err, want)
		}
	}
	for _, key := range []string{"api", "docs"} {
		if _, err := os.Stat(filepath.Join(ws, "repos", key, ".git")); err == nil {
			t.Errorf("tree %s has a .git: a tree is read, never built in", key)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(filepath.Dir(ws), "tmp")); len(entries) != 0 {
		t.Errorf("tmp holds %d entries after the init, want none", len(entries))
	}

	// One digest off, and the init fails before unpacking that tree.
	bad := spec
	bad.Trees = slices.Clone(spec.Trees)
	bad.Trees[1].ArtifactDigest = hexDigest(api)
	ws, out, err = runTreesScript(t, bad)
	if err == nil {
		t.Fatalf("trees script with a digest mismatch exited 0:\n%s", out)
	}
	if entries, _ := os.ReadDir(filepath.Join(ws, "repos", "docs")); len(entries) != 0 {
		t.Errorf("the mismatched tree was unpacked: %d entries", len(entries))
	}
	if _, err := os.Stat(filepath.Join(ws, "input", agentrun.RepositoriesManifest)); err == nil {
		t.Error("the manifest was staged after a failed tree: agent-runner would read it")
	}
}

// shellFields splits one line as POSIX `read -r a b c` does with the
// default IFS: on runs of spaces, tabs (and line breaks, which a line has
// none of), leading and trailing ones dropped, the last name taking the rest
// of the line with its inner separators.
func shellFields(line string, names int) []string {
	isIFS := func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' }
	rest := strings.TrimFunc(line, isIFS)
	var out []string
	for len(out) < names-1 && rest != "" {
		i := strings.IndexFunc(rest, isIFS)
		if i < 0 {
			out = append(out, rest)
			rest = ""
			break
		}
		out = append(out, rest[:i])
		rest = strings.TrimLeftFunc(rest[i:], isIFS)
	}
	if rest != "" {
		out = append(out, rest)
	}
	return out
}

// treesConfig generates trees for the round-trip property: each field
// mostly drawn from what a field may hold, now and then from what may split
// a line (whitespace of every kind, a NEL, controls), and now and then a
// field generated whole.
func treesConfig(seed int64) *quick.Config {
	safe := []string{"https://", "http://", "github.com", "/acme/app", ":9790", "/artifacts/x.tar.gz", "a", "Z",
		"-", "_", ".", "$(id)", "`id`", ";", "\\", "'", "\"", "#", "-o", "file://", "--", "0123456789abcdef", "%20"}
	// The wider whitespace by code point, so the source holds no character
	// that renders as nothing: NEL, no-break space, line separator,
	// ideographic space.
	splitting := []string{" ", "\t", "\n", "\r", "\v", "\f", "\x00", "\x1b",
		string(rune(0x0085)), string(rune(0x00A0)), string(rune(0x2028)), string(rune(0x3000))}
	gen := func(r *rand.Rand) string {
		pieces := safe
		if r.Intn(4) == 0 {
			pieces = slices.Concat(safe, splitting)
		}
		var b strings.Builder
		for range r.Intn(8) {
			b.WriteString(pieces[r.Intn(len(pieces))])
		}
		return b.String()
	}
	return &quick.Config{
		MaxCount: 3000,
		Rand:     rand.New(rand.NewSource(seed)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			trees := make([]Tree, 1+r.Intn(3))
			for i := range trees {
				tr := Tree{
					Key:            fmt.Sprintf("t%d", i),
					URL:            "https://github.com/acme/app" + gen(r),
					ArtifactURL:    []string{"http://", "https://"}[r.Intn(2)] + "src.svc:9790/a.tar.gz" + gen(r),
					ArtifactDigest: hexDigest([]byte(gen(r))),
					BaseSHA:        strings.Repeat("c", 40),
				}
				switch r.Intn(10) {
				case 0:
					tr.URL = gen(r)
				case 1:
					tr.ArtifactURL = gen(r)
				case 2:
					tr.Key = strings.TrimSpace(gen(r))
				}
				trees[i] = tr
			}
			args[0] = reflect.ValueOf(trees)
		},
	}
}

// treesRoundTrip returns how the lines Create writes for spec's accepted
// trees fail to read back, or "": the fetch list is one line per tree, each
// splitting as `read -r key digest url` splits it into exactly that tree's
// fields, and each manifest line is exactly its three fields.
func treesRoundTrip(spec Spec) string {
	lines := strings.SplitAfter(treesFile(spec.Trees), "\n")
	if lines[len(lines)-1] != "" || len(lines)-1 != len(spec.Trees) {
		return fmt.Sprintf("trees file %q is not one line per tree", treesFile(spec.Trees))
	}
	for i, tr := range spec.Trees {
		got := shellFields(strings.TrimSuffix(lines[i], "\n"), 3)
		if want := []string{tr.Key, tr.ArtifactDigest, tr.ArtifactURL}; !slices.Equal(got, want) {
			return fmt.Sprintf("line %q reads as %q, want %q", lines[i], got, want)
		}
	}
	for i, line := range strings.Split(strings.TrimSuffix(repositoriesFile(spec), "\n"), "\n") {
		if fields := strings.Split(line, " "); len(fields) != 3 || (i > 0 && fields[2] != spec.Trees[i-1].URL) {
			return fmt.Sprintf("manifest line %q is not its three fields", line)
		}
	}
	return ""
}

// TestTreesLinesRoundTripProperty: for any trees, hostile or not, Create
// either refuses them, or every line it writes for the init splits back,
// as `read -r key digest url` splits it, into exactly the tree it was
// written from — so no URL can smuggle a field, a line or an option past
// the script. The manifest's lines likewise split into exactly their three
// fields. A real sh confirms the reading over every accepted line.
func TestTreesLinesRoundTripProperty(t *testing.T) {
	var accepted []Tree
	refused := 0
	var failure string
	holds := func(trees []Tree) bool {
		spec := treesSpec()
		spec.Trees = trees
		if treesRefusal(spec) != nil {
			refused++
			return true
		}
		if failure = treesRoundTrip(spec); failure != "" {
			return false
		}
		accepted = append(accepted, trees...)
		return true
	}
	if err := quick.Check(holds, treesConfig(20261003)); err != nil {
		t.Fatalf("%v\n%s", err, failure)
	}
	if refused < 300 || len(accepted) < 1000 {
		t.Fatalf("refused %d cases and accepted %d trees; the generator stopped reaching one side",
			refused, len(accepted))
	}
	checkShellReads(t, accepted)
}

// checkShellReads has a real sh read the fetch list of every accepted tree
// as the init script does, and checks each line reads back as its tree.
func checkShellReads(t *testing.T, accepted []Tree) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	if _, err := exec.LookPath("sh"); err != nil {
		return
	}
	const unitSep, recordSep = "\x1f", "\x1e"
	cmd := exec.Command("sh", "-c",
		`while read -r key digest url; do printf '%s\037%s\037%s\036' "$key" "$digest" "$url"; done`)
	cmd.Stdin = strings.NewReader(treesFile(accepted))
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("sh: %v", err)
	}
	records := strings.Split(strings.TrimSuffix(string(raw), recordSep), recordSep)
	if len(records) != len(accepted) {
		t.Fatalf("sh read %d lines, want %d", len(records), len(accepted))
	}
	for i, rec := range records {
		tr := accepted[i]
		if want := tr.Key + unitSep + tr.ArtifactDigest + unitSep + tr.ArtifactURL; rec != want {
			t.Errorf("sh read line %d as %q, want %q", i, rec, want)
		}
	}
}

// TestTreesOnlyOnTheirJob: every Job but a plan with trees is the Job it was
// before: the Finding stages, an intent build, and a one-repository plan,
// on every runner flavour — no trees key, no trees script, the goldens
// pinning the rest.
func TestTreesOnlyOnTheirJob(t *testing.T) {
	plan := testSpec()
	plan.Phase, plan.Kind = "plan", "intent"
	build := testSpec()
	build.Phase, build.Kind, build.InvestigationMarkdown = "build", "intent", "# Approved plan\n"
	for _, cfg := range []func() Config{testConfig, brokeredConfig, injectedConfig} {
		for _, spec := range []Spec{testSpec(), injectedSpec(), plan, build} {
			job := buildJobForTest(t, cfg(), spec)
			if strings.Contains(container(t, job, initContainerName).Command[2], "while read -r key digest url") {
				t.Errorf("%s Job without trees runs the trees script", spec.Phase)
			}
			for key := range buildSecret(job.Name, "patchy-agents", spec).Data {
				if key != secretKeyIssue && key != secretKeyInvestigation {
					t.Errorf("%s Job without trees carries Secret key %q", spec.Phase, key)
				}
			}
		}
	}
	// The trees are the init's alone: the agent container mounts no input.
	job := buildJobForTest(t, brokeredConfig(), treesSpec())
	for _, m := range container(t, job, agentContainerName).VolumeMounts {
		if m.Name == volInput {
			t.Errorf("the agent container mounts the per-Job Secret at %s", m.MountPath)
		}
	}
}
