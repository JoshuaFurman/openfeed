package feed

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
	"github.com/mmcdole/gofeed/atom"
)

func parse(body []byte, base *url.URL) ([]Item, error) {
	parser := gofeed.NewParser()
	parser.AtomTranslator = atomTranslator{}
	parsed, err := parser.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	items := make([]Item, 0, len(parsed.Items))
	for _, raw := range parsed.Items {
		item := Item{
			Title:   strings.TrimSpace(raw.Title),
			URL:     articleURL(raw.Link, base),
			Content: strings.TrimSpace(raw.Content),
		}
		if item.Content == "" {
			item.Content = strings.TrimSpace(raw.Description)
		}
		if item.Title == "" && item.Content == "" && item.URL == "" {
			continue
		}
		if raw.PublishedParsed != nil {
			item.PublishedAt = raw.PublishedParsed.UTC()
		}
		item.Key = itemKey(raw.GUID, item)
		items = append(items, item)
	}
	return items, nil
}

func articleURL(link string, base *url.URL) string {
	link = strings.TrimSpace(link)
	if link == "" {
		return ""
	}
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	u = base.ResolveReference(u)
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return ""
	}
	u.Host = strings.ToLower(u.Host)
	return u.String()
}

func itemKey(id string, item Item) string {
	if id = strings.TrimSpace(id); id != "" {
		return "id:" + id
	}
	if item.URL != "" {
		return "url:" + item.URL
	}

	published := ""
	if !item.PublishedAt.IsZero() {
		published = item.PublishedAt.UTC().Format(time.RFC3339Nano)
	}
	hash := sha256.New()
	for _, field := range []string{item.Title, item.Content, published} {
		// Length prefixes prevent ambiguous concatenations, including embedded delimiters.
		fmt.Fprintf(hash, "%d:%s", len(field), field)
	}
	return fmt.Sprintf("sha256:%x", hash.Sum(nil))
}

type atomTranslator struct{}

// Translate preserves missing publication dates instead of promoting mutable update dates.
func (atomTranslator) Translate(source any) (*gofeed.Feed, error) {
	translated, err := (&gofeed.DefaultAtomTranslator{}).Translate(source)
	if err != nil {
		return nil, err
	}
	for i, entry := range source.(*atom.Feed).Entries {
		translated.Items[i].Published = entry.Published
		translated.Items[i].PublishedParsed = entry.PublishedParsed
	}
	return translated, nil
}
