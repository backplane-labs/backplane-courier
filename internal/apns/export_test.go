package apns

import "time"

// SetNow makes c read the time from now (token lifetime, expiration).
func SetNow(c *Client, now func() time.Time) { c.now = now }
