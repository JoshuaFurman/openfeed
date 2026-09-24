CREATE TABLE feeds (
    url TEXT PRIMARY KEY CHECK (url <> ''),
    etag TEXT NOT NULL,
    last_modified TEXT NOT NULL,
    fetched_at TEXT NOT NULL CHECK (length(fetched_at) = 30),
    revision INTEGER NOT NULL CHECK (revision > 0)
) STRICT;

CREATE TABLE items (
    id INTEGER PRIMARY KEY,
    feed_url TEXT NOT NULL REFERENCES feeds(url),
    key TEXT NOT NULL CHECK (key <> ''),
    title TEXT NOT NULL,
    url TEXT NOT NULL,
    content TEXT NOT NULL,
    published_at TEXT CHECK (published_at IS NULL OR length(published_at) = 30),
    first_fetched_at TEXT NOT NULL CHECK (length(first_fetched_at) = 30),
    UNIQUE (feed_url, key),
    CHECK (title <> '' OR url <> '' OR content <> '')
) STRICT;
