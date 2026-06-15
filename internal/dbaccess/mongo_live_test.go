package dbaccess

import (
	"context"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func mongoDSN() string {
	if v := os.Getenv("DBACCESS_MONGO_DSN"); v != "" {
		return v
	}
	return "mongodb://localhost:27018/ragtest"
}

// TestMongoAccess_Live proves the MongoDB engine on the SAME DB interface :
// JSON find + aggregate queries, read-only ($out blocked), PII masking, row
// caps, and schemaless introspection (collections + sampled fields + decor).
func TestMongoAccess_Live(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cli, err := mongo.Connect(options.Client().ApplyURI(mongoDSN()))
	if err != nil {
		t.Skipf("no mongo: %v", err)
	}
	if cli.Ping(ctx, nil) != nil {
		t.Skipf("no mongo at %s", mongoDSN())
	}
	coll := cli.Database("ragtest").Collection("customers")
	_ = coll.Drop(ctx)
	_, err = coll.InsertMany(ctx, []any{
		bson.M{"_id": 1, "name": "Alice", "ssn": "111-11", "status": "active", "spend": 100},
		bson.M{"_id": 2, "name": "Bob", "ssn": "222-22", "status": "closed", "spend": 50},
		bson.M{"_id": 3, "name": "Carol", "ssn": "333-33", "status": "active", "spend": 70},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = cli.Disconnect(ctx)

	mgr := NewManager(0, 0)
	defer mgr.Shutdown()
	cfg := ConnConfig{
		Name: "mongo", Kind: "mongodb", DSN: mongoDSN(), SampleValues: true,
		Security: SecurityPolicy{Mode: "read_only", MaxRows: 100, SensitiveColumns: []string{"ssn"}},
		Decor:    SchemaDecor{Tables: map[string]TableDecor{"customers": {Description: "Customer documents", Sensitive: []string{"ssn"}}}},
	}
	db, err := mgr.Named(ctx, "app1", cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	// find with filter + PII masking.
	res, err := db.Query(ctx, `{"collection":"customers","filter":{"status":"active"},"sort":{"_id":1}}`)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if res.RowCount != 2 {
		t.Fatalf("find rows = %d, want 2 (active)", res.RowCount)
	}
	if res.Rows[0]["ssn"] != "***" {
		t.Fatalf("ssn not masked: %v", res.Rows[0]["ssn"])
	}
	if res.Rows[0]["name"] != "Alice" {
		t.Fatalf("name = %v, want Alice", res.Rows[0]["name"])
	}

	// aggregate (group/sum).
	res, err = db.Query(ctx, `{"collection":"customers","pipeline":[{"$group":{"_id":"$status","total":{"$sum":"$spend"}}},{"$sort":{"_id":1}}]}`)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if res.RowCount != 2 {
		t.Fatalf("aggregate groups = %d, want 2", res.RowCount)
	}

	// read_only blocks a writing aggregation ($out).
	if _, err := db.Query(ctx, `{"collection":"customers","pipeline":[{"$match":{}},{"$out":"copy"}]}`); err == nil {
		t.Fatal("read_only let $out (write) through")
	}

	// schemaless introspection + decoration.
	cat, err := db.Schema(ctx)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	var cust *TableInfo
	for i := range cat.Tables {
		if cat.Tables[i].Name == "customers" {
			cust = &cat.Tables[i]
		}
	}
	if cust == nil || cust.Description != "Customer documents" {
		t.Fatalf("collection not introspected/decorated: %+v", cat.Tables)
	}
	if len(cust.Columns) == 0 {
		t.Fatalf("no fields inferred from sampled documents")
	}
	var ssn *ColumnInfo
	for i := range cust.Columns {
		if cust.Columns[i].Name == "ssn" {
			ssn = &cust.Columns[i]
		}
	}
	if ssn == nil || !ssn.Sensitive {
		t.Fatalf("ssn field not flagged sensitive: %+v", cust.Columns)
	}
	t.Logf("mongo: %d fields inferred, columns=%v", len(cust.Columns), columnNames(cust))
	_ = time.Second
}

func columnNames(t *TableInfo) []string {
	out := make([]string, 0, len(t.Columns))
	for _, c := range t.Columns {
		out = append(out, c.Name)
	}
	return out
}
