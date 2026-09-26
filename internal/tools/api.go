package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Chi-teck/mm-mcp/internal/mattermost"
)

const (
	apiRoot        = "/api/v4"
	maxAPIOutput   = 4000
	maxAPIError    = 500
	maxBodyEcho    = 120
	maxAPIRead     = 4 << 20 // bytes of a response body read at most; the rest is never read
	apiReadCutNote = "\n**[response cut at 4 MiB%s — the rest was not read; narrow the request, e.g. with page/per_page or other query params]**"
	apiFullHint    = "pass full=true for the whole response"
	emptyResponse  = "(empty response)"
	placeholderID  = "00000000000000000000000000" // stands in for ids while a path is checked
	teamIDHolder   = "{team_id}"
	userIDHolder   = "{user_id}"
	noLocationText = "an undisclosed location"
)

type apiIn struct {
	Path   string `json:"path" jsonschema:"Endpoint under /api/v4, e.g. /users/me/status — may carry a query string and the {team_id} and {user_id} placeholders"`
	Method string `json:"method,omitempty" jsonschema:"HTTP method (default GET)"`
	Body   string `json:"body,omitempty" jsonschema:"Request body as a JSON string"`
	Full   bool   `json:"full,omitempty" jsonschema:"Print the whole response instead of cutting it at 4000 chars (still capped at 4 MiB)"`
}

// registerAPI adds the tools of this file.
func registerAPI(s *mcp.Server, c *mattermost.Context) {
	addTool(s, "api",
		"Fallback for Mattermost REST endpoints that have no dedicated tool: send a raw request under "+
			"/api/v4 and return the response body. Prefer a dedicated tool whenever one covers the task — "+
			"those resolve channel names and format the output for reading.",
		func(ctx context.Context, in apiIn) (string, error) { return callAPI(ctx, c, in) },
		enum("method", http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete),
		defaultTo("method", http.MethodGet))
}

// callAPI sends one raw request under /api/v4. Every argument check runs before any
// request; the path confinement runs again after the placeholder lookups.
func callAPI(ctx context.Context, c *mattermost.Context, in apiIn) (string, error) {
	method := in.Method
	if in.Body != "" {
		if method == http.MethodGet {
			return "", errors.New("a GET request cannot carry a body")
		}
		if !json.Valid([]byte(in.Body)) {
			return "", fmt.Errorf("body is not valid JSON: %s", mattermost.Truncate(in.Body, "shorten it", maxBodyEcho))
		}
	}
	// Check with a stand-in id first, so a path is refused as written. Expansion can still change
	// the verdict ("%2{team_id}" reads "%2f" for an id starting with "f"), so the check on the
	// expanded path below is the one that confines the request.
	if _, err := apiTarget(c.URL(), expand(in.Path, placeholderID, placeholderID), in.Path); err != nil {
		return "", err
	}
	target, err := apiTarget(c.URL(), expand(in.Path, c.Team().Id, c.Me().Id), in.Path)
	if err != nil {
		return "", err
	}

	var body io.Reader
	contentType := ""
	if in.Body != "" {
		body = strings.NewReader(in.Body)
		contentType = "application/json"
	}
	resp, err := c.RawDo(ctx, method, apiRoot+target, body, contentType)
	if err != nil {
		return "", fmt.Errorf("mattermost API %s %s failed: %w", method, target, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		location := resp.Header.Get("Location")
		if location == "" {
			location = noLocationText
		}
		return "", fmt.Errorf("mattermost API %s %s redirected to %s; refusing to follow it",
			method, target, location)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIRead+1))
	if err != nil {
		return "", fmt.Errorf("mattermost API %s %s failed: %w", method, target, err)
	}
	cut := len(raw) > maxAPIRead
	if cut {
		raw = dropPartialRune(raw[:maxAPIRead])
	}
	text := strings.TrimSpace(string(raw))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if text == "" {
			text = mattermost.NoServerMessage
		}
		return "", fmt.Errorf("mattermost API %s %s failed (%d): %s",
			method, target, resp.StatusCode, mattermost.Truncate(text, "response cut", maxAPIError))
	}
	switch {
	case text == "":
		return emptyResponse, nil
	case in.Full && cut:
		size := ""
		if resp.ContentLength > maxAPIRead {
			size = fmt.Sprintf(" of %d bytes", resp.ContentLength)
		}
		return text + fmt.Sprintf(apiReadCutNote, size), nil
	case in.Full:
		return text, nil
	default:
		return mattermost.Truncate(text, apiFullHint, maxAPIOutput), nil
	}
}

// dropPartialRune drops a UTF-8 sequence cut off at the end of b.
func dropPartialRune(b []byte) []byte {
	for i := 0; i < utf8.UTFMax-1 && len(b) > 0; i++ {
		if r, size := utf8.DecodeLastRune(b); r != utf8.RuneError || size != 1 {
			break
		}
		b = b[:len(b)-1]
	}
	return b
}

func expand(path, teamID, userID string) string {
	if teamID != "" {
		path = strings.ReplaceAll(path, teamIDHolder, teamID)
	}
	if userID != "" {
		path = strings.ReplaceAll(path, userIDHolder, userID)
	}
	return path
}

var (
	schemeRe    = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)
	apiPrefixRe = regexp.MustCompile(`^/?api/v4([/?#]|$)`)
)

// apiTarget confines path to the API root and returns it normalized, relative to
// /api/v4 and with its query, e.g. "/users/me?x=1". The path is concatenated onto the root, never
// resolved against it, so "//host" stays a path segment. Normalization follows the WHATWG URL
// parser for http(s) — what a browser or proxy would make of the URL: tab and newline dropped,
// "\" read as "/", "." / ".." (also as %2e) resolved, the fragment dropped. The check is repeated
// on the percent-decoded path, so "..%2f" climbs the server would see are refused too. shown is
// the argument quoted in errors.
func apiTarget(serverURL, path, shown string) (string, error) {
	base := serverURL + apiRoot
	outside := fmt.Errorf("path must stay under %s: %s", base, shown)
	if schemeRe.MatchString(path) {
		return "", fmt.Errorf("path must be relative to /api/v4, not a full URL: %s", shown)
	}
	if m := apiPrefixRe.FindStringSubmatchIndex(path); m != nil {
		path = path[m[2]:] // keep the separator after "api/v4"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	pathPart, query := splitURL(apiRoot + path)
	pathPart = normalizePath(pathPart)
	if !strings.HasPrefix(pathPart, apiRoot+"/") {
		return "", outside
	}

	decoded, err := url.PathUnescape(pathPart)
	if err != nil || !utf8.ValidString(decoded) {
		return "", fmt.Errorf("path must use valid percent-encoding: %s", shown)
	}
	// As a URL parser would read it again: tab and newline dropped, "?" and "#" kept as path.
	decoded = strings.NewReplacer("?", "%3F", "#", "%23", "\t", "", "\n", "", "\r", "").Replace(decoded)
	if !strings.HasPrefix(normalizePath(decoded), apiRoot+"/") {
		return "", outside
	}

	target := strings.TrimPrefix(pathPart, apiRoot)
	if query != "" {
		target += "?" + query
	}
	full, err := url.Parse(serverURL + apiRoot + target)
	root, rootErr := url.Parse(serverURL)
	if err != nil || rootErr != nil || full.Scheme != root.Scheme || full.Host != root.Host {
		return "", outside
	}
	return target, nil
}

// splitURL drops tab/CR/LF and trailing C0 controls and spaces, cuts the fragment off, and splits
// path from query, percent-encoding what a URL parser would.
func splitURL(s string) (path, query string) {
	s = strings.TrimRight(s, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f"+
		"\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f ")
	s = strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(s)
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s = s[:i]
	}
	path, query, _ = strings.Cut(s, "?")
	return escapeBytes(path, "\"<>`{}"), escapeBytes(query, "\"<>'")
}

// normalizePath reads "\" as "/" and resolves dot segments the way the WHATWG URL parser does.
// p starts with "/".
func normalizePath(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	segments := strings.Split(p[1:], "/")
	out := make([]string, 0, len(segments))
	for i, seg := range segments {
		last := i == len(segments)-1
		switch lower := strings.ToLower(seg); lower {
		case "..", ".%2e", "%2e.", "%2e%2e":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
			if last {
				out = append(out, "")
			}
		case ".", "%2e":
			if last {
				out = append(out, "")
			}
		default:
			out = append(out, seg)
		}
	}
	return "/" + strings.Join(out, "/")
}

// escapeBytes percent-encodes controls, space, non-ASCII bytes and the bytes in extra.
func escapeBytes(s, extra string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch <= ' ' || ch >= 0x7f || strings.IndexByte(extra, ch) >= 0 {
			_, _ = fmt.Fprintf(&b, "%%%02X", ch)
			continue
		}
		b.WriteByte(ch)
	}
	return b.String()
}
