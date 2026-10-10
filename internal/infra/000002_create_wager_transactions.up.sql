-- Transações financeiras: externas (BET, WIN, LOSS, REFUND, ROLLBACK) e internas (OPENING).
-- Dinheiro em unidades mínimas (BIGINT). Nada de float.
-- As regras abaixo valem no banco, independentemente de locks ou código da aplicação.

-- Permite uma FK composta que garante que jogador e moeda da transação
-- coincidem com os da carteira referenciada.
ALTER TABLE wallets
    ADD CONSTRAINT wallets_id_player_currency_uk UNIQUE (id, player_id, currency);

CREATE TABLE wager_transactions (
    id                                UUID        PRIMARY KEY,
    kind                              TEXT        NOT NULL,
    status                            TEXT        NOT NULL,

    wallet_id                         UUID        NOT NULL,
    player_id                         UUID        NOT NULL,
    currency                          CHAR(3)     NOT NULL,
    amount                            BIGINT      NOT NULL,

    -- Origem externa (nulos em OPENING, que é interno)
    provider_id                       TEXT,
    external_transaction_id           TEXT,
    idempotency_key                   TEXT,
    payload_hash                      BYTEA,      -- SHA-256 do JSON canônico (32 bytes)
    round_id                          TEXT,
    game_id                           TEXT,

    -- Referência (REFUND / ROLLBACK, e opcionalmente WIN / LOSS)
    reference_external_transaction_id TEXT,
    reference_transaction_id          UUID        REFERENCES wager_transactions (id),

    -- Resultado
    failure_code                      TEXT,       -- obrigatório em REJECTED e FAILED
    result_balance                    BIGINT,     -- saldo observado no processamento (usado no replay)

    -- Retomada durável (PENDING e PENDING_REFERENCE)
    attempts                          INTEGER     NOT NULL DEFAULT 0,
    next_attempt_at                   TIMESTAMPTZ,
    expires_at                        TIMESTAMPTZ,

    created_at                        TIMESTAMPTZ NOT NULL,
    updated_at                        TIMESTAMPTZ NOT NULL,
    finished_at                       TIMESTAMPTZ,

    CONSTRAINT wager_tx_kind_chk
        CHECK (kind IN ('OPENING', 'BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')),
    CONSTRAINT wager_tx_status_chk
        CHECK (status IN ('PENDING', 'PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED')),

    -- Jogador e moeda precisam ser os da carteira.
    CONSTRAINT wager_tx_wallet_fk
        FOREIGN KEY (wallet_id, player_id, currency)
        REFERENCES wallets (id, player_id, currency),

    -- LOSS tem valor zero; todos os demais exigem valor positivo.
    CONSTRAINT wager_tx_amount_chk
        CHECK ((kind = 'LOSS' AND amount = 0) OR (kind <> 'LOSS' AND amount > 0)),

    -- Origem: OPENING é interno (sem metadados externos); os demais exigem todos eles.
    CONSTRAINT wager_tx_origin_chk CHECK (
        (kind = 'OPENING'
            AND provider_id IS NULL AND external_transaction_id IS NULL
            AND idempotency_key IS NULL AND payload_hash IS NULL
            AND round_id IS NULL AND game_id IS NULL
            AND reference_external_transaction_id IS NULL)
        OR
        (kind <> 'OPENING'
            AND provider_id IS NOT NULL AND external_transaction_id IS NOT NULL
            AND idempotency_key IS NOT NULL AND payload_hash IS NOT NULL
            AND round_id IS NOT NULL AND game_id IS NOT NULL)
    ),
    CONSTRAINT wager_tx_payload_hash_len_chk
        CHECK (payload_hash IS NULL OR octet_length(payload_hash) = 32),

    -- REFUND e ROLLBACK exigem a referência externa.
    CONSTRAINT wager_tx_reference_required_chk
        CHECK (kind NOT IN ('REFUND', 'ROLLBACK') OR reference_external_transaction_id IS NOT NULL),
    -- Reversão processada exige a referência interna já resolvida.
    CONSTRAINT wager_tx_reference_resolved_chk
        CHECK (NOT (status = 'PROCESSED' AND kind IN ('REFUND', 'ROLLBACK'))
               OR reference_transaction_id IS NOT NULL),

    -- failure_code existe se, e somente se, a transação foi rejeitada ou falhou.
    CONSTRAINT wager_tx_failure_code_chk CHECK (
        (status IN ('REJECTED', 'FAILED') AND failure_code IS NOT NULL AND failure_code <> '')
        OR (status NOT IN ('REJECTED', 'FAILED') AND failure_code IS NULL)
    ),

    -- Processada exige o saldo observado (replay devolve esse valor).
    CONSTRAINT wager_tx_result_balance_chk
        CHECK ((status <> 'PROCESSED' OR result_balance IS NOT NULL)
               AND (result_balance IS NULL OR result_balance >= 0)),

    -- Pendente <=> tem próxima tentativa agendada; terminal <=> tem finished_at.
    CONSTRAINT wager_tx_pending_schedule_chk
        CHECK ((status IN ('PENDING', 'PENDING_REFERENCE')) = (next_attempt_at IS NOT NULL)),
    CONSTRAINT wager_tx_finished_chk
        CHECK ((status IN ('PROCESSED', 'REJECTED', 'FAILED')) = (finished_at IS NOT NULL)),

    CONSTRAINT wager_tx_attempts_chk CHECK (attempts >= 0),
    CONSTRAINT wager_tx_timestamps_chk CHECK (updated_at >= created_at),

    -- Idempotência persistente. A chave é única por provedor, para um provedor
    -- nunca colidir com a chave de outro.
    CONSTRAINT wager_tx_provider_external_uk UNIQUE (provider_id, external_transaction_id),
    CONSTRAINT wager_tx_provider_idempotency_uk UNIQUE (provider_id, idempotency_key)
);

-- Crédito inicial duplicado é impossível: no máximo um OPENING por carteira.
CREATE UNIQUE INDEX wager_tx_one_opening_per_wallet_uk
    ON wager_transactions (wallet_id)
    WHERE kind = 'OPENING';

-- Uma transação referenciada recebe no máximo UMA reversão bem-sucedida
-- (REFUND ou ROLLBACK), o que impede devolução duplicada do mesmo débito.
CREATE UNIQUE INDEX wager_tx_one_processed_reversal_per_reference_uk
    ON wager_transactions (reference_transaction_id)
    WHERE status = 'PROCESSED' AND kind IN ('REFUND', 'ROLLBACK');

-- Workers de retomada: busca por trabalho vencido.
CREATE INDEX wager_tx_due_idx
    ON wager_transactions (next_attempt_at)
    WHERE status IN ('PENDING', 'PENDING_REFERENCE');

-- Resolução de referências: quem está esperando por (provedor, id externo)?
CREATE INDEX wager_tx_waiting_reference_idx
    ON wager_transactions (provider_id, reference_external_transaction_id)
    WHERE status = 'PENDING_REFERENCE';

-- Máquina de estados protegida no banco:
--  * estados terminais (PROCESSED, REJECTED, FAILED) não aceitam nenhuma alteração;
--  * campos de negócio e de identidade são imutáveis;
--  * a referência interna, uma vez resolvida, não muda;
--  * PENDING_REFERENCE não volta para PENDING.
CREATE FUNCTION wager_transactions_guard_update() RETURNS trigger AS $$
BEGIN
    IF OLD.status IN ('PROCESSED', 'REJECTED', 'FAILED') THEN
        RAISE EXCEPTION 'wager transaction % is in terminal state % and cannot change', OLD.id, OLD.status
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.kind IS DISTINCT FROM OLD.kind
       OR NEW.wallet_id IS DISTINCT FROM OLD.wallet_id
       OR NEW.player_id IS DISTINCT FROM OLD.player_id
       OR NEW.currency IS DISTINCT FROM OLD.currency
       OR NEW.amount IS DISTINCT FROM OLD.amount
       OR NEW.provider_id IS DISTINCT FROM OLD.provider_id
       OR NEW.external_transaction_id IS DISTINCT FROM OLD.external_transaction_id
       OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
       OR NEW.payload_hash IS DISTINCT FROM OLD.payload_hash
       OR NEW.round_id IS DISTINCT FROM OLD.round_id
       OR NEW.game_id IS DISTINCT FROM OLD.game_id
       OR NEW.reference_external_transaction_id IS DISTINCT FROM OLD.reference_external_transaction_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'business fields of wager transaction % are immutable', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF OLD.reference_transaction_id IS NOT NULL
       AND NEW.reference_transaction_id IS DISTINCT FROM OLD.reference_transaction_id THEN
        RAISE EXCEPTION 'resolved reference of wager transaction % cannot change', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF OLD.status = 'PENDING_REFERENCE' AND NEW.status = 'PENDING' THEN
        RAISE EXCEPTION 'invalid transition PENDING_REFERENCE -> PENDING (transaction %)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER wager_transactions_guard_update
    BEFORE UPDATE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION wager_transactions_guard_update();

-- Auditoria: transações nunca são apagadas.
CREATE FUNCTION wager_transactions_forbid_delete() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'wager transactions are append-only and cannot be deleted (transaction %)', OLD.id
        USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER wager_transactions_forbid_delete
    BEFORE DELETE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION wager_transactions_forbid_delete();
