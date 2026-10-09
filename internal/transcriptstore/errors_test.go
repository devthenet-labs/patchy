// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package transcriptstore

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/bitwise-media-group/patchy/internal/transcript"
)

var errBoom = errors.New("boom")

func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestUnmarshalNotGzip(t *testing.T) {
	turns, err := Unmarshal([]byte("not gzip at all"))
	if err == nil || !strings.Contains(err.Error(), "transcriptstore: unmarshal") {
		t.Fatalf("Unmarshal err = %v, want unmarshal error", err)
	}
	if turns != nil {
		t.Errorf("turns = %+v, want nil", turns)
	}
}

// TestUnmarshalTolerance: blank lines are skipped and malformed data ends the
// read, keeping every turn before it — a partial transcript beats an error.
func TestUnmarshalTolerance(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantText []string
	}{
		{name: "empty", body: "", wantText: nil},
		{
			name:     "blank lines skipped",
			body:     "\n  \n{\"seq\":1,\"text\":\"a\"}\n\n{\"seq\":2,\"text\":\"b\"}\n",
			wantText: []string{"a", "b"},
		},
		{name: "malformed trailer kept prefix", body: "{\"seq\":1,\"text\":\"a\"}\n{\"seq\":2,\"te", wantText: []string{"a"}},
		{
			name:     "malformed middle stops read",
			body:     "{\"seq\":1,\"text\":\"a\"}\nGARBAGE\n{\"seq\":3,\"text\":\"c\"}\n",
			wantText: []string{"a"},
		},
		{name: "malformed first", body: "nope\n{\"seq\":1,\"text\":\"a\"}\n", wantText: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			turns, err := Unmarshal(gzipped(t, tt.body))
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			var got []string
			for _, tr := range turns {
				got = append(got, tr.Text)
			}
			if !reflect.DeepEqual(got, tt.wantText) {
				t.Errorf("texts = %q, want %q", got, tt.wantText)
			}
		})
	}
}

// TestMarshalStampsVersion: whatever V a caller set, the stored turn carries
// the package's transcript version.
func TestMarshalStampsVersion(t *testing.T) {
	raw, err := Marshal([]transcript.Turn{{Seq: 1, V: 999, Text: "x"}, {Seq: 2, Text: "y"}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Unmarshal(raw)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	for _, tr := range got {
		if tr.V != transcript.Version {
			t.Errorf("turn %d V = %d, want %d", tr.Seq, tr.V, transcript.Version)
		}
	}
}

// TestMarshalRoundTripProperty: any turn sequence survives Marshal/Unmarshal
// with its count, order and text intact.
func TestMarshalRoundTripProperty(t *testing.T) {
	prop := func(texts []string, tools []string) bool {
		turns := make([]transcript.Turn, len(texts))
		for i, s := range texts {
			turns[i] = transcript.Turn{Seq: i + 1, Role: transcript.RoleAssistant, Kind: transcript.KindText, Text: s}
			if i < len(tools) {
				turns[i].Tool = tools[i]
			}
		}
		raw, err := Marshal(turns)
		if err != nil {
			return false
		}
		got, err := Unmarshal(raw)
		if err != nil || len(got) != len(turns) {
			return false
		}
		for i := range turns {
			if got[i].Seq != turns[i].Seq || got[i].Text != turns[i].Text || got[i].Tool != turns[i].Tool {
				return false
			}
		}
		return true
	}
	cfg := &quick.Config{Rand: rand.New(rand.NewSource(7)), MaxCount: 100}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

func TestConfigMapShape(t *testing.T) {
	inv := investigation("fnd-cm-inv-1")
	turns := []transcript.Turn{{Seq: 1, Role: transcript.RoleAssistant, Kind: transcript.KindText, Text: "hi"}}
	cm, err := ConfigMap("agents", map[string]string{"a": "b"}, inv, invGVK, turns)
	if err != nil {
		t.Fatalf("ConfigMap: %v", err)
	}
	if cm.Name != "fnd-cm-inv-1-transcript" || cm.Namespace != "agents" || cm.Labels["a"] != "b" {
		t.Errorf("meta = %+v", cm.ObjectMeta)
	}
	got, err := FromConfigMap(cm)
	if err != nil || len(got) != 1 || got[0].Text != "hi" {
		t.Errorf("FromConfigMap = (%+v, %v)", got, err)
	}
	if got, err := FromConfigMap(&corev1.ConfigMap{BinaryData: map[string][]byte{DataKey: {}}}); err != nil || got != nil {
		t.Errorf("FromConfigMap(empty value) = (%+v, %v), want nil, nil", got, err)
	}
}

func TestPersistClientErrors(t *testing.T) {
	alreadyExists := apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, "x")
	tests := []struct {
		name           string
		create, update error
		wantErr        string
	}{
		{name: "create fails", create: errBoom, wantErr: "transcriptstore: write fnd-e-inv-1-transcript"},
		{
			name: "rewrite fails", create: alreadyExists, update: errBoom,
			wantErr: "transcriptstore: rewrite fnd-e-inv-1-transcript",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(interceptor.Funcs{
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
					return tt.create
				},
				Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
					return tt.update
				},
			}).Build()
			turns := []transcript.Turn{{Seq: 1, Text: "a"}}
			ref, err := Persist(context.Background(), c, "patchy", nil, investigation("fnd-e-inv-1"), invGVK, turns)
			if !errors.Is(err, errBoom) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Persist err = %v, want %q wrapping boom", err, tt.wantErr)
			}
			if ref != nil {
				t.Errorf("ref = %+v, want nil", ref)
			}
		})
	}
}

func TestLoadErrors(t *testing.T) {
	t.Run("get fails", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return errBoom
			},
		}).Build()
		if _, err := Load(context.Background(), c, "patchy", "t"); !errors.Is(err, errBoom) ||
			!strings.Contains(err.Error(), "transcriptstore: read t") {
			t.Errorf("Load err = %v", err)
		}
	})
	t.Run("corrupt data", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "patchy"},
			BinaryData: map[string][]byte{DataKey: []byte("junk")},
		}).Build()
		if _, err := Load(context.Background(), c, "patchy", "t"); err == nil {
			t.Error("Load(corrupt) err = nil, want unmarshal error")
		}
	})
}
