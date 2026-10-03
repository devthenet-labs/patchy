// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package scaffold

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"go/format"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"github.com/bitwise-media-group/patchy/internal/runnerimage"
)

//go:embed all:templates
var templates embed.FS

// Lang is an application language init app has templates for.
type Lang string

// LangGo is Go, the one language with templates so far.
const LangGo Lang = "go"

// Langs lists the languages there are templates for.
func Langs() []string { return []string{string(LangGo)} }

const (
	// PreviewPrefix is the registry path every runtime (preview) image sits
	// under, fixed by the Project CRD's imageRepository pattern, the
	// preview-controller and its admission policy. The agent prefix must
	// stay disjoint from it.
	PreviewPrefix = "patchy/previews"
	// DefaultAgentPrefix is the registry path agent images sit under by
	// default: what the operator's --repository-image-registries allows.
	DefaultAgentPrefix = "patchy/app-envs"
	// DefaultBranch is the default branch when none is given or found.
	DefaultBranch = "main"
	// ToolchainTag is the first agent image tag. The tag is versioned
	// because registry tags are immutable: a changed toolchain is published
	// as toolchain-v2, toolchain-v3 and so on, bumped in .patchy/agent.yaml.
	ToolchainTag = "toolchain-v1"
	// Port is the generated service's one port.
	Port = 8080
	// ReadinessPath is the generated service's readiness check.
	ReadinessPath = "/healthz"

	// GoVersion is the Go toolchain the generated CI, agent image and
	// runtime build use, GoImage that toolchain's image pinned by digest,
	// and goLine the go directive of a generated go.mod.
	GoVersion = "1.26.6"
	GoImage   = "golang:1.26.6@sha256:0d1d3a794be25f809dd2cb3160d8c73276c4056a9f8242a138e908ddeee7b6b6"
	goLine    = "1.26.0"
	// RuntimeImage is the distroless image a generated service's binary
	// runs on, pinned by digest: uid 65532, no shell. The same base as
	// patchy's own controller images.
	RuntimeImage = "gcr.io/distroless/static-debian13:nonroot@sha256:" +
		"1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7"
)

var (
	// registryPattern is an ECR registry host, the only kind the generated
	// publishers push to (check-config.sh holds them to the same form).
	registryPattern = regexp.MustCompile(`^([0-9]{12})\.dkr\.ecr\.([a-z]{2}(?:-[a-z]+)+-[0-9]+)\.amazonaws\.com$`)
	// pathPattern is a registry repository path as ECR (and check-config.sh)
	// accepts one: lowercase segments of alphanumeric runs joined by single
	// separators.
	pathPattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)
)

// Images are the images the generated Dockerfiles build FROM, each pinned
// by digest. The engine never resolves one: the caller does, so a
// scaffold is reproducible from its inputs.
type Images struct {
	// AgentBase is patchy's agent base image, the FROM of .patchy/Dockerfile.
	AgentBase string
	// Go is the Go toolchain image (GoImage).
	Go string
	// Runtime is the image a generated service runs on (RuntimeImage).
	Runtime string
}

// Options says what to generate.
type Options struct {
	// Repo is the GitHub repository the files are for.
	Repo Repo
	// Lang picks the templates.
	Lang Lang
	// ImageName is the image leaf both registry repositories end in.
	ImageName string
	// Registry is the ECR registry host, <account>.dkr.ecr.<region>.amazonaws.com.
	Registry string
	// AgentPrefix is the registry path the agent image sits under.
	AgentPrefix string
	// Branch is the repository's default branch.
	Branch string
	// Images are the pinned images the Dockerfiles build FROM.
	Images Images
	// Existing generates only .patchy/ and the CI publishers, for an
	// application that already has its source and runtime Dockerfile.
	Existing bool
}

// Validate checks every option, so a bad one fails before anything is
// written.
func (o Options) Validate() error {
	return errors.Join(o.ValidateTarget(), o.Images.Validate())
}

// ValidateTarget checks every option but the images: what the caller can
// check before it resolves them, which may take a registry call.
func (o Options) ValidateTarget() error {
	var errs []error
	if _, err := ParseRepo(o.Repo.String()); err != nil {
		errs = append(errs, err)
	}
	if !slices.Contains(Langs(), string(o.Lang)) {
		errs = append(errs, fmt.Errorf("language %q has no templates; choose one of %s", o.Lang,
			strings.Join(Langs(), ", ")))
	}
	if err := ValidateSlug(o.ImageName); err != nil {
		errs = append(errs, err)
	}
	if !registryPattern.MatchString(o.Registry) {
		errs = append(errs, fmt.Errorf("registry %q must be an ECR registry, <account>.dkr.ecr.<region>.amazonaws.com",
			o.Registry))
	}
	if err := validateAgentPrefix(o.AgentPrefix); err != nil {
		errs = append(errs, err)
	}
	if err := ValidateBranch(o.Branch); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Validate checks every image is a valid reference pinned by digest.
func (i Images) Validate() error {
	var errs []error
	for _, img := range []struct{ what, ref string }{
		{"agent base", i.AgentBase}, {"Go image", i.Go}, {"runtime image", i.Runtime},
	} {
		if err := validatePinned(img.ref); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", img.what, err))
		}
	}
	return errors.Join(errs...)
}

// validateAgentPrefix checks the agent path prefix, which must be a
// repository path and never equal, contain or sit inside PreviewPrefix: a
// runtime image, built from an unreviewed PR head, must never be an
// admissible agent image, nor an agent image pullable as a preview.
func validateAgentPrefix(prefix string) error {
	if !pathPattern.MatchString(prefix) {
		return fmt.Errorf("agent prefix %q must be a registry path such as %s", prefix, DefaultAgentPrefix)
	}
	if prefix == PreviewPrefix || strings.HasPrefix(prefix, PreviewPrefix+"/") ||
		strings.HasPrefix(PreviewPrefix, prefix+"/") {
		return fmt.Errorf("agent prefix %q overlaps the preview prefix %s; agent and preview images must "+
			"stay apart", prefix, PreviewPrefix)
	}
	return nil
}

// validatePinned checks an image reference is valid and pinned by digest,
// with source-controller's own reference grammar. A tag beside the digest
// is kept, as documentation of what the digest was.
func validatePinned(ref string) error {
	parsed, err := runnerimage.ParseDeclared(ref)
	if err != nil {
		return err
	}
	if parsed.Digest == "" {
		return fmt.Errorf("image %q must be pinned by digest (...@sha256:<64 hex>)", ref)
	}
	return nil
}

// AgentRepository is the agent image's registry repository path.
func (o Options) AgentRepository() string { return o.AgentPrefix + "/" + o.ImageName }

// RuntimeRepository is the runtime image's registry repository path.
func (o Options) RuntimeRepository() string { return PreviewPrefix + "/" + o.ImageName }

// AgentImage is the reference .patchy/agent.yaml declares.
func (o Options) AgentImage() string {
	return o.Registry + "/" + o.AgentRepository() + ":" + ToolchainTag
}

// Region is the registry's AWS region.
func (o Options) Region() string {
	if m := registryPattern.FindStringSubmatch(o.Registry); m != nil {
		return m[2]
	}
	return ""
}

// File is one generated file, at a slash-separated path relative to the
// repository root.
type File struct {
	Path string
	Data []byte
}

// workflow is the uncredentialed workflow that builds the runtime image:
// its file, its name (which publish-images.yml listens for), its
// concurrency group and its job.
type workflow struct{ file, name, group, job string }

var (
	// testWorkflow tests a new application and builds its runtime image, as
	// `test`.
	testWorkflow = workflow{file: "ci.yml", name: "test", group: "test", job: "test"}
	// buildWorkflow builds the runtime image of an existing application,
	// whose own CI keeps testing it, under a name unlikely to collide with
	// one of its workflows (publish-images.yml listens by name).
	buildWorkflow = workflow{file: "runtime-image.yml", name: "runtime image", group: "runtime-image", job: "build"}
)

// runtimeWorkflow is the workflow that builds the runtime image.
func (o Options) runtimeWorkflow() workflow {
	if o.Existing {
		return buildWorkflow
	}
	return testWorkflow
}

// Workflows are the paths of the workflows a scaffold generates, which
// its CI lints: the runtime image's build, the agent image's build, the
// dispatcher and the two trusted publishers.
func (o Options) Workflows() []string {
	return []string{
		".github/workflows/" + o.runtimeWorkflow().file, ".github/workflows/agent-image.yml",
		".github/workflows/publish-images.yml", ".github/workflows/publish-runtime.yml",
		".github/workflows/publish-agent.yml",
	}
}

// data is what the templates see.
type data struct {
	Repo                 Repo
	Slug                 string
	Registry             string
	Region               string
	AgentRepository      string
	RuntimeRepository    string
	AgentImage           string
	ToolchainTag         string
	Images               Images
	Branch               string
	Port                 int
	ReadinessPath        string
	GoVersion            string
	GoLine               string
	RuntimeWorkflowFile  string
	RuntimeWorkflowName  string
	RuntimeWorkflowGroup string
	RuntimeWorkflowJob   string
	Workflows            []string
	VariablesTable       string
	PublishersTable      string
	App                  bool
}

func (o Options) data() data {
	wf := o.runtimeWorkflow()
	return data{
		Repo:                 o.Repo,
		Slug:                 o.ImageName,
		Registry:             o.Registry,
		Region:               o.Region(),
		AgentRepository:      o.AgentRepository(),
		RuntimeRepository:    o.RuntimeRepository(),
		AgentImage:           o.AgentImage(),
		ToolchainTag:         ToolchainTag,
		Images:               o.Images,
		Branch:               o.Branch,
		Port:                 Port,
		ReadinessPath:        ReadinessPath,
		GoVersion:            GoVersion,
		GoLine:               goLine,
		RuntimeWorkflowFile:  wf.file,
		RuntimeWorkflowName:  wf.name,
		RuntimeWorkflowGroup: wf.group,
		RuntimeWorkflowJob:   wf.job,
		Workflows:            o.Workflows(),
		VariablesTable:       variablesTable(o),
		PublishersTable:      publishersTable(o),
		App:                  !o.Existing,
	}
}

// Plan renders the files Options asks for, in path order, without writing
// anything. The .patchy/agent.yaml it renders is checked with
// source-controller's own parser, so a scaffold never declares an image
// patchy would refuse to read.
func Plan(o Options) ([]File, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	roots := []string{"templates/base", "templates/" + string(o.Lang) + "/base"}
	if !o.Existing {
		roots = append(roots, "templates/"+string(o.Lang)+"/app")
	}
	d := o.data()
	seen := map[string]bool{}
	var files []File
	for _, root := range roots {
		err := fs.WalkDir(templates, root, func(p string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			f, err := render(root, p, d)
			if err != nil {
				return err
			}
			if seen[f.Path] {
				return fmt.Errorf("template %s: %s is generated twice", p, f.Path)
			}
			seen[f.Path] = true
			files = append(files, f)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	slices.SortFunc(files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	if err := checkDeclaration(files, o.AgentImage()); err != nil {
		return nil, err
	}
	return files, nil
}

// checkDeclaration reads the rendered .patchy/agent.yaml the way
// source-controller does and requires it to declare exactly want.
func checkDeclaration(files []File, want string) error {
	i := slices.IndexFunc(files, func(f File) bool { return f.Path == runnerimage.AgentYAMLPath })
	if i < 0 {
		return fmt.Errorf("no %s was rendered", runnerimage.AgentYAMLPath)
	}
	data := files[i].Data
	decl, err := runnerimage.Declare(runnerimage.Files{
		AgentYAML: runnerimage.File{Present: true, Size: int64(len(data)), Data: data},
	})
	if err != nil {
		return fmt.Errorf("rendered %s: %w", runnerimage.AgentYAMLPath, err)
	}
	if decl.Image != want {
		return fmt.Errorf("rendered %s declares %q, not %q", runnerimage.AgentYAMLPath, decl.Image, want)
	}
	return nil
}

// render renders one template file to the File it generates: its path
// relative to root, less .tmpl, with the runtime workflow named for the
// mode, and Go source gofmt'd.
func render(root, p string, d data) (File, error) {
	raw, err := templates.ReadFile(p)
	if err != nil {
		return File{}, err
	}
	rel := strings.TrimSuffix(strings.TrimPrefix(p, root+"/"), ".tmpl")
	if rel == ".github/workflows/runtime.yml" {
		rel = ".github/workflows/" + d.RuntimeWorkflowFile
	}
	// <% %> delimiters, since workflows are full of ${{ }} and the guard of
	// JavaScript template literals.
	tmpl, err := template.New(rel).Delims("<%", "%>").Option("missingkey=error").Parse(string(raw))
	if err != nil {
		return File{}, fmt.Errorf("template %s: %w", p, err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, d); err != nil {
		return File{}, fmt.Errorf("template %s: %w", p, err)
	}
	b := out.Bytes()
	if path.Ext(rel) == ".go" {
		if b, err = format.Source(b); err != nil {
			return File{}, fmt.Errorf("template %s: %w", p, err)
		}
	}
	return File{Path: rel, Data: b}, nil
}
