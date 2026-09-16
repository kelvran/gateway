package router

import "testing"

// TestNewModelStateOnEmptyDeploymentSliceDoesNotPanic is the regression
// proof for the real bug fixed in newModelState's own doc comment: a
// zero-length deps slice used to panic on the unconditional
// normalized[0] access, even though next() already had a dedicated
// n==0 defensive branch that could never run as a result. Not reachable
// via any current call site (New/SetWeight), but newModelState itself
// must degrade gracefully if a future caller ever does pass one.
func TestNewModelStateOnEmptyDeploymentSliceDoesNotPanic(t *testing.T) {
	ms := newModelState(nil)
	if ms == nil {
		t.Fatal("newModelState(nil) returned nil")
	}

	name, ok := ms.next()
	if ok {
		t.Errorf("next() on a zero-deployment modelState returned ok=true, name=%q, want ok=false", name)
	}
}
