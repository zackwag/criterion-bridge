package main

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const fakeStream = "https://stream.example/live/index.m3u8?user_id=test"

// fakeCriterion plays both criterionchannel.com and its Auth0 tenant, with
// the same steps as the real sign-in.
type fakeCriterion struct {
	password   string // the account's password
	formFields bool   // the post-login form asks for input
	noForm     bool   // no post-login form at all

	mu          sync.Mutex
	passwordHit int
	signIns     int
	submissions []map[string]any
	session     string // the currently valid __session
}

func (f *fakeCriterion) handler() http.Handler {
	page := func(w http.ResponseWriter, state string) {
		_, _ = io.WriteString(w, `<form method="POST"><input type="hidden" name="state" value="`+state+`"/></form>`)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/u/login/identifier?state=s1", http.StatusFound)
	})
	mux.HandleFunc("GET /u/login/identifier", func(w http.ResponseWriter, _ *http.Request) { page(w, "s1") })
	mux.HandleFunc("POST /u/login/identifier", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("state") != "s1" || r.FormValue("username") != "me@example.com" {
			http.Error(w, "bad email step", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, "/u/login/password?state=s2", http.StatusFound)
	})
	mux.HandleFunc("GET /u/login/password", func(w http.ResponseWriter, _ *http.Request) { page(w, "s2") })
	mux.HandleFunc("POST /u/login/password", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.passwordHit++
		f.mu.Unlock()
		if r.FormValue("state") != "s2" || r.FormValue("password") != f.password {
			w.WriteHeader(http.StatusBadRequest) // Auth0 shows the password step again
			page(w, "s2")
			return
		}
		next := "/u/custom-prompt/ap_test?state=s3"
		if f.noForm {
			next = "/auth/callback?code=c"
		}
		http.Redirect(w, r, next, http.StatusFound)
	})
	mux.HandleFunc("GET /u/custom-prompt/ap_test", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<input type="hidden" id="custom-form-config" value="{}"/>`)
	})
	mux.HandleFunc("GET /forms/api/forms/ap_test", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "s3" {
			http.Error(w, "no state", http.StatusBadRequest)
			return
		}
		steps := "[]"
		if f.formFields {
			steps = `[{"id":"step_1"}]`
		}
		_, _ = io.WriteString(w, `{"id":"ap_test","steps":`+steps+`,"hiddenFields":[]}`)
	})
	mux.HandleFunc("POST /forms/api/forms/ap_test/validations/{step}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"effect":{"type":"SUBMIT_FORM","checkpoint":"ajc_test"}}`)
	})
	mux.HandleFunc("POST /forms/api/forms/ap_test/submissions/", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.submissions = append(f.submissions, body)
		f.mu.Unlock()
	})
	mux.HandleFunc("POST /u/custom-prompt/ap_test", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("state") != "s3" {
			http.Error(w, "bad state", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, "/auth/callback?code=c", http.StatusFound)
	})
	mux.HandleFunc("GET /auth/callback", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.signIns++
		f.session = "session-" + string(rune('0'+f.signIns))
		http.SetCookie(w, &http.Cookie{Name: "__session", Value: f.session, Path: "/", HttpOnly: true})
		f.mu.Unlock()
		http.Redirect(w, r, "/", http.StatusFound)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "home") })
	mux.HandleFunc("GET /api/playback/1emmgvqX", func(w http.ResponseWriter, r *http.Request) {
		ck, err := r.Cookie("__session")
		f.mu.Lock()
		ok := err == nil && ck.Value == f.session && f.session != ""
		f.mu.Unlock()
		if !ok {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"playlist":[{"mediaid":"1emmgvqX","sources":[{"file":"https://stream.example/live/index.mpd","type":"application/dash+xml"},{"file":"`+fakeStream+`","type":"application/vnd.apple.mpegurl"}]}]}`)
	})
	mux.HandleFunc("GET /api/media/1emmgvqX", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"title":"Criterion 24/7","schedule":[
 {"startTime":"2026-11-25T11:00:00Z","endTime":"2026-11-25T11:06:31Z","title":"A Short & Sharp Film","shortDescription":"Film","longDescription":"One \"quoted\" line.","programType":"Movie","genres":["Movies"],"ratings":[{"body":"US","code":"NR"}]},
 {"startTime":"2026-11-25T11:06:31Z","endTime":"2026-11-25T12:42:25Z","title":"Tótem","shortDescription":"Film","programType":"Movie","genres":["Movies","Drama"],"ratings":[]},
 {"startTime":"2026-11-25T12:42:25Z","endTime":"2026-11-25T12:42:25Z","title":"Zero length","programType":"Movie"}
]}`)
	})
	return mux
}

func newBridge(t *testing.T, f *fakeCriterion, password string) (*fakeCriterion, *client, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(f.handler())
	t.Cleanup(upstream.Close)
	c := &client{baseURL: upstream.URL, email: "me@example.com", password: password, logger: slog.New(slog.DiscardHandler)}
	g := &guide{http: upstream.Client(), baseURL: upstream.URL}
	srv := httptest.NewServer(handler(c, g, slog.New(slog.DiscardHandler)))
	t.Cleanup(srv.Close)
	return f, c, srv
}

var noFollow = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func play(t *testing.T, srv *httptest.Server) (int, string) {
	t.Helper()
	resp, err := noFollow.Get(srv.URL + "/play")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Location")
}

func TestPlaySignsInThroughTheSelfSubmittingForm(t *testing.T) {
	f, _, srv := newBridge(t, &fakeCriterion{password: "right"}, "right")

	for range 2 {
		if code, loc := play(t, srv); code != http.StatusFound || loc != fakeStream {
			t.Fatalf("/play = %d to %q, want a 302 to the HLS stream", code, loc)
		}
	}
	if f.signIns != 1 {
		t.Errorf("signed in %d times, want 1 (the session is reused)", f.signIns)
	}
	if len(f.submissions) != 1 {
		t.Fatalf("post-login form was submitted %d times, want 1", len(f.submissions))
	}
	sub := f.submissions[0]
	if fd, ok := sub["formData"].(map[string]any); !ok || len(fd) != 0 {
		t.Errorf("formData = %v, want empty", sub["formData"])
	}
	ua, _ := sub["metaData"].(map[string]any)["navigator"].(map[string]any)["userAgent"].(string)
	if !strings.HasPrefix(ua, "criterion-bridge/") {
		t.Errorf("form metadata userAgent = %q, want the bridge's own", ua)
	}
}

func TestPlaySignsInWithoutAPostLoginForm(t *testing.T) {
	f, _, srv := newBridge(t, &fakeCriterion{password: "right", noForm: true}, "right")
	if code, loc := play(t, srv); code != http.StatusFound || loc != fakeStream {
		t.Fatalf("/play = %d to %q, want a 302 to the HLS stream", code, loc)
	}
	if len(f.submissions) != 0 {
		t.Errorf("submitted a form that wasn't there")
	}
}

func TestWrongPasswordIsNotRetried(t *testing.T) {
	f, c, srv := newBridge(t, &fakeCriterion{password: "right"}, "wrong")

	for range 3 {
		if code, _ := play(t, srv); code != http.StatusServiceUnavailable {
			t.Fatalf("/play = %d, want 503", code)
		}
	}
	if f.passwordHit != 1 {
		t.Errorf("password step was tried %d times, want 1", f.passwordHit)
	}
	if !errors.Is(c.failErr, errBadCredentials) {
		t.Errorf("failure = %v, want errBadCredentials", c.failErr)
	}
}

func TestFailedSignInBacksOff(t *testing.T) {
	f, c, srv := newBridge(t, &fakeCriterion{password: "right", formFields: true}, "right")

	if code, _ := play(t, srv); code != http.StatusServiceUnavailable {
		t.Fatalf("/play = %d, want 503 when the post-login form asks for input", code)
	}
	if !strings.Contains(c.failErr.Error(), "asks for input") {
		t.Errorf("failure = %v, want it to say the form asks for input", c.failErr)
	}
	if len(f.submissions) != 0 {
		t.Errorf("submitted a form that asks for input")
	}
	if code, _ := play(t, srv); code != http.StatusServiceUnavailable || f.passwordHit != 1 {
		t.Errorf("second tune: %d with %d password attempts, want 503 without trying again", code, f.passwordHit)
	}

	// After retryAfter, it tries again.
	f.formFields = false
	c.failed = time.Now().Add(-retryAfter)
	if code, loc := play(t, srv); code != http.StatusFound || loc != fakeStream {
		t.Errorf("after the wait, /play = %d to %q, want a 302 to the stream", code, loc)
	}
}

func TestExpiredSessionSignsInAgainOnce(t *testing.T) {
	f, c, srv := newBridge(t, &fakeCriterion{password: "right"}, "right")
	if code, _ := play(t, srv); code != http.StatusFound {
		t.Fatalf("first /play = %d", code)
	}

	// Criterion ends the session early.
	f.session = "ended"
	c.signedIn = time.Now().Add(-2 * time.Hour)
	if code, loc := play(t, srv); code != http.StatusFound || loc != fakeStream {
		t.Errorf("/play after the session ended = %d to %q, want a fresh sign-in and a 302", code, loc)
	}
	if f.signIns != 2 {
		t.Errorf("signed in %d times, want 2", f.signIns)
	}

	// Right after signing in, a 401 means the account can't play it; no loop.
	f.session = "ended"
	if code, _ := play(t, srv); code != http.StatusServiceUnavailable || f.signIns != 2 {
		t.Errorf("401 right after sign-in: %d after %d sign-ins, want 503 without signing in again", code, f.signIns)
	}
}

func TestPlaylistAndGuide(t *testing.T) {
	_, _, srv := newBridge(t, &fakeCriterion{password: "right"}, "right")

	resp, err := http.Get(srv.URL + "/playlist.m3u")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	m3u := string(b)
	if !strings.HasPrefix(m3u, "#EXTM3U\n") || !strings.Contains(m3u, `tvg-id="criterion-24-7"`) || !strings.HasSuffix(m3u, "\n"+srv.URL+"/play\n") {
		t.Errorf("playlist:\n%s", m3u)
	}

	resp, err = http.Get(srv.URL + "/guide.xml")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var tv struct {
		Channel struct {
			ID string `xml:"id,attr"`
		} `xml:"channel"`
		Programmes []xmltvProgramme `xml:"programme"`
	}
	if err := xml.Unmarshal(b, &tv); err != nil {
		t.Fatalf("guide isn't valid XML: %v\n%s", err, b)
	}
	if tv.Channel.ID != channelID || len(tv.Programmes) != 2 {
		t.Fatalf("guide has channel %q and %d programmes, want %q and 2 (the zero-length one dropped):\n%s", tv.Channel.ID, len(tv.Programmes), channelID, b)
	}
	first := tv.Programmes[0]
	if first.Start != "20261125110000 +0000" || first.Stop != "20261125110631 +0000" || first.Channel != channelID {
		t.Errorf("first programme times/channel = %+v", first)
	}
	if first.Title != "A Short & Sharp Film" || first.Desc != `One "quoted" line.` {
		t.Errorf("first programme title/desc = %q / %q", first.Title, first.Desc)
	}
	if strings.Join(first.Categories, ",") != "Movie,Movies" || len(first.Ratings) != 1 || first.Ratings[0].Value != "NR" {
		t.Errorf("first programme categories/ratings = %v / %v", first.Categories, first.Ratings)
	}
	if second := tv.Programmes[1]; second.Desc != "Film" || strings.Join(second.Categories, ",") != "Movie,Movies,Drama" {
		t.Errorf("second programme desc/categories = %q / %v, want the short description and deduped categories", second.Desc, second.Categories)
	}
}
