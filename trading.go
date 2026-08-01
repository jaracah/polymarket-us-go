// Authenticated trading endpoints: orders, positions, balance. All of these
// require a Signer (NewAuthedClient); the read-only market-data client never
// touches them. They hit the api host, not the public gateway.
//
// The exchange has four order intents because LONG and SHORT are separate
// instruments: BUY_LONG/SELL_LONG open and close a long (yes) position,
// BUY_SHORT/SELL_SHORT the short side. Prices always quote the named
// instrument — there is no 100−p inversion anywhere.
//
// Order placement does NOT retry. This API has no client order id, so a
// retried create is a brand-new order to the exchange — after a 429 or an
// ambiguous transport failure the only safe recovery is to reconcile via
// OpenOrders/Positions, which is the caller's job. Cancels and reads are
// idempotent and keep the retry loop.
package polymarket

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
)

// Order intents. BUY_LONG opens/extends a long (yes) position; SELL_LONG
// unwinds one. The SHORT pair does the same for the short (no) side.
const (
	IntentBuyLong   = "ORDER_INTENT_BUY_LONG"
	IntentSellLong  = "ORDER_INTENT_SELL_LONG"
	IntentBuyShort  = "ORDER_INTENT_BUY_SHORT"
	IntentSellShort = "ORDER_INTENT_SELL_SHORT"
)

// Time-in-force values (the API also offers GOOD_TILL_DATE, which this
// client does not support).
const (
	TIFImmediateOrCancel = "TIME_IN_FORCE_IMMEDIATE_OR_CANCEL"
	TIFGoodTillCancel    = "TIME_IN_FORCE_GOOD_TILL_CANCEL"
	TIFFillOrKill        = "TIME_IN_FORCE_FILL_OR_KILL"
)

// Order states (subset — the full machine includes PENDING_* transients).
const (
	StateNew             = "ORDER_STATE_NEW"
	StatePartiallyFilled = "ORDER_STATE_PARTIALLY_FILLED"
	StateFilled          = "ORDER_STATE_FILLED"
	StateCanceled        = "ORDER_STATE_CANCELED"
	StateRejected        = "ORDER_STATE_REJECTED"
	StateExpired         = "ORDER_STATE_EXPIRED"
)

// Order is a limit order in integer cents and whole contracts. The zero
// TimeInForce is IOC. PostOnly (the API's participateDontInitiate) makes
// the exchange reject a placement that would cross instead of taking; it
// requires a resting time-in-force.
type Order struct {
	MarketSlug  string
	Intent      string // IntentBuyLong | IntentSellLong | IntentBuyShort | IntentSellShort
	Quantity    int    // whole contracts, > 0
	PriceC      int    // limit price in cents, 1..99
	TimeInForce string // "" (= TIFImmediateOrCancel), TIFGoodTillCancel, or TIFFillOrKill
	PostOnly    bool
}

// OrderResult is the exchange's synchronous answer to an order (the client
// sends synchronousExecution so IOC outcomes come back in the response).
type OrderResult struct {
	OrderID   string
	FillCount int    // contracts filled so far (cumQuantity)
	Remaining int    // contracts still open (leavesQuantity)
	AvgPriceC int    // volume-weighted average fill price, cents (0 when no fill)
	AvgPx     string // raw decimal average fill price ("" when no fill)
	State     string // final ORDER_STATE_* seen, "" if the response carried none
}

// wireOrder mirrors the API's Order object, only the fields we use.
type wireOrder struct {
	ID             string `json:"id"`
	MarketSlug     string `json:"marketSlug"`
	Intent         string `json:"intent"`
	Type           string `json:"type"`
	Price          Amount `json:"price"`
	Quantity       int    `json:"quantity"`
	CumQuantity    int    `json:"cumQuantity"`
	LeavesQuantity int    `json:"leavesQuantity"`
	Tif            string `json:"tif"`
	State          string `json:"state"`
	AvgPx          Amount `json:"avgPx"`
	CreateTime     string `json:"createTime"`
}

// OpenOrder is one resting or recently-terminal order.
type OpenOrder struct {
	ID          string
	MarketSlug  string
	Intent      string
	PriceC      int
	Quantity    int
	CumQuantity int
	Remaining   int
	TimeInForce string
	State       string
	AvgPriceC   int
	AvgPx       string // raw decimal average fill price ("" when no fill)
	CreateTime  string // ISO-8601
}

func (w wireOrder) open() OpenOrder {
	return OpenOrder{
		ID: w.ID, MarketSlug: w.MarketSlug, Intent: w.Intent,
		PriceC: w.Price.Cents(), Quantity: w.Quantity,
		CumQuantity: w.CumQuantity, Remaining: w.LeavesQuantity,
		TimeInForce: w.Tif, State: w.State,
		AvgPriceC: w.AvgPx.Cents(), AvgPx: w.AvgPx.Value,
		CreateTime: w.CreateTime,
	}
}

type wireExecution struct {
	Type              string    `json:"type"`
	Order             wireOrder `json:"order"`
	OrderRejectReason string    `json:"orderRejectReason"`
}

type orderExecResponse struct {
	ID         string          `json:"id"`
	Executions []wireExecution `json:"executions"`
}

// result reduces an execution stream to the final order snapshot. A REJECTED
// execution becomes an error — a rejected order must never read as a quiet
// zero-fill.
func (r orderExecResponse) result(what string) (OrderResult, error) {
	out := OrderResult{OrderID: r.ID}
	for _, ex := range r.Executions {
		if ex.Type == "EXECUTION_TYPE_REJECTED" {
			return OrderResult{}, fmt.Errorf("%s: rejected: %s", what, ex.OrderRejectReason)
		}
		// Executions arrive oldest-first; each carries the order's state
		// after it, so the last one is the answer.
		out.FillCount = ex.Order.CumQuantity
		out.Remaining = ex.Order.LeavesQuantity
		out.State = ex.Order.State
		if ex.Order.CumQuantity > 0 {
			out.AvgPriceC = ex.Order.AvgPx.Cents()
			out.AvgPx = ex.Order.AvgPx.Value
		}
	}
	if out.FillCount > 0 && out.AvgPriceC == 0 {
		// The fill price feeds real-money bookkeeping: refuse to report
		// contracts at 0¢ and silently corrupt a caller's cost basis.
		return OrderResult{}, fmt.Errorf("%s: filled %d but no usable avgPx in response", what, out.FillCount)
	}
	return out, nil
}

type createOrderRequest struct {
	MarketSlug              string  `json:"marketSlug"`
	Intent                  string  `json:"intent"`
	Type                    string  `json:"type"`
	Price                   *Amount `json:"price,omitempty"`
	Quantity                int     `json:"quantity"`
	Tif                     string  `json:"tif"`
	ParticipateDontInitiate bool    `json:"participateDontInitiate,omitempty"`
	SynchronousExecution    bool    `json:"synchronousExecution"`
}

// CreateOrder places o (always a limit order — market orders need
// slippage-tolerance machinery this client does not implement) and reports
// what executed synchronously. Single attempt, never retried: with no
// client order id, a retry is a second order (see the package comment in
// this file).
func (c *Client) CreateOrder(ctx context.Context, o Order) (OrderResult, error) {
	if o.MarketSlug == "" {
		return OrderResult{}, fmt.Errorf("polymarket: bad order: missing market slug")
	}
	switch o.Intent {
	case IntentBuyLong, IntentSellLong, IntentBuyShort, IntentSellShort:
	default:
		return OrderResult{}, fmt.Errorf("polymarket: bad order intent %q", o.Intent)
	}
	if o.Quantity <= 0 || o.PriceC < 1 || o.PriceC > 99 {
		return OrderResult{}, fmt.Errorf("polymarket: bad order quantity=%d priceC=%d", o.Quantity, o.PriceC)
	}
	tif := o.TimeInForce
	if tif == "" {
		tif = TIFImmediateOrCancel
	}
	if tif != TIFImmediateOrCancel && tif != TIFGoodTillCancel && tif != TIFFillOrKill {
		return OrderResult{}, fmt.Errorf("polymarket: bad time-in-force %q", tif)
	}
	if o.PostOnly && tif != TIFGoodTillCancel {
		return OrderResult{}, fmt.Errorf("polymarket: PostOnly requires a resting time-in-force")
	}
	price := USD(o.PriceC)
	req := createOrderRequest{
		MarketSlug: o.MarketSlug, Intent: o.Intent, Type: "ORDER_TYPE_LIMIT",
		Price: &price, Quantity: o.Quantity, Tif: tif,
		ParticipateDontInitiate: o.PostOnly, SynchronousExecution: true,
	}
	what := "create order " + o.MarketSlug
	var resp orderExecResponse
	if err := c.post(ctx, what, "/v1/orders", req, &resp, 1); err != nil {
		return OrderResult{}, err
	}
	return resp.result(what)
}

// OpenOrders returns open orders, optionally filtered to the given market
// slugs.
func (c *Client) OpenOrders(ctx context.Context, slugs ...string) ([]OpenOrder, error) {
	path := "/v1/orders/open"
	if len(slugs) > 0 {
		q := url.Values{"slugs": slugs}
		path += "?" + q.Encode()
	}
	var resp struct {
		Orders []wireOrder `json:"orders"`
	}
	if err := c.authedGet(ctx, "open orders", path, &resp); err != nil {
		return nil, err
	}
	out := make([]OpenOrder, 0, len(resp.Orders))
	for _, w := range resp.Orders {
		out = append(out, w.open())
	}
	return out, nil
}

// FetchOrder returns one order by id.
func (c *Client) FetchOrder(ctx context.Context, orderID string) (OpenOrder, error) {
	var resp struct {
		Order wireOrder `json:"order"`
	}
	err := c.authedGet(ctx, "order "+orderID, "/v1/order/"+url.PathEscape(orderID), &resp)
	return resp.Order.open(), err
}

// CancelOrder cancels one resting order. The API requires the order's market
// slug alongside its id.
func (c *Client) CancelOrder(ctx context.Context, orderID, marketSlug string) error {
	body := map[string]string{"marketSlug": marketSlug}
	return c.post(ctx, "cancel order "+orderID, "/v1/order/"+url.PathEscape(orderID)+"/cancel", body, nil, 4)
}

// CancelAllOrders cancels every open order (or, with slugs, every open order
// in those markets) and returns the canceled ids.
func (c *Client) CancelAllOrders(ctx context.Context, slugs ...string) ([]string, error) {
	body := map[string]any{}
	if len(slugs) > 0 {
		body["slugs"] = slugs
	}
	var resp struct {
		CanceledOrderIDs []string `json:"canceledOrderIds"`
	}
	if err := c.post(ctx, "cancel all orders", "/v1/orders/open/cancel", body, &resp, 4); err != nil {
		return nil, err
	}
	return resp.CanceledOrderIDs, nil
}

// ClosePosition asks the exchange to flatten the position in one market at
// market price and reports what executed. Like CreateOrder it is a single
// attempt — it places an order.
func (c *Client) ClosePosition(ctx context.Context, marketSlug string) (OrderResult, error) {
	body := map[string]any{"marketSlug": marketSlug, "synchronousExecution": true}
	what := "close position " + marketSlug
	var resp orderExecResponse
	if err := c.post(ctx, what, "/v1/order/close-position", body, &resp, 1); err != nil {
		return OrderResult{}, err
	}
	return resp.result(what)
}

// Position is one market's exchange-side position. Net is positive for a
// long holding and negative for a short one, matching the sign of the
// exchange's netPosition.
type Position struct {
	Net       int
	NetFP     float64 // netPosition verbatim; fractional while partially unwound
	CostC     int     // aggregate cost basis, cents
	RealizedC int     // realized P&L, cents
	Expired   bool
}

type wirePosition struct {
	NetPosition string `json:"netPosition"`
	Cost        Amount `json:"cost"`
	Realized    Amount `json:"realized"`
	Expired     bool   `json:"expired"`
}

// Positions returns all positions keyed by market slug, following the
// cursor until the API reports eof.
func (c *Client) Positions(ctx context.Context) (map[string]Position, error) {
	out := map[string]Position{}
	cursor := ""
	for {
		path := "/v1/portfolio/positions"
		if cursor != "" {
			path += "?cursor=" + url.QueryEscape(cursor)
		}
		var resp struct {
			Positions  map[string]wirePosition `json:"positions"`
			NextCursor string                  `json:"nextCursor"`
			EOF        bool                    `json:"eof"`
		}
		if err := c.authedGet(ctx, "positions", path, &resp); err != nil {
			return nil, err
		}
		for slug, w := range resp.Positions {
			fp, err := strconv.ParseFloat(w.NetPosition, 64)
			if err != nil {
				// Real-money reconciliation input: surface drift, don't
				// fabricate a flat position.
				return nil, fmt.Errorf("positions: %s netPosition %q unparseable", slug, w.NetPosition)
			}
			out[slug] = Position{
				Net: int(fp), NetFP: fp,
				CostC: w.Cost.Cents(), RealizedC: w.Realized.Cents(),
				Expired: w.Expired,
			}
		}
		if resp.EOF || resp.NextCursor == "" || len(resp.Positions) == 0 {
			return out, nil
		}
		cursor = resp.NextCursor
	}
}

// Balance is the account's cash state in cents.
type Balance struct {
	BalanceC     int64 // settled cash
	BuyingPowerC int64
}

// Balance returns the account's first (USD) balance. It also serves as a
// cheap authenticated connectivity probe at startup.
func (c *Client) Balance(ctx context.Context) (Balance, error) {
	var resp struct {
		Balances []struct {
			CurrentBalance float64 `json:"currentBalance"`
			BuyingPower    float64 `json:"buyingPower"`
		} `json:"balances"`
	}
	if err := c.authedGet(ctx, "balances", "/v1/account/balances", &resp); err != nil {
		return Balance{}, err
	}
	if len(resp.Balances) == 0 {
		return Balance{}, fmt.Errorf("balances: empty response")
	}
	b := resp.Balances[0]
	return Balance{
		BalanceC:     int64(math.Round(b.CurrentBalance * 100)),
		BuyingPowerC: int64(math.Round(b.BuyingPower * 100)),
	}, nil
}

// authedGet issues a signed GET to the api host.
func (c *Client) authedGet(ctx context.Context, what, path string, out any) error {
	return c.do(ctx, http.MethodGet, c.apiURL, path, what, nil, out, true, 4)
}

// post issues a signed JSON POST to the api host. maxAttempts is 1 for
// anything that places an order.
func (c *Client) post(ctx context.Context, what, path string, body, out any, maxAttempts int) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("%s: encode body: %w", what, err)
	}
	return c.do(ctx, http.MethodPost, c.apiURL, path, what, payload, out, true, maxAttempts)
}
