package main

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"maps"
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
	jwLookups   map[string]int // JW Player media lookups by ID
	tmdbQueries map[string]int // TMDB searches by "title|year"
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
	// The forms API, as Auth0's forms script calls it: loading the form takes
	// the login state and returns a signed checkpoint in a header, and each
	// later call has to send the latest checkpoint as a bearer token.
	location := func(r *http.Request) bool {
		return r.Header.Get("Auth0-Forms-Location") == "http://"+r.Host+"/u/custom-prompt/ap_test"
	}
	mux.HandleFunc("GET /forms/api/forms/ap_test", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "s3" || r.Header.Get("Auth0-Forms-State") != "s3" || !location(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Auth0-Forms-Journey-Checkpoint", "sig_load")
		steps := "[]"
		if f.formFields {
			steps = `[{"id":"step_1"}]`
		}
		_, _ = io.WriteString(w, `{"id":"ap_test","steps":`+steps+`,"hiddenFields":[]}`)
	})
	mux.HandleFunc("POST /forms/api/forms/ap_test/validations/{step}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sig_load" || !location(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"effect":{"type":"SUBMIT_FORM","checkpoint":"ajc_test"},"checkpoint":"ajc_test"}`)
	})
	mux.HandleFunc("POST /forms/api/forms/ap_test/submissions/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ajc_test" || !location(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
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
	// JW Player's media API: two films have a release date, one fails, and
	// the rest aren't found.
	mux.HandleFunc("GET /v2/media/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		f.mu.Lock()
		if f.jwLookups == nil {
			f.jwLookups = map[string]int{}
		}
		f.jwLookups[id]++
		f.mu.Unlock()
		switch id {
		case "805MwiYh":
			_, _ = io.WriteString(w, `{"playlist":[{"mediaid":"805MwiYh","release_date":"1972-12-30"}]}`)
		case "obscure1":
			_, _ = io.WriteString(w, `{"playlist":[{"mediaid":"obscure1","release_date":"1960-05-01"}]}`)
		case "gone0001":
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})
	// TMDB's movie search, with key "testkey" or read access token "eyJtest":
	// the searches below have results, the rest have none.
	mux.HandleFunc("GET /3/search/movie", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("api_key") != "testkey" && r.Header.Get("Authorization") != "Bearer eyJtest" {
			http.Error(w, `{"status_message":"Invalid API key"}`, http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		if f.tmdbQueries == nil {
			f.tmdbQueries = map[string]int{}
		}
		f.tmdbQueries[q.Get("query")+"|"+q.Get("year")]++
		f.mu.Unlock()
		results, ok := map[string]string{
			"A Short & Sharp Film|1972": `{"id":1,"poster_path":"/sharp_Poster-1.jpg"},{"id":2,"poster_path":"/other.jpg"}`,
			"Dated a Year Late|1985":    `{"id":3,"poster_path":"/late.jpg"}`,
			"Heaven's Gate|":            `{"id":4,"poster_path":"/gate.jpg"}`,
			"Remade|":                   `{"id":5,"poster_path":"/original.jpg"},{"id":6,"poster_path":"/remake.jpg"}`,
			"No Poster|1990":            `{"id":7,"poster_path":null}`,
			"Unlisted|":                 `{"id":8,"poster_path":"/unlisted.jpg"}`,
		}[q.Get("query")+"|"+q.Get("year")]
		if !ok {
			results = ""
		}
		_, _ = io.WriteString(w, `{"results":[`+results+`]}`)
	})
	mux.HandleFunc("GET /api/media/1emmgvqX", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"title":"Criterion 24/7","schedule":[
 {"startTime":"2026-11-25T11:00:00Z","endTime":"2026-11-25T11:06:31Z","title":"A Short & Sharp Film","shortDescription":"Film","longDescription":"One \"quoted\" line.","programType":"Movie","mediaid":"805MwiYh","genres":["Movies"],"ratings":[{"body":"US","code":"NR"}]},
 {"startTime":"2026-11-25T11:06:31Z","endTime":"2026-11-25T12:42:25Z","title":"Tótem","shortDescription":"Film","programType":"Movie","mediaid":"../evil","genres":["Movies","Drama"],"ratings":[]},
 {"startTime":"2026-11-25T12:42:25Z","endTime":"2026-11-25T12:42:25Z","title":"Zero length","programType":"Movie"},
 {"startTime":"2026-11-25T12:42:25Z","endTime":"2026-11-25T14:00:00Z","title":"No Metadata","programType":"Movie","mediaid":"gone0001"},
 {"startTime":"2026-11-25T14:00:00Z","endTime":"2026-11-25T15:30:00Z","title":"A Short & Sharp Film","programType":"Movie","mediaid":"805MwiYh"},
 {"startTime":"2026-11-25T15:30:00Z","endTime":"2026-11-25T17:00:00Z","title":"Obscure Film","programType":"Movie","mediaid":"obscure1"},
 {"startTime":"2026-11-25T17:00:00Z","endTime":"2026-11-25T18:30:00Z","title":"Unlisted","programType":"Movie","mediaid":"unlisted"}
]}`)
	})
	return mux
}

func newBridge(t *testing.T, f *fakeCriterion, password string) (*fakeCriterion, *client, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(f.handler())
	t.Cleanup(upstream.Close)
	c := &client{baseURL: upstream.URL, email: "me@example.com", password: password, logger: slog.New(slog.DiscardHandler)}
	g := &guide{http: upstream.Client(), baseURL: upstream.URL, jwURL: upstream.URL, tmdbURL: upstream.URL}
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
	if tv.Channel.ID != channelID || len(tv.Programmes) != 6 {
		t.Fatalf("guide has channel %q and %d programmes, want %q and 6 (the zero-length one dropped):\n%s", tv.Channel.ID, len(tv.Programmes), channelID, b)
	}
	first := tv.Programmes[0]
	if first.Start != "20261125110000 +0000" || first.Stop != "20261125110631 +0000" || first.Channel != channelID {
		t.Errorf("first programme times/channel = %+v", first)
	}
	if first.Title != "A Short & Sharp Film" || first.Desc != `One "quoted" line.` {
		t.Errorf("first programme title/desc = %q / %q", first.Title, first.Desc)
	}
	if first.Date != "19721230" {
		t.Errorf("first programme date = %q, want the release date 19721230", first.Date)
	}
	if strings.Join(first.Categories, ",") != "Movie" || len(first.Ratings) != 1 || first.Ratings[0].Value != "NR" {
		t.Errorf("first programme categories/ratings = %v / %v", first.Categories, first.Ratings)
	}
	if first.Icon == nil || first.Icon.Src != "https://cdn.jwplayer.com/v2/media/805MwiYh/poster.jpg?width=1280" {
		t.Errorf("first programme icon = %+v, want the film's JW Player poster", first.Icon)
	}
	if !strings.Contains(string(b), "</category>\n  <icon ") || !strings.Contains(string(b), "</icon>\n  <rating ") {
		t.Errorf("icon should come between category and rating, as XMLTV orders them:\n%s", b)
	}
	if tv.Programmes[1].Icon != nil {
		t.Errorf("an entry with a malformed media ID got an icon: %+v", tv.Programmes[1].Icon)
	}
	if second := tv.Programmes[1]; second.Desc != "Film" || strings.Join(second.Categories, ",") != "Movie,Drama" || second.Date != "" {
		t.Errorf("second programme desc/categories/date = %q / %v / %q, want the short description, Movie,Drama and no year for a malformed media ID", second.Desc, second.Categories, second.Date)
	}
}

func TestReleaseDatesAreCachedAndFailuresRetried(t *testing.T) {
	f := &fakeCriterion{password: "right"}
	upstream := httptest.NewServer(f.handler())
	t.Cleanup(upstream.Close)
	g := &guide{http: upstream.Client(), baseURL: upstream.URL, jwURL: upstream.URL, tmdbURL: upstream.URL}

	for range 2 {
		g.fetched = time.Time{} // force a refetch
		b, err := g.get(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "<date>19721230</date>") {
			t.Fatalf("guide is missing the release date:\n%s", b)
		}
		if !strings.Contains(string(b), "<title>No Metadata</title>") {
			t.Fatalf("a failed metadata lookup dropped its programme:\n%s", b)
		}
	}
	if n := f.jwLookups["805MwiYh"]; n != 1 {
		t.Errorf("805MwiYh was looked up %d times across two refreshes, want 1 (cached)", n)
	}
	if n := f.jwLookups["gone0001"]; n != 2 {
		t.Errorf("gone0001 was looked up %d times across two refreshes, want 2 (a failure isn't cached)", n)
	}
	if n := f.jwLookups["unlisted"]; n != 1 {
		t.Errorf("unlisted was looked up %d times across two refreshes, want 1 (a 404 is cached)", n)
	}
	if n := f.jwLookups["../evil"]; n != 0 {
		t.Errorf("a malformed media ID was looked up %d times, want 0", n)
	}
	if len(f.tmdbQueries) != 0 {
		t.Errorf("searched TMDB without a key: %v", f.tmdbQueries)
	}
}

// guideIcons refreshes the guide and returns each programme's icon by title.
func guideIcons(t *testing.T, g *guide) map[string]string {
	t.Helper()
	g.fetched = time.Time{} // force a refetch
	b, err := g.get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var tv struct {
		Programmes []xmltvProgramme `xml:"programme"`
	}
	if err := xml.Unmarshal(b, &tv); err != nil {
		t.Fatal(err)
	}
	icons := map[string]string{}
	for _, p := range tv.Programmes {
		if p.Icon != nil {
			icons[p.Title] = p.Icon.Src
		}
	}
	return icons
}

func TestTMDBPosters(t *testing.T) {
	for _, key := range []string{"testkey", "eyJtest"} {
		t.Run(key, func(t *testing.T) {
			f := &fakeCriterion{password: "right"}
			upstream := httptest.NewServer(f.handler())
			t.Cleanup(upstream.Close)
			g := &guide{http: upstream.Client(), baseURL: upstream.URL, jwURL: upstream.URL, tmdbURL: upstream.URL, tmdbKey: key}

			for range 2 {
				icons := guideIcons(t, g)
				if want := tmdbPosterURL + "/sharp_Poster-1.jpg"; icons["A Short & Sharp Film"] != want {
					t.Errorf("icon = %q, want TMDB's top result %q", icons["A Short & Sharp Film"], want)
				}
				if want := "https://cdn.jwplayer.com/v2/media/obscure1/poster.jpg?width=1280"; icons["Obscure Film"] != want {
					t.Errorf("a film TMDB doesn't have got icon %q, want the JW still", icons["Obscure Film"])
				}
				if want := "https://cdn.jwplayer.com/v2/media/gone0001/poster.jpg?width=1280"; icons["No Metadata"] != want {
					t.Errorf("a film whose metadata lookup failed got icon %q, want the JW still", icons["No Metadata"])
				}
				if want := tmdbPosterURL + "/unlisted.jpg"; icons["Unlisted"] != want {
					t.Errorf("a film JW Player has no metadata for got icon %q, want TMDB's sole title match %q", icons["Unlisted"], want)
				}
			}
			want := map[string]int{
				"A Short & Sharp Film|1972": 1,
				// no results is an answer too, so these aren't repeated
				"Obscure Film|1960": 1, "Obscure Film|1959": 1, "Obscure Film|1961": 1, "Obscure Film|": 1,
				// JW Player has no metadata for it, so only the title is searched
				"Unlisted|": 1,
			}
			if !maps.Equal(f.tmdbQueries, want) {
				t.Errorf("TMDB searches across two refreshes = %v, want %v", f.tmdbQueries, want)
			}
		})
	}
}

func TestTMDBPosterFallbacks(t *testing.T) {
	f := &fakeCriterion{password: "right"}
	upstream := httptest.NewServer(f.handler())
	t.Cleanup(upstream.Close)
	g := &guide{http: upstream.Client(), tmdbURL: upstream.URL, tmdbKey: "testkey"}

	for _, tc := range []struct {
		title, date, want string
		searches          []string
	}{
		{"A Short & Sharp Film", "19720101", "/sharp_Poster-1.jpg", []string{"1972"}},
		{"Dated a Year Late", "19860101", "/late.jpg", []string{"1986", "1985"}},
		{"Heaven\u2019s Gate", "20200101", "/gate.jpg", []string{"2020", "2019", "2021", ""}},
		{"Heaven\u2019s Gate", "", "/gate.jpg", []string{""}},
		{"Remade", "19500101", "", []string{"1950", "1949", "1951", ""}},
		{"No Poster", "19900101", "", []string{"1990"}},
		{"", "19900101", "", nil},
	} {
		t.Run(tc.title+"|"+tc.date, func(t *testing.T) {
			f.tmdbQueries = nil
			got, err := g.tmdbPoster(t.Context(), tc.title, tc.date)
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			if tc.want != "" {
				want = tmdbPosterURL + tc.want
			}
			if got != want {
				t.Errorf("poster = %q, want %q", got, want)
			}
			title := strings.ReplaceAll(tc.title, "\u2019", "'")
			wantQueries := map[string]int{}
			for _, y := range tc.searches {
				wantQueries[title+"|"+y] = 1
			}
			if !maps.Equal(f.tmdbQueries, wantQueries) {
				t.Errorf("searches = %v, want %v", f.tmdbQueries, wantQueries)
			}
		})
	}
}

func TestRejectedTMDBKeyKeepsTheStillsAndRetries(t *testing.T) {
	f := &fakeCriterion{password: "right"}
	upstream := httptest.NewServer(f.handler())
	t.Cleanup(upstream.Close)
	g := &guide{http: upstream.Client(), baseURL: upstream.URL, jwURL: upstream.URL, tmdbURL: upstream.URL, tmdbKey: "wrong"}

	if icons := guideIcons(t, g); icons["A Short & Sharp Film"] != "https://cdn.jwplayer.com/v2/media/805MwiYh/poster.jpg?width=1280" {
		t.Errorf("icon with a rejected key = %q, want the JW still", icons["A Short & Sharp Film"])
	}
	g.tmdbKey = "testkey" // fixed, as after a restart
	if icons := guideIcons(t, g); icons["A Short & Sharp Film"] != tmdbPosterURL+"/sharp_Poster-1.jpg" {
		t.Errorf("icon once the key works = %q, want the TMDB poster", icons["A Short & Sharp Film"])
	}
	if n := f.jwLookups["805MwiYh"]; n != 1 {
		t.Errorf("JW Player was asked %d times, want 1: only the TMDB part is retried", n)
	}
}

func TestXMLTVDate(t *testing.T) {
	for in, want := range map[string]string{
		"1985-11-27":           "19851127",
		"1985-11-27T00:00:00Z": "19851127",
		"1953":                 "1953",
		"1953-13-40":           "1953", // not a real date, so just the year
		"0099-01-01":           "",
		"":                     "",
		"soon":                 "",
	} {
		if got := xmltvDate(in); got != want {
			t.Errorf("xmltvDate(%q) = %q, want %q", in, got, want)
		}
	}
}
