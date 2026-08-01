package polymarket

import "fmt"

// APIError is a non-2xx response from the exchange. Match with errors.As to
// branch on StatusCode:
//
//	var apiErr *polymarket.APIError
//	if errors.As(err, &apiErr) && apiErr.StatusCode == 429 { ... }
//
// Order rejections are NOT APIErrors — the exchange reports them inside a
// 2xx execution stream, and CreateOrder surfaces them as plain errors
// carrying the reject reason.
type APIError struct {
	StatusCode int    // HTTP status
	What       string // request label, e.g. "create order btc-100k"
	Body       string // response body (truncated) — the exchange's reason
}

func (e *APIError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("%s: status %d", e.What, e.StatusCode)
	}
	return fmt.Sprintf("%s: status %d: %s", e.What, e.StatusCode, e.Body)
}
