-- Carteira: raiz do agregado financeiro.
-- Dinheiro em unidades mínimas (centavos) como BIGINT; nenhum float em qualquer ponto.

CREATE TABLE wallets (
    id         UUID        PRIMARY KEY,
    player_id  UUID        NOT NULL,
    currency   CHAR(3)     NOT NULL,
    balance    BIGINT      NOT NULL,
    version    BIGINT      NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,

    -- Moeda no formato ISO 4217 (três letras maiúsculas).
    CONSTRAINT wallets_currency_format_chk CHECK (currency ~ '^[A-Z]{3}$'),
    -- Saldo nunca negativo, independentemente de locks da aplicação.
    CONSTRAINT wallets_balance_non_negative_chk CHECK (balance >= 0),
    CONSTRAINT wallets_version_positive_chk CHECK (version >= 1),
    -- Um único par (jogador, moeda) por carteira.
    CONSTRAINT wallets_player_currency_uk UNIQUE (player_id, currency)
);

-- Proteções de atualização no próprio banco:
--  * identidade da carteira (id, jogador, moeda) é imutável;
--  * a versão sobe exatamente +1 quando o saldo muda, e só nesse caso;
--  * com isso, um escritor que parta de uma versão antiga não consegue
--    sobrescrever uma atualização já confirmada sem que isso seja detectável.
CREATE FUNCTION wallets_guard_update() RETURNS trigger AS $$
BEGIN
    IF NEW.id <> OLD.id
       OR NEW.player_id <> OLD.player_id
       OR NEW.currency <> OLD.currency THEN
        RAISE EXCEPTION 'wallet identity is immutable (wallet %)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF NEW.balance <> OLD.balance AND NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'balance change requires version increment by exactly 1 (wallet %)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF NEW.balance = OLD.balance AND NEW.version <> OLD.version THEN
        RAISE EXCEPTION 'version may only change together with balance (wallet %)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER wallets_guard_update
    BEFORE UPDATE ON wallets
    FOR EACH ROW EXECUTE FUNCTION wallets_guard_update();
