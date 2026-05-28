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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SyncDirection controls how secrets flow between Source and Destination.
//
// +kubebuilder:validation:Enum=SourceToDestination;DestinationToSource;Bidirectional
type SyncDirection string

const (
	SourceToDestination SyncDirection = "SourceToDestination"
	DestinationToSource SyncDirection = "DestinationToSource"
	Bidirectional       SyncDirection = "Bidirectional"
)

// ConflictStrategy controls what happens when Bidirectional sync detects
// the same secret has changed on both sides since last reconcile.
//
// +kubebuilder:validation:Enum=SourceWins;DestinationWins;NewestWins;Fail
type ConflictStrategy string

const (
	SourceWins      ConflictStrategy = "SourceWins"
	DestinationWins ConflictStrategy = "DestinationWins"
	NewestWins      ConflictStrategy = "NewestWins"
	FailOnConflict  ConflictStrategy = "Fail"
)

// ProviderRef points at a backend by name + opaque config map. The Provider
// package interprets `config` (region/url/role/etc.) in its own way.
type ProviderRef struct {
	// Type is the registered provider name.
	//
	// +kubebuilder:validation:Enum=aws;gcp;azure;vault
	Type string `json:"type"`

	// Config is a free-form bag of provider-specific settings (region,
	// endpoint, role to assume, KV mount path, etc.). See each provider's
	// godoc for accepted keys.
	//
	// +optional
	Config map[string]string `json:"config,omitempty"`

	// CredentialsSecretRef references a Kubernetes Secret holding any
	// extra credentials the provider needs (PAT, client_secret, etc.).
	// Workload-identity-based auth (IRSA / GCP WI / Azure WI) is preferred
	// and needs no Secret.
	//
	// +optional
	CredentialsSecretRef *SecretReference `json:"credentialsSecretRef,omitempty"`
}

// SecretReference is a namespaced K8s Secret reference.
type SecretReference struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// Selector filters which secrets are in scope for the sync.
type Selector struct {
	// IncludePrefixes — only sync secret names with at least one of these prefixes.
	// Empty means "include everything".
	// +optional
	IncludePrefixes []string `json:"includePrefixes,omitempty"`

	// ExcludePrefixes — never sync secret names with any of these prefixes.
	// Evaluated after IncludePrefixes.
	// +optional
	ExcludePrefixes []string `json:"excludePrefixes,omitempty"`
}

// SecretsSyncSpec defines the desired state of a SecretsSync.
type SecretsSyncSpec struct {
	// Source is the side secrets are read from.
	Source ProviderRef `json:"source"`

	// Destination is the side secrets are written to (and read from for
	// Bidirectional drift detection).
	Destination ProviderRef `json:"destination"`

	// Selector narrows which secrets are in scope.
	// +optional
	Selector Selector `json:"selector,omitempty"`

	// Direction controls the flow. Default: SourceToDestination.
	// +kubebuilder:default=SourceToDestination
	// +optional
	Direction SyncDirection `json:"direction,omitempty"`

	// ConflictStrategy is consulted only when Direction == Bidirectional.
	// Default: SourceWins.
	// +kubebuilder:default=SourceWins
	// +optional
	ConflictStrategy ConflictStrategy `json:"conflictStrategy,omitempty"`

	// RefreshInterval is how often the controller re-reconciles even when
	// there are no Kubernetes events. Use a duration string like "5m".
	// Default: "5m".
	// +kubebuilder:default="5m"
	// +optional
	RefreshInterval metav1.Duration `json:"refreshInterval,omitempty"`

	// DeleteOrphans — when true, secrets that exist on the destination but
	// not on the source (and match the Selector) are deleted from the
	// destination. Only honoured for SourceToDestination.
	// Default: false (safer).
	// +optional
	DeleteOrphans bool `json:"deleteOrphans,omitempty"`
}

// SecretSyncResult is a per-secret status entry surfaced in Status.Results.
type SecretSyncResult struct {
	// Name is the canonical secret name.
	Name string `json:"name"`

	// Phase is one of Synced, Skipped, Failed.
	Phase string `json:"phase"`

	// Message is human-readable detail (error text on Failed).
	// +optional
	Message string `json:"message,omitempty"`

	// LastSyncTime is when the reconciler last touched this secret.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`
}

// SecretsSyncStatus defines the observed state of a SecretsSync.
type SecretsSyncStatus struct {
	// Conditions follows the standard k8s convention. Type "Ready" means
	// the last reconcile completed without error.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the spec.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastReconcileTime is when the controller last ran (regardless of outcome).
	// +optional
	LastReconcileTime *metav1.Time `json:"lastReconcileTime,omitempty"`

	// SyncedCount is the number of secrets currently in sync.
	// +optional
	SyncedCount int32 `json:"syncedCount,omitempty"`

	// FailedCount is the number of secrets that failed on the last reconcile.
	// +optional
	FailedCount int32 `json:"failedCount,omitempty"`

	// Results is a per-secret breakdown. Capped to avoid unbounded growth;
	// excess entries are summarised in metrics.
	// +optional
	Results []SecretSyncResult `json:"results,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=ss
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.source.type`
// +kubebuilder:printcolumn:name="Dest",type=string,JSONPath=`.spec.destination.type`
// +kubebuilder:printcolumn:name="Direction",type=string,JSONPath=`.spec.direction`
// +kubebuilder:printcolumn:name="Synced",type=integer,JSONPath=`.status.syncedCount`
// +kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=`.status.failedCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SecretsSync is the Schema for the secretssyncs API.
type SecretsSync struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SecretsSyncSpec   `json:"spec,omitempty"`
	Status SecretsSyncStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SecretsSyncList contains a list of SecretsSync.
type SecretsSyncList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SecretsSync `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SecretsSync{}, &SecretsSyncList{})
}
