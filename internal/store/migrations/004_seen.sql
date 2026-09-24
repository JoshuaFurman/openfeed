-- item_id is a timeline tie-breaker value, not a reference; the point outlives its item.
CREATE TABLE seen_position (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    date TEXT NOT NULL CHECK (length(date) = 30),
    item_id INTEGER NOT NULL CHECK (item_id > 0)
) STRICT;
