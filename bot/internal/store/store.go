package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// SearchHit is one result returned by FTS5 recall.
type SearchHit struct {
	Role    string
	Content string
	TS      time.Time
}

type Reminder struct {
	ID     int64
	ChatID int64
	UserID int64
	FireAt time.Time
	Text   string
}

type Member struct {
	UserID    int64
	FirstName string
	Username  string
	LastSeen  time.Time
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS history (
			chat_id    INTEGER PRIMARY KEY,
			messages   TEXT    NOT NULL DEFAULT '[]',
			updated_at INTEGER NOT NULL DEFAULT (unixepoch())
		)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
			content,
			chat_id UNINDEXED,
			role    UNINDEXED,
			ts      UNINDEXED,
			tokenize = 'unicode61 remove_diacritics 2'
		)`,
		`CREATE TABLE IF NOT EXISTS reminders (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			chat_id    INTEGER NOT NULL,
			user_id    INTEGER NOT NULL,
			fire_at    INTEGER NOT NULL,
			text       TEXT    NOT NULL,
			created_at INTEGER NOT NULL DEFAULT (unixepoch())
		)`,
		`CREATE INDEX IF NOT EXISTS reminders_fire_at ON reminders (fire_at)`,
		`CREATE TABLE IF NOT EXISTS chat_members (
			chat_id    INTEGER NOT NULL,
			user_id    INTEGER NOT NULL,
			first_name TEXT    NOT NULL DEFAULT '',
			username   TEXT    NOT NULL DEFAULT '',
			last_seen  INTEGER NOT NULL DEFAULT (unixepoch()),
			PRIMARY KEY (chat_id, user_id)
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return nil, fmt.Errorf("schema init: %w", err)
		}
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

// IndexMessage adds a message to the FTS5 search index so it remains
// retrievable even after the live history blob trims past it.
func (s *Store) IndexMessage(chatID int64, role, content string, ts time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO messages_fts (content, chat_id, role, ts) VALUES (?, ?, ?, ?)`,
		content, chatID, role, ts.Unix(),
	)
	return err
}

// Search returns up to limit FTS5 hits for chatID, ranked by relevance.
// Newest matches break ties.
func (s *Store) Search(chatID int64, query string, limit int) ([]SearchHit, error) {
	if limit <= 0 {
		limit = 5
	}
	rows, err := s.db.Query(`
		SELECT role, content, ts
		FROM messages_fts
		WHERE chat_id = ? AND messages_fts MATCH ?
		ORDER BY rank, ts DESC
		LIMIT ?`, chatID, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		var ts int64
		if err := rows.Scan(&h.Role, &h.Content, &ts); err != nil {
			return nil, err
		}
		h.TS = time.Unix(ts, 0)
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

func (s *Store) CreateReminder(chatID, userID int64, fireAt time.Time, text string) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO reminders (chat_id, user_id, fire_at, text) VALUES (?, ?, ?, ?)`,
		chatID, userID, fireAt.Unix(), text,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// DueReminders returns all reminders whose fire_at is at or before now.
func (s *Store) DueReminders(now time.Time) ([]Reminder, error) {
	rows, err := s.db.Query(
		`SELECT id, chat_id, user_id, fire_at, text FROM reminders WHERE fire_at <= ? ORDER BY fire_at`,
		now.Unix(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reminder
	for rows.Next() {
		var r Reminder
		var ts int64
		if err := rows.Scan(&r.ID, &r.ChatID, &r.UserID, &ts, &r.Text); err != nil {
			return nil, err
		}
		r.FireAt = time.Unix(ts, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) DeleteReminder(id int64) error {
	_, err := s.db.Exec(`DELETE FROM reminders WHERE id = ?`, id)
	return err
}

// UpsertMember records that userID is in chatID with the given names. last_seen
// is bumped to now. Used both for the active sender and for new_chat_members
// join events.
func (s *Store) UpsertMember(chatID, userID int64, firstName, username string) error {
	_, err := s.db.Exec(`
		INSERT INTO chat_members (chat_id, user_id, first_name, username, last_seen)
		VALUES (?, ?, ?, ?, unixepoch())
		ON CONFLICT(chat_id, user_id) DO UPDATE SET
			first_name = excluded.first_name,
			username   = excluded.username,
			last_seen  = excluded.last_seen
	`, chatID, userID, firstName, username)
	return err
}

func (s *Store) RemoveMember(chatID, userID int64) error {
	_, err := s.db.Exec(`DELETE FROM chat_members WHERE chat_id = ? AND user_id = ?`, chatID, userID)
	return err
}

// ListMembers returns all known members of chatID, ordered by most recently
// seen first.
func (s *Store) ListMembers(chatID int64) ([]Member, error) {
	rows, err := s.db.Query(
		`SELECT user_id, first_name, username, last_seen FROM chat_members WHERE chat_id = ? ORDER BY last_seen DESC`,
		chatID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		var ts int64
		if err := rows.Scan(&m.UserID, &m.FirstName, &m.Username, &ts); err != nil {
			return nil, err
		}
		m.LastSeen = time.Unix(ts, 0)
		out = append(out, m)
	}
	return out, rows.Err()
}
