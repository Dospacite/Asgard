-- Shared, Asgard-managed bridge networks allow explicitly selected services
-- from different projects to communicate without weakening each project's
-- default isolated network. The canonical executable migration remains in
-- internal/store/migrations.go.
PRAGMA user_version = 2;
