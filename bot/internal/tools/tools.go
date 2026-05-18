package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	readability "github.com/go-shiori/go-readability"
	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"llm-telegram-bot/internal/llm"
	"llm-telegram-bot/internal/messages"
	"llm-telegram-bot/internal/store"
	"llm-telegram-bot/internal/textutil"
)

const (
	maxSearchResults = 5
	maxFetchBytes    = 4 * 1024
	fetchBodyLimit   = 2 * 1024 * 1024
	searchBodyLimit  = 4 * 1024 * 1024
)

type ctxKey int

const (
	ctxKeyChat ctxKey = iota
	ctxKeyUser
)

// WithChat attaches chat and user IDs to ctx so tools that need them
// (recall, set_reminder) can read them inside Execute.
func WithChat(ctx context.Context, chatID, userID int64) context.Context {
	ctx = context.WithValue(ctx, ctxKeyChat, chatID)
	ctx = context.WithValue(ctx, ctxKeyUser, userID)
	return ctx
}

func chatIDFromContext(ctx context.Context) (int64, bool) {
	v, ok := ctx.Value(ctxKeyChat).(int64)
	return v, ok
}

func userIDFromContext(ctx context.Context) (int64, bool) {
	v, ok := ctx.Value(ctxKeyUser).(int64)
	return v, ok
}

type Registry struct {
	searxngURL string
	dataAPIURL string
	store      *store.Store
	msgs       *messages.Loader
	http       *http.Client // for internal services (searxng, data-api)
	fetch      *http.Client // for fetch_url: SSRF-guarded against non-public IPs
	tg         *telego.Bot  // for tools with chat-side effects (send_meme); set via SetTelegram
}

// SetTelegram wires the Telegram client used by side-effect tools. Called
// from main after bot.New constructs the Bot, since the Bot owns the telego
// client and the Registry is created before it.
func (r *Registry) SetTelegram(tg *telego.Bot) {
	r.tg = tg
}

func New(searxngURL, dataAPIURL string, db *store.Store, msgs *messages.Loader) *Registry {
	return &Registry{
		searxngURL: strings.TrimRight(searxngURL, "/"),
		dataAPIURL: strings.TrimRight(dataAPIURL, "/"),
		store:      db,
		msgs:       msgs,
		http:       &http.Client{Timeout: 25 * time.Second},
		fetch:      newFetchClient(),
	}
}

// newFetchClient returns an http.Client whose DialContext resolves the target
// hostname and refuses any non-public IP. Validating at dial time covers
// redirects automatically and dodges resolve→connect TOCTOU.
func newFetchClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			var first net.IP
			for _, ip := range ips {
				if !isPublicIP(ip) {
					return nil, fmt.Errorf("blocked non-public address %s for host %s", ip, host)
				}
				if first == nil {
					first = ip
				}
			}
			if first == nil {
				return nil, fmt.Errorf("no usable address for host %s", host)
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(first.String(), port))
		},
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return &http.Client{
		Timeout:   25 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

// isPublicIP reports whether ip is safe to fetch from the bot. Blocks
// loopback, RFC1918/ULA private, link-local, multicast, and unspecified.
func isPublicIP(ip net.IP) bool {
	return !ip.IsLoopback() &&
		!ip.IsPrivate() &&
		!ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() &&
		!ip.IsMulticast() &&
		!ip.IsUnspecified()
}

func (r *Registry) Definitions() []llm.Tool {
	return []llm.Tool{
		{
			Type: "function",
			Function: llm.ToolDefinition{
				Name: "search_web",
				Description: "Live web search. Call FIRST for any time-sensitive or factual question whose answer changes over time. " +
					"Returns up to 5 results: title, URL, ~300-char snippet. " +
					"Use for: today's/tomorrow's weather, current news, recent events, prices, " +
					"opening hours, sports scores, niche facts, anything you don't reliably know. " +
					"Example queries: \"clima Vilassar de Mar mañana\", \"cotización dólar hoy\", " +
					"\"historia frase tu vieja\", \"resultado Boca River\".",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{
							"type":        "string",
							"description": "Concise search query in the user's language. 2-8 words. No quotes, no boolean operators.",
						},
					},
					"required": []string{"query"},
				},
			},
		},
		{
			Type: "function",
			Function: llm.ToolDefinition{
				Name: "fetch_url",
				Description: "Fetch and extract readable text from one webpage. ~4 KB max. " +
					"Call AFTER search_web when a snippet doesn't directly answer the question — " +
					"articles to summarize, or specific URLs the user asks about. " +
					"Example: after searching, fetch_url the most relevant page. " +
					"For WEATHER, CRYPTO, or CURRENCY questions, do NOT use this — use the dedicated tools instead.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"url": map[string]any{
							"type":        "string",
							"description": "Absolute URL starting with http:// or https://. Usually a URL returned by a previous search_web call.",
						},
					},
					"required": []string{"url"},
				},
			},
		},
		{
			Type: "function",
			Function: llm.ToolDefinition{
				Name: "get_weather",
				Description: "Get current weather + today/tomorrow forecast for a city or place. " +
					"PREFER this over search_web for any weather question. " +
					"Returns: current temp/wind/condition + min/max/precip for today and tomorrow. " +
					"Examples: \"clima Vilassar de Mar\", \"qué tiempo hace mañana en Buenos Aires\", \"va a llover en Madrid?\".",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"location": map[string]any{
							"type":        "string",
							"description": "City or place name. Free-form, in any language. Example: \"Vilassar de Mar\" or \"Buenos Aires\".",
						},
					},
					"required": []string{"location"},
				},
			},
		},
		{
			Type: "function",
			Function: llm.ToolDefinition{
				Name: "get_crypto_price",
				Description: "Get current price of a cryptocurrency in USD and ARS, plus 24h change. " +
					"PREFER this over search_web for any crypto price question. " +
					"Supported symbols: BTC, ETH, USDT, USDC, SOL, XRP, ADA, DOGE, DOT, AVAX, LINK, MATIC, BNB, LTC, TRX. " +
					"Examples: \"cuánto sale el bitcoin\", \"precio del ETH\", \"BTC en pesos\".",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"symbol": map[string]any{
							"type":        "string",
							"description": "Crypto ticker symbol, uppercase. Examples: BTC, ETH, SOL.",
						},
					},
					"required": []string{"symbol"},
				},
			},
		},
		{
			Type: "function",
			Function: llm.ToolDefinition{
				Name: "recall",
				Description: "Search your long-term memory of past messages in THIS chat. " +
					"Use when the user asks about something said before — possibly weeks or months ago — " +
					"that may have fallen out of the live conversation window. " +
					"Returns up to 5 matching messages with timestamps. " +
					"Examples: \"qué dijo Pibe sobre el viaje a Mar del Plata\", \"el chiste de la suegra\", " +
					"\"cuándo era el cumple de Capo\".",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{
							"type":        "string",
							"description": "Keywords to search for. FTS5 syntax. 1-5 words usually. Example: \"Mar del Plata viaje\".",
						},
					},
					"required": []string{"query"},
				},
			},
		},
		{
			Type: "function",
			Function: llm.ToolDefinition{
				Name: "set_reminder",
				Description: "Schedule a reminder message to be posted in THIS chat. " +
					"Use whenever the user asks you to remind them of something. " +
					"PREFER 'in_seconds' for relative requests — the server adds it to the current time. " +
					"Examples: \"avisame en 5 minutos\" → in_seconds=300, \"en 1 hora\" → 3600, \"en 2 días\" → 172800. " +
					"Only use 'at_iso' when the user gave an explicit absolute date+time you can encode in RFC3339.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"in_seconds": map[string]any{
							"type":        "integer",
							"description": "Seconds from now until the reminder fires. Use this for any \"in X minutes/hours/days\" request. Minimum 1.",
						},
						"at_iso": map[string]any{
							"type":        "string",
							"description": "Absolute RFC3339 timestamp WITH timezone, e.g. \"2026-05-22T20:00:00-03:00\". Argentina is UTC-3. Only use when the user names a specific date; otherwise use in_seconds.",
						},
						"text": map[string]any{
							"type":        "string",
							"description": "What to remind the user about. Short, in the user's language.",
						},
					},
					"required": []string{"text"},
				},
			},
		},
		{
			Type: "function",
			Function: llm.ToolDefinition{
				Name: "send_meme",
				Description: "Send an animated GIF / meme to the chat as a reaction. " +
					"Use to convey a feeling or mood with a short keyword: \"facepalm\", \"crying\", " +
					"\"happy dance\", \"shrug\", \"thumbs up\", \"mind blown\". The GIF is delivered " +
					"directly to the chat — you do NOT need to add a long text reply afterward; " +
					"one or two extra words at most, or nothing.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{
							"type":        "string",
							"description": "1-4 English keywords describing the mood or reaction. Examples: \"sad\", \"facepalm\", \"happy dance\", \"shrug\".",
						},
					},
					"required": []string{"query"},
				},
			},
		},
		{
			Type: "function",
			Function: llm.ToolDefinition{
				Name: "get_exchange_rate",
				Description: "Get currency conversion rate. PREFER this over search_web for any exchange-rate question. " +
					"Special case: from=USD to=ARS returns ALL the Argentinian variants (oficial, blue, mep, ccl, tarjeta, mayorista, cripto) — the user usually wants 'blue'. " +
					"Other currency pairs return a single rate. " +
					"Examples: \"cuánto está el dólar\" (use USD→ARS), \"euro a peso\" (EUR→ARS), \"dólar a euro\" (USD→EUR).",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"from": map[string]any{
							"type":        "string",
							"description": "Source currency, ISO code, uppercase. Examples: USD, EUR, ARS, GBP, BRL.",
						},
						"to": map[string]any{
							"type":        "string",
							"description": "Target currency, ISO code, uppercase. Examples: ARS, USD, EUR.",
						},
					},
					"required": []string{"from", "to"},
				},
			},
		},
	}
}

func (r *Registry) Execute(ctx context.Context, call llm.ToolCall) string {
	raw := call.Function.Arguments
	switch call.Function.Name {
	case "search_web":
		var a struct {
			Query string `json:"query"`
		}
		if !parseArgs(raw, &a, &a.Query) {
			return `Error: invalid arguments. Expected {"query": "..."}.`
		}
		return r.searchWeb(ctx, a.Query)
	case "fetch_url":
		var a struct {
			URL string `json:"url"`
		}
		if !parseArgs(raw, &a, &a.URL) {
			return `Error: invalid arguments. Expected {"url": "..."}.`
		}
		return r.fetchURL(ctx, a.URL)
	case "get_weather":
		var a struct {
			Location string `json:"location"`
		}
		if !parseArgs(raw, &a, &a.Location) {
			return `Error: invalid arguments. Expected {"location": "..."}.`
		}
		return r.getFromDataAPI(ctx, "/weather", "location", a.Location)
	case "get_crypto_price":
		var a struct {
			Symbol string `json:"symbol"`
		}
		if !parseArgs(raw, &a, &a.Symbol) {
			return `Error: invalid arguments. Expected {"symbol": "..."}.`
		}
		return r.getFromDataAPI(ctx, "/crypto", "symbol", a.Symbol)
	case "get_exchange_rate":
		var a struct {
			From string `json:"from"`
			To   string `json:"to"`
		}
		if !parseArgs(raw, &a, &a.From, &a.To) {
			return `Error: invalid arguments. Expected {"from": "...", "to": "..."}.`
		}
		return r.getFromDataAPI(ctx, "/fx", "from", a.From, "to", a.To)
	case "recall":
		var a struct{ Query string `json:"query"` }
		if !parseArgs(raw, &a, &a.Query) {
			return `Error: invalid arguments. Expected {"query": "..."}.`
		}
		return r.recall(ctx, a.Query)
	case "send_meme":
		var a struct {
			Query string `json:"query"`
		}
		if !parseArgs(raw, &a, &a.Query) {
			return `Error: invalid arguments. Expected {"query": "..."}.`
		}
		return r.sendMeme(ctx, a.Query)
	case "set_reminder":
		var a struct {
			InSeconds int    `json:"in_seconds"`
			AtISO     string `json:"at_iso"`
			Text      string `json:"text"`
		}
		if !parseArgs(raw, &a, &a.Text) {
			return `Error: invalid arguments. Expected {"in_seconds": N, "text": "..."} or {"at_iso": "RFC3339", "text": "..."}.`
		}
		return r.setReminder(ctx, a.InSeconds, a.AtISO, a.Text)
	default:
		return "Error: unknown tool " + call.Function.Name
	}
}

func (r *Registry) sendMeme(ctx context.Context, query string) string {
	if r.tg == nil {
		return "Error: telegram client not configured for send_meme."
	}
	chatID, ok := chatIDFromContext(ctx)
	if !ok {
		return "Error: chat context unavailable."
	}
	body := r.getFromDataAPI(ctx, "/meme", "q", query)
	var meme struct {
		URL   string `json:"url"`
		Title string `json:"title"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &meme); err != nil {
		return "Error: meme lookup failed."
	}
	if meme.Error != "" || meme.URL == "" {
		return "No meme found."
	}
	_, err := r.tg.SendAnimation(ctx, &telego.SendAnimationParams{
		ChatID:    tu.ID(chatID),
		Animation: telego.InputFile{URL: meme.URL},
	})
	if err != nil {
		return "Error: failed to send meme."
	}
	return "Meme sent. Keep your reply to one or two words, or nothing."
}

func (r *Registry) recall(ctx context.Context, query string) string {
	m := r.msgs.Get()
	if r.store == nil {
		return m.Tools.MemoryUnavailable
	}
	chatID, ok := chatIDFromContext(ctx)
	if !ok {
		return m.Tools.ChatContextUnavailable
	}
	hits, err := r.store.Search(chatID, query, 5)
	if err != nil {
		return "Error: " + err.Error()
	}
	if len(hits) == 0 {
		return m.Tools.RecallEmpty
	}
	loc, _ := time.LoadLocation("America/Argentina/Buenos_Aires")
	if loc == nil {
		loc = time.UTC
	}
	var b strings.Builder
	for i, h := range hits {
		fmt.Fprintf(&b, "[%d] %s (%s)\n%s\n\n",
			i+1,
			h.TS.In(loc).Format("2006-01-02 15:04"),
			h.Role,
			textutil.Ellipsize(strings.TrimSpace(h.Content), 300),
		)
	}
	return strings.TrimSpace(b.String())
}

func (r *Registry) setReminder(ctx context.Context, inSeconds int, atISO, text string) string {
	m := r.msgs.Get()
	if r.store == nil {
		return m.Tools.RemindersUnavailable
	}
	chatID, ok := chatIDFromContext(ctx)
	if !ok {
		return m.Tools.ChatContextUnavailable
	}
	userID, _ := userIDFromContext(ctx)

	now := time.Now()
	var fireAt time.Time
	switch {
	case inSeconds > 0:
		fireAt = now.Add(time.Duration(inSeconds) * time.Second)
	case atISO != "":
		parsed, err := time.Parse(time.RFC3339, atISO)
		if err != nil {
			return m.Tools.ReminderAtISOInvalid
		}
		fireAt = parsed
	default:
		return m.Tools.ReminderNeedsTime
	}
	if !fireAt.After(now) {
		return m.Tools.ReminderInPast
	}

	id, err := r.store.CreateReminder(chatID, userID, fireAt, text)
	if err != nil {
		return "Error: " + err.Error()
	}
	loc, _ := time.LoadLocation("America/Argentina/Buenos_Aires")
	if loc == nil {
		loc = time.UTC
	}
	return fmt.Sprintf(m.Tools.ReminderConfirmFormat,
		id, fireAt.In(loc).Format("2006-01-02 15:04 -0700"), text)
}

// parseArgs unmarshals raw into dst and verifies each required field is
// non-empty after trimming. Pass field pointers so they're dereferenced
// AFTER Unmarshal has populated them.
func parseArgs(raw string, dst any, required ...*string) bool {
	if err := json.Unmarshal([]byte(raw), dst); err != nil {
		return false
	}
	for _, p := range required {
		if strings.TrimSpace(*p) == "" {
			return false
		}
	}
	return true
}

func (r *Registry) getFromDataAPI(ctx context.Context, path string, kvs ...string) string {
	if r.dataAPIURL == "" {
		return "Error: data-api not configured"
	}
	q := url.Values{}
	for i := 0; i+1 < len(kvs); i += 2 {
		q.Set(kvs[i], kvs[i+1])
	}
	req, err := http.NewRequestWithContext(ctx, "GET", r.dataAPIURL+path+"?"+q.Encode(), nil)
	if err != nil {
		return "Error: " + err.Error()
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return "Error: " + err.Error()
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
	if err != nil {
		return "Error: " + err.Error()
	}
	return string(body)
}

type searxResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

type searxResponse struct {
	Results []searxResult `json:"results"`
}

func (r *Registry) searchWeb(ctx context.Context, query string) string {
	q := url.Values{}
	q.Set("q", query)
	q.Set("format", "json")
	q.Set("safesearch", "1")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, r.searxngURL+"/search?"+q.Encode(), nil)
	// SearXNG's botdetection rejects requests with neither X-Forwarded-For
	// nor X-Real-IP set. Our limiter.toml passes private ranges through, so
	// claim a loopback IP to satisfy the check.
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	resp, err := r.http.Do(req)
	if err != nil {
		return fmt.Sprintf("Error: search request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Sprintf("Error: search HTTP %d: %s", resp.StatusCode, textutil.Ellipsize(string(b), 200))
	}
	var sr searxResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, searchBodyLimit)).Decode(&sr); err != nil {
		return fmt.Sprintf("Error: search decode: %v", err)
	}
	if len(sr.Results) == 0 {
		return "No results."
	}
	var b strings.Builder
	for i, res := range sr.Results {
		if i >= maxSearchResults {
			break
		}
		fmt.Fprintf(&b, "[%d] %s\n%s\n%s\n\n", i+1, res.Title, res.URL, textutil.Ellipsize(strings.TrimSpace(res.Content), 300))
	}
	return strings.TrimSpace(b.String())
}

func (r *Registry) fetchURL(ctx context.Context, raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "Error: invalid URL (must be absolute http(s))."
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	req.Header.Set("User-Agent", "llm-telegram-bot/1.0")
	resp, err := r.fetch.Do(req)
	if err != nil {
		return fmt.Sprintf("Error: fetch failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Sprintf("Error: HTTP %d for %s", resp.StatusCode, raw)
	}
	article, err := readability.FromReader(io.LimitReader(resp.Body, fetchBodyLimit), u)
	if err != nil {
		return fmt.Sprintf("Error: extraction failed: %v", err)
	}
	text := strings.TrimSpace(article.TextContent)
	if text == "" {
		return "Error: no readable text extracted."
	}
	return fmt.Sprintf("Title: %s\nURL: %s\n\n%s", article.Title, raw, textutil.Ellipsize(text, maxFetchBytes))
}
