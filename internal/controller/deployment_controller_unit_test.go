package controller

import (
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestParseRulesFromConfigMap(t *testing.T) {
	cm := &corev1.ConfigMap{Data: map[string]string{
		"rules.yaml":             "namespaces:\n  - demo\nnamePrefix: demo-\nlabelKey: kedascan\nlabelValue: \"true\"",
		"scaledobject-spec.yaml": "triggers: []\nscaleTargetRef: {}",
	}}

	rules, err := parseRulesFromConfigMap(cm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rules.Namespaces) != 1 || rules.Namespaces[0] != "demo" {
		t.Fatalf("unexpected namespaces: %#v", rules.Namespaces)
	}
	if rules.ScaledObjectSpec["scaleTargetRef"] == nil {
		t.Fatalf("expected scaleTargetRef to be present after parsing")
	}

	// ensure spec is JSON serializable to detect accidental map key regressions
	if _, err := json.Marshal(rules.ScaledObjectSpec); err != nil {
		t.Fatalf("scaled object spec should be JSON marshalable: %v", err)
	}
}

func TestParseRulesFromConfigMapRejectsEmptyNamespaces(t *testing.T) {
	cm := &corev1.ConfigMap{Data: map[string]string{
		"rules.yaml":             "namespaces:\n  - \nnamePrefix: demo-\nlabelKey: kedascan\nlabelValue: \"true\"",
		"scaledobject-spec.yaml": "triggers: []",
	}}

	if _, err := parseRulesFromConfigMap(cm); err == nil {
		t.Fatalf("expected error when namespaces are empty")
	}
}

func TestMatchesRules(t *testing.T) {
	rules := &Rules{
		Namespaces: []string{"demo"},
		NamePrefix: "demo-",
		LabelKey:   "kedascan",
		LabelValue: "true",
	}

	deployment := &appsv1.Deployment{}
	deployment.Name = "demo-app"
	deployment.Labels = map[string]string{"kedascan": "true"}

	if !(&DeploymentReconciler{}).matchesRules(deployment, rules) {
		t.Fatalf("expected deployment to match rules")
	}

	deployment.Name = "other-app"
	if (&DeploymentReconciler{}).matchesRules(deployment, rules) {
		t.Fatalf("deployment name prefix should be enforced")
	}
}
