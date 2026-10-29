package relocate

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
)

// When each site arrived on this node from a move or an import: for a day
// after, a site that goes down is repaired by the house's AI even with the
// automatic repairs off, because what fails then is most likely what the
// move did not carry (a link, a dependency, a path of the other machine).

// ArrivalWindow is how long after arriving a site is watched that way.
const ArrivalWindow = 24 * time.Hour

var arrivedMu sync.Mutex

func arrivedPath(root string) string { return filepath.Join(root, "data", "relocate", "arrived.json") }

// MarkArrived writes that domain arrived now, forgetting what is older
// than the window.
func MarkArrived(root, domain string, now time.Time) error {
	arrivedMu.Lock()
	defer arrivedMu.Unlock()
	m := map[string]time.Time{}
	_ = fsx.ReadJSON(arrivedPath(root), &m)
	for d, at := range m {
		if now.Sub(at) > ArrivalWindow {
			delete(m, d)
		}
	}
	m[domain] = now.UTC()
	if err := os.MkdirAll(filepath.Dir(arrivedPath(root)), 0o755); err != nil {
		return err
	}
	return fsx.WriteJSON(arrivedPath(root), m, 0o644)
}

// ArrivedAt is when domain arrived, zero when it did not lately.
func ArrivedAt(root, domain string) time.Time {
	arrivedMu.Lock()
	defer arrivedMu.Unlock()
	m := map[string]time.Time{}
	_ = fsx.ReadJSON(arrivedPath(root), &m)
	return m[domain]
}
