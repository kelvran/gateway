package dataplane

import (
	"net/http"
	"testing"
)

// TestClientForDeploymentFallsBackToDefaultWhenAbsent proves the common
// case (no perDeployment override at all, or this deployment's name
// isn't in it) is a silent no-op -- the exact behavior every deployment
// without a TLSConfig relies on.
func TestClientForDeploymentFallsBackToDefaultWhenAbsent(t *testing.T) {
	defaultClient := &http.Client{}
	if got := clientForDeployment(Deployment{Name: "d1"}, defaultClient, nil); got != defaultClient {
		t.Error("clientForDeployment with a nil perDeployment map did not return defaultClient")
	}

	other := &http.Client{}
	perDeployment := map[string]*http.Client{"d2": other}
	if got := clientForDeployment(Deployment{Name: "d1"}, defaultClient, perDeployment); got != defaultClient {
		t.Error("clientForDeployment for a deployment absent from perDeployment did not return defaultClient")
	}
}

// TestClientForDeploymentReturnsOverrideWhenPresent proves the positive
// case: a deployment present in perDeployment gets its own dedicated
// client, never the shared default.
func TestClientForDeploymentReturnsOverrideWhenPresent(t *testing.T) {
	defaultClient := &http.Client{}
	dedicated := &http.Client{}
	perDeployment := map[string]*http.Client{"tls-dep": dedicated}

	if got := clientForDeployment(Deployment{Name: "tls-dep"}, defaultClient, perDeployment); got != dedicated {
		t.Error("clientForDeployment for a deployment present in perDeployment did not return the dedicated client")
	}
}
