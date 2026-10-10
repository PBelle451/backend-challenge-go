package wallet_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/PBelle451/backend-challenge-go/internal/domain/money"
	"github.com/PBelle451/backend-challenge-go/internal/domain/wallet"
)

func validParams(t *testing.T) wallet.LedgerEntryParams {
	return wallet.LedgerEntryParams{
		ID: uid(1), WalletID: uid(2), TransactionID: uid(3),
		Direction: wallet.DirectionDebit, Amount: brl(t, "25.00"),
		BalanceBefore: brl(t, "100.00"), BalanceAfter: brl(t, "75.00"),
		WalletVersion: 2, CreatedAt: t0,
	}
}

func TestNewLedgerEntry_Valid(t *testing.T) {
	debit := validParams(t)
	credit := validParams(t)
	credit.Direction = wallet.DirectionCredit
	credit.BalanceAfter = brl(t, "125.00")

	for name, p := range map[string]wallet.LedgerEntryParams{"debito": debit, "credito": credit} {
		t.Run(name, func(t *testing.T) {
			e, err := wallet.NewLedgerEntry(p)
			if err != nil {
				t.Fatal(err)
			}
			if e.ID() != p.ID || e.WalletID() != p.WalletID || e.TransactionID() != p.TransactionID ||
				e.Direction() != p.Direction || !e.Amount().Equal(p.Amount) ||
				!e.BalanceBefore().Equal(p.BalanceBefore) || !e.BalanceAfter().Equal(p.BalanceAfter) ||
				e.WalletVersion() != p.WalletVersion || !e.CreatedAt().Equal(p.CreatedAt) {
				t.Errorf("entry does not carry the given values: %+v", e)
			}
			// reidratar o mesmo lançamento persistido devolve o mesmo conteúdo
			r, err := wallet.RehydrateLedgerEntry(p)
			if err != nil || r != e {
				t.Errorf("rehydrated = %+v, %v; want %+v", r, err, e)
			}
		})
	}
}

func TestNewLedgerEntry_Invalid(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(p *wallet.LedgerEntryParams)
		is     error
	}{
		{"id nulo", func(p *wallet.LedgerEntryParams) { p.ID = uuid.Nil }, wallet.ErrInvalidLedgerEntry},
		{"walletId nulo", func(p *wallet.LedgerEntryParams) { p.WalletID = uuid.Nil }, wallet.ErrInvalidLedgerEntry},
		{"transactionId nulo", func(p *wallet.LedgerEntryParams) { p.TransactionID = uuid.Nil }, wallet.ErrInvalidLedgerEntry},
		{"direcao invalida", func(p *wallet.LedgerEntryParams) { p.Direction = "SIDEWAYS" }, wallet.ErrInvalidLedgerEntry},
		{"direcao vazia", func(p *wallet.LedgerEntryParams) { p.Direction = "" }, wallet.ErrInvalidLedgerEntry},
		{"valor zero", func(p *wallet.LedgerEntryParams) { p.Amount = brl(t, "0.00") }, wallet.ErrInvalidLedgerEntry},
		{"valor negativo", func(p *wallet.LedgerEntryParams) { p.Amount = minor(t, -2500, "BRL") }, wallet.ErrInvalidLedgerEntry},
		{"valor nao inicializado", func(p *wallet.LedgerEntryParams) { p.Amount = money.Money{} }, wallet.ErrInvalidLedgerEntry},
		{"saldo anterior nao inicializado", func(p *wallet.LedgerEntryParams) { p.BalanceBefore = money.Money{} }, wallet.ErrInvalidLedgerEntry},
		{"saldo posterior nao inicializado", func(p *wallet.LedgerEntryParams) { p.BalanceAfter = money.Money{} }, wallet.ErrInvalidLedgerEntry},
		{"aritmetica errada no debito", func(p *wallet.LedgerEntryParams) { p.BalanceAfter = brl(t, "76.00") }, wallet.ErrInvalidLedgerEntry},
		{"aritmetica errada no credito", func(p *wallet.LedgerEntryParams) {
			p.Direction = wallet.DirectionCredit // 100 + 25 != 75
		}, wallet.ErrInvalidLedgerEntry},
		{"debito que deixaria saldo negativo", func(p *wallet.LedgerEntryParams) {
			p.BalanceBefore = brl(t, "10.00")
			p.BalanceAfter = minor(t, -1500, "BRL")
		}, wallet.ErrInvalidLedgerEntry},
		{"moeda do valor diferente da dos saldos", func(p *wallet.LedgerEntryParams) { p.Amount = minor(t, 2500, "USD") }, money.ErrCurrencyMismatch},
		{"moeda do saldo posterior diferente", func(p *wallet.LedgerEntryParams) { p.BalanceAfter = minor(t, 7500, "USD") }, money.ErrCurrencyMismatch},
		{"versao zero", func(p *wallet.LedgerEntryParams) { p.WalletVersion = 0 }, wallet.ErrInvalidLedgerEntry},
		{"sem instante", func(p *wallet.LedgerEntryParams) { p.CreatedAt = time.Time{} }, wallet.ErrInvalidLedgerEntry},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validParams(t)
			tc.mutate(&p)
			for label, build := range map[string]func(wallet.LedgerEntryParams) (wallet.LedgerEntry, error){
				"New": wallet.NewLedgerEntry, "Rehydrate": wallet.RehydrateLedgerEntry,
			} {
				e, err := build(p)
				if !errors.Is(err, tc.is) {
					t.Errorf("%s: error = %v, want %v", label, err, tc.is)
				}
				if e != (wallet.LedgerEntry{}) {
					t.Errorf("%s: an invalid entry must not be returned", label)
				}
			}
		})
	}
}

func TestDirectionValid(t *testing.T) {
	if !wallet.DirectionDebit.Valid() || !wallet.DirectionCredit.Valid() {
		t.Error("DEBIT and CREDIT must be valid")
	}
	for _, d := range []wallet.Direction{"", "debit", "credit", "OTHER"} {
		if d.Valid() {
			t.Errorf("%q must not be valid", d)
		}
	}
}
