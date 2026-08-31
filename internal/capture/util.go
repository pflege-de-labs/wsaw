package capture

import (
	"encoding/base64"
	"sort"

	"github.com/martint17r/wsaw/internal/model"
)

func base64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func sortStrings(s []string) {
	sort.Strings(s)
}

// sortCookies orders cookies deterministically so that two scans of an
// unchanged site produce an identical cookie list.
func sortCookies(c []model.Cookie) {
	sort.SliceStable(c, func(i, j int) bool {
		if c[i].Domain != c[j].Domain {
			return c[i].Domain < c[j].Domain
		}

		if c[i].Path != c[j].Path {
			return c[i].Path < c[j].Path
		}

		return c[i].Name < c[j].Name
	})
}
