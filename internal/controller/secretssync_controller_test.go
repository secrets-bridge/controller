/*
Copyright 2026 Haider Raed.
SPDX-License-Identifier: Apache-2.0
*/

package controller

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/secrets-bridge/core/providers"

	syncv1alpha1 "github.com/secrets-bridge/controller/api/v1alpha1"
)

// stubProvider satisfies providers.Provider without doing any work.
// Returned by registered factories during tests.
type stubProvider struct{}

func (stubProvider) GetMetadata(context.Context, providers.SecretRef) (providers.SecretMetadata, error) {
	return providers.SecretMetadata{}, providers.ErrNotFound
}
func (stubProvider) ListMetadata(context.Context, providers.ProviderScope) ([]providers.SecretMetadata, error) {
	return nil, nil
}
func (stubProvider) GetValue(context.Context, providers.SecretRef) (providers.SecretValue, error) {
	return providers.SecretValue{}, providers.ErrNotFound
}
func (stubProvider) PutValue(context.Context, providers.SecretRef, providers.SecretValue, providers.PutOptions) (providers.SecretVersion, error) {
	return providers.SecretVersion{}, nil
}

func registerStub(r *providers.Registry, kind string) {
	r.Register(kind, func(context.Context, providers.Config) (providers.Provider, error) {
		return stubProvider{}, nil
	})
}

func registerFailing(r *providers.Registry, kind string, err error) {
	r.Register(kind, func(context.Context, providers.Config) (providers.Provider, error) {
		return nil, err
	})
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1 scheme: %v", err)
	}
	if err := syncv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("syncv1alpha1 scheme: %v", err)
	}
	return s
}

func newReconciler(t *testing.T, registry *providers.Registry, objs ...client.Object) (*SecretsSyncReconciler, client.Client) {
	t.Helper()
	scheme := newScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&syncv1alpha1.SecretsSync{}).
		Build()
	return &SecretsSyncReconciler{
		Client:    c,
		Scheme:    scheme,
		Providers: registry,
	}, c
}

func sampleCR(name string) *syncv1alpha1.SecretsSync {
	return &syncv1alpha1.SecretsSync{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 7},
		Spec: syncv1alpha1.SecretsSyncSpec{
			Source:      syncv1alpha1.ProviderRef{Type: "aws-sm", Config: map[string]string{"region": "us-east-1"}},
			Destination: syncv1alpha1.ProviderRef{Type: "vault", Config: map[string]string{"address": "https://vault.example.com"}},
			Direction:   syncv1alpha1.SourceToDestination,
		},
	}
}

func TestReconcile_Validated_SetsReadyTrue(t *testing.T) {
	cr := sampleCR("happy")

	r := providers.NewRegistry()
	registerStub(r, "aws-sm")
	registerStub(r, "vault")

	rec, cl := newReconciler(t, r, cr)
	res, err := rec.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: cr.Name},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Fatalf("expected positive RequeueAfter, got %v", res.RequeueAfter)
	}

	var got syncv1alpha1.SecretsSync
	if err := cl.Get(t.Context(), client.ObjectKey{Name: cr.Name}, &got); err != nil {
		t.Fatalf("get back CR: %v", err)
	}
	if got.Status.ObservedGeneration != 7 {
		t.Fatalf("ObservedGeneration: got %d want 7", got.Status.ObservedGeneration)
	}
	if got.Status.LastReconcileTime == nil {
		t.Fatal("LastReconcileTime not set")
	}
	ready := findCondition(got.Status.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("Ready condition: %+v", ready)
	}
	if ready.Reason != "Validated" {
		t.Fatalf("Ready.Reason: got %q want Validated", ready.Reason)
	}
}

func TestReconcile_UnregisteredSource_SetsReadyFalse(t *testing.T) {
	cr := sampleCR("bad-source")
	cr.Spec.Source.Type = "missing-provider"

	r := providers.NewRegistry()
	registerStub(r, "vault") // destination only

	rec, cl := newReconciler(t, r, cr)
	if _, err := rec.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: cr.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got syncv1alpha1.SecretsSync
	_ = cl.Get(t.Context(), client.ObjectKey{Name: cr.Name}, &got)
	ready := findCondition(got.Status.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("expected Ready=False, got %+v", ready)
	}
	if ready.Reason != "ProviderError" {
		t.Fatalf("Reason: got %q want ProviderError", ready.Reason)
	}
}

func TestReconcile_FailingDestinationFactory_SurfacesError(t *testing.T) {
	cr := sampleCR("bad-dest")

	r := providers.NewRegistry()
	registerStub(r, "aws-sm")
	registerFailing(r, "vault", errors.New("invalid kubernetesRole"))

	rec, cl := newReconciler(t, r, cr)
	if _, err := rec.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: cr.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got syncv1alpha1.SecretsSync
	_ = cl.Get(t.Context(), client.ObjectKey{Name: cr.Name}, &got)
	ready := findCondition(got.Status.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("expected Ready=False, got %+v", ready)
	}
	// The factory's actual error text must be surfaced so operators
	// can diagnose without digging through pod logs.
	if !contains(ready.Message, "invalid kubernetesRole") {
		t.Fatalf("error message not propagated: %q", ready.Message)
	}
}

func TestReconcile_NotFound_NoError(t *testing.T) {
	r := providers.NewRegistry()
	rec, _ := newReconciler(t, r) // empty client
	res, err := rec.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "missing"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected no RequeueAfter, got %v", res.RequeueAfter)
	}
}

func TestSetupWithManager_NilRegistry_Errors(t *testing.T) {
	// Guards against a refactor accidentally constructing the reconciler
	// without wiring providers in main.
	r := &SecretsSyncReconciler{}
	if err := r.SetupWithManager(nil); err == nil {
		t.Fatal("expected error when Providers is nil")
	}
}

func findCondition(conds []metav1.Condition, t string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == t {
			return &conds[i]
		}
	}
	return nil
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
