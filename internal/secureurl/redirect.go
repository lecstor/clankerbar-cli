package secureurl

import (
	"fmt"
	"net/http"
)

// NoDowngradeRedirect is an http.Client CheckRedirect hook that refuses a
// redirect which would put the bearer token on the wire in cleartext. Go
// already strips Authorization when a redirect changes host, but it FORWARDS it
// on an https -> http hop to the SAME host — exactly the cleartext exposure the
// credential-origin rule exists to prevent (CLA-257), arriving by a route
// config validation cannot see.
func NoDowngradeRedirect(req *http.Request, _ []*http.Request) error {
	if _, err := Origin(req.URL.String()); err != nil {
		return fmt.Errorf("refusing redirect: %w", err)
	}
	return nil
}
