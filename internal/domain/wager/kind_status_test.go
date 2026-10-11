package wager_test

import (
	"testing"

	"github.com/PBelle451/backend-challenge-go/internal/domain/wager"
	"github.com/PBelle451/backend-challenge-go/internal/domain/wallet"
)

func TestKindProperties(t *testing.T) {
	tests := []struct {
		kind                         wager.Kind
		external, requiresRef, allow bool
		dir                          wallet.Direction
		dirOK                        bool
	}{
		{wager.KindOpening, false, false, false, wallet.DirectionCredit, true},
		{wager.KindBet, true, false, false, wallet.DirectionDebit, true},
		{wager.KindWin, true, false, true, wallet.DirectionCredit, true},
		{wager.KindLoss, true, false, false, "", false},
		{wager.KindRefund, true, true, true, wallet.DirectionCredit, true},
		{wager.KindRollback, true, true, true, "", false}, // depende da referência
	}
	for _, tc := range tests {
		t.Run(string(tc.kind), func(t *testing.T) {
			if !tc.kind.Valid() || tc.kind.IsExternal() != tc.external ||
				tc.kind.RequiresReference() != tc.requiresRef || tc.kind.AllowsReference() != tc.allow {
				t.Errorf("unexpected properties for %s", tc.kind)
			}
			d, ok := tc.kind.Direction()
			if d != tc.dir || ok != tc.dirOK {
				t.Errorf("Direction() = %q, %v; want %q, %v", d, ok, tc.dir, tc.dirOK)
			}
		})
	}
	for _, bad := range []wager.Kind{"", "bet", "TRANSFER", "OPENING "} {
		if bad.Valid() || bad.IsExternal() {
			t.Errorf("%q must not be a valid kind", bad)
		}
	}
}

func TestStatusProperties(t *testing.T) {
	tests := []struct {
		s                 wager.Status
		terminal, pending bool
	}{
		{wager.StatusPending, false, true},
		{wager.StatusPendingReference, false, true},
		{wager.StatusProcessed, true, false},
		{wager.StatusRejected, true, false},
		{wager.StatusFailed, true, false},
	}
	for _, tc := range tests {
		if !tc.s.Valid() || tc.s.IsTerminal() != tc.terminal || tc.s.IsPending() != tc.pending {
			t.Errorf("unexpected properties for %s", tc.s)
		}
	}
	if wager.Status("DONE").Valid() || wager.Status("").Valid() {
		t.Error("unknown statuses must not be valid")
	}
}

// Os valores de failureCode são contrato público: renomear um deles deve quebrar este teste.
func TestFailureCodesAreStable(t *testing.T) {
	want := map[wager.FailureCode]struct {
		value string
		class wager.FailureClass
	}{
		wager.FailureInsufficientFunds:         {"INSUFFICIENT_FUNDS", wager.ClassDefinitive},
		wager.FailureReversalInsufficientFunds: {"REVERSAL_INSUFFICIENT_FUNDS", wager.ClassDefinitive},
		wager.FailureAlreadyReversed:           {"ALREADY_REVERSED", wager.ClassDefinitive},
		wager.FailureReferenceNotFound:         {"REFERENCE_NOT_FOUND", wager.ClassDefinitive},
		wager.FailureReferencePendingExpired:   {"REFERENCE_PENDING_EXPIRED", wager.ClassDefinitive},
		wager.FailureReferenceNotProcessed:     {"REFERENCE_NOT_PROCESSED", wager.ClassDefinitive},
		wager.FailureReferenceMismatch:         {"REFERENCE_MISMATCH", wager.ClassCorrectable},
		wager.FailureAmountMismatch:            {"AMOUNT_MISMATCH", wager.ClassCorrectable},
		wager.FailureInvalidReferenceKind:      {"INVALID_REFERENCE_KIND", wager.ClassCorrectable},
		wager.FailureRetriesExhausted:          {"RETRIES_EXHAUSTED", wager.ClassInfrastructure},
		wager.FailureInternalError:             {"INTERNAL_ERROR", wager.ClassInfrastructure},
	}
	for code, w := range want {
		if string(code) != w.value {
			t.Errorf("code %q changed value (want %q)", code, w.value)
		}
		class, ok := code.Class()
		if !ok || class != w.class {
			t.Errorf("%s: class = %q, %v; want %q", code, class, ok, w.class)
		}
		// cada código serve a exatamente um destino: REJECTED ou FAILED
		if code.IsRejection() == code.IsFailure() {
			t.Errorf("%s must be either a rejection or a failure code", code)
		}
		if (w.class == wager.ClassInfrastructure) != code.IsFailure() {
			t.Errorf("%s: infrastructure class must map to FAILED only", code)
		}
	}
	for _, unknown := range []wager.FailureCode{"", "UNKNOWN", "insufficient_funds"} {
		if _, ok := unknown.Class(); ok || unknown.IsRejection() || unknown.IsFailure() {
			t.Errorf("%q must not be a known failure code", unknown)
		}
	}
}

// Requisito do enunciado: reversão sem saldo e aposta sem saldo usam códigos diferentes.
func TestInsufficientFundsCodeDiffersBetweenBetAndReversal(t *testing.T) {
	bet := wager.KindBet.InsufficientFundsCode()
	if bet != wager.FailureInsufficientFunds {
		t.Errorf("BET code = %s", bet)
	}
	for _, k := range []wager.Kind{wager.KindRefund, wager.KindRollback} {
		if got := k.InsufficientFundsCode(); got != wager.FailureReversalInsufficientFunds || got == bet {
			t.Errorf("%s code = %s", k, got)
		}
	}
}
