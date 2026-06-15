package indexer

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

func init() { Register(&dbConnector{}) }

// dbConnector indexes rows from a SQL query (Postgres). Walk = run the query,
// each row → one document (text from the configured text columns, the rest
// as filterable metadata), keyed by the id column. Watch = real-time CDC :
// stream the table's WAL via logical replication (pglogrepl/pgoutput) and
// upsert/delete documents as rows change, resuming from a durable LSN.
type dbConnector struct{}

func (*dbConnector) Type() string      { return "database" }
func (*dbConnector) Capabilities() Caps { return Caps{Walk: true, Watch: true} }

type dbOpts struct {
	DSN         string
	Query       string
	IDColumn    string
	TextColumns map[string]bool
	Table       string
	Slot        string
	Publication string
}

func parseDBOpts(opts map[string]any) dbOpts {
	o := dbOpts{TextColumns: map[string]bool{}}
	o.DSN = optString(opts, "dsn")
	if o.DSN == "" {
		o.DSN = optString(opts, "url")
	}
	o.Query = optString(opts, "query")
	o.IDColumn = optString(opts, "id_column")
	for _, c := range optStrings(opts, "text_columns") {
		o.TextColumns[c] = true
	}
	o.Table = optString(opts, "cdc_table")
	o.Slot = optString(opts, "cdc_slot")
	o.Publication = optString(opts, "cdc_publication")
	return o
}

func (o dbOpts) slotPub(spec SourceSpec) (slot, pub string) {
	slot = o.Slot
	if slot == "" {
		slot = "rag_" + sanitizeIdent(spec.KB+"_"+spec.Name)
	}
	pub = o.Publication
	if pub == "" {
		pub = slot + "_pub"
	}
	return slot, pub
}

// Cleanup drops the CDC replication slot + publication so a permanently
// removed source stops retaining WAL on the upstream database. Best-effort
// with a short retry to absorb the race against the just-cancelled Watch
// releasing the slot.
func (*dbConnector) Cleanup(ctx context.Context, spec SourceSpec) error {
	o := parseDBOpts(spec.Opts)
	if o.DSN == "" || o.Table == "" {
		return nil
	}
	slot, pub := o.slotPub(spec)

	conn, err := pgconn.Connect(ctx, replicationDSN(o.DSN))
	if err == nil {
		for i := 0; i < 3; i++ {
			if derr := pglogrepl.DropReplicationSlot(ctx, conn, slot, pglogrepl.DropReplicationSlotOptions{Wait: true}); derr == nil {
				break
			} else if !strings.Contains(strings.ToLower(derr.Error()), "active") {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		conn.Close(ctx)
	}
	if pool, perr := pgxpool.New(ctx, o.DSN); perr == nil {
		_, _ = pool.Exec(ctx, "DROP PUBLICATION IF EXISTS "+pgx.Identifier{pub}.Sanitize())
		pool.Close()
	}
	return nil
}

func (o dbOpts) docFromRow(rec map[string]any) (Document, bool) {
	id := dbStr(rec[o.IDColumn])
	if id == "" {
		return Document{}, false
	}
	cols := make([]string, 0, len(rec))
	for c := range rec {
		cols = append(cols, c)
	}
	sort.Strings(cols) // deterministic text order → stable content hash
	var parts []string
	meta := map[string]any{}
	for _, c := range cols {
		if c == o.IDColumn {
			continue
		}
		if len(o.TextColumns) == 0 || o.TextColumns[c] {
			if s := dbStr(rec[c]); s != "" {
				parts = append(parts, s)
			}
		} else {
			meta[c] = dbStr(rec[c])
		}
	}
	return Document{ID: id, Text: strings.Join(parts, "\n"), Meta: meta}, true
}

func (*dbConnector) Walk(ctx context.Context, spec SourceSpec, emit func(Document) error) error {
	o := parseDBOpts(spec.Opts)
	if o.DSN == "" || o.Query == "" || o.IDColumn == "" {
		return fmt.Errorf("indexer/database: dsn, query and id_column are required")
	}
	pool, err := pgxpool.New(ctx, o.DSN)
	if err != nil {
		return fmt.Errorf("indexer/database: connect: %w", err)
	}
	defer pool.Close()

	rows, err := pool.Query(ctx, o.Query)
	if err != nil {
		return fmt.Errorf("indexer/database: query: %w", err)
	}
	defer rows.Close()

	cols := make([]string, 0)
	for _, fd := range rows.FieldDescriptions() {
		cols = append(cols, string(fd.Name))
	}
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return fmt.Errorf("indexer/database: scan: %w", err)
		}
		rec := map[string]any{}
		for i, c := range cols {
			if i < len(vals) {
				rec[c] = vals[i]
			}
		}
		doc, ok := o.docFromRow(rec)
		if !ok {
			continue
		}
		if err := emit(doc); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Watch streams a table's changes in real time via Postgres logical
// replication (pgoutput) : each insert/update upserts a document, each delete
// removes one, resuming from a durable LSN saved in the cursor. The slot +
// publication are auto-created. Requires Postgres wal_level=logical.
func (*dbConnector) Watch(ctx context.Context, spec SourceSpec, sink Sink, cur Cursor) error {
	o := parseDBOpts(spec.Opts)
	if o.DSN == "" || o.Table == "" || o.IDColumn == "" {
		return fmt.Errorf("indexer/database cdc: dsn, cdc_table and id_column required")
	}
	slot, pub := o.slotPub(spec)

	if pool, err := pgxpool.New(ctx, o.DSN); err == nil {
		_, _ = pool.Exec(ctx, "CREATE PUBLICATION "+pgx.Identifier{pub}.Sanitize()+" FOR TABLE "+pgx.Identifier{o.Table}.Sanitize())
		pool.Close()
	}

	conn, err := pgconn.Connect(ctx, replicationDSN(o.DSN))
	if err != nil {
		return fmt.Errorf("indexer/database cdc: connect: %w", err)
	}
	defer conn.Close(ctx)

	if _, err := pglogrepl.CreateReplicationSlot(ctx, conn, slot, "pgoutput", pglogrepl.CreateReplicationSlotOptions{}); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "already exists") {
			return fmt.Errorf("indexer/database cdc: create slot: %w", err)
		}
	}

	lsnKey := stateKey(spec) + ":lsn"
	startLSN := pglogrepl.LSN(0)
	if b, _ := cur.Load(lsnKey); len(b) > 0 {
		if l, err := pglogrepl.ParseLSN(string(b)); err == nil {
			startLSN = l
		}
	}
	if startLSN == 0 {
		if sys, err := pglogrepl.IdentifySystem(ctx, conn); err == nil {
			startLSN = sys.XLogPos
		}
	}

	args := []string{"proto_version '1'", "publication_names '" + pub + "'"}
	if err := pglogrepl.StartReplication(ctx, conn, slot, startLSN, pglogrepl.StartReplicationOptions{PluginArgs: args}); err != nil {
		return fmt.Errorf("indexer/database cdc: start replication: %w", err)
	}

	relations := map[uint32]*pglogrepl.RelationMessage{}
	clientPos := startLSN
	nextStandby := time.Now().Add(10 * time.Second)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(nextStandby) {
			_ = pglogrepl.SendStandbyStatusUpdate(ctx, conn, pglogrepl.StandbyStatusUpdate{WALWritePosition: clientPos})
			_ = cur.Save(lsnKey, []byte(clientPos.String()))
			nextStandby = time.Now().Add(10 * time.Second)
		}
		rctx, cancel := context.WithDeadline(ctx, nextStandby)
		raw, err := conn.ReceiveMessage(rctx)
		cancel()
		if err != nil {
			if pgconn.Timeout(err) {
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("indexer/database cdc: receive: %w", err)
		}
		cd, ok := raw.(*pgproto3.CopyData)
		if !ok || len(cd.Data) == 0 {
			continue
		}
		switch cd.Data[0] {
		case pglogrepl.PrimaryKeepaliveMessageByteID:
			if pkm, err := pglogrepl.ParsePrimaryKeepaliveMessage(cd.Data[1:]); err == nil && pkm.ReplyRequested {
				_ = pglogrepl.SendStandbyStatusUpdate(ctx, conn, pglogrepl.StandbyStatusUpdate{WALWritePosition: clientPos})
			}
		case pglogrepl.XLogDataByteID:
			xld, err := pglogrepl.ParseXLogData(cd.Data[1:])
			if err != nil {
				continue
			}
			o.applyCDC(ctx, xld.WALData, relations, sink, spec.KB)
			clientPos = xld.WALStart + pglogrepl.LSN(len(xld.WALData))
		}
	}
}

func (o dbOpts) applyCDC(ctx context.Context, walData []byte, relations map[uint32]*pglogrepl.RelationMessage, sink Sink, kb string) {
	m, err := pglogrepl.Parse(walData)
	if err != nil {
		return
	}
	switch v := m.(type) {
	case *pglogrepl.RelationMessage:
		relations[v.RelationID] = v
	case *pglogrepl.InsertMessage:
		if rel := relations[v.RelationID]; rel != nil {
			if doc, ok := o.docFromRow(tupleRow(rel, v.Tuple)); ok {
				_ = sink.Upsert(ctx, kb, []Document{doc})
			}
		}
	case *pglogrepl.UpdateMessage:
		if rel := relations[v.RelationID]; rel != nil {
			if doc, ok := o.docFromRow(tupleRow(rel, v.NewTuple)); ok {
				_ = sink.Upsert(ctx, kb, []Document{doc})
			}
		}
	case *pglogrepl.DeleteMessage:
		if rel := relations[v.RelationID]; rel != nil {
			if id := dbStr(tupleRow(rel, v.OldTuple)[o.IDColumn]); id != "" {
				_ = sink.Delete(ctx, kb, id)
			}
		}
	}
}

func tupleRow(rel *pglogrepl.RelationMessage, t *pglogrepl.TupleData) map[string]any {
	row := map[string]any{}
	if t == nil {
		return row
	}
	for i, col := range t.Columns {
		if i >= len(rel.Columns) {
			break
		}
		name := rel.Columns[i].Name
		if col.DataType == pglogrepl.TupleDataTypeText {
			row[name] = string(col.Data)
		} else {
			row[name] = ""
		}
	}
	return row
}

func replicationDSN(dsn string) string {
	if strings.Contains(dsn, "replication=") {
		return dsn
	}
	if strings.Contains(dsn, "?") {
		return dsn + "&replication=database"
	}
	return dsn + "?replication=database"
}

func sanitizeIdent(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func dbStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}
