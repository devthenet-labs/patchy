// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package resolve

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1/google"
)

// ecrHost matches a private ECR registry host and captures its region:
// <account>.dkr.ecr[-fips].<region>.amazonaws.com[.cn].
var ecrHost = regexp.MustCompile(`^\d{12}\.dkr\.ecr(?:-fips)?\.([a-z0-9-]+)\.amazonaws\.com(?:\.cn)?$`)

// ecrTokenSkew is how long before its expiry a cached ECR token is renewed.
const ecrTokenSkew = 5 * time.Minute

// NewKeychain builds the host-selected keychain the resolver authenticates
// with. ECR hosts go through the AWS SDK's default credential chain (IRSA or
// Pod Identity in-cluster, the same path the AWS enhancer uses), Artifact
// Registry and GCR through Application Default Credentials (Workload
// Identity in-cluster), and every other host through the docker config
// under DOCKER_CONFIG (the mounted pullSecret), which falls back to
// anonymous when it has no entry for the host. Selection is by host, so a
// cloud host never consults the docker config and a ghcr host never mints a
// cloud token. A cloud credential failure is an error, never anonymous.
func NewKeychain() authn.Keychain {
	return &hostKeychain{
		ecr:    &ecrKeychain{tokens: awsECRToken, now: time.Now},
		google: &googleKeychain{auth: google.NewEnvAuthenticator},
		docker: authn.DefaultKeychain,
	}
}

// hostKeychain dispatches on the registry host.
type hostKeychain struct {
	ecr    authn.ContextKeychain
	google authn.Keychain
	docker authn.Keychain
}

// Resolve implements authn.Keychain.
func (k *hostKeychain) Resolve(target authn.Resource) (authn.Authenticator, error) {
	return k.ResolveContext(context.Background(), target)
}

// ResolveContext implements authn.ContextKeychain.
func (k *hostKeychain) ResolveContext(ctx context.Context, target authn.Resource) (authn.Authenticator, error) {
	host := target.RegistryStr()
	switch {
	case ecrHost.MatchString(host):
		return k.ecr.ResolveContext(ctx, target)
	case host == "gcr.io" || strings.HasSuffix(host, ".gcr.io") || strings.HasSuffix(host, ".pkg.dev"):
		return authn.Resolve(ctx, k.google, target)
	default:
		return k.docker.Resolve(target)
	}
}

// googleKeychain authenticates Artifact Registry and GCR through Application
// Default Credentials, caching only a success. ggcr's google.Keychain
// resolves once per process and keeps authn.Anonymous for good when ADC is
// unavailable at that moment (the GKE metadata server refuses a new Pod's
// first requests), which turned a transient credential failure into a 401,
// a deterministic AccessDenied rejection pinned on the Repository. Here a
// failure is an error, so the caller backs off and the next resolution asks
// again; a host that needs no credential still needs ADC to be configured.
type googleKeychain struct {
	// auth builds the authenticator; the default is
	// google.NewEnvAuthenticator (whose token source refreshes itself) and
	// tests substitute a fake.
	auth func(ctx context.Context) (authn.Authenticator, error)

	mu     sync.Mutex
	cached authn.Authenticator
}

// Resolve implements authn.Keychain.
func (k *googleKeychain) Resolve(target authn.Resource) (authn.Authenticator, error) {
	return k.ResolveContext(context.Background(), target)
}

// ResolveContext implements authn.ContextKeychain.
func (k *googleKeychain) ResolveContext(ctx context.Context, target authn.Resource) (authn.Authenticator, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cached != nil {
		return k.cached, nil
	}
	a, err := k.auth(ctx)
	if err != nil {
		return nil, fmt.Errorf("google application default credentials for %s: %w", target.RegistryStr(), err)
	}
	k.cached = a
	return a, nil
}

// ecrToken is one authorization token: "AWS:<password>" decoded, with the
// expiry the API reported.
type ecrToken struct {
	user, password string
	expires        time.Time
}

// ecrKeychain mints ECR authorization tokens per region and caches them
// until shortly before they expire (the API issues twelve-hour tokens). A
// credential failure is returned as an error, so the caller backs off
// instead of recording a deterministic rejection against a registry that
// was never actually asked.
type ecrKeychain struct {
	// tokens fetches a token for a region; the default calls the API and
	// tests substitute a fake.
	tokens func(ctx context.Context, region string) (token string, expires time.Time, err error)
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]ecrToken
}

// Resolve implements authn.Keychain.
func (k *ecrKeychain) Resolve(target authn.Resource) (authn.Authenticator, error) {
	return k.ResolveContext(context.Background(), target)
}

// ResolveContext implements authn.ContextKeychain.
func (k *ecrKeychain) ResolveContext(ctx context.Context, target authn.Resource) (authn.Authenticator, error) {
	m := ecrHost.FindStringSubmatch(target.RegistryStr())
	if m == nil {
		return authn.Anonymous, nil
	}
	region := m[1]
	k.mu.Lock()
	defer k.mu.Unlock()
	if tok, ok := k.cache[region]; ok && k.now().Add(ecrTokenSkew).Before(tok.expires) {
		return &authn.Basic{Username: tok.user, Password: tok.password}, nil
	}
	raw, expires, err := k.tokens(ctx, region)
	if err != nil {
		return nil, fmt.Errorf("ecr authorization token for %s: %w", region, err)
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("ecr authorization token for %s: %w", region, err)
	}
	user, password, ok := strings.Cut(string(decoded), ":")
	if !ok || user == "" {
		return nil, fmt.Errorf("ecr authorization token for %s is not user:password", region)
	}
	if k.cache == nil {
		k.cache = make(map[string]ecrToken)
	}
	k.cache[region] = ecrToken{user: user, password: password, expires: expires}
	return &authn.Basic{Username: user, Password: password}, nil
}

// awsECRToken fetches an ECR authorization token through the SDK's default
// credential chain, dialing the registry's own region.
func awsECRToken(ctx context.Context, region string) (string, time.Time, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return "", time.Time{}, err
	}
	out, err := ecr.NewFromConfig(cfg).GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return "", time.Time{}, err
	}
	if len(out.AuthorizationData) == 0 || out.AuthorizationData[0].AuthorizationToken == nil {
		return "", time.Time{}, errors.New("empty authorization data")
	}
	d := out.AuthorizationData[0]
	return aws.ToString(d.AuthorizationToken), aws.ToTime(d.ExpiresAt), nil
}
