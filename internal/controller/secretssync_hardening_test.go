/*
Copyright 2026 Haider Raed.
SPDX-License-Identifier: Apache-2.0
*/

package controller

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/secrets-bridge/core/providers"
	"github.com/secrets-bridge/core/providers/vault"

	syncv1alpha1 "github.com/secrets-bridge/controller/api/v1alpha1"
)

// The status reasons below are a contract for anyone alerting on the
// Ready condition, so the tests pin the literal strings rather than the
// Go constants.
const (
	reasonCredentialConfigRejected = "CredentialConfigRejected"
	reasonProviderError            = "ProviderError"
	reasonValidated                = "Validated"
)

// fakeVault is an httptest server standing in for a Vault the CR author
// controls. It counts every request and answers each one with status
// and body.
func fakeVault(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// writeTokenFile writes content to a fresh file and returns its path.
// Stands in for a projected service account token or any other file
// the controller pod can read.
func writeTokenFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	return p
}

// capturedLogs routes the reconciler's logger into a buffer so a test
// can assert what reaches the controller logs.
func capturedLogs(t *testing.T) (context.Context, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger := zap.New(zap.WriteTo(&buf), zap.UseDevMode(true))
	return log.IntoContext(t.Context(), logger), &buf
}

func reconcileAndGet(t *testing.T, ctx context.Context, rec *SecretsSyncReconciler, cl client.Client, name string) (ctrl.Result, syncv1alpha1.SecretsSync) {
	t.Helper()
	res, err := rec.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: name}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got syncv1alpha1.SecretsSync
	if err := cl.Get(t.Context(), client.ObjectKey{Name: name}, &got); err != nil {
		t.Fatalf("get back CR: %v", err)
	}
	return res, got
}

func countingStub(r *providers.Registry, kind string, builds *atomic.Int64) {
	r.Register(kind, func(context.Context, providers.Config) (providers.Provider, error) {
		builds.Add(1)
		return stubProvider{}, nil
	})
}

// CTL-N-01: a CR must not be able to hand the controller a credential
// (token) or a credential source (kubernetesTokenPath). Building a Vault
// provider logs in straight away, so a CR carrying kubernetesTokenPath
// made the controller read that file and PUT it as `jwt` to whatever
// address the CR named. Those keys are now refused before any provider
// is built, so the provider is never contacted.
func TestReconcile_CredentialKeysInConfig_RefusedBeforeAnyProviderIO(t *testing.T) {
	const canary = "CANARY-credential-5b1e0c"
	tokenFile := writeTokenFile(t, canary)

	cases := []struct {
		name      string
		vaultSide string // "source" or "destination"
		cfg       func(addr string) map[string]string
		wantKey   string
	}{
		{
			name:      "kubernetesTokenPath on destination",
			vaultSide: "destination",
			cfg: func(addr string) map[string]string {
				return map[string]string{
					vault.ConfigAddress:             addr,
					vault.ConfigKubernetesRole:      "app",
					vault.ConfigKubernetesTokenPath: tokenFile,
				}
			},
			wantKey: vault.ConfigKubernetesTokenPath,
		},
		{
			name:      "kubernetesTokenPath on source",
			vaultSide: "source",
			cfg: func(addr string) map[string]string {
				return map[string]string{
					vault.ConfigAddress:             addr,
					vault.ConfigKubernetesRole:      "app",
					vault.ConfigKubernetesTokenPath: tokenFile,
				}
			},
			wantKey: vault.ConfigKubernetesTokenPath,
		},
		{
			name:      "static token on destination",
			vaultSide: "destination",
			cfg: func(addr string) map[string]string {
				return map[string]string{
					vault.ConfigAddress:    addr,
					vault.ConfigAuthMethod: "token",
					vault.ConfigToken:      canary,
				}
			},
			wantKey: vault.ConfigToken,
		},
		{
			name:      "blank token is still refused (presence, not value)",
			vaultSide: "destination",
			cfg: func(addr string) map[string]string {
				return map[string]string{
					vault.ConfigAddress:        addr,
					vault.ConfigKubernetesRole: "app",
					vault.ConfigToken:          "",
				}
			},
			wantKey: vault.ConfigToken,
		},
		{
			name:      "key match ignores case",
			vaultSide: "destination",
			cfg: func(addr string) map[string]string {
				return map[string]string{
					vault.ConfigAddress:        addr,
					vault.ConfigKubernetesRole: "app",
					"KubernetesTokenPATH":      tokenFile,
				}
			},
			wantKey: vault.ConfigKubernetesTokenPath,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, hits := fakeVault(t, http.StatusOK, `{"auth":{"client_token":"x","lease_duration":3600}}`)

			cr := sampleCR("cred-" + strings.ReplaceAll(strings.ToLower(tc.name), " ", "-"))
			vaultRef := syncv1alpha1.ProviderRef{Type: vault.Kind, Config: tc.cfg(srv.URL)}
			awsRef := syncv1alpha1.ProviderRef{Type: "aws-sm", Config: map[string]string{"region": "us-east-1"}}
			if tc.vaultSide == "source" {
				cr.Spec.Source, cr.Spec.Destination = vaultRef, awsRef
			} else {
				cr.Spec.Source, cr.Spec.Destination = awsRef, vaultRef
			}

			var awsBuilds atomic.Int64
			r := providers.NewRegistry()
			vault.Register(r) // the real core Vault factory
			countingStub(r, "aws-sm", &awsBuilds)

			ctx, logs := capturedLogs(t)
			rec, cl := newReconciler(t, r, cr)
			_, got := reconcileAndGet(t, ctx, rec, cl, cr.Name)

			if n := hits.Load(); n != 0 {
				t.Fatalf("provider was contacted %d time(s); credential-bearing config must be refused before any IO", n)
			}
			if n := awsBuilds.Load(); n != 0 {
				t.Fatalf("other side was built %d time(s); refusal must happen before either provider is built", n)
			}
			ready := findCondition(got.Status.Conditions, "Ready")
			if ready == nil || ready.Status != metav1.ConditionFalse {
				t.Fatalf("expected Ready=False, got %+v", ready)
			}
			if ready.Reason != reasonCredentialConfigRejected {
				t.Fatalf("Reason: got %q want %q", ready.Reason, reasonCredentialConfigRejected)
			}
			if !strings.Contains(ready.Message, tc.vaultSide) || !strings.Contains(ready.Message, tc.wantKey) {
				t.Fatalf("message should name the side %q and key %q: %q", tc.vaultSide, tc.wantKey, ready.Message)
			}
			for _, leak := range []string{canary, tokenFile} {
				if strings.Contains(ready.Message, leak) {
					t.Fatalf("status message carries a config value: %q", ready.Message)
				}
				if strings.Contains(logs.String(), leak) {
					t.Fatalf("controller logs carry a config value:\n%s", logs.String())
				}
			}
		})
	}
}

// CTL-N-01 status hygiene: provider error text can carry a response
// body from whatever endpoint the CR named, so it must never be copied
// into status (readable by anyone who can get the CR) or the logs.
func TestReconcile_ProviderErrorText_NeverReachesStatusOrLogs(t *testing.T) {
	const bodyCanary = "CANARY-response-body-91d7"

	t.Run("factory error text", func(t *testing.T) {
		cr := sampleCR("err-factory")
		r := providers.NewRegistry()
		registerStub(r, "aws-sm")
		// Same shape as a wrapped vault/api ResponseError.
		registerFailing(r, "vault", errors.New("vault: kubernetes login: unable to log in with Kubernetes auth: "+
			"Error making API request.\n\nURL: PUT https://vault.example.com/v1/auth/kubernetes/login\n"+
			"Code: 403. Errors:\n\n* "+bodyCanary))

		ctx, logs := capturedLogs(t)
		rec, cl := newReconciler(t, r, cr)
		_, got := reconcileAndGet(t, ctx, rec, cl, cr.Name)

		ready := findCondition(got.Status.Conditions, "Ready")
		if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != reasonProviderError {
			t.Fatalf("expected Ready=False/%s, got %+v", reasonProviderError, ready)
		}
		if strings.Contains(ready.Message, bodyCanary) || strings.Contains(ready.Message, "403") {
			t.Fatalf("provider error text leaked into status: %q", ready.Message)
		}
		if !strings.Contains(ready.Message, "destination") {
			t.Fatalf("message should still say which side failed: %q", ready.Message)
		}
		if strings.Contains(logs.String(), bodyCanary) {
			t.Fatalf("response body leaked into logs:\n%s", logs.String())
		}
		if !strings.Contains(logs.String(), "provider build failed") {
			t.Fatalf("expected a server-side log line for the failure:\n%s", logs.String())
		}
	})

	t.Run("real vault login rejected by a hostile endpoint", func(t *testing.T) {
		const tokenCanary = "CANARY-sa-token-77ab"
		srv, hits := fakeVault(t, http.StatusForbidden, `{"errors":["`+bodyCanary+`"]}`)

		// In a pod the Vault kubernetes auth reads the controller's own
		// projected token from the default path. Tests have no such
		// file, so this factory points the real core factory at one,
		// outside the CR config the reconciler inspects.
		saToken := writeTokenFile(t, tokenCanary)
		r := providers.NewRegistry()
		registerStub(r, "aws-sm")
		r.Register(vault.Kind, func(ctx context.Context, cfg providers.Config) (providers.Provider, error) {
			withPodToken := maps.Clone(cfg)
			withPodToken[vault.ConfigKubernetesTokenPath] = saToken
			return vault.New(ctx, withPodToken)
		})

		cr := sampleCR("err-real-vault")
		cr.Spec.Destination.Config = map[string]string{
			vault.ConfigAddress:        srv.URL,
			vault.ConfigKubernetesRole: "app",
		}

		ctx, logs := capturedLogs(t)
		rec, cl := newReconciler(t, r, cr)
		_, got := reconcileAndGet(t, ctx, rec, cl, cr.Name)

		if hits.Load() == 0 {
			t.Fatal("precondition: the hostile endpoint was never reached, so this test proves nothing")
		}
		ready := findCondition(got.Status.Conditions, "Ready")
		if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != reasonProviderError {
			t.Fatalf("expected Ready=False/%s, got %+v", reasonProviderError, ready)
		}
		for _, leak := range []string{bodyCanary, tokenCanary, srv.URL} {
			if strings.Contains(ready.Message, leak) {
				t.Fatalf("status message leaked %q: %q", leak, ready.Message)
			}
		}
		for _, leak := range []string{bodyCanary, tokenCanary} {
			if strings.Contains(logs.String(), leak) {
				t.Fatalf("logs leaked %q:\n%s", leak, logs.String())
			}
		}
	})
}

// A tiny refreshInterval turns every SecretsSync into a hot loop
// against the providers. The CRD rejects values under 30s; the
// reconciler clamps anything stored before that rule existed.
func TestReconcile_RefreshInterval_Floor(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want time.Duration
	}{
		{0, 5 * time.Minute},
		{-time.Minute, 5 * time.Minute},
		{time.Nanosecond, 30 * time.Second},
		{time.Second, 30 * time.Second},
		{29 * time.Second, 30 * time.Second},
		{30 * time.Second, 30 * time.Second},
		{10 * time.Minute, 10 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.in.String(), func(t *testing.T) {
			cr := sampleCR("refresh")
			cr.Spec.RefreshInterval = metav1.Duration{Duration: tc.in}

			r := providers.NewRegistry()
			registerStub(r, "aws-sm")
			registerStub(r, "vault")

			rec, cl := newReconciler(t, r, cr)
			res, got := reconcileAndGet(t, t.Context(), rec, cl, cr.Name)
			if res.RequeueAfter != tc.want {
				t.Fatalf("RequeueAfter: got %v want %v", res.RequeueAfter, tc.want)
			}
			if ready := findCondition(got.Status.Conditions, "Ready"); ready == nil || ready.Reason != reasonValidated {
				t.Fatalf("clamping must not change the outcome, got %+v", ready)
			}
		})
	}
}

// Legit CRs keep working: the realistic config keys for both providers
// reach the factories unchanged and the CR goes Ready.
func TestReconcile_LegitConfig_PassesThroughUnchanged(t *testing.T) {
	srcCfg := map[string]string{
		"region":   "us-east-1",
		"roleArn":  "arn:aws:iam::123456789012:role/secrets-bridge-reader",
		"endpoint": "https://secretsmanager.example.com",
	}
	dstCfg := map[string]string{
		vault.ConfigAddress:             "https://vault.example.com",
		vault.ConfigNamespace:           "tenant-a-uat",
		vault.ConfigAuthMethod:          "kubernetes",
		vault.ConfigKubernetesRole:      "secrets-bridge",
		vault.ConfigKubernetesMountPath: "kubernetes-tenant-a",
		vault.ConfigKVMount:             "kv",
		vault.ConfigKVPrefix:            "team-alpha/uat",
	}

	cr := sampleCR("legit")
	cr.Spec.Source = syncv1alpha1.ProviderRef{Type: "aws-sm", Config: srcCfg}
	cr.Spec.Destination = syncv1alpha1.ProviderRef{Type: vault.Kind, Config: dstCfg}
	cr.Spec.RefreshInterval = metav1.Duration{Duration: 10 * time.Minute}

	seen := map[string]providers.Config{}
	r := providers.NewRegistry()
	for _, kind := range []string{"aws-sm", vault.Kind} {
		r.Register(kind, func(_ context.Context, cfg providers.Config) (providers.Provider, error) {
			seen[kind] = cfg
			return stubProvider{}, nil
		})
	}

	rec, cl := newReconciler(t, r, cr)
	res, got := reconcileAndGet(t, t.Context(), rec, cl, cr.Name)

	ready := findCondition(got.Status.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.Reason != reasonValidated {
		t.Fatalf("expected Ready=True/%s, got %+v", reasonValidated, ready)
	}
	if res.RequeueAfter != 10*time.Minute {
		t.Fatalf("RequeueAfter: got %v want 10m", res.RequeueAfter)
	}
	for kind, want := range map[string]map[string]string{"aws-sm": srcCfg, vault.Kind: dstCfg} {
		gotCfg := seen[kind]
		if len(gotCfg) != len(want) {
			t.Fatalf("%s factory got %d keys, want %d: %v", kind, len(gotCfg), len(want), gotCfg)
		}
		for k, v := range want {
			if gotCfg[k] != v {
				t.Fatalf("%s factory: key %q got %v want %q", kind, k, gotCfg[k], v)
			}
		}
	}
}
