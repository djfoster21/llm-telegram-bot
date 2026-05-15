package store

import (
	"database/sql"
	"encoding/json"
	"fmt"

	_ "modernc.org/sqlite"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS history (
			chat_id    INTEGER PRIMARY KEY,
			messages   TEXT    NOT NULL DEFAULT '[]',
			updated_at INTEGER NOT NULL DEFAULT (unixepoch())
		)`); err != nil {
		return nil, fmt.Errorf("create table: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Load(chatID int64) ([]Message, error) {
	var raw string
	err := s.db.QueryRow(`SELECT messages FROM history WHERE chat_id = ?`, chatID).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var msgs []Message
	if err := json.Unmarshal([]byte(raw), &msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

func (s *Store) Save(chatID int64, msgs []Message) error {
	raw, err := json.Marshal(msgs)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO history (chat_id, messages, updated_at) VALUES (?, ?, unixepoch())
		ON CONFLICT(chat_id) DO UPDATE
		SET messages = excluded.messages, updated_at = excluded.updated_at
	`, chatID, string(raw))
	return err
}

func (s *Store) Clear(chatID int64) error {
	_, err := s.db.Exec(`DELETE FROM history WHERE chat_id = ?`, chatID)
	return err
}

// Count returns the number of messages stored for chatID without unmarshalling
// the full blob. Returns 0 with nil error when the row does not exist.
func (s *Store) Count(chatID int64) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT json_array_length(messages) FROM history WHERE chat_id = ?`,
		chatID,
	).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return n, err
}
