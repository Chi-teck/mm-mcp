package mattermost

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Chi-teck/mm-mcp/internal/testutil"
)

func TestMaxFileSize(t *testing.T) {
	s, c := newFakeContext(t)
	now := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return now }

	var calls atomic.Int32
	value := "104857600"
	fail := false
	s.HandleClientConfig(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if fail {
			testutil.WriteError(w, http.StatusInternalServerError, "x", "boom")
			return
		}
		testutil.WriteJSON(w, http.StatusOK, map[string]string{"MaxFileSize": value})
	})
	ctx := context.Background()

	check := func(wantSize int64, wantOK bool, wantCalls int32) {
		t.Helper()
		size, ok := c.MaxFileSize(ctx)
		if size != wantSize || ok != wantOK || calls.Load() != wantCalls {
			t.Fatalf("got (%d, %v) after %d calls, want (%d, %v) after %d", size, ok, calls.Load(), wantSize, wantOK, wantCalls)
		}
	}

	check(104857600, true, 1)
	value = "1"
	now = now.Add(CacheTTL - time.Second)
	check(104857600, true, 1) // cached
	now = now.Add(time.Second)
	check(1, true, 2) // expired

	fail = true
	now = now.Add(CacheTTL)
	check(0, false, 3)
	check(0, false, 4) // failure not cached
	fail = false
	check(1, true, 5)
}

func TestMaxFileSizeUnknown(t *testing.T) {
	for _, v := range []string{"", "abc", "0", "-5"} {
		t.Run(v, func(t *testing.T) {
			s, c := newFakeContext(t)
			s.HandleClientConfig(func(w http.ResponseWriter, _ *http.Request) {
				body := map[string]string{}
				if v != "" {
					body["MaxFileSize"] = v
				}
				testutil.WriteJSON(w, http.StatusOK, body)
			})
			if size, ok := c.MaxFileSize(context.Background()); ok || size != 0 {
				t.Fatalf("got (%d, %v), want unknown", size, ok)
			}
		})
	}
}

func TestMaxFileSizeNoRoute(t *testing.T) {
	_, c := newFakeContext(t)
	if _, ok := c.MaxFileSize(context.Background()); ok {
		t.Fatal("want unknown when the config route 404s")
	}
}

func TestMaxFileSizeConcurrent(t *testing.T) {
	s, c := newFakeContext(t)
	s.HandleClientConfig(func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteJSON(w, http.StatusOK, map[string]string{"MaxFileSize": "42"})
	})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if size, ok := c.MaxFileSize(context.Background()); !ok || size != 42 {
				t.Errorf("got (%d, %v)", size, ok)
			}
		})
	}
	wg.Wait()
}
