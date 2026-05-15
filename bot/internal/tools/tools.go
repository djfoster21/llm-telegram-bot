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

	"llm-telegram-bot/internal/llm"
)

const (
	maxSearchResults = 5
	maxFetchBytes    = 4 * 1024
	fetchBodyLimit   = 2 * 1024 * 1024
	searchBodyLimit  = 4 * 1024 * 1024
)

type Registry struct {
	searxngURL string
	dataAPIURL string
	http       *http.Client // for internal services (searxng, data-api)
	fetch      *http.Client // for fetch_url: SSRF-guarded against non-public IPs
}

func New(searxngURL, dataAPIURL string) *Registry {
	return &Registry{
		searxngURL: strings.TrimRight(searxngURL, "/"),
		dataAPIURL: strings.TrimRight(dataAPIURL, "/"),
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
	switch call.Function.Name {
	case "search_web":
		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || strings.TrimSpace(args.Query) == "" {
			return `Error: invalid arguments. Expected {"query": "..."}.`
		}
		return r.searchWeb(ctx, args.Query)
	case "fetch_url":
		var args struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || strings.TrimSpace(args.URL) == "" {
			return `Error: invalid arguments. Expected {"url": "..."}.`
		}
		return r.fetchURL(ctx, args.URL)
	case "get_weather":
		var args struct {
			Location string `json:"location"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || strings.TrimSpace(args.Location) == "" {
			return `Error: invalid arguments. Expected {"location": "..."}.`
		}
		return r.getFromDataAPI(ctx, "/weather", "location", args.Location)
	case "get_crypto_price":
		var args struct {
			Symbol string `json:"symbol"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || strings.TrimSpace(args.Symbol) == "" {
			return `Error: invalid arguments. Expected {"symbol": "..."}.`
		}
		return r.getFromDataAPI(ctx, "/crypto", "symbol", args.Symbol)
	case "get_exchange_rate":
		var args struct {
			From string `json:"from"`
			To   string `json:"to"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil ||
			strings.TrimSpace(args.From) == "" || strings.TrimSpace(args.To) == "" {
			return `Error: invalid arguments. Expected {"from": "...", "to": "..."}.`
		}
		return r.getFromDataAPI(ctx, "/fx", "from", args.From, "to", args.To)
	default:
		return "Error: unknown tool " + call.Function.Name
	}
}

// getFromDataAPI calls the data-api service and returns its JSON response (or
// error message) as a raw string for the LLM to read. Variadic kvs are
// pairs of query-string key/value.
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
	resp, err := r.http.Do(req)
	if err != nil {
		return fmt.Sprintf("Error: search request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Sprintf("Error: search HTTP %d: %s", resp.StatusCode, truncate(string(b), 200))
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
		fmt.Fprintf(&b, "[%d] %s\n%s\n%s\n\n", i+1, res.Title, res.URL, truncate(strings.TrimSpace(res.Content), 300))
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
	return fmt.Sprintf("Title: %s\nURL: %s\n\n%s", article.Title, raw, truncate(text, maxFetchBytes))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
