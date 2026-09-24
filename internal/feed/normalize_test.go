package feed

import (
	"encoding/json"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseFixtures(t *testing.T) {
	for _, name := range []string{"rss.xml", "atom.xml", "json.json"} {
		t.Run(name, func(t *testing.T) {
			base, err := url.Parse("https://example.com/feeds/index")
			if err != nil {
				t.Fatal(err)
			}
			got, err := parse(fixture(t, name), base)
			if err != nil {
				t.Fatal(err)
			}
			var want []Item
			if err := json.Unmarshal(fixture(t, name+".golden.json"), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				data, _ := json.MarshalIndent(got, "", "  ")
				t.Errorf("items differ from golden file; got:\n%s", data)
			}
		})
	}
}

func TestParsePreservesSourceOrderAndDuplicateKeys(t *testing.T) {
	base, err := url.Parse("https://example.com/feed")
	if err != nil {
		t.Fatal(err)
	}
	body := `<rss version="2.0"><channel>
		<item><guid isPermaLink="false">same</guid><title>First</title></item>
		<item><guid isPermaLink="false">empty</guid></item>
		<item><guid isPermaLink="false">same</guid><title>Second</title></item>
	</channel></rss>`
	items, err := parse([]byte(body), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Title != "First" || items[1].Title != "Second" || items[0].Key != items[1].Key {
		t.Errorf("fetching must leave ordering and deduplication to storage: %+v", items)
	}
}

func TestParseSkipsOnlyEmptyEntries(t *testing.T) {
	base, err := url.Parse("https://example.com/feed")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		entry string
		want  Item
	}{
		{"empty", "", Item{}},
		{"whitespace", "<title> \t </title><description>\n </description><link> </link>", Item{}},
		{"identifier only", `<guid isPermaLink="false">post-1</guid>`, Item{}},
		{"date only", "<pubDate>Tue, 02 Sep 2025 13:00:00 GMT</pubDate>", Item{}},
		{"unusable link only", "<link>javascript:alert(1)</link>", Item{}},
		{"malformed link only", "<link>%zz</link>", Item{}},
		{"title only", "<title> Title </title>", Item{Title: "Title"}},
		{"description only", "<description> Summary </description>", Item{Content: "Summary"}},
		{"content only", "<content:encoded> Content </content:encoded>", Item{Content: "Content"}},
		{"blank content with description", "<content:encoded> </content:encoded><description>Summary</description>", Item{Content: "Summary"}},
		{"URL only", "<link>/post</link>", Item{URL: "https://example.com/post"}},
		{"title with unusable link", "<title>Title</title><link>javascript:alert(1)</link>", Item{Title: "Title"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/"><channel><item>` + tc.entry + `</item></channel></rss>`
			items, err := parse([]byte(body), base)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == (Item{}) {
				if len(items) != 0 {
					t.Errorf("empty entry was retained: %+v", items)
				}
				return
			}
			if len(items) != 1 {
				t.Fatalf("got %d entries, want 1", len(items))
			}
			got := items[0]
			if got.Key == "" || got.Title != tc.want.Title || got.Content != tc.want.Content || got.URL != tc.want.URL {
				t.Errorf("got %+v, want content fields %+v and a nonempty key", got, tc.want)
			}
		})
	}
}

func TestArticleURL(t *testing.T) {
	base, err := url.Parse("https://example.com/feeds/index")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		link string
		want string
	}{
		{"absolute", " HTTPS://EXAMPLE.COM/CaseSensitive?q=A#part ", "https://example.com/CaseSensitive?q=A#part"},
		{"relative", "../posts/1", "https://example.com/posts/1"},
		{"root relative", "/posts/1", "https://example.com/posts/1"},
		{"scheme relative", "//OTHER.Example/posts/1", "https://other.example/posts/1"},
		{"empty", "", ""},
		{"whitespace", "  \n", ""},
		{"bad escape", "%zz", ""},
		{"script", "javascript:alert(1)", ""},
		{"file", "file:///etc/passwd", ""},
		{"mail", "mailto:author@example.com", ""},
		{"no host", "https:///posts/1", ""},
		{"no hostname", "https://:443/posts/1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := articleURL(tc.link, base); got != tc.want {
				t.Errorf("articleURL(%q) = %q, want %q", tc.link, got, tc.want)
			}
		})
	}
}

func TestItemKeyHierarchy(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
		item Item
		want string
	}{
		{"ID before URL", " post-1 ", Item{URL: "https://example.com/1"}, "id:post-1"},
		{"opaque ID", "url:https://example.com/1", Item{}, "id:url:https://example.com/1"},
		{"URL before content", "", Item{URL: "https://example.com/1", Title: "Title"}, "url:https://example.com/1"},
		{"blank ID", " \n", Item{URL: "https://example.com/1"}, "url:https://example.com/1"},
		{"content hash", "", Item{Title: "Undated", Content: "Text without an identifier."}, "sha256:7296bd8e80f663b676160d4159fade3afc619d4dce80a5e1b533dfcd0c11654d"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := itemKey(tc.id, tc.item); got != tc.want {
				t.Errorf("itemKey = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestItemKeyStability(t *testing.T) {
	date := time.Date(2025, 9, 2, 13, 0, 0, 0, time.UTC)
	original := Item{Title: "Title", Content: "Content", PublishedAt: date}
	for _, tc := range []struct {
		name     string
		id       string
		before   Item
		after    Item
		wantSame bool
	}{
		{"unchanged", "", original, original, true},
		{"same instant", "", original, Item{Title: "Title", Content: "Content", PublishedAt: date.In(time.FixedZone("east", 7200))}, true},
		{"edited title", "", original, Item{Title: "New title", Content: "Content", PublishedAt: date}, false},
		{"edited content", "", original, Item{Title: "Title", Content: "New content", PublishedAt: date}, false},
		{"corrected date", "", original, Item{Title: "Title", Content: "Content", PublishedAt: date.Add(time.Hour)}, false},
		{"stable publisher ID", "post-1", original, Item{Title: "New title", URL: "https://example.com/new"}, true},
		{"stable URL", "", Item{URL: "https://example.com/1", Title: "Old"}, Item{URL: "https://example.com/1", Title: "New", PublishedAt: date}, true},
		{"undated content differs", "", Item{Title: "Same", Content: "First"}, Item{Title: "Same", Content: "Second"}, false},
		{"field boundaries", "", Item{Title: "ab", Content: "c"}, Item{Title: "a", Content: "bc"}, false},
		{"embedded separator", "", Item{Title: "a\x00", Content: "b"}, Item{Title: "a", Content: "\x00b"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, after := itemKey(tc.id, tc.before), itemKey(tc.id, tc.after)
			if same := before == after; same != tc.wantSame {
				t.Errorf("keys %q and %q equal = %v, want %v", before, after, same, tc.wantSame)
			}
		})
	}
	if itemKey("https://example.com/1", Item{}) == itemKey("", Item{URL: "https://example.com/1"}) {
		t.Error("publisher IDs must not collide with URL identities")
	}
}

func TestAtomUpdateDateDoesNotChangeFallbackIdentity(t *testing.T) {
	base, err := url.Parse("https://example.com/feed")
	if err != nil {
		t.Fatal(err)
	}
	body := string(fixture(t, "atom.xml"))
	first, err := parse([]byte(body), base)
	if err != nil {
		t.Fatal(err)
	}
	second, err := parse([]byte(strings.ReplaceAll(body, "2025-09-03T15:00:00Z", "2025-09-04T15:00:00Z")), base)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Error("changing only Atom update dates changed normalized items")
	}
}

func TestAtomTranslatorRejectsWrongSource(t *testing.T) {
	if _, err := (atomTranslator{}).Translate(&gofeed.Feed{}); err == nil {
		t.Error("expected error for non-Atom source")
	}
}
