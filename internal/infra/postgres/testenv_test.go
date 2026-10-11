package postgres_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/PBelle451/backend-challenge-go/internal/infra/postgres"
)

// Os testes de integração usam um PostgreSQL REAL (o do docker-compose), sem mocks.
//
//	TEST_POSTGRES_URL     URL de um usuário com CREATEDB (padrão: o Postgres do compose, porta 5433)
//	TEST_REQUIRE_INFRA=1  falha em vez de pular quando o banco não está acessível
//
// Cada teste cria um banco novo, aplica as migrations reais (migrations/*.up.sql) e o
// remove ao final, o que isola os testes entre si (o ledger e o outbox não aceitam TRUNCATE).
func adminURL() string {
	if v := os.Getenv("TEST_POSTGRES_URL"); v != "" {
		return v
	}
	return "postgres://wager:wager_local_pw@localhost:5433/postgres?sslmode=disable"
}

type testEnv struct {
	Pool *pgxpool.Pool
	TM   *postgres.TxManager
}

func newEnv(t *testing.T, mod func(*postgres.Config)) *testEnv {
	t.Helper()
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, adminURL())
	if err == nil {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = admin.Ping(pingCtx)
		cancel()
	}
	if err != nil {
		if os.Getenv("TEST_REQUIRE_INFRA") == "1" {
			t.Fatalf("PostgreSQL de teste inacessível (%s): %v", adminURL(), err)
		}
		t.Skipf("PostgreSQL de teste inacessível (defina TEST_POSTGRES_URL ou suba o docker compose): %v", err)
	}

	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	name := "wager_it_" + hex.EncodeToString(suffix)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create database: %v", err)
	}

	u, err := url.Parse(adminURL())
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	cfg := postgres.Config{URL: u.String(), MaxConns: 40}
	if mod != nil {
		mod(&cfg)
	}
	pool, err := postgres.NewPool(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		admin.Close()
	})

	files, err := filepath.Glob("../../../migrations/*.up.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("migrations não encontradas: %v", err)
	}
	sort.Strings(files)
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("aplicando %s: %v", f, err)
		}
	}
	return &testEnv{Pool: pool, TM: postgres.NewTxManager(pool, cfg)}
}
