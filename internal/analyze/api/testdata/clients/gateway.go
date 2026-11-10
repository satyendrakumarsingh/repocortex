package gateway

// A Go service calling another Go service, which is the same hop as a browser
// calling a backend and has to be found the same way.

import (
	"bytes"
	"fmt"
	"net/http"
)

type Client struct {
	httpClient *http.Client
	base       string
}

func (c *Client) Settle(body []byte) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, "https://ledger.internal/settlements", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	return c.httpClient.Do(req)
}

func (c *Client) Health() (*http.Response, error) {
	return http.Get("https://ledger.internal/health")
}

func (c *Client) Transaction(id string) (*http.Response, error) {
	// Built with Sprintf: the target is not knowable from the text.
	return c.httpClient.Get(fmt.Sprintf("%s/transactions/%s", c.base, id))
}

func (c *Client) Refund(id string, body []byte) (*http.Response, error) {
	// A concatenation carrying a literal prefix.
	return c.httpClient.Post("/payment/refund/"+id, "application/json", bytes.NewReader(body))
}

func (c *Client) Verb(method, url string) (*http.Request, error) {
	// The verb is a variable. Reported as ANY rather than assumed.
	return http.NewRequestWithContext(ctx, method, "/payment/callback", nil)
}

func (c *Client) NotHTTP(key string) string {
	// A cache lookup on a well-named receiver. Not an outbound call.
	return c.cache.Get("session-key")
}
