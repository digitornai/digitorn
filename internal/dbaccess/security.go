package dbaccess

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// readLeading is the set of statement-leading keywords allowed in read_only
// mode. A data-modifying CTE (WITH … AS (DELETE …)) still trips the write-verb
// scan below, so WITH is safe to allow here.
var readLeading = map[string]bool{
	"select": true, "with": true, "show": true, "explain": true,
	"describe": true, "desc": true, "table": true, "values": true,
}

// writeVerbs are rejected outright in read_only mode (in addition to any
// operator-configured DeniedStatements, which apply in every mode).
var writeVerbs = []string{
	"insert", "update", "delete", "drop", "truncate", "alter", "create",
	"grant", "revoke", "replace", "merge", "call", "do", "copy", "vacuum",
	"reindex", "lock", "attach", "comment", "set", "load", "handler",
	"import", "rename", "execute", "prepare",
}

// guardStatement is the in-process first line of defense (the DB read-only
// transaction is the backstop). It rejects multi-statement injection, enforces
// the read_only leading-verb allowlist, and bans every write/operator-denied
// keyword — scanning a stripped query so string literals can't smuggle one in.
func guardStatement(query string, pol SecurityPolicy) error {
	stripped := stripSQL(query)
	stmts := splitStatements(stripped)
	if len(stmts) == 0 {
		return fmt.Errorf("empty query")
	}
	if len(stmts) > 1 {
		return fmt.Errorf("multiple statements are not allowed")
	}
	tokens := tokenize(strings.ToLower(stmts[0]))
	if len(tokens) == 0 {
		return fmt.Errorf("empty query")
	}

	denied := map[string]bool{}
	for _, d := range pol.DeniedStatements {
		denied[strings.ToLower(strings.TrimSpace(d))] = true
	}
	if pol.readOnly() {
		for _, v := range writeVerbs {
			denied[v] = true
		}
		if !readLeading[tokens[0]] {
			return fmt.Errorf("read_only: statement must start with SELECT/WITH/SHOW/EXPLAIN (got %q)", tokens[0])
		}
	}
	for _, t := range tokens {
		if denied[t] {
			return fmt.Errorf("statement contains a forbidden keyword: %q", t)
		}
	}
	return nil
}

// stripSQL blanks out comments and string/identifier literals so a keyword
// scan can't be fooled by SELECT 'drop table' or "delete" identifiers, while
// preserving statement-separating semicolons.
func stripSQL(q string) string {
	var b strings.Builder
	rs := []rune(q)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '-' && i+1 < len(rs) && rs[i+1] == '-':
			for i < len(rs) && rs[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
		case c == '/' && i+1 < len(rs) && rs[i+1] == '*':
			i += 2
			for i+1 < len(rs) && !(rs[i] == '*' && rs[i+1] == '/') {
				i++
			}
			i++
			b.WriteByte(' ')
		case c == '\'' || c == '"' || c == '`':
			q := c
			i++
			for i < len(rs) {
				if rs[i] == q {
					if i+1 < len(rs) && rs[i+1] == q { // doubled escape
						i += 2
						continue
					}
					break
				}
				i++
			}
			b.WriteByte(' ')
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

func splitStatements(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ";") {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

var tokenRe = regexp.MustCompile(`[a-zA-Z_][a-zA-Z0-9_]*`)

func tokenize(s string) []string { return tokenRe.FindAllString(s, -1) }

// guardEgress blocks an agent-supplied DSN from reaching the host's loopback /
// link-local (cloud-metadata) addresses when egress is "guarded". RFC1918
// private ranges are NOT blocked — databases legitimately live on private
// networks. Only runs when the policy opts in.
func guardEgress(kind, dsn string, pol SecurityPolicy) error {
	if pol.Egress != "guarded" {
		return nil
	}
	host := hostFromDSN(kind, dsn)
	if host == "" {
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil // unresolvable → the driver will fail; not our call to block
	}
	for _, ip := range ips {
		if dbBlockedIP(ip) {
			return fmt.Errorf("egress blocked: %s resolves to forbidden address %s", host, ip)
		}
	}
	return nil
}

func dbBlockedIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

var mysqlTCPRe = regexp.MustCompile(`@tcp\(([^:)]+)`)

func hostFromDSN(kind, dsn string) string {
	if strings.Contains(dsn, "://") {
		if u, err := url.Parse(dsn); err == nil {
			return u.Hostname()
		}
	}
	if normalizeKind(kind) == "mysql" {
		if m := mysqlTCPRe.FindStringSubmatch(dsn); m != nil {
			return m[1]
		}
	}
	return ""
}
