package rss

import "testing"

const rss2 = `<?xml version="1.0"?>
<rss version="2.0"><channel><title>News</title>
  <item><guid>g3</guid><title>Third</title><link>http://x/3</link><description>d3</description><pubDate>Wed, 03 Jun 2026</pubDate></item>
  <item><guid>g2</guid><title>Second</title><link>http://x/2</link><description>d2</description></item>
  <item><guid>g1</guid><title>First</title><link>http://x/1</link></item>
</channel></rss>`

const atom = `<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom"><title>Blog</title>
  <entry><id>a2</id><title>Newer</title><link href="http://y/2"/><summary>s2</summary><updated>2026-06-03</updated></entry>
  <entry><id>a1</id><title>Older</title><link href="http://y/1"/><summary>s1</summary></entry>
</feed>`

func TestParse_RSS(t *testing.T) {
	items, newest, err := parseFeed([]byte(rss2))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || newest != "g3" {
		t.Fatalf("rss parse: %d items newest=%q", len(items), newest)
	}
	if items[0].ID != "g3" || items[0].Payload["title"] != "Third" || items[0].Payload["link"] != "http://x/3" {
		t.Fatalf("rss item0 wrong: %+v", items[0])
	}
	if items[0].Payload["summary"] != "d3" {
		t.Fatalf("rss description not mapped: %+v", items[0].Payload)
	}
}

func TestParse_Atom(t *testing.T) {
	items, newest, err := parseFeed([]byte(atom))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || newest != "a2" {
		t.Fatalf("atom parse: %d items newest=%q", len(items), newest)
	}
	if items[0].ID != "a2" || items[0].Payload["link"] != "http://y/2" || items[0].Payload["summary"] != "s2" {
		t.Fatalf("atom item0 wrong: %+v", items[0])
	}
}

func TestParse_Empty(t *testing.T) {
	items, newest, err := parseFeed([]byte(`<rss version="2.0"><channel><title>x</title></channel></rss>`))
	if err != nil || len(items) != 0 || newest != "" {
		t.Fatalf("empty feed: items=%d newest=%q err=%v", len(items), newest, err)
	}
}
