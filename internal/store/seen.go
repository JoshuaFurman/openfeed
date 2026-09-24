package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SeenPosition returns the saved reading position, or nil before any item has been reached.
// It is a point in timeline order that persists independently of the item it came from.
func (s *Store) SeenPosition(ctx context.Context) (_ *TimelineCursor, err error) {
	defer func() { err = publicError(ctx, "read seen position", err) }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var position TimelineCursor
	err = s.db.QueryRowContext(ctx, "SELECT date, item_id FROM seen_position").Scan(&position.date, &position.id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &position, nil
}

// AdvanceSeen saves the observed position of an item the user reached, unless a newer one is saved.
// The position must come from this database; it remains valid after its item is corrected or removed.
func (s *Store) AdvanceSeen(ctx context.Context, reached TimelineCursor) (err error) {
	defer func() { err = publicError(ctx, "advance seen position", err) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if reached.date == "" || reached.id <= 0 {
		return errors.New("position was not obtained from Timeline or SeenPosition")
	}

	tx, err := beginWrite(ctx, s.db)
	if err != nil {
		return fmt.Errorf("lock seen position: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO seen_position (singleton, date, item_id) VALUES (1, ?, ?)
		ON CONFLICT (singleton) DO UPDATE SET date = excluded.date, item_id = excluded.item_id
		WHERE (excluded.date, excluded.item_id) > (seen_position.date, seen_position.item_id)
	`, reached.date, reached.id); err != nil {
		return fmt.Errorf("save seen position: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit seen position: %w", err)
	}
	return nil
}
