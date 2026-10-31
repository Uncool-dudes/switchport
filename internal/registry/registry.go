// Package registry is the persistent set of routes, shared between every
// switchport process on a machine.
package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/Uncool-dudes/switchport/internal/route"
)

const (
	schemaVersion = 1
	routesFile    = "routes.json"
	lockFile      = "routes.lock"
	dirMode       = 0o700
	lockMode      = 0o600
)

// NameTakenError means a live route already owns the name.
type NameTakenError struct {
	Name route.Name
	PID  int // zero for an alias
}

func (e *NameTakenError) Error() string {
	if e.PID == 0 {
		return fmt.Sprintf("%s is already an alias", e.Name)
	}
	return fmt.Sprintf("%s is already in use by pid %d", e.Name, e.PID)
}

// Routes is a view of the registry.
type Routes struct {
	Apps    []route.App   `json:"apps"`
	Aliases []route.Alias `json:"aliases"`
}

type stored struct {
	Version int `json:"version"`
	Routes
}

// Registry reads and writes routes in one directory.
type Registry struct {
	dir string
}

// Open prepares dir for use, creating it if needed.
func Open(dir string) (*Registry, error) {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, err
	}
	return &Registry{dir: dir}, nil
}

// List returns every live route. The file is replaced by rename, so a reader
// never sees a partial write and needs no lock.
func (r *Registry) List() (Routes, error) {
	s, err := r.load()
	if err != nil {
		return Routes{}, err
	}
	s.Apps = slices.DeleteFunc(s.Apps, func(a route.App) bool { return !pidAlive(a.PID) })
	return s.Routes, nil
}

// Stale returns apps whose wrapper died, leaving their command unmanaged.
func (r *Registry) Stale() ([]route.App, error) {
	s, err := r.load()
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(s.Apps, func(a route.App) bool { return pidAlive(a.PID) }), nil
}

// AddApp registers an app. With replace, it takes the name from a live owner.
func (r *Registry) AddApp(app route.App, replace bool) error {
	return r.update(func(s *stored) error {
		if !replace {
			if err := taken(s, app.Name, app.PID); err != nil {
				return err
			}
		}
		remove(s, app.Name)
		s.Apps = append(s.Apps, app)
		return nil
	})
}

// AddAlias registers an alias. With replace, it takes the name from a live owner.
func (r *Registry) AddAlias(alias route.Alias, replace bool) error {
	return r.update(func(s *stored) error {
		if !replace {
			if err := taken(s, alias.Name, 0); err != nil {
				return err
			}
		}
		remove(s, alias.Name)
		s.Aliases = append(s.Aliases, alias)
		return nil
	})
}

// Available reports a NameTakenError if a live route owns name.
func (r *Registry) Available(name route.Name) error {
	s, err := r.load()
	if err != nil {
		return err
	}
	return taken(&s, name, 0)
}

// RemoveApp deletes an app, but only if pid still owns the name, so a wrapper
// whose route was replaced cannot delete its successor.
func (r *Registry) RemoveApp(name route.Name, pid int) error {
	return r.update(func(s *stored) error {
		s.Apps = slices.DeleteFunc(s.Apps, func(a route.App) bool { return a.Name == name && a.PID == pid })
		return nil
	})
}

// RemoveAlias deletes an alias.
func (r *Registry) RemoveAlias(name route.Name) error {
	return r.update(func(s *stored) error {
		s.Aliases = slices.DeleteFunc(s.Aliases, func(a route.Alias) bool { return a.Name == name })
		return nil
	})
}

// RemoveStale deletes apps whose wrapper died.
func (r *Registry) RemoveStale() error {
	return r.update(func(s *stored) error {
		s.Apps = slices.DeleteFunc(s.Apps, func(a route.App) bool { return !pidAlive(a.PID) })
		return nil
	})
}

func taken(s *stored, name route.Name, self int) error {
	for _, a := range s.Apps {
		if a.Name == name && a.PID != self && pidAlive(a.PID) {
			return &NameTakenError{Name: name, PID: a.PID}
		}
	}
	for _, a := range s.Aliases {
		if a.Name == name {
			return &NameTakenError{Name: name}
		}
	}
	return nil
}

func remove(s *stored, name route.Name) {
	s.Apps = slices.DeleteFunc(s.Apps, func(a route.App) bool { return a.Name == name })
	s.Aliases = slices.DeleteFunc(s.Aliases, func(a route.Alias) bool { return a.Name == name })
}

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func (r *Registry) load() (stored, error) {
	s := stored{Version: schemaVersion}
	data, err := os.ReadFile(filepath.Join(r.dir, routesFile))
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("read %s: %w", routesFile, err)
	}
	if s.Version != schemaVersion {
		return s, fmt.Errorf("%s has schema version %d, this switchport reads %d", routesFile, s.Version, schemaVersion)
	}
	return s, nil
}

// update applies fn while holding the lock. The kernel drops a flock when its
// holder dies, so a crashed writer needs no expiry logic.
func (r *Registry) update(fn func(*stored) error) error {
	lock, err := os.OpenFile(filepath.Join(r.dir, lockFile), os.O_CREATE|os.O_RDWR, lockMode)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock registry: %w", err)
	}
	s, err := r.load()
	if err != nil {
		return err
	}
	if err := fn(&s); err != nil {
		return err
	}
	return r.save(s)
}

// save replaces the file through a rename in the same directory, so a writer
// killed at any point leaves the previous version intact.
func (r *Registry) save(s stored) (err error) {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(r.dir, routesFile+".*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(r.dir, routesFile))
}
