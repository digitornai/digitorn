package dbaccess

import (
	"fmt"
	"testing"
)

func TestRepro_DollarQuote(t *testing.T) {
	ro := SecurityPolicy{Mode: "read_only"}
	// Valid pg: dollar-quoted body that contains the word "drop" but is a literal.
	q := `SELECT $$drop table here$$ AS msg`
	stripped := stripSQL(q)
	fmt.Printf("[dollar] orig=%q\n", q)
	fmt.Printf("[dollar] stripped=%q\n", stripped)
	fmt.Printf("[dollar] split=%#v\n", splitStatements(stripped))
	err := guardStatement(q, ro)
	fmt.Printf("[dollar] guardStatement err=%v\n", err)

	// A tagged dollar quote.
	q2 := `SELECT $body$ delete from x $body$`
	fmt.Printf("[dollar2] stripped=%q\n", stripSQL(q2))
	fmt.Printf("[dollar2] guardStatement err=%v\n", guardStatement(q2, ro))
}

func TestRepro_EStringDesync(t *testing.T) {
	ro := SecurityPolicy{Mode: "read_only"}
	rw := SecurityPolicy{Mode: "read_write", DeniedStatements: []string{"drop"}}

	// Postgres E-string: backslash escapes the quote. The real literal is: foo'; DROP TABLE x; --
	q := `SELECT E'foo\'; DROP TABLE x; --' AS c`
	stripped := stripSQL(q)
	fmt.Printf("[estr] orig=%q\n", q)
	fmt.Printf("[estr] stripped=%q\n", stripped)
	fmt.Printf("[estr] split=%#v\n", splitStatements(stripped))
	fmt.Printf("[estr] guard(read_only) err=%v\n", guardStatement(q, ro))
	fmt.Printf("[estr] guard(read_write+denied drop) err=%v\n", guardStatement(q, rw))
}

func TestRepro_BackslashHidesDenied(t *testing.T) {
	// Can a backslash escape HIDE a denied keyword from the scan (bypass)?
	// In standard-conforming strings backslash is literal; the closing is the doubled quote.
	rw := SecurityPolicy{Mode: "read_write", DeniedStatements: []string{"drop"}}
	// Attacker wants DROP to execute but be masked. Try smuggling via backslash.
	q := `SELECT 1 WHERE x = E'\' ; drop table victim ; --'`
	fmt.Printf("[hide] orig=%q\n", q)
	fmt.Printf("[hide] stripped=%q\n", stripSQL(q))
	fmt.Printf("[hide] split=%#v\n", splitStatements(stripSQL(q)))
	fmt.Printf("[hide] guard(read_write+denied) err=%v\n", guardStatement(q, rw))
}
