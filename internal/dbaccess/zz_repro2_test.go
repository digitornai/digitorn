package dbaccess

import (
	"fmt"
	"testing"
)

// Hunt for a FAIL-OPEN: a query that guardStatement ACCEPTS but actually
// executes a denied/write statement on a real engine. That would be a true
// security bypass (vs. mere over-rejection).
func TestRepro_FailOpenHunt(t *testing.T) {
	rw := SecurityPolicy{Mode: "read_write", DeniedStatements: []string{"drop", "delete"}}
	ro := SecurityPolicy{Mode: "read_only"}

	cases := []string{
		// backslash makes the OPENING quote look unterminated -> swallow the drop into a "string"
		`SELECT 'a\' , drop`,                    // not real sql, probe
		`SELECT '\'; drop table x`,              // does backslash swallow the rest into a literal?
		`SELECT E'\' ; SELECT 1`,               // even number of backslashes -> string closes
		`SELECT 'foo\' || drop_me`,             // doubled backslash
		// Try to get a denied verb absorbed into a phantom string (hidden from scan)
		`SELECT 1 /* ' */ ; drop table y`,       // comment+quote interplay
		`SELECT '' drop ''`,                     // doubled quotes
		`SELECT 1; DROP TABLE z`,                // baseline: clearly 2 stmts, must reject
	}
	for _, q := range cases {
		fmt.Printf("\n--- q=%q\n", q)
		fmt.Printf("    stripped=%q\n", stripSQL(q))
		fmt.Printf("    split=%#v\n", splitStatements(stripSQL(q)))
		fmt.Printf("    guard(read_write+denied)=%v\n", guardStatement(q, rw))
		fmt.Printf("    guard(read_only)=%v\n", guardStatement(q, ro))
	}
}
