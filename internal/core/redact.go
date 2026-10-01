package core

import (
	"errors"
	"fmt"
	"net/url"
)

// RedactURLError strips the full request URL net/http puts into every
// *url.Error ("Get \"https://cdn/…?token=…\": …"), keeping the operation and
// host: upstream URLs carry CDN tokens and plugin API keys, and these errors
// end up in data/logs and in responses. The underlying error stays wrapped,
// so errors.Is(err, context.DeadlineExceeded) etc. still work. Other errors
// pass through unchanged.
func RedactURLError(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	host := "?"
	if u, perr := url.Parse(ue.URL); perr == nil && u.Host != "" {
		host = u.Host
	}
	return fmt.Errorf("%s %s: %w", ue.Op, host, ue.Err)
}
