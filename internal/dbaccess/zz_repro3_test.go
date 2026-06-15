package dbaccess

import (
	"fmt"
	"testing"
)

// FAIL-OPEN via "guard sees a longer string than the engine does".
// Standard-conforming strings (PG default): backslash is literal; '' closes+reopens.
// If guard's stripSQL keeps consuming past where the engine ends the string,
// a real keyword could be hidden inside guard's phantom literal -> accepted.
func TestRepro_HideKeyword(t *testing.T) {
	ro := SecurityPolicy{Mode: "read_only"}
	rw := SecurityPolicy{Mode: "read_write", DeniedStatements: []string{"drop", "delete", "update"}}

	cases := []string{
		// In std-conforming strings, '' is escape. Guard handles '' (line 92). So aligned.
		`SELECT 'a''b'`,
		// The classic: guard treats \' as close? No - guard ignores backslash, '' is the escape.
		// Engine (std-conforming) also: \ literal, '' escape. So they AGREE here.
		// The DISAGREEMENT is only for E'' strings where engine treats \' as escape but guard doesn't.
		// E-string: real value of E'\'' is a single quote char. Guard: sees \' -> ' is close? 
		`SELECT E'\''  , delete`,   // engine: E'\'' = "'", then ", delete" is part of stmt -> denied anyway
		// What if backslash COUNT is even, making guard close early and ENGINE keep open?
		// Guard never treats backslash specially, so guard closes at first lone '.
		// Engine with E-string: \' continues. So ENGINE string is LONGER than guard's.
		// => engine hides MORE than guard. Guard sees keywords engine treats as literal => OVER-reject. Safe.
		`SELECT E'\' , delete from t , \'' `,
	}
	for _, q := range cases {
		fmt.Printf("\n--- q=%q\n", q)
		fmt.Printf("    stripped=%q\n", stripSQL(q))
		fmt.Printf("    guard(read_only)=%v\n", guardStatement(q, ro))
		fmt.Printf("    guard(read_write+denied)=%v\n", guardStatement(q, rw))
	}
}
