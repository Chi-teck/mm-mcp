package mattermost

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
)

// maxErrorBody caps how much of an error response body is read.
const maxErrorBody = 1 << 20

// acceptLanguage is sent with every request, by Client4 and RawDo alike, so that server
// messages come back in English.
const acceptLanguage = "en"

// maxPlainErrorLen caps a plain-text error body that is used as the message.
const maxPlainErrorLen = 200

// newClient4 returns a Client4 for baseURL authenticated with token. Every
// request it sends carries Accept-Language: en, and every error response is
// normalized into AppError JSON that carries the HTTP status.
func newClient4(baseURL, token string) *model.Client4 {
	c := model.NewAPIv4Client(baseURL)
	c.SetToken(token)
	c.HTTPClient = &http.Client{Transport: &transport{base: http.DefaultTransport}, CheckRedirect: sameOrigin}
	return c
}

// maxRedirects matches net/http's default policy.
const maxRedirects = 10

// sameOrigin is Client4's redirect policy. Client4 sets the Authorization header itself, and
// net/http forwards it on a redirect to the same host or a subdomain, even from https to http.
// Only redirects to the exact host of the first request are followed, and never a downgrade
// from https to http. A 301/302/303 turns DELETE, POST and PUT into a body-less GET, so a
// redirect that changes the method is refused rather than reported as a successful write.
func sameOrigin(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	first := via[0]
	if req.URL.Host != first.URL.Host || (first.URL.Scheme == "https" && req.URL.Scheme != "https") {
		return errors.New("redirected to another origin; refusing to follow it")
	}
	if req.Method != first.Method {
		return fmt.Errorf("redirect would turn %s into %s; refusing to follow it; set MM_MCP_URL to the final server URL",
			first.Method, req.Method)
	}
	return nil
}

// transport sets Accept-Language and normalizes error bodies for Client4.
//
// Client4 only yields a *model.AppError when the body is AppError JSON, and it
// never fills StatusCode from the response itself. Rewriting error bodies here
// lets WrapErr report the real status for proxy pages, empty bodies and
// servers that omit status_code.
type transport struct {
	base http.RoundTripper
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Accept-Language", acceptLanguage)
	resp, err := t.base.RoundTrip(req)
	// Client4 treats every status from 300 up, except 304, as an error and decodes its body
	// without a limit, so 3xx bodies are normalized as well.
	if err != nil || resp.StatusCode < http.StatusMultipleChoices || resp.StatusCode == http.StatusNotModified {
		return resp, err
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	_ = resp.Body.Close()
	if readErr != nil {
		// e.g. the request context ended mid-body; do not mask it as a server error.
		return nil, readErr
	}
	body = normalizeErrorBody(resp.StatusCode, resp.Header.Get("Content-Type"), body)
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.Header.Set("Content-Type", "application/json")
	return resp, nil
}

// normalizeErrorBody returns AppError JSON for an error response: the
// server's own AppError with status_code filled in, or a synthetic one whose
// message is a short plain-text body, or empty. A JSON object that Client4
// cannot decode into model.AppError (a field of the wrong type) counts as no
// AppError, since Client4 would otherwise fail without the status.
func normalizeErrorBody(status int, contentType string, body []byte) []byte {
	if obj, _, ok := decodeAppError(body); ok {
		if code, _ := obj["status_code"].(float64); code == 0 {
			obj["status_code"] = status
		}
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
	}
	appErr := model.AppError{StatusCode: status, Message: plainMessage(contentType, body)}
	out, _ := json.Marshal(appErr) // a plain struct of strings and ints cannot fail
	return out
}

// ServerMessage returns the message of an error response body exactly as the
// Client4 path reports it: the AppError message, else a short single-line
// text/plain body, else "" (which APIError renders as NoServerMessage).
func ServerMessage(contentType string, body []byte) string {
	if _, appErr, ok := decodeAppError(body); ok {
		return appErr.Message
	}
	return plainMessage(contentType, body)
}

// decodeAppError reports whether body is a JSON object that Client4 can decode
// into model.AppError, returning it both as a generic object and decoded.
func decodeAppError(body []byte) (map[string]any, model.AppError, bool) {
	var obj map[string]any
	var appErr model.AppError
	ok := json.Unmarshal(body, &obj) == nil && obj != nil && json.Unmarshal(body, &appErr) == nil
	return obj, appErr, ok
}

// plainMessage returns a text/plain body trimmed, if it is one line of at most
// maxPlainErrorLen runes; otherwise "".
func plainMessage(contentType string, body []byte) string {
	if mediaType, _, _ := mime.ParseMediaType(contentType); mediaType != "text/plain" {
		return ""
	}
	msg := strings.TrimSpace(string(body))
	if strings.Contains(msg, "\n") || len([]rune(msg)) > maxPlainErrorLen {
		return ""
	}
	return msg
}
