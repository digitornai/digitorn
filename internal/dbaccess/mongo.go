package dbaccess

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func init() { Register("mongodb", openMongo) }

// mongoDB fronts MongoDB on the uniform DB interface. The query string is a
// JSON spec — {collection, filter|pipeline, projection, sort, limit} — so the
// agent expresses a Mongo query natively and gets back documents as Rows. A
// schemaless collection is "introspected" by sampling documents for their
// fields.
type mongoDB struct {
	client *mongo.Client
	db     *mongo.Database
	pol    SecurityPolicy
	decor  SchemaDecor
	sample bool
}

func (m *mongoDB) Kind() string { return "mongodb" }

func (m *mongoDB) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return m.client.Disconnect(ctx)
}

func openMongo(ctx context.Context, cfg ConnConfig) (DB, error) {
	if err := guardEgress(cfg.Kind, cfg.DSN, cfg.Security); err != nil {
		return nil, err
	}
	dbName := mongoDBName(cfg.DSN)
	if dbName == "" {
		return nil, fmt.Errorf("dbaccess/mongodb: database name required in DSN (mongodb://host/dbname)")
	}
	cli, err := mongo.Connect(options.Client().ApplyURI(cfg.DSN))
	if err != nil {
		return nil, fmt.Errorf("dbaccess/mongodb: connect: %w", err)
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := cli.Ping(pctx, nil); err != nil {
		_ = cli.Disconnect(context.Background())
		return nil, fmt.Errorf("dbaccess/mongodb: ping: %w", err)
	}
	pol := cfg.Security
	for _, td := range cfg.Decor.Tables {
		pol.SensitiveColumns = append(pol.SensitiveColumns, td.Sensitive...)
	}
	return &mongoDB{client: cli, db: cli.Database(dbName), pol: pol, decor: cfg.Decor, sample: cfg.SampleValues}, nil
}

func mongoDBName(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Path, "/")
}

type mongoQuery struct {
	Collection string           `json:"collection"`
	Filter     map[string]any   `json:"filter"`
	Projection map[string]any   `json:"projection"`
	Sort       map[string]any   `json:"sort"`
	Limit      int64            `json:"limit"`
	Pipeline   []map[string]any `json:"pipeline"`
}

func (m *mongoDB) Query(ctx context.Context, q string, _ ...any) (*Result, error) {
	var spec mongoQuery
	if err := json.Unmarshal([]byte(q), &spec); err != nil {
		return nil, fmt.Errorf("dbaccess/mongodb: query must be JSON {collection, filter|pipeline, …}: %w", err)
	}
	if strings.TrimSpace(spec.Collection) == "" {
		return nil, fmt.Errorf("dbaccess/mongodb: collection is required")
	}
	ctx, cancel := context.WithTimeout(ctx, m.pol.timeout())
	defer cancel()
	coll := m.db.Collection(spec.Collection)
	max := int64(m.pol.maxRows())

	var docs []bson.M
	if len(spec.Pipeline) > 0 {
		if m.pol.readOnly() {
			for _, st := range spec.Pipeline {
				if _, ok := st["$out"]; ok {
					return nil, fmt.Errorf("read_only: aggregation $out (write) is not allowed")
				}
				if _, ok := st["$merge"]; ok {
					return nil, fmt.Errorf("read_only: aggregation $merge (write) is not allowed")
				}
			}
		}
		cur, err := coll.Aggregate(ctx, spec.Pipeline)
		if err != nil {
			return nil, err
		}
		if err := cur.All(ctx, &docs); err != nil {
			return nil, err
		}
	} else {
		opt := options.Find()
		lim := max + 1 // one extra to detect truncation
		if spec.Limit > 0 && spec.Limit < lim {
			lim = spec.Limit
		}
		opt.SetLimit(lim)
		if spec.Projection != nil {
			opt.SetProjection(spec.Projection)
		}
		if spec.Sort != nil {
			opt.SetSort(spec.Sort)
		}
		var filter any = bson.M{}
		if spec.Filter != nil {
			filter = spec.Filter
		}
		cur, err := coll.Find(ctx, filter, opt)
		if err != nil {
			return nil, err
		}
		if err := cur.All(ctx, &docs); err != nil {
			return nil, err
		}
	}
	return mongoResult(docs, m.pol), nil
}

func mongoResult(docs []bson.M, pol SecurityPolicy) *Result {
	max := pol.maxRows()
	sensitive := map[string]bool{}
	for _, c := range pol.SensitiveColumns {
		sensitive[strings.ToLower(c)] = true
	}
	res := &Result{Rows: []Row{}}
	colSet := map[string]bool{}
	for _, d := range docs {
		if len(res.Rows) >= max {
			res.Truncated = true
			break
		}
		row := bsonToRow(d)
		for k := range row {
			if sensitive[strings.ToLower(k)] {
				row[k] = "***"
			}
			colSet[k] = true
		}
		res.Rows = append(res.Rows, row)
	}
	for c := range colSet {
		res.Columns = append(res.Columns, c)
	}
	sort.Strings(res.Columns)
	res.RowCount = len(res.Rows)
	return res
}

func bsonToRow(d bson.M) Row {
	b, _ := json.Marshal(d)
	var r Row
	_ = json.Unmarshal(b, &r)
	if r == nil {
		r = Row{}
	}
	return r
}

func (m *mongoDB) Schema(ctx context.Context) (*Catalog, error) {
	ctx, cancel := context.WithTimeout(ctx, m.pol.timeout())
	defer cancel()
	names, err := m.db.ListCollectionNames(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	cat := &Catalog{}
	for _, name := range names {
		t := &TableInfo{Name: name}
		if m.sample {
			t.Columns = m.inferFields(ctx, name)
		}
		if d, ok := m.decor.Tables[name]; ok {
			applyDecor(t, d)
		}
		cat.Tables = append(cat.Tables, *t)
	}
	return cat, nil
}

func (m *mongoDB) inferFields(ctx context.Context, coll string) []ColumnInfo {
	cur, err := m.db.Collection(coll).Find(ctx, bson.M{}, options.Find().SetLimit(20))
	if err != nil {
		return nil
	}
	var docs []bson.M
	if cur.All(ctx, &docs) != nil {
		return nil
	}
	types := map[string]string{}
	for _, d := range docs {
		for k, v := range bsonToRow(d) {
			if _, ok := types[k]; !ok {
				types[k] = goType(v)
			}
		}
	}
	keys := make([]string, 0, len(types))
	for k := range types {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	cols := make([]ColumnInfo, 0, len(keys))
	for _, k := range keys {
		cols = append(cols, ColumnInfo{Name: k, Type: types[k]})
	}
	return cols
}

func goType(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case float64, int, int64:
		return "number"
	case bool:
		return "bool"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case nil:
		return "null"
	}
	return fmt.Sprintf("%T", v)
}
