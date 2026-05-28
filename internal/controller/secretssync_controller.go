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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/secrets-bridge/core/providers"

	syncv1alpha1 "github.com/secrets-bridge/controller/api/v1alpha1"
)

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

// +kubebuilder:rbac:groups=sync.secrets-bridge.io,resources=secretssyncs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sync.secrets-bridge.io,resources=secretssyncs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sync.secrets-bridge.io,resources=secretssyncs/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

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

	// Resolve both providers from the registry. The factory is invoked
	// only as a validation step here — the returned Provider is not
	// retained. Once Steps 7/8 land, the resolved providers will be
	// used to dispatch a sync job to the agent.
	if _, err := r.buildProvider(ctx, cr.Spec.Source); err != nil {
		return r.fail(ctx, &cr, fmt.Sprintf("source provider build failed: %v", err))
	}
	if _, err := r.buildProvider(ctx, cr.Spec.Destination); err != nil {
		return r.fail(ctx, &cr, fmt.Sprintf("destination provider build failed: %v", err))
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
		Reason:             "Validated",
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

	requeue := cr.Spec.RefreshInterval.Duration
	if requeue == 0 {
		requeue = 5 * time.Minute
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
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

func (r *SecretsSyncReconciler) fail(ctx context.Context, cr *syncv1alpha1.SecretsSync, msg string) (ctrl.Result, error) {
	now := metav1.NewTime(time.Now())
	cr.Status.LastReconcileTime = &now
	cr.Status.ObservedGeneration = cr.Generation
	cr.Status.Conditions = setCondition(cr.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "ProviderError",
		Message:            msg,
		LastTransitionTime: now,
	})
	if err := r.Status().Update(ctx, cr); err != nil && !apierrors.IsConflict(err) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: time.Minute}, nil
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
