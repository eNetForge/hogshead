package admin_test

import "github.com/enetforge/hogshead/internal/adminclient"

// clientWithoutCreds talks to a trusted handler over plain HTTP.
func clientWithoutCreds(url string) *adminclient.Client { return adminclient.NewPlain(url) }
