-- Ledger da carteira: append-only e verificável.
-- Garantias impostas pelo banco:
--   * cada lançamento é imutável (sem UPDATE, DELETE ou TRUNCATE);
--   * balance_after = balance_before +/- valor, nunca negativo;
--   * um lançamento por (carteira, transação);
--   * a cadeia de lançamentos é contínua (balance_before = balance_after anterior);
--   * ao fim de cada commit, saldo e versão da carteira coincidem com o último lançamento.

-- Permite uma FK composta: o lançamento herda carteira, moeda e valor da transação
-- e não pode divergir deles.
ALTER TABLE wager_transactions
    ADD CONSTRAINT wager_tx_id_wallet_currency_amount_uk UNIQUE (id, wallet_id, currency, amount);

CREATE TABLE wallet_ledger_entries (
    id             UUID        PRIMARY KEY,
    wallet_id      UUID        NOT NULL REFERENCES wallets (id),
    transaction_id UUID        NOT NULL,
    direction      TEXT        NOT NULL,
    currency       CHAR(3)     NOT NULL,
    amount         BIGINT      NOT NULL,
    balance_before BIGINT      NOT NULL,
    balance_after  BIGINT      NOT NULL,
    wallet_version BIGINT      NOT NULL,  -- versão da carteira após este lançamento
    created_at     TIMESTAMPTZ NOT NULL,

    CONSTRAINT ledger_direction_chk CHECK (direction IN ('DEBIT', 'CREDIT')),
    CONSTRAINT ledger_amount_positive_chk CHECK (amount > 0),
    CONSTRAINT ledger_balances_non_negative_chk CHECK (balance_before >= 0 AND balance_after >= 0),
    CONSTRAINT ledger_version_positive_chk CHECK (wallet_version >= 1),
    CONSTRAINT ledger_arithmetic_chk CHECK (
        (direction = 'CREDIT' AND balance_after = balance_before + amount)
        OR (direction = 'DEBIT' AND balance_after = balance_before - amount)
    ),

    -- Carteira, moeda e valor do lançamento devem ser os da transação.
    CONSTRAINT ledger_transaction_fk
        FOREIGN KEY (transaction_id, wallet_id, currency, amount)
        REFERENCES wager_transactions (id, wallet_id, currency, amount),

    CONSTRAINT ledger_wallet_transaction_uk UNIQUE (wallet_id, transaction_id),
    -- Também serve de índice para paginação estável por carteira.
    CONSTRAINT ledger_wallet_version_uk UNIQUE (wallet_id, wallet_version)
);

-- Imutabilidade
CREATE FUNCTION wallet_ledger_entries_forbid_change() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'wallet ledger is append-only: % is not allowed', TG_OP
        USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER wallet_ledger_entries_no_update_delete
    BEFORE UPDATE OR DELETE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION wallet_ledger_entries_forbid_change();

-- TRUNCATE não dispara triggers de linha, então precisa de um trigger próprio.
CREATE TRIGGER wallet_ledger_entries_no_truncate
    BEFORE TRUNCATE ON wallet_ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION wallet_ledger_entries_forbid_change();

-- Saldo e versão da carteira precisam coincidir com o último lançamento do ledger.
-- Sem lançamentos: saldo 0 e versão 1 (carteira aberta com saldo zero).
CREATE FUNCTION wallet_ledger_assert_consistent(p_wallet_id UUID) RETURNS void AS $$
DECLARE
    w_balance  BIGINT;
    w_version  BIGINT;
    l_balance  BIGINT;
    l_version  BIGINT;
BEGIN
    SELECT balance, version INTO w_balance, w_version FROM wallets WHERE id = p_wallet_id;
    IF NOT FOUND THEN
        RETURN;
    END IF;

    SELECT balance_after, wallet_version INTO l_balance, l_version
      FROM wallet_ledger_entries
     WHERE wallet_id = p_wallet_id
     ORDER BY wallet_version DESC
     LIMIT 1;

    IF NOT FOUND THEN
        l_balance := 0;
        l_version := 1;
    END IF;

    IF w_balance <> l_balance OR w_version <> l_version THEN
        RAISE EXCEPTION 'wallet % diverges from its ledger (wallet: balance=% version=%, ledger: balance=% version=%)',
            p_wallet_id, w_balance, w_version, l_balance, l_version
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
END;
$$ LANGUAGE plpgsql;

-- Validação de cada lançamento, executada no COMMIT (DEFERRED), quando
-- transação, carteira e lançamento já estão todos gravados.
CREATE FUNCTION wallet_ledger_entries_check_entry() RETURNS trigger AS $$
DECLARE
    t_kind   TEXT;
    t_status TEXT;
    prev_version BIGINT;
    prev_balance BIGINT;
    first_version BIGINT;
BEGIN
    SELECT kind, status INTO t_kind, t_status
      FROM wager_transactions WHERE id = NEW.transaction_id;

    IF t_status <> 'PROCESSED' OR t_kind = 'LOSS' THEN
        RAISE EXCEPTION 'ledger entry % requires a PROCESSED non-LOSS transaction (found % %)',
            NEW.id, t_kind, t_status
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF (t_kind = 'BET' AND NEW.direction <> 'DEBIT')
       OR (t_kind IN ('WIN', 'REFUND', 'OPENING') AND NEW.direction <> 'CREDIT') THEN
        RAISE EXCEPTION 'ledger entry % has direction % incompatible with transaction kind %',
            NEW.id, NEW.direction, t_kind
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF t_kind = 'OPENING' AND NEW.wallet_version <> 1 THEN
        RAISE EXCEPTION 'OPENING entry % must be the first of the wallet (version 1)', NEW.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    -- Cadeia contínua: o saldo anterior é o saldo posterior do lançamento anterior
    -- e a versão avança de 1 em 1. O primeiro lançamento parte de saldo 0: versão 1
    -- quando é a abertura com saldo positivo, versão 2 quando a carteira foi aberta zerada.
    SELECT wallet_version, balance_after INTO prev_version, prev_balance
      FROM wallet_ledger_entries
     WHERE wallet_id = NEW.wallet_id AND wallet_version < NEW.wallet_version
     ORDER BY wallet_version DESC
     LIMIT 1;

    IF NOT FOUND THEN
        first_version := CASE WHEN t_kind = 'OPENING' THEN 1 ELSE 2 END;
        IF NEW.balance_before <> 0 OR NEW.wallet_version <> first_version THEN
            RAISE EXCEPTION 'ledger entry % breaks the chain: first entry must start at balance 0 (version 1 for OPENING, otherwise 2)', NEW.id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    ELSIF NEW.wallet_version <> prev_version + 1 OR NEW.balance_before <> prev_balance THEN
        RAISE EXCEPTION 'ledger entry % breaks the chain (expected version % and balance_before %)',
            NEW.id, prev_version + 1, prev_balance
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    PERFORM wallet_ledger_assert_consistent(NEW.wallet_id);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER wallet_ledger_entries_check_entry
    AFTER INSERT ON wallet_ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION wallet_ledger_entries_check_entry();

-- O inverso: nenhuma alteração de saldo ou versão sobrevive ao commit sem o
-- lançamento correspondente no ledger.
CREATE FUNCTION wallets_check_against_ledger() RETURNS trigger AS $$
BEGIN
    PERFORM wallet_ledger_assert_consistent(NEW.id);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER wallets_check_against_ledger
    AFTER INSERT OR UPDATE ON wallets
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION wallets_check_against_ledger();
