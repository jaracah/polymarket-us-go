package polymarket

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// wsTestServer accepts one WebSocket at path, verifies the signed handshake,
// echoes back each frame listed in replies after reading one inbound frame
// (the subscribe), then keeps the socket open.
func wsTestServer(t *testing.T, path string, gotSubscribe chan<- []byte, replies []string) *httptest.Server {
	t.Helper()
	pub := ed25519.NewKeyFromSeed(testSeed).Public().(ed25519.PublicKey)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			t.Errorf("dial path = %q, want %q", r.URL.Path, path)
		}
		ts := r.Header.Get("X-PM-Timestamp")
		sig, _ := base64.StdEncoding.DecodeString(r.Header.Get("X-PM-Signature"))
		if !ed25519.Verify(pub, []byte(ts+"GET"+path), sig) {
			t.Error("handshake signature does not verify over GET+path")
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		ctx := r.Context()
		_, frame, err := conn.Read(ctx)
		if err != nil {
			return
		}
		gotSubscribe <- frame
		for _, reply := range replies {
			if err := conn.Write(ctx, websocket.MessageText, []byte(reply)); err != nil {
				return
			}
		}
		// Hold the connection open until the client closes.
		conn.Read(ctx)
	}))
}

func wsClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	s := testSigner(t, base64.StdEncoding.EncodeToString(testSeed))
	// Transport-only client: the dialer must not inherit a Timeout.
	hc := &http.Client{Transport: srv.Client().Transport}
	return NewAuthedClient(hc, s, srv.URL, srv.URL)
}

func TestMarketsStream(t *testing.T) {
	subscribed := make(chan []byte, 1)
	srv := wsTestServer(t, "/v1/ws/markets", subscribed, []string{
		`{"heartbeat":{}}`,
		`{"requestId":"md-1","marketData":{"marketSlug":"btc-100k","state":"MARKET_STATE_OPEN",
			"bids":[{"px":{"value":"0.54"},"qty":"120"}],"offers":[{"px":{"value":"0.56"},"qty":"80"}]}}`,
		`{"requestId":"t-1","trade":{"marketSlug":"btc-100k","price":{"value":"0.55"},
			"quantity":{"value":"25"},"tradeTime":"2026-08-01T12:00:00Z","taker":{"intent":"ORDER_INTENT_BUY_LONG"}}}`,
		`{"requestId":"md-1","error":"subscription limit"}`,
	})
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := wsClient(t, srv).DialMarkets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.SubscribeBooks(ctx, "md-1", "btc-100k"); err != nil {
		t.Fatal(err)
	}
	var sub wsSubscribe
	if err := json.Unmarshal(<-subscribed, &sub); err != nil {
		t.Fatal(err)
	}
	if sub.Subscribe.RequestID != "md-1" || sub.Subscribe.SubscriptionType != SubMarketData ||
		len(sub.Subscribe.MarketSlugs) != 1 {
		t.Errorf("subscribe frame = %+v", sub)
	}

	m1, err := st.Next(ctx)
	if err != nil || !m1.Heartbeat {
		t.Fatalf("frame 1 = %+v, %v — want heartbeat", m1, err)
	}
	m2, err := st.Next(ctx)
	if err != nil || m2.Book == nil {
		t.Fatalf("frame 2 = %+v, %v — want book", m2, err)
	}
	if m2.Book.Bids[0] != (Level{PriceC: 54, Size: 120, Px: "0.54", Qty: "120"}) || m2.Book.State != "MARKET_STATE_OPEN" {
		t.Errorf("book = %+v", m2.Book)
	}
	m3, err := st.Next(ctx)
	if err != nil || m3.Trade == nil {
		t.Fatalf("frame 3 = %+v, %v — want trade", m3, err)
	}
	if m3.Trade.PriceC != 55 || m3.Trade.Quantity != 25 || m3.Trade.Px != "0.55" || m3.Trade.TakerIntent != IntentBuyLong {
		t.Errorf("trade = %+v", m3.Trade)
	}
	m4, err := st.Next(ctx)
	if err != nil || m4.Err != "subscription limit" || m4.RequestID != "md-1" {
		t.Fatalf("frame 4 = %+v, %v — want server error surfaced", m4, err)
	}
}

func TestPrivateStream(t *testing.T) {
	subscribed := make(chan []byte, 1)
	srv := wsTestServer(t, "/v1/ws/private", subscribed, []string{
		`{"requestId":"ord-1","orderSubscriptionSnapshot":{"eof":true,"orders":[
			{"id":"o1","marketSlug":"mkt-a","price":{"value":"0.42"},"quantity":10,"leavesQuantity":10,"state":"ORDER_STATE_NEW"}]}}`,
		`{"requestId":"ord-1","orderSubscriptionUpdate":{"execution":{
			"type":"EXECUTION_TYPE_FILL","lastShares":"10","lastPx":{"value":"0.42"},"tradeId":"tr-9","aggressor":true,
			"order":{"id":"o1","marketSlug":"mkt-a","cumQuantity":10,"leavesQuantity":0,"state":"ORDER_STATE_FILLED","avgPx":{"value":"0.42"}}}}}`,
		`{"requestId":"pos-1","positionSubscriptionUpdate":{"marketSlug":"mkt-a","position":{
			"netPosition":"10","cost":{"value":"4.20"},"realized":{"value":"0"}}}}`,
		`{"requestId":"bal-1","accountBalanceSubscriptionUpdate":{"balance":95.80,"buyingPower":90.00}}`,
	})
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := wsClient(t, srv).DialPrivate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.SubscribeOrders(ctx, "ord-1"); err != nil {
		t.Fatal(err)
	}
	var sub wsSubscribe
	if err := json.Unmarshal(<-subscribed, &sub); err != nil {
		t.Fatal(err)
	}
	if sub.Subscribe.SubscriptionType != SubOrder || sub.Subscribe.MarketSlugs != nil {
		t.Errorf("subscribe frame = %+v", sub)
	}

	m1, err := st.Next(ctx)
	if err != nil || m1.OrderSnapshot == nil || !m1.OrderSnapshot.EOF || len(m1.OrderSnapshot.Orders) != 1 {
		t.Fatalf("frame 1 = %+v, %v — want order snapshot", m1, err)
	}
	m2, err := st.Next(ctx)
	if err != nil || m2.Execution == nil {
		t.Fatalf("frame 2 = %+v, %v — want execution", m2, err)
	}
	ex := m2.Execution
	if ex.Type != "EXECUTION_TYPE_FILL" || ex.LastShares != 10 || ex.LastQty != "10" ||
		ex.LastPriceC != 42 || ex.TradeID != "tr-9" || !ex.Aggressor || ex.Order.State != StateFilled {
		t.Errorf("execution = %+v", ex)
	}
	m3, err := st.Next(ctx)
	if err != nil || m3.PositionUpdate == nil {
		t.Fatalf("frame 3 = %+v, %v — want position update", m3, err)
	}
	if p := m3.PositionUpdate; p.MarketSlug != "mkt-a" || p.Position.Net != 10 || p.Position.CostC != 420 {
		t.Errorf("position update = %+v", p)
	}
	m4, err := st.Next(ctx)
	if err != nil || m4.Balance == nil {
		t.Fatalf("frame 4 = %+v, %v — want balance", m4, err)
	}
	if m4.Balance.BalanceC != 9580 || m4.Balance.BuyingPowerC != 9000 {
		t.Errorf("balance = %+v", m4.Balance)
	}
}

func TestPrivateStreamBadLastSharesIsError(t *testing.T) {
	subscribed := make(chan []byte, 1)
	srv := wsTestServer(t, "/v1/ws/private", subscribed, []string{
		`{"requestId":"ord-1","orderSubscriptionUpdate":{"execution":{
			"type":"EXECUTION_TYPE_FILL","lastShares":"garbage","lastPx":{"value":"0.42"},
			"order":{"id":"o1","marketSlug":"mkt-a","state":"ORDER_STATE_FILLED"}}}}`,
		`{"requestId":"bal-1","accountBalanceSubscriptionUpdate":{"balance":95.80,"buyingPower":90.00}}`,
	})
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := wsClient(t, srv).DialPrivate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SubscribeOrders(ctx, "ord-1"); err != nil {
		t.Fatal(err)
	}
	<-subscribed

	if _, err := st.Next(ctx); err == nil {
		t.Fatal("malformed lastShares parsed as a fill, want error")
	}
	// A content error must leave the stream usable.
	m, err := st.Next(ctx)
	if err != nil || m.Balance == nil {
		t.Fatalf("frame after content error = %+v, %v — want balance", m, err)
	}
}
