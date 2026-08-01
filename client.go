// Client construction, public market-data endpoints, and the shared HTTP
// plumbing. Trading endpoints live in trading.go, WebSocket streams in
// ws.go, request signing in auth.go. See doc.go for the package overview.
package polymarket

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Production hosts. Public market data and authenticated trading are served
// from different bases; the client routes by endpoint, callers never choose.
const (
	DefaultGatewayURL = "https://gateway.polymarket.us"
	DefaultAPIURL     = "https://api.polymarket.us"
)

// Client reads public market data, and — when built by NewAuthedClient —
// places orders and reads the portfolio. The zero value is not usable.
type Client struct {
	hc         *http.Client
	gatewayURL string  // public market data host
	apiURL     string  // authenticated trading host
	signer     *Signer // nil = read-only public client
}

// NewClient returns a read-only Client using hc (or a sane default if hc is nil).
func NewClient(hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{hc: hc, gatewayURL: DefaultGatewayURL, apiURL: DefaultAPIURL}
}

// NewAuthedClient returns a Client that signs trading requests with signer.
// gatewayURL/apiURL "" mean production; tests pass their own.
func NewAuthedClient(hc *http.Client, signer *Signer, gatewayURL, apiURL string) *Client {
	c := NewClient(hc)
	c.signer = signer
	// Trailing slashes are trimmed because every request path is appended
	// with its own leading slash; on the WebSocket handshake a doubled
	// slash would also desync the request path from the signed path.
	if gatewayURL != "" {
		c.gatewayURL = strings.TrimRight(gatewayURL, "/")
	}
	if apiURL != "" {
		c.apiURL = strings.TrimRight(apiURL, "/")
	}
	return c
}

// Amount is the API's money shape: a decimal-dollar string plus currency.
type Amount struct {
	Value    string `json:"value"`
	Currency string `json:"currency"`
}

// Cents returns the amount in nearest integer cents (0 for empty/junk).
func (a Amount) Cents() int { return dollarsToCents(a.Value) }

// USD builds an Amount from integer cents (negative cents for debits).
func USD(cents int) Amount {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return Amount{Value: fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100), Currency: "USD"}
}

// Market is one tradeable outcome; an Event groups the related markets.
type Market struct {
	ID          int64   `json:"id"`
	Slug        string  `json:"slug"`
	Title       string  `json:"title"`
	Outcome     string  `json:"outcome"`
	Description string  `json:"description"`
	Active      bool    `json:"active"`
	Closed      bool    `json:"closed"`
	Liquidity   float64 `json:"liquidity"`
	Volume      float64 `json:"volume"`
	EventSlug   string  `json:"eventSlug"`
}

// Event groups related markets (e.g. one game, one date's question).
type Event struct {
	ID        int64    `json:"id"`
	Slug      string   `json:"slug"`
	Title     string   `json:"title"`
	StartTime string   `json:"startTime"` // ISO-8601
	EndTime   string   `json:"endTime"`   // ISO-8601
	Active    bool     `json:"active"`
	Closed    bool     `json:"closed"`
	Markets   []Market `json:"markets"`
}

// Level is one resting orderbook level. PriceC and Size are parsed for
// convenience; Px and Qty are the exchange's exact decimal strings.
type Level struct {
	PriceC int     // price in integer cents
	Size   float64 // contracts
	Px     string  // raw decimal-dollar price, e.g. "0.55"
	Qty    string  // raw decimal quantity
}

// wireLevel is the API's {px, qty} book level.
type wireLevel struct {
	Px  Amount `json:"px"`
	Qty string `json:"qty"`
}

func (w wireLevel) level() Level {
	sz, _ := strconv.ParseFloat(w.Qty, 64)
	return Level{PriceC: w.Px.Cents(), Size: sz, Px: w.Px.Value, Qty: w.Qty}
}

// Book is a market's full displayed depth. Both sides quote the same
// instrument (the market's LONG contract): bids are resting buys, offers
// resting sells. Best-priced level first on each side.
type Book struct {
	MarketSlug string
	Bids       []Level
	Offers     []Level
	State      string // MARKET_STATE_OPEN | _SUSPENDED | _HALTED | ...
}

// BBO is a market's top of book. The cents fields are 0 when that side is
// empty; the Px fields carry the exchange's exact decimals.
type BBO struct {
	MarketSlug  string
	BidC        int
	AskC        int
	LastTradeC  int
	BidPx       string
	AskPx       string
	LastTradePx string
}

// Settlement is a settled market's final price.
type Settlement struct {
	MarketSlug string
	PriceC     int
	Px         string // raw decimal settlement price
	SettledAt  string // ISO-8601
}

// get issues a GET to path on the public gateway host and decodes into out.
func (c *Client) get(ctx context.Context, path, what string, out any) error {
	return c.do(ctx, http.MethodGet, c.gatewayURL, path, what, nil, out, false, 4)
}

// do issues a request, retrying a 429 up to maxAttempts times with linear
// backoff; other non-2xx fail immediately as an *APIError carrying the
// response body (the exchange's reason is the caller's only clue). authed
// requests go signed; signing happens per attempt so the signature embeds a
// fresh timestamp. maxAttempts is 1 for order placement — see CreateOrder.
func (c *Client) do(ctx context.Context, method, base, path, what string, body []byte, out any, authed bool, maxAttempts int) error {
	if authed && c.signer == nil {
		return fmt.Errorf("%s: authenticated endpoint requires NewAuthedClient", what)
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
		if err != nil {
			return fmt.Errorf("%s: build request: %w", what, err)
		}
		req.Header.Set("Content-Type", "application/json")
		if authed {
			c.signer.sign(req)
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			lastErr = &APIError{StatusCode: resp.StatusCode, What: what, Body: "rate limited"}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			return &APIError{StatusCode: resp.StatusCode, What: what, Body: string(bytes.TrimSpace(msg))}
		}
		if out == nil {
			resp.Body.Close()
			return nil
		}
		err = json.NewDecoder(resp.Body).Decode(out)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("%s: decode response: %w", what, err)
		}
		return nil
	}
	return lastErr
}

// FetchMarket returns one market by slug.
func (c *Client) FetchMarket(ctx context.Context, slug string) (Market, error) {
	var mr struct {
		Market Market `json:"market"`
	}
	err := c.get(ctx, "/v1/market/slug/"+url.PathEscape(slug), slug, &mr)
	return mr.Market, err
}

// MarketFilter narrows ListMarkets. Zero fields are omitted; ActiveOnly
// maps to active=true&closed=false.
type MarketFilter struct {
	EventSlug  string
	ActiveOnly bool
	Limit      int // 0 = server default
	Offset     int
}

// ListMarkets returns markets matching f.
func (c *Client) ListMarkets(ctx context.Context, f MarketFilter) ([]Market, error) {
	q := url.Values{}
	if f.EventSlug != "" {
		q.Set("eventSlug", f.EventSlug)
	}
	if f.ActiveOnly {
		q.Set("active", "true")
		q.Set("closed", "false")
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	if f.Offset > 0 {
		q.Set("offset", strconv.Itoa(f.Offset))
	}
	path := "/v1/markets"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var mr struct {
		Markets []Market `json:"markets"`
	}
	err := c.get(ctx, path, "markets", &mr)
	return mr.Markets, err
}

// FetchEvent returns one event (with its nested markets) by slug.
func (c *Client) FetchEvent(ctx context.Context, slug string) (Event, error) {
	var er struct {
		Event Event `json:"event"`
	}
	err := c.get(ctx, "/v1/events/slug/"+url.PathEscape(slug), slug, &er)
	return er.Event, err
}

// FetchBook returns a market's live displayed depth.
func (c *Client) FetchBook(ctx context.Context, slug string) (Book, error) {
	var br struct {
		MarketSlug string      `json:"marketSlug"`
		Bids       []wireLevel `json:"bids"`
		Offers     []wireLevel `json:"offers"`
		State      string      `json:"state"`
	}
	if err := c.get(ctx, "/v1/markets/"+url.PathEscape(slug)+"/book", "book "+slug, &br); err != nil {
		return Book{}, err
	}
	b := Book{MarketSlug: br.MarketSlug, State: br.State}
	for _, l := range br.Bids {
		b.Bids = append(b.Bids, l.level())
	}
	for _, l := range br.Offers {
		b.Offers = append(b.Offers, l.level())
	}
	return b, nil
}

// FetchBBO returns a market's top of book.
func (c *Client) FetchBBO(ctx context.Context, slug string) (BBO, error) {
	var br struct {
		MarketSlug  string `json:"marketSlug"`
		BestBid     Amount `json:"bestBid"`
		BestAsk     Amount `json:"bestAsk"`
		LastTradePx Amount `json:"lastTradePx"`
	}
	if err := c.get(ctx, "/v1/markets/"+url.PathEscape(slug)+"/bbo", "bbo "+slug, &br); err != nil {
		return BBO{}, err
	}
	return BBO{
		MarketSlug:  br.MarketSlug,
		BidC:        br.BestBid.Cents(),
		AskC:        br.BestAsk.Cents(),
		LastTradeC:  br.LastTradePx.Cents(),
		BidPx:       br.BestBid.Value,
		AskPx:       br.BestAsk.Value,
		LastTradePx: br.LastTradePx.Value,
	}, nil
}

// FetchSettlement returns a settled market's settlement price and time.
func (c *Client) FetchSettlement(ctx context.Context, slug string) (Settlement, error) {
	var sr struct {
		MarketSlug      string `json:"marketSlug"`
		SettlementPrice Amount `json:"settlementPrice"`
		SettledAt       string `json:"settledAt"`
	}
	if err := c.get(ctx, "/v1/markets/"+url.PathEscape(slug)+"/settlement", "settlement "+slug, &sr); err != nil {
		return Settlement{}, err
	}
	return Settlement{
		MarketSlug: sr.MarketSlug,
		PriceC:     sr.SettlementPrice.Cents(),
		Px:         sr.SettlementPrice.Value,
		SettledAt:  sr.SettledAt,
	}, nil
}

// dollarsToCents parses "0.55" -> 55 (nearest cent), tolerant of junk.
// Rounds half away from zero so negative amounts (realized losses) round
// symmetrically with gains.
func dollarsToCents(s string) int {
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int(math.Round(v * 100))
}
