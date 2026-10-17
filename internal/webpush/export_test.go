package webpush

import "net/http"

// UseTransport swaps c's transport, keeping its redirect policy.
func UseTransport(c *Client, t http.RoundTripper) { c.http.Transport = t }
