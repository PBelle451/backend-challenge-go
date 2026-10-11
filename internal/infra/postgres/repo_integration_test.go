package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/PBelle451/backend-challenge-go/internal/domain/wager"
	"github.com/PBelle451/backend-challenge-go/internal/domain/wallet"
	"github.com/PBelle451/backend-challenge-go/internal/infra/postgres"
)

func TestOpeningFlow_PersistsAndReadsBack(t *testing.T) {
	env := newEnv(t, nil)
	walletID, playerID := openWallet(t, env, "1000.00")
	ctx := context.Background()
	r := env.TM.Reader()

	w, err := r.Wallets.GetByID(ctx, walletID)
	if err != nil {
		t.Fatal(err)
	}
	if w.Balance().Amount() != "1000.00" || w.Version() != 1 || w.LoadedVersion() != 1 ||
		w.PlayerID() != playerID || w.Currency() != "BRL" {
		t.Errorf("wallet = %v v%d loaded %d", w.Balance(), w.Version(), w.LoadedVersion())
	}
	// instantes: UTC e com a precisão de microssegundos preservada
	if w.CreatedAt().Location() != time.UTC || !w.CreatedAt().Equal(t0) {
		t.Errorf("CreatedAt = %v (%v), want %v", w.CreatedAt(), w.CreatedAt().Location(), t0)
	}

	byPlayer, err := r.Wallets.GetByPlayer(ctx, playerID, "BRL")
	if err != nil || byPlayer.ID() != walletID {
		t.Errorf("GetByPlayer = %v, %v", byPlayer, err)
	}

	_, entries := assertLedgerConsistent(t, env, walletID)
	if len(entries) != 1 || entries[0].Direction() != wallet.DirectionCredit || entries[0].WalletVersion() != 1 ||
		entries[0].BalanceBefore().Amount() != "0.00" || entries[0].BalanceAfter().Amount() != "1000.00" {
		t.Errorf("opening ledger = %+v", entries)
	}

	opening, err := r.Transactions.GetByID(ctx, entries[0].TransactionID())
	if err != nil {
		t.Fatal(err)
	}
	if rb, _ := opening.ResultBalance(); opening.Kind() != wager.KindOpening || opening.Status() != wager.StatusProcessed || rb.Amount() != "1000.00" {
		t.Errorf("opening tx = %s %s", opening.Kind(), opening.Status())
	}
	if _, ok := opening.Origin(); ok {
		t.Error("OPENING must not carry external metadata")
	}
}

func TestNotFound(t *testing.T) {
	env := newEnv(t, nil)
	ctx := context.Background()
	r := env.TM.Reader()
	if _, err := r.Wallets.GetByID(ctx, uuid.New()); !errors.Is(err, postgres.ErrNotFound) {
		t.Errorf("GetByID: %v", err)
	}
	if _, err := r.Wallets.GetByPlayer(ctx, uuid.New(), "BRL"); !errors.Is(err, postgres.ErrNotFound) {
		t.Errorf("GetByPlayer: %v", err)
	}
	if _, err := r.Transactions.GetByID(ctx, uuid.New()); !errors.Is(err, postgres.ErrNotFound) {
		t.Errorf("Transactions.GetByID: %v", err)
	}
	if _, err := r.Transactions.GetByProviderExternalID(ctx, "p", "x"); !errors.Is(err, postgres.ErrNotFound) {
		t.Errorf("GetByProviderExternalID: %v", err)
	}
	if _, err := r.Transactions.GetByProviderIdempotencyKey(ctx, "p", "k"); !errors.Is(err, postgres.ErrNotFound) {
		t.Errorf("GetByProviderIdempotencyKey: %v", err)
	}
}

// Uma carteira com saldo positivo sem o lançamento de abertura não sobrevive ao commit.
func TestWalletWithoutOpeningEntryIsRejectedAtCommit(t *testing.T) {
	env := newEnv(t, nil)
	walletID := uuid.New()
	err := env.TM.Do(context.Background(), func(ctx context.Context, r postgres.Repositories) error {
		w, _, err := wallet.Open(wallet.OpenParams{
			WalletID: walletID, PlayerID: uuid.New(), InitialBalance: brl(t, "100.00"),
			OpeningTransactionID: uuid.New(), OpeningEntryID: uuid.New(), Now: t0,
		})
		if err != nil {
			return err
		}
		return r.Wallets.Insert(ctx, w) // sem a transação OPENING e sem o lançamento
	})
	if !errors.Is(err, postgres.ErrIntegrity) {
		t.Fatalf("error = %v, want ErrIntegrity", err)
	}
	if _, err := env.TM.Reader().Wallets.GetByID(context.Background(), walletID); !errors.Is(err, postgres.ErrNotFound) {
		t.Errorf("the wallet must not exist after the failed commit: %v", err)
	}
}

// Alterar o saldo sem o lançamento correspondente falha no commit e desfaz tudo.
func TestBalanceChangeWithoutLedgerEntryIsRolledBack(t *testing.T) {
	env := newEnv(t, nil)
	walletID, _ := openWallet(t, env, "100.00")

	err := env.TM.Do(context.Background(), func(ctx context.Context, r postgres.Repositories) error {
		w, err := r.Wallets.GetForUpdate(ctx, walletID)
		if err != nil {
			return err
		}
		if _, err := w.Debit(wallet.MovementParams{EntryID: uuid.New(), TransactionID: uuid.New(), Amount: brl(t, "30.00"), Now: t0}); err != nil {
			return err
		}
		return r.Wallets.Save(ctx, w) // o lançamento é "esquecido"
	})
	if !errors.Is(err, postgres.ErrIntegrity) {
		t.Fatalf("error = %v, want ErrIntegrity", err)
	}
	w, _ := assertLedgerConsistent(t, env, walletID)
	if w.Balance().Amount() != "100.00" || w.Version() != 1 {
		t.Errorf("balance %v version %d: the failed transaction left traces", w.Balance(), w.Version())
	}
}

func TestDo_RollsBackOnError(t *testing.T) {
	env := newEnv(t, nil)
	walletID := uuid.New()
	boom := errors.New("boom")
	err := env.TM.Do(context.Background(), func(ctx context.Context, r postgres.Repositories) error {
		w, _, _ := wallet.Open(wallet.OpenParams{WalletID: walletID, PlayerID: uuid.New(), InitialBalance: brl(t, "0.00"), Now: t0})
		if err := r.Wallets.Insert(ctx, w); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v", err)
	}
	if _, err := env.TM.Reader().Wallets.GetByID(context.Background(), walletID); !errors.Is(err, postgres.ErrNotFound) {
		t.Errorf("the insert must have been rolled back: %v", err)
	}
}

func TestWalletUniquePerPlayerAndCurrency(t *testing.T) {
	env := newEnv(t, nil)
	_, playerID := openWallet(t, env, "0.00")
	err := env.TM.Do(context.Background(), func(ctx context.Context, r postgres.Repositories) error {
		w, _, _ := wallet.Open(wallet.OpenParams{WalletID: uuid.New(), PlayerID: playerID, InitialBalance: brl(t, "0.00"), Now: t0})
		return r.Wallets.Insert(ctx, w)
	})
	if !errors.Is(err, postgres.ErrWalletExists) {
		t.Fatalf("error = %v, want ErrWalletExists", err)
	}
}

func TestInsertTransaction_TypedConstraintErrors(t *testing.T) {
	env := newEnv(t, nil)
	walletID, playerID := openWallet(t, env, "0.00")
	ctx := context.Background()

	insert := func(tx *wager.Transaction) error {
		return env.TM.Do(ctx, func(ctx context.Context, r postgres.Repositories) error { return r.Transactions.Insert(ctx, tx) })
	}
	first := newExternal(t, uuid.New(), wager.KindBet, walletID, playerID, "tx-1", "10.00", "", t0)
	if err := insert(first); err != nil {
		t.Fatal(err)
	}

	// mesmo (provedor, id externo)
	dup := newExternal(t, uuid.New(), wager.KindBet, walletID, playerID, "tx-1", "10.00", "", t0)
	if err := insert(dup); !errors.Is(err, postgres.ErrDuplicateExternalID) {
		t.Errorf("duplicate external id: %v", err)
	}

	// mesma chave de idempotência, outro id externo (chave fabricada à mão)
	hash, _ := wager.ComputePayloadHash(wager.HashInput{
		ProviderID: "provider-a", ExternalTransactionID: "tx-2", PlayerID: playerID, WalletID: walletID,
		RoundID: "round-1", GameID: "game-1", Kind: wager.KindBet, Money: brl(t, "10.00"),
	})
	sameKey, _ := wager.NewExternal(wager.NewExternalParams{
		ID: uuid.New(), Kind: wager.KindBet, WalletID: walletID, PlayerID: playerID, Amount: brl(t, "10.00"), Now: t0,
		Origin: wager.Origin{ProviderID: "provider-a", ExternalTransactionID: "tx-2", IdempotencyKey: "provider-a:tx-1",
			PayloadHash: hash, RoundID: "round-1", GameID: "game-1"},
	})
	if err := insert(sameKey); !errors.Is(err, postgres.ErrDuplicateIdempotencyKey) {
		t.Errorf("duplicate idempotency key: %v", err)
	}

	// carteira inexistente e jogador divergente
	if err := insert(newExternal(t, uuid.New(), wager.KindBet, uuid.New(), playerID, "tx-3", "10.00", "", t0)); !errors.Is(err, postgres.ErrWalletMismatch) {
		t.Errorf("unknown wallet: %v", err)
	}
	if err := insert(newExternal(t, uuid.New(), wager.KindBet, walletID, uuid.New(), "tx-4", "10.00", "", t0)); !errors.Is(err, postgres.ErrWalletMismatch) {
		t.Errorf("player mismatch: %v", err)
	}
}

func TestSecondOpeningForTheSameWalletIsRejected(t *testing.T) {
	env := newEnv(t, nil)
	walletID, playerID := openWallet(t, env, "100.00")
	opening, _ := wager.NewOpening(wager.NewOpeningParams{ID: uuid.New(), WalletID: walletID, PlayerID: playerID, Amount: brl(t, "5.00"), Now: t0})
	err := env.TM.Do(context.Background(), func(ctx context.Context, r postgres.Repositories) error {
		return r.Transactions.Insert(ctx, opening)
	})
	if !errors.Is(err, postgres.ErrOpeningExists) {
		t.Fatalf("error = %v, want ErrOpeningExists", err)
	}
}

func TestTransactionRoundTrip_AllStates(t *testing.T) {
	env := newEnv(t, nil)
	walletID, playerID := openWallet(t, env, "0.00")
	ctx := context.Background()
	later := t0.Add(time.Minute)

	mk := func(ext string, kind wager.Kind, amount, ref string) *wager.Transaction {
		return newExternal(t, uuid.New(), kind, walletID, playerID, ext, amount, ref, t0)
	}
	pending := mk("pending", wager.KindBet, "10.00", "")
	waiting := mk("waiting", wager.KindRefund, "10.00", "bet-x")
	rejected := mk("rejected", wager.KindBet, "10.00", "")
	failed := mk("failed", wager.KindBet, "10.00", "")
	processed := mk("processed", wager.KindLoss, "0.00", "")

	if err := waiting.AwaitReference(later, later.Add(time.Second), later.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := waiting.Retry(later, later.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := rejected.Reject(wager.FailureInsufficientFunds, later); err != nil {
		t.Fatal(err)
	}
	if err := failed.Fail(wager.FailureRetriesExhausted, later); err != nil {
		t.Fatal(err)
	}
	if err := processed.Process(wager.ProcessParams{ResultBalance: brl(t, "0.00"), Now: later}); err != nil {
		t.Fatal(err)
	}

	all := []*wager.Transaction{pending, waiting, rejected, failed, processed}
	if err := env.TM.Do(ctx, func(ctx context.Context, r postgres.Repositories) error {
		for _, tx := range all {
			if err := r.Transactions.Insert(ctx, tx); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	for _, want := range all {
		got, err := env.TM.Reader().Transactions.GetByID(ctx, want.ID())
		if err != nil {
			t.Fatalf("%s: %v", want.Status(), err)
		}
		wo, _ := want.Origin()
		gotOrigin, _ := got.Origin()
		wrb, _ := want.ResultBalance()
		grb, _ := got.ResultBalance()
		if got.Status() != want.Status() || got.Kind() != want.Kind() || got.FailureCode() != want.FailureCode() ||
			got.Attempts() != want.Attempts() || !got.Amount().Equal(want.Amount()) ||
			!got.NextAttemptAt().Equal(want.NextAttemptAt()) || !got.ExpiresAt().Equal(want.ExpiresAt()) ||
			!got.FinishedAt().Equal(want.FinishedAt()) || !got.UpdatedAt().Equal(want.UpdatedAt()) ||
			wrb.Minor() != grb.Minor() || wo != gotOrigin {
			t.Errorf("%s: round trip differs\n got  %+v\n want %+v", want.Status(), got, want)
		}
		// as buscas alternativas devolvem a mesma transação
		byExt, err := env.TM.Reader().Transactions.GetByProviderExternalID(ctx, wo.ProviderID, wo.ExternalTransactionID)
		if err != nil || byExt.ID() != want.ID() {
			t.Errorf("GetByProviderExternalID: %v", err)
		}
		byKey, err := env.TM.Reader().Transactions.GetByProviderIdempotencyKey(ctx, wo.ProviderID, wo.IdempotencyKey)
		if err != nil || byKey.ID() != want.ID() {
			t.Errorf("GetByProviderIdempotencyKey: %v", err)
		}
	}
}

func TestSaveTransaction_StateConflictWhenAlreadyTerminal(t *testing.T) {
	env := newEnv(t, nil)
	walletID, playerID := openWallet(t, env, "0.00")
	ctx := context.Background()

	tx := newExternal(t, uuid.New(), wager.KindBet, walletID, playerID, "tx-1", "10.00", "", t0)
	if err := env.TM.Do(ctx, func(ctx context.Context, r postgres.Repositories) error { return r.Transactions.Insert(ctx, tx) }); err != nil {
		t.Fatal(err)
	}
	// duas "instâncias" leram a mesma transação pendente
	a, _ := env.TM.Reader().Transactions.GetByID(ctx, tx.ID())
	b, _ := env.TM.Reader().Transactions.GetByID(ctx, tx.ID())
	if err := a.Reject(wager.FailureInsufficientFunds, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := b.Fail(wager.FailureInternalError, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	if err := env.TM.Do(ctx, func(ctx context.Context, r postgres.Repositories) error { return r.Transactions.Save(ctx, a) }); err != nil {
		t.Fatalf("first save: %v", err)
	}
	err := env.TM.Do(ctx, func(ctx context.Context, r postgres.Repositories) error { return r.Transactions.Save(ctx, b) })
	if !errors.Is(err, postgres.ErrStateConflict) {
		t.Fatalf("second save: error = %v, want ErrStateConflict", err)
	}
	final, _ := env.TM.Reader().Transactions.GetByID(ctx, tx.ID())
	if final.Status() != wager.StatusRejected {
		t.Errorf("the terminal state of the first writer must win, got %s", final.Status())
	}
}

func TestWalletSave_StaleVersionConflicts(t *testing.T) {
	env := newEnv(t, nil)
	walletID, playerID := openWallet(t, env, "100.00")
	ctx := context.Background()

	stale, _ := env.TM.Reader().Wallets.GetByID(ctx, walletID)
	if _, err := placeBet(env, walletID, playerID, "tx-1", "10.00", false, t); err != nil { // avança a carteira para a versão 2
		t.Fatal(err)
	}
	if _, err := stale.Debit(wallet.MovementParams{EntryID: uuid.New(), TransactionID: uuid.New(), Amount: brl(t, "10.00"), Now: t0.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	err := env.TM.Do(ctx, func(ctx context.Context, r postgres.Repositories) error { return r.Wallets.Save(ctx, stale) })
	if !errors.Is(err, postgres.ErrVersionConflict) {
		t.Fatalf("error = %v, want ErrVersionConflict", err)
	}
	w, _ := assertLedgerConsistent(t, env, walletID)
	if w.Balance().Amount() != "90.00" || w.Version() != 2 {
		t.Errorf("balance %v version %d", w.Balance(), w.Version())
	}
}

// Cenário central do enunciado: 100.00 de saldo e duas apostas simultâneas de 80.00.
// Repetido muitas vezes para exercitar interleavings diferentes.
func TestConcurrentBets_TwoBetsOf80OnBalanceOf100(t *testing.T) {
	env := newEnv(t, nil)
	for iter := 0; iter < 30; iter++ {
		walletID, playerID := openWallet(t, env, "100.00")
		type out struct {
			res betResult
			err error
		}
		results := runConcurrently(2, func(i int) out {
			res, err := placeBet(env, walletID, playerID, "bet-"+uuid.NewString(), "80.00", iter%2 == 0, t)
			return out{res, err}
		})
		processed, rejected := 0, 0
		for _, r := range results {
			if r.err != nil {
				t.Fatalf("iteration %d: unexpected error: %v", iter, r.err)
			}
			switch {
			case r.res.Status == wager.StatusProcessed:
				processed++
			case r.res.Status == wager.StatusRejected && r.res.Code == wager.FailureInsufficientFunds:
				rejected++
			}
		}
		if processed != 1 || rejected != 1 {
			t.Fatalf("iteration %d: processed=%d rejected=%d, want 1 and 1", iter, processed, rejected)
		}
		w, entries := assertLedgerConsistent(t, env, walletID)
		if w.Balance().Amount() != "20.00" || w.Version() != 2 || len(entries) != 2 {
			t.Fatalf("iteration %d: balance %v version %d entries %d", iter, w.Balance(), w.Version(), len(entries))
		}
	}
}

// Muitas apostas simultâneas: o saldo nunca fica negativo, nada é debitado em dobro
// e o ledger reconstrói exatamente o saldo. Metade das iterações grava a transação
// antes de travar a carteira para provar que não há deadlock com as chaves estrangeiras.
func TestConcurrentBets_NeverOverdrawAndLedgerStaysExact(t *testing.T) {
	env := newEnv(t, nil)
	for _, insertFirst := range []bool{false, true} {
		walletID, playerID := openWallet(t, env, "100.00")
		const bets = 30 // 30 x 10.00 contra 100.00
		type out struct {
			res betResult
			err error
		}
		results := runConcurrently(bets, func(i int) out {
			res, err := placeBet(env, walletID, playerID, "bet-"+uuid.NewString(), "10.00", insertFirst, t)
			return out{res, err}
		})
		processed, rejected := 0, 0
		for _, r := range results {
			if r.err != nil {
				t.Fatalf("insertFirst=%v: unexpected error: %v", insertFirst, r.err)
			}
			if r.res.Status == wager.StatusProcessed {
				processed++
			} else if r.res.Code == wager.FailureInsufficientFunds {
				rejected++
			}
		}
		if processed != 10 || rejected != 20 {
			t.Fatalf("insertFirst=%v: processed=%d rejected=%d, want 10 and 20", insertFirst, processed, rejected)
		}
		w, entries := assertLedgerConsistent(t, env, walletID)
		if !w.Balance().IsZero() || w.Version() != 11 || len(entries) != 11 {
			t.Fatalf("insertFirst=%v: balance %v version %d entries %d", insertFirst, w.Balance(), w.Version(), len(entries))
		}
	}
}

// A mesma operação (mesmo provedor + id externo) enviada por várias instâncias ao
// mesmo tempo: o banco deixa passar UMA e barra as demais; o saldo é debitado uma vez.
func TestConcurrentSameExternalTransaction_AppliesOnce(t *testing.T) {
	env := newEnv(t, nil)
	walletID, playerID := openWallet(t, env, "100.00")

	errs := runConcurrently(12, func(i int) error {
		_, err := placeBet(env, walletID, playerID, "same-tx", "10.00", false, t)
		return err
	})
	ok, dup := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, postgres.ErrDuplicateExternalID):
			dup++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || dup != 11 {
		t.Fatalf("applied=%d duplicates=%d, want 1 and 11", ok, dup)
	}
	w, entries := assertLedgerConsistent(t, env, walletID)
	if w.Balance().Amount() != "90.00" || len(entries) != 2 {
		t.Errorf("balance %v entries %d", w.Balance(), len(entries))
	}
}

func TestReversalUniqueness(t *testing.T) {
	env := newEnv(t, nil)
	walletID, playerID := openWallet(t, env, "100.00")
	ctx := context.Background()
	if _, err := placeBet(env, walletID, playerID, "bet-1", "10.00", false, t); err != nil {
		t.Fatal(err)
	}
	bet, err := env.TM.Reader().Transactions.GetByProviderExternalID(ctx, "provider-a", "bet-1")
	if err != nil {
		t.Fatal(err)
	}
	if has, err := env.TM.Reader().Transactions.HasProcessedReversal(ctx, bet.ID()); err != nil || has {
		t.Fatalf("HasProcessedReversal before = %v, %v", has, err)
	}

	reverse := func(kind wager.Kind, ext string) error {
		return env.TM.Do(ctx, func(ctx context.Context, r postgres.Repositories) error {
			tx := newExternal(t, uuid.New(), kind, walletID, playerID, ext, "10.00", "bet-1", t0)
			if err := r.Transactions.Insert(ctx, tx); err != nil {
				return err
			}
			if err := tx.Process(wager.ProcessParams{ResultBalance: brl(t, "100.00"), ReferenceID: bet.ID(), Now: t0.Add(time.Second)}); err != nil {
				return err
			}
			return r.Transactions.Save(ctx, tx)
		})
	}
	if err := reverse(wager.KindRefund, "refund-1"); err != nil {
		t.Fatalf("first reversal: %v", err)
	}
	if has, _ := env.TM.Reader().Transactions.HasProcessedReversal(ctx, bet.ID()); !has {
		t.Error("HasProcessedReversal after = false")
	}
	for _, kind := range []wager.Kind{wager.KindRefund, wager.KindRollback} {
		if err := reverse(kind, "again-"+string(kind)); !errors.Is(err, postgres.ErrReversalExists) {
			t.Errorf("%s again: error = %v, want ErrReversalExists", kind, err)
		}
	}
}

func TestClaimDue_SkipLockedGivesDisjointBatches(t *testing.T) {
	env := newEnv(t, nil)
	walletID, playerID := openWallet(t, env, "0.00")
	ctx := context.Background()

	for i := 0; i < 6; i++ {
		tx := newExternal(t, uuid.New(), wager.KindBet, walletID, playerID, "p-"+string(rune('a'+i)), "1.00", "", t0)
		if err := env.TM.Do(ctx, func(ctx context.Context, r postgres.Repositories) error { return r.Transactions.Insert(ctx, tx) }); err != nil {
			t.Fatal(err)
		}
	}
	// nada vence antes de t0
	if got, err := env.TM.Reader().Transactions.ClaimDue(ctx, t0.Add(-time.Second), 10); err != nil || len(got) != 0 {
		t.Fatalf("not due yet: %d, %v", len(got), err)
	}

	claimed := make(chan []*wager.Transaction, 1)
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // worker A: reivindica 3 e SEGURA a transação aberta
		defer wg.Done()
		_ = env.TM.Do(ctx, func(ctx context.Context, r postgres.Repositories) error {
			got, err := r.Transactions.ClaimDue(ctx, t0.Add(time.Second), 3)
			if err != nil {
				t.Error(err)
			}
			claimed <- got
			<-release
			return nil
		})
	}()
	a := <-claimed

	var b []*wager.Transaction
	if err := env.TM.Do(ctx, func(ctx context.Context, r postgres.Repositories) error { // worker B enquanto A segura os locks
		var err error
		b, err = r.Transactions.ClaimDue(ctx, t0.Add(time.Second), 10)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	wg.Wait()

	if len(a) != 3 || len(b) != 3 {
		t.Fatalf("A claimed %d, B claimed %d, want 3 and 3", len(a), len(b))
	}
	seen := map[uuid.UUID]bool{}
	for _, tx := range append(a, b...) {
		if seen[tx.ID()] {
			t.Fatalf("transaction %s was claimed by both workers", tx.ID())
		}
		seen[tx.ID()] = true
	}
	// terminal não é reivindicado de novo
	done, _ := env.TM.Reader().Transactions.GetByID(ctx, a[0].ID())
	if err := done.Reject(wager.FailureInsufficientFunds, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := env.TM.Do(ctx, func(ctx context.Context, r postgres.Repositories) error { return r.Transactions.Save(ctx, done) }); err != nil {
		t.Fatal(err)
	}
	rest, _ := env.TM.Reader().Transactions.ClaimDue(ctx, t0.Add(time.Hour), 10)
	if len(rest) != 5 {
		t.Errorf("after one rejection, %d pending remain, want 5", len(rest))
	}
}

func TestLockTimeout_IsTransient(t *testing.T) {
	env := newEnv(t, func(c *postgres.Config) { c.LockTimeout = 300 * time.Millisecond })
	walletID, _ := openWallet(t, env, "100.00")
	ctx := context.Background()

	holding := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = env.TM.Do(ctx, func(ctx context.Context, r postgres.Repositories) error {
			if _, err := r.Wallets.GetForUpdate(ctx, walletID); err != nil {
				t.Error(err)
			}
			close(holding)
			<-release
			return nil
		})
	}()
	<-holding

	start := time.Now()
	err := env.TM.Do(ctx, func(ctx context.Context, r postgres.Repositories) error {
		_, err := r.Wallets.GetForUpdate(ctx, walletID)
		return err
	})
	elapsed := time.Since(start)
	close(release)
	wg.Wait()

	var pe *pgconn.PgError
	if err == nil || !errors.As(err, &pe) || pe.Code != "55P03" {
		t.Fatalf("error = %v, want SQLSTATE 55P03 (lock_not_available)", err)
	}
	if !postgres.IsTransient(err) {
		t.Error("a lock timeout is a transient failure")
	}
	if elapsed > 3*time.Second {
		t.Errorf("waited %v: the lock timeout did not bound the wait", elapsed)
	}
}

func TestDo_RetriesDeadlockAndSerializationFailures(t *testing.T) {
	env := newEnv(t, func(c *postgres.Config) { c.MaxTxRetries = 3 })
	ctx := context.Background()

	for _, code := range []string{"40001", "40P01"} {
		attempts := 0
		err := env.TM.Do(ctx, func(ctx context.Context, r postgres.Repositories) error {
			attempts++
			if attempts < 3 {
				return &pgconn.PgError{Code: code}
			}
			return nil
		})
		if err != nil || attempts != 3 {
			t.Errorf("%s: attempts=%d err=%v, want 3 and nil", code, attempts, err)
		}
	}

	// esgotou as repetições: 1 tentativa + 3 repetições
	attempts := 0
	err := env.TM.Do(ctx, func(ctx context.Context, r postgres.Repositories) error {
		attempts++
		return &pgconn.PgError{Code: "40001"}
	})
	if err == nil || attempts != 4 {
		t.Errorf("exhausted: attempts=%d err=%v, want 4 attempts and an error", attempts, err)
	}

	// erro não repetível: devolvido na hora
	attempts = 0
	boom := errors.New("boom")
	if err := env.TM.Do(ctx, func(ctx context.Context, r postgres.Repositories) error { attempts++; return boom }); !errors.Is(err, boom) || attempts != 1 {
		t.Errorf("non-retryable: attempts=%d err=%v", attempts, err)
	}
}

func TestDo_CanceledContextRollsBack(t *testing.T) {
	env := newEnv(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	walletID := uuid.New()
	err := env.TM.Do(ctx, func(ctx context.Context, r postgres.Repositories) error {
		w, _, _ := wallet.Open(wallet.OpenParams{WalletID: walletID, PlayerID: uuid.New(), InitialBalance: brl(t, "0.00"), Now: t0})
		if err := r.Wallets.Insert(ctx, w); err != nil {
			return err
		}
		cancel() // o cliente desistiu no meio da transação
		return ctx.Err()
	})
	if !postgres.IsTransient(err) {
		t.Errorf("error = %v: a canceled context must be classified as transient", err)
	}
	if _, err := env.TM.Reader().Wallets.GetByID(context.Background(), walletID); !errors.Is(err, postgres.ErrNotFound) {
		t.Errorf("the insert must have been rolled back: %v", err)
	}
}

func TestLedgerList_Pagination(t *testing.T) {
	env := newEnv(t, nil)
	walletID, playerID := openWallet(t, env, "100.00")
	for i := 0; i < 5; i++ {
		if _, err := placeBet(env, walletID, playerID, "b-"+string(rune('a'+i)), "1.00", false, t); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	var got []int64
	var cursor int64
	pages := 0
	for {
		page, err := env.TM.Reader().Ledger.List(ctx, walletID, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		pages++
		for _, e := range page {
			got = append(got, e.WalletVersion())
		}
		cursor = page[len(page)-1].WalletVersion()
	}
	want := []int64{1, 2, 3, 4, 5, 6}
	if pages != 3 || len(got) != len(want) {
		t.Fatalf("pages=%d versions=%v", pages, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("versions = %v, want %v", got, want)
		}
	}
	if _, err := env.TM.Reader().Ledger.List(ctx, walletID, 0, 0); err == nil {
		t.Error("a non-positive limit must be rejected")
	}
}

func TestLedgerIsImmutableThroughTheRepositoryPath(t *testing.T) {
	env := newEnv(t, nil)
	walletID, _ := openWallet(t, env, "100.00")
	ctx := context.Background()
	for _, stmt := range []string{
		`UPDATE wallet_ledger_entries SET amount = amount`,
		`DELETE FROM wallet_ledger_entries`,
		`TRUNCATE wallet_ledger_entries`,
	} {
		if _, err := env.Pool.Exec(ctx, stmt); err == nil {
			t.Errorf("%q must be rejected by the schema", stmt)
		}
	}
	assertLedgerConsistent(t, env, walletID)
}
