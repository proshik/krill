package client

import "time"

// SetOrdinaryTimeout shortens the timeout of ordinary calls, so a test can
// prove that the held calls (a deployment or log wait) do not run under it.
func SetOrdinaryTimeout(c *Client, d time.Duration) { c.hc.Timeout = d }
