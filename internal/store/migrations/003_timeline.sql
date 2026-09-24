CREATE INDEX items_timeline ON items (
    COALESCE(published_at, first_fetched_at) DESC,
    id DESC,
    feed_url
);
