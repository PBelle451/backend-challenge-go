DROP TABLE IF EXISTS outbox_events;
DROP FUNCTION IF EXISTS outbox_events_forbid_truncate();
DROP FUNCTION IF EXISTS outbox_events_guard_delete();
DROP FUNCTION IF EXISTS outbox_events_guard_update();
DROP TABLE IF EXISTS inbox_messages;
DROP FUNCTION IF EXISTS inbox_messages_guard_update();
