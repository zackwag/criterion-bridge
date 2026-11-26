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
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	criterionURL = "https://www.criterionchannel.com"
	// mediaID is Criterion 24/7's ID, from its page's URL.
	mediaID     = "1emmgvqX"
	channelID   = "criterion-24-7"
	channelName = "Criterion 24/7"
	channelLogo = "https://cdn.jwplayer.com/v2/media/" + mediaID + "/poster.jpg?width=720"
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
	g := &guide{http: &http.Client{Timeout: 30 * time.Second}, baseURL: criterionURL}

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

	mu      sync.Mutex
	fetched time.Time
	xml     []byte
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
	if err := doJSON(ctx, g.http, http.MethodGet, g.baseURL+"/api/media/"+mediaID+"?fresh=1", nil, &body); err != nil {
		return nil, fmt.Errorf("fetch schedule: %w", err)
	}
	if len(body.Schedule) == 0 {
		return nil, errors.New("criterion's schedule was empty")
	}
	return xmltv(body.Schedule)
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
	Categories []string      `xml:"category"`
	Ratings    []xmltvRating `xml:"rating"`
}

func xmltv(schedule []scheduleEntry) ([]byte, error) {
	const stamp = "20060102150405 -0700"
	type icon struct {
		Src string `xml:"src,attr"`
	}
	doc := struct {
		XMLName   xml.Name `xml:"tv"`
		Generator string   `xml:"generator-info-name,attr"`
		Channel   struct {
			ID   string `xml:"id,attr"`
			Name string `xml:"display-name"`
			Icon icon   `xml:"icon"`
		} `xml:"channel"`
		Programmes []xmltvProgramme `xml:"programme"`
	}{Generator: "criterion-bridge"}
	doc.Channel.ID, doc.Channel.Name, doc.Channel.Icon = channelID, channelName, icon{channelLogo}

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
		}
		// Channels DVR treats a "Movie" category as a film.
		for _, c := range append([]string{e.ProgramType}, e.Genres...) {
			if c != "" && !slices.Contains(p.Categories, c) {
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
