-- The canonical embedded schema lives in internal/store/migrations.go so the
-- single static control-plane binary can migrate itself. This marker documents
-- the first release for operators and external migration tooling.
PRAGMA user_version = 1;
