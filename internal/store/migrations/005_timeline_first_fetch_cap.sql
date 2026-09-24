DROP INDEX items_timeline;

CREATE INDEX items_timeline ON items (
    COALESCE(MIN(published_at, first_fetched_at), first_fetched_at) DESC,
    id DESC,
    feed_url
);
