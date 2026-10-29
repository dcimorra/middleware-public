package modules

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/secrets"
)

// StoreFile is the store's name under data/.
const StoreFile = "modules.json"

// storeData is data/modules.json. Secret fields are encrypted values;
// everything else is clear, so `cat data/modules.json` answers "which
// modules are on" without a key.
type storeData struct {
	Enabled map[string]bool              `json:"enabled"`
	Config  map[string]map[string]string `json:"config"`
}

func emptyStore() storeData {
	return storeData{Enabled: map[string]bool{}, Config: map[string]map[string]string{}}
}

// store reads and writes the file, encrypting on the way in and decrypting
// on the way out. It remembers the file's size and mtime so a reload that
// finds nothing changed costs one stat: the supervisor re-reads the store
// on every tick to pick up what the CLI wrote.
type store struct {
	path  string
	box   *secrets.Box
	data  storeData
	size  int64
	mtime time.Time
}

func newStore(root string, box *secrets.Box) *store {
	return &store{path: filepath.Join(root, "data", StoreFile), box: box, data: emptyStore()}
}

// reload re-reads the file when it changed on disk. A missing file is an
// empty store, not an error: a fresh installation has none.
func (s *store) reload() error {
	info, err := os.Stat(s.path)
	if err != nil {
		if fsx.IsNotExist(err) {
			s.data, s.size, s.mtime = emptyStore(), 0, time.Time{}
			return nil
		}
		return err
	}
	if info.Size() == s.size && info.ModTime().Equal(s.mtime) && !s.mtime.IsZero() {
		return nil
	}
	var d storeData
	if err := fsx.ReadJSON(s.path, &d); err != nil {
		return err
	}
	if d.Enabled == nil {
		d.Enabled = map[string]bool{}
	}
	if d.Config == nil {
		d.Config = map[string]map[string]string{}
	}
	s.data, s.size, s.mtime = d, info.Size(), info.ModTime()
	return nil
}

// save writes the file atomically, owner-only: the values are encrypted
// but the enabled map and the non-secret fields (account ids, repository
// names) are still nobody else's business.
func (s *store) save() error {
	if err := fsx.WriteJSON(s.path, s.data, 0o600); err != nil {
		return err
	}
	if info, err := os.Stat(s.path); err == nil {
		s.size, s.mtime = info.Size(), info.ModTime()
	}
	return nil
}

// encrypt returns cfg with the secret needs sealed. A value that already
// carries the prefix is kept as it is, so a config read from the store and
// written back is not encrypted twice.
func (s *store) encrypt(needs []Need, cfg Config) (map[string]string, error) {
	out := make(map[string]string, len(cfg))
	secret := secretKeys(needs)
	for k, v := range cfg {
		if secret[k] && v != "" && !secrets.IsEncrypted(v) {
			sealed, err := s.box.Encrypt(v)
			if err != nil {
				return nil, err
			}
			v = sealed
		}
		out[k] = v
	}
	return out, nil
}

// decrypt returns the stored config in clear. A secret that is not
// encrypted is accepted as typed: an operator who edits the file by hand
// to paste a token gets it sealed on the next save instead of an error
// that would make the module unusable until they find out why.
func (s *store) decrypt(needs []Need, stored map[string]string) (Config, error) {
	out := make(Config, len(stored))
	secret := secretKeys(needs)
	for k, v := range stored {
		if secret[k] && secrets.IsEncrypted(v) {
			plain, err := s.box.Decrypt(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			v = plain
		}
		out[k] = v
	}
	return out, nil
}

func secretKeys(needs []Need) map[string]bool {
	out := map[string]bool{}
	for _, n := range needs {
		if n.Secret {
			out[n.Key] = true
		}
	}
	return out
}
