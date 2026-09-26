package tools

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/Chi-teck/mm-mcp/internal/mattermost"
	"github.com/Chi-teck/mm-mcp/internal/testutil"
)

// serveRaw answers method+pattern with status, body and optional headers (key, value pairs), and
// records the request URI the fake received.
func serveRaw(h *harness, method, pattern string, status int, body string, header ...string) *[]string {
	var uris []string
	h.fake.Handle(method, pattern, func(w http.ResponseWriter, r *http.Request) {
		uris = append(uris, r.RequestURI)
		for i := 0; i+1 < len(header); i += 2 {
			w.Header().Set(header[i], header[i+1])
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	return &uris
}

func TestAPIGet(t *testing.T) {
	h := newHarness(t)
	uris := serveRaw(h, http.MethodGet, "/users/me/status", http.StatusOK, "  {\"status\":\"online\"}\n")
	for _, path := range []string{"/users/me/status", "users/me/status", "/api/v4/users/me/status", "api/v4/users/me/status"} {
		if got := h.callOK(t, "api", map[string]any{"path": path}); got != `{"status":"online"}` {
			t.Fatalf("%s: got %q", path, got)
		}
	}
	for _, u := range *uris {
		if u != "/api/v4/users/me/status" {
			t.Fatalf("request uri %q", u)
		}
	}
	r := h.fake.Requests()[0]
	if got := r.Header.Get("Authorization"); !strings.EqualFold(got, "Bearer "+testutil.Token) {
		t.Fatalf("authorization %q", got)
	}
}

func TestAPIQueryAndPlaceholders(t *testing.T) {
	h := newHarness(t)
	uris := serveRaw(h, http.MethodGet, "/users/{user_id}/teams/{team_id}/channels", http.StatusOK, "[]")
	h.callOK(t, "api", map[string]any{"path": "/users/{user_id}/teams/{team_id}/channels?per_page=2&page=1"})
	want := "/api/v4/users/" + testutil.MeID + "/teams/" + testutil.TeamID + "/channels?per_page=2&page=1"
	if got := (*uris)[len(*uris)-1]; got != want {
		t.Fatalf("request uri\n got %q\nwant %q", got, want)
	}
}

func TestAPINoPlaceholderNoLookup(t *testing.T) {
	h := newHarness(t)
	serveRaw(h, http.MethodGet, "/system/ping", http.StatusOK, `{"status":"OK"}`)
	h.callOK(t, "api", map[string]any{"path": "/system/ping"})
	if got := calls(h, true); len(got) != 1 {
		t.Fatalf("requests %q", got)
	}
}

func TestAPIPostBody(t *testing.T) {
	h := newHarness(t)
	serveRaw(h, http.MethodPost, "/posts", http.StatusCreated, `{"id":"p1"}`)
	body := `{"channel_id":"` + testutil.TestChannelID + `","message":"hi"}`
	if got := h.callOK(t, "api", map[string]any{"path": "/posts", "method": "POST", "body": body}); got != `{"id":"p1"}` {
		t.Fatalf("got %q", got)
	}
	r := h.fake.Requests()[0]
	if r.Method != http.MethodPost || string(r.Body) != body || r.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("request %s body %q type %q", r.Method, r.Body, r.Header.Get("Content-Type"))
	}
}

func TestAPIMethods(t *testing.T) {
	h := newHarness(t)
	for _, m := range []string{"PUT", "PATCH", "DELETE"} {
		serveRaw(h, m, "/posts/{post_id}", http.StatusOK, "")
		h.callOK(t, "api", map[string]any{"path": "/posts/" + postID("p"), "method": m})
	}
	wantCalls(t, h, "PUT /api/v4/posts/"+postID("p"), "PATCH /api/v4/posts/"+postID("p"), "DELETE /api/v4/posts/"+postID("p"))

	text, isErr := h.callTool(t, "api", map[string]any{"path": "/users", "method": "HEAD"})
	wantErr(t, text, isErr, `validating "arguments": validating root: validating /properties/method: `+
		`enum: HEAD does not equal any of: [GET POST PUT PATCH DELETE]`)
	if n := len(calls(h, true)); n != 3 {
		t.Fatalf("HEAD sent a request: %d calls, want 3", n)
	}
}

func TestAPIBodyRefused(t *testing.T) {
	h := newHarness(t)
	text, isErr := h.callTool(t, "api", map[string]any{"path": "/users", "body": "{}"})
	wantErr(t, text, isErr, "a GET request cannot carry a body")
	text, isErr = h.callTool(t, "api", map[string]any{"path": "/posts", "method": "POST", "body": "message=hi"})
	wantErr(t, text, isErr, "body is not valid JSON: message=hi")
	if got := calls(h, true); len(got) != 0 {
		t.Fatalf("requests sent: %q", got)
	}
}

func TestAPIErrorStatus(t *testing.T) {
	h := newHarness(t)
	serveRaw(h, http.MethodGet, "/channels/{id}", http.StatusNotFound, `{"message":"Unable to find the channel."}`)
	text, isErr := h.callTool(t, "api", map[string]any{"path": "/api/v4/channels/nope"})
	wantErr(t, text, isErr, `mattermost API GET /channels/nope failed (404): {"message":"Unable to find the channel."}`)

	serveRaw(h, http.MethodDelete, "/posts/{id}", http.StatusForbidden, strings.Repeat("e", 600))
	text, isErr = h.callTool(t, "api", map[string]any{"path": "/posts/x", "method": "DELETE"})
	wantErr(t, text, isErr, "mattermost API DELETE /posts/x failed (403): "+strings.Repeat("e", 500)+
		" **[truncated at 500 chars — response cut]**")

	serveRaw(h, http.MethodPut, "/posts/{id}", http.StatusMethodNotAllowed, " \n")
	text, isErr = h.callTool(t, "api", map[string]any{"path": "/posts/x", "method": "PUT", "body": "{}"})
	wantErr(t, text, isErr, "mattermost API PUT /posts/x failed (405): "+mattermost.NoServerMessage)
}

func TestAPITruncation(t *testing.T) {
	h := newHarness(t)
	big := strings.Repeat("ж", 5000)
	serveRaw(h, http.MethodGet, "/users", http.StatusOK, big)
	want := strings.Repeat("ж", 4000) + "\n**[truncated at 4000 chars — pass full=true for the whole response]**"
	if got := h.callOK(t, "api", map[string]any{"path": "/users"}); got != want {
		t.Fatalf("cut output: len %d, tail %q", len(got), got[len(got)-80:])
	}
	if got := h.callOK(t, "api", map[string]any{"path": "/users", "full": true}); got != big {
		t.Fatalf("full output: len %d", len(got))
	}
	exact := strings.Repeat("x", 4000)
	serveRaw(h, http.MethodGet, "/users", http.StatusOK, exact)
	if got := h.callOK(t, "api", map[string]any{"path": "/users"}); got != exact {
		t.Fatalf("4000-char body was cut")
	}
}

func TestAPIEmptyResponse(t *testing.T) {
	h := newHarness(t)
	serveRaw(h, http.MethodGet, "/users/me/status", http.StatusOK, " \n")
	if got := h.callOK(t, "api", map[string]any{"path": "/users/me/status"}); got != "(empty response)" {
		t.Fatalf("got %q", got)
	}
}

func TestAPIRedirect(t *testing.T) {
	h := newHarness(t)
	uris := serveRaw(h, http.MethodGet, "/users/me/status", http.StatusFound, "", "Location", "https://evil.example/x")
	text, isErr := h.callTool(t, "api", map[string]any{"path": "/users/me/status"})
	wantErr(t, text, isErr, "mattermost API GET /users/me/status redirected to https://evil.example/x; refusing to follow it")
	if len(*uris) != 1 || len(h.fake.Requests()) != 1 {
		t.Fatalf("redirect followed: %q", calls(h, true))
	}

	serveRaw(h, http.MethodGet, "/users/me/status", http.StatusMovedPermanently, "")
	text, isErr = h.callTool(t, "api", map[string]any{"path": "/users/me/status"})
	wantErr(t, text, isErr, "mattermost API GET /users/me/status redirected to an undisclosed location; refusing to follow it")
}

func TestAPITokenNotInOutput(t *testing.T) {
	h := newHarness(t)
	serveRaw(h, http.MethodGet, "/users", http.StatusInternalServerError, "boom")
	text, _ := h.callTool(t, "api", map[string]any{"path": "/users"})
	text2, _ := h.callTool(t, "api", map[string]any{"path": "/../x"})
	if strings.Contains(text+text2, testutil.Token) {
		t.Fatalf("token leaked: %q / %q", text, text2)
	}
}

// TestAPIConfinement covers every escape from the API root: each is refused through the tool with no request.
func TestAPIConfinement(t *testing.T) {
	h := newHarness(t)
	base := h.fake.URL + "/api/v4"
	cases := map[string]string{
		"https://evil.example/x":   "path must be relative to /api/v4, not a full URL: https://evil.example/x",
		"HTTP://evil.example/x":    "path must be relative to /api/v4, not a full URL: HTTP://evil.example/x",
		"/../../evil":              "",
		"/users/../../../evil":     "",
		"/..":                      "",
		"..":                       "",
		"/api/v4/../evil":          "",
		"/..%2f..%2fevil":          "",
		"/..%2F..%2Fevil":          "",
		"/%2e%2e/evil":             "",
		"/.%2E/evil":               "",
		"/%2e%2e%2f%2e%2e%2fevil":  "",
		"/..\\..\\evil":            "",
		"/..%5c..%5cevil":          "",
		"/x%3f..%2f..%2f..%2fevil": "",
		"/x%23..%2f..%2f..%2fevil": "",
		"/.\t./evil":               "",
		"/.%09./evil":              "",
		"/%252e%252e/evil":         "",
		"/{team_id}/../../evil":    "",
		"/users/100%":              "path must use valid percent-encoding: /users/100%",
		"/users/%zz":               "path must use valid percent-encoding: /users/%zz",
		"/users/%C0%80":            "path must use valid percent-encoding: /users/%C0%80",
		"/%c0%ae%c0%ae/evil":       "path must use valid percent-encoding: /%c0%ae%c0%ae/evil",
		"/x;/../../evil":           "",
		"/api/v4/../api/v4/x":      "",
		"/x|y%2f..%2f..%2fevil":    "",
		"/x%0a..%2f..%2f..%2fevil": "",
	}
	for path, want := range cases {
		if want == "" {
			// Error results are one line: errorText folds the tab into a space.
			want = "path must stay under " + base + ": " + strings.ReplaceAll(path, "\t", " ")
		}
		text, isErr := h.callTool(t, "api", map[string]any{"path": path})
		wantErr(t, text, isErr, want)
	}
	if got := calls(h, true); len(got) != 0 {
		t.Fatalf("requests sent: %q", got)
	}
}

// TestAPITarget checks what is sent for paths that stay under the root.
func TestAPITarget(t *testing.T) {
	cases := map[string]string{
		"":                               "/",
		"/api/v4":                        "/",
		"api/v4?x=1":                     "/?x=1",
		"/api/v4beta/users":              "/api/v4beta/users",
		"//evil.example/x":               "//evil.example/x",
		"/users/../me":                   "/me",
		"/users/./me/.":                  "/users/me/",
		"/users/me/..":                   "/users/",
		"/users\\me":                     "/users/me",
		"/users%2f..%2fme":               "/users%2f..%2fme",
		"/files/search?terms=docs%2Fapi": "/files/search?terms=docs%2Fapi",
		"/users?terms=a%2Fb%5Cc%3Fd%23e": "/users?terms=a%2Fb%5Cc%3Fd%23e",
		"/users?terms=../../../evil":     "/users?terms=../../../evil",
		"/users/username/a b":            "/users/username/a%20b",
		"/users/username/a%20b":          "/users/username/a%20b",
		"/users?q=a b'<>":                "/users?q=a%20b%27%3C%3E",
		"/users/ж":                       "/users/%D0%B6",
		"/users/me#frag":                 "/users/me",
		"/users/me?x=1#frag":             "/users/me?x=1",
		"/users/me \n":                   "/users/me",
	}
	for path, want := range cases {
		got, err := apiTarget("https://mm.example.com/mm", path, path)
		if err != nil || got != want {
			t.Errorf("apiTarget(%q) = %q, %v; want %q", path, got, err, want)
		}
	}
	if _, err := apiTarget("https://mm.example.com/mm", "/../api/v4/x", "p"); err == nil {
		t.Errorf("climb out of the server base path accepted")
	}
}

// TestAPIConfinementAfterExpansion covers a path that climbs out only once {team_id} is expanded:
// "%2{team_id}" reads "%2f" because the team id starts with "f".
func TestAPIConfinementAfterExpansion(t *testing.T) {
	h := newHarness(t)
	path := "/..%2{team_id}/x"
	text, isErr := h.callTool(t, "api", map[string]any{"path": path})
	wantErr(t, text, isErr, "path must stay under "+h.fake.URL+"/api/v4: "+path)
	if got := calls(h, true); len(got) != 0 {
		t.Fatalf("requests sent: %q", got)
	}
}

// TestAPIReadCap checks that a body is read up to maxAPIRead bytes only, cut on a rune boundary.
func TestAPIReadCap(t *testing.T) {
	h := newHarness(t)
	big := "a" + strings.Repeat("ж", maxAPIRead/2) // one byte over the cap, "ж" split by it
	serveRaw(h, http.MethodGet, "/users", http.StatusOK, big)
	kept := "a" + strings.Repeat("ж", maxAPIRead/2-1)
	hint := " — the rest was not read; narrow the request, e.g. with page/per_page or other query params]**"
	want := kept + "\n**[response cut at 4 MiB" + hint // chunked: no Content-Length
	if got := h.callOK(t, "api", map[string]any{"path": "/users", "full": true}); got != want {
		t.Fatalf("full output: len %d, tail %q", len(got), got[len(got)-120:])
	}
	if got := h.callOK(t, "api", map[string]any{"path": "/users"}); !strings.HasSuffix(got, "pass full=true for the whole response]**") {
		t.Fatalf("cut output tail %q", got[len(got)-80:])
	}
	serveRaw(h, http.MethodGet, "/users", http.StatusOK, big, "Content-Length", strconv.Itoa(len(big)))
	want = kept + "\n**[response cut at 4 MiB of 4194305 bytes" + hint
	if got := h.callOK(t, "api", map[string]any{"path": "/users", "full": true}); got != want {
		t.Fatalf("full output with length: len %d, tail %q", len(got), got[len(got)-120:])
	}
}
