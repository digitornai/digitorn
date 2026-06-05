package index

import "strings"

// SynonymTable maps a word to its synonyms (both directions). The
// reference daemon's scoring.py uses an FR/EN bilingual synonym map
// to make non-English queries find the right tool ("supprimer"
// finds the "delete" action).
//
// The table is immutable after construction. Two access patterns :
//
//   - Expand(token) returns the token plus all its known synonyms
//     (the bag the search uses to widen a query).
//   - Includes(a, b) is a symmetric "are these two tokens synonyms ?"
//     check (used for tag-boost where order doesn't matter).
//
// Data-driven : the default table is hard-coded for V1 to avoid
// a YAML loading dependency at boot. CB-1 follow-up will move it to
// a configurable YAML file under config/synonyms.yaml.
type SynonymTable struct {
	// canonical maps every known word to a canonical bucket id.
	// Words in the same bucket are mutually synonymous.
	canonical map[string]int

	// buckets[id] is the slice of every word in that bucket.
	buckets [][]string
}

// NewSynonymTable builds a SynonymTable from a set of groups. Each
// group is a slice of mutually-synonymous words (any language). The
// constructor lower-cases every entry and de-duplicates.
//
// Example :
//
//	NewSynonymTable([][]string{
//	    {"delete", "remove", "destroy", "erase", "supprimer", "effacer"},
//	    {"read", "lire", "fetch", "load"},
//	})
func NewSynonymTable(groups [][]string) *SynonymTable {
	t := &SynonymTable{canonical: make(map[string]int)}
	for _, g := range groups {
		bucket := make([]string, 0, len(g))
		bucketID := len(t.buckets)
		for _, raw := range g {
			w := strings.ToLower(strings.TrimSpace(raw))
			if w == "" {
				continue
			}
			if _, exists := t.canonical[w]; exists {
				continue // already in another bucket — first wins
			}
			t.canonical[w] = bucketID
			bucket = append(bucket, w)
		}
		if len(bucket) > 0 {
			t.buckets = append(t.buckets, bucket)
		}
	}
	return t
}

// Expand returns the token plus every word in its synonym bucket.
// The token itself is included whether or not it has synonyms.
// Returns a NEW slice — safe to mutate.
//
// Lower-cased internally so the table can be built once with any
// casing.
func (t *SynonymTable) Expand(token string) []string {
	if t == nil {
		return []string{token}
	}
	lower := strings.ToLower(token)
	id, ok := t.canonical[lower]
	if !ok {
		return []string{lower}
	}
	bucket := t.buckets[id]
	out := make([]string, len(bucket))
	copy(out, bucket)
	return out
}

// Includes reports whether `a` and `b` belong to the same synonym
// bucket. Symmetric : Includes(x, y) == Includes(y, x). Lower-cased
// internally. Two words are considered synonyms if they're in the
// same bucket OR if they're equal (case-insensitive).
func (t *SynonymTable) Includes(a, b string) bool {
	la := strings.ToLower(a)
	lb := strings.ToLower(b)
	if la == lb {
		return true
	}
	if t == nil {
		return false
	}
	ida, oka := t.canonical[la]
	idb, okb := t.canonical[lb]
	return oka && okb && ida == idb
}

// Size returns the number of distinct synonym buckets in the table.
// Useful for tests and observability.
func (t *SynonymTable) Size() int {
	if t == nil {
		return 0
	}
	return len(t.buckets)
}

// DefaultSynonyms is the bilingual FR/EN bag the keyword search
// uses out of the box. The reference daemon (scoring.py) uses a
// similar set — port + extend as new tools surface non-obvious
// vocabulary.
//
// Adding a synonym is conservative : when in doubt, leave it out.
// False matches degrade search precision more than missing matches
// hurt recall.
func DefaultSynonyms() *SynonymTable {
	return NewSynonymTable([][]string{
		// File operations
		{"read", "lire", "fetch", "load", "get", "show", "afficher"},
		{"write", "ecrire", "écrire", "save", "store", "enregistrer"},
		{"edit", "modify", "modifier", "update", "change", "changer"},
		{"delete", "remove", "destroy", "erase", "supprimer", "effacer"},
		{"search", "find", "chercher", "trouver", "lookup", "query"},
		{"list", "lister", "browse", "show", "enumerate"},
		{"copy", "copier", "duplicate", "dupliquer", "clone"},
		{"move", "deplacer", "déplacer", "rename", "renommer"},
		// Network
		{"http", "https", "web", "url", "request"},
		{"post", "send", "envoyer", "submit"},
		{"download", "telecharger", "télécharger", "fetch"},
		{"upload", "televerser", "téléverser", "send"},
		// Shell / process
		{"bash", "shell", "command", "commande", "exec", "execute", "run"},
		{"kill", "terminate", "stop", "arreter", "arrêter"},
		// Database
		{"query", "sql", "request", "requete", "requête"},
		{"insert", "add", "ajouter"}, // NOTE : "create" intentionally NOT here ;
		// it belongs to the Common-verbs bucket so
		// "create" ↔ "creer/créer" works.
		// Memory / state
		{"remember", "memoriser", "mémoriser", "save", "store"},
		{"forget", "oublier", "clear", "delete"},
		{"recall", "rappeler", "remember", "fetch"},
		// Common verbs
		{"create", "new", "make", "creer", "créer"},
		{"open", "ouvrir", "launch"},
		{"close", "fermer", "stop"},
		{"start", "demarrer", "démarrer", "launch", "lancer"},
	})
}
