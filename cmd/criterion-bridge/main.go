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
//	LISTEN_ADDR         where to serve (default :8080)
//
// "criterion-bridge healthcheck" checks a running instance's /healthz, for
// Docker's HEALTHCHECK in an image with no shell or curl.
package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// mediaIDPattern matches a JW Player media ID, which goes into an image URL.
var mediaIDPattern = regexp.MustCompile(`^[A-Za-z0-9]{1,32}$`)

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
	g := &guide{http: &http.Client{Timeout: 30 * time.Second}, baseURL: criterionURL, jwURL: jwPlayerURL}

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
	baseURL string
	jwURL   string // JW Player's media API, which has each film's release date

	mu      sync.Mutex
	fetched time.Time
	xml     []byte
	dates   map[string]string // XMLTV release date by media ID; "" when a film has none
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
	return xmltv(body.Schedule, g.releaseDates(ctx, body.Schedule))
}

// releaseDates looks up each scheduled film's release date in JW Player's
// public metadata, a few at a time. A film's date doesn't change, so each is
// looked up once and kept while the film stays on the schedule. A failed
// lookup leaves that film without a date and is retried next refresh.
func (g *guide) releaseDates(ctx context.Context, schedule []scheduleEntry) map[string]string {
	dates := map[string]string{}
	var missing []string
	for _, e := range schedule {
		if _, seen := dates[e.MediaID]; seen || !mediaIDPattern.MatchString(e.MediaID) {
			continue
		}
		if d, ok := g.dates[e.MediaID]; ok {
			dates[e.MediaID] = d
		} else {
			dates[e.MediaID] = ""
			missing = append(missing, e.MediaID)
		}
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for _, id := range missing {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			var body struct {
				Playlist []struct {
					ReleaseDate string `json:"release_date"`
				} `json:"playlist"`
			}
			if _, err := doJSON(ctx, g.http, http.MethodGet, g.jwURL+"/v2/media/"+id, nil, nil, &body); err != nil {
				mu.Lock()
				delete(dates, id) // not cached, so it's tried again
				mu.Unlock()
				return
			}
			d := ""
			if len(body.Playlist) > 0 {
				d = xmltvDate(body.Playlist[0].ReleaseDate)
			}
			mu.Lock()
			dates[id] = d
			mu.Unlock()
		})
	}
	wg.Wait()
	g.dates = dates
	return dates
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

func xmltv(schedule []scheduleEntry, dates map[string]string) ([]byte, error) {
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
			Date:    dates[e.MediaID],
		}
		// Each film's artwork is its JW Player poster: Criterion's title card
		// with a still, the director, title and year.
		if mediaIDPattern.MatchString(e.MediaID) {
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
