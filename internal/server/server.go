// Package server serves the river as a mobile-first web page for `openfeed serve`.
// It has no authentication: anyone who can reach its address can read the river and refresh it.
package server

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/JoshuaFurman/openfeed/internal/config"
	"github.com/JoshuaFurman/openfeed/internal/reader"
)

// River is the reader behavior the server depends on; *reader.Reader implements it.
type River interface {
	Feeds() []config.Feed
	Tags() []string
	Timeline(ctx context.Context, query reader.Query) (reader.Page, error)
	Refresh(ctx context.Context) (reader.Report, error)
	Reached(position reader.Position)
	SaveSeen(ctx context.Context) error
}

const (
	pageSize     = 50
	shutdownWait = 5 * time.Second
	tagColors    = 12 // CSS classes tag-0 through tag-11, assigned by position in River.Tags.
)

var (
	//go:embed assets templates
	files     embed.FS
	templates = template.Must(template.New("").Funcs(template.FuncMap{
		"token": token,
	}).ParseFS(files, "templates/*.html"))
)

// Serve answers requests on listener until ctx ends, then waits up to five seconds for requests in
// flight and saves the reading position. It returns ctx's error when canceled.
func Serve(ctx context.Context, listener net.Listener, river River, log *slog.Logger) error {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	srv := &http.Server{
		Handler:           New(river, log),
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(listener) }()

	var err error
	select {
	case err = <-served:
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownWait)
		defer cancel()
		err = errors.Join(ctx.Err(), srv.Shutdown(shutdown))
		<-served
	}
	save, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownWait)
	defer cancel()
	return errors.Join(err, river.SaveSeen(save))
}

// New returns the server's handler. A page's launch refresh reaches the newest post it showed, as `g`
// does in the TUI; state changes arrive only by same-origin POST.
func New(river River, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	h := &handler{river: river, log: log, assets: assetVersion()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.home)
	mux.HandleFunc("GET /older", h.older)
	mux.HandleFunc("POST /refresh", h.refresh)
	mux.Handle("GET /favicon.ico", http.RedirectHandler("/assets/icon.svg?v="+h.assets, http.StatusMovedPermanently))
	static, _ := fs.Sub(files, "assets")
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", immutable(http.FileServerFS(static))))
	return secure(http.NewCrossOriginProtection().Handler(mux))
}

type handler struct {
	river  River
	log    *slog.Logger
	assets string // Content hash appended to asset URLs, so browsers may cache them forever.
}

// post is one river row, holding only what the page shows.
type post struct {
	Title, URL, Excerpt, Feed string
	Tags                      []tag
	Date                      time.Time
	Age                       string
}

type tag struct {
	Name  string
	Color int
}

// view is everything a template renders.
type view struct {
	Assets   string
	Posts    []post
	Next     *reader.Position // Older continuation; nil means no older posts for now.
	Head     *reader.Position // The newest post shown, which a refresh checks for newer posts above.
	NoFeeds  bool
	Failing  string
	Newer    bool
	ReadFail bool
}

func (h *handler) home(w http.ResponseWriter, r *http.Request) {
	v := view{Assets: h.assets, NoFeeds: len(h.river.Feeds()) == 0}
	page, err := h.river.Timeline(r.Context(), reader.Query{Limit: pageSize})
	if err != nil {
		h.log.Warn("web: read river", "error", err)
		v.ReadFail = true
	} else {
		h.fill(&v, page)
	}
	h.render(w, "page", v)
}

func (h *handler) older(w http.ResponseWriter, r *http.Request) {
	var before reader.Position
	if err := before.UnmarshalText([]byte(r.URL.Query().Get("before"))); err != nil {
		http.Error(w, "invalid position", http.StatusBadRequest)
		return
	}
	v := view{}
	page, err := h.river.Timeline(r.Context(), reader.Query{Before: &before, Limit: pageSize})
	if err != nil {
		h.log.Warn("web: read older posts", "error", err)
		v.ReadFail, v.Next = true, &before // Retrying reads the same page.
	} else {
		h.fill(&v, page)
	}
	h.render(w, "posts", v)
}

// refresh reaches the newest post the page showed, runs a refresh round, and answers with the notice
// slot: a pill when posts newer than that head exist, and any failing feeds.
func (h *handler) refresh(w http.ResponseWriter, r *http.Request) {
	var shown *reader.Position
	if text := r.URL.Query().Get("head"); text != "" {
		shown = new(reader.Position)
		if err := shown.UnmarshalText([]byte(text)); err != nil {
			http.Error(w, "invalid position", http.StatusBadRequest)
			return
		}
		h.reach(r.Context(), *shown)
	}
	report, err := h.river.Refresh(r.Context())
	if err != nil {
		return // The client went away.
	}
	v := view{Failing: failing(report)}
	if page, err := h.river.Timeline(r.Context(), reader.Query{After: shown, Limit: 1}); err == nil {
		v.Newer = len(page.Items) > 0 && shown != nil
	} else {
		h.log.Warn("web: check for newer posts", "error", err)
	}
	h.render(w, "notice", v)
}

// reach records the shown head as read, as `g` does, capped at the river's real head so a forged
// position cannot pass posts that do not exist yet.
func (h *handler) reach(ctx context.Context, shown reader.Position) {
	head, err := h.river.Timeline(ctx, reader.Query{Limit: 1})
	if err != nil || len(head.Items) == 0 {
		return
	}
	if newest := head.Items[0].Position; newest.Compare(shown) < 0 {
		shown = newest
	}
	h.river.Reached(shown)
	_ = h.river.SaveSeen(ctx) // The reader logs failures; reading goes on.
}

// failing names the feeds whose latest refresh failed, or is empty when none did.
func failing(report reader.Report) string {
	var names []string
	for _, result := range report.Feeds {
		if result.Status == reader.Failing {
			names = append(names, result.Feed.Name)
		}
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0] + " is failing to refresh"
	}
	return fmt.Sprintf("%d feeds failing: %s", len(names), strings.Join(names, ", "))
}

func (h *handler) fill(v *view, page reader.Page) {
	colors := make(map[string]int)
	for i, t := range h.river.Tags() {
		colors[t] = i % tagColors
	}
	now := time.Now()
	for _, item := range page.Items {
		p := post{Title: item.Title, URL: item.URL, Excerpt: excerpt(item.Content), Feed: item.FeedName, Date: item.EffectiveDate()}
		if p.Excerpt == strings.TrimSpace(p.Title) {
			p.Excerpt = "" // Title-only feeds repeat the title as content.
		}
		p.Age = age(p.Date, now)
		for _, t := range item.FeedTags {
			p.Tags = append(p.Tags, tag{Name: t, Color: colors[t]})
		}
		v.Posts = append(v.Posts, p)
	}
	if len(page.Items) > 0 {
		v.Head = &page.Items[0].Position
	}
	v.Next = page.Next
}

func (h *handler) render(w http.ResponseWriter, name string, v view) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := templates.ExecuteTemplate(w, name, v); err != nil {
		h.log.Warn("web: render", "template", name, "error", err)
	}
}

// token encodes a position for a URL; templates only receive positions from the reader.
func token(p *reader.Position) (string, error) {
	text, err := p.MarshalText()
	return string(text), err
}

// age is how long ago a post's river date was, in the most compact useful unit.
func age(date, now time.Time) string {
	elapsed := now.Sub(date)
	switch {
	case elapsed < time.Minute:
		return "now"
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm", int(elapsed.Minutes()))
	case elapsed < 24*time.Hour:
		return fmt.Sprintf("%dh", int(elapsed.Hours()))
	case elapsed < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(elapsed.Hours()/24))
	case date.Local().Year() == now.Local().Year():
		return date.Local().Format("Jan 2")
	}
	return date.Local().Format("Jan 2, 2006")
}

func assetVersion() string {
	sum := sha256.New()
	_ = fs.WalkDir(files, "assets", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			data, _ := files.ReadFile(path)
			sum.Write([]byte(path))
			sum.Write(data)
		}
		return err
	})
	return hex.EncodeToString(sum.Sum(nil))[:12]
}

// immutable lets browsers cache assets for a year; their URLs change with their content.
func immutable(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		next.ServeHTTP(w, r)
	})
}

// secure confines pages to their own scripts and styles and keeps the server's address from article sites.
func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}
