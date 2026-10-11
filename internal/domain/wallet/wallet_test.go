package wallet_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/PBelle451/backend-challenge-go/internal/domain/money"
	"github.com/PBelle451/backend-challenge-go/internal/domain/wallet"
)

var t0 = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

func uid(n byte) uuid.UUID { return uuid.UUID{15: n} }

func brl(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.Parse(amount, "BRL")
	if err != nil {
		t.Fatalf("Parse(%q): %v", amount, err)
	}
	return m
}

func minor(t *testing.T, v int64, cur string) money.Money {
	t.Helper()
	m, err := money.New(v, money.Currency(cur))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func openWallet(t *testing.T, initial string) *wallet.Wallet {
	t.Helper()
	w, _, err := wallet.Open(wallet.OpenParams{
		WalletID: uid(1), PlayerID: uid(2), InitialBalance: brl(t, initial),
		OpeningTransactionID: uid(3), OpeningEntryID: uid(4), Now: t0,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return w
}

func mv(t *testing.T, n byte, amount string) wallet.MovementParams {
	return wallet.MovementParams{EntryID: uid(100 + n), TransactionID: uid(150 + n), Amount: brl(t, amount), Now: t0.Add(time.Duration(n) * time.Second)}
}

// snapshot permite provar que uma operação que falha não altera a carteira.
type snapshot struct {
	balance   string
	version   int64
	updatedAt time.Time
}

func snap(w *wallet.Wallet) snapshot {
	return snapshot{w.Balance().String(), w.Version(), w.UpdatedAt()}
}

func TestOpen_PositiveBalanceCreatesOpeningEntry(t *testing.T) {
	w, entry, err := wallet.Open(wallet.OpenParams{
		WalletID: uid(1), PlayerID: uid(2), InitialBalance: brl(t, "1000.00"),
		OpeningTransactionID: uid(3), OpeningEntryID: uid(4), Now: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if w.Version() != 1 || w.Balance().Amount() != "1000.00" || w.Currency() != "BRL" {
		t.Errorf("wallet = version %d balance %v", w.Version(), w.Balance())
	}
	if w.LoadedVersion() != 0 {
		t.Errorf("a freshly opened wallet is not persisted yet: LoadedVersion = %d", w.LoadedVersion())
	}
	if entry == nil {
		t.Fatal("a positive initial balance must produce an opening ledger entry")
	}
	if entry.Direction() != wallet.DirectionCredit || entry.Amount().Amount() != "1000.00" ||
		entry.BalanceBefore().Amount() != "0.00" || entry.BalanceAfter().Amount() != "1000.00" ||
		entry.WalletVersion() != 1 || entry.TransactionID() != uid(3) || entry.WalletID() != uid(1) {
		t.Errorf("unexpected opening entry: %+v", entry)
	}
}

func TestOpen_ZeroBalanceHasNoEntry(t *testing.T) {
	w, entry, err := wallet.Open(wallet.OpenParams{WalletID: uid(1), PlayerID: uid(2), InitialBalance: brl(t, "0.00"), Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	if entry != nil {
		t.Errorf("zero initial balance must not create a ledger entry, got %+v", entry)
	}
	if w.Version() != 1 || !w.Balance().IsZero() {
		t.Errorf("wallet = version %d balance %v", w.Version(), w.Balance())
	}
}

func TestOpen_Invalid(t *testing.T) {
	ok := wallet.OpenParams{
		WalletID: uid(1), PlayerID: uid(2), InitialBalance: brl(t, "10.00"),
		OpeningTransactionID: uid(3), OpeningEntryID: uid(4), Now: t0,
	}
	tests := []struct {
		name   string
		mutate func(p *wallet.OpenParams)
		want   error
	}{
		{"walletId nulo", func(p *wallet.OpenParams) { p.WalletID = uuid.Nil }, wallet.ErrInvalidWallet},
		{"playerId nulo", func(p *wallet.OpenParams) { p.PlayerID = uuid.Nil }, wallet.ErrInvalidWallet},
		{"saldo nao inicializado", func(p *wallet.OpenParams) { p.InitialBalance = money.Money{} }, wallet.ErrInvalidWallet},
		{"saldo negativo", func(p *wallet.OpenParams) { p.InitialBalance = minor(t, -1, "BRL") }, wallet.ErrInvalidWallet},
		{"sem instante", func(p *wallet.OpenParams) { p.Now = time.Time{} }, wallet.ErrInvalidWallet},
		{"saldo positivo sem id da transacao de abertura", func(p *wallet.OpenParams) { p.OpeningTransactionID = uuid.Nil }, wallet.ErrInvalidLedgerEntry},
		{"saldo positivo sem id do lancamento", func(p *wallet.OpenParams) { p.OpeningEntryID = uuid.Nil }, wallet.ErrInvalidLedgerEntry},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := ok
			tc.mutate(&p)
			w, e, err := wallet.Open(p)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if w != nil || e != nil {
				t.Error("a failed Open must not return a wallet or an entry")
			}
		})
	}
}

func TestDebitAndCredit(t *testing.T) {
	w := openWallet(t, "100.00")

	e1, err := w.Debit(mv(t, 1, "25.00"))
	if err != nil {
		t.Fatal(err)
	}
	if w.Balance().Amount() != "75.00" || w.Version() != 2 {
		t.Errorf("after debit: balance %v version %d", w.Balance(), w.Version())
	}
	if e1.Direction() != wallet.DirectionDebit || e1.BalanceBefore().Amount() != "100.00" ||
		e1.BalanceAfter().Amount() != "75.00" || e1.WalletVersion() != 2 ||
		e1.TransactionID() != uid(151) || e1.ID() != uid(101) || e1.WalletID() != w.ID() {
		t.Errorf("unexpected debit entry: %+v", e1)
	}

	e2, err := w.Credit(mv(t, 2, "10.50"))
	if err != nil {
		t.Fatal(err)
	}
	if w.Balance().Amount() != "85.50" || w.Version() != 3 {
		t.Errorf("after credit: balance %v version %d", w.Balance(), w.Version())
	}
	if e2.Direction() != wallet.DirectionCredit || e2.BalanceBefore().Amount() != "75.00" || e2.WalletVersion() != 3 {
		t.Errorf("unexpected credit entry: %+v", e2)
	}
	if !w.UpdatedAt().Equal(t0.Add(2 * time.Second)) {
		t.Errorf("UpdatedAt = %v", w.UpdatedAt())
	}
}

func TestDebit_ExactBalanceLeavesZero(t *testing.T) {
	w := openWallet(t, "80.00")
	if _, err := w.Debit(mv(t, 1, "80.00")); err != nil {
		t.Fatal(err)
	}
	if !w.Balance().IsZero() || w.Version() != 2 {
		t.Errorf("balance %v version %d", w.Balance(), w.Version())
	}
}

// Cenário do enunciado: 100.00 recebendo duas apostas de 80.00. No agregado,
// sequencialmente, uma passa e a outra é recusada por saldo insuficiente.
func TestTwoBetsOf80OnBalanceOf100(t *testing.T) {
	w := openWallet(t, "100.00")

	if _, err := w.Debit(mv(t, 1, "80.00")); err != nil {
		t.Fatalf("first bet: %v", err)
	}
	before := snap(w)
	_, err := w.Debit(mv(t, 2, "80.00"))
	if !errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Fatalf("second bet error = %v, want ErrInsufficientFunds", err)
	}
	if snap(w) != before {
		t.Error("a rejected debit must not change the wallet")
	}
	if w.Balance().Amount() != "20.00" {
		t.Errorf("final balance = %v, want 20.00", w.Balance())
	}
}

func TestMovement_FailuresDoNotMutate(t *testing.T) {
	max, _ := money.New(math.MaxInt64, "BRL")
	maxWallet, _, err := wallet.Open(wallet.OpenParams{
		WalletID: uid(1), PlayerID: uid(2), InitialBalance: max,
		OpeningTransactionID: uid(3), OpeningEntryID: uid(4), Now: t0,
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		w    *wallet.Wallet
		op   func(w *wallet.Wallet) (wallet.LedgerEntry, error)
		want error
	}{
		{"debito maior que o saldo", openWallet(t, "10.00"), func(w *wallet.Wallet) (wallet.LedgerEntry, error) {
			return w.Debit(mv(t, 1, "10.01"))
		}, wallet.ErrInsufficientFunds},
		{"moeda diferente no debito", openWallet(t, "10.00"), func(w *wallet.Wallet) (wallet.LedgerEntry, error) {
			p := mv(t, 1, "1.00")
			p.Amount = minor(t, 100, "USD")
			return w.Debit(p)
		}, money.ErrCurrencyMismatch},
		{"moeda diferente no credito", openWallet(t, "10.00"), func(w *wallet.Wallet) (wallet.LedgerEntry, error) {
			p := mv(t, 1, "1.00")
			p.Amount = minor(t, 100, "USD")
			return w.Credit(p)
		}, money.ErrCurrencyMismatch},
		{"valor zero", openWallet(t, "10.00"), func(w *wallet.Wallet) (wallet.LedgerEntry, error) {
			return w.Debit(mv(t, 1, "0.00"))
		}, wallet.ErrNonPositiveAmount},
		{"valor negativo", openWallet(t, "10.00"), func(w *wallet.Wallet) (wallet.LedgerEntry, error) {
			p := mv(t, 1, "1.00")
			p.Amount = minor(t, -100, "BRL")
			return w.Credit(p)
		}, wallet.ErrNonPositiveAmount},
		{"valor nao inicializado", openWallet(t, "10.00"), func(w *wallet.Wallet) (wallet.LedgerEntry, error) {
			p := mv(t, 1, "1.00")
			p.Amount = money.Money{}
			return w.Credit(p)
		}, wallet.ErrInvalidMovement},
		{"id do lancamento nulo", openWallet(t, "10.00"), func(w *wallet.Wallet) (wallet.LedgerEntry, error) {
			p := mv(t, 1, "1.00")
			p.EntryID = uuid.Nil
			return w.Debit(p)
		}, wallet.ErrInvalidMovement},
		{"id da transacao nulo", openWallet(t, "10.00"), func(w *wallet.Wallet) (wallet.LedgerEntry, error) {
			p := mv(t, 1, "1.00")
			p.TransactionID = uuid.Nil
			return w.Credit(p)
		}, wallet.ErrInvalidMovement},
		{"sem instante", openWallet(t, "10.00"), func(w *wallet.Wallet) (wallet.LedgerEntry, error) {
			p := mv(t, 1, "1.00")
			p.Now = time.Time{}
			return w.Debit(p)
		}, wallet.ErrInvalidMovement},
		{"overflow no credito", maxWallet, func(w *wallet.Wallet) (wallet.LedgerEntry, error) {
			return w.Credit(mv(t, 1, "0.01"))
		}, money.ErrOverflow},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := snap(tc.w)
			entry, err := tc.op(tc.w)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if entry != (wallet.LedgerEntry{}) {
				t.Error("a failed movement must not return a ledger entry")
			}
			if snap(tc.w) != before {
				t.Errorf("wallet changed after a failed movement: %+v -> %+v", before, snap(tc.w))
			}
		})
	}
}

// Propriedade: sequência de operações produz uma cadeia contínua de lançamentos e
// o saldo final é a soma de créditos menos débitos.
func TestLedgerChainAndFinalBalance(t *testing.T) {
	w := openWallet(t, "500.00")
	ops := []struct {
		debit  bool
		amount string
	}{
		{true, "120.00"}, {false, "30.25"}, {true, "10.00"}, {true, "400.25"},
		{false, "0.01"}, {true, "999.99"} /* recusado */, {false, "100.00"}, {true, "0.02"},
	}

	prevAfter := w.Balance()
	prevVersion := w.Version()
	var credits, debits int64
	var n byte
	for _, op := range ops {
		n++
		var entry wallet.LedgerEntry
		var err error
		if op.debit {
			entry, err = w.Debit(mv(t, n, op.amount))
		} else {
			entry, err = w.Credit(mv(t, n, op.amount))
		}
		if err != nil {
			if !errors.Is(err, wallet.ErrInsufficientFunds) {
				t.Fatalf("op %d: unexpected error %v", n, err)
			}
			continue
		}
		if !entry.BalanceBefore().Equal(prevAfter) {
			t.Errorf("op %d: chain broken: before %v, previous after %v", n, entry.BalanceBefore(), prevAfter)
		}
		if entry.WalletVersion() != prevVersion+1 {
			t.Errorf("op %d: version %d, want %d", n, entry.WalletVersion(), prevVersion+1)
		}
		prevAfter, prevVersion = entry.BalanceAfter(), entry.WalletVersion()
		if op.debit {
			debits += entry.Amount().Minor()
		} else {
			credits += entry.Amount().Minor()
		}
		if w.Balance().IsNegative() {
			t.Fatalf("op %d: negative balance %v", n, w.Balance())
		}
	}
	want := int64(50000) + credits - debits
	if w.Balance().Minor() != want {
		t.Errorf("final balance %d, want %d (opening + credits - debits)", w.Balance().Minor(), want)
	}
	if w.Version() != prevVersion {
		t.Errorf("wallet version %d, last entry version %d", w.Version(), prevVersion)
	}
}

func TestRehydrate(t *testing.T) {
	created := t0
	updated := t0.Add(time.Hour)
	w, err := wallet.Rehydrate(wallet.RehydrateParams{
		ID: uid(1), PlayerID: uid(2), Balance: brl(t, "75.00"), Version: 7, CreatedAt: created, UpdatedAt: updated,
	})
	if err != nil {
		t.Fatal(err)
	}
	if w.Version() != 7 || w.LoadedVersion() != 7 || w.Balance().Amount() != "75.00" ||
		!w.CreatedAt().Equal(created) || !w.UpdatedAt().Equal(updated) {
		t.Errorf("rehydrated wallet = version %d loaded %d balance %v", w.Version(), w.LoadedVersion(), w.Balance())
	}

	// reidratar não aplica nada: a próxima movimentação parte da versão lida
	e, err := w.Credit(mv(t, 1, "1.00"))
	if err != nil || e.WalletVersion() != 8 || w.Version() != 8 || w.LoadedVersion() != 7 {
		t.Errorf("after credit: entry version %d, wallet %d, loaded %d, err %v", e.WalletVersion(), w.Version(), w.LoadedVersion(), err)
	}

	ok := wallet.RehydrateParams{ID: uid(1), PlayerID: uid(2), Balance: brl(t, "1.00"), Version: 1, CreatedAt: created, UpdatedAt: updated}
	tests := []struct {
		name   string
		mutate func(p *wallet.RehydrateParams)
	}{
		{"id nulo", func(p *wallet.RehydrateParams) { p.ID = uuid.Nil }},
		{"playerId nulo", func(p *wallet.RehydrateParams) { p.PlayerID = uuid.Nil }},
		{"saldo nao inicializado", func(p *wallet.RehydrateParams) { p.Balance = money.Money{} }},
		{"saldo negativo", func(p *wallet.RehydrateParams) { p.Balance = minor(t, -1, "BRL") }},
		{"versao zero", func(p *wallet.RehydrateParams) { p.Version = 0 }},
		{"versao negativa", func(p *wallet.RehydrateParams) { p.Version = -3 }},
		{"sem createdAt", func(p *wallet.RehydrateParams) { p.CreatedAt = time.Time{} }},
		{"sem updatedAt", func(p *wallet.RehydrateParams) { p.UpdatedAt = time.Time{} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := ok
			tc.mutate(&p)
			if got, err := wallet.Rehydrate(p); !errors.Is(err, wallet.ErrInvalidWallet) || got != nil {
				t.Errorf("Rehydrate = %v, %v; want ErrInvalidWallet", got, err)
			}
		})
	}
}

func TestTimesAreStoredInUTC(t *testing.T) {
	brt := time.FixedZone("BRT", -3*3600)
	w, _, err := wallet.Open(wallet.OpenParams{WalletID: uid(1), PlayerID: uid(2), InitialBalance: brl(t, "0.00"), Now: t0.In(brt)})
	if err != nil {
		t.Fatal(err)
	}
	if w.CreatedAt().Location() != time.UTC || !w.CreatedAt().Equal(t0) {
		t.Errorf("CreatedAt = %v (%v)", w.CreatedAt(), w.CreatedAt().Location())
	}
}

func TestMarkPersisted(t *testing.T) {
	w := openWallet(t, "100.00")
	if w.LoadedVersion() != 0 {
		t.Fatalf("a freshly opened wallet is not persisted: LoadedVersion = %d", w.LoadedVersion())
	}
	w.MarkPersisted()
	if w.LoadedVersion() != 1 || w.Version() != 1 {
		t.Errorf("after the INSERT: loaded %d version %d", w.LoadedVersion(), w.Version())
	}

	if _, err := w.Debit(mv(t, 1, "10.00")); err != nil {
		t.Fatal(err)
	}
	if w.LoadedVersion() != 1 || w.Version() != 2 {
		t.Errorf("a movement must not touch LoadedVersion: loaded %d version %d", w.LoadedVersion(), w.Version())
	}
	w.MarkPersisted() // o repositório confirmou o UPDATE
	if w.LoadedVersion() != 2 {
		t.Errorf("after the UPDATE: loaded %d, want 2", w.LoadedVersion())
	}
}
