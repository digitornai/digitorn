package validate

import "github.com/digitornai/digitorn/internal/compiler/position"

// posUnknown is the position used when a diagnostic cannot pinpoint a source
// location — the file is still attached at the bag level when present.
var posUnknown = position.Pos{}
