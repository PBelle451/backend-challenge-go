-- Publicação em ordem por agregado: um evento só é reivindicado quando todos os
-- eventos anteriores (seq menor) do MESMO agregado já foram publicados. Este índice
-- torna essa checagem (NOT EXISTS) barata, olhando apenas os eventos ainda pendentes.
CREATE INDEX outbox_aggregate_pending_idx
    ON outbox_events (aggregate_id, seq)
    WHERE published_at IS NULL;
