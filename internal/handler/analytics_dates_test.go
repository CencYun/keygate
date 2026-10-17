package handler

import (
	"testing"
	"time"
)

// An activation made during the "to" day must fall inside the bound.
// Midnight of that day, the previous behaviour for the activation
// trend, left it out.
func TestEndOfDayIncludesTheWholeDate(t *testing.T) {
	to, ok := endOfDay("2026-10-14")
	if !ok {
		t.Fatal("valid date rejected")
	}
	// Stated to the microsecond, the precision bun writes times at.
	if want := time.Date(2026, 10, 14, 23, 59, 59, 999999000, time.UTC); !to.Equal(want) {
		t.Errorf("bound %v, want %v", to, want)
	}
	lateThatDay := time.Date(2026, 10, 14, 23, 59, 59, 0, time.UTC)
	if lateThatDay.After(to) {
		t.Errorf("activation at %v is after the bound %v", lateThatDay, to)
	}
	nextDay := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	if !nextDay.After(to) {
		t.Errorf("next midnight %v is inside the bound %v", nextDay, to)
	}
}

func TestEndOfDayRejectsBadInput(t *testing.T) {
	if to, ok := endOfDay("14/10/2026"); ok || !to.IsZero() {
		t.Errorf("got %v, %v; want zero time, false", to, ok)
	}
}
