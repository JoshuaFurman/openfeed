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
	"net/url"
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
		"link": link,
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

// New returns the server's handler. A launch refresh of the whole river reaches the newest post it showed,
// as `g` does in the TUI, and a tag-filtered view never reaches; state changes arrive only by same-origin POST.
func New(river River, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	h := &handler{river: river, log: log, assets: assetVersion()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.home)
	mux.HandleFunc("GET /older", h.older)
	mux.HandleFunc("POST /refresh", h.refresh)
	mux.HandleFunc("POST /settings", h.settings)
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
	Title, Excerpt, Feed string
	Link                 string // Where a tap goes: the article, or its archive.ph snapshot; empty without one.
	Host                 string // The article's site, without "www.".
	Archived             bool   // Link goes through archive.ph.
	Tags                 []tag
	Date                 time.Time
	Age                  string
}

// alert is one failing feed in the bell's list.
type alert struct {
	Feed, Reason string
}

type tag struct {
	Name  string
	Color int
}

// view is everything a template renders.
type view struct {
	Assets    string
	Theme     string // "light", "dark", or empty to follow the device.
	Archive   bool   // Articles open through archive.ph.
	Tag       tag    // The filter, spelled as River.Tags lists it; its Name is empty for the whole river.
	Tags      []tag  // The filter picker's choices.
	Posts     []post
	Next      *reader.Position // Older continuation; nil means no older posts for now.
	Head      *reader.Position // The newest post shown, which a refresh checks for newer posts above.
	NoFeeds   bool
	Alerts    []alert
	Refreshed bool // The bell's list reflects a finished refresh.
	Newer     bool
	ReadFail  bool
}

func (h *handler) home(w http.ResponseWriter, r *http.Request) {
	filter, ok := h.filter(r)
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther) // A tag no feed carries any more shows the whole river.
		return
	}
	prefs := preferences(r)
	v := view{Assets: h.assets, Theme: prefs.Theme, Archive: prefs.Archive, Tag: filter, Tags: h.tags(), NoFeeds: len(h.river.Feeds()) == 0}
	page, err := h.river.Timeline(r.Context(), reader.Query{Tag: filter.Name, Limit: pageSize})
	if err != nil {
		h.log.Warn("web: read river", "error", err)
		v.ReadFail = true
	} else {
		h.fill(&v, page)
	}
	h.render(w, "page", v)
}

func (h *handler) older(w http.ResponseWriter, r *http.Request) {
	filter, ok := h.filter(r)
	var before reader.Position
	if err := before.UnmarshalText([]byte(r.URL.Query().Get("before"))); err != nil || !ok {
		http.Error(w, "invalid position or tag", http.StatusBadRequest)
		return
	}
	v := view{Tag: filter, Archive: preferences(r).Archive}
	page, err := h.river.Timeline(r.Context(), reader.Query{Tag: filter.Name, Before: &before, Limit: pageSize})
	if err != nil {
		h.log.Warn("web: read older posts", "error", err)
		v.ReadFail, v.Next = true, &before // Retrying reads the same page.
	} else {
		h.fill(&v, page)
	}
	h.render(w, "posts", v)
}

// refresh reaches the newest post a whole-river page showed, runs a refresh round, and answers with the
// notice slot: a pill when posts newer than that head exist in the view, with the bell out of band.
func (h *handler) refresh(w http.ResponseWriter, r *http.Request) {
	filter, ok := h.filter(r)
	if !ok {
		http.Error(w, "unknown tag", http.StatusBadRequest)
		return
	}
	var shown *reader.Position
	if text := r.URL.Query().Get("head"); text != "" {
		shown = new(reader.Position)
		if err := shown.UnmarshalText([]byte(text)); err != nil {
			http.Error(w, "invalid position", http.StatusBadRequest)
			return
		}
		if filter.Name == "" {
			h.reach(r.Context(), *shown) // The filter hid posts below the head, so a filtered view has not passed them.
		}
	}
	report, err := h.river.Refresh(r.Context())
	if err != nil {
		return // The client went away.
	}
	v := view{Tag: filter, Alerts: alerts(report), Refreshed: true}
	if page, err := h.river.Timeline(r.Context(), reader.Query{Tag: filter.Name, After: shown, Limit: 1}); err == nil {
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

// alerts lists the feeds whose latest refresh failed, with why, in config order.
func alerts(report reader.Report) []alert {
	var list []alert
	for _, result := range report.Feeds {
		if result.Status == reader.Failing {
			list = append(list, alert{Feed: result.Feed.Name, Reason: result.Err.Error()})
		}
	}
	return list
}

const (
	themeCookie   = "theme"
	openCookie    = "open"
	cookieAge     = 400 * 24 * 60 * 60 // Browsers cap cookies at 400 days.
	archivePrefix = "https://archive.ph/latest/"
)

// prefs are one browser's settings, kept in cookies.
type prefs struct {
	Theme   string // "light", "dark", or empty to follow the device.
	Archive bool   // Open articles through archive.ph's latest snapshot.
}

// preferences reads the request's settings, ignoring values the settings form could not have set.
func preferences(r *http.Request) prefs {
	var p prefs
	if c, err := r.Cookie(themeCookie); err == nil && (c.Value == "light" || c.Value == "dark") {
		p.Theme = c.Value
	}
	if c, err := r.Cookie(openCookie); err == nil && c.Value == "archive" {
		p.Archive = true
	}
	return p
}

// settings saves one setting from the form: theme (light, dark, or auto) or open (direct or archive).
// htmx gets the menu back, restyled in place, or a page reload when card links change; plain form posts
// return to the river.
func (h *handler) settings(w http.ResponseWriter, r *http.Request) {
	p := preferences(r)
	reload := false
	switch theme, open := r.PostFormValue("theme"), r.PostFormValue("open"); {
	case theme != "" && open == "" && (theme == "light" || theme == "dark" || theme == "auto"):
		p.Theme = strings.TrimPrefix(theme, "auto")
		remember(w, themeCookie, p.Theme)
	case open != "" && theme == "" && (open == "direct" || open == "archive"):
		reload = p.Archive != (open == "archive")
		p.Archive = open == "archive"
		remember(w, openCookie, strings.TrimPrefix(open, "direct"))
	default:
		http.Error(w, "unknown setting", http.StatusBadRequest)
		return
	}
	switch {
	case r.Header.Get("HX-Request") != "true":
		http.Redirect(w, r, "/", http.StatusSeeOther)
	case reload:
		w.Header().Set("HX-Refresh", "true")
	default:
		h.render(w, "settings", view{Theme: p.Theme, Archive: p.Archive})
	}
}

// remember keeps a setting in a cookie, or forgets it when value is empty, the default.
func remember(w http.ResponseWriter, name, value string) {
	c := &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: cookieAge, HttpOnly: true, SameSite: http.SameSiteLaxMode}
	if value == "" {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}

// tags lists the configured tags with their colors, in River.Tags order.
func (h *handler) tags() []tag {
	names := h.river.Tags()
	list := make([]tag, len(names))
	for i, name := range names {
		list[i] = tag{Name: name, Color: i % tagColors}
	}
	return list
}

// filter is the request's tag filter as River.Tags spells it, or the zero tag for the whole river.
// It reports false for a tag no configured feed carries.
func (h *handler) filter(r *http.Request) (tag, bool) {
	wanted := strings.TrimSpace(r.URL.Query().Get("tag"))
	if wanted == "" {
		return tag{}, true
	}
	for _, t := range h.tags() {
		if strings.EqualFold(t.Name, wanted) {
			return t, true
		}
	}
	return tag{}, false
}

func (h *handler) fill(v *view, page reader.Page) {
	colors := make(map[string]int)
	for _, t := range h.tags() {
		colors[t.Name] = t.Color
	}
	now := time.Now()
	for _, item := range page.Items {
		p := post{Title: item.Title, Link: item.URL, Host: host(item.URL), Excerpt: excerpt(item.Content), Feed: item.FeedName, Date: item.EffectiveDate()}
		if v.Archive && p.Link != "" {
			p.Link, p.Archived = archivePrefix+p.Link, true // archive.ph expects the URL unencoded.
		}
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

// host is an article URL's site for display, without "www.".
func host(articleURL string) string {
	u, err := url.Parse(articleURL)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

// link builds a same-site URL from path and key/value pairs, omitting empty values. Values are strings
// or positions, which travel as text tokens.
func link(path string, pairs ...any) (string, error) {
	if len(pairs)%2 != 0 {
		return "", errors.New("link needs key/value pairs")
	}
	query := url.Values{}
	for i := 0; i < len(pairs); i += 2 {
		key, _ := pairs[i].(string)
		switch value := pairs[i+1].(type) {
		case string:
			if value != "" {
				query.Set(key, value)
			}
		case *reader.Position:
			if value != nil {
				text, err := value.MarshalText()
				if err != nil {
					return "", err
				}
				query.Set(key, string(text))
			}
		default:
			return "", fmt.Errorf("link value %T", value)
		}
	}
	if len(query) == 0 {
		return path, nil
	}
	return path + "?" + query.Encode(), nil
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
