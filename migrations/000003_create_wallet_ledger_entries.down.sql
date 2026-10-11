DROP TRIGGER IF EXISTS wallets_check_against_ledger ON wallets;
DROP FUNCTION IF EXISTS wallets_check_against_ledger();
DROP TABLE IF EXISTS wallet_ledger_entries;
DROP FUNCTION IF EXISTS wallet_ledger_entries_check_entry();
DROP FUNCTION IF EXISTS wallet_ledger_assert_consistent(UUID);
DROP FUNCTION IF EXISTS wallet_ledger_entries_forbid_change();
ALTER TABLE wager_transactions DROP CONSTRAINT IF EXISTS wager_tx_id_wallet_currency_amount_uk;
