package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/PBelle451/backend-challenge-go/internal/infra/postgres"
	"github.com/google/uuid"

	"github.com/PBelle451/backend-challenge-go/internal/domain/money"
	"github.com/PBelle451/backend-challenge-go/internal/domain/wager"
	"github.com/PBelle451/backend-challenge-go/internal/domain/wallet"
)

var t0 = time.Date(2026, 10, 10, 12, 0, 0, 123456000, time.UTC) // precisão de microssegundos, como o PostgreSQL

func brl(t testing.TB, amount string) money.Money {
	t.Helper()
	m, err := money.Parse(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// openWallet executa o fluxo de abertura completo, em uma única transação SQL:
// carteira + transação OPENING + lançamento de crédito (quando o saldo é positivo).
func openWallet(t testing.TB, env *testEnv, initial string) (walletID, playerID uuid.UUID) {
	t.Helper()
	walletID, playerID = uuid.New(), uuid.New()
	err := env.TM.Do(context.Background(), func(ctx context.Context, r postgres.Repositories) error {
		openingTx := uuid.New()
		w, entry, err := wallet.Open(wallet.OpenParams{
			WalletID: walletID, PlayerID: playerID, InitialBalance: brl(t, initial),
			OpeningTransactionID: openingTx, OpeningEntryID: uuid.New(), Now: t0,
		})
		if err != nil {
			return err
		}
		if err := r.Wallets.Insert(ctx, w); err != nil {
			return err
		}
		if entry == nil {
			return nil
		}
		tx, err := wager.NewOpening(wager.NewOpeningParams{
			ID: openingTx, WalletID: walletID, PlayerID: playerID, Amount: brl(t, initial), Now: t0,
		})
		if err != nil {
			return err
		}
		if err := r.Transactions.Insert(ctx, tx); err != nil {
			return err
		}
		return r.Ledger.Insert(ctx, *entry)
	})
	if err != nil {
		t.Fatalf("openWallet: %v", err)
	}
	return walletID, playerID
}

func newExternal(t testing.TB, id uuid.UUID, kind wager.Kind, walletID, playerID uuid.UUID, extID, amount, ref string, now time.Time) *wager.Transaction {
	t.Helper()
	hash, err := wager.ComputePayloadHash(wager.HashInput{
		ProviderID: "provider-a", ExternalTransactionID: extID, PlayerID: playerID, WalletID: walletID,
		RoundID: "round-1", GameID: "game-1", Kind: kind, Money: brl(t, amount), ReferenceExternalTransactionID: ref,
	})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := wager.NewExternal(wager.NewExternalParams{
		ID: id, Kind: kind, WalletID: walletID, PlayerID: playerID, Amount: brl(t, amount), Now: now,
		Origin: wager.Origin{
			ProviderID: "provider-a", ExternalTransactionID: extID, IdempotencyKey: "provider-a:" + extID,
			PayloadHash: hash, RoundID: "round-1", GameID: "game-1", ReferenceExternalTransactionID: ref,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

type betResult struct {
	Status wager.Status
	Code   wager.FailureCode
}

// placeBet é o fluxo de uma aposta como o caso de uso o fará: trava a carteira,
// grava a transação, debita, e então confirma saldo + ledger + transação juntos.
// Com insertFirst, grava a transação ANTES de travar a carteira (ordem inversa),
// o que provaria um deadlock se o lock da carteira conflitasse com as chaves estrangeiras.
func placeBet(env *testEnv, walletID, playerID uuid.UUID, extID, amount string, insertFirst bool, tt testing.TB) (betResult, error) {
	var res betResult
	err := env.TM.Do(context.Background(), func(ctx context.Context, r postgres.Repositories) error {
		res = betResult{}
		tx := newExternal(tt, uuid.New(), wager.KindBet, walletID, playerID, extID, amount, "", t0)
		var w *wallet.Wallet
		var err error
		if insertFirst {
			if err = r.Transactions.Insert(ctx, tx); err != nil {
				return err
			}
			if w, err = r.Wallets.GetForUpdate(ctx, walletID); err != nil {
				return err
			}
		} else {
			if w, err = r.Wallets.GetForUpdate(ctx, walletID); err != nil {
				return err
			}
			if err = r.Transactions.Insert(ctx, tx); err != nil {
				return err
			}
		}

		entry, err := w.Debit(wallet.MovementParams{EntryID: uuid.New(), TransactionID: tx.ID(), Amount: brl(tt, amount), Now: t0.Add(time.Second)})
		if errors.Is(err, wallet.ErrInsufficientFunds) {
			if err := tx.Reject(wager.KindBet.InsufficientFundsCode(), t0.Add(time.Second)); err != nil {
				return err
			}
			res = betResult{tx.Status(), tx.FailureCode()}
			return r.Transactions.Save(ctx, tx)
		}
		if err != nil {
			return err
		}
		if err := r.Wallets.Save(ctx, w); err != nil {
			return err
		}
		if err := r.Ledger.Insert(ctx, entry); err != nil {
			return err
		}
		if err := tx.Process(wager.ProcessParams{ResultBalance: w.Balance(), Now: t0.Add(time.Second)}); err != nil {
			return err
		}
		res = betResult{tx.Status(), tx.FailureCode()}
		return r.Transactions.Save(ctx, tx)
	})
	return res, err
}

// runConcurrently dispara n goroutines ao mesmo tempo e devolve os resultados em ordem.
func runConcurrently[T any](n int, fn func(i int) T) []T {
	out := make([]T, n)
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	ready.Add(n)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			out[i] = fn(i)
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()
	return out
}

// assertLedgerConsistent confere, lendo o banco, que o saldo da carteira é
// exatamente o que o ledger conta: cadeia contínua, versões sequenciais e
// saldo final = soma de créditos - débitos. Devolve a carteira e os lançamentos.
func assertLedgerConsistent(t testing.TB, env *testEnv, walletID uuid.UUID) (*wallet.Wallet, []wallet.LedgerEntry) {
	t.Helper()
	ctx := context.Background()
	r := env.TM.Reader()
	w, err := r.Wallets.GetByID(ctx, walletID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	var all []wallet.LedgerEntry
	var cursor int64
	for {
		page, err := r.Ledger.List(ctx, walletID, cursor, 7)
		if err != nil {
			t.Fatalf("Ledger.List: %v", err)
		}
		if len(page) == 0 {
			break
		}
		all = append(all, page...)
		cursor = page[len(page)-1].WalletVersion()
	}
	var balance int64
	prevVersion := all[0].WalletVersion() - 1
	for i, e := range all {
		if e.WalletVersion() != prevVersion+1 {
			t.Fatalf("entry %d: version %d after %d", i, e.WalletVersion(), prevVersion)
		}
		if e.BalanceBefore().Minor() != balance {
			t.Fatalf("entry %d: balanceBefore %d != running balance %d", i, e.BalanceBefore().Minor(), balance)
		}
		if e.Direction() == wallet.DirectionCredit {
			balance += e.Amount().Minor()
		} else {
			balance -= e.Amount().Minor()
		}
		if balance != e.BalanceAfter().Minor() {
			t.Fatalf("entry %d: balanceAfter %d != %d", i, e.BalanceAfter().Minor(), balance)
		}
		prevVersion = e.WalletVersion()
	}
	if w.Balance().Minor() != balance {
		t.Fatalf("wallet balance %d != ledger balance %d", w.Balance().Minor(), balance)
	}
	if w.Version() != prevVersion {
		t.Fatalf("wallet version %d != last ledger version %d", w.Version(), prevVersion)
	}
	return w, all
}
