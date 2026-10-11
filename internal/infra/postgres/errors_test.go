package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/PBelle451/backend-challenge-go/internal/infra/postgres"
)

func TestIsTransient(t *testing.T) {
	pg := func(code string) error { return &pgconn.PgError{Code: code} }
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"falha de serializacao", pg("40001"), true},
		{"deadlock", pg("40P01"), true},
		{"lock timeout", pg("55P03"), true},
		{"statement timeout / cancelada", pg("57014"), true},
		{"servidor desligando", pg("57P01"), true},
		{"crash do servidor", pg("57P02"), true},
		{"servidor iniciando", pg("57P03"), true},
		{"conexao perdida (08006)", pg("08006"), true},
		{"falha ao conectar (08001)", pg("08001"), true},
		{"muitas conexoes (53300)", pg("53300"), true},
		{"sem memoria (53200)", pg("53200"), true},
		{"context deadline", context.DeadlineExceeded, true},
		{"context canceled", context.Canceled, true},
		{"EOF", io.EOF, true},
		{"EOF inesperado", io.ErrUnexpectedEOF, true},
		{"erro de rede", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		{"embrulhado", fmt.Errorf("camada acima: %w", pg("40P01")), true},
		// permanentes: repetir não adianta
		{"violacao de unicidade", pg("23505"), false},
		{"violacao de CHECK", pg("23514"), false},
		{"violacao de FK", pg("23503"), false},
		{"integridade (triggers)", pg("23000"), false},
		{"tabela inexistente", pg("42P01"), false},
		{"sintaxe", pg("42601"), false},
		{"erro comum", errors.New("boom"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := postgres.IsTransient(tc.err); got != tc.want {
				t.Errorf("IsTransient(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
