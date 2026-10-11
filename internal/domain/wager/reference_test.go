package wager_test

import (
	"errors"
	"testing"
	"time"

	"github.com/PBelle451/backend-challenge-go/internal/domain/wager"
	"github.com/PBelle451/backend-challenge-go/internal/domain/wallet"
)

// Referência processada padrão: uma BET de 25.00 do provider-a, jogador 1, carteira 2, rodada round-987.
func processedBet(t *testing.T, mod func(s *spec)) *wager.Transaction {
	s := spec{id: 80, kind: wager.KindBet, extID: "bet-1"}
	if mod != nil {
		mod(&s)
	}
	return processedTx(t, s)
}

// Operação que referencia "bet-1" e concorda com processedBet em tudo.
func opRef(t *testing.T, kind wager.Kind, mod func(s *spec)) *wager.Transaction {
	s := spec{id: 81, kind: kind, ref: "bet-1", extID: "op-1"}
	if mod != nil {
		mod(&s)
	}
	return newTx(t, s)
}

func decide(t *testing.T, op, ref *wager.Transaction, already bool) wager.ReferenceDecision {
	t.Helper()
	d, err := op.EvaluateReference(ref, already)
	if err != nil {
		t.Fatalf("EvaluateReference: %v", err)
	}
	return d
}

func TestEvaluateReference_Apply(t *testing.T) {
	bet := processedBet(t, nil)
	win := processedTx(t, spec{id: 82, kind: wager.KindWin, extID: "win-1", amount: "25.00"})
	refund := processedTx(t, spec{id: 83, kind: wager.KindRefund, extID: "refund-1", ref: "bet-1", amount: "25.00"})

	tests := []struct {
		name string
		op   *wager.Transaction
		ref  *wager.Transaction
		dir  wallet.Direction
	}{
		{"REFUND de uma BET credita", opRef(t, wager.KindRefund, nil), bet, wallet.DirectionCredit},
		{"ROLLBACK de uma BET devolve o debito (credita)", opRef(t, wager.KindRollback, nil), bet, wallet.DirectionCredit},
		{"ROLLBACK de um WIN desfaz o credito (debita)", opRef(t, wager.KindRollback, func(s *spec) { s.ref = "win-1" }), win, wallet.DirectionDebit},
		{"ROLLBACK de um REFUND desfaz o credito (debita)", opRef(t, wager.KindRollback, func(s *spec) { s.ref = "refund-1" }), refund, wallet.DirectionDebit},
		{"WIN com referencia a uma BET credita", opRef(t, wager.KindWin, func(s *spec) { s.amount = "80.00" }), bet, wallet.DirectionCredit},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := decide(t, tc.op, tc.ref, false)
			if d.Outcome != wager.OutcomeApply || d.Direction != tc.dir || d.ReferenceID != tc.ref.ID() || d.Failure != "" {
				t.Errorf("decision = %+v", d)
			}
		})
	}
}

func TestEvaluateReference_WaitWhenReferenceIsMissingOrPending(t *testing.T) {
	op := opRef(t, wager.KindRefund, nil)
	pendingRef := newTx(t, spec{id: 80, kind: wager.KindBet, extID: "bet-1"})
	awaiting := statusTx(t, spec{id: 84, kind: wager.KindRefund, extID: "bet-1", ref: "other"}, wager.StatusPendingReference, "")

	for name, ref := range map[string]*wager.Transaction{
		"referencia ainda nao chegou":              nil,
		"referencia existe, mas PENDING":           pendingRef,
		"referencia existe, mas PENDING_REFERENCE": awaiting,
	} {
		d := decide(t, op, ref, false)
		if d.Outcome != wager.OutcomeWait || d.Failure != "" || d.Direction != "" {
			t.Errorf("%s: decision = %+v, want WAIT", name, d)
		}
	}
}

func TestEvaluateReference_RejectsReferenceThatEndedWithoutSuccess(t *testing.T) {
	op := opRef(t, wager.KindRollback, nil)
	for name, ref := range map[string]*wager.Transaction{
		"REJECTED": statusTx(t, spec{id: 80, kind: wager.KindBet, extID: "bet-1"}, wager.StatusRejected, wager.FailureInsufficientFunds),
		"FAILED":   statusTx(t, spec{id: 80, kind: wager.KindBet, extID: "bet-1"}, wager.StatusFailed, wager.FailureInternalError),
	} {
		d := decide(t, op, ref, false)
		if d.Outcome != wager.OutcomeReject || d.Failure != wager.FailureReferenceNotProcessed {
			t.Errorf("%s: decision = %+v", name, d)
		}
	}
}

func TestEvaluateReference_Rejections(t *testing.T) {
	bet := processedBet(t, nil)
	win := processedTx(t, spec{id: 82, kind: wager.KindWin, extID: "win-1"})
	loss := processedTx(t, spec{id: 85, kind: wager.KindLoss, extID: "loss-1"})
	rollback := processedTx(t, spec{id: 86, kind: wager.KindRollback, extID: "rb-1", ref: "bet-1"})
	opening, _ := wager.NewOpening(wager.NewOpeningParams{ID: uid(87), WalletID: uid(2), PlayerID: uid(1), Amount: amt(t, "10.00", "BRL"), Now: t0})

	tests := []struct {
		name    string
		op      *wager.Transaction
		ref     *wager.Transaction
		already bool
		want    wager.FailureCode
	}{
		// tipo da referência
		{"REFUND de um WIN", opRef(t, wager.KindRefund, func(s *spec) { s.ref = "win-1" }), win, false, wager.FailureInvalidReferenceKind},
		{"REFUND de um LOSS", opRef(t, wager.KindRefund, func(s *spec) { s.ref = "loss-1" }), loss, false, wager.FailureInvalidReferenceKind},
		{"ROLLBACK de um ROLLBACK", opRef(t, wager.KindRollback, func(s *spec) { s.ref = "rb-1" }), rollback, false, wager.FailureInvalidReferenceKind},
		{"ROLLBACK de um LOSS", opRef(t, wager.KindRollback, func(s *spec) { s.ref = "loss-1" }), loss, false, wager.FailureInvalidReferenceKind},
		{"ROLLBACK de um OPENING", opRef(t, wager.KindRollback, nil), opening, false, wager.FailureInvalidReferenceKind},
		{"WIN referenciando outro WIN", opRef(t, wager.KindWin, func(s *spec) { s.ref = "win-1" }), win, false, wager.FailureInvalidReferenceKind},
		{"operacao referenciando a si mesma", opRef(t, wager.KindRefund, func(s *spec) { s.id = 80 }), bet, false, wager.FailureInvalidReferenceKind},
		// coerência
		{"outro provedor", opRef(t, wager.KindRefund, func(s *spec) { s.provider = "provider-b" }), bet, false, wager.FailureReferenceMismatch},
		{"outro jogador", opRef(t, wager.KindRefund, func(s *spec) { s.player = 9 }), bet, false, wager.FailureReferenceMismatch},
		{"outra carteira", opRef(t, wager.KindRefund, func(s *spec) { s.wallet = 9 }), bet, false, wager.FailureReferenceMismatch},
		{"outra moeda", opRef(t, wager.KindRefund, func(s *spec) { s.currency = "USD" }), bet, false, wager.FailureReferenceMismatch},
		{"outra rodada", opRef(t, wager.KindRefund, func(s *spec) { s.round = "round-1" }), bet, false, wager.FailureReferenceMismatch},
		{"WIN de outra rodada", opRef(t, wager.KindWin, func(s *spec) { s.round = "round-1" }), bet, false, wager.FailureReferenceMismatch},
		// valor: reversões exigem valor integral
		{"REFUND parcial", opRef(t, wager.KindRefund, func(s *spec) { s.amount = "10.00" }), bet, false, wager.FailureAmountMismatch},
		{"REFUND maior que a aposta", opRef(t, wager.KindRefund, func(s *spec) { s.amount = "25.01" }), bet, false, wager.FailureAmountMismatch},
		{"ROLLBACK parcial", opRef(t, wager.KindRollback, func(s *spec) { s.amount = "10.00" }), bet, false, wager.FailureAmountMismatch},
		// reversão duplicada
		{"REFUND de BET ja revertida", opRef(t, wager.KindRefund, nil), bet, true, wager.FailureAlreadyReversed},
		{"ROLLBACK de BET ja revertida", opRef(t, wager.KindRollback, nil), bet, true, wager.FailureAlreadyReversed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := decide(t, tc.op, tc.ref, tc.already)
			if d.Outcome != wager.OutcomeReject || d.Failure != tc.want {
				t.Errorf("decision = %+v, want REJECT %s", d, tc.want)
			}
			if !d.Failure.IsRejection() {
				t.Errorf("%s must be a valid rejection code", d.Failure)
			}
		})
	}
}

// WIN pode pagar mais que a aposta e não é reversão: valor diferente e
// "alreadyReversed" não se aplicam a ele.
func TestEvaluateReference_WinIgnoresAmountAndReversalChecks(t *testing.T) {
	bet := processedBet(t, nil)
	win := opRef(t, wager.KindWin, func(s *spec) { s.amount = "500.00" })
	d := decide(t, win, bet, true)
	if d.Outcome != wager.OutcomeApply || d.Direction != wallet.DirectionCredit {
		t.Errorf("decision = %+v, want APPLY credit", d)
	}
}

// Ordem das verificações documentada: tipo > coerência > valor > reversão duplicada.
func TestEvaluateReference_CheckOrder(t *testing.T) {
	bet := processedBet(t, nil)
	win := processedTx(t, spec{id: 82, kind: wager.KindWin, extID: "win-1"})

	// tipo inválido E provedor diferente: vale o tipo
	op := opRef(t, wager.KindRefund, func(s *spec) { s.ref = "win-1"; s.provider = "provider-b" })
	if d := decide(t, op, win, false); d.Failure != wager.FailureInvalidReferenceKind {
		t.Errorf("kind must win over mismatch: %+v", d)
	}
	// incoerente E valor diferente: vale a coerência
	op = opRef(t, wager.KindRefund, func(s *spec) { s.round = "other"; s.amount = "1.00" })
	if d := decide(t, op, bet, false); d.Failure != wager.FailureReferenceMismatch {
		t.Errorf("mismatch must win over amount: %+v", d)
	}
	// valor diferente E já revertida: vale o valor
	op = opRef(t, wager.KindRefund, func(s *spec) { s.amount = "1.00" })
	if d := decide(t, op, bet, true); d.Failure != wager.FailureAmountMismatch {
		t.Errorf("amount must win over already-reversed: %+v", d)
	}
}

func TestEvaluateReference_Misuse(t *testing.T) {
	bet := processedBet(t, nil)

	for name, op := range map[string]*wager.Transaction{
		"BET nao tem referencia":       newTx(t, spec{id: 90, kind: wager.KindBet}),
		"LOSS nao tem referencia":      newTx(t, spec{id: 91, kind: wager.KindLoss}),
		"WIN sem referencia informada": newTx(t, spec{id: 92, kind: wager.KindWin}),
	} {
		if _, err := op.EvaluateReference(bet, false); !errors.Is(err, wager.ErrInvalidReference) {
			t.Errorf("%s: error = %v, want ErrInvalidReference", name, err)
		}
	}

	terminal := statusTx(t, spec{id: 93, kind: wager.KindRefund, ref: "bet-1"}, wager.StatusRejected, wager.FailureReferenceNotFound)
	if _, err := terminal.EvaluateReference(bet, false); !errors.Is(err, wager.ErrInvalidTransition) {
		t.Errorf("terminal transaction: error = %v, want ErrInvalidTransition", err)
	}
}

// Cenário completo da espera por referência: a reversão chega antes da aposta,
// espera, tenta de novo com backoff e acaba aplicada quando a aposta é processada.
func TestReferenceWaitingLifecycle(t *testing.T) {
	now := t0
	refund := opRef(t, wager.KindRefund, nil)
	deadline := now.Add(10 * time.Minute)
	const maxAttempts = 5

	// 1) a referência ainda não chegou
	if d := decide(t, refund, nil, false); d.Outcome != wager.OutcomeWait {
		t.Fatalf("decision = %+v", d)
	}
	if err := refund.AwaitReference(now, now.Add(wager.Backoff(1, time.Second, time.Minute)), deadline); err != nil {
		t.Fatal(err)
	}
	if wager.ReferenceExhausted(refund.Attempts(), maxAttempts, now, deadline) {
		t.Fatal("must not be exhausted right after waiting starts")
	}

	// 2) uma tentativa depois, a referência chegou e foi processada
	now = now.Add(2 * time.Second)
	if err := refund.Retry(now, now.Add(wager.Backoff(2, time.Second, time.Minute))); err != nil {
		t.Fatal(err)
	}
	bet := processedBet(t, nil)
	d := decide(t, refund, bet, false)
	if d.Outcome != wager.OutcomeApply {
		t.Fatalf("decision = %+v", d)
	}
	if err := refund.Process(wager.ProcessParams{ResultBalance: amt(t, "1025.00", "BRL"), ReferenceID: d.ReferenceID, Now: now}); err != nil {
		t.Fatal(err)
	}
	if ref, _ := refund.ReferenceID(); ref != bet.ID() || refund.Status() != wager.StatusProcessed {
		t.Errorf("status %s reference %v", refund.Status(), ref)
	}

	// 3) outro caminho: a referência nunca chega e a espera expira
	late := opRef(t, wager.KindRollback, func(s *spec) { s.id = 95; s.extID = "op-2" })
	if err := late.AwaitReference(t0, t0.Add(time.Second), deadline); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxAttempts; i++ {
		if err := late.Retry(t0, t0.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if !wager.ReferenceExhausted(late.Attempts(), maxAttempts, t0, deadline) {
		t.Fatal("the wait must be exhausted after the maximum attempts")
	}
	if err := late.Reject(wager.FailureReferenceNotFound, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if late.Status() != wager.StatusRejected || late.FailureCode() != wager.FailureReferenceNotFound {
		t.Errorf("status %s code %s", late.Status(), late.FailureCode())
	}
}
