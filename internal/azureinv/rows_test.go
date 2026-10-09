// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package azureinv

import (
	"strings"
	"testing"
)

// TestTagsForRowShapes: a row that is not an object is a decode error (worth
// retrying, not final), and a row without a name is named by its ID.
func TestTagsForRowShapes(t *testing.T) {
	c := &Client{graph: &fakeGraph{rows: []any{"not an object"}}}
	if _, err := c.TagsFor(t.Context(), vmID); err == nil ||
		!strings.Contains(err.Error(), "unexpected row shape string") {
		t.Errorf("non-object row = %v", err)
	}

	c = &Client{graph: &fakeGraph{rows: []any{map[string]any{"tags": map[string]any{"team": "orders", "cost": 7}}}}}
	tags, err := c.TagsFor(t.Context(), vmID)
	if err != nil {
		t.Fatal(err)
	}
	if tags.Name != vmID || len(tags.Tags) != 1 || tags.Tags["team"] != "orders" {
		t.Errorf("nameless row = %+v", tags)
	}

	c = &Client{graph: &fakeGraph{rows: []any{map[string]any{"name": "web-01", "tags": "team=orders"}}}}
	tags, err = c.TagsFor(t.Context(), vmID)
	if err != nil || tags.Name != "web-01" || tags.Tags != nil {
		t.Errorf("schemaless tags column = %+v, %v; want no tags", tags, err)
	}
}

func TestNormalizeTagsOnlyStrings(t *testing.T) {
	if got := normalizeTags(map[string]any{"a": 1, "b": true}); got != nil {
		t.Errorf("normalizeTags(no strings) = %v, want nil", got)
	}
	if got := normalizeTags(nil); got != nil {
		t.Errorf("normalizeTags(nil) = %v", got)
	}
}

// TestEscapeKeepsTheLiteralClosed: no ID can end the KQL string literal it
// is placed in.
func TestEscapeKeepsTheLiteralClosed(t *testing.T) {
	cases := map[string]string{
		`plain`:     `plain`,
		`it's`:      `it\'s`,
		`a\b`:       `a\\b`,
		`x\' or 1`:  `x\\\' or 1`,
		`''`:        `\'\'`,
		`\`:         `\\`,
		"multi\nln": "multi\nln",
	}
	for in, want := range cases {
		if got := escape(in); got != want {
			t.Errorf("escape(%q) = %q, want %q", in, got, want)
		}
	}
	c := &Client{graph: &fakeGraph{}}
	_, _ = c.TagsFor(t.Context(), `x' | project secret //`)
	if q := c.graph.(*fakeGraph).query; !strings.Contains(q, `=~ 'x\' | project secret //'`) {
		t.Errorf("query = %q, want the quote escaped inside the literal", q)
	}
}

func TestCloseIsANoOp(t *testing.T) {
	if err := (&Client{}).Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
}
