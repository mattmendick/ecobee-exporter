package ecobee

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Token deliberately matches the oauth2.Token JSON used by previous releases.
type Token struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
}

type Client struct {
	HTTP                       *http.Client
	baseURL, appKey, cacheFile string
	mu                         sync.Mutex // Serialize token refreshes and requests; never race cache writers.
	token                      Token
	tokenDirty                 bool
}

func NewClient(appKey, cacheFile string, timeout time.Duration) (*Client, error) {
	c := &Client{HTTP: &http.Client{Timeout: timeout}, baseURL: "https://api.ecobee.com", appKey: appKey, cacheFile: cacheFile}
	// Never forward credentials to a redirect target.
	c.HTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	b, err := os.ReadFile(cacheFile)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read token cache: %w", err)
	}
	if err := json.Unmarshal(b, &c.token); err != nil {
		return nil, errors.New("invalid token cache; restore a backup or move it aside before running --auth")
	}
	return c, nil
}

func (c *Client) saveToken() error {
	b, err := json.Marshal(c.token)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(c.cacheFile), ".ecobee-token-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), c.cacheFile); err != nil {
		return err
	}
	c.tokenDirty = false
	return nil
}

func (c *Client) exchange(ctx context.Context, values url.Values) error {
	values.Set("client_id", c.appKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/token", strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var response struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := c.do(req, &response); err != nil {
		return err
	}
	if response.AccessToken == "" || response.ExpiresIn <= 0 {
		return errors.New("invalid token response")
	}
	refresh := response.RefreshToken
	if refresh == "" {
		refresh = c.token.RefreshToken
	}
	if refresh == "" {
		return errors.New("token response has no refresh token")
	}
	c.token = Token{response.AccessToken, response.TokenType, refresh, time.Now().Add(time.Duration(response.ExpiresIn) * time.Second)}
	c.tokenDirty = true
	if err := c.saveToken(); err != nil {
		return fmt.Errorf("persist token cache: %w", err)
	}
	return nil
}

// Authorize is deliberately separate from polling; no credentials or PINs are logged.
func (c *Client) Authorize(ctx context.Context, input io.Reader, output io.Writer) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	values := url.Values{"response_type": {"ecobeePin"}, "client_id": {c.appKey}, "scope": {"smartRead"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/authorize?"+values.Encode(), nil)
	if err != nil {
		return err
	}
	var pin struct {
		PIN  string `json:"ecobeePin"`
		Code string `json:"code"`
	}
	if err := c.do(req, &pin); err != nil {
		return err
	}
	if pin.PIN == "" || pin.Code == "" {
		return errors.New("invalid PIN response")
	}
	fmt.Fprintf(output, "Authorize PIN %s at https://www.ecobee.com/consumerportal under My Apps, then press Enter.\n", pin.PIN)
	// Waiting for user input is only permitted in this explicit interactive command.
	_, err = bufio.NewReader(input).ReadString('\n')
	if err != nil {
		return errors.New("authorization requires interactive input")
	}
	return c.exchange(ctx, url.Values{"grant_type": {"ecobeePin"}, "code": {pin.Code}})
}

type apiStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *Client) get(ctx context.Context, endpoint string, selection map[string]any, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Retry a failed cache write before claiming a successful poll. Do not
	// unnecessarily exchange the new token again after a transient disk error.
	if c.tokenDirty {
		if err := c.saveToken(); err != nil {
			return fmt.Errorf("persist token cache: %w", err)
		}
	}
	b, err := json.Marshal(map[string]any{"selection": selection})
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 2; attempt++ {
		if c.token.AccessToken == "" || time.Until(c.token.Expiry) < time.Minute {
			if c.token.RefreshToken == "" {
				return errors.New("no refresh token; run --auth first")
			}
			if err := c.exchange(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {c.token.RefreshToken}}); err != nil {
				return err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/1/"+endpoint+"?"+url.Values{"json": {string(b)}}.Encode(), nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.token.AccessToken)
		var raw json.RawMessage
		err = c.do(req, &raw)
		if errors.Is(err, errUnauthorized) && attempt == 0 {
			c.token.Expiry = time.Time{}
			continue
		}
		if err != nil {
			return err
		}
		var envelope struct {
			Status *apiStatus `json:"status"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return errors.New("invalid API response")
		}
		if envelope.Status == nil {
			return errors.New("API response missing status")
		}
		if envelope.Status.Code == 14 && attempt == 0 {
			c.token.Expiry = time.Time{}
			continue
		}
		if envelope.Status.Code != 0 {
			return fmt.Errorf("ecobee API status %d", envelope.Status.Code)
		}
		return json.Unmarshal(raw, out)
	}
	return errors.New("ecobee access token rejected after refresh")
}

var errUnauthorized = errors.New("ecobee HTTP 401")

// RateLimitError tells the poller not to retry before Retry-After has elapsed.
type RateLimitError struct{ After time.Duration }

func (e *RateLimitError) Error() string { return "ecobee HTTP 429 (rate limited)" }

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.HTTP.Do(req)
	// Do not propagate URL errors: authorization URLs contain application keys.
	if err != nil {
		return errors.New("ecobee HTTP request failed (network error or timeout)")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errUnauthorized
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		delay := 3 * time.Minute
		if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && n > 0 {
			delay = time.Duration(n) * time.Second
		} else if date, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil && time.Until(date) > delay {
			delay = time.Until(date)
		}
		return &RateLimitError{After: delay}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ecobee HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out); err != nil {
		return errors.New("invalid ecobee JSON response")
	}
	return nil
}

func (c *Client) Summary(ctx context.Context) (map[string]string, error) {
	var response struct {
		RevisionList    []string `json:"revisionList"`
		ThermostatCount *int     `json:"thermostatCount"`
	}
	if err := c.get(ctx, "thermostatSummary", map[string]any{"selectionType": "registered", "selectionMatch": ""}, &response); err != nil {
		return nil, err
	}
	if response.ThermostatCount == nil || *response.ThermostatCount != len(response.RevisionList) {
		return nil, errors.New("invalid thermostat summary count")
	}
	revisions := make(map[string]string, len(response.RevisionList))
	for _, revision := range response.RevisionList {
		fields := strings.Split(revision, ":")
		if len(fields) < 7 || fields[0] == "" {
			return nil, errors.New("invalid thermostat revision")
		}
		if _, err := strconv.ParseBool(fields[2]); err != nil {
			return nil, errors.New("invalid thermostat connection state")
		}
		if _, exists := revisions[fields[0]]; exists {
			return nil, errors.New("duplicate thermostat revision")
		}
		revisions[fields[0]] = revision
	}
	return revisions, nil
}

func (c *Client) Thermostats(ctx context.Context) ([]Thermostat, error) {
	var response struct {
		ThermostatList []Thermostat `json:"thermostatList"`
		Page           struct {
			TotalPages int `json:"totalPages"`
		} `json:"page"`
	}
	err := c.get(ctx, "thermostat", map[string]any{"selectionType": "registered", "selectionMatch": "", "includeSensors": true, "includeRuntime": true, "includeSettings": true, "includeExtendedRuntime": true}, &response)
	if err != nil {
		return nil, err
	}
	if response.ThermostatList == nil {
		return nil, errors.New("missing thermostat list")
	}
	if response.Page.TotalPages > 1 {
		return nil, errors.New("paginated thermostat responses are not yet supported")
	}
	return response.ThermostatList, nil
}
