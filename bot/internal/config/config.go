package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	TelegramToken      string
	AllowedUserIDs     map[int64]bool
	AllowedChatIDs     map[int64]bool
	LlamaBaseURL       string
	SearxngURL         string
	DataAPIURL         string
	DBPath             string
	SystemPromptPath   string
	UserNamesPath      string
	HistoryTokenBudget int
	ModelFile          string
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
		ModelFile:        os.Getenv("MODEL_FILE"),
	}
	if c.TelegramToken == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
	}

	budget, _ := strconv.Atoi(getenv("HISTORY_TOKEN_BUDGET", "2500"))
	if budget < 200 {
		budget = 2500
	}
	c.HistoryTokenBudget = budget

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
