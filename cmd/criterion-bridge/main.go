// Command criterion-bridge serves the Criterion Channel's 24/7 live stream to
// Channels DVR (or VLC): an M3U playlist at /playlist.m3u and an XMLTV guide
// at /guide.xml, built from Criterion's own schedule.
//
// It signs in with your own Criterion Channel subscription. Each tune of
// /play asks Criterion for the stream with that session and redirects to it,
// so it stops working if the subscription ends.
//
// It's configured with environment variables:
//
//	CRITERION_EMAIL     your Criterion Channel email (required)
//	CRITERION_PASSWORD  your Criterion Channel password (required)
//	TMDB_API_KEY        a TMDB API key or read access token, for portrait
//	                    posters (optional)
//	LISTEN_ADDR         where to serve (default :8080)
//
// "criterion-bridge healthcheck" checks a running instance's /healthz, for
// Docker's HEALTHCHECK in an image with no shell or curl.
package main

import (
	"cmp"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// mediaIDPattern matches a JW Player media ID, which goes into an image URL.
var mediaIDPattern = regexp.MustCompile(`^[A-Za-z0-9]{1,32}$`)

// posterPathPattern matches a TMDB poster path, which goes into an image URL.
var posterPathPattern = regexp.MustCompile(`^/[A-Za-z0-9_-]{1,64}\.(jpg|png)$`)

// yearPattern matches a release date that starts with a plausible year.
var yearPattern = regexp.MustCompile(`^(18|19|20)[0-9]{2}`)

// xmltvDate turns a JW Player release date like 1985-11-27 into XMLTV's
// 19851127, which Channels DVR stores the way it stores Gracenote's release
// dates. A date with no valid month and day gives just the year, and one
// without a plausible year gives nothing.
func xmltvDate(release string) string {
	if !yearPattern.MatchString(release) {
		return ""
	}
	if len(release) >= 10 {
		if d, err := time.Parse(time.DateOnly, release[:10]); err == nil {
			return d.Format("20060102")
		}
	}
	return release[:4]
}

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	criterionURL = "https://www.criterionchannel.com"
	// mediaID is Criterion 24/7's ID, from its page's URL.
	mediaID     = "1emmgvqX"
	channelID   = "criterion-24-7"
	channelName = "Criterion 24/7"
	// channelLogo is criterionchannel.com's own square icon, the Criterion
	// "C". The stream's JW Player poster is only a generic film-strip image.
	channelLogo = criterionURL + "/apple-icon.png"
	jwPlayerURL = "https://cdn.jwplayer.com"
	tmdbURL     = "https://api.themoviedb.org"
	// tmdbPosterURL is the base for TMDB posters 780 pixels wide, the size
	// Channels DVR uses for its own TMDB posters.
	tmdbPosterURL = "https://image.tmdb.org/t/p/w780"
	// guideFor is how long a fetched schedule is reused.
	guideFor = time.Hour
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(os.Getenv("LISTEN_ADDR")))
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	email, password := os.Getenv("CRITERION_EMAIL"), os.Getenv("CRITERION_PASSWORD")
	if email == "" || password == "" {
		return errors.New("CRITERION_EMAIL and CRITERION_PASSWORD must both be set to your Criterion Channel sign-in")
	}
	listenAddr := or(os.Getenv("LISTEN_ADDR"), ":8080")

	c := &client{baseURL: criterionURL, email: email, password: password, logger: logger}
	g := &guide{
		http:    &http.Client{Timeout: 30 * time.Second},
		logger:  logger,
		baseURL: criterionURL,
		jwURL:   jwPlayerURL,
		tmdbURL: tmdbURL,
		tmdbKey: strings.TrimSpace(os.Getenv("TMDB_API_KEY")),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           handler(c, g, logger),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		// Sign-in waits for the first tune, so starting up contacts nothing.
		logger.Info("serving", "addr", listenAddr, "version", version)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down: %w", err)
	}
	return nil
}

func handler(c *client, g *guide, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /playlist.m3u", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/x-mpegurl")
		_, _ = fmt.Fprintf(w, "#EXTM3U\n#EXTINF:-1 channel-id=%q tvg-id=%q tvg-name=%q tvg-logo=%q,%s\nhttp://%s/play\n",
			channelID, channelID, channelName, channelLogo, channelName, r.Host)
	})
	mux.HandleFunc("GET /guide.xml", func(w http.ResponseWriter, r *http.Request) {
		b, err := g.get(r.Context())
		if err != nil {
			logger.Error("guide", "err", err)
			if b == nil {
				http.Error(w, "couldn't fetch Criterion's schedule", http.StatusBadGateway)
				return
			}
		}
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("GET /play", func(w http.ResponseWriter, r *http.Request) {
		u, err := c.streamURL(r.Context())
		if err != nil {
			logger.Error("play", "err", err)
			http.Error(w, "couldn't get the Criterion 24/7 stream; see the bridge's log", http.StatusServiceUnavailable)
			return
		}
		http.Redirect(w, r, u, http.StatusFound)
	})
	return mux
}

type guide struct {
	http    *http.Client
	logger  *slog.Logger
	baseURL string
	jwURL   string // JW Player's media API, which has each film's release date
	tmdbURL string // TMDB's API, for portrait posters
	tmdbKey string // a TMDB API key or read access token; "" skips TMDB

	mu      sync.Mutex
	fetched time.Time
	xml     []byte
	films   map[string]film // by media ID
}

// film is what's looked up about a scheduled film beyond Criterion's
// schedule.
type film struct {
	date   string // XMLTV release date; "" when the film has none
	poster string // TMDB portrait poster URL; "" when there's none
	// searched is true once TMDB has answered for the film, whether or not it
	// had a poster, so it isn't asked again.
	searched bool
}

// get returns the XMLTV guide, refetching Criterion's schedule every
// guideFor. If a refetch fails, it returns the last guide along with the
// error.
func (g *guide) get(ctx context.Context) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Since(g.fetched) < guideFor {
		return g.xml, nil
	}
	b, err := g.fetch(ctx)
	if err != nil {
		return g.xml, err
	}
	g.xml, g.fetched = b, time.Now()
	return b, nil
}

type scheduleEntry struct {
	StartTime        time.Time `json:"startTime"`
	EndTime          time.Time `json:"endTime"`
	Title            string    `json:"title"`
	ShortDescription string    `json:"shortDescription"`
	LongDescription  string    `json:"longDescription"`
	ProgramType      string    `json:"programType"`
	MediaID          string    `json:"mediaid"` // the film's own JW Player media ID
	Genres           []string  `json:"genres"`
	Ratings          []struct {
		Body string `json:"body"`
		Code string `json:"code"`
	} `json:"ratings"`
}

// fetch builds the guide from the schedule criterionchannel.com's channel
// page shows, which needs no sign-in.
func (g *guide) fetch(ctx context.Context) ([]byte, error) {
	var body struct {
		Schedule []scheduleEntry `json:"schedule"`
	}
	if _, err := doJSON(ctx, g.http, http.MethodGet, g.baseURL+"/api/media/"+mediaID+"?fresh=1", nil, nil, &body); err != nil {
		return nil, fmt.Errorf("fetch schedule: %w", err)
	}
	if len(body.Schedule) == 0 {
		return nil, errors.New("criterion's schedule was empty")
	}
	for i := range body.Schedule {
		e := &body.Schedule[i]
		e.Title, e.ShortDescription, e.LongDescription = fixMojibake(e.Title), fixMojibake(e.ShortDescription), fixMojibake(e.LongDescription)
	}
	return xmltv(body.Schedule, g.lookUp(ctx, body.Schedule))
}

// macRoman is the upper half of Mac Roman, bytes 0x80 to 0xFF.
const macRoman = "ÄÅÇÉÑÖÜáàâäãåçéèêëíìîïñóòôöõúùûü†°¢£§•¶ß®©™´¨≠ÆØ∞±≤≥¥µ∂∑∏π∫ªºΩæø¿¡¬√ƒ≈∆«»…\u00a0ÀÃÕŒœ–—“”‘’÷◊ÿŸ⁄€‹›ﬁﬂ‡·‚„‰ÂÊÁËÈÍÎÏÌÓÔ\uf8ffÒÚÛÙıˆ˜¯˘˙˚¸˝˛ˇ"

// macRomanByte maps each character in macRoman back to its byte.
var macRomanByte = func() map[rune]byte {
	m := map[rune]byte{}
	b := byte(0x80)
	for _, r := range macRoman {
		m[r] = b
		b++
	}
	return m
}()

var nonASCII = regexp.MustCompile(`[^\x00-\x7F]+`)

// fixMojibake repairs text that was UTF-8 but got read as Mac Roman, like
// Criterion's "Heaven‚Äôs Crossroad". It maps each run of non-ASCII
// characters back to Mac Roman bytes and keeps the result only if those bytes
// are valid UTF-8, which accented letters and real typographic punctuation
// never are.
func fixMojibake(s string) string {
	return nonASCII.ReplaceAllStringFunc(s, func(run string) string {
		b := make([]byte, 0, len(run))
		for _, r := range run {
			c, ok := macRomanByte[r]
			if !ok {
				return run
			}
			b = append(b, c)
		}
		if !utf8.Valid(b) {
			return run
		}
		return string(b)
	})
}

// lookUp finds each scheduled film's release date in JW Player's public
// metadata and, with a TMDB key, its portrait poster on TMDB, a few films at a
// time. Neither changes, so each film is looked up once and kept while it
// stays on the schedule. A failed lookup leaves that film without the missing
// details and is retried next refresh.
func (g *guide) lookUp(ctx context.Context, schedule []scheduleEntry) map[string]film {
	films := map[string]film{}
	titles := map[string]string{}
	var todo []string
	for _, e := range schedule {
		if _, seen := titles[e.MediaID]; seen || !mediaIDPattern.MatchString(e.MediaID) {
			continue
		}
		titles[e.MediaID] = e.Title
		f, known := g.films[e.MediaID]
		if known {
			films[e.MediaID] = f
		}
		if !known || (g.tmdbKey != "" && !f.searched) {
			todo = append(todo, e.MediaID)
		}
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	var tmdbFailed int
	var tmdbErr error
	slots := make(chan struct{}, 8)
	for _, id := range todo {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			mu.Lock()
			f, known := films[id]
			mu.Unlock()
			if !known {
				var body struct {
					Playlist []struct {
						ReleaseDate string `json:"release_date"`
					} `json:"playlist"`
				}
				// A 404 means JW Player has no metadata for the film, so it's
				// kept with no date; any other error isn't cached, so it's
				// tried again.
				if _, err := doJSON(ctx, g.http, http.MethodGet, g.jwURL+"/v2/media/"+id, nil, nil, &body); err != nil && !errors.Is(err, errNotFound) {
					return
				}
				if len(body.Playlist) > 0 {
					f.date = xmltvDate(body.Playlist[0].ReleaseDate)
				}
			}
			if g.tmdbKey != "" && !f.searched {
				poster, err := g.tmdbPoster(ctx, titles[id], f.date)
				if err != nil {
					mu.Lock()
					tmdbFailed++
					tmdbErr = cmp.Or(tmdbErr, err)
					mu.Unlock()
				} else {
					f.poster, f.searched = poster, true
				}
			}
			mu.Lock()
			films[id] = f
			mu.Unlock()
		})
	}
	wg.Wait()
	if tmdbFailed > 0 && g.logger != nil {
		g.logger.Warn("TMDB poster lookups failed; those films keep Criterion's still until the next refresh", "count", tmdbFailed, "err", tmdbErr)
	}
	g.films = films
	return films
}

// tmdbPoster searches TMDB for a film's portrait poster and returns "" if
// there's none. It tries the release year, then a year either side, since
// Criterion and TMDB sometimes date a film by different releases. Last it
// tries the title alone, but only trusts a sole match, since a title alone
// too often matches a remake.
func (g *guide) tmdbPoster(ctx context.Context, title, date string) (string, error) {
	title = curlyQuotes.Replace(title)
	if title == "" {
		return "", nil
	}
	var years []string
	if len(date) >= 4 {
		if y, err := strconv.Atoi(date[:4]); err == nil {
			years = []string{strconv.Itoa(y), strconv.Itoa(y - 1), strconv.Itoa(y + 1)}
		}
	}
	for _, year := range years {
		results, err := g.tmdbSearch(ctx, title, year)
		if err != nil || len(results) > 0 {
			return tmdbPosterOf(results), err
		}
	}
	results, err := g.tmdbSearch(ctx, title, "")
	if err != nil || len(results) != 1 {
		return "", err
	}
	return tmdbPosterOf(results), nil
}

// curlyQuotes straightens the typographic quotes in Criterion's titles, which
// TMDB's search doesn't match.
var curlyQuotes = strings.NewReplacer("\u2018", "'", "\u2019", "'", "\u201c", `"`, "\u201d", `"`)

type tmdbMovie struct {
	PosterPath string `json:"poster_path"`
}

// tmdbSearch runs a TMDB movie search, narrowed to a release year unless year
// is "".
func (g *guide) tmdbSearch(ctx context.Context, title, year string) ([]tmdbMovie, error) {
	q := url.Values{"query": {title}, "include_adult": {"false"}, "language": {"en-US"}}
	if year != "" {
		q.Set("year", year)
	}
	var header http.Header
	// A v4 read access token is a JWT, sent as a bearer token; a v3 API key
	// goes in the query.
	if strings.HasPrefix(g.tmdbKey, "eyJ") {
		header = http.Header{"Authorization": {"Bearer " + g.tmdbKey}}
	} else {
		q.Set("api_key", g.tmdbKey)
	}
	var body struct {
		Results []tmdbMovie `json:"results"`
	}
	if _, err := doJSON(ctx, g.http, http.MethodGet, g.tmdbURL+"/3/search/movie?"+q.Encode(), header, nil, &body); err != nil {
		if errors.Is(err, errUnauthorized) {
			return nil, errors.New("TMDB rejected TMDB_API_KEY")
		}
		return nil, err
	}
	return body.Results, nil
}

// tmdbPosterOf returns the top result's poster URL, or "" if it has none.
func tmdbPosterOf(results []tmdbMovie) string {
	if len(results) == 0 || !posterPathPattern.MatchString(results[0].PosterPath) {
		return ""
	}
	return tmdbPosterURL + results[0].PosterPath
}

type xmltvIcon struct {
	Src string `xml:"src,attr"`
}

type xmltvRating struct {
	System string `xml:"system,attr"`
	Value  string `xml:"value"`
}

type xmltvProgramme struct {
	Start      string        `xml:"start,attr"`
	Stop       string        `xml:"stop,attr"`
	Channel    string        `xml:"channel,attr"`
	Title      string        `xml:"title"`
	Desc       string        `xml:"desc,omitempty"`
	Date       string        `xml:"date,omitempty"` // release date; Channels takes the film's year from it
	Categories []string      `xml:"category"`
	Icon       *xmltvIcon    `xml:"icon"` // XMLTV puts icon after category and before rating
	Ratings    []xmltvRating `xml:"rating"`
}

func xmltv(schedule []scheduleEntry, films map[string]film) ([]byte, error) {
	const stamp = "20060102150405 -0700"
	doc := struct {
		XMLName   xml.Name `xml:"tv"`
		Generator string   `xml:"generator-info-name,attr"`
		Channel   struct {
			ID   string    `xml:"id,attr"`
			Name string    `xml:"display-name"`
			Icon xmltvIcon `xml:"icon"`
		} `xml:"channel"`
		Programmes []xmltvProgramme `xml:"programme"`
	}{Generator: "criterion-bridge"}
	doc.Channel.ID, doc.Channel.Name, doc.Channel.Icon = channelID, channelName, xmltvIcon{channelLogo}

	for _, e := range schedule {
		if e.Title == "" || !e.EndTime.After(e.StartTime) {
			continue
		}
		p := xmltvProgramme{
			Start:   e.StartTime.UTC().Format(stamp),
			Stop:    e.EndTime.UTC().Format(stamp),
			Channel: channelID,
			Title:   e.Title,
			Desc:    or(e.LongDescription, e.ShortDescription),
			Date:    films[e.MediaID].date,
		}
		// Each film's artwork is its TMDB portrait poster when there is one,
		// or else its JW Player poster: Criterion's widescreen title card with
		// a still, the director, title and year.
		switch {
		case films[e.MediaID].poster != "":
			p.Icon = &xmltvIcon{films[e.MediaID].poster}
		case mediaIDPattern.MatchString(e.MediaID):
			p.Icon = &xmltvIcon{jwPlayerURL + "/v2/media/" + e.MediaID + "/poster.jpg?width=1280"}
		}
		// Channels DVR treats a "Movie" category as a film. Criterion's
		// matching "Movies" genre only repeats it, so it's left out.
		for _, c := range append([]string{e.ProgramType}, e.Genres...) {
			if c != "" && !slices.Contains(p.Categories, c) && !strings.EqualFold(c, e.ProgramType+"s") {
				p.Categories = append(p.Categories, c)
			}
		}
		for _, r := range e.Ratings {
			if r.Code != "" {
				p.Ratings = append(p.Ratings, xmltvRating{System: or(r.Body, "US"), Value: r.Code})
			}
		}
		doc.Programmes = append(doc.Programmes, p)
	}

	b, err := xml.MarshalIndent(doc, "", " ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), append(b, '\n')...), nil
}

// healthcheck asks the instance listening on listenAddr for /healthz and
// returns the exit code: 0 if it answers 200, 1 if not.
func healthcheck(listenAddr string) int {
	_, port, err := net.SplitHostPort(or(listenAddr, ":8080"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck:", resp.Status)
		return 1
	}
	return 0
}

func or(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
