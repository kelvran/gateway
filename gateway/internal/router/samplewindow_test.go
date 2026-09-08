package router

import "testing"

func TestSampleWindowBelowFloorReportsNotEnoughSignal(t *testing.T) {
	w := NewSampleWindow(5)
	for i := 0; i < 4; i++ {
		w.Record("d1")
	}
	if w.HasEnoughSignal("d1") {
		t.Fatalf("HasEnoughSignal(d1) after 4 records against a floor of 5 = true, want false")
	}
	if got := w.Count("d1"); got != 4 {
		t.Errorf("Count(d1) = %d, want 4", got)
	}
}

func TestSampleWindowAtOrAboveFloorReportsEnoughSignal(t *testing.T) {
	w := NewSampleWindow(5)
	for i := 0; i < 5; i++ {
		w.Record("d1")
	}
	if !w.HasEnoughSignal("d1") {
		t.Fatal("HasEnoughSignal(d1) after exactly 5 records against a floor of 5 = false, want true")
	}
	w.Record("d1")
	if !w.HasEnoughSignal("d1") {
		t.Fatal("HasEnoughSignal(d1) after 6 records against a floor of 5 = false, want true")
	}
}

func TestSampleWindowNeverRecordedNameHasNoSignal(t *testing.T) {
	w := NewSampleWindow(1)
	if w.HasEnoughSignal("never-touched") {
		t.Fatal("HasEnoughSignal(never-touched) = true, want false — zero recorded outcomes")
	}
	if got := w.Count("never-touched"); got != 0 {
		t.Errorf("Count(never-touched) = %d, want 0", got)
	}
}

func TestSampleWindowZeroOrNegativeFloorMeansAlwaysEnoughSignal(t *testing.T) {
	for _, min := range []int{0, -1, -100} {
		w := NewSampleWindow(min)
		if !w.HasEnoughSignal("never-touched") {
			t.Errorf("min=%d: HasEnoughSignal(never-touched) = false, want true (0/negative floor means no restriction)", min)
		}
	}
}

func TestSampleWindowCountsAreIndependentPerDeployment(t *testing.T) {
	w := NewSampleWindow(3)
	w.Record("d1")
	w.Record("d1")
	w.Record("d1")
	w.Record("d2")

	if !w.HasEnoughSignal("d1") {
		t.Error("HasEnoughSignal(d1) = false, want true (3 records against a floor of 3)")
	}
	if w.HasEnoughSignal("d2") {
		t.Error("HasEnoughSignal(d2) = true, want false (1 record against a floor of 3)")
	}
}

func TestSampleWindowResetZeroesCount(t *testing.T) {
	w := NewSampleWindow(2)
	w.Record("d1")
	w.Record("d1")
	if !w.HasEnoughSignal("d1") {
		t.Fatal("setup: HasEnoughSignal(d1) = false, want true before Reset")
	}

	w.Reset("d1")
	if w.HasEnoughSignal("d1") {
		t.Fatal("HasEnoughSignal(d1) after Reset = true, want false")
	}
	if got := w.Count("d1"); got != 0 {
		t.Errorf("Count(d1) after Reset = %d, want 0", got)
	}
}
