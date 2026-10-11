package wager_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/PBelle451/backend-challenge-go/internal/domain/money"
	"github.com/PBelle451/backend-challenge-go/internal/domain/wager"
)

func TestNewExternal_AllFiveKindsStartPending(t *testing.T) {
	for _, s := range []spec{
		{kind: wager.KindBet},
		{kind: wager.KindWin},
		{kind: wager.KindWin, ref: "bet-1"},
		{kind: wager.KindLoss},
		{kind: wager.KindRefund, ref: "bet-1"},
		{kind: wager.KindRollback, ref: "bet-1"},
	} {
		t.Run(string(s.kind)+s.ref, func(t *testing.T) {
			tx := newTx(t, s)
			if tx.Status() != wager.StatusPending || tx.Attempts() != 0 || tx.IsTerminal() {
				t.Errorf("status %s attempts %d", tx.Status(), tx.Attempts())
			}
			if !tx.NextAttemptAt().Equal(tx.CreatedAt()) {
				t.Error("a PENDING transaction must have a durable retry scheduled from the start")
			}
			if _, ok := tx.Origin(); !ok {
				t.Error("external transactions carry their origin")
			}
			if _, ok := tx.ResultBalance(); ok {
				t.Error("a pending transaction has no result yet")
			}
			if _, ok := tx.ReferenceID(); ok || tx.FailureCode() != "" || !tx.FinishedAt().IsZero() {
				t.Error("unexpected resolved reference, failure code or finish time")
			}
		})
	}
}

// Política de valores de cada tipo (enunciado, seção 7).
func TestNewExternal_AmountPolicyPerKind(t *testing.T) {
	type c struct {
		kind   wager.Kind
		amount string
		ok     bool
	}
	cases := []c{
		{wager.KindBet, "0.00", false}, {wager.KindBet, "0.01", true},
		{wager.KindWin, "0.00", false}, {wager.KindWin, "0.01", true},
		{wager.KindRefund, "0.00", false}, {wager.KindRefund, "0.01", true},
		{wager.KindRollback, "0.00", false}, {wager.KindRollback, "0.01", true},
		{wager.KindLoss, "0.00", true}, {wager.KindLoss, "0.01", false},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind)+"/"+tc.amount, func(t *testing.T) {
			s := spec{kind: tc.kind, amount: tc.amount}.fill()
			if tc.kind.RequiresReference() {
				s.ref = "bet-1"
			}
			_, err := wager.NewExternal(wager.NewExternalParams{
				ID: uid(s.id), Kind: tc.kind, WalletID: uid(s.wallet), PlayerID: uid(s.player),
				Amount: amt(t, tc.amount, "BRL"), Origin: s.origin(t), Now: t0,
			})
			if tc.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.ok && !errors.Is(err, wager.ErrInvalidAmount) {
				t.Fatalf("error = %v, want ErrInvalidAmount", err)
			}
		})
	}
}

func TestNewExternal_Invalid(t *testing.T) {
	good := func(t *testing.T) wager.NewExternalParams {
		s := spec{}.fill()
		return wager.NewExternalParams{
			ID: uid(s.id), Kind: wager.KindBet, WalletID: uid(2), PlayerID: uid(1),
			Amount: amt(t, "25.00", "BRL"), Origin: s.origin(t), Now: t0,
		}
	}
	refundWithoutRef := func(t *testing.T) wager.NewExternalParams {
		p := good(t)
		p.Kind = wager.KindRefund
		return p
	}
	tests := []struct {
		name   string
		build  func(t *testing.T) wager.NewExternalParams
		mutate func(p *wager.NewExternalParams)
		want   error
	}{
		{"OPENING e reservado a abertura interna (HTTP/SQS rejeitam)", good, func(p *wager.NewExternalParams) { p.Kind = wager.KindOpening }, wager.ErrOpeningNotAllowed},
		{"tipo desconhecido", good, func(p *wager.NewExternalParams) { p.Kind = "TRANSFER" }, wager.ErrInvalidKind},
		{"tipo vazio", good, func(p *wager.NewExternalParams) { p.Kind = "" }, wager.ErrInvalidKind},
		{"valor negativo", good, func(p *wager.NewExternalParams) { p.Amount = negMoney(t) }, wager.ErrInvalidAmount},
		{"valor nao inicializado", good, func(p *wager.NewExternalParams) { p.Amount = zeroMoney() }, wager.ErrInvalidAmount},
		{"REFUND sem referencia", refundWithoutRef, func(p *wager.NewExternalParams) {}, wager.ErrInvalidReference},
		{"ROLLBACK sem referencia", refundWithoutRef, func(p *wager.NewExternalParams) { p.Kind = wager.KindRollback }, wager.ErrInvalidReference},
		{"BET com referencia", good, func(p *wager.NewExternalParams) { p.Origin.ReferenceExternalTransactionID = "x" }, wager.ErrInvalidReference},
		{"LOSS com referencia", good, func(p *wager.NewExternalParams) {
			p.Kind = wager.KindLoss
			p.Amount = amt(t, "0.00", "BRL")
			p.Origin.ReferenceExternalTransactionID = "x"
		}, wager.ErrInvalidReference},
		{"providerId vazio", good, func(p *wager.NewExternalParams) { p.Origin.ProviderID = "" }, wager.ErrInvalidField},
		{"externalTransactionId vazio", good, func(p *wager.NewExternalParams) { p.Origin.ExternalTransactionID = "" }, wager.ErrInvalidField},
		{"idempotencyKey vazia", good, func(p *wager.NewExternalParams) { p.Origin.IdempotencyKey = "" }, wager.ErrInvalidField},
		{"idempotencyKey longa demais", good, func(p *wager.NewExternalParams) { p.Origin.IdempotencyKey = strings.Repeat("k", 256) }, wager.ErrInvalidField},
		{"roundId vazio", good, func(p *wager.NewExternalParams) { p.Origin.RoundID = "" }, wager.ErrInvalidField},
		{"gameId vazio", good, func(p *wager.NewExternalParams) { p.Origin.GameID = "" }, wager.ErrInvalidField},
		{"hash zerado", good, func(p *wager.NewExternalParams) { p.Origin.PayloadHash = wager.PayloadHash{} }, wager.ErrInvalidField},
		{"id nulo", good, func(p *wager.NewExternalParams) { p.ID = uuid.Nil }, wager.ErrInvalidTransaction},
		{"walletId nulo", good, func(p *wager.NewExternalParams) { p.WalletID = uuid.Nil }, wager.ErrInvalidTransaction},
		{"playerId nulo", good, func(p *wager.NewExternalParams) { p.PlayerID = uuid.Nil }, wager.ErrInvalidTransaction},
		{"sem instante", good, func(p *wager.NewExternalParams) { p.Now = time.Time{} }, wager.ErrInvalidTransaction},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.build(t)
			tc.mutate(&p)
			tx, err := wager.NewExternal(p)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if tx != nil {
				t.Error("an invalid transaction must not be returned")
			}
		})
	}
}

func TestNewOpening(t *testing.T) {
	tx, err := wager.NewOpening(wager.NewOpeningParams{
		ID: uid(7), WalletID: uid(2), PlayerID: uid(1), Amount: amt(t, "1000.00", "BRL"), Now: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if tx.Kind() != wager.KindOpening || tx.Status() != wager.StatusProcessed || !tx.IsTerminal() {
		t.Errorf("kind %s status %s", tx.Kind(), tx.Status())
	}
	if _, ok := tx.Origin(); ok {
		t.Error("OPENING has no external metadata")
	}
	if rb, ok := tx.ResultBalance(); !ok || rb.Amount() != "1000.00" {
		t.Errorf("result balance = %v, %v", rb, ok)
	}
	if !tx.FinishedAt().Equal(t0) || !tx.NextAttemptAt().IsZero() {
		t.Error("an OPENING is born finished, with nothing scheduled")
	}
	if tx.MatchesPayload(wager.PayloadHash{1}) {
		t.Error("OPENING has no payload hash to match")
	}

	for name, p := range map[string]wager.NewOpeningParams{
		"valor zero":    {ID: uid(7), WalletID: uid(2), PlayerID: uid(1), Amount: amt(t, "0.00", "BRL"), Now: t0},
		"valor ausente": {ID: uid(7), WalletID: uid(2), PlayerID: uid(1), Now: t0},
		"id nulo":       {WalletID: uid(2), PlayerID: uid(1), Amount: amt(t, "1.00", "BRL"), Now: t0},
		"sem instante":  {ID: uid(7), WalletID: uid(2), PlayerID: uid(1), Amount: amt(t, "1.00", "BRL")},
	} {
		if got, err := wager.NewOpening(p); err == nil || got != nil {
			t.Errorf("%s: expected an error, got %v, %v", name, got, err)
		}
	}
}

func TestProcess(t *testing.T) {
	later := t0.Add(5 * time.Second)

	tx := newTx(t, spec{kind: wager.KindBet})
	if err := tx.Process(wager.ProcessParams{ResultBalance: amt(t, "975.00", "BRL"), Now: later}); err != nil {
		t.Fatal(err)
	}
	rb, ok := tx.ResultBalance()
	if tx.Status() != wager.StatusProcessed || !ok || rb.Amount() != "975.00" ||
		!tx.FinishedAt().Equal(later) || !tx.UpdatedAt().Equal(later) || !tx.NextAttemptAt().IsZero() {
		t.Errorf("unexpected processed state: %s %v %v", tx.Status(), rb, tx.FinishedAt())
	}

	// reversão processada precisa da referência resolvida
	refund := newTx(t, spec{id: 11, kind: wager.KindRefund, ref: "tx-bet"})
	if err := refund.Process(wager.ProcessParams{ResultBalance: amt(t, "1000.00", "BRL"), Now: later}); !errors.Is(err, wager.ErrInvalidReference) {
		t.Errorf("REFUND without a resolved reference: error = %v", err)
	}
	if err := refund.Process(wager.ProcessParams{ResultBalance: amt(t, "1000.00", "BRL"), ReferenceID: uid(99), Now: later}); err != nil {
		t.Fatal(err)
	}
	if ref, ok := refund.ReferenceID(); !ok || ref != uid(99) {
		t.Errorf("ReferenceID = %v, %v", ref, ok)
	}

	// WIN: referência opcional (com e sem)
	for _, s := range []spec{{id: 12, kind: wager.KindWin}, {id: 13, kind: wager.KindWin, ref: "bet-1"}} {
		w := newTx(t, s)
		p := wager.ProcessParams{ResultBalance: amt(t, "1.00", "BRL"), Now: later}
		if s.ref != "" {
			p.ReferenceID = uid(99)
		}
		if err := w.Process(p); err != nil {
			t.Errorf("WIN %q: %v", s.ref, err)
		}
	}

	// LOSS processa sem movimentação
	loss := newTx(t, spec{id: 14, kind: wager.KindLoss})
	if err := loss.Process(wager.ProcessParams{ResultBalance: amt(t, "500.00", "BRL"), Now: later}); err != nil {
		t.Errorf("LOSS: %v", err)
	}
}

func TestProcess_Invalid(t *testing.T) {
	later := t0.Add(time.Second)
	tests := []struct {
		name string
		s    spec
		p    wager.ProcessParams
		want error
	}{
		{"BET com referencia resolvida", spec{kind: wager.KindBet}, wager.ProcessParams{ResultBalance: amt(t, "1.00", "BRL"), ReferenceID: uid(9), Now: later}, wager.ErrInvalidReference},
		{"saldo nao inicializado", spec{kind: wager.KindBet}, wager.ProcessParams{Now: later}, wager.ErrInvalidTransaction},
		{"saldo negativo", spec{kind: wager.KindBet}, wager.ProcessParams{ResultBalance: negMoney(t), Now: later}, wager.ErrInvalidTransaction},
		{"saldo em outra moeda", spec{kind: wager.KindBet}, wager.ProcessParams{ResultBalance: amt(t, "1.00", "USD"), Now: later}, wager.ErrInvalidTransaction},
		{"sem instante", spec{kind: wager.KindBet}, wager.ProcessParams{ResultBalance: amt(t, "1.00", "BRL")}, wager.ErrInvalidTransition},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tx := newTx(t, tc.s)
			before := *snapshotOf(tx)
			if err := tx.Process(tc.p); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if *snapshotOf(tx) != before {
				t.Error("a failed transition must leave the transaction untouched")
			}
		})
	}
}

type snap struct {
	status   wager.Status
	attempts int
	updated  time.Time
	finished time.Time
	next     time.Time
	expires  time.Time
	code     wager.FailureCode
	result   string
}

func snapshotOf(tx *wager.Transaction) *snap {
	rb, _ := tx.ResultBalance()
	return &snap{tx.Status(), tx.Attempts(), tx.UpdatedAt(), tx.FinishedAt(), tx.NextAttemptAt(), tx.ExpiresAt(), tx.FailureCode(), rb.String()}
}

func TestRejectAndFail(t *testing.T) {
	now := t0.Add(time.Second)

	rejected := newTx(t, spec{kind: wager.KindBet})
	if err := rejected.Reject(wager.FailureInsufficientFunds, now); err != nil {
		t.Fatal(err)
	}
	if rejected.Status() != wager.StatusRejected || rejected.FailureCode() != wager.FailureInsufficientFunds ||
		!rejected.IsTerminal() || !rejected.FinishedAt().Equal(now) || !rejected.NextAttemptAt().IsZero() {
		t.Errorf("unexpected rejected state")
	}
	if _, ok := rejected.ResultBalance(); ok {
		t.Error("a rejected transaction has no result balance")
	}

	failed := newTx(t, spec{kind: wager.KindBet})
	if err := failed.Fail(wager.FailureRetriesExhausted, now); err != nil {
		t.Fatal(err)
	}
	if failed.Status() != wager.StatusFailed || failed.FailureCode() != wager.FailureRetriesExhausted || !failed.IsTerminal() {
		t.Errorf("unexpected failed state")
	}

	// códigos trocados ou desconhecidos são recusados
	for name, op := range map[string]func(tx *wager.Transaction) error{
		"Reject com codigo de falha":     func(tx *wager.Transaction) error { return tx.Reject(wager.FailureInternalError, now) },
		"Reject com codigo desconhecido": func(tx *wager.Transaction) error { return tx.Reject("MADE_UP", now) },
		"Reject sem codigo":              func(tx *wager.Transaction) error { return tx.Reject("", now) },
		"Fail com codigo de rejeicao":    func(tx *wager.Transaction) error { return tx.Fail(wager.FailureInsufficientFunds, now) },
		"Fail sem codigo":                func(tx *wager.Transaction) error { return tx.Fail("", now) },
		"Reject sem instante":            func(tx *wager.Transaction) error { return tx.Reject(wager.FailureInsufficientFunds, time.Time{}) },
	} {
		tx := newTx(t, spec{kind: wager.KindBet})
		before := *snapshotOf(tx)
		if err := op(tx); !errors.Is(err, wager.ErrInvalidTransition) {
			t.Errorf("%s: error = %v, want ErrInvalidTransition", name, err)
		}
		if *snapshotOf(tx) != before {
			t.Errorf("%s: transaction changed", name)
		}
	}
}

func TestAwaitReferenceAndRetry(t *testing.T) {
	now := t0.Add(time.Second)
	next := now.Add(2 * time.Second)
	deadline := now.Add(10 * time.Minute)

	tx := newTx(t, spec{kind: wager.KindRefund, ref: "bet-1"})
	if err := tx.AwaitReference(now, next, deadline); err != nil {
		t.Fatal(err)
	}
	if tx.Status() != wager.StatusPendingReference || !tx.NextAttemptAt().Equal(next) || !tx.ExpiresAt().Equal(deadline) || tx.IsTerminal() {
		t.Errorf("unexpected PENDING_REFERENCE state: %s", tx.Status())
	}

	// novas tentativas contam e reagendam, sem mudar o estado nem o prazo
	for i := 1; i <= 3; i++ {
		n := next.Add(time.Duration(i) * time.Minute)
		if err := tx.Retry(now.Add(time.Duration(i)*time.Minute), n); err != nil {
			t.Fatal(err)
		}
		if tx.Attempts() != i || !tx.NextAttemptAt().Equal(n) || tx.Status() != wager.StatusPendingReference || !tx.ExpiresAt().Equal(deadline) {
			t.Errorf("after retry %d: attempts %d status %s", i, tx.Attempts(), tx.Status())
		}
	}

	// PENDING_REFERENCE resolve para qualquer terminal
	resolved := tx
	if err := resolved.Process(wager.ProcessParams{ResultBalance: amt(t, "10.00", "BRL"), ReferenceID: uid(99), Now: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	for name, code := range map[string]wager.FailureCode{"rejeita": wager.FailureReferenceNotFound} {
		other := statusTx(t, spec{id: 20, kind: wager.KindRefund, ref: "bet-1"}, wager.StatusPendingReference, "")
		if err := other.Reject(code, now.Add(time.Hour)); err != nil {
			t.Errorf("PENDING_REFERENCE %s: %v", name, err)
		}
	}
	other := statusTx(t, spec{id: 21, kind: wager.KindRefund, ref: "bet-1"}, wager.StatusPendingReference, "")
	if err := other.Fail(wager.FailureRetriesExhausted, now.Add(time.Hour)); err != nil {
		t.Errorf("PENDING_REFERENCE fail: %v", err)
	}

	// PENDING simples também aceita Retry (falha transitória, ex.: banco indisponível)
	p := newTx(t, spec{id: 22, kind: wager.KindBet})
	if err := p.Retry(now, now.Add(time.Second)); err != nil || p.Attempts() != 1 || p.Status() != wager.StatusPending {
		t.Errorf("PENDING retry: attempts %d status %s err %v", p.Attempts(), p.Status(), err)
	}
}

func TestAwaitReference_Invalid(t *testing.T) {
	now := t0.Add(time.Second)
	tests := []struct {
		name string
		tx   *wager.Transaction
		op   func(tx *wager.Transaction) error
	}{
		{"BET nao tem referencia para esperar", newTx(t, spec{kind: wager.KindBet}), func(tx *wager.Transaction) error {
			return tx.AwaitReference(now, now.Add(time.Second), now.Add(time.Hour))
		}},
		{"WIN sem referencia", newTx(t, spec{kind: wager.KindWin}), func(tx *wager.Transaction) error {
			return tx.AwaitReference(now, now.Add(time.Second), now.Add(time.Hour))
		}},
		{"prazo no passado", newTx(t, spec{kind: wager.KindRefund, ref: "b"}), func(tx *wager.Transaction) error {
			return tx.AwaitReference(now, now.Add(time.Second), now.Add(-time.Second))
		}},
		{"prazo igual a agora", newTx(t, spec{kind: wager.KindRefund, ref: "b"}), func(tx *wager.Transaction) error {
			return tx.AwaitReference(now, now.Add(time.Second), now)
		}},
		{"sem proxima tentativa", newTx(t, spec{kind: wager.KindRefund, ref: "b"}), func(tx *wager.Transaction) error {
			return tx.AwaitReference(now, time.Time{}, now.Add(time.Hour))
		}},
		{"ja esta em PENDING_REFERENCE", statusTx(t, spec{kind: wager.KindRefund, ref: "b"}, wager.StatusPendingReference, ""), func(tx *wager.Transaction) error {
			return tx.AwaitReference(now, now.Add(time.Second), now.Add(time.Hour))
		}},
		{"Retry sem proxima tentativa", newTx(t, spec{kind: wager.KindBet}), func(tx *wager.Transaction) error {
			return tx.Retry(now, time.Time{})
		}},
		{"Retry sem instante", newTx(t, spec{kind: wager.KindBet}), func(tx *wager.Transaction) error {
			return tx.Retry(time.Time{}, now)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := *snapshotOf(tc.tx)
			if err := tc.op(tc.tx); !errors.Is(err, wager.ErrInvalidTransition) {
				t.Fatalf("error = %v, want ErrInvalidTransition", err)
			}
			if *snapshotOf(tc.tx) != before {
				t.Error("a failed transition must leave the transaction untouched")
			}
		})
	}
}

// Estados terminais não sofrem nenhuma transição (enunciado, seção 6.3).
func TestTerminalStatesAreImmutable(t *testing.T) {
	now := t0.Add(time.Hour)
	terminals := map[string]*wager.Transaction{
		"PROCESSED": processedTx(t, spec{id: 30, kind: wager.KindBet}),
		"REJECTED":  statusTx(t, spec{id: 31, kind: wager.KindBet}, wager.StatusRejected, wager.FailureInsufficientFunds),
		"FAILED":    statusTx(t, spec{id: 32, kind: wager.KindBet}, wager.StatusFailed, wager.FailureInternalError),
	}
	ops := map[string]func(tx *wager.Transaction) error{
		"Process": func(tx *wager.Transaction) error {
			return tx.Process(wager.ProcessParams{ResultBalance: amt(t, "1.00", "BRL"), Now: now})
		},
		"Reject": func(tx *wager.Transaction) error { return tx.Reject(wager.FailureInsufficientFunds, now) },
		"Fail":   func(tx *wager.Transaction) error { return tx.Fail(wager.FailureInternalError, now) },
		"Retry":  func(tx *wager.Transaction) error { return tx.Retry(now, now.Add(time.Second)) },
		"AwaitReference": func(tx *wager.Transaction) error {
			return tx.AwaitReference(now, now.Add(time.Second), now.Add(time.Hour))
		},
	}
	for sname, tx := range terminals {
		for oname, op := range ops {
			t.Run(sname+"/"+oname, func(t *testing.T) {
				before := *snapshotOf(tx)
				if err := op(tx); !errors.Is(err, wager.ErrInvalidTransition) {
					t.Fatalf("error = %v, want ErrInvalidTransition", err)
				}
				if *snapshotOf(tx) != before {
					t.Error("a terminal transaction changed")
				}
			})
		}
	}

	opening, _ := wager.NewOpening(wager.NewOpeningParams{ID: uid(40), WalletID: uid(2), PlayerID: uid(1), Amount: amt(t, "10.00", "BRL"), Now: t0})
	if err := opening.Fail(wager.FailureInternalError, now); !errors.Is(err, wager.ErrInvalidTransition) {
		t.Errorf("OPENING is born terminal: error = %v", err)
	}
}

// Diferença de relógio entre instâncias não pode produzir updated_at < created_at,
// que o schema proíbe.
func TestUpdatedAtNeverGoesBackwards(t *testing.T) {
	tx := newTx(t, spec{kind: wager.KindBet}) // createdAt = t0
	skewed := t0.Add(-3 * time.Second)
	if err := tx.Process(wager.ProcessParams{ResultBalance: amt(t, "1.00", "BRL"), Now: skewed}); err != nil {
		t.Fatal(err)
	}
	if tx.UpdatedAt().Before(tx.CreatedAt()) || tx.FinishedAt().Before(tx.CreatedAt()) {
		t.Errorf("updatedAt %v / finishedAt %v are before createdAt %v", tx.UpdatedAt(), tx.FinishedAt(), tx.CreatedAt())
	}
}

func TestTimesAreUTC(t *testing.T) {
	brt := time.FixedZone("BRT", -3*3600)
	s := spec{kind: wager.KindBet}.fill()
	tx, err := wager.NewExternal(wager.NewExternalParams{
		ID: uid(s.id), Kind: s.kind, WalletID: uid(s.wallet), PlayerID: uid(s.player),
		Amount: amt(t, s.amount, s.currency), Origin: s.origin(t), Now: t0.In(brt),
	})
	if err != nil {
		t.Fatal(err)
	}
	if tx.CreatedAt().Location() != time.UTC || !tx.CreatedAt().Equal(t0) {
		t.Errorf("CreatedAt = %v", tx.CreatedAt())
	}
}

// Conflito de idempotência: mesma chave, conteúdo diferente.
func TestMatchesPayload(t *testing.T) {
	tx := newTx(t, spec{kind: wager.KindBet, amount: "25.00"})
	same := newTx(t, spec{kind: wager.KindBet, amount: "25.00"})
	other := newTx(t, spec{kind: wager.KindBet, amount: "80.00"})

	sameOrigin, _ := same.Origin()
	otherOrigin, _ := other.Origin()
	if !tx.MatchesPayload(sameOrigin.PayloadHash) {
		t.Error("an equivalent payload must match (legitimate replay)")
	}
	if tx.MatchesPayload(otherOrigin.PayloadHash) {
		t.Error("a different payload must not match (idempotency conflict)")
	}
}

func TestRehydrateRoundTrip(t *testing.T) {
	cases := map[string]*wager.Transaction{
		"PENDING":           newTx(t, spec{id: 50, kind: wager.KindBet}),
		"PENDING_REFERENCE": statusTx(t, spec{id: 51, kind: wager.KindRollback, ref: "bet-1"}, wager.StatusPendingReference, ""),
		"PROCESSED BET":     processedTx(t, spec{id: 52, kind: wager.KindBet}),
		"PROCESSED REFUND":  processedTx(t, spec{id: 53, kind: wager.KindRefund, ref: "bet-1"}),
		"PROCESSED LOSS":    processedTx(t, spec{id: 54, kind: wager.KindLoss}),
		"REJECTED":          statusTx(t, spec{id: 55, kind: wager.KindBet}, wager.StatusRejected, wager.FailureInsufficientFunds),
		"FAILED":            statusTx(t, spec{id: 56, kind: wager.KindBet}, wager.StatusFailed, wager.FailureRetriesExhausted),
	}
	opening, _ := wager.NewOpening(wager.NewOpeningParams{ID: uid(57), WalletID: uid(2), PlayerID: uid(1), Amount: amt(t, "10.00", "BRL"), Now: t0})
	cases["OPENING"] = opening

	for name, orig := range cases {
		t.Run(name, func(t *testing.T) {
			origin, hasOrigin := orig.Origin()
			var op *wager.Origin
			if hasOrigin {
				op = &origin
			}
			ref, _ := orig.ReferenceID()
			rb, _ := orig.ResultBalance()
			got, err := wager.Rehydrate(wager.RehydrateParams{
				ID: orig.ID(), Kind: orig.Kind(), Status: orig.Status(), WalletID: orig.WalletID(), PlayerID: orig.PlayerID(),
				Amount: orig.Amount(), Origin: op, ReferenceID: ref, FailureCode: orig.FailureCode(), ResultBalance: rb,
				Attempts: orig.Attempts(), NextAttemptAt: orig.NextAttemptAt(), ExpiresAt: orig.ExpiresAt(),
				CreatedAt: orig.CreatedAt(), UpdatedAt: orig.UpdatedAt(), FinishedAt: orig.FinishedAt(),
			})
			if err != nil {
				t.Fatalf("Rehydrate: %v", err)
			}
			if *snapshotOf(got) != *snapshotOf(orig) || got.Kind() != orig.Kind() || got.ID() != orig.ID() ||
				!got.Amount().Equal(orig.Amount()) || got.IsTerminal() != orig.IsTerminal() {
				t.Errorf("rehydrated transaction differs from the original")
			}
		})
	}
}

func TestRehydrate_RejectsInconsistentState(t *testing.T) {
	// parte de uma linha PROCESSED válida e estraga um campo por vez
	proc := processedTx(t, spec{id: 60, kind: wager.KindBet})
	origin, _ := proc.Origin()
	rb, _ := proc.ResultBalance()
	good := func() wager.RehydrateParams {
		o := origin
		return wager.RehydrateParams{
			ID: proc.ID(), Kind: proc.Kind(), Status: proc.Status(), WalletID: proc.WalletID(), PlayerID: proc.PlayerID(),
			Amount: proc.Amount(), Origin: &o, ResultBalance: rb,
			CreatedAt: proc.CreatedAt(), UpdatedAt: proc.UpdatedAt(), FinishedAt: proc.FinishedAt(),
		}
	}
	if _, err := wager.Rehydrate(good()); err != nil {
		t.Fatalf("baseline must be valid: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(p *wager.RehydrateParams)
	}{
		{"PROCESSED sem saldo de resultado", func(p *wager.RehydrateParams) { p.ResultBalance = money.Money{} }},
		{"PROCESSED sem finishedAt", func(p *wager.RehydrateParams) { p.FinishedAt = time.Time{} }},
		{"PROCESSED com proxima tentativa", func(p *wager.RehydrateParams) { p.NextAttemptAt = t0 }},
		{"PROCESSED com failureCode", func(p *wager.RehydrateParams) { p.FailureCode = wager.FailureInsufficientFunds }},
		{"BET com referencia interna", func(p *wager.RehydrateParams) { p.ReferenceID = uid(9) }},
		{"REJECTED sem failureCode", func(p *wager.RehydrateParams) {
			p.Status, p.ResultBalance = wager.StatusRejected, money.Money{}
		}},
		{"REJECTED com codigo de falha", func(p *wager.RehydrateParams) {
			p.Status, p.ResultBalance, p.FailureCode = wager.StatusRejected, money.Money{}, wager.FailureInternalError
		}},
		{"FAILED com codigo de rejeicao", func(p *wager.RehydrateParams) {
			p.Status, p.ResultBalance, p.FailureCode = wager.StatusFailed, money.Money{}, wager.FailureInsufficientFunds
		}},
		{"PENDING sem proxima tentativa", func(p *wager.RehydrateParams) {
			p.Status, p.ResultBalance, p.FinishedAt = wager.StatusPending, money.Money{}, time.Time{}
		}},
		{"PENDING com resultado", func(p *wager.RehydrateParams) {
			p.Status, p.FinishedAt, p.NextAttemptAt = wager.StatusPending, time.Time{}, t0
		}},
		{"PENDING_REFERENCE sem prazo", func(p *wager.RehydrateParams) {
			p.Status, p.ResultBalance, p.FinishedAt, p.NextAttemptAt = wager.StatusPendingReference, money.Money{}, time.Time{}, t0
		}},
		{"status desconhecido", func(p *wager.RehydrateParams) { p.Status = "DONE" }},
		{"tentativas negativas", func(p *wager.RehydrateParams) { p.Attempts = -1 }},
		{"updatedAt anterior a createdAt", func(p *wager.RehydrateParams) { p.UpdatedAt = p.CreatedAt.Add(-time.Second) }},
		{"externa sem origem", func(p *wager.RehydrateParams) { p.Origin = nil }},
		{"valor zero em BET", func(p *wager.RehydrateParams) { p.Amount = amt(t, "0.00", "BRL") }},
		{"saldo de resultado em outra moeda", func(p *wager.RehydrateParams) { p.ResultBalance = amt(t, "1.00", "USD") }},
		{"tipo desconhecido", func(p *wager.RehydrateParams) { p.Kind = "TRANSFER" }},
		{"id nulo", func(p *wager.RehydrateParams) { p.ID = uuid.Nil }},
		{"OPENING com origem externa", func(p *wager.RehydrateParams) { p.Kind = wager.KindOpening }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := good()
			tc.mutate(&p)
			if got, err := wager.Rehydrate(p); !errors.Is(err, wager.ErrInvalidTransaction) || got != nil {
				t.Errorf("Rehydrate = %v, %v; want ErrInvalidTransaction", got, err)
			}
		})
	}

	// OPENING pendente é inválido
	op := wager.RehydrateParams{
		ID: uid(70), Kind: wager.KindOpening, Status: wager.StatusPending, WalletID: uid(2), PlayerID: uid(1),
		Amount: amt(t, "1.00", "BRL"), NextAttemptAt: t0, CreatedAt: t0, UpdatedAt: t0,
	}
	if _, err := wager.Rehydrate(op); !errors.Is(err, wager.ErrInvalidTransaction) {
		t.Errorf("a pending OPENING must be rejected: %v", err)
	}
}
