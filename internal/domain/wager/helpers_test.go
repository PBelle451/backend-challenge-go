package wager_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/PBelle451/backend-challenge-go/internal/domain/money"
	"github.com/PBelle451/backend-challenge-go/internal/domain/wager"
)

var t0 = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

func uid(n byte) uuid.UUID { return uuid.UUID{15: n} }

func amt(t testing.TB, amount, currency string) money.Money {
	t.Helper()
	m, err := money.Parse(amount, currency)
	if err != nil {
		t.Fatalf("money.Parse(%q, %q): %v", amount, currency, err)
	}
	return m
}

// spec descreve uma transação de teste; campos vazios recebem valores padrão.
type spec struct {
	id       byte
	kind     wager.Kind
	amount   string
	currency string
	provider string
	round    string
	game     string
	player   byte
	wallet   byte
	ref      string // referenceExternalTransactionId
	extID    string
	key      string
}

func (s spec) fill() spec {
	if s.id == 0 {
		s.id = 10
	}
	if s.kind == "" {
		s.kind = wager.KindBet
	}
	if s.amount == "" {
		s.amount = "25.00"
		if s.kind == wager.KindLoss {
			s.amount = "0.00"
		}
	}
	if s.currency == "" {
		s.currency = "BRL"
	}
	if s.provider == "" {
		s.provider = "provider-a"
	}
	if s.round == "" {
		s.round = "round-987"
	}
	if s.game == "" {
		s.game = "fortune-chimp"
	}
	if s.player == 0 {
		s.player = 1
	}
	if s.wallet == 0 {
		s.wallet = 2
	}
	if s.extID == "" {
		s.extID = "tx-" + string(rune('a'+s.id%26)) + string(rune('0'+s.id/26))
	}
	if s.key == "" {
		s.key = s.provider + ":" + s.extID
	}
	return s
}

func (s spec) origin(t testing.TB) wager.Origin {
	t.Helper()
	s = s.fill()
	h, err := wager.ComputePayloadHash(wager.HashInput{
		ProviderID: s.provider, ExternalTransactionID: s.extID,
		PlayerID: uid(s.player), WalletID: uid(s.wallet),
		RoundID: s.round, GameID: s.game, Kind: s.kind,
		Money: amt(t, s.amount, s.currency), ReferenceExternalTransactionID: s.ref,
	})
	if err != nil {
		t.Fatalf("ComputePayloadHash: %v", err)
	}
	return wager.Origin{
		ProviderID: s.provider, ExternalTransactionID: s.extID, IdempotencyKey: s.key,
		PayloadHash: h, RoundID: s.round, GameID: s.game, ReferenceExternalTransactionID: s.ref,
	}
}

func newTx(t testing.TB, s spec) *wager.Transaction {
	t.Helper()
	s = s.fill()
	tx, err := wager.NewExternal(wager.NewExternalParams{
		ID: uid(s.id), Kind: s.kind, WalletID: uid(s.wallet), PlayerID: uid(s.player),
		Amount: amt(t, s.amount, s.currency), Origin: s.origin(t), Now: t0,
	})
	if err != nil {
		t.Fatalf("NewExternal(%+v): %v", s, err)
	}
	return tx
}

// processedTx devolve uma transação PROCESSED (reversões recebem uma referência qualquer).
func processedTx(t testing.TB, s spec) *wager.Transaction {
	t.Helper()
	tx := newTx(t, s)
	p := wager.ProcessParams{ResultBalance: amt(t, "1000.00", tx.Amount().Currency().String()), Now: t0.Add(time.Second)}
	if tx.Kind().RequiresReference() {
		p.ReferenceID = uid(250)
	}
	if err := tx.Process(p); err != nil {
		t.Fatalf("Process: %v", err)
	}
	return tx
}

func statusTx(t testing.TB, s spec, st wager.Status, code wager.FailureCode) *wager.Transaction {
	t.Helper()
	tx := newTx(t, s)
	now := t0.Add(time.Second)
	switch st {
	case wager.StatusPending:
	case wager.StatusPendingReference:
		if err := tx.AwaitReference(now, now.Add(time.Minute), now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	case wager.StatusRejected:
		if err := tx.Reject(code, now); err != nil {
			t.Fatal(err)
		}
	case wager.StatusFailed:
		if err := tx.Fail(code, now); err != nil {
			t.Fatal(err)
		}
	case wager.StatusProcessed:
		return processedTx(t, s)
	}
	return tx
}

func zeroMoney() (m money.Money) { return m }

func negMoney(t testing.TB) money.Money {
	t.Helper()
	m, err := money.New(-100, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}
