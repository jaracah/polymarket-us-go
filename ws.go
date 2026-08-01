// WebSocket streams: market data (books, BBOs, trades) and private data
// (order executions, positions, balances), both on the api host and both
// requiring a Signer — the exchange authenticates even the market stream.
// Auth rides the HTTP upgrade request as the same three X-PM-* headers,
// signed over "GET" + the stream path.
//
// The shape is pull-based: Dial, Subscribe*, then call Next in a loop. Next
// returns one parsed message; an error from Next about one message's content
// leaves the stream usable, a transport error does not (redial to recover —
// subscriptions do not survive a reconnect).
//
// Concurrency: at most one goroutine may call Next at a time. Subscribe*,
// Unsubscribe, and Close are safe to call concurrently with Next and with
// each other (the underlying library serializes writes).
package polymarket

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/coder/websocket"
)

// Subscription types, matching the exchange's vocabulary.
const (
	SubMarketData     = "SUBSCRIPTION_TYPE_MARKET_DATA"      // full book refreshes
	SubMarketDataLite = "SUBSCRIPTION_TYPE_MARKET_DATA_LITE" // best bid/ask only
	SubTrade          = "SUBSCRIPTION_TYPE_TRADE"
	SubOrder          = "SUBSCRIPTION_TYPE_ORDER"
	SubPosition       = "SUBSCRIPTION_TYPE_POSITION"
	SubAccountBalance = "SUBSCRIPTION_TYPE_ACCOUNT_BALANCE"
)

// stream is the shared connection plumbing under both public stream types.
type stream struct {
	conn *websocket.Conn
}

// dialStream opens an authenticated WebSocket at path on the api host.
func (c *Client) dialStream(ctx context.Context, path string) (*stream, error) {
	if c.signer == nil {
		return nil, fmt.Errorf("polymarket: streams require an authenticated client")
	}
	wsURL := strings.Replace(strings.Replace(c.apiURL, "https://", "wss://", 1), "http://", "ws://", 1) + path
	ts, sig := c.signer.signature(http.MethodGet, path)
	h := http.Header{}
	h.Set("X-PM-Access-Key", c.signer.keyID)
	h.Set("X-PM-Timestamp", ts)
	h.Set("X-PM-Signature", sig)
	// The dial client must have no Timeout (it would sever the upgraded
	// connection); reuse the configured transport, drop the deadline.
	hc := &http.Client{Transport: c.hc.Transport}
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: h, HTTPClient: hc})
	if err != nil {
		return nil, fmt.Errorf("polymarket: dial %s: %w", path, err)
	}
	// Book snapshots on busy markets outgrow the library's 32 KiB default.
	conn.SetReadLimit(1 << 20)
	return &stream{conn: conn}, nil
}

type wsSubscribe struct {
	Subscribe wsSubscribeBody `json:"subscribe"`
}

type wsSubscribeBody struct {
	RequestID        string   `json:"requestId"`
	SubscriptionType string   `json:"subscriptionType"`
	MarketSlugs      []string `json:"marketSlugs,omitempty"`
}

// subscribe sends one subscribe frame. requestID is caller-chosen and keys
// every message the subscription produces (and its unsubscribe).
func (s *stream) subscribe(ctx context.Context, requestID, subType string, slugs []string) error {
	frame, err := json.Marshal(wsSubscribe{Subscribe: wsSubscribeBody{
		RequestID: requestID, SubscriptionType: subType, MarketSlugs: slugs,
	}})
	if err != nil {
		return err
	}
	return s.conn.Write(ctx, websocket.MessageText, frame)
}

// Unsubscribe cancels the subscription opened under requestID.
func (s *stream) Unsubscribe(ctx context.Context, requestID string) error {
	frame, err := json.Marshal(map[string]any{"unsubscribe": map[string]string{"requestId": requestID}})
	if err != nil {
		return err
	}
	return s.conn.Write(ctx, websocket.MessageText, frame)
}

// Close closes the connection.
func (s *stream) Close() error {
	return s.conn.Close(websocket.StatusNormalClosure, "")
}

// next reads one raw frame.
func (s *stream) next(ctx context.Context) ([]byte, error) {
	_, data, err := s.conn.Read(ctx)
	return data, err
}

// MarketsStream is the market-data stream (path /v1/ws/markets).
type MarketsStream struct{ stream }

// DialMarkets opens the market-data stream.
func (c *Client) DialMarkets(ctx context.Context) (*MarketsStream, error) {
	st, err := c.dialStream(ctx, "/v1/ws/markets")
	if err != nil {
		return nil, err
	}
	return &MarketsStream{stream: *st}, nil
}

// SubscribeBooks streams full-depth Book refreshes for slugs.
func (s *MarketsStream) SubscribeBooks(ctx context.Context, requestID string, slugs ...string) error {
	return s.subscribe(ctx, requestID, SubMarketData, slugs)
}

// SubscribeBBOs streams top-of-book updates for slugs.
func (s *MarketsStream) SubscribeBBOs(ctx context.Context, requestID string, slugs ...string) error {
	return s.subscribe(ctx, requestID, SubMarketDataLite, slugs)
}

// SubscribeTrades streams the tape for slugs.
func (s *MarketsStream) SubscribeTrades(ctx context.Context, requestID string, slugs ...string) error {
	return s.subscribe(ctx, requestID, SubTrade, slugs)
}

// StreamTrade is one print off the tape.
type StreamTrade struct {
	MarketSlug  string
	PriceC      int
	Quantity    float64
	Px          string // raw decimal price
	Qty         string // raw decimal quantity
	TradeTime   string // ISO-8601
	TakerIntent string // ORDER_INTENT_* of the aggressor
}

// MarketsMessage is one parsed frame off the market stream. Exactly one of
// Book, BBO, Trade is non-nil unless Heartbeat is set or Err is non-empty.
type MarketsMessage struct {
	RequestID string
	Heartbeat bool
	Err       string // server-reported subscription error
	Book      *Book
	BBO       *BBO
	Trade     *StreamTrade
}

// Next blocks for one frame and parses it.
func (s *MarketsStream) Next(ctx context.Context) (MarketsMessage, error) {
	data, err := s.next(ctx)
	if err != nil {
		return MarketsMessage{}, err
	}
	var raw struct {
		RequestID  string          `json:"requestId"`
		Heartbeat  json.RawMessage `json:"heartbeat"`
		Error      string          `json:"error"`
		MarketData *struct {
			MarketSlug string      `json:"marketSlug"`
			Bids       []wireLevel `json:"bids"`
			Offers     []wireLevel `json:"offers"`
			State      string      `json:"state"`
		} `json:"marketData"`
		MarketDataLite *struct {
			MarketSlug  string `json:"marketSlug"`
			BestBid     Amount `json:"bestBid"`
			BestAsk     Amount `json:"bestAsk"`
			LastTradePx Amount `json:"lastTradePx"`
		} `json:"marketDataLite"`
		Trade *struct {
			MarketSlug string `json:"marketSlug"`
			Price      Amount `json:"price"`
			Quantity   Amount `json:"quantity"`
			TradeTime  string `json:"tradeTime"`
			Taker      struct {
				Intent string `json:"intent"`
			} `json:"taker"`
		} `json:"trade"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return MarketsMessage{}, fmt.Errorf("polymarket: parse market frame: %w", err)
	}
	msg := MarketsMessage{RequestID: raw.RequestID, Heartbeat: raw.Heartbeat != nil, Err: raw.Error}
	switch {
	case raw.MarketData != nil:
		b := Book{MarketSlug: raw.MarketData.MarketSlug, State: raw.MarketData.State}
		for _, l := range raw.MarketData.Bids {
			b.Bids = append(b.Bids, l.level())
		}
		for _, l := range raw.MarketData.Offers {
			b.Offers = append(b.Offers, l.level())
		}
		msg.Book = &b
	case raw.MarketDataLite != nil:
		msg.BBO = &BBO{
			MarketSlug:  raw.MarketDataLite.MarketSlug,
			BidC:        raw.MarketDataLite.BestBid.Cents(),
			AskC:        raw.MarketDataLite.BestAsk.Cents(),
			LastTradeC:  raw.MarketDataLite.LastTradePx.Cents(),
			BidPx:       raw.MarketDataLite.BestBid.Value,
			AskPx:       raw.MarketDataLite.BestAsk.Value,
			LastTradePx: raw.MarketDataLite.LastTradePx.Value,
		}
	case raw.Trade != nil:
		qty, _ := strconv.ParseFloat(raw.Trade.Quantity.Value, 64)
		msg.Trade = &StreamTrade{
			MarketSlug:  raw.Trade.MarketSlug,
			PriceC:      raw.Trade.Price.Cents(),
			Quantity:    qty,
			Px:          raw.Trade.Price.Value,
			Qty:         raw.Trade.Quantity.Value,
			TradeTime:   raw.Trade.TradeTime,
			TakerIntent: raw.Trade.Taker.Intent,
		}
	}
	return msg, nil
}

// PrivateStream is the account stream (path /v1/ws/private).
type PrivateStream struct{ stream }

// DialPrivate opens the private stream.
func (c *Client) DialPrivate(ctx context.Context) (*PrivateStream, error) {
	st, err := c.dialStream(ctx, "/v1/ws/private")
	if err != nil {
		return nil, err
	}
	return &PrivateStream{stream: *st}, nil
}

// SubscribeOrders streams the open-order snapshot then per-execution
// updates, optionally filtered to slugs.
func (s *PrivateStream) SubscribeOrders(ctx context.Context, requestID string, slugs ...string) error {
	return s.subscribe(ctx, requestID, SubOrder, slugs)
}

// SubscribePositions streams the position snapshot then per-market updates,
// optionally filtered to slugs.
func (s *PrivateStream) SubscribePositions(ctx context.Context, requestID string, slugs ...string) error {
	return s.subscribe(ctx, requestID, SubPosition, slugs)
}

// SubscribeBalance streams the account balance.
func (s *PrivateStream) SubscribeBalance(ctx context.Context, requestID string) error {
	return s.subscribe(ctx, requestID, SubAccountBalance, nil)
}

// StreamExecution is one execution-report update on an order.
type StreamExecution struct {
	Type         string // EXECUTION_TYPE_*
	Order        OpenOrder
	LastShares   float64 // contracts in this execution (fills)
	LastPriceC   int     // price of this execution, cents (fills)
	LastPx       string  // raw decimal price of this execution
	TradeID      string
	Aggressor    bool
	TransactTime string // ISO-8601
	RejectReason string
}

// OrderSnapshot is the subscription-open replay of resting orders. EOF marks
// the last snapshot frame; updates follow.
type OrderSnapshot struct {
	Orders []OpenOrder
	EOF    bool
}

// PositionSnapshot is the subscription-open replay of positions by slug.
type PositionSnapshot struct {
	Positions map[string]Position
	EOF       bool
}

// PositionUpdate is one market's position after a change.
type PositionUpdate struct {
	MarketSlug string
	Position   Position
}

// PrivateMessage is one parsed frame off the private stream. Exactly one
// payload field is non-nil unless Heartbeat is set or Err is non-empty.
// Balance covers both the snapshot and update frames (same shape).
type PrivateMessage struct {
	RequestID        string
	Heartbeat        bool
	Err              string
	OrderSnapshot    *OrderSnapshot
	Execution        *StreamExecution
	PositionSnapshot *PositionSnapshot
	PositionUpdate   *PositionUpdate
	Balance          *Balance
}

func toPosition(w wirePosition, slug string) (Position, error) {
	fp, err := strconv.ParseFloat(w.NetPosition, 64)
	if err != nil {
		return Position{}, fmt.Errorf("polymarket: %s netPosition %q unparseable", slug, w.NetPosition)
	}
	return Position{
		Net: int(fp), NetFP: fp,
		CostC: w.Cost.Cents(), RealizedC: w.Realized.Cents(),
		Expired: w.Expired,
	}, nil
}

// Next blocks for one frame and parses it. A parse error on one frame's
// content (e.g. an unparseable position) is returned but consumes only that
// frame — the stream remains readable.
func (s *PrivateStream) Next(ctx context.Context) (PrivateMessage, error) {
	data, err := s.next(ctx)
	if err != nil {
		return PrivateMessage{}, err
	}
	var raw struct {
		RequestID string          `json:"requestId"`
		Heartbeat json.RawMessage `json:"heartbeat"`
		Error     string          `json:"error"`
		OrderSnap *struct {
			Orders []wireOrder `json:"orders"`
			EOF    bool        `json:"eof"`
		} `json:"orderSubscriptionSnapshot"`
		OrderUpdate *struct {
			Execution struct {
				Type              string    `json:"type"`
				Order             wireOrder `json:"order"`
				LastShares        string    `json:"lastShares"`
				LastPx            Amount    `json:"lastPx"`
				TradeID           string    `json:"tradeId"`
				Aggressor         bool      `json:"aggressor"`
				TransactTime      string    `json:"transactTime"`
				OrderRejectReason string    `json:"orderRejectReason"`
			} `json:"execution"`
		} `json:"orderSubscriptionUpdate"`
		PosSnap *struct {
			Positions map[string]wirePosition `json:"positions"`
			EOF       bool                    `json:"eof"`
		} `json:"positionSubscriptionSnapshot"`
		PosUpdate *struct {
			MarketSlug string       `json:"marketSlug"`
			Position   wirePosition `json:"position"`
		} `json:"positionSubscriptionUpdate"`
		BalSnap   *wireBalance `json:"accountBalanceSubscriptionSnapshot"`
		BalUpdate *wireBalance `json:"accountBalanceSubscriptionUpdate"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return PrivateMessage{}, fmt.Errorf("polymarket: parse private frame: %w", err)
	}
	msg := PrivateMessage{RequestID: raw.RequestID, Heartbeat: raw.Heartbeat != nil, Err: raw.Error}
	switch {
	case raw.OrderSnap != nil:
		snap := OrderSnapshot{EOF: raw.OrderSnap.EOF}
		for _, w := range raw.OrderSnap.Orders {
			snap.Orders = append(snap.Orders, w.open())
		}
		msg.OrderSnapshot = &snap
	case raw.OrderUpdate != nil:
		ex := raw.OrderUpdate.Execution
		// Fill sizes feed reconciliation: a malformed lastShares must fail
		// loudly, not become a 0-contract fill. Absent is fine (non-fills).
		var shares float64
		if ex.LastShares != "" {
			var perr error
			shares, perr = strconv.ParseFloat(ex.LastShares, 64)
			if perr != nil {
				return PrivateMessage{}, fmt.Errorf("polymarket: execution lastShares %q: %w", ex.LastShares, perr)
			}
		}
		msg.Execution = &StreamExecution{
			Type: ex.Type, Order: ex.Order.open(),
			LastShares: shares, LastPriceC: ex.LastPx.Cents(), LastPx: ex.LastPx.Value,
			TradeID: ex.TradeID, Aggressor: ex.Aggressor,
			TransactTime: ex.TransactTime, RejectReason: ex.OrderRejectReason,
		}
	case raw.PosSnap != nil:
		snap := PositionSnapshot{Positions: map[string]Position{}, EOF: raw.PosSnap.EOF}
		for slug, w := range raw.PosSnap.Positions {
			p, err := toPosition(w, slug)
			if err != nil {
				return PrivateMessage{}, err
			}
			snap.Positions[slug] = p
		}
		msg.PositionSnapshot = &snap
	case raw.PosUpdate != nil:
		p, err := toPosition(raw.PosUpdate.Position, raw.PosUpdate.MarketSlug)
		if err != nil {
			return PrivateMessage{}, err
		}
		msg.PositionUpdate = &PositionUpdate{MarketSlug: raw.PosUpdate.MarketSlug, Position: p}
	case raw.BalSnap != nil:
		msg.Balance = raw.BalSnap.balance()
	case raw.BalUpdate != nil:
		msg.Balance = raw.BalUpdate.balance()
	}
	return msg, nil
}

type wireBalance struct {
	Balance     float64 `json:"balance"`
	BuyingPower float64 `json:"buyingPower"`
}

func (w wireBalance) balance() *Balance {
	return &Balance{
		BalanceC:     int64(math.Round(w.Balance * 100)),
		BuyingPowerC: int64(math.Round(w.BuyingPower * 100)),
	}
}
