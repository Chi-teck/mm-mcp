package mattermost

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
)

// rawClient serves RawDo. It is separate from Client4's client so that error
// bodies reach the caller untouched, and it never follows redirects.
var rawClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// RawDo sends an authenticated request to path on the server and returns the
// response as is; the caller must close its body. path is relative to the
// server URL and must start with "/" (e.g. "/api/v4/users/me?x=1"). body may
// be nil; contentType is set only when non-empty. The request carries
// `Authorization: Bearer <token>` and `Accept-Language: en` and is bound to
// ctx. Redirects are not followed: a 3xx comes back as the response, with its
// Location header, for the caller to report.
func (c *Context) RawDo(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, errors.New("raw request path must start with /: " + path)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.URL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("Accept-Language", acceptLanguage)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return rawClient.Do(req)
}
