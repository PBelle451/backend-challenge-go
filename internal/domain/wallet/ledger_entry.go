package wallet

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/PBelle451/backend-challenge-go/internal/domain/money"
)

// ErrInvalidLedgerEntry indica que um lançamento violaria suas invariantes.
var ErrInvalidLedgerEntry = errors.New("wallet: invalid ledger entry")

// Direction é o sentido de um lançamento no ledger.
type Direction string

const (
	DirectionDebit  Direction = "DEBIT"
	DirectionCredit Direction = "CREDIT"
)

// Valid informa se a direção é DEBIT ou CREDIT.
func (d Direction) Valid() bool { return d == DirectionDebit || d == DirectionCredit }

// LedgerEntry é um lançamento imutável do ledger da carteira (append-only).
// Correções financeiras exigem novos lançamentos, nunca a edição de um existente.
type LedgerEntry struct {
	id            uuid.UUID
	walletID      uuid.UUID
	transactionID uuid.UUID
	direction     Direction
	amount        money.Money
	balanceBefore money.Money
	balanceAfter  money.Money
	walletVersion int64
	createdAt     time.Time
}

// LedgerEntryParams reúne os dados de um lançamento.
type LedgerEntryParams struct {
	ID            uuid.UUID
	WalletID      uuid.UUID
	TransactionID uuid.UUID
	Direction     Direction
	Amount        money.Money
	BalanceBefore money.Money
	BalanceAfter  money.Money
	WalletVersion int64 // versão da carteira APÓS este lançamento
	CreatedAt     time.Time
}

// NewLedgerEntry cria um lançamento validando balanceAfter = balanceBefore ± amount.
func NewLedgerEntry(p LedgerEntryParams) (LedgerEntry, error) {
	return buildLedgerEntry(p)
}

// RehydrateLedgerEntry reconstrói um lançamento já persistido. Não gera
// identidade nem instante e não reaplica movimentação alguma; apenas revalida
// as invariantes do que veio do armazenamento.
func RehydrateLedgerEntry(p LedgerEntryParams) (LedgerEntry, error) {
	return buildLedgerEntry(p)
}

func buildLedgerEntry(p LedgerEntryParams) (LedgerEntry, error) {
	fail := func(format string, args ...any) (LedgerEntry, error) {
		return LedgerEntry{}, fmt.Errorf("%w: %s", ErrInvalidLedgerEntry, fmt.Sprintf(format, args...))
	}

	if p.ID == uuid.Nil || p.WalletID == uuid.Nil || p.TransactionID == uuid.Nil {
		return fail("id, walletId and transactionId are required")
	}
	if !p.Direction.Valid() {
		return fail("direction %q is not DEBIT or CREDIT", p.Direction)
	}
	if !p.Amount.IsPositive() {
		return fail("amount must be positive (got %v)", p.Amount)
	}
	if !p.BalanceBefore.IsValid() || !p.BalanceAfter.IsValid() {
		return fail("balances must be initialized")
	}
	if p.BalanceBefore.IsNegative() || p.BalanceAfter.IsNegative() {
		return fail("balances cannot be negative")
	}
	if p.Amount.Currency() != p.BalanceBefore.Currency() || p.Amount.Currency() != p.BalanceAfter.Currency() {
		return LedgerEntry{}, fmt.Errorf("%w: %w", ErrInvalidLedgerEntry, money.ErrCurrencyMismatch)
	}

	var expected money.Money
	var err error
	if p.Direction == DirectionCredit {
		expected, err = p.BalanceBefore.Add(p.Amount)
	} else {
		expected, err = p.BalanceBefore.Sub(p.Amount)
	}
	if err != nil {
		return fail("%v", err)
	}
	if !expected.Equal(p.BalanceAfter) {
		return fail("balanceAfter %v != balanceBefore %v %s amount %v",
			p.BalanceAfter, p.BalanceBefore, sign(p.Direction), p.Amount)
	}

	if p.WalletVersion < 1 {
		return fail("walletVersion must be >= 1")
	}
	if p.CreatedAt.IsZero() {
		return fail("createdAt is required")
	}

	return LedgerEntry{
		id:            p.ID,
		walletID:      p.WalletID,
		transactionID: p.TransactionID,
		direction:     p.Direction,
		amount:        p.Amount,
		balanceBefore: p.BalanceBefore,
		balanceAfter:  p.BalanceAfter,
		walletVersion: p.WalletVersion,
		createdAt:     p.CreatedAt.UTC(),
	}, nil
}

func sign(d Direction) string {
	if d == DirectionCredit {
		return "+"
	}
	return "-"
}

func (e LedgerEntry) ID() uuid.UUID            { return e.id }
func (e LedgerEntry) WalletID() uuid.UUID      { return e.walletID }
func (e LedgerEntry) TransactionID() uuid.UUID { return e.transactionID }
func (e LedgerEntry) Direction() Direction     { return e.direction }
func (e LedgerEntry) Amount() money.Money      { return e.amount }
func (e LedgerEntry) BalanceBefore() money.Money {
	return e.balanceBefore
}
func (e LedgerEntry) BalanceAfter() money.Money { return e.balanceAfter }
func (e LedgerEntry) WalletVersion() int64      { return e.walletVersion }
func (e LedgerEntry) CreatedAt() time.Time      { return e.createdAt }
