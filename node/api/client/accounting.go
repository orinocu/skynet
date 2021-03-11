package client

import (
	"net/url"
	"strconv"

	"gitlab.com/skynetlabs/skyd/modules"
)

// AccountingGet requests the /accounting resource
func (c *Client) AccountingGet(start, end int64) (ais []modules.AccountingInfo, err error) {
	values := url.Values{}
	values.Set("start", strconv.FormatInt(start, 10))
	values.Set("end", strconv.FormatInt(end, 10))
	err = c.get("/accounting?"+values.Encode(), &ais)
	return
}
