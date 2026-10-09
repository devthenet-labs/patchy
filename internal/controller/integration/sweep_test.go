// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/ghsecret"
)

// testAppKey is one RSA key for every App-credential test; generating a
// 2048-bit key per test is slow under -race.
var testAppKey = sync.OnceValue(func() []byte {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
})

// appSecret is a GitHub App credential Secret with a webhook HMAC secret.
func appSecret(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patchy"},
		Data: map[string][]byte{
			ghsecret.KeyAppID:         []byte("12345"),
			ghsecret.KeyPrivateKey:    testAppKey(),
			ghsecret.KeyWebhookSecret: []byte("hmac"),
		},
	}
}

func TestCredsValidateGitHub(t *testing.T) {
	badAppID := appSecret("creds")
	badAppID.Data[ghsecret.KeyAppID] = []byte("not-a-number")
	badKey := appSecret("creds")
	badKey.Data[ghsecret.KeyPrivateKey] = []byte("not a pem")
	noWebhook := appSecret("creds")
	delete(noWebhook.Data, ghsecret.KeyWebhookSecret)

	tests := []struct {
		name    string
		secret  *corev1.Secret
		noRef   bool
		wantErr string
	}{
		{name: "a PAT with a webhook secret", secret: patSecret("creds", "pat", "hmac")},
		{name: "an App with a webhook secret", secret: appSecret("creds")},
		{
			name: "a PAT without a webhook secret", secret: patSecret("creds", "pat", ""),
			wantErr: "missing key webhookSecret",
		},
		{name: "an App without a webhook secret", secret: noWebhook, wantErr: "missing key webhookSecret"},
		{
			name: "an empty token",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "patchy"},
				Data:       map[string][]byte{"token": []byte("  ")},
			},
			wantErr: "token is empty",
		},
		{name: "neither credential", secret: patSecret("creds", "", "hmac"), wantErr: "neither token nor appID"},
		{name: "an unparseable App id", secret: badAppID, wantErr: "appID"},
		{name: "an unparseable private key", secret: badKey, wantErr: "app transport"},
		{name: "a missing Secret", wantErr: "get integration secret"},
		{name: "no secretRef", noRef: true, wantErr: ErrNoCredential.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			integ := ghIntegrationAt("gh", "creds", "")
			if tt.noRef {
				integ.Spec.SecretRef = nil
			}
			var objs []client.Object
			if tt.secret != nil {
				objs = append(objs, tt.secret)
			}
			err := NewCreds(receiverClient(nil, objs...)).Validate(t.Context(), integ)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateGoogleCloud(t *testing.T) {
	scc := func(enabled bool, audience, account string) *v1alpha1.GoogleCloudIntegration {
		return &v1alpha1.GoogleCloudIntegration{SecurityCommandCenter: &v1alpha1.GoogleCloudSCC{
			Enabled: enabled, Audience: audience, ServiceAccount: account,
		}}
	}
	tests := []struct {
		name    string
		gc      *v1alpha1.GoogleCloudIntegration
		wantErr bool
	}{
		{name: "no block", gc: nil, wantErr: true},
		{name: "an empty block", gc: &v1alpha1.GoogleCloudIntegration{}, wantErr: true},
		{name: "asset inventory alone", gc: &v1alpha1.GoogleCloudIntegration{
			CloudAssetInventory: &v1alpha1.GoogleCloudAssetInventory{},
		}},
		{name: "scc fully configured", gc: scc(true, sccAudience, sccAccount)},
		{name: "scc disabled needs nothing", gc: scc(false, "", "")},
		{name: "scc without an audience", gc: scc(true, "", sccAccount), wantErr: true},
		{name: "scc without a service account", gc: scc(true, sccAudience, ""), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			integ := &v1alpha1.Integration{
				ObjectMeta: metav1.ObjectMeta{Name: "gcp", Namespace: "patchy"},
				Spec:       v1alpha1.IntegrationSpec{Provider: v1alpha1.IntegrationProviderGoogleCloud, GoogleCloud: tt.gc},
			}
			// google-cloud holds no credential: Validate must not need a Secret.
			err := NewCreds(receiverClient(nil)).Validate(t.Context(), integ)
			if tt.wantErr != (err != nil) {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCredsApp(t *testing.T) {
	c := receiverClient(nil, patSecret("pat-creds", "pat", "hmac"), appSecret("app-creds"))
	creds := NewCreds(c)

	app, ok, err := creds.App(t.Context(), ghIntegrationAt("gh", "app-creds", ""))
	if err != nil || !ok || app == nil {
		t.Errorf("App(app credential) = %v, %v, %v; want an App", app, ok, err)
	}
	app, ok, err = creds.App(t.Context(), ghIntegrationAt("gh", "pat-creds", ""))
	if err != nil || ok || app != nil {
		t.Errorf("App(PAT) = %v, %v, %v; want ok=false and no error", app, ok, err)
	}
	if _, _, err := creds.App(t.Context(), ghIntegrationAt("gh", "absent", "")); err == nil {
		t.Error("App(missing Secret) = nil error, want the read failure")
	}
	broken := appSecret("broken")
	broken.Data[ghsecret.KeyPrivateKey] = nil
	c2 := receiverClient(nil, broken)
	if _, _, err := NewCreds(c2).App(t.Context(), ghIntegrationAt("gh", "broken", "")); err == nil {
		t.Error("App(no private key) = nil error, want the parse failure")
	}
}

// An App credential resolves the installation covering the repository and
// calls the API with that installation's token.
func TestCredsClientApp(t *testing.T) {
	mux, base := newGitHubAPI(t)
	mux.HandleFunc("GET /repos/acme/orders/installation", func(w http.ResponseWriter, _ *http.Request) {
		respondJSON(w, http.StatusOK, `{"id":5}`)
	})
	mux.HandleFunc("POST /app/installations/5/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			respondJSON(w, http.StatusUnauthorized, `{"message":"JWT required"}`)
			return
		}
		respondJSON(w, http.StatusCreated, `{"token":"inst-tok","expires_at":"2099-01-01T00:00:00Z"}`)
	})
	mux.HandleFunc("GET /repos/acme/orders/code-scanning/alerts/9", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Authorization"), "inst-tok") {
			respondJSON(w, http.StatusUnauthorized, `{"message":"Bad credentials"}`)
			return
		}
		respondJSON(w, http.StatusOK, alertJSON)
	})
	c := receiverClient(nil, appSecret("creds"))
	g := &alertGetter{creds: NewCreds(c), integ: ghIntegrationAt("gh", "creds", base)}
	alert, err := g.GetAlert(t.Context(), ghclient.Repo{Owner: "acme", Name: "orders"}, 9)
	if err != nil {
		t.Fatalf("GetAlert() = %v", err)
	}
	if alert.Number != 9 {
		t.Errorf("alert number = %d, want 9", alert.Number)
	}

	// A malformed proxy on a PAT credential is refused before any call.
	pat := ghIntegrationAt("gh", "pat", base)
	pat.Spec.GitHub.Proxy = &v1alpha1.ProxyConfig{URL: "://bad proxy"}
	cp := receiverClient(nil, patSecret("pat", "tok", "hmac"))
	if _, err := NewCreds(cp).Client(t.Context(), pat, ghclient.Repo{Owner: "acme", Name: "orders"}); err == nil {
		t.Error("Client() with a malformed proxy = nil error, want a refusal")
	}
}

// deliveryLog is a fake App webhook delivery log: one page of deliveries,
// optionally always advertising a next page, and a redeliver endpoint.
type deliveryLog struct {
	mu          sync.Mutex
	deliveries  []map[string]any
	listStatus  int
	failIDs     map[int64]bool
	endless     bool
	redelivered []int64
	pages       int
}

func (l *deliveryLog) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /app/hook/deliveries", func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.pages++
		if l.listStatus != 0 {
			respondJSON(w, l.listStatus, `{"message":"nope"}`)
			return
		}
		if l.endless {
			w.Header().Set("Link", fmt.Sprintf(`<%s?cursor=c%d>; rel="next"`, r.URL.Path, l.pages))
		}
		body, _ := json.Marshal(l.deliveries)
		respondJSON(w, http.StatusOK, string(body))
	})
	mux.HandleFunc("POST /app/hook/deliveries/{id}/attempts", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.failIDs[id] {
			respondJSON(w, http.StatusUnprocessableEntity, `{"message":"cannot redeliver"}`)
			return
		}
		l.redelivered = append(l.redelivered, id)
		respondJSON(w, http.StatusAccepted, `{}`)
	})
}

func delivery(id int64, guid string, status int, age time.Duration) map[string]any {
	return map[string]any{
		"id": id, "guid": guid, "status_code": status, "event": "code_scanning_alert",
		"delivered_at": testClock.Add(-age).Format(time.RFC3339),
	}
}

// sweepIntegration is an issues+code-scanning github Integration at base
// with the redelivery sweep on, over the given Secret.
func sweepIntegration(base, secretName string) *v1alpha1.Integration {
	integ := ghIntegrationAt("gh", secretName, base)
	integ.Spec.GitHub.Redelivery = &v1alpha1.GitHubRedelivery{
		Enabled: true, Lookback: metav1.Duration{Duration: time.Hour},
	}
	return integ
}

// ghRequest is the reconcile request for the Integration named gh.
var ghRequest = ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "patchy", Name: "gh"}}

func reconcileGH(t *testing.T, r *IntegrationReconciler) (ctrl.Result, v1alpha1.Integration) {
	t.Helper()
	req := ghRequest
	res, err := r.Reconcile(t.Context(), req)
	if err != nil {
		t.Fatalf("Reconcile() = %v", err)
	}
	var integ v1alpha1.Integration
	if err := r.Get(t.Context(), req.NamespacedName, &integ); err != nil {
		t.Fatalf("get integration: %v", err)
	}
	return res, integ
}

func TestReconcileRedeliverySweep(t *testing.T) {
	replayAt := metav1.NewTime(testClock.Add(-time.Minute))
	earlier := metav1.NewTime(testClock.Add(-time.Hour))

	tests := []struct {
		name            string
		log             *deliveryLog
		secret          *corev1.Secret
		mutate          func(*v1alpha1.Integration)
		prevReplayedAt  *metav1.Time
		wantScanned     int32
		wantRedelivered []int64
		wantErr         string
		wantTruncated   bool
		wantReplayedAt  *metav1.Time
		wantDedupDrop   bool
	}{
		{
			name: "the standing sweep redelivers failures inside the lookback",
			log: &deliveryLog{deliveries: []map[string]any{
				delivery(3, "a", 500, time.Minute),
				delivery(2, "b", 200, 2*time.Minute),
				delivery(1, "c", 502, 2*time.Hour), // past the horizon: walk stops
			}},
			secret:          appSecret("creds"),
			prevReplayedAt:  &earlier,
			wantScanned:     2,
			wantRedelivered: []int64{3},
			wantReplayedAt:  &earlier, // the standing sweep keeps the handled replay
		},
		{
			name: "a failed redelivery is surfaced and the rest still go",
			log: &deliveryLog{
				deliveries: []map[string]any{
					delivery(5, "a", 500, time.Minute),
					delivery(4, "b", 500, time.Minute),
				},
				failIDs: map[int64]bool{5: true},
			},
			secret:          appSecret("creds"),
			wantScanned:     2,
			wantRedelivered: []int64{4},
			wantErr:         "redeliver 5",
		},
		{
			name:    "a PAT cannot see the delivery log",
			secret:  patSecret("creds", "pat", "hmac"),
			wantErr: "requires GitHub App credentials",
		},
		{
			name:    "a delivery log failure is surfaced",
			log:     &deliveryLog{listStatus: http.StatusNotFound},
			secret:  appSecret("creds"),
			wantErr: "list hook deliveries",
		},
		{
			name: "an endless log is truncated at the page cap",
			log: &deliveryLog{
				deliveries: []map[string]any{delivery(1, "a", 200, time.Minute)},
				endless:    true,
			},
			secret:        appSecret("creds"),
			wantScanned:   50,
			wantTruncated: true,
		},
		{
			name: "a replay redelivers the newest attempt of every delivery",
			log: &deliveryLog{deliveries: []map[string]any{
				delivery(9, "a", 200, time.Minute),
				delivery(8, "a", 500, 2*time.Minute),
				delivery(7, "b", 200, 3*time.Minute),
			}},
			secret: appSecret("creds"),
			mutate: func(i *v1alpha1.Integration) {
				i.Spec.GitHub.Redelivery = nil // replay runs without the standing sweep
				i.Spec.Replay = &v1alpha1.ActionRequest{By: "dev", At: replayAt}
			},
			wantScanned:     3,
			wantRedelivered: []int64{9, 7},
			wantReplayedAt:  &replayAt,
			wantDedupDrop:   true,
		},
		{
			name:   "a replay whose log walk fails is not marked handled",
			log:    &deliveryLog{listStatus: http.StatusNotFound},
			secret: appSecret("creds"),
			mutate: func(i *v1alpha1.Integration) {
				i.Spec.Replay = &v1alpha1.ActionRequest{By: "dev", At: replayAt}
			},
			wantErr:       "list hook deliveries",
			wantDedupDrop: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux, base := newGitHubAPI(t)
			if tt.log == nil {
				tt.log = &deliveryLog{}
			}
			tt.log.register(mux)
			integ := sweepIntegration(base, "creds")
			if tt.mutate != nil {
				tt.mutate(integ)
			}
			c := receiverClient(nil, integ, tt.secret)
			if tt.prevReplayedAt != nil {
				integ.Status.Redelivery = &v1alpha1.RedeliveryStatus{ReplayedAt: tt.prevReplayedAt}
				if err := c.Status().Update(t.Context(), integ); err != nil {
					t.Fatalf("seed status: %v", err)
				}
			}
			dropped := 0
			r := &IntegrationReconciler{
				Client: c, Creds: NewCreds(c), Now: func() time.Time { return testClock },
				ResetDedup: func() { dropped++ },
			}
			_, got := reconcileGH(t, r)

			if !meta.IsStatusConditionTrue(got.Status.Conditions, v1alpha1.ConditionReady) {
				t.Fatalf("Ready = %+v, want True", got.Status.Conditions)
			}
			st := got.Status.Redelivery
			if st == nil || st.LastSweepAt == nil || !st.LastSweepAt.Time.Equal(testClock) {
				t.Fatalf("redelivery status = %+v, want a sweep stamped at the clock", st)
			}
			if st.Scanned != tt.wantScanned || st.Truncated != tt.wantTruncated {
				t.Errorf("scanned/truncated = %d/%v, want %d/%v", st.Scanned, st.Truncated, tt.wantScanned, tt.wantTruncated)
			}
			if int(st.Redelivered) != len(tt.wantRedelivered) || !slices.Equal(tt.log.redelivered, tt.wantRedelivered) {
				t.Errorf("redelivered %d %v, want %v", st.Redelivered, tt.log.redelivered, tt.wantRedelivered)
			}
			assertSweepError(t, st.Error, tt.wantErr)
			assertReplayedAt(t, st.ReplayedAt, tt.wantReplayedAt)
			if (dropped > 0) != tt.wantDedupDrop {
				t.Errorf("dedup dropped %d times, want dropped=%v", dropped, tt.wantDedupDrop)
			}
		})
	}
}

// assertSweepError checks the sweep's surfaced error: empty when want is,
// else containing want.
func assertSweepError(t *testing.T, got, want string) {
	t.Helper()
	if want == "" && got != "" || !strings.Contains(got, want) {
		t.Errorf("error = %q, want %q", got, want)
	}
}

// assertReplayedAt checks the replay echo: unset when want is nil.
func assertReplayedAt(t *testing.T, got, want *metav1.Time) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Errorf("replayedAt = %v, want unset", got)
	case want != nil && (got == nil || !got.Time.Equal(want.Time)):
		t.Errorf("replayedAt = %v, want %v", got, want)
	}
}

// An invalid credential marks the Integration not Ready and skips every
// credentialed action; the receiver path is still published.
func TestReconcileInvalidCredential(t *testing.T) {
	mux, base := newGitHubAPI(t)
	log := &deliveryLog{}
	log.register(mux)
	integ := sweepIntegration(base, "creds")
	c := receiverClient(nil, integ) // no Secret
	r := &IntegrationReconciler{Client: c, Creds: NewCreds(c), Now: func() time.Time { return testClock }}
	res, got := reconcileGH(t, r)

	cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "CredentialInvalid" ||
		!strings.Contains(cond.Message, "get integration secret") {
		t.Errorf("Ready = %+v, want False/CredentialInvalid naming the secret", cond)
	}
	if got.Status.WebhookPath != GitHubPath {
		t.Errorf("webhookPath = %q, want %q", got.Status.WebhookPath, GitHubPath)
	}
	if got.Status.Redelivery != nil || log.pages != 0 {
		t.Errorf("redelivery = %+v after %d log reads; want no sweep while not Ready", got.Status.Redelivery, log.pages)
	}
	if res.RequeueAfter != defaultRevalidate {
		t.Errorf("RequeueAfter = %v, want the default %v", res.RequeueAfter, defaultRevalidate)
	}
}

func TestReconcileIntegrationLifecycle(t *testing.T) {
	gr := schema.GroupResource{Group: "patchy.bitwisemedia.uk", Resource: "integrations"}
	boom := errors.New("apiserver down")
	statusFails := func(err error) *interceptor.Funcs {
		return &interceptor.Funcs{
			SubResourceUpdate: func(
				context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption,
			) error {
				return err
			},
		}
	}

	t.Run("a missing integration is a no-op", func(t *testing.T) {
		c := receiverClient(nil)
		r := &IntegrationReconciler{Client: c, Creds: NewCreds(c)}
		res, err := r.Reconcile(t.Context(), ghRequest)
		if err != nil || res != (ctrl.Result{}) {
			t.Errorf("Reconcile() = %+v, %v; want zero, nil", res, err)
		}
	})

	t.Run("a suspended integration is left alone", func(t *testing.T) {
		integ := ghIntegrationAt("gh", "creds", "")
		integ.Spec.Suspend = true
		c := receiverClient(nil, integ)
		r := &IntegrationReconciler{Client: c, Creds: NewCreds(c)}
		res, got := reconcileGH(t, r)
		if res != (ctrl.Result{}) || len(got.Status.Conditions) != 0 {
			t.Errorf("result %+v, conditions %+v; want nothing written", res, got.Status.Conditions)
		}
	})

	t.Run("spec.interval paces revalidation", func(t *testing.T) {
		integ := ghIntegrationAt("gh", "creds", "")
		integ.Spec.Interval = metav1.Duration{Duration: 3 * time.Minute}
		c := receiverClient(nil, integ, patSecret("creds", "pat", "hmac"))
		r := &IntegrationReconciler{Client: c, Creds: NewCreds(c)}
		res, got := reconcileGH(t, r)
		if res.RequeueAfter != 3*time.Minute {
			t.Errorf("RequeueAfter = %v, want 3m", res.RequeueAfter)
		}
		if !meta.IsStatusConditionTrue(got.Status.Conditions, v1alpha1.ConditionReady) {
			t.Errorf("Ready = %+v, want True", got.Status.Conditions)
		}
	})

	t.Run("a status conflict requeues", func(t *testing.T) {
		c := receiverClient(statusFails(kerrors.NewConflict(gr, "gh", errors.New("stale"))),
			ghIntegrationAt("gh", "creds", ""), patSecret("creds", "pat", "hmac"))
		r := &IntegrationReconciler{Client: c, Creds: NewCreds(c)}
		res, err := r.Reconcile(t.Context(), ghRequest)
		if err != nil || !res.Requeue { //nolint:staticcheck // the reconciler's contract is Requeue.
			t.Errorf("Reconcile() = %+v, %v; want a requeue and no error", res, err)
		}
	})

	t.Run("a status write error is returned", func(t *testing.T) {
		c := receiverClient(statusFails(boom), ghIntegrationAt("gh", "creds", ""), patSecret("creds", "pat", "hmac"))
		r := &IntegrationReconciler{Client: c, Creds: NewCreds(c)}
		if _, err := r.Reconcile(t.Context(), ghRequest); !errors.Is(err, boom) {
			t.Errorf("Reconcile() = %v, want the write error", err)
		}
	})
}
