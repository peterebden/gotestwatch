// Package main implements a small tool that watches a Go repo and automatically runs tests as files are changed.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

const debouncePeriod = 200 * time.Millisecond

func main() {
	dir := flag.String("d", "", "Directory to start in")
	flag.Parse()
	if *dir != "" {
		if err := os.Chdir(*dir); err != nil {
			fmt.Printf("Failed to change directory to %s: %s\n", *dir, err)
			os.Exit(1)
		}
	}

	if err := run(); err != nil {
		fmt.Printf("Running gotestwatch failed: %s\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Find the go.mod first
	cmd := exec.Command("go", "env", "GOMOD")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to run `go env GOMOD`: %w", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == "/dev/null" {
		return fmt.Errorf("the current directory is not within a Go module")
	}
	dir := filepath.Dir(gomod)
	// Move there for simplicity
	if err := os.Chdir(dir); err != nil {
		return fmt.Errorf("failed to change to %s: %w", dir, err)
	}
	pkgs, err := loadPackages()
	if err != nil {
		return err
	}

	// Set up watches
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to set up filesystem watcher: %w", err)
	}
	for dir := range pkgs {
		// N.B. UnportableCloseWrite is normally unexported. It doesn't work on all platforms.
		if err := w.AddWith(dir, fsnotify.WithOps(fsnotify.UnportableCloseWrite)); err != nil {
			return fmt.Errorf("failed to set up watch on %s: %w", dir, err)
		}
	}
	revdeps := buildRevdeps(pkgs)

	fmt.Printf("Watching %d directories...\n", len(pkgs))
	go func() {
		for err := range w.Errors {
			fmt.Printf("Error watching directories: %s\n", err)
			os.Exit(1)
		}
	}()
	for event := range w.Events {
		fmt.Printf("%s changed", event.Name)
		events := debounceFor(w.Events, debouncePeriod)
		if len(events) != 0 {
			fmt.Printf(" (and %d others)\n", len(events))
		} else {
			fmt.Println("")
		}
		filenames := []string{event.Name}
		for _, event := range events {
			filenames = append(filenames, event.Name)
		}
		// Run affected tests
		if err := runAllTests(pkgs, revdeps, filenames); err != nil {
			fmt.Printf("Tests failed: %s\n", err)
		}
		fmt.Println("")
	}
	return nil
}

// debounceFor reads all events from a channel for up to the given period of time.
func debounceFor[T any](ch <-chan T, duration time.Duration) []T {
	deadline := time.After(duration)
	events := []T{}
	for {
		select {
		case t := <-ch:
			events = append(events, t)
		case <-deadline:
			return events
		}
	}
}

// loadPackages finds all Go packages within this module
func loadPackages() (map[string]*Package, error) {
	cmd := exec.Command("go", "list", "-json", "./...")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to run `go list -json ./...`: %w", err)
	}
	d := json.NewDecoder(bytes.NewReader(out))
	pkgs := map[string]*Package{}
	for {
		pkg := &Package{}
		if err := d.Decode(pkg); err != nil {
			if err == io.EOF {
				return pkgs, nil
			}
			return nil, fmt.Errorf("failed to decode JSON: %w", err)
		}
		pkgs[pkg.Dir] = pkg
	}
}

// A Package is a minimal model of a Go package described by go list
type Package struct {
	Dir            string   `json:"Dir"`
	ImportPath     string   `json:"ImportPath"`
	Deps           []string `json:"Deps"`
	GoFiles        []string `json:"GoFiles"`
	IgnoredGoFiles []string `json:"IgnoredGoFiles"`
	TestGoFiles    []string `json:"TestGoFiles"`
	XTestGoFiles   []string `json:"XTestGoFiles"`
	EmbedFiles     []string `json:"EmbedFiles"`
	TestImports    []string `json:"TestImports"`
	XTestImports   []string `json:"XTestImports"`
	// TODO(peter): We might need to think about cgo here? Are there any other relevant file types on this thing?
	Module struct {
		Path string `json:"Path"`
	} `json:"Module"`
}

// buildRevdeps builds a reverse dependency map of all tests that depend on each package.
func buildRevdeps(pkgs map[string]*Package) map[string][]*Package {
	revdeps := map[string][]*Package{}
	for _, pkg := range pkgs {
		for _, dep := range pkg.Deps {
			if strings.HasPrefix(dep, pkg.Module.Path) {
				revdeps[dep] = append(revdeps[dep], pkg)
			}
		}
		// Deps don't include test deps.
		// TODO(peter): These import fields aren't recursive so we could miss something.
		for _, imp := range append(pkg.TestImports, pkg.XTestImports...) {
			if strings.HasPrefix(imp, pkg.Module.Path) && !slices.Contains(revdeps[imp], pkg) {
				revdeps[imp] = append(revdeps[imp], pkg)
			}
		}
	}
	return revdeps
}

// testsToRun returns the affected set of test packages that should be run
func testsToRun(pkg *Package, revdeps []*Package, filenames []string) []*Package {
	if len(revdeps) == 0 {
		return nil
	}
	// Excise any files that are ignored (e.g. excluded by build constraints)
	filenames = slices.DeleteFunc(filenames, func(filename string) bool {
		return slices.Contains(pkg.IgnoredGoFiles, filename)
	})
	if len(filenames) == 0 {
		return nil
	}
	toRun := slices.Clone(revdeps)
	// If all files are test files, then we are just running this test
	if allFunc(filenames, func(filename string) bool {
		return slices.Contains(pkg.TestGoFiles, filename) || slices.Contains(pkg.XTestGoFiles, filename)
	}) {
		return []*Package{pkg}
	}
	// Include this package, which isn't in the list because it doesn't depend on itself
	return append(toRun, pkg)
}

// runAllTests runs all tests possibly affected by changes to the given files
func runAllTests(pkgs map[string]*Package, revdeps map[string][]*Package, filenames []string) error {
	byDir := map[string][]string{}
	for _, filename := range filenames {
		dir, filename := filepath.Split(filename)
		dir = strings.TrimSuffix(dir, "/")
		byDir[dir] = append(byDir[dir], filename)
	}
	toRun := []*Package{}
	for dir, files := range byDir {
		pkg, present := pkgs[dir]
		if !present {
			continue
		}
		toRun = append(toRun, testsToRun(pkg, revdeps[pkg.ImportPath], files)...)
	}
	// Only run tests in packages that have tests in them
	toRun = slices.DeleteFunc(toRun, func(pkg *Package) bool {
		return len(pkg.TestGoFiles) == 0 && len(pkg.XTestGoFiles) == 0
	})
	paths := make([]string, len(toRun))
	for i, pkg := range toRun {
		paths[i] = pkg.ImportPath
	}
	// Make these unique
	slices.Sort(paths)
	paths = slices.Compact(paths)
	if len(paths) == 0 {
		fmt.Println("No affected tests to run")
		return nil
	} else if len(paths) == 1 {
		fmt.Println("Running tests in 1 package...")
	} else {
		fmt.Printf("Running tests in %d packages...\n", len(paths))
	}
	args := append([]string{"test"}, paths...)
	cmd := exec.Command("go", args...)
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	if err := cmd.Run(); err != nil {
		return err
	}
	fmt.Println("Tests passed")
	return nil
}

func allFunc[S ~[]E, E any](s S, f func(E) bool) bool {
	for _, x := range s {
		if !f(x) {
			return false
		}
	}
	return true
}
