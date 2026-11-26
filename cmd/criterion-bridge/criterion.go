package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// sessionFor is how long a sign-in is reused. Criterion's session cookie
// lasts 30 days and isn't renewed, so the bridge signs in again before then.
const sessionFor = 25 * 24 * time.Hour

// retryAfter is how long the bridge waits after a failed sign-in before
// trying again, so a broken sign-in can't hammer Criterion's login and get
// the account locked. A rejected email and password isn't retried at all.
const retryAfter = 30 * time.Minute

// errBadCredentials means Criterion's login showed the password step again.
var errBadCredentials = errors.New("sign-in rejected: check CRITERION_EMAIL and CRITERION_PASSWORD, then restart the bridge")

var errUnauthorized = errors.New("criterion answered 401")

var stateField = regexp.MustCompile(`name="state"[^>]*?value="([^"]*)"`)

// client signs in to the Criterion Channel with the subscriber's own account
// and asks it for the live stream, the same way criterionchannel.com does in
// a browser. Every request goes through Criterion's own login and
// subscription check, so it stops working if the subscription ends.
type client struct {
	baseURL  string
	email    string
	password string
	logger   *slog.Logger

	mu       sync.Mutex
	http     *http.Client // carries the signed-in session's cookies; nil until signed in
	signedIn time.Time
	failed   time.Time
	failErr  error
}

// streamURL returns the live stream's HLS URL, signing in first if needed.
// The lock is held throughout so concurrent tunes share one sign-in.
func (c *client) streamURL(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.http == nil || time.Since(c.signedIn) >= sessionFor {
		if err := c.signIn(ctx); err != nil {
			return "", err
		}
	}
	u, err := c.playback(ctx)
	// A session that ends early gets one fresh sign-in. Right after signing
	// in, a 401 means the account itself can't play the stream.
	if errors.Is(err, errUnauthorized) && time.Since(c.signedIn) >= time.Hour {
		if err := c.signIn(ctx); err != nil {
			return "", err
		}
		u, err = c.playback(ctx)
	}
	if errors.Is(err, errUnauthorized) {
		return "", errors.New("signed in, but Criterion won't play the stream for this account: check that the subscription is active")
	}
	return u, err
}

// signIn signs in unless a recent failure says to wait. It finishes even if
// the tune that started it is cancelled, so a hung-up request doesn't count
// as a failed sign-in.
func (c *client) signIn(ctx context.Context) error {
	if c.failErr != nil {
		if errors.Is(c.failErr, errBadCredentials) {
			return c.failErr
		}
		if wait := retryAfter - time.Since(c.failed); wait > 0 {
			return fmt.Errorf("not signing in again for %s after a failure: %w", wait.Round(time.Second), c.failErr)
		}
	}
	hc, err := c.attemptSignIn(context.WithoutCancel(ctx))
	if err != nil {
		c.failed, c.failErr = time.Now(), err
		c.logger.Error("sign-in failed", "err", err)
		return err
	}
	c.http, c.signedIn, c.failErr = hc, time.Now(), nil
	c.logger.Info("signed in to the Criterion Channel")
	return nil
}

// attemptSignIn walks Criterion's Auth0 login: the email step, the password
// step, and the empty form Criterion's login shows after the password, which
// a browser submits on its own without showing it. It returns a client
// carrying the resulting session cookie.
func (c *client) attemptSignIn(ctx context.Context) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Jar: jar, Timeout: 30 * time.Second}

	// Criterion's /login redirects to its Auth0 tenant's email step.
	p, err := do(ctx, hc, http.MethodGet, c.baseURL+"/login", "", nil)
	if err != nil {
		return nil, fmt.Errorf("start sign-in: %w", err)
	}
	p, err = p.submit(ctx, hc, url.Values{
		"username":                    {c.email},
		"js-available":                {"true"},
		"webauthn-available":          {"false"},
		"is-brave":                    {"false"},
		"webauthn-platform-available": {"false"},
		"action":                      {"default"},
	})
	if err != nil {
		return nil, fmt.Errorf("email step: %w", err)
	}
	if !strings.HasSuffix(p.url.Path, "/u/login/password") {
		return nil, fmt.Errorf("expected the password step after the email, got %s", p.url.Path)
	}
	p, err = p.submit(ctx, hc, url.Values{
		"username": {c.email},
		"password": {c.password},
		"action":   {"default"},
	})
	if err != nil {
		return nil, fmt.Errorf("password step: %w", err)
	}
	if id, ok := strings.CutPrefix(p.url.Path, "/u/custom-prompt/"); ok {
		if p, err = completeForm(ctx, hc, p, id); err != nil {
			return nil, err
		}
	}

	switch {
	case strings.HasSuffix(p.url.Path, "/u/login/password"):
		return nil, errBadCredentials
	case strings.Contains(p.url.Path, "/mfa"):
		return nil, errors.New("criterion asked for two-factor authentication, which the bridge can't complete")
	}
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, err
	}
	for _, ck := range jar.Cookies(base) {
		if ck.Name == "__session" {
			return hc, nil
		}
	}
	return nil, fmt.Errorf("sign-in ended at %s without a session", p.url.Path)
}

// completeForm submits the post-login form a browser submits on its own. It
// only does so for a form with nothing to fill in that asks to be submitted
// straight away; anything else is left for a person.
func completeForm(ctx context.Context, hc *http.Client, prompt *page, id string) (*page, error) {
	api := prompt.url.Scheme + "://" + prompt.url.Host + "/forms/api/forms/" + url.PathEscape(id)
	state := prompt.url.Query().Get("state")

	var def struct {
		Steps        []json.RawMessage `json:"steps"`
		HiddenFields []json.RawMessage `json:"hiddenFields"`
	}
	if err := doJSON(ctx, hc, http.MethodGet, api+"?"+url.Values{"state": {state}}.Encode(), nil, &def); err != nil {
		return nil, fmt.Errorf("post-login form: %w", err)
	}
	if len(def.Steps) > 0 || len(def.HiddenFields) > 0 {
		return nil, fmt.Errorf("criterion's post-login form %s now asks for input, which the bridge doesn't fill in: sign in at criterionchannel.com to see it", id)
	}

	// The same envelope the form's script sends, describing the bridge as itself.
	submission := map[string]any{
		"formData": map[string]any{},
		"metaData": map[string]any{"navigator": map[string]string{"userAgent": userAgent(), "language": "en-US"}},
	}
	var start struct {
		Effect struct {
			Type string `json:"type"`
		} `json:"effect"`
	}
	if err := doJSON(ctx, hc, http.MethodPost, api+"/validations/$start", submission, &start); err != nil {
		return nil, fmt.Errorf("post-login form: %w", err)
	}
	if start.Effect.Type != "SUBMIT_FORM" {
		return nil, fmt.Errorf("criterion's post-login form %s didn't ask to be submitted (got %q): sign in at criterionchannel.com to see it", id, start.Effect.Type)
	}
	if err := doJSON(ctx, hc, http.MethodPost, api+"/submissions/", submission, nil); err != nil {
		return nil, fmt.Errorf("post-login form: %w", err)
	}
	p, err := prompt.submit(ctx, hc, url.Values{})
	if err != nil {
		return nil, fmt.Errorf("post-login form: %w", err)
	}
	return p, nil
}

// playback asks Criterion for the live stream's sources, with the session.
func (c *client) playback(ctx context.Context) (string, error) {
	var body struct {
		Playlist []struct {
			Sources []struct {
				File string `json:"file"`
				Type string `json:"type"`
			} `json:"sources"`
		} `json:"playlist"`
	}
	if err := doJSON(ctx, c.http, http.MethodGet, c.baseURL+"/api/playback/"+mediaID, nil, &body); err != nil {
		return "", err
	}
	for _, pl := range body.Playlist {
		for _, s := range pl.Sources {
			if s.Type == "application/vnd.apple.mpegurl" && strings.HasPrefix(s.File, "https://") {
				return s.File, nil
			}
		}
	}
	return "", errors.New("criterion's playback response had no HLS stream")
}

type page struct {
	url  *url.URL
	body string
}

// submit posts the page's form back to the page's own URL, carrying its
// state field along with fields.
func (p *page) submit(ctx context.Context, hc *http.Client, fields url.Values) (*page, error) {
	state := p.url.Query().Get("state")
	if m := stateField.FindStringSubmatch(p.body); m != nil {
		state = html.UnescapeString(m[1])
	}
	if state == "" {
		return nil, fmt.Errorf("no login state on %s", p.url.Path)
	}
	fields.Set("state", state)
	return do(ctx, hc, http.MethodPost, p.url.String(), "application/x-www-form-urlencoded", strings.NewReader(fields.Encode()))
}

// do makes a request, follows redirects, and returns where it ended up.
// Errors name only the path, never a query string or form field.
func do(ctx context.Context, hc *http.Client, method, u, contentType string, body io.Reader) (*page, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent())
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := hc.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err // drops the URL, which can carry login state
		}
		return nil, fmt.Errorf("%s %s: %w", method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("%s %s: %s", method, resp.Request.URL.Path, resp.Status)
	}
	return &page{url: resp.Request.URL, body: string(b)}, nil
}

// doJSON sends in as JSON (when not nil) and decodes a 2xx response into out
// (when not nil). A 401 or 403 is errUnauthorized.
func doJSON(ctx context.Context, hc *http.Client, method, u string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent())
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return fmt.Errorf("%s %s: %w", method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return errUnauthorized
	case resp.StatusCode/100 != 2:
		return fmt.Errorf("%s %s: %s", method, req.URL.Path, resp.Status)
	case out == nil:
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", req.URL.Path, err)
	}
	return nil
}

func userAgent() string {
	return "criterion-bridge/" + version + " (+https://github.com/zackwag/criterion-bridge)"
}
