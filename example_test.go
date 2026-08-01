package polymarket_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"

	polymarket "github.com/jaracah/polymarket-us-go"
)

// Read public market data with no credentials.
func ExampleNewClient() {
	c := polymarket.NewClient(nil)

	bbo, err := c.FetchBBO(context.Background(), "some-market-slug")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("bid %d¢ ask %d¢ (raw %s/%s)\n", bbo.BidC, bbo.AskC, bbo.BidPx, bbo.AskPx)
}

// Place a limit order with an authenticated client.
func ExampleClient_CreateOrder() {
	signer, err := polymarket.NewSigner(
		os.Getenv("POLYMARKET_KEY_ID"),
		os.Getenv("POLYMARKET_SECRET_KEY"),
	)
	if err != nil {
		log.Fatal(err)
	}
	c := polymarket.NewAuthedClient(nil, signer, "", "")

	res, err := c.CreateOrder(context.Background(), polymarket.Order{
		MarketSlug: "some-market-slug",
		Intent:     polymarket.IntentBuyLong,
		Quantity:   100,
		PriceC:     55, // 55¢ limit, zero TimeInForce = immediate-or-cancel
	})
	if err != nil {
		var apiErr *polymarket.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusTooManyRequests {
			// Rate limited before the order reached the book. Do NOT blindly
			// re-send after an ambiguous failure — reconcile with
			// OpenOrders/Positions first (there is no client order id).
		}
		log.Fatal(err)
	}
	fmt.Printf("order %s: filled %d @ %d¢, %d resting\n",
		res.OrderID, res.FillCount, res.AvgPriceC, res.Remaining)
}

// Consume live books and trades from the market-data stream.
func ExampleClient_DialMarkets() {
	signer, err := polymarket.NewSigner(
		os.Getenv("POLYMARKET_KEY_ID"),
		os.Getenv("POLYMARKET_SECRET_KEY"),
	)
	if err != nil {
		log.Fatal(err)
	}
	c := polymarket.NewAuthedClient(nil, signer, "", "")

	ctx := context.Background()
	st, err := c.DialMarkets(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	if err := st.SubscribeBooks(ctx, "books-1", "some-market-slug"); err != nil {
		log.Fatal(err)
	}
	for {
		msg, err := st.Next(ctx)
		if err != nil {
			log.Fatal(err) // transport error: redial and re-subscribe
		}
		switch {
		case msg.Book != nil:
			fmt.Printf("%s: %d bids, %d offers\n",
				msg.Book.MarketSlug, len(msg.Book.Bids), len(msg.Book.Offers))
		case msg.Err != "":
			log.Printf("subscription %s failed: %s", msg.RequestID, msg.Err)
		}
	}
}
