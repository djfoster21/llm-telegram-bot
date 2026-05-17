// Package messages loads user-facing strings, LLM meta-prompts, and reaction
// patterns from a JSON file. The file is hot-reloaded on mtime change so
// edits take effect without a restart, mirroring the system-prompt cache.
package messages

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type UI struct {
	GenericError           string `json:"generic_error"`
	ContextOverflowGiveUp  string `json:"context_overflow_giveup"`
	Help                   string `json:"help"`
	Start                  string `json:"start"`
	MemoryCleared          string `json:"memory_cleared"`
	StatusFormat           string `json:"status_format"`
	BusyOther              string `json:"busy_other"`
	BusyPrevious           string `json:"busy_previous"`
	NothingToSummarize     string `json:"nothing_to_summarize"`
	EmptySummary           string `json:"empty_summary"`
	ThinkingPlaceholder    string `json:"thinking_placeholder"`
	EmptyResponse          string `json:"empty_response"`
	Unauthorized           string `json:"unauthorized"`
	ReminderPrefix         string `json:"reminder_prefix"`
	ContextWarnFormat      string `json:"context_warn_format"`
	RecentAssistantHeader  string `json:"recent_assistant_header"`
	ChatMembersPrefix      string `json:"chat_members_prefix"`
}

type Prompts struct {
	Spontaneous     string `json:"spontaneous"`
	SummaryOnDemand string `json:"summary_ondemand"`
	SummaryAuto     string `json:"summary_auto"`
}

type Tools struct {
	MemoryUnavailable      string `json:"memory_unavailable"`
	ChatContextUnavailable string `json:"chat_context_unavailable"`
	RecallEmpty            string `json:"recall_empty"`
	RemindersUnavailable   string `json:"reminders_unavailable"`
	ReminderAtISOInvalid   string `json:"reminder_at_iso_invalid"`
	ReminderNeedsTime      string `json:"reminder_needs_time"`
	ReminderInPast         string `json:"reminder_in_past"`
	ReminderConfirmFormat  string `json:"reminder_confirm_format"`
}

type Reaction struct {
	Pattern string `json:"pattern"`
	Emoji   string `json:"emoji"`
	RE      *regexp.Regexp `json:"-"`
}

type Bundle struct {
	UI        UI         `json:"ui"`
	Prompts   Prompts    `json:"prompts"`
	Tools     Tools      `json:"tools"`
	Reactions []Reaction `json:"reactions"`
}

// Loader hot-reloads a Bundle from a JSON file. Concurrent-safe; the cached
// Bundle is replaced atomically on mtime bump.
type Loader struct {
	path string

	mu     sync.RWMutex
	mtime  time.Time
	cached *Bundle
}

func NewLoader(path string) *Loader {
	return &Loader{path: path}
}

// Get returns the current Bundle, re-reading the file only when its mtime has
// advanced. Falls back to the sibling .example.json so a fresh clone runs
// without the user having to copy the file first. On any I/O or parse error,
// returns the last good Bundle if one exists, or BuiltinDefaults().
func (l *Loader) Get() *Bundle {
	path := resolveExamplePath(l.path, ".example.json")
	if path == "" {
		return l.fallback()
	}
	info, err := os.Stat(path)
	if err != nil {
		return l.fallback()
	}
	mtime := info.ModTime()

	l.mu.RLock()
	if l.cached != nil && !mtime.After(l.mtime) {
		b := l.cached
		l.mu.RUnlock()
		return b
	}
	l.mu.RUnlock()

	raw, err := os.ReadFile(path)
	if err != nil {
		return l.fallback()
	}
	var b Bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return l.fallback()
	}
	for i := range b.Reactions {
		re, err := regexp.Compile(b.Reactions[i].Pattern)
		if err != nil {
			continue
		}
		b.Reactions[i].RE = re
	}

	l.mu.Lock()
	l.cached = &b
	l.mtime = mtime
	l.mu.Unlock()
	return &b
}

func (l *Loader) fallback() *Bundle {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.cached != nil {
		return l.cached
	}
	return BuiltinDefaults()
}

// BuiltinDefaults returns a safe English fallback Bundle so the bot is still
// usable if messages.json is missing or unparseable on first boot. The
// shipped config/messages.example.json holds the canonical Spanish strings.
func BuiltinDefaults() *Bundle {
	return &Bundle{
		UI: UI{
			GenericError:          "Something went wrong. Try again.",
			ContextOverflowGiveUp: "The message was too large even after a reset. Try splitting it.",
			Help:                  "Conversational bot. Use /start, /reset, /summary, /status, /help.",
			Start:                 "Hi. Ask me anything.",
			MemoryCleared:         "Memory cleared.",
			StatusFormat:          "Model: %s\nUser ID: %d\nChat ID: %d",
			BusyOther:             "Hold on, still working on something else.",
			BusyPrevious:          "Hold on, still working on the previous one.",
			NothingToSummarize:    "Not much to summarize yet.",
			EmptySummary:          "(nothing to summarize)",
			ThinkingPlaceholder:   "Thinking...",
			EmptyResponse:         "(empty response)",
			Unauthorized:          "Not authorized.",
			ReminderPrefix:        "⏰ ",
			ContextWarnFormat:     "This chat is getting long (≈%d tokens). Send /reset if memory matters.",
			RecentAssistantHeader: "[INTERNAL CONTEXT — do not cite or respond]\nYour recent messages — do not repeat:",
			ChatMembersPrefix:     "Known members of this chat (names in [brackets] map to these people): ",
		},
		Prompts: Prompts{
			Spontaneous:     "[SYSTEM] Optional unsolicited reply. Only chime in with a sharp line; otherwise reply SKIP.",
			SummaryOnDemand: "[SYSTEM] Summarize the conversation in 2-3 short sentences. No greeting.",
			SummaryAuto:     "[SYSTEM] Summarize the conversation in 4-5 sentences, preserving names, decisions, plans. No greeting.",
		},
		Tools: Tools{
			MemoryUnavailable:      "Error: memory unavailable.",
			ChatContextUnavailable: "Error: chat context unavailable.",
			RecallEmpty:            "Nothing found for that search.",
			RemindersUnavailable:   "Error: reminders unavailable.",
			ReminderAtISOInvalid:   "Error: at_iso must be RFC3339 with timezone (e.g. 2026-05-22T20:00:00-03:00).",
			ReminderNeedsTime:      "Error: need in_seconds (preferred) or at_iso.",
			ReminderInPast:         "Error: reminder time must be in the future.",
			ReminderConfirmFormat:  "Reminder #%d scheduled for %s: %s",
		},
	}
}

// Sprintf is a thin wrapper kept here so callers don't have to import fmt
// just for one-line message formatting.
func Sprintf(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}

// resolveExamplePath returns p if it exists, else the sibling example file
// (stem + exampleSuffix in the same dir) if THAT exists, else "".
func resolveExamplePath(p, exampleSuffix string) string {
	if _, err := os.Stat(p); err == nil {
		return p
	}
	dir := filepath.Dir(p)
	base := filepath.Base(p)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	candidate := filepath.Join(dir, stem+exampleSuffix)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return ""
}
