// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/bitwise-media-group/patchy/internal/provider"
)

// inspection is what the broker learned from a buffered request body.
type inspection struct {
	// model is the body's "model" field, "" when absent.
	model string
	// maxTokens is the body's "max_tokens", 0 when absent.
	maxTokens int64
	// estimate is the token charge for a response whose usage is never
	// seen: the body size over four, the usual bytes-per-token ratio.
	estimate int64
	// rewritten is the body to forward in place of the original when a
	// denied anthropic_beta entry was stripped from it; nil otherwise.
	rewritten []byte
}

// invalidRequestError is a body the upstream API would itself refuse as
// malformed, answered 400 rather than the 403 of a refused capability.
type invalidRequestError struct{ msg string }

func (e invalidRequestError) Error() string { return e.msg }

// deniedToolPrefixes are the server-side tool families that reach the
// internet or the Files API on the pod's behalf; mcp_toolset is the exact
// type the MCP connector uses.
var deniedToolPrefixes = []string{"web_search_", "web_fetch_", "code_execution_"}

// deniedToolExact are server-side tool types denied by exact name.
var deniedToolExact = map[string]bool{"mcp_toolset": true}

// deniedTopLevel are request fields that bring a server-side resource into
// the call: remote MCP servers and code-execution containers.
var deniedTopLevel = []string{"mcp_servers", "container"}

// inlineSourceTypes are the content-block source types that carry their
// data in the request itself. Anything else — "url" (an upstream fetch on
// the pod's behalf), "file" (a Files API reference), or a type added after
// this list — is refused: an allowlist, because a new remote source type
// must not be forwarded by default.
var inlineSourceTypes = map[string]bool{"base64": true, "text": true, "content": true}

// bodyBetaField is where bedrock and vertex carry beta opt-ins: in the body,
// not the anthropic-beta header.
const bodyBetaField = "anthropic_beta"

// inspectBody parses a messages-shaped body and refuses anything that would
// have the upstream reach beyond model inference: a denied server tool, an
// MCP server, a container, a Files API reference or a non-inline content
// source. It strips denied entries from a body anthropic_beta array exactly
// as filterBetas does for the header. An empty body passes (a listing has
// none); a body that is not a JSON object, a max_tokens that is not a plain
// non-negative integer or an anthropic_beta that is not an array of strings
// is an invalidRequestError; a body that is not a JSON object is refused.
func inspectBody(body []byte, deny []string) (inspection, error) {
	insp := inspection{estimate: int64(len(body)) / 4}
	if len(bytes.TrimSpace(body)) == 0 {
		return insp, nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var top map[string]any
	if err := dec.Decode(&top); err != nil {
		return insp, errors.New("request body is not a JSON object")
	}
	for _, key := range deniedTopLevel {
		if v, ok := top[key]; ok && v != nil {
			return insp, fmt.Errorf("request field %q is not brokered", key)
		}
	}
	if tools, ok := top["tools"].([]any); ok {
		for _, t := range tools {
			tool, ok := t.(map[string]any)
			if !ok {
				continue
			}
			if typ, _ := tool["type"].(string); deniedToolType(typ) {
				return insp, fmt.Errorf("server tool %q is not brokered", clip(typ))
			}
		}
	}
	if err := walkReferences(top); err != nil {
		return insp, err
	}
	insp.model, _ = top["model"].(string)
	if raw, ok := top["max_tokens"]; ok {
		n, ok := plainInt(raw)
		if !ok {
			return insp, invalidRequestError{"max_tokens must be a non-negative integer"}
		}
		insp.maxTokens = n
	}
	rewritten, err := filterBodyBetas(top, deny)
	if err != nil {
		return insp, err
	}
	insp.rewritten = rewritten
	return insp, nil
}

// plainInt reads a JSON number that is a plain non-negative integer: no
// sign, fraction or exponent, and within int64. Anything else — a float the
// upstream might round, an exponent, a numeric string — is not one, so a
// ceiling can never be dodged by a spelling it does not parse.
func plainInt(v any) (int64, bool) {
	num, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	s := num.String()
	if s == "" || strings.ContainsAny(s, ".eE+-") {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// filterBodyBetas strips denied entries from the body's anthropic_beta
// array, returning the re-encoded body when anything was stripped (the
// field is dropped when nothing survives, as the header is) and nil when
// the body is forwarded unchanged.
func filterBodyBetas(top map[string]any, deny []string) ([]byte, error) {
	raw, ok := top[bodyBetaField]
	if !ok {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, invalidRequestError{bodyBetaField + " must be an array of strings"}
	}
	keep := make([]any, 0, len(list))
	for _, e := range list {
		entry, ok := e.(string)
		if !ok {
			return nil, invalidRequestError{bodyBetaField + " must be an array of strings"}
		}
		if !betaDenied(strings.TrimSpace(entry), deny) {
			keep = append(keep, entry)
		}
	}
	if len(keep) == len(list) {
		return nil, nil
	}
	if len(keep) == 0 {
		delete(top, bodyBetaField)
	} else {
		top[bodyBetaField] = keep
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(top); err != nil {
		return nil, fmt.Errorf("re-encode request body: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// deniedToolType reports whether a tools[].type is a denied server tool.
func deniedToolType(typ string) bool {
	if deniedToolExact[typ] {
		return true
	}
	for _, p := range deniedToolPrefixes {
		if strings.HasPrefix(typ, p) {
			return true
		}
	}
	return false
}

// errFileReference is the refusal for a Files API reference.
var errFileReference = errors.New("file references are not brokered")

// walkReferences refuses, at any depth outside caller-defined data, a
// file_id key (a document or container_upload block naming a Files API
// object) and a content source that is not inline. Caller-defined data is
// what the API never dereferences — a tool's input JSON Schema, a
// structured-output schema, a tool call's input — so a property that
// happens to be called file_id or source there is not a reference.
func walkReferences(v any) error {
	switch x := v.(type) {
	case map[string]any:
		if _, ok := x["file_id"]; ok {
			return errFileReference
		}
		if src, ok := x["source"].(map[string]any); ok {
			typ, _ := src["type"].(string)
			switch {
			case typ == "file":
				return errFileReference
			case !inlineSourceTypes[typ]:
				return fmt.Errorf("content source type %q is not brokered", clip(typ))
			}
		}
		for key, child := range x {
			if callerData(x, key) {
				continue
			}
			if err := walkReferences(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range x {
			if err := walkReferences(child); err != nil {
				return err
			}
		}
	}
	return nil
}

// callerData reports whether obj[key] is caller-defined data rather than
// request structure: a tool definition's input_schema, the schema of a
// json_schema output format, or the input of a tool_use-family block.
func callerData(obj map[string]any, key string) bool {
	typ, _ := obj["type"].(string)
	switch key {
	case "input_schema":
		return true
	case "schema":
		return typ == "json_schema"
	case "input":
		return strings.HasSuffix(typ, "tool_use")
	}
	return false
}

// modelPolicy is the effective model allowlist. Entries are compared in
// their bare, lower-case form (a canonical "anthropic/claude-sonnet-5" and
// the wire "claude-sonnet-5" are the same entry), and a wire id matches an
// entry exactly or with only a dated/versioned suffix appended: the
// "-20260514", "@20260514" and "-v1:0" forms the providers add. The CLI's
// helper model family is always admitted beside the configured entries,
// because every run calls it.
type modelPolicy struct {
	entries []string
}

// newModelPolicy returns nil (no restriction) for an empty list.
func newModelPolicy(list []string) *modelPolicy {
	if len(list) == 0 {
		return nil
	}
	p := &modelPolicy{}
	for _, e := range list {
		if e = bareModel(e); e != "" {
			p.entries = append(p.entries, e)
		}
	}
	return p
}

// modelSuffix is what may follow an entry in a wire id: nothing, a dated
// snapshot with an optional bedrock version, or a bare bedrock version.
var modelSuffix = regexp.MustCompile(`^([-@][0-9]{8}(-v[0-9]+:[0-9]+)?|-v[0-9]+:[0-9]+)?$`)

// allows reports whether wire, a model id as it appears on the request, is
// admitted. A nil policy admits everything.
func (p *modelPolicy) allows(wire string) bool {
	if p == nil {
		return true
	}
	w := bareModel(wire)
	if w == "" {
		return false
	}
	for _, e := range p.entries {
		if rest, ok := strings.CutPrefix(w, e); ok && modelSuffix.MatchString(rest) {
			return true
		}
	}
	for _, family := range HelperModelFamilies {
		if w == family || strings.HasPrefix(w, family+"-") {
			return true
		}
	}
	return false
}

// bareModel normalizes an entry or a wire id with provider.BareModelID, the
// normalisation the controllers' model maps are built against, after
// unwrapping a Bedrock ARN to the model id it names. An ARN of any other
// resource type normalizes to "", which no entry matches.
func bareModel(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if strings.HasPrefix(id, "arn:") {
		id = arnModel(id)
		if id == "" {
			return ""
		}
	}
	return provider.BareModelID(id)
}

// arnModel returns the model id a Bedrock ARN names —
// arn:<partition>:bedrock:<region>:<account>:foundation-model/<id> or
// …:inference-profile/<id> — and "" for any other ARN: an application
// inference profile, a provisioned or custom model, or another service's
// resource names no model the allowlist can judge.
func arnModel(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || !strings.HasPrefix(parts[1], "aws") || parts[2] != "bedrock" {
		return ""
	}
	typ, id, ok := strings.Cut(parts[5], "/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return ""
	}
	switch typ {
	case "foundation-model", "inference-profile":
		return id
	}
	return ""
}

// maxEcho bounds a caller-controlled string (a model id, a path, a type
// name) before it reaches an error message or an audit line.
const maxEcho = 256

// clip truncates s to maxEcho bytes, marking the cut.
func clip(s string) string {
	if len(s) <= maxEcho {
		return s
	}
	return s[:maxEcho] + "...(truncated)"
}

// betaHeader is the header the CLI opts into beta API features with.
const betaHeader = "anthropic-beta"

// filterBetas strips every anthropic-beta entry matching a deny pattern and
// rewrites the header as one comma-joined value, or removes it when nothing
// survives. Patterns are path.Match globs over the lower-cased entry.
func filterBetas(h http.Header, deny []string) {
	values := h.Values(betaHeader)
	if len(values) == 0 {
		return
	}
	var keep []string
	for _, v := range values {
		for entry := range strings.SplitSeq(v, ",") {
			entry = strings.TrimSpace(entry)
			if entry != "" && !betaDenied(entry, deny) {
				keep = append(keep, entry)
			}
		}
	}
	if len(keep) == 0 {
		h.Del(betaHeader)
		return
	}
	h.Set(betaHeader, strings.Join(keep, ","))
}

// betaDenied reports whether entry matches any deny pattern.
func betaDenied(entry string, deny []string) bool {
	entry = strings.ToLower(entry)
	for _, pattern := range deny {
		if ok, _ := path.Match(pattern, entry); ok {
			return true
		}
	}
	return false
}
