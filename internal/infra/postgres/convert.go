package postgres

import (
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/PBelle451/backend-challenge-go/internal/domain/money"
)

// Money <-> (BIGINT em unidades mínimas, CHAR(3)). Sem float em nenhum ponto.
func moneyFrom(minor int64, currency string) (money.Money, error) {
	return money.New(minor, money.Currency(currency))
}

// UUID. O valor nulo do domínio (uuid.Nil) vira NULL no banco.
func pgUUID(u uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: u, Valid: u != uuid.Nil} }

func fromPgUUID(u pgtype.UUID) uuid.UUID {
	if !u.Valid {
		return uuid.Nil
	}
	return uuid.UUID(u.Bytes)
}

// Instantes: sempre UTC; o instante zero do domínio vira NULL.
func pgTime(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: !t.IsZero()}
}

func fromPgTime(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time.UTC()
}

// Texto opcional: string vazia vira NULL.
func pgText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

// BIGINT opcional.
func pgMoneyMinor(m money.Money) pgtype.Int8 {
	return pgtype.Int8{Int64: m.Minor(), Valid: m.IsValid()}
}
