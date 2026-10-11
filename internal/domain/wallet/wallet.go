// Package wallet contém a Wallet (raiz do agregado financeiro) e o LedgerEntry.
//
// Concorrência: a Wallet NÃO é segura para uso concorrente. A coordenação entre
// escritores é feita fora do agregado, por carteira, na transação SQL (lock
// pessimista com SELECT ... FOR UPDATE) e reforçada pelo banco (CHECK de saldo,
// trigger de versão e checagem do ledger no commit). O agregado garante as
// invariantes locais: saldo >= 0, moeda consistente e versão incrementada somente
// quando o saldo muda, sempre junto de um lançamento do ledger.
package wallet

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/PBelle451/backend-challenge-go/internal/domain/money"
)

// Erros de domínio, classificáveis com errors.Is.
var (
	ErrInvalidWallet     = errors.New("wallet: invalid wallet")
	ErrInsufficientFunds = errors.New("wallet: insufficient funds")
	ErrNonPositiveAmount = errors.New("wallet: amount must be positive")
	ErrInvalidMovement   = errors.New("wallet: invalid movement")
)

// Wallet é a raiz do agregado financeiro de um jogador em uma moeda.
type Wallet struct {
	id            uuid.UUID
	playerID      uuid.UUID
	balance       money.Money
	version       int64
	loadedVersion int64 // versão lida do armazenamento (0 = ainda não persistida)
	createdAt     time.Time
	updatedAt     time.Time
}

// OpenParams reúne os dados da abertura de uma carteira.
type OpenParams struct {
	WalletID       uuid.UUID
	PlayerID       uuid.UUID
	InitialBalance money.Money
	// Usados apenas quando o saldo inicial é positivo (geram a transação OPENING e seu lançamento).
	OpeningTransactionID uuid.UUID
	OpeningEntryID       uuid.UUID
	Now                  time.Time
}

// Open cria uma carteira nova com versão 1.
//
// Com saldo inicial positivo, devolve também o lançamento de crédito da abertura
// (saldo anterior 0, versão 1), que deve ser persistido na mesma transação SQL da
// carteira e da transação OPENING. Com saldo zero, não há lançamento (nil).
func Open(p OpenParams) (*Wallet, *LedgerEntry, error) {
	if p.WalletID == uuid.Nil || p.PlayerID == uuid.Nil {
		return nil, nil, fmt.Errorf("%w: walletId and playerId are required", ErrInvalidWallet)
	}
	if !p.InitialBalance.IsValid() {
		return nil, nil, fmt.Errorf("%w: initial balance is uninitialized", ErrInvalidWallet)
	}
	if p.InitialBalance.IsNegative() {
		return nil, nil, fmt.Errorf("%w: initial balance cannot be negative", ErrInvalidWallet)
	}
	if p.Now.IsZero() {
		return nil, nil, fmt.Errorf("%w: creation time is required", ErrInvalidWallet)
	}
	now := p.Now.UTC()

	w := &Wallet{
		id:        p.WalletID,
		playerID:  p.PlayerID,
		balance:   p.InitialBalance,
		version:   1,
		createdAt: now,
		updatedAt: now,
	}
	if !p.InitialBalance.IsPositive() {
		return w, nil, nil
	}

	zero, err := money.Zero(p.InitialBalance.Currency())
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidWallet, err)
	}
	entry, err := NewLedgerEntry(LedgerEntryParams{
		ID:            p.OpeningEntryID,
		WalletID:      p.WalletID,
		TransactionID: p.OpeningTransactionID,
		Direction:     DirectionCredit,
		Amount:        p.InitialBalance,
		BalanceBefore: zero,
		BalanceAfter:  p.InitialBalance,
		WalletVersion: 1,
		CreatedAt:     now,
	})
	if err != nil {
		return nil, nil, err
	}
	return w, &entry, nil
}

// RehydrateParams reúne o estado persistido de uma carteira.
type RehydrateParams struct {
	ID        uuid.UUID
	PlayerID  uuid.UUID
	Balance   money.Money
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Rehydrate reconstrói uma carteira persistida. Não aplica movimentações, não
// altera a versão e não gera eventos; só revalida as invariantes do que foi lido.
func Rehydrate(p RehydrateParams) (*Wallet, error) {
	switch {
	case p.ID == uuid.Nil || p.PlayerID == uuid.Nil:
		return nil, fmt.Errorf("%w: id and playerId are required", ErrInvalidWallet)
	case !p.Balance.IsValid():
		return nil, fmt.Errorf("%w: balance is uninitialized", ErrInvalidWallet)
	case p.Balance.IsNegative():
		return nil, fmt.Errorf("%w: balance cannot be negative", ErrInvalidWallet)
	case p.Version < 1:
		return nil, fmt.Errorf("%w: version must be >= 1", ErrInvalidWallet)
	case p.CreatedAt.IsZero() || p.UpdatedAt.IsZero():
		return nil, fmt.Errorf("%w: timestamps are required", ErrInvalidWallet)
	}
	return &Wallet{
		id:            p.ID,
		playerID:      p.PlayerID,
		balance:       p.Balance,
		version:       p.Version,
		loadedVersion: p.Version,
		createdAt:     p.CreatedAt.UTC(),
		updatedAt:     p.UpdatedAt.UTC(),
	}, nil
}

// MovementParams identifica uma movimentação de saldo.
type MovementParams struct {
	EntryID       uuid.UUID
	TransactionID uuid.UUID
	Amount        money.Money
	Now           time.Time
}

// Debit retira Amount do saldo e devolve o lançamento correspondente. Se o saldo
// for insuficiente, devolve ErrInsufficientFunds e a carteira permanece intacta.
func (w *Wallet) Debit(p MovementParams) (LedgerEntry, error) {
	return w.apply(DirectionDebit, p)
}

// Credit soma Amount ao saldo e devolve o lançamento correspondente.
func (w *Wallet) Credit(p MovementParams) (LedgerEntry, error) {
	return w.apply(DirectionCredit, p)
}

func (w *Wallet) apply(direction Direction, p MovementParams) (LedgerEntry, error) {
	if p.EntryID == uuid.Nil || p.TransactionID == uuid.Nil {
		return LedgerEntry{}, fmt.Errorf("%w: entryId and transactionId are required", ErrInvalidMovement)
	}
	if p.Now.IsZero() {
		return LedgerEntry{}, fmt.Errorf("%w: movement time is required", ErrInvalidMovement)
	}
	if !p.Amount.IsValid() {
		return LedgerEntry{}, fmt.Errorf("%w: amount is uninitialized", ErrInvalidMovement)
	}
	if p.Amount.Currency() != w.balance.Currency() {
		return LedgerEntry{}, fmt.Errorf("%w: wallet is %s, movement is %s",
			money.ErrCurrencyMismatch, w.balance.Currency(), p.Amount.Currency())
	}
	if !p.Amount.IsPositive() {
		return LedgerEntry{}, ErrNonPositiveAmount
	}

	before := w.balance
	var after money.Money
	var err error
	if direction == DirectionDebit {
		after, err = before.Sub(p.Amount)
		if err == nil && after.IsNegative() {
			return LedgerEntry{}, fmt.Errorf("%w: balance %v, debit %v", ErrInsufficientFunds, before, p.Amount)
		}
	} else {
		after, err = before.Add(p.Amount)
	}
	if err != nil {
		return LedgerEntry{}, err
	}

	// O lançamento é construído (e validado) ANTES de qualquer mutação: se algo
	// falhar, a carteira permanece exatamente como estava.
	entry, err := NewLedgerEntry(LedgerEntryParams{
		ID:            p.EntryID,
		WalletID:      w.id,
		TransactionID: p.TransactionID,
		Direction:     direction,
		Amount:        p.Amount,
		BalanceBefore: before,
		BalanceAfter:  after,
		WalletVersion: w.version + 1,
		CreatedAt:     p.Now,
	})
	if err != nil {
		return LedgerEntry{}, err
	}

	w.balance = after
	w.version++
	w.updatedAt = p.Now.UTC()
	return entry, nil
}

func (w *Wallet) ID() uuid.UUID            { return w.id }
func (w *Wallet) PlayerID() uuid.UUID      { return w.playerID }
func (w *Wallet) Balance() money.Money     { return w.balance }
func (w *Wallet) Currency() money.Currency { return w.balance.Currency() }
func (w *Wallet) Version() int64           { return w.version }
func (w *Wallet) CreatedAt() time.Time     { return w.createdAt }
func (w *Wallet) UpdatedAt() time.Time     { return w.updatedAt }

// MarkPersisted registra que o estado atual foi gravado com sucesso: a versão
// lida passa a ser a versão corrente. Chamado pelo repositório após INSERT/UPDATE.
func (w *Wallet) MarkPersisted() { w.loadedVersion = w.version }

// LoadedVersion é a versão lida do armazenamento (0 para uma carteira ainda não
// persistida). O repositório a usa no UPDATE condicional (WHERE version = loaded).
func (w *Wallet) LoadedVersion() int64 { return w.loadedVersion }
