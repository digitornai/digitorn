package indexer

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// TestDBAccessConnector_MySQL_Live proves the RAG indexer can Walk a remote
// MySQL table through the shared dbaccess socle : each row → a Document with a
// stable id and the configured text. This is the "index a long MySQL table"
// case, streamed row-by-row.
func TestDBAccessConnector_MySQL_Live(t *testing.T) {
	dsn := os.Getenv("DBACCESS_MYSQL_NATIVE")
	if dsn == "" {
		dsn = "root:root@tcp(localhost:3307)/ragtest"
	}
	raw, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if raw.PingContext(ctx) != nil {
		t.Skipf("no mysql at %s", dsn)
	}
	for _, s := range []string{
		"DROP TABLE IF EXISTS articles",
		"CREATE TABLE articles (id int PRIMARY KEY, title varchar(128), body text, author varchar(64))",
		"INSERT INTO articles VALUES (1,'Go at scale','Streaming rows keeps memory bounded.','alice'),(2,'Indexing','The RAG reads any database via dbaccess.','bob')",
	} {
		if _, err := raw.Exec(s); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	raw.Close()

	spec := SourceSpec{
		Name: "articles", Type: "mysql", KB: "kb",
		Opts: map[string]any{
			"dsn":          "mysql://root:root@localhost:3307/ragtest",
			"query":        "SELECT id, title, body, author FROM articles ORDER BY id",
			"id_column":    "id",
			"text_columns": []string{"title", "body"},
		},
	}
	c := dbaccessConnector{kind: "mysql"}
	var docs []Document
	if err := c.Walk(ctx, spec, func(d Document) error { docs = append(docs, d); return nil }); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("docs = %d, want 2", len(docs))
	}
	if docs[0].ID != "1" {
		t.Fatalf("doc id = %q, want 1", docs[0].ID)
	}
	if docs[0].Text == "" || docs[0].Meta["author"] != "alice" {
		t.Fatalf("doc text/meta wrong: text=%q meta=%v", docs[0].Text, docs[0].Meta)
	}
	t.Logf("mysql walk: %d docs, doc1.text=%q meta=%v", len(docs), docs[0].Text, docs[0].Meta)
}
