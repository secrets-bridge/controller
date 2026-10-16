/*
Copyright 2026 Haider Raed.
SPDX-License-Identifier: Apache-2.0
*/

package controller

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

// These tests read the generated manifests under config/ so a later
// controller-gen run (or a hand edit) can't quietly widen RBAC again
// or drop the refreshInterval floor from the CRD.

func decodeManifest(t *testing.T, rel string, into any) {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("open %s: %v", rel, err)
	}
	defer func() { _ = f.Close() }()
	if err := k8syaml.NewYAMLOrJSONDecoder(f, 4096).Decode(into); err != nil {
		t.Fatalf("decode %s: %v", rel, err)
	}
}

func assertVerbsWithin(t *testing.T, what string, got []string, allowed ...string) {
	t.Helper()
	for _, v := range got {
		if !slices.Contains(allowed, v) {
			t.Fatalf("%s: verb %q is not allowed (allowed: %v, got: %v)", what, v, allowed, got)
		}
	}
}

// CTL-N-02: the reconciler only reads SecretsSync objects and writes
// their status. It never creates, updates, patches or deletes them and
// uses no finalizers.
func TestManagerRole_LeastPrivilege(t *testing.T) {
	var role rbacv1.ClusterRole
	decodeManifest(t, "config/rbac/role.yaml", &role)

	seen := map[string]bool{}
	for _, rule := range role.Rules {
		for _, res := range rule.Resources {
			seen[res] = true
			switch res {
			case "secretssyncs":
				assertVerbsWithin(t, res, rule.Verbs, "get", "list", "watch")
			case "secretssyncs/status":
				assertVerbsWithin(t, res, rule.Verbs, "update", "patch")
			default:
				t.Fatalf("unexpected resource %q in manager-role (verbs %v)", res, rule.Verbs)
			}
		}
	}
	for _, res := range []string{"secretssyncs", "secretssyncs/status"} {
		if !seen[res] {
			t.Fatalf("manager-role is missing a rule for %q", res)
		}
	}
}

// CTL-N-02: controller-runtime's leader election uses a Lease lock
// (Get/Create/Update) and records core/v1 events; ConfigMaps are not
// used at all.
func TestLeaderElectionRole_LeasesAndEventsOnly(t *testing.T) {
	var role rbacv1.Role
	decodeManifest(t, "config/rbac/leader_election_role.yaml", &role)

	var leaseVerbs []string
	for _, rule := range role.Rules {
		for _, res := range rule.Resources {
			switch res {
			case "leases":
				assertVerbsWithin(t, res, rule.Verbs, "get", "create", "update")
				leaseVerbs = append(leaseVerbs, rule.Verbs...)
			case "events":
				assertVerbsWithin(t, res, rule.Verbs, "create", "patch")
			default:
				t.Fatalf("unexpected resource %q in leader-election-role (verbs %v)", res, rule.Verbs)
			}
		}
	}
	for _, v := range []string{"get", "create", "update"} {
		if !slices.Contains(leaseVerbs, v) {
			t.Fatalf("leader election needs %q on leases, got %v", v, leaseVerbs)
		}
	}
}

// The CRD must refuse a refreshInterval under 30s at admission time.
func TestCRD_RefreshIntervalHasFloor(t *testing.T) {
	var crd map[string]any
	decodeManifest(t, "config/crd/sync.secrets-bridge.io_secretssyncs.yaml", &crd)

	field := dig(t, crd, "spec", "versions", 0, "schema", "openAPIV3Schema",
		"properties", "spec", "properties", "refreshInterval")
	rules, _ := field.(map[string]any)["x-kubernetes-validations"].([]any)
	for _, r := range rules {
		rule, _ := r.(map[string]any)["rule"].(string)
		if strings.Contains(rule, "duration(self)") && strings.Contains(rule, "duration('30s')") {
			return
		}
	}
	t.Fatalf("refreshInterval has no 30s floor rule; x-kubernetes-validations = %v", rules)
}

func dig(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, p := range path {
		switch key := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("expected an object at %q", key)
			}
			v = m[key]
		case int:
			s, ok := v.([]any)
			if !ok || len(s) <= key {
				t.Fatalf("expected a list with index %d", key)
			}
			v = s[key]
		}
	}
	return v
}
