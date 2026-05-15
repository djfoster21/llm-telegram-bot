// data-api is a small HTTP service that fronts a few public data sources
// (weather, crypto, FX) and exposes them as clean, LLM-friendly JSON
// endpoints. It lives next to the bot in docker-compose and is reached at
// http://data-api:8080 from the bot's tool calls.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	geocodeAPI    = "https://geocoding-api.open-meteo.com/v1/search"
	forecastAPI   = "https://api.open-meteo.com/v1/forecast"
	coingeckoAPI  = "https://api.coingecko.com/api/v3/simple/price"
	dolarAPIBase  = "https://dolarapi.com/v1/dolares"
	erAPIBase     = "https://open.er-api.com/v6/latest"
	requestTimeout = 10 * time.Second
)

var client = &http.Client{Timeout: requestTimeout}

// coinIDs maps user-friendly crypto symbols to CoinGecko's coin IDs. Add to
// this list as needed — anything not here returns a helpful error.
var coinIDs = map[string]string{
	"BTC":  "bitcoin",
	"ETH":  "ethereum",
	"USDT": "tether",
	"USDC": "usd-coin",
	"SOL":  "solana",
	"XRP":  "ripple",
	"ADA":  "cardano",
	"DOGE": "dogecoin",
	"DOT":  "polkadot",
	"AVAX": "avalanche-2",
	"LINK": "chainlink",
	"MATIC": "matic-network",
	"BNB":  "binancecoin",
	"LTC":  "litecoin",
	"TRX":  "tron",
}

// dolarTypes lists the ARS exchange-rate variants DolarAPI returns.
var dolarTypes = []string{"oficial", "blue", "bolsa", "contadoconliqui", "tarjeta", "mayorista", "cripto"}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/weather", handleWeather)
	mux.HandleFunc("/crypto", handleCrypto)
	mux.HandleFunc("/fx", handleFx)

	addr := ":" + getenv("PORT", "8080")
	log.Printf("data-api listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---------------- weather ----------------

type weatherResp struct {
	Location  string       `json:"location"`
	Country   string       `json:"country,omitempty"`
	Current   currentDay   `json:"current"`
	Today     dailyForecast `json:"today"`
	Tomorrow  dailyForecast `json:"tomorrow,omitempty"`
}

type currentDay struct {
	TempC      float64 `json:"temp_c"`
	WindKmh    float64 `json:"wind_kmh"`
	Condition  string  `json:"condition"`
}

type dailyForecast struct {
	MinC      float64 `json:"min_c"`
	MaxC      float64 `json:"max_c"`
	RainMm    float64 `json:"precip_mm"`
	Condition string  `json:"condition"`
}

func handleWeather(w http.ResponseWriter, r *http.Request) {
	loc := strings.TrimSpace(r.URL.Query().Get("location"))
	if loc == "" {
		writeError(w, http.StatusBadRequest, "missing required parameter: location")
		return
	}

	// 1. Geocode the location string into lat/lon.
	gq := url.Values{}
	gq.Set("name", loc)
	gq.Set("count", "1")
	gq.Set("language", "es")
	var geo struct {
		Results []struct {
			Name      string  `json:"name"`
			Country   string  `json:"country"`
			Admin1    string  `json:"admin1"`
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
		} `json:"results"`
	}
	if err := fetchJSON(r.Context(), geocodeAPI+"?"+gq.Encode(), &geo); err != nil {
		writeError(w, http.StatusBadGateway, "geocoding failed: "+err.Error())
		return
	}
	if len(geo.Results) == 0 {
		writeError(w, http.StatusNotFound, "no location found for "+loc)
		return
	}
	g := geo.Results[0]

	// 2. Fetch current + 2-day forecast.
	fq := url.Values{}
	fq.Set("latitude", strconv.FormatFloat(g.Latitude, 'f', 4, 64))
	fq.Set("longitude", strconv.FormatFloat(g.Longitude, 'f', 4, 64))
	fq.Set("current", "temperature_2m,weather_code,wind_speed_10m")
	fq.Set("daily", "weather_code,temperature_2m_max,temperature_2m_min,precipitation_sum")
	fq.Set("timezone", "auto")
	fq.Set("forecast_days", "2")
	var fcst struct {
		Current struct {
			Temperature2m float64 `json:"temperature_2m"`
			WeatherCode   int     `json:"weather_code"`
			WindSpeed10m  float64 `json:"wind_speed_10m"`
		} `json:"current"`
		Daily struct {
			WeatherCode      []int     `json:"weather_code"`
			TempMin          []float64 `json:"temperature_2m_min"`
			TempMax          []float64 `json:"temperature_2m_max"`
			PrecipitationSum []float64 `json:"precipitation_sum"`
		} `json:"daily"`
	}
	if err := fetchJSON(r.Context(), forecastAPI+"?"+fq.Encode(), &fcst); err != nil {
		writeError(w, http.StatusBadGateway, "forecast failed: "+err.Error())
		return
	}

	resp := weatherResp{
		Location: g.Name,
		Country:  g.Country,
		Current: currentDay{
			TempC:     fcst.Current.Temperature2m,
			WindKmh:   fcst.Current.WindSpeed10m,
			Condition: wmoToString(fcst.Current.WeatherCode),
		},
	}
	if len(fcst.Daily.WeatherCode) >= 1 {
		resp.Today = dailyForecast{
			MinC:      fcst.Daily.TempMin[0],
			MaxC:      fcst.Daily.TempMax[0],
			RainMm:    fcst.Daily.PrecipitationSum[0],
			Condition: wmoToString(fcst.Daily.WeatherCode[0]),
		}
	}
	if len(fcst.Daily.WeatherCode) >= 2 {
		resp.Tomorrow = dailyForecast{
			MinC:      fcst.Daily.TempMin[1],
			MaxC:      fcst.Daily.TempMax[1],
			RainMm:    fcst.Daily.PrecipitationSum[1],
			Condition: wmoToString(fcst.Daily.WeatherCode[1]),
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// wmoToString maps WMO weather codes to short Spanish descriptions.
func wmoToString(code int) string {
	switch {
	case code == 0:
		return "Despejado"
	case code <= 3:
		return "Parcialmente nublado"
	case code == 45 || code == 48:
		return "Niebla"
	case code >= 51 && code <= 57:
		return "Llovizna"
	case code >= 61 && code <= 67:
		return "Lluvia"
	case code >= 71 && code <= 77:
		return "Nieve"
	case code >= 80 && code <= 82:
		return "Chubascos"
	case code >= 85 && code <= 86:
		return "Nieve fuerte"
	case code == 95:
		return "Tormenta"
	case code == 96 || code == 99:
		return "Tormenta con granizo"
	}
	return fmt.Sprintf("Código %d", code)
}

// ---------------- crypto ----------------

type cryptoResp struct {
	Symbol   string  `json:"symbol"`
	Name     string  `json:"name"`
	PriceUSD float64 `json:"price_usd"`
	PriceARS float64 `json:"price_ars,omitempty"`
	Change24hPct float64 `json:"change_24h_pct"`
}

func handleCrypto(w http.ResponseWriter, r *http.Request) {
	sym := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("symbol")))
	if sym == "" {
		writeError(w, http.StatusBadRequest, "missing required parameter: symbol")
		return
	}
	id, ok := coinIDs[sym]
	if !ok {
		known := make([]string, 0, len(coinIDs))
		for k := range coinIDs {
			known = append(known, k)
		}
		writeError(w, http.StatusNotFound, fmt.Sprintf(
			"unknown symbol %q. Known symbols: %s", sym, strings.Join(known, ", ")))
		return
	}

	q := url.Values{}
	q.Set("ids", id)
	q.Set("vs_currencies", "usd,ars")
	q.Set("include_24hr_change", "true")
	var raw map[string]map[string]float64
	if err := fetchJSON(r.Context(), coingeckoAPI+"?"+q.Encode(), &raw); err != nil {
		writeError(w, http.StatusBadGateway, "coingecko failed: "+err.Error())
		return
	}
	row, ok := raw[id]
	if !ok {
		writeError(w, http.StatusNotFound, "no data for "+sym)
		return
	}
	writeJSON(w, http.StatusOK, cryptoResp{
		Symbol:       sym,
		Name:         id,
		PriceUSD:     row["usd"],
		PriceARS:     row["ars"],
		Change24hPct: row["usd_24h_change"],
	})
}

// ---------------- fx (currency conversions) ----------------

type fxResp struct {
	From  string             `json:"from"`
	To    string             `json:"to"`
	Rate  float64            `json:"rate,omitempty"`
	Rates map[string]float64 `json:"rates,omitempty"`
	AsOf  string             `json:"as_of,omitempty"`
}

func handleFx(w http.ResponseWriter, r *http.Request) {
	from := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("from")))
	to := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("to")))
	if from == "" || to == "" {
		writeError(w, http.StatusBadRequest, "required parameters: from, to")
		return
	}

	// Special case: USD→ARS returns ALL the ARS variants (blue, oficial, etc.)
	// since Argentinians always want to know which rate.
	if from == "USD" && to == "ARS" {
		rates := map[string]float64{}
		for _, t := range dolarTypes {
			var d struct {
				Compra float64 `json:"compra"`
				Venta  float64 `json:"venta"`
			}
			if err := fetchJSON(r.Context(), dolarAPIBase+"/"+t, &d); err != nil {
				continue
			}
			if d.Venta > 0 {
				rates[t] = d.Venta
			}
		}
		if len(rates) == 0 {
			writeError(w, http.StatusBadGateway, "dolarapi returned no rates")
			return
		}
		writeJSON(w, http.StatusOK, fxResp{
			From:  from,
			To:    to,
			Rates: rates,
			AsOf:  time.Now().UTC().Format("2006-01-02"),
		})
		return
	}

	// General case: open.er-api.com supports 150+ currencies.
	var er struct {
		Result string             `json:"result"`
		Base   string             `json:"base_code"`
		Rates  map[string]float64 `json:"rates"`
		Date   string             `json:"time_last_update_utc"`
	}
	if err := fetchJSON(r.Context(), erAPIBase+"/"+from, &er); err != nil {
		writeError(w, http.StatusBadGateway, "exchangerate-api failed: "+err.Error())
		return
	}
	if er.Result != "success" {
		writeError(w, http.StatusBadGateway, "exchangerate-api: "+er.Result)
		return
	}
	rate, ok := er.Rates[to]
	if !ok {
		writeError(w, http.StatusNotFound, "no rate for "+from+"→"+to)
		return
	}
	writeJSON(w, http.StatusOK, fxResp{
		From: from,
		To:   to,
		Rate: rate,
		AsOf: er.Date,
	})
}

// ---------------- helpers ----------------

func fetchJSON(ctx context.Context, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "llm-telegram-bot-data-api/0.1")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("upstream %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
