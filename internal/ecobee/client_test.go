package ecobee

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	c, err := NewClient("test-app", filepath.Join(t.TempDir(), "auth.cache"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.baseURL = s.URL
	c.token = Token{AccessToken: "test-access", RefreshToken: "test-refresh", Expiry: time.Now().Add(time.Hour)}
	return c
}

func TestRefreshAndLegacyCache(t *testing.T) {
	var refreshes atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			refreshes.Add(1)
			if r.Method != http.MethodPost || r.URL.RawQuery != "" {
				t.Error("tokens must be submitted in form body")
			}
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("refresh_token") != "test-refresh" || r.Form.Get("client_id") != "test-app" {
				t.Error("incorrect token form")
			}
			fmt.Fprint(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600,"token_type":"Bearer"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer new-access" {
			t.Error("incorrect bearer token")
		}
		fmt.Fprint(w, `{"status":{"code":0},"thermostatCount":1,"revisionList":["test:Room:true:a:b:c:d:future-field"]}`)
	})
	c.token.Expiry = time.Now().Add(-time.Hour)
	legacy, _ := json.Marshal(c.token)
	if err := os.WriteFile(c.cacheFile, legacy, 0777); err != nil {
		t.Fatal(err)
	}
	loaded, err := NewClient("test-app", c.cacheFile, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	loaded.baseURL = c.baseURL
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			if _, err := loaded.Summary(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if refreshes.Load() != 1 {
		t.Fatal("concurrent refreshes", refreshes.Load())
	}
	info, err := os.Stat(c.cacheFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal("unsafe token permissions", info.Mode())
	}
	var token Token
	b, _ := os.ReadFile(c.cacheFile)
	if err := json.Unmarshal(b, &token); err != nil {
		t.Fatal(err)
	}
	if token.RefreshToken != "new-refresh" {
		t.Fatal("refresh token not saved")
	}
}

func TestSummaryValidation(t *testing.T) {
	for _, body := range []string{
		`{"status":{"code":7}}`,
		`{"thermostatCount":0,"revisionList":[]}`,
		`{"status":{"code":0},"thermostatCount":2,"revisionList":[]}`,
		`{"status":{"code":0},"thermostatCount":1,"revisionList":["short"]}`,
		`{"status":{"code":0},"thermostatCount":1,"revisionList":["test:Room:maybe:a:b:c:d"]}`,
		`not json`,
	} {
		t.Run(body, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
			if _, err := c.Summary(context.Background()); err == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}
}

func TestRejectedTokenRefreshesOnce(t *testing.T) {
	for _, rejection := range []int{http.StatusUnauthorized, http.StatusOK} {
		t.Run(fmt.Sprint(rejection), func(t *testing.T) {
			requests, tokens := 0, 0
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					tokens++
					fmt.Fprint(w, `{"access_token":"replacement","expires_in":3600}`)
					return
				}
				requests++
				w.WriteHeader(rejection)
				fmt.Fprint(w, `{"status":{"code":14}}`)
			})
			if _, err := c.Summary(context.Background()); err == nil {
				t.Fatal("expected token rejection")
			}
			if requests != 2 || tokens != 1 {
				t.Fatal("unbounded auth retries", requests, tokens)
			}
		})
	}
}

func TestRateLimitAndTimeout(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := c.Summary(context.Background())
	var rateLimit *RateLimitError
	if !errors.As(err, &rateLimit) || rateLimit.After != 10*time.Minute {
		t.Fatal(err)
	}
	c = testClient(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	c.HTTP.Timeout = 10 * time.Millisecond
	if _, err := c.Summary(context.Background()); err == nil || strings.Contains(err.Error(), "test-app") {
		t.Fatal("timeout absent or secrets exposed", err)
	}
}

func TestNoImplicitAuthorizationAndCorruptCache(t *testing.T) {
	requests := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { requests++ })
	c.token = Token{}
	if _, err := c.Summary(context.Background()); err == nil {
		t.Fatal("missing credentials accepted")
	}
	if requests != 0 {
		t.Fatal("implicit authorization attempted")
	}
	if err := os.WriteFile(c.cacheFile, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewClient("test-app", c.cacheFile, time.Second); err == nil {
		t.Fatal("corrupt cache silently discarded")
	}
	b, _ := os.ReadFile(c.cacheFile)
	if string(b) != "corrupt" {
		t.Fatal("corrupt cache overwritten")
	}
}

func TestReadOnlySelectionAndPagination(t *testing.T) {
	for _, pages := range []int{1, 2} {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/1/thermostat" {
				t.Error("unexpected request")
			}
			var request struct {
				Selection map[string]any `json:"selection"`
			}
			if err := json.Unmarshal([]byte(r.URL.Query().Get("json")), &request); err != nil {
				t.Error(err)
			}
			if request.Selection["includeExtendedRuntime"] != true || request.Selection["includeSensors"] != true {
				t.Error(request)
			}
			fmt.Fprintf(w, `{"status":{"code":0},"thermostatList":[],"page":{"totalPages":%d}}`, pages)
		})
		_, err := c.Thermostats(context.Background())
		if (err != nil) != (pages == 2) {
			t.Fatal("pagination silently truncated", err)
		}
	}
}

func TestExplicitPINAuthorization(t *testing.T) {
	requests := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path == "/authorize" {
			if r.URL.Query().Get("scope") != "smartRead" {
				t.Error("authorization must be read-only")
			}
			fmt.Fprint(w, `{"ecobeePin":"TEST-PIN","code":"test-code"}`)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("grant_type") != "ecobeePin" || r.Form.Get("code") != "test-code" {
			t.Error("incorrect PIN exchange")
		}
		fmt.Fprint(w, `{"access_token":"access","refresh_token":"refresh","expires_in":3600}`)
	})
	var output bytes.Buffer
	if err := c.Authorize(context.Background(), strings.NewReader("\n"), &output); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || !strings.Contains(output.String(), "TEST-PIN") {
		t.Fatal("incorrect PIN flow")
	}
	if strings.Contains(output.String(), "test-code") || strings.Contains(output.String(), "refresh") {
		t.Fatal("tokens exposed")
	}
	if err := c.Authorize(context.Background(), strings.NewReader(""), &output); err == nil {
		t.Fatal("EOF accepted as user confirmation")
	}
}

func TestCacheWriteFailureIsRetriedWithoutRefreshingAgain(t *testing.T) {
	refreshes := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			refreshes++
			fmt.Fprint(w, `{"access_token":"new","refresh_token":"rotated","expires_in":3600}`)
			return
		}
		fmt.Fprint(w, `{"status":{"code":0},"thermostatCount":0,"revisionList":[]}`)
	})
	c.token.Expiry = time.Time{}
	goodCache := c.cacheFile
	c.cacheFile = filepath.Join(t.TempDir(), "missing-dir", "auth.cache")
	if _, err := c.Summary(context.Background()); err == nil {
		t.Fatal("cache write failure ignored")
	}
	if _, err := c.Summary(context.Background()); err == nil {
		t.Fatal("unpersisted token reported healthy")
	}
	c.cacheFile = goodCache
	if _, err := c.Summary(context.Background()); err != nil {
		t.Fatal(err)
	}
	if refreshes != 1 {
		t.Fatal("token unnecessarily refreshed again")
	}
}

func TestAuthorizationRedirectDoesNotForwardCredentials(t *testing.T) {
	forwarded := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded = true }))
	defer target.Close()
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) })
	if err := c.Authorize(context.Background(), strings.NewReader("\n"), &bytes.Buffer{}); err == nil {
		t.Fatal("redirect followed")
	}
	if forwarded {
		t.Fatal("authorization redirected to another origin")
	}
}
