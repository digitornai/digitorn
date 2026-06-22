---
name: "no-touch-edit-preview"
description: "Never modify the edit error message / preview in the filesystem module — many clients depend on it"
metadata:
  type: feedback
---

Ne jamais modifier le message d'erreur du filesystem edit (le hint avec "IMMEDIATE FIX: copy closest_matches[0].preview VERBATIM...") ni le format du preview retourné dans closest_matches.

**Why:** Beaucoup de clients dépendent de ce format exact. Modifier le message ou le preview casse les clients.

**How to apply:** Quand on travaille sur le module filesystem, ne jamais toucher au hint/error message du edit ni au champ preview de suggestion.
