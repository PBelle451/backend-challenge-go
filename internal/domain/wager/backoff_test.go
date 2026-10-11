package wager_test

import (
	"math"
	"testing"
	"time"

	"github.com/PBelle451/backend-challenge-go/internal/domain/wager"
)

func TestBackoff(t *testing.T) {
	base, max := time.Second, 30*time.Second
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30, 30}
	for i, w := range want {
		attempt := i + 1
		if got := wager.Backoff(attempt, base, max); got != w*time.Second {
			t.Errorf("Backoff(%d) = %v, want %v", attempt, got, w*time.Second)
		}
	}
	// nunca diminui
	prev := time.Duration(0)
	for a := 1; a <= 80; a++ {
		got := wager.Backoff(a, base, max)
		if got < prev {
			t.Fatalf("backoff decreased at attempt %d: %v < %v", a, got, prev)
		}
		prev = got
	}
}

func TestBackoff_Edges(t *testing.T) {
	if got := wager.Backoff(0, time.Second, time.Minute); got != time.Second {
		t.Errorf("attempt 0 = %v, want base", got)
	}
	if got := wager.Backoff(-5, time.Second, time.Minute); got != time.Second {
		t.Errorf("negative attempt = %v, want base", got)
	}
	if got := wager.Backoff(3, 0, time.Minute); got != 0 {
		t.Errorf("zero base = %v, want 0", got)
	}
	if got := wager.Backoff(3, -time.Second, time.Minute); got != 0 {
		t.Errorf("negative base = %v, want 0", got)
	}
	if got := wager.Backoff(4, 10*time.Second, time.Second); got != 10*time.Second {
		t.Errorf("max < base must fall back to base, got %v", got)
	}
	// sem overflow mesmo com um teto enorme e muitas tentativas
	huge := time.Duration(math.MaxInt64)
	for _, a := range []int{1, 10, 62, 63, 64, 1000, math.MaxInt32} {
		if got := wager.Backoff(a, time.Second, huge); got <= 0 {
			t.Errorf("Backoff(%d) overflowed: %v", a, got)
		}
	}
}

func TestReferenceExhausted(t *testing.T) {
	now := t0
	deadline := t0.Add(time.Hour)
	tests := []struct {
		name     string
		attempts int
		now      time.Time
		want     bool
	}{
		{"dentro do prazo e das tentativas", 3, now, false},
		{"ultima tentativa ainda disponivel", 9, now, false},
		{"tentativas esgotadas", 10, now, true},
		{"tentativas acima do limite", 11, now, true},
		{"um instante antes do prazo", 3, deadline.Add(-time.Nanosecond), false},
		{"exatamente no prazo", 3, deadline, true},
		{"depois do prazo", 3, deadline.Add(time.Second), true},
	}
	for _, tc := range tests {
		if got := wager.ReferenceExhausted(tc.attempts, 10, tc.now, deadline); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
