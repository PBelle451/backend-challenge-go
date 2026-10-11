DROP TRIGGER IF EXISTS wager_transactions_forbid_delete ON wager_transactions;
DROP TRIGGER IF EXISTS wager_transactions_guard_update ON wager_transactions;
DROP FUNCTION IF EXISTS wager_transactions_forbid_delete();
DROP FUNCTION IF EXISTS wager_transactions_guard_update();
DROP TABLE IF EXISTS wager_transactions;
ALTER TABLE wallets DROP CONSTRAINT IF EXISTS wallets_id_player_currency_uk;
