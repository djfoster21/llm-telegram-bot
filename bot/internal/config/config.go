package config

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	TelegramToken    string
	AllowedUserIDs   map[int64]bool
	AllowedChatIDs   map[int64]bool
	LlamaBaseURL     string
	SearxngURL       string
	DataAPIURL       string
	DBPath           string
	SystemPromptPath string
	UserNamesPath    string
	MessagesPath     string
	ModelFile        string

	HistoryTokenBudget int

	// LLM sampling — defaults match Qwen 2.5 / Qwen 3 official recipe.
	MaxResponseTokens int
	Temperature       float64
	TopP              float64
	TopK              int
	MinP              float64
	RepeatPenalty     float64

	// Bot behavior tunables.
	MaxStoredMessages     int
	StreamInterval        time.Duration
	ReactionProbability   float64
	SpontaneousMin        int
	SpontaneousMax        int
	AutoSummaryAtMessages int
	AutoSummaryKeepRecent int
	AutoSummaryCooldown   time.Duration
	ContextWarnThreshold  int
	ContextWarnCooldown   time.Duration
}

func Load() (*Config, error) {
	c := &Config{
		TelegramToken:    os.Getenv("TELEGRAM_BOT_TOKEN"),
		LlamaBaseURL:     getenv("LLAMA_BASE_URL", "http://llama-server:8080"),
		SearxngURL:       getenv("SEARXNG_URL", "http://searxng:8080"),
		DataAPIURL:       getenv("DATA_API_URL", "http://data-api:8080"),
		DBPath:           getenv("DB_PATH", "/data/bot.db"),
		SystemPromptPath: getenv("SYSTEM_PROMPT_PATH", "/config/system-prompt.txt"),
		UserNamesPath:    getenv("USER_NAMES_PATH", "/config/user-names.json"),
		MessagesPath:     getenv("MESSAGES_PATH", "/config/messages.json"),
		ModelFile:        os.Getenv("MODEL_FILE"),
	}
	if c.TelegramToken == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
	}

	c.HistoryTokenBudget = getenvIntMin("HISTORY_TOKEN_BUDGET", 2500, 200)

	c.MaxResponseTokens = getenvInt("MAX_RESPONSE_TOKENS", 800)
	c.Temperature = getenvFloat("LLM_TEMPERATURE", 0.7)
	c.TopP = getenvFloat("LLM_TOP_P", 0.8)
	c.TopK = getenvInt("LLM_TOP_K", 20)
	c.MinP = getenvFloat("LLM_MIN_P", 0.0)
	c.RepeatPenalty = getenvFloat("LLM_REPEAT_PENALTY", 1.0)

	c.MaxStoredMessages = getenvInt("MAX_STORED_MESSAGES", 300)
	c.StreamInterval = time.Duration(getenvInt("STREAM_INTERVAL_MS", 2500)) * time.Millisecond
	c.ReactionProbability = getenvFloat("REACTION_PROBABILITY", 0.4)
	c.SpontaneousMin = getenvInt("SPONTANEOUS_MIN", 1)
	c.SpontaneousMax = getenvInt("SPONTANEOUS_MAX", 15)
	if c.SpontaneousMax < c.SpontaneousMin {
		log.Printf("SPONTANEOUS_MAX=%d below SPONTANEOUS_MIN=%d; using min", c.SpontaneousMax, c.SpontaneousMin)
		c.SpontaneousMax = c.SpontaneousMin
	}
	c.AutoSummaryAtMessages = getenvInt("AUTO_SUMMARY_AT_MESSAGES", 60)
	c.AutoSummaryKeepRecent = getenvInt("AUTO_SUMMARY_KEEP_RECENT", 30)
	c.AutoSummaryCooldown = time.Duration(getenvInt("AUTO_SUMMARY_COOLDOWN_MIN", 30)) * time.Minute
	c.ContextWarnThreshold = getenvInt("CONTEXT_WARN_THRESHOLD", 3500)
	c.ContextWarnCooldown = time.Duration(getenvInt("CONTEXT_WARN_COOLDOWN_MIN", 60)) * time.Minute

	userIDs, err := parseIDList(os.Getenv("ALLOWED_USER_IDS"))
	if err != nil {
		return nil, fmt.Errorf("ALLOWED_USER_IDS: %w", err)
	}
	if len(userIDs) == 0 {
		return nil, fmt.Errorf("ALLOWED_USER_IDS is required (CSV of Telegram user IDs)")
	}
	c.AllowedUserIDs = userIDs

	chatIDs, err := parseIDList(os.Getenv("ALLOWED_CHAT_IDS"))
	if err != nil {
		return nil, fmt.Errorf("ALLOWED_CHAT_IDS: %w", err)
	}
	c.AllowedChatIDs = chatIDs // optional; empty is fine

	return c, nil
}

func parseIDList(raw string) (map[int64]bool, error) {
	out := map[int64]bool{}
	for _, s := range strings.Split(strings.TrimSpace(raw), ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bad id %q: %w", s, err)
		}
		out[id] = true
	}
	return out, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// getenvInt parses k as an int, falling back to def with a logged warning on
// parse failure or empty value. Logs only when the var was set but unusable.
func getenvInt(k string, def int) int {
	raw := os.Getenv(k)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("%s=%q is not an integer; falling back to %d", k, raw, def)
		return def
	}
	return n
}

// getenvIntMin is like getenvInt but enforces a minimum value, logging if the
// provided value is below it. Used for safety floors (e.g., a token budget
// too small to fit any history).
func getenvIntMin(k string, def, min int) int {
	v := getenvInt(k, def)
	if v < min {
		log.Printf("%s=%d below %d minimum; using %d", k, v, min, def)
		return def
	}
	return v
}

func getenvFloat(k string, def float64) float64 {
	raw := os.Getenv(k)
	if raw == "" {
		return def
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		log.Printf("%s=%q is not a number; falling back to %g", k, raw, def)
		return def
	}
	return f
}
