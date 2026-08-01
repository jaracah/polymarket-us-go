package polymarket

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAmountCents(t *testing.T) {
	cases := map[string]int{
		"":       0,
		"0.55":   55,
		"0.5":    50,
		"0.999":  100,
		"1.00":   100,
		"junk":   0,
		"-0.05":  -5, // negative (realized loss) must round away from zero
		"-0.999": -100,
	}
	for in, want := range cases {
		if got := (Amount{Value: in}).Cents(); got != want {
			t.Errorf("Amount{%q}.Cents() = %d, want %d", in, got, want)
		}
	}
	if got := USD(55).Value; got != "0.55" {
		t.Errorf("USD(55).Value = %q", got)
	}
	if got := USD(5).Value; got != "0.05" {
		t.Errorf("USD(5).Value = %q", got)
	}
	if got := USD(-5).Value; got != "-0.05" {
		t.Errorf("USD(-5).Value = %q", got)
	}
}

// newTestPair returns an authed client whose gateway AND api hosts both
// point at a test server running mux.
func newTestPair(t *testing.T, mux *http.ServeMux) *Client {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s := testSigner(t, base64.StdEncoding.EncodeToString(testSeed))
	return NewAuthedClient(srv.Client(), s, srv.URL, srv.URL)
}

func TestPublicEndpointsUnsignedAndParsed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/markets/btc-100k/book", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-PM-Access-Key") != "" {
			t.Error("public endpoint got auth headers")
		}
		fmt.Fprint(w, `{"marketSlug":"btc-100k","state":"MARKET_STATE_OPEN",
			"bids":[{"px":{"value":"0.54","currency":"USD"},"qty":"120"}],
			"offers":[{"px":{"value":"0.56","currency":"USD"},"qty":"80.5"}]}`)
	})
	mux.HandleFunc("/v1/markets/btc-100k/bbo", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"marketSlug":"btc-100k","bestBid":{"value":"0.54"},"bestAsk":{"value":"0.56"},"lastTradePx":{"value":"0.55"}}`)
	})
	c := newTestPair(t, mux)

	book, err := c.FetchBook(context.Background(), "btc-100k")
	if err != nil {
		t.Fatal(err)
	}
	if book.State != "MARKET_STATE_OPEN" || len(book.Bids) != 1 || len(book.Offers) != 1 {
		t.Fatalf("book = %+v", book)
	}
	if book.Bids[0] != (Level{PriceC: 54, Size: 120, Px: "0.54", Qty: "120"}) {
		t.Errorf("bid = %+v", book.Bids[0])
	}
	if book.Offers[0] != (Level{PriceC: 56, Size: 80.5, Px: "0.56", Qty: "80.5"}) {
		t.Errorf("offer = %+v", book.Offers[0])
	}

	bbo, err := c.FetchBBO(context.Background(), "btc-100k")
	if err != nil {
		t.Fatal(err)
	}
	if bbo.BidC != 54 || bbo.AskC != 56 || bbo.LastTradeC != 55 || bbo.BidPx != "0.54" {
		t.Errorf("bbo = %+v", bbo)
	}
}

func TestAuthedEndpointSignedAndVerifiable(t *testing.T) {
	pub := ed25519.NewKeyFromSeed(testSeed).Public().(ed25519.PublicKey)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/account/balances", func(w http.ResponseWriter, r *http.Request) {
		ts := r.Header.Get("X-PM-Timestamp")
		sig, _ := base64.StdEncoding.DecodeString(r.Header.Get("X-PM-Signature"))
		if !ed25519.Verify(pub, []byte(ts+r.Method+r.URL.Path), sig) {
			t.Error("request signature does not verify server-side")
		}
		fmt.Fprint(w, `{"balances":[{"currentBalance":123.45,"buyingPower":100.10,"currency":"USD"}]}`)
	})
	c := newTestPair(t, mux)

	b, err := c.Balance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if b.BalanceC != 12345 || b.BuyingPowerC != 10010 {
		t.Errorf("balance = %+v", b)
	}
}

func TestTradingRequiresSigner(t *testing.T) {
	c := NewClient(nil)
	if _, err := c.Balance(context.Background()); err == nil || !strings.Contains(err.Error(), "NewAuthedClient") {
		t.Errorf("unauthed Balance error = %v", err)
	}
	if _, err := c.DialMarkets(context.Background()); err == nil {
		t.Error("unauthed DialMarkets succeeded")
	}
}

func TestCreateOrder(t *testing.T) {
	var got createOrderRequest
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/orders", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(w, `{"id":"ord-1","executions":[
			{"type":"EXECUTION_TYPE_NEW","order":{"state":"ORDER_STATE_NEW","cumQuantity":0,"leavesQuantity":100}},
			{"type":"EXECUTION_TYPE_FILL","order":{"state":"ORDER_STATE_FILLED","cumQuantity":100,"leavesQuantity":0,"avgPx":{"value":"0.55"}}}]}`)
	})
	c := newTestPair(t, mux)

	res, err := c.CreateOrder(context.Background(), Order{
		MarketSlug: "btc-100k", Intent: IntentBuyLong, Quantity: 100, PriceC: 55,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != "ORDER_TYPE_LIMIT" || got.Tif != TIFImmediateOrCancel || !got.SynchronousExecution {
		t.Errorf("wire request = %+v", got)
	}
	if got.Price == nil || got.Price.Value != "0.55" || got.Quantity != 100 {
		t.Errorf("wire price/quantity = %+v", got)
	}
	want := OrderResult{OrderID: "ord-1", FillCount: 100, Remaining: 0, AvgPriceC: 55, AvgPx: "0.55", State: StateFilled}
	if res != want {
		t.Errorf("result = %+v, want %+v", res, want)
	}
}

func TestCreateOrderValidation(t *testing.T) {
	c := newTestPair(t, http.NewServeMux()) // any request would 404; none should be sent
	bad := []Order{
		{Intent: IntentBuyLong, Quantity: 1, PriceC: 50}, // missing slug
		{MarketSlug: "m", Intent: "ORDER_INTENT_NONSENSE", Quantity: 1, PriceC: 50},
		{MarketSlug: "m", Intent: IntentBuyLong, Quantity: 0, PriceC: 50},
		{MarketSlug: "m", Intent: IntentBuyLong, Quantity: 1, PriceC: 0},
		{MarketSlug: "m", Intent: IntentBuyLong, Quantity: 1, PriceC: 100},
		{MarketSlug: "m", Intent: IntentBuyLong, Quantity: 1, PriceC: 50, TimeInForce: "TIME_IN_FORCE_GOOD_TILL_DATE"},
		{MarketSlug: "m", Intent: IntentBuyLong, Quantity: 1, PriceC: 50, PostOnly: true}, // IOC + post-only
	}
	for i, o := range bad {
		if _, err := c.CreateOrder(context.Background(), o); err == nil {
			t.Errorf("bad order %d accepted: %+v", i, o)
		}
	}
}

func TestCreateOrderRejectionIsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/orders", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"ord-2","executions":[
			{"type":"EXECUTION_TYPE_REJECTED","orderRejectReason":"insufficient buying power","order":{"state":"ORDER_STATE_REJECTED"}}]}`)
	})
	c := newTestPair(t, mux)
	_, err := c.CreateOrder(context.Background(), Order{
		MarketSlug: "btc-100k", Intent: IntentBuyLong, Quantity: 100, PriceC: 55,
	})
	if err == nil || !strings.Contains(err.Error(), "insufficient buying power") {
		t.Errorf("rejection error = %v", err)
	}
}

func TestCreateOrderFillWithoutPriceIsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/orders", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"ord-3","executions":[
			{"type":"EXECUTION_TYPE_FILL","order":{"state":"ORDER_STATE_FILLED","cumQuantity":10,"leavesQuantity":0}}]}`)
	})
	c := newTestPair(t, mux)
	_, err := c.CreateOrder(context.Background(), Order{
		MarketSlug: "btc-100k", Intent: IntentBuyLong, Quantity: 10, PriceC: 55,
	})
	if err == nil || !strings.Contains(err.Error(), "avgPx") {
		t.Errorf("fill-without-price error = %v", err)
	}
}

func TestCreateOrderNeverRetries429(t *testing.T) {
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/orders", func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c := newTestPair(t, mux)
	_, err := c.CreateOrder(context.Background(), Order{
		MarketSlug: "btc-100k", Intent: IntentBuyLong, Quantity: 1, PriceC: 50,
	})
	if err == nil {
		t.Fatal("429 order create returned nil error")
	}
	if calls != 1 {
		t.Errorf("order create sent %d times; without a client order id a retry is a second order", calls)
	}
}

func TestGetRetries429(t *testing.T) {
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/markets/x/bbo", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"marketSlug":"x"}`)
	})
	c := newTestPair(t, mux)
	if _, err := c.FetchBBO(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestErrorBodySurfaced(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/order/ord-9/cancel", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"message":"order not open"}`)
	})
	c := newTestPair(t, mux)
	err := c.CancelOrder(context.Background(), "ord-9", "btc-100k")
	if err == nil || !strings.Contains(err.Error(), "order not open") {
		t.Errorf("error = %v, want exchange reason surfaced", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("error = %#v, want *APIError with StatusCode 400", err)
	}
}

func TestPositionsFollowsCursor(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/portfolio/positions", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			fmt.Fprint(w, `{"positions":{"mkt-a":{"netPosition":"100","cost":{"value":"55.00"},"realized":{"value":"0"}}},"nextCursor":"c2","eof":false}`)
			return
		}
		fmt.Fprint(w, `{"positions":{"mkt-b":{"netPosition":"-40.5","cost":{"value":"10.00"},"realized":{"value":"1.25"}}},"eof":true}`)
	})
	c := newTestPair(t, mux)
	pos, err := c.Positions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pos) != 2 {
		t.Fatalf("positions = %+v", pos)
	}
	if p := pos["mkt-a"]; p.Net != 100 || p.CostC != 5500 {
		t.Errorf("mkt-a = %+v", p)
	}
	if p := pos["mkt-b"]; p.Net != -40 || p.NetFP != -40.5 || p.RealizedC != 125 {
		t.Errorf("mkt-b = %+v", p)
	}
}

func TestPositionsUnparseableNetIsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/portfolio/positions", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"positions":{"mkt-a":{"netPosition":"garbage"}},"eof":true}`)
	})
	c := newTestPair(t, mux)
	if _, err := c.Positions(context.Background()); err == nil {
		t.Error("garbage netPosition did not error — would fabricate a flat position")
	}
}

func TestOpenOrdersAndCancelAll(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/orders/open", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query()["slugs"]; len(got) != 2 {
			t.Errorf("slugs query = %v", got)
		}
		fmt.Fprint(w, `{"orders":[{"id":"o1","marketSlug":"mkt-a","intent":"ORDER_INTENT_BUY_LONG",
			"price":{"value":"0.42"},"quantity":10,"cumQuantity":4,"leavesQuantity":6,
			"state":"ORDER_STATE_PARTIALLY_FILLED","tif":"TIME_IN_FORCE_GOOD_TILL_CANCEL"}]}`)
	})
	mux.HandleFunc("/v1/orders/open/cancel", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"canceledOrderIds":["o1","o2"]}`)
	})
	c := newTestPair(t, mux)

	orders, err := c.OpenOrders(context.Background(), "mkt-a", "mkt-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 1 {
		t.Fatalf("orders = %+v", orders)
	}
	o := orders[0]
	if o.PriceC != 42 || o.CumQuantity != 4 || o.Remaining != 6 || o.State != StatePartiallyFilled {
		t.Errorf("order = %+v", o)
	}

	ids, err := c.CancelAllOrders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Errorf("canceled ids = %v", ids)
	}
}

func TestNewAuthedClientTrimsTrailingSlash(t *testing.T) {
	// Request paths are appended with their own leading slash; an untrimmed
	// base URL would produce "//"-prefixed paths and, on the WS handshake,
	// desync the request path from the signed path.
	c := NewAuthedClient(nil, nil, "https://gw.example/", "https://api.example//")
	if c.gatewayURL != "https://gw.example" || c.apiURL != "https://api.example" {
		t.Errorf("base URLs = %q / %q, want trailing slashes trimmed", c.gatewayURL, c.apiURL)
	}
}
