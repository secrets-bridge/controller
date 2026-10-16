/*
Copyright 2026 Haider Raed.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package controller is the SecretsSync reconciler.
//
// The reconciler imports only core/providers — never api/pkg/storage,
// api/pkg/runtime, or any Control Plane internal — per the polyrepo
// dependency rule. Reading and writing actual secret values is the
// agent's job (BRD §12.4); this controller validates the CR, resolves
// source and destination providers from the Registry, and surfaces the
// reconcile outcome on the CR status.
package controller

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/secrets-bridge/core/providers"

	syncv1alpha1 "github.com/secrets-bridge/controller/api/v1alpha1"
)

// Ready condition reasons. They are a stable contract for anyone alerting
// on the condition, so don't rename them.
const (
	// ReasonValidated: both providers were built from the spec.
	ReasonValidated = "Validated"
	// ReasonProviderError: a provider could not be built. The status
	// message is generic; details are in the controller logs.
	ReasonProviderError = "ProviderError"
	// ReasonCredentialConfigRejected: a ProviderRef.Config carried a key
	// that holds a credential or points at a credential source.
	ReasonCredentialConfigRejected = "CredentialConfigRejected"
)

const (
	// DefaultRefreshInterval applies when Spec.RefreshInterval is unset
	// (or not positive).
	DefaultRefreshInterval = 5 * time.Minute
	// MinRefreshInterval is the floor for Spec.RefreshInterval. The CRD
	// rejects lower values; the reconciler clamps objects stored before
	// that rule existed. Keep it in line with the XValidation marker on
	// SecretsSyncSpec.RefreshInterval.
	MinRefreshInterval = 30 * time.Second
	// failedRequeue is how soon a failed validation is retried.
	failedRequeue = time.Minute
	// maxLoggedErrLen caps the provider error summary written to logs.
	maxLoggedErrLen = 200
)

// credentialConfigKeys are ProviderRef.Config keys that carry a
// credential or name a credential source (CTL-N-01). The controller
// authenticates to providers with its own workload identity only, so a
// SecretsSync may not supply any of these. They are matched without
// regard to case and refused on presence, before any provider is built.
//
// From core/providers/vault:
//   - "token": a static Vault token.
//   - "kubernetesTokenPath": a file the controller reads and sends to
//     the configured address as the Kubernetes auth `jwt`.
//
// core/providers/awssecretsmanager reads no credential from its config
// (region, roleArn and endpoint only; credentials come from the default
// AWS chain).
var credentialConfigKeys = []string{
	"token",
	"kubernetesTokenPath",
}

// SecretsSyncReconciler reconciles a SecretsSync object.
//
// Providers is the registry of available provider factories (one per
// supported backend kind: aws-sm, vault, ...). It is built in main and
// passed in here so unit tests can inject a fake registry with stub
// providers.
type SecretsSyncReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Providers *providers.Registry
}

// +kubebuilder:rbac:groups=sync.secrets-bridge.io,resources=secretssyncs,verbs=get;list;watch
// +kubebuilder:rbac:groups=sync.secrets-bridge.io,resources=secretssyncs/status,verbs=update;patch
//
// The reconciler reads SecretsSync objects and writes their status. It
// never creates, updates, patches or deletes them and adds no
// finalizers, so it gets no write verbs on the main resource and no
// secretssyncs/finalizers rule (CTL-N-02).
//
// No core/v1 Secret RBAC is declared here on purpose (security assessment
// CTL-01 / H5): this reconciler only validates that Spec.Source and
// Spec.Destination resolve via the Providers registry — it never Gets,
// Lists, or Watches a corev1.Secret, and ProviderRef.CredentialsSecretRef
// is not dereferenced anywhere in this package. If a future change wires
// CredentialsSecretRef up, add a narrowly-scoped Role (not a ClusterRole)
// for just the namespaces that need it, and add the marker back here.

// Reconcile is the entry point for each SecretsSync event.
//
// In v0.1.0 the reconciler also performed the sync. In the polyrepo
// architecture the sync execution moves to the agent, driven by the
// Control Plane API (issues secrets-bridge/api#4, #5; agent#1, #2).
// This controller is responsible for:
//   - validating the CR (both providers resolvable from the Registry)
//   - surfacing a Ready condition on status
//   - keeping the CR in sync with its observedGeneration
//
// Actual list/get/put work against the providers happens on the agent
// once Step 7 (registration) and Step 8 (job loop) land.
func (r *SecretsSyncReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("secretssync", req.Name)

	var cr syncv1alpha1.SecretsSync
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Refuse credential-bearing config on either side before building
	// anything: building a provider can already talk to the address in
	// the spec (Vault logs in from its factory).
	for _, side := range []struct {
		name string
		ref  syncv1alpha1.ProviderRef
	}{{"source", cr.Spec.Source}, {"destination", cr.Spec.Destination}} {
		if key, found := credentialConfigKey(side.ref.Config); found {
			logger.Info("refusing SecretsSync: credential-bearing config key",
				"side", side.name, "type", side.ref.Type, "key", key)
			return r.fail(ctx, &cr, ReasonCredentialConfigRejected, fmt.Sprintf(
				"%s config key %q is not allowed: provider credentials come from the controller's own identity, not the SecretsSync spec",
				side.name, key))
		}
	}

	// Resolve both providers from the registry. The factory is invoked
	// only as a validation step here — the returned Provider is not
	// retained. Once Steps 7/8 land, the resolved providers will be
	// used to dispatch a sync job to the agent.
	//
	// Provider error text can carry a response body from the endpoint
	// named in the spec, so it never goes into status, and only a
	// one-line summary goes into the logs.
	if _, err := r.buildProvider(ctx, cr.Spec.Source); err != nil {
		logger.Error(nil, "provider build failed", providerErrorLogValues("source", cr.Spec.Source.Type, err)...)
		return r.fail(ctx, &cr, ReasonProviderError,
			"source provider could not be built; see the controller logs for details")
	}
	if _, err := r.buildProvider(ctx, cr.Spec.Destination); err != nil {
		logger.Error(nil, "provider build failed", providerErrorLogValues("destination", cr.Spec.Destination.Type, err)...)
		return r.fail(ctx, &cr, ReasonProviderError,
			"destination provider could not be built; see the controller logs for details")
	}

	logger.Info("reconcile",
		"direction", cr.Spec.Direction,
		"source", cr.Spec.Source.Type,
		"destination", cr.Spec.Destination.Type,
	)

	now := metav1.NewTime(time.Now())
	cr.Status.LastReconcileTime = &now
	cr.Status.ObservedGeneration = cr.Generation
	cr.Status.Conditions = setCondition(cr.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             ReasonValidated,
		Message:            "providers resolved; sync execution will be performed by the agent (BRD §12.4)",
		LastTransitionTime: now,
	})

	if err := r.Status().Update(ctx, &cr); err != nil {
		// Conflict (409) is expected when multiple events race; the
		// next reconcile will see the new ResourceVersion.
		if !apierrors.IsConflict(err) {
			logger.Error(err, "failed to update status")
		}
	}

	requeue := refreshInterval(cr.Spec.RefreshInterval.Duration)
	if requeue != cr.Spec.RefreshInterval.Duration && cr.Spec.RefreshInterval.Duration > 0 {
		logger.Info("refreshInterval below the minimum, clamping",
			"refreshInterval", cr.Spec.RefreshInterval.Duration.String(), "using", requeue.String())
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// refreshInterval turns Spec.RefreshInterval into a requeue delay:
// unset or non-positive means the default, anything under the floor is
// raised to it.
func refreshInterval(d time.Duration) time.Duration {
	switch {
	case d <= 0:
		return DefaultRefreshInterval
	case d < MinRefreshInterval:
		return MinRefreshInterval
	default:
		return d
	}
}

// credentialConfigKey reports the first credential-bearing key present
// in cfg, by its canonical name. Presence is enough, even with a blank
// value.
func credentialConfigKey(cfg map[string]string) (string, bool) {
	for _, forbidden := range credentialConfigKeys {
		for k := range cfg {
			if strings.EqualFold(strings.TrimSpace(k), forbidden) {
				return forbidden, true
			}
		}
	}
	return "", false
}

// providerErrorLogValues describes a provider build failure for the
// logs without the parts of the error that can carry a response body:
// only the first line, stripped of non-printable characters and capped
// in length. Errors from core providers put the remote body after the
// first line (for example vault/api's "Error making API
// request.\n\nURL: ...").
func providerErrorLogValues(side, providerType string, err error) []any {
	return []any{"side", side, "type", providerType, "summary", errSummary(err)}
}

func errSummary(err error) string {
	s := err.Error()
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s)
	if r := []rune(s); len(r) > maxLoggedErrLen {
		s = string(r[:maxLoggedErrLen]) + "..."
	}
	return s
}

// buildProvider converts a CR's ProviderRef into a providers.Config and
// asks the registry to instantiate. The string-valued config map from
// the CRD is widened to map[string]any since providers.Config is
// opaque to each connector.
func (r *SecretsSyncReconciler) buildProvider(ctx context.Context, ref syncv1alpha1.ProviderRef) (providers.Provider, error) {
	cfg := make(providers.Config, len(ref.Config))
	for k, v := range ref.Config {
		cfg[k] = v
	}
	return r.Providers.Build(ctx, ref.Type, cfg)
}

// fail marks the CR Ready=False. reason must be one of the Reason*
// constants and msg must be a fixed, generic string: never provider
// error text or config values.
func (r *SecretsSyncReconciler) fail(ctx context.Context, cr *syncv1alpha1.SecretsSync, reason, msg string) (ctrl.Result, error) {
	now := metav1.NewTime(time.Now())
	cr.Status.LastReconcileTime = &now
	cr.Status.ObservedGeneration = cr.Generation
	cr.Status.Conditions = setCondition(cr.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: now,
	})
	if err := r.Status().Update(ctx, cr); err != nil && !apierrors.IsConflict(err) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: failedRequeue}, nil
}

// setCondition replaces or appends a condition by type.
func setCondition(in []metav1.Condition, c metav1.Condition) []metav1.Condition {
	for i := range in {
		if in[i].Type == c.Type {
			in[i] = c
			return in
		}
	}
	return append(in, c)
}

// SetupWithManager wires the controller into the manager.
func (r *SecretsSyncReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Providers == nil {
		return fmt.Errorf("SecretsSyncReconciler: Providers registry is nil")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&syncv1alpha1.SecretsSync{}).
		Named("secretssync").
		Complete(r)
}
