CREATE TABLE schema_migrations (
    version INTEGER PRIMARY KEY CHECK (version > 0)
) STRICT;
