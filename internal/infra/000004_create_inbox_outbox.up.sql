-- Inbox (deduplicação de mensagens recebidas) e outbox (publicação confiável de eventos).

-- =========================================================================
-- INBOX
-- Uma linha por (consumidor, mensagem). Na entrada por SQS, esta linha é gravada
-- já concluída (completed_at preenchido) na MESMA transação SQL das alterações
-- de domínio, do ledger e dos registros de outbox.
-- =========================================================================
CREATE TABLE inbox_messages (
    id            UUID        PRIMARY KEY,
    consumer_name TEXT        NOT NULL,
    message_id    TEXT        NOT NULL,   -- messageId do envelope da mensagem
    payload_hash  BYTEA       NOT NULL,   -- SHA-256 do payload, para detectar reentrega adulterada
    received_at   TIMESTAMPTZ NOT NULL,
    completed_at  TIMESTAMPTZ,

    CONSTRAINT inbox_consumer_name_chk CHECK (consumer_name <> ''),
    CONSTRAINT inbox_message_id_chk CHECK (message_id <> ''),
    CONSTRAINT inbox_payload_hash_len_chk CHECK (octet_length(payload_hash) = 32),
    CONSTRAINT inbox_completed_after_received_chk CHECK (completed_at IS NULL OR completed_at >= received_at),
    CONSTRAINT inbox_consumer_message_uk UNIQUE (consumer_name, message_id)
);

-- Identidade e hash são imutáveis; a conclusão só pode ser registrada uma vez.
CREATE FUNCTION inbox_messages_guard_update() RETURNS trigger AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.consumer_name IS DISTINCT FROM OLD.consumer_name
       OR NEW.message_id IS DISTINCT FROM OLD.message_id
       OR NEW.payload_hash IS DISTINCT FROM OLD.payload_hash
       OR NEW.received_at IS DISTINCT FROM OLD.received_at THEN
        RAISE EXCEPTION 'identity and hash of inbox message % are immutable', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF OLD.completed_at IS NOT NULL AND NEW.completed_at IS DISTINCT FROM OLD.completed_at THEN
        RAISE EXCEPTION 'inbox message % is already completed', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER inbox_messages_guard_update
    BEFORE UPDATE ON inbox_messages
    FOR EACH ROW EXECUTE FUNCTION inbox_messages_guard_update();

-- =========================================================================
-- OUTBOX
-- Eventos gravados na mesma transação do fato que os originou e publicados
-- depois, por workers separados. O envelope completo é reconstruído a partir
-- das colunas + payload (snapshot imutável do campo "data").
-- =========================================================================
CREATE TABLE outbox_events (
    event_id        UUID        PRIMARY KEY,                       -- identidade estável (preservada em republicações)
    seq             BIGINT      GENERATED ALWAYS AS IDENTITY,       -- ordenação estável de claim
    event_type      TEXT        NOT NULL,
    event_version   INTEGER     NOT NULL,
    aggregate_type  TEXT        NOT NULL,
    aggregate_id    UUID        NOT NULL,
    correlation_id  TEXT        NOT NULL,
    causation_id    TEXT,
    payload         JSONB       NOT NULL,                           -- "data" do envelope
    occurred_at     TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL,

    -- Estado de publicação
    attempts        INTEGER     NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL,                           -- backoff: próximo envio
    locked_by       TEXT,                                           -- lease do publisher que reivindicou o evento
    locked_until    TIMESTAMPTZ,                                    -- lease vencido = trabalho abandonado, outro publisher assume
    last_error      TEXT,
    published_at    TIMESTAMPTZ,

    CONSTRAINT outbox_event_type_chk CHECK (event_type IN (
        'WagerTransactionProcessed',
        'WagerTransactionRejected',
        'WalletBalanceChanged',
        'WagerTransactionPendingReference')),
    CONSTRAINT outbox_aggregate_type_chk CHECK (aggregate_type IN ('WALLET', 'WAGER_TRANSACTION')),
    CONSTRAINT outbox_event_version_chk CHECK (event_version >= 1),
    CONSTRAINT outbox_correlation_id_chk CHECK (correlation_id <> ''),
    CONSTRAINT outbox_payload_object_chk CHECK (jsonb_typeof(payload) = 'object'),
    CONSTRAINT outbox_attempts_chk CHECK (attempts >= 0),
    CONSTRAINT outbox_lease_chk CHECK ((locked_by IS NULL) = (locked_until IS NULL)),
    -- Publicado = sem lease pendente.
    CONSTRAINT outbox_published_unlocked_chk CHECK (published_at IS NULL OR locked_by IS NULL),
    CONSTRAINT outbox_seq_uk UNIQUE (seq)
);

-- Busca de trabalho: eventos não publicados e vencidos, em ordem estável.
CREATE INDEX outbox_due_idx
    ON outbox_events (next_attempt_at, seq)
    WHERE published_at IS NULL;

-- Conteúdo do evento é imutável; só o estado de publicação evolui, e um evento
-- publicado não muda mais.
CREATE FUNCTION outbox_events_guard_update() RETURNS trigger AS $$
BEGIN
    IF OLD.published_at IS NOT NULL THEN
        RAISE EXCEPTION 'outbox event % is already published and immutable', OLD.event_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF NEW.event_id IS DISTINCT FROM OLD.event_id
       OR NEW.seq IS DISTINCT FROM OLD.seq
       OR NEW.event_type IS DISTINCT FROM OLD.event_type
       OR NEW.event_version IS DISTINCT FROM OLD.event_version
       OR NEW.aggregate_type IS DISTINCT FROM OLD.aggregate_type
       OR NEW.aggregate_id IS DISTINCT FROM OLD.aggregate_id
       OR NEW.correlation_id IS DISTINCT FROM OLD.correlation_id
       OR NEW.causation_id IS DISTINCT FROM OLD.causation_id
       OR NEW.payload IS DISTINCT FROM OLD.payload
       OR NEW.occurred_at IS DISTINCT FROM OLD.occurred_at
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'content of outbox event % is an immutable snapshot', OLD.event_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF NEW.attempts < OLD.attempts THEN
        RAISE EXCEPTION 'attempts of outbox event % cannot decrease', OLD.event_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER outbox_events_guard_update
    BEFORE UPDATE ON outbox_events
    FOR EACH ROW EXECUTE FUNCTION outbox_events_guard_update();

-- Nenhum evento pendente pode ser apagado (só os já publicados, por retenção).
CREATE FUNCTION outbox_events_guard_delete() RETURNS trigger AS $$
BEGIN
    IF OLD.published_at IS NULL THEN
        RAISE EXCEPTION 'unpublished outbox event % cannot be deleted', OLD.event_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER outbox_events_guard_delete
    BEFORE DELETE ON outbox_events
    FOR EACH ROW EXECUTE FUNCTION outbox_events_guard_delete();

-- TRUNCATE ignora triggers de linha e apagaria eventos pendentes.
CREATE FUNCTION outbox_events_forbid_truncate() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'TRUNCATE is not allowed on outbox_events'
        USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER outbox_events_no_truncate
    BEFORE TRUNCATE ON outbox_events
    FOR EACH STATEMENT EXECUTE FUNCTION outbox_events_forbid_truncate();
