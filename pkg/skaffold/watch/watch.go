/*
Copyright 2018 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package watch

import (
	"context"
	"path"
	"path/filepath"
	"sort"
	"time"

	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/fileevents"

	"github.com/fsnotify/fsnotify"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const quietPeriod = 500 * time.Millisecond

// WatcherFactory can build Watchers from a list of files to be watched for changes
type WatcherFactory func(root string, paths []string) (Watcher, error)

// Watcher provides a watch trigger for the skaffold pipeline to begin
type Watcher interface {
	// Run watches a set of files for changes, and calls `onChange`
	// on each file change.
	Run(ctx context.Context, onChange func([]string)) error
}

type watchmanWatcher struct {
	files map[string]string
	root  string
}

func commonPrefix(sep byte, paths ...string) string {
	// Handle special cases.
	switch len(paths) {
	case 0:
		return ""
	case 1:
		return path.Clean(paths[0])
	}

	// Note, we treat string as []byte, not []rune as is often
	// done in Go. (And sep as byte, not rune). This is because
	// most/all supported OS' treat paths as string of non-zero
	// bytes. A filename may be displayed as a sequence of Unicode
	// runes (typically encoded as UTF-8) but paths are
	// not required to be valid UTF-8 or in any normalized form
	// (e.g. "é" (U+00C9) and "é" (U+0065,U+0301) are different
	// file names.
	c := []byte(path.Clean(paths[0]))

	// We add a trailing sep to handle the case where the
	// common prefix directory is included in the path list
	// (e.g. /home/user1, /home/user1/foo, /home/user1/bar).
	// path.Clean will have cleaned off trailing / separators with
	// the exception of the root directory, "/" (in which case we
	// make it "//", but this will get fixed up to "/" bellow).
	c = append(c, sep)

	// Ignore the first path since it's already in c
	for _, v := range paths[1:] {
		// Clean up each path before testing it
		v = path.Clean(v) + string(sep)

		// Find the first non-common byte and truncate c
		if len(v) < len(c) {
			c = c[:len(v)]
		}
		for i := 0; i < len(c); i++ {
			if v[i] != c[i] {
				c = c[:i]
				break
			}
		}
	}

	// Remove trailing non-separator characters and the final separator
	for i := len(c) - 1; i >= 0; i-- {
		if c[i] == sep {
			c = c[:i]
			break
		}
	}

	return string(c)
}

func NewWatchmanWatcher(root string, paths []string) (Watcher, error) {
	files := map[string]string{}

	sort.Strings(paths)
	if root == "" {
		root = commonPrefix(byte('/'), paths...)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}

	for _, p := range paths {
		absP, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}

		rel, err := filepath.Rel(root, absP)
		if err != nil {
			return nil, err
		}
		files[rel] = p
		logrus.Infof("Added watch for %s", p)
	}

	return &watchmanWatcher{
		root:  root,
		files: files,
	}, nil
}

func (f *watchmanWatcher) Run(ctx context.Context, onChange func([]string)) error {
	if len(f.files) == 0 {
		return nil
	}

	wm, err := fileevents.UseWatchman()
	if err != nil {
		return err
	}

	query := []interface{}{"anyof"}
	for p := range f.files {
		query = append(query, []string{
			"name",
			p,
			"wholename",
		})
	}

	sub, err := wm.Subscribe(f.root, "skaffold-watch", query)
	if err != nil {
		return err
	}

	go func() {
		<-ctx.Done()
		sub.Close()
	}()

	defer sub.Close()

	filter := fileevents.NewEventContentChangeFilter()
	filteredEvents := filter.FilterContentChanges(sub.Events)

	for fileevent := range filteredEvents {
		paths := []string{}
		for _, file := range fileevent.Files {
			paths = append(paths, f.files[file.Name])
		}
		onChange(paths)
	}
	return nil
}

// fsWatcher uses inotify to watch for changes and implements
// the Watcher interface
type fsWatcher struct {
	watcher *fsnotify.Watcher
	files   map[string]bool
}

// NewWatcher creates a new Watcher on a list of files.
func NewWatcher(paths []string) (Watcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, errors.Wrapf(err, "creating watcher")
	}

	files := map[string]bool{}

	sort.Strings(paths)
	for _, p := range paths {
		files[p] = true
		logrus.Infof("Added watch for %s", p)

		if err := w.Add(p); err != nil {
			w.Close()
			return nil, errors.Wrapf(err, "adding watch for %s", p)
		}

		if err := w.Add(filepath.Dir(p)); err != nil {
			w.Close()
			return nil, errors.Wrapf(err, "adding watch for %s", p)
		}
	}

	logrus.Info("Watch is ready")
	return &fsWatcher{
		watcher: w,
		files:   files,
	}, nil
}

// Run watches a set of files for changes, and calls `onChange`
// on each file change.
func (f *fsWatcher) Run(ctx context.Context, onChange func([]string)) error {
	changedPaths := map[string]bool{}

	timer := time.NewTimer(1<<63 - 1) // Forever
	defer timer.Stop()

	for {
		select {
		case ev := <-f.watcher.Events:
			if ev.Op == fsnotify.Chmod {
				continue // TODO(dgageot): VSCode seems to chmod randomly
			}
			if !f.files[ev.Name] {
				continue // File is not directly watched. Maybe its parent is
			}
			timer.Reset(quietPeriod)
			logrus.Infof("Change: %s", ev)
			changedPaths[ev.Name] = true
		case err := <-f.watcher.Errors:
			return errors.Wrap(err, "watch error")
		case <-timer.C:
			changes := sortedPaths(changedPaths)
			changedPaths = map[string]bool{}
			onChange(changes)
		case <-ctx.Done():
			f.watcher.Close()
			return nil
		}
	}
}

func sortedPaths(changedPaths map[string]bool) []string {
	var paths []string

	for path := range changedPaths {
		paths = append(paths, path)
	}

	sort.Strings(paths)
	return paths
}
