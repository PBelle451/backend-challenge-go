package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/PBelle451/backend-challenge-go/internal/domain/wager"
	"github.com/PBelle451/backend-challenge-go/internal/domain/wallet"
	"github.com/PBelle451/backend-challenge-go/internal/infra/postgres"
)

func hashOf(b byte) wager.PayloadHash {
	var h wager.PayloadHash
	for i := range h {
		h[i] = b
	}
	return h
}

func inboxMsg(consumer, messageID string, hash wager.PayloadHash, completed bool) postgres.InboxMessage {
	m := postgres.InboxMessage{ID: uuid.New(), ConsumerName: consumer, MessageID: messageID, PayloadHash: hash, ReceivedAt: t0}
	if completed {
		m.CompletedAt = t0.Add(time.Second)
	}
	return m
}

func event(aggregate uuid.UUID, typ string) postgres.OutboxEvent {
	return postgres.OutboxEvent{
		EventID: uuid.New(), EventType: typ, EventVersion: 1, AggregateType: "WALLET", AggregateID: aggregate,
		CorrelationID: "corr-" + uuid.NewString(), CausationID: "msg-1",
		Payload:    json.RawMessage(`{"walletId":"w-1","amount":{"amount":"10.00","currency":"BRL"}}`),
		OccurredAt: t0, CreatedAt: t0,
	}
}

func insertEvents(t *testing.T, env *testEnv, evs ...postgres.OutboxEvent) {
	t.Helper()
	err := env.TM.Do(context.Background(), func(ctx context.Context, r postgres.Repositories) error {
		for _, e := range evs {
			if err := r.Outbox.Insert(ctx, e); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("insert events: %v", err)
	}
}

// ---------------------------------------------------------------- inbox

func TestInbox_RegisterAndDuplicates(t *testing.T) {
	env := newEnv(t, nil)
	ctx := context.Background()
	reg := func(m postgres.InboxMessage) postgres.InboxOutcome {
		var out postgres.InboxOutcome
		if err := env.TM.Do(ctx, func(ctx context.Context, r postgres.Repositories) (err error) {
			out, err = r.Inbox.Register(ctx, m)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}

	if got := reg(inboxMsg("sqs", "msg-1", hashOf(1), true)); got != postgres.InboxNew {
		t.Errorf("first delivery = %s", got)
	}
	if got := reg(inboxMsg("sqs", "msg-1", hashOf(1), true)); got != postgres.InboxDuplicate {
		t.Errorf("redelivery with the same content = %s", got)
	}
	if got := reg(inboxMsg("sqs", "msg-1", hashOf(2), true)); got != postgres.InboxHashMismatch {
		t.Errorf("same messageId with other content = %s", got)
	}
	if got := reg(inboxMsg("outro-consumidor", "msg-1", hashOf(1), true)); got != postgres.InboxNew {
		t.Errorf("same messageId for ANOTHER consumer = %s", got)
	}
	if got := reg(inboxMsg("sqs", "msg-2", hashOf(1), false)); got != postgres.InboxNew {
		t.Errorf("a different message = %s", got)
	}

	// completed_at: gravada quando informada, NULL quando não
	var withDone, withoutDone int
	_ = env.Pool.QueryRow(ctx, `SELECT count(*) FROM inbox_messages WHERE completed_at IS NOT NULL`).Scan(&withDone)
	_ = env.Pool.QueryRow(ctx, `SELECT count(*) FROM inbox_messages WHERE completed_at IS NULL`).Scan(&withoutDone)
	if withDone != 2 || withoutDone != 1 {
		t.Errorf("completed=%d pending=%d, want 2 and 1", withDone, withoutDone)
	}

	// entradas malformadas
	for name, m := range map[string]postgres.InboxMessage{
		"sem consumidor": inboxMsg("", "m", hashOf(1), true),
		"sem messageId":  inboxMsg("sqs", "", hashOf(1), true),
		"sem hash":       inboxMsg("sqs", "m", wager.PayloadHash{}, true),
		"sem id":         {ConsumerName: "sqs", MessageID: "m", PayloadHash: hashOf(1), ReceivedAt: t0},
	} {
		err := env.TM.Do(ctx, func(ctx context.Context, r postgres.Repositories) error {
			_, err := r.Inbox.Register(ctx, m)
			return err
		})
		if err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// A mesma mensagem entregue a várias instâncias ao mesmo tempo: o efeito é aplicado uma vez.
func TestInbox_ConcurrentSameMessageAppliesEffectOnce(t *testing.T) {
	env := newEnv(t, nil)
	aggregate := uuid.New()
	var effects atomic.Int32

	outcomes := runConcurrently(12, func(i int) postgres.InboxOutcome {
		var out postgres.InboxOutcome
		err := env.TM.Do(context.Background(), func(ctx context.Context, r postgres.Repositories) error {
			var err error
			if out, err = r.Inbox.Register(ctx, inboxMsg("sqs", "same-message", hashOf(7), true)); err != nil {
				return err
			}
			if out != postgres.InboxNew {
				return nil // duplicada: nenhum efeito
			}
			effects.Add(1)
			return r.Outbox.Insert(ctx, event(aggregate, "WalletBalanceChanged")) // o "efeito" de domínio
		})
		if err != nil {
			t.Errorf("worker %d: %v", i, err)
		}
		return out
	})

	news, dups := 0, 0
	for _, o := range outcomes {
		switch o {
		case postgres.InboxNew:
			news++
		case postgres.InboxDuplicate:
			dups++
		}
	}
	var stored int
	_ = env.Pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events`).Scan(&stored)
	if news != 1 || dups != 11 || effects.Load() != 1 || stored != 1 {
		t.Fatalf("new=%d duplicate=%d effects=%d events=%d, want 1, 11, 1, 1", news, dups, effects.Load(), stored)
	}
}

// Se a transação é desfeita, a mensagem NÃO fica registrada: a reentrega é reprocessada.
func TestInbox_RollbackAllowsReprocessing(t *testing.T) {
	env := newEnv(t, nil)
	ctx := context.Background()
	m := inboxMsg("sqs", "retry-me", hashOf(3), true)

	err := env.TM.Do(ctx, func(ctx context.Context, r postgres.Repositories) error {
		if out, err := r.Inbox.Register(ctx, m); err != nil || out != postgres.InboxNew {
			return fmt.Errorf("register: %v %v", out, err)
		}
		return errors.New("falha de processamento depois do registro")
	})
	if err == nil {
		t.Fatal("expected the processing failure")
	}
	var out postgres.InboxOutcome
	if err := env.TM.Do(ctx, func(ctx context.Context, r postgres.Repositories) (err error) {
		out, err = r.Inbox.Register(ctx, inboxMsg("sqs", "retry-me", hashOf(3), true))
		return err
	}); err != nil || out != postgres.InboxNew {
		t.Fatalf("after the rollback the redelivery must be NEW: %v, %v", out, err)
	}
}

// ---------------------------------------------------------------- outbox

func TestOutbox_InsertAndGet(t *testing.T) {
	env := newEnv(t, nil)
	ctx := context.Background()
	e := event(uuid.New(), "WagerTransactionProcessed")
	insertEvents(t, env, e)

	got, err := env.TM.Reader().Outbox.Get(ctx, e.EventID)
	if err != nil {
		t.Fatal(err)
	}
	var want, have map[string]any
	_ = json.Unmarshal(e.Payload, &want)
	if err := json.Unmarshal(got.Payload, &have); err != nil || fmt.Sprint(want) != fmt.Sprint(have) {
		t.Errorf("payload = %s, want %s", got.Payload, e.Payload)
	}
	if got.EventType != e.EventType || got.AggregateID != e.AggregateID || got.CorrelationID != e.CorrelationID ||
		got.CausationID != "msg-1" || got.EventVersion != 1 || got.Seq == 0 ||
		!got.OccurredAt.Equal(t0) || !got.NextAttemptAt.Equal(t0) {
		t.Errorf("event = %+v", got)
	}
	if got.Attempts != 0 || got.LockedBy != "" || !got.LockedUntil.IsZero() || !got.PublishedAt.IsZero() || got.LastError != "" {
		t.Errorf("a new event must be untouched: %+v", got)
	}
	if _, err := env.TM.Reader().Outbox.Get(ctx, uuid.New()); !errors.Is(err, postgres.ErrNotFound) {
		t.Errorf("Get unknown: %v", err)
	}

	ins := func(ev postgres.OutboxEvent) error {
		return env.TM.Do(ctx, func(ctx context.Context, r postgres.Repositories) error { return r.Outbox.Insert(ctx, ev) })
	}
	if err := ins(e); !errors.Is(err, postgres.ErrDuplicateEvent) {
		t.Errorf("duplicate event id: %v", err)
	}
	bad := event(uuid.New(), "TipoInventado")
	if err := ins(bad); !errors.Is(err, postgres.ErrIntegrity) {
		t.Errorf("unknown event type: %v", err)
	}
	for name, mod := range map[string]func(ev *postgres.OutboxEvent){
		"payload que nao e objeto": func(ev *postgres.OutboxEvent) { ev.Payload = json.RawMessage(`[1,2]`) },
		"payload vazio":            func(ev *postgres.OutboxEvent) { ev.Payload = nil },
		"payload invalido":         func(ev *postgres.OutboxEvent) { ev.Payload = json.RawMessage(`{oops`) },
		"payload null":             func(ev *postgres.OutboxEvent) { ev.Payload = json.RawMessage(`null`) },
		"sem correlationId":        func(ev *postgres.OutboxEvent) { ev.CorrelationID = "" },
		"sem agregado":             func(ev *postgres.OutboxEvent) { ev.AggregateID = uuid.Nil },
		"versao zero":              func(ev *postgres.OutboxEvent) { ev.EventVersion = 0 },
	} {
		ev := event(uuid.New(), "WalletBalanceChanged")
		mod(&ev)
		if err := ins(ev); !errors.Is(err, postgres.ErrInvalidEvent) {
			t.Errorf("%s: error = %v, want ErrInvalidEvent", name, err)
		}
	}
}

// O evento nasce e morre junto com a transação do fato que o originou.
func TestOutbox_IsAtomicWithTheDomainChange(t *testing.T) {
	env := newEnv(t, nil)
	ctx := context.Background()
	walletID := uuid.New()
	ev := event(walletID, "WalletBalanceChanged")

	do := func(fail bool) error {
		return env.TM.Do(ctx, func(ctx context.Context, r postgres.Repositories) error {
			w, _, _ := wallet.Open(wallet.OpenParams{WalletID: walletID, PlayerID: uuid.New(), InitialBalance: brl(t, "0.00"), Now: t0})
			if err := r.Wallets.Insert(ctx, w); err != nil {
				return err
			}
			if err := r.Outbox.Insert(ctx, ev); err != nil {
				return err
			}
			if fail {
				return errors.New("falha depois de gravar o evento")
			}
			return nil
		})
	}
	if err := do(true); err == nil {
		t.Fatal("expected the failure")
	}
	if _, err := env.TM.Reader().Outbox.Get(ctx, ev.EventID); !errors.Is(err, postgres.ErrNotFound) {
		t.Errorf("a rolled-back transaction must not leave an event: %v", err)
	}
	if err := do(false); err != nil {
		t.Fatal(err)
	}
	if _, err := env.TM.Reader().Outbox.Get(ctx, ev.EventID); err != nil {
		t.Errorf("a committed transaction must have its event: %v", err)
	}
	if _, err := env.TM.Reader().Wallets.GetByID(ctx, walletID); err != nil {
		t.Errorf("and its wallet: %v", err)
	}
}

func TestOutbox_ClaimLeaseAndPublish(t *testing.T) {
	env := newEnv(t, nil)
	ctx := context.Background()
	out := env.TM.Reader().Outbox
	now := t0.Add(time.Hour)

	e1, e2, e3 := event(uuid.New(), "WalletBalanceChanged"), event(uuid.New(), "WalletBalanceChanged"), event(uuid.New(), "WalletBalanceChanged")
	insertEvents(t, env, e1, e2, e3)

	a, err := out.Claim(ctx, "publisher-A", now, time.Minute, 2)
	if err != nil || len(a) != 2 || a[0].EventID != e1.EventID || a[1].EventID != e2.EventID || a[0].Seq >= a[1].Seq {
		t.Fatalf("A claimed %d events (%v), want e1 and e2 in order", len(a), err)
	}
	if a[0].Attempts != 1 || a[0].LockedBy != "publisher-A" || !a[0].LockedUntil.Equal(now.Add(time.Minute)) {
		t.Errorf("claimed event = %+v", a[0])
	}
	b, err := out.Claim(ctx, "publisher-B", now, time.Minute, 10)
	if err != nil || len(b) != 1 || b[0].EventID != e3.EventID {
		t.Fatalf("B must get only the remaining event, got %d (%v)", len(b), err)
	}

	// só o dono do lease marca como publicado
	if err := out.MarkPublished(ctx, e1.EventID, "publisher-B", now); !errors.Is(err, postgres.ErrLeaseLost) {
		t.Errorf("MarkPublished by a stranger: %v", err)
	}
	if err := out.MarkPublished(ctx, e1.EventID, "publisher-A", now); err != nil {
		t.Fatal(err)
	}
	if err := out.MarkPublished(ctx, e1.EventID, "publisher-A", now); !errors.Is(err, postgres.ErrLeaseLost) {
		t.Errorf("publishing twice must be refused: %v", err)
	}
	got, _ := out.Get(ctx, e1.EventID)
	if got.PublishedAt.IsZero() || got.LockedBy != "" || !got.LockedUntil.IsZero() {
		t.Errorf("published event = %+v", got)
	}
	// publicado não é reivindicado de novo, nem depois de o lease original vencer
	if c, _ := out.Claim(ctx, "publisher-C", now.Add(time.Hour), time.Minute, 10); len(c) != 2 {
		t.Errorf("after the leases expire only e2 and e3 are claimable again, got %d", len(c))
	}

	pending, oldest, err := out.PendingStats(ctx)
	if err != nil || pending != 2 || !oldest.Equal(t0) {
		t.Errorf("PendingStats = %d, %v, %v", pending, oldest, err)
	}
}

// Publisher que cai: o lease vence e outro assume o trabalho abandonado.
func TestOutbox_ExpiredLeaseIsRecoveredByAnotherPublisher(t *testing.T) {
	env := newEnv(t, nil)
	ctx := context.Background()
	out := env.TM.Reader().Outbox
	e := event(uuid.New(), "WalletBalanceChanged")
	insertEvents(t, env, e)

	start := t0.Add(time.Hour)
	if got, _ := out.Claim(ctx, "publisher-A", start, time.Second, 5); len(got) != 1 {
		t.Fatal("A must claim the event")
	}
	// A "morreu". Antes de o lease vencer, ninguém mais pode pegá-lo.
	if got, _ := out.Claim(ctx, "publisher-B", start.Add(500*time.Millisecond), time.Minute, 5); len(got) != 0 {
		t.Fatalf("an event under a live lease was stolen: %d", len(got))
	}
	got, err := out.Claim(ctx, "publisher-B", start.Add(2*time.Second), time.Minute, 5)
	if err != nil || len(got) != 1 || got[0].EventID != e.EventID || got[0].Attempts != 2 || got[0].LockedBy != "publisher-B" {
		t.Fatalf("B must recover the abandoned event with attempts=2: %+v, %v", got, err)
	}
	// o A "ressuscitado" não pode mais confirmar nem falhar o evento
	if err := out.MarkPublished(ctx, e.EventID, "publisher-A", start.Add(3*time.Second)); !errors.Is(err, postgres.ErrLeaseLost) {
		t.Errorf("zombie MarkPublished: %v", err)
	}
	if err := out.MarkFailed(ctx, e.EventID, "publisher-A", start, errors.New("x")); !errors.Is(err, postgres.ErrLeaseLost) {
		t.Errorf("zombie MarkFailed: %v", err)
	}
	if err := out.MarkPublished(ctx, e.EventID, "publisher-B", start.Add(3*time.Second)); err != nil {
		t.Errorf("the new owner must be able to publish: %v", err)
	}
}

func TestOutbox_FailureBackoffAndErrorTruncation(t *testing.T) {
	env := newEnv(t, nil)
	ctx := context.Background()
	out := env.TM.Reader().Outbox
	e := event(uuid.New(), "WalletBalanceChanged")
	insertEvents(t, env, e)

	now := t0.Add(time.Hour)
	if got, _ := out.Claim(ctx, "p", now, time.Minute, 1); len(got) != 1 {
		t.Fatal("claim")
	}
	longErr := errors.New(strings.Repeat("é", 2000)) // 4000 bytes, caracteres de 2 bytes
	retryAt := now.Add(10 * time.Second)
	if err := out.MarkFailed(ctx, e.EventID, "p", retryAt, longErr); err != nil {
		t.Fatal(err)
	}
	got, _ := out.Get(ctx, e.EventID)
	if got.LockedBy != "" || !got.NextAttemptAt.Equal(retryAt) || got.Attempts != 1 ||
		len(got.LastError) > 1000 || !utf8.ValidString(got.LastError) || got.LastError == "" {
		t.Errorf("failed event: lockedBy=%q next=%v attempts=%d lastError=%d bytes valid=%v",
			got.LockedBy, got.NextAttemptAt, got.Attempts, len(got.LastError), utf8.ValidString(got.LastError))
	}

	if c, _ := out.Claim(ctx, "p", now.Add(5*time.Second), time.Minute, 1); len(c) != 0 {
		t.Error("the event must wait for its backoff")
	}
	c, _ := out.Claim(ctx, "p", retryAt, time.Minute, 1)
	if len(c) != 1 || c[0].Attempts != 2 {
		t.Fatalf("after the backoff the event is claimable again with attempts=2: %+v", c)
	}
	if err := out.MarkPublished(ctx, e.EventID, "p", retryAt); err != nil {
		t.Fatal(err)
	}
	if done, _ := out.Get(ctx, e.EventID); done.LastError != "" {
		t.Errorf("a published event clears its last error: %q", done.LastError)
	}
}

// Ordem por agregado: um evento só é reivindicável depois que os anteriores do mesmo agregado foram publicados.
func TestOutbox_PerAggregateOrdering(t *testing.T) {
	env := newEnv(t, nil)
	ctx := context.Background()
	out := env.TM.Reader().Outbox
	now := t0.Add(time.Hour)

	aggA, aggB := uuid.New(), uuid.New()
	a1, a2, a3 := event(aggA, "WalletBalanceChanged"), event(aggA, "WalletBalanceChanged"), event(aggA, "WalletBalanceChanged")
	b1 := event(aggB, "WalletBalanceChanged")
	insertEvents(t, env, a1, a2, a3, b1)

	first, _ := out.Claim(ctx, "p1", now, time.Minute, 10)
	if len(first) != 2 || first[0].EventID != a1.EventID || first[1].EventID != b1.EventID {
		t.Fatalf("only the head of each aggregate is claimable, got %d events", len(first))
	}
	// com a cabeça de A em voo, outro publisher não pode furar a fila
	if other, _ := out.Claim(ctx, "p2", now, time.Minute, 10); len(other) != 0 {
		t.Fatalf("a2 must wait for a1: %d claimed", len(other))
	}
	if err := out.MarkPublished(ctx, a1.EventID, "p1", now); err != nil {
		t.Fatal(err)
	}
	next, _ := out.Claim(ctx, "p2", now, time.Minute, 10)
	if len(next) != 1 || next[0].EventID != a2.EventID {
		t.Fatalf("after a1, only a2 may be claimed (not a3): %+v", next)
	}
	// uma falha na cabeça segura a fila do agregado até a próxima tentativa
	if err := out.MarkFailed(ctx, a2.EventID, "p2", now.Add(time.Minute), errors.New("sqs indisponivel")); err != nil {
		t.Fatal(err)
	}
	if stuck, _ := out.Claim(ctx, "p3", now.Add(10*time.Second), time.Minute, 10); len(stuck) != 0 {
		t.Fatalf("a3 must keep waiting while a2 is not published: %d", len(stuck))
	}
}

// Vários publishers ao mesmo tempo, com falhas intermitentes: cada evento é
// publicado com sucesso exatamente uma vez e, dentro de cada agregado, em ordem.
func TestOutbox_ConcurrentPublishersPublishEachEventOnceAndInOrder(t *testing.T) {
	env := newEnv(t, nil)
	ctx := context.Background()
	const aggregates, perAggregate, publishers = 6, 10, 5

	var all []postgres.OutboxEvent
	aggIDs := make([]uuid.UUID, aggregates)
	for a := range aggIDs {
		aggIDs[a] = uuid.New()
		for i := 0; i < perAggregate; i++ {
			all = append(all, event(aggIDs[a], "WalletBalanceChanged"))
		}
	}
	insertEvents(t, env, all...)

	var mu sync.Mutex
	published := map[uuid.UUID]int{}
	order := map[uuid.UUID][]int64{}
	var sends atomic.Int32

	deadline := time.Now().Add(40 * time.Second)
	runConcurrently(publishers, func(p int) struct{} {
		id := fmt.Sprintf("publisher-%d", p)
		out := env.TM.Reader().Outbox
		for time.Now().Before(deadline) {
			batch, err := out.Claim(ctx, id, time.Now(), 30*time.Second, 4)
			if err != nil {
				t.Errorf("%s claim: %v", id, err)
				return struct{}{}
			}
			if len(batch) == 0 {
				pending, _, _ := out.PendingStats(ctx)
				if pending == 0 {
					return struct{}{}
				}
				time.Sleep(3 * time.Millisecond)
				continue
			}
			for _, e := range batch {
				if sends.Add(1)%7 == 0 { // o SQS "falha" de vez em quando
					if err := out.MarkFailed(ctx, e.EventID, id, time.Now(), errors.New("sqs indisponivel")); err != nil {
						t.Errorf("%s MarkFailed: %v", id, err)
					}
					continue
				}
				mu.Lock()
				published[e.EventID]++
				order[e.AggregateID] = append(order[e.AggregateID], e.Seq)
				mu.Unlock()
				if err := out.MarkPublished(ctx, e.EventID, id, time.Now()); err != nil {
					t.Errorf("%s MarkPublished: %v", id, err)
				}
			}
		}
		return struct{}{}
	})

	for _, e := range all {
		if published[e.EventID] != 1 {
			t.Fatalf("event %s was published %d times, want exactly 1", e.EventID, published[e.EventID])
		}
	}
	for agg, seqs := range order {
		for i := 1; i < len(seqs); i++ {
			if seqs[i] <= seqs[i-1] {
				t.Fatalf("aggregate %s published out of order: %v", agg, seqs)
			}
		}
		if len(seqs) != perAggregate {
			t.Fatalf("aggregate %s published %d events, want %d", agg, len(seqs), perAggregate)
		}
	}
	if pending, _, _ := env.TM.Reader().Outbox.PendingStats(ctx); pending != 0 {
		t.Errorf("%d events still pending", pending)
	}
}

// ---------------------------------------------------------------- reconciliação

func TestLedgerSummaryAndDivergenceDetection(t *testing.T) {
	env := newEnv(t, nil)
	ctx := context.Background()
	walletID, playerID := openWallet(t, env, "100.00")
	zeroWallet, _ := openWallet(t, env, "0.00")
	for i := 0; i < 3; i++ {
		if _, err := placeBet(env, walletID, playerID, fmt.Sprintf("bet-%d", i), "10.00", false, t); err != nil {
			t.Fatal(err)
		}
	}
	r := env.TM.Reader().Ledger

	s, err := r.Summary(ctx, walletID)
	if err != nil {
		t.Fatal(err)
	}
	if s.Currency != "BRL" || s.Credits.Amount() != "100.00" || s.Debits.Amount() != "30.00" || s.EntryCount != 4 || s.LastVersion != 4 {
		t.Errorf("summary = %+v", s)
	}
	z, err := r.Summary(ctx, zeroWallet)
	if err != nil || !z.Credits.IsZero() || !z.Debits.IsZero() || z.EntryCount != 0 || z.LastVersion != 0 {
		t.Errorf("empty wallet summary = %+v, %v", z, err)
	}
	if _, err := r.Summary(ctx, uuid.New()); !errors.Is(err, postgres.ErrNotFound) {
		t.Errorf("unknown wallet: %v", err)
	}

	if bad, err := r.FindDivergentWallets(ctx, 10); err != nil || len(bad) != 0 {
		t.Fatalf("a consistent database has no divergent wallets: %v, %v", bad, err)
	}

	// Corrompe o saldo DE PROPÓSITO, contornando as proteções (usuário de teste é superusuário):
	// a verificação independente precisa apontar exatamente essa carteira.
	for _, stmt := range []string{
		`ALTER TABLE wallets DISABLE TRIGGER USER`,
		fmt.Sprintf(`UPDATE wallets SET balance = balance + 1 WHERE id = '%s'`, walletID),
		`ALTER TABLE wallets ENABLE TRIGGER USER`,
	} {
		if _, err := env.Pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	bad, err := r.FindDivergentWallets(ctx, 10)
	if err != nil || len(bad) != 1 || bad[0] != walletID {
		t.Fatalf("divergent wallets = %v, %v; want only %s", bad, err, walletID)
	}
	if _, err := r.FindDivergentWallets(ctx, 0); err == nil {
		t.Error("a non-positive limit must be rejected")
	}
}
