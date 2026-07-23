package dataset

import "github.com/nc1107/lidarr-metadata-provider/internal/skyhook"

// Normalize is the shared name-folding used for both indexing and matching.
// It lives in skyhook so the server can score search results with the exact
// rules the dataset indexed under.
func Normalize(s string) string { return skyhook.Normalize(s) }
