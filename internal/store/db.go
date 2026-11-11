// Package store persists the small amount of state the tool keeps between
// runs: which PRs have auto-merge / auto-rerun / auto-rebase enabled, the
// auto-rebase queue order, and the last auto-rerun time (for cooldowns). Everything else (retry counts, etc.) is derived live
// from the GitHub API, so the on-disk file stays tiny and disposable.
package store

import (
	"maps"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// DB is the in-memory representation of the persisted state.
type DB struct {
	AutoMerge map[string]bool
	// AutoMergeApprovals and AutoMergeResolved are the automerge conditions of
	// a PR: the number of approvals to wait for (2 or more), and whether to
	// wait until no review comment is unresolved.
	AutoMergeApprovals map[string]int
	AutoMergeResolved  map[string]bool

	AutoRerun     map[string]bool
	AutoRerunLast map[string]time.Time
	// AutoRebase maps each PR with auto-rebase on to its queue order. Only the
	// relative order of PRs on the same repo and base branch matters.
	AutoRebase map[string]int
}

// dbData is the on-disk YAML shape.
type dbData struct {
	AutoMerge          []string       `yaml:"auto_merge,omitempty"`
	AutoMergeApprovals map[string]int `yaml:"auto_merge_approvals,omitempty"`
	AutoMergeResolved  []string       `yaml:"auto_merge_resolved,omitempty"`

	AutoRerun     []string             `yaml:"auto_rerun,omitempty"`
	AutoRerunLast map[string]time.Time `yaml:"auto_rerun_last,omitempty"`
	AutoRebase    map[string]int       `yaml:"auto_rebase,omitempty"`
}

// Path returns the location of the db file inside the data dir.
func Path(dataDir string) string {
	return filepath.Join(dataDir, "db.yaml")
}

// Load reads the db file. A missing file is not an error and yields empty maps.
func Load(dataDir string) (DB, error) {
	db := DB{
		AutoMerge:          make(map[string]bool),
		AutoMergeApprovals: make(map[string]int),
		AutoMergeResolved:  make(map[string]bool),
		AutoRerun:          make(map[string]bool),
		AutoRerunLast:      make(map[string]time.Time),
		AutoRebase:         make(map[string]int),
	}

	data, err := os.ReadFile(Path(dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			return db, nil
		}

		return db, err
	}

	var d dbData
	if err := yaml.Unmarshal(data, &d); err != nil {
		return db, err
	}

	for _, key := range d.AutoMerge {
		db.AutoMerge[key] = true
	}

	for _, key := range d.AutoRerun {
		db.AutoRerun[key] = true
	}

	for _, key := range d.AutoMergeResolved {
		db.AutoMergeResolved[key] = true
	}

	maps.Copy(db.AutoMergeApprovals, d.AutoMergeApprovals)

	maps.Copy(db.AutoRerunLast, d.AutoRerunLast)
	maps.Copy(db.AutoRebase, d.AutoRebase)

	return db, nil
}

// Save writes the db file atomically (write temp, then rename).
func Save(dataDir string, db DB) error {
	var d dbData

	for key := range db.AutoMerge {
		d.AutoMerge = append(d.AutoMerge, key)
	}

	sort.Strings(d.AutoMerge)

	for key := range db.AutoMergeResolved {
		d.AutoMergeResolved = append(d.AutoMergeResolved, key)
	}

	sort.Strings(d.AutoMergeResolved)

	if len(db.AutoMergeApprovals) > 0 {
		d.AutoMergeApprovals = db.AutoMergeApprovals
	}

	for key := range db.AutoRerun {
		d.AutoRerun = append(d.AutoRerun, key)
	}

	sort.Strings(d.AutoRerun)

	if len(db.AutoRebase) > 0 {
		d.AutoRebase = db.AutoRebase
	}

	if len(db.AutoRerunLast) > 0 {
		d.AutoRerunLast = make(map[string]time.Time, len(db.AutoRerunLast))
		for key, last := range db.AutoRerunLast {
			// Only persist timestamps for PRs that still have auto-rerun on.
			if db.AutoRerun[key] && !last.IsZero() {
				d.AutoRerunLast[key] = last
			}
		}
	}

	data, err := yaml.Marshal(d)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dataDir, ".db.yaml.*")
	if err != nil {
		return err
	}

	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()

		return err
	}

	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()

		return err
	}

	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmpName, Path(dataDir))
}
