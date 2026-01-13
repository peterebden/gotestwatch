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

	"github.com/fsnotify/fsnotify"
)

func main() {
	dir := flag.String("d", "", "Directory to start in")
	flag.Parse()
	if *dir != "" {
		if err := os.Chdir(*dir); err != nil {
			fmt.Printf("%s\n", err)
			os.Exit(1)
		}
	}

	if err := run(); err != nil {
		fmt.Printf("%s\n", err)
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
	dir := filepath.Dir(strings.TrimSpace(string(out)))
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
		// N.B. CloseWrite is an extension I added. It doesn't work on all platforms.
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
		// TODO(peter): we might want some debouncing here
		fmt.Printf("%s changed\n", event.Name)
		dir, filename := filepath.Split(event.Name)
		dir = strings.TrimSuffix(dir, "/")
		pkg, present := pkgs[dir]
		if !present {
			fmt.Printf("unknown package %s (from %s)\n", dir, event.Name)
			continue
		}
		// Run affected tests
		if err := runTests(pkg, revdeps[pkg.ImportPath], filename); err != nil {
			fmt.Printf("Tests failed: %s\n", err)
		}
		fmt.Println("")
	}
	return nil
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

// runTests runs all affected tests when an individual file changes
func runTests(pkg *Package, revdeps []*Package, filename string) error {
	if len(revdeps) == 0 {
		fmt.Println("No affected tests to run")
		return nil
	}
	// Check if this file affects anything
	if slices.Contains(pkg.IgnoredGoFiles, filename) {
		fmt.Println("File is excluded by build constraints")
		return nil
	}
	if slices.Contains(pkg.TestGoFiles, filename) || slices.Contains(pkg.XTestGoFiles, filename) {
		// A change to a test file means we're just running this test. That's fine.
		revdeps = []*Package{pkg}
	} else {
		// Include this package, which isn't in the list because it doesn't depend on itself
		revdeps = append(revdeps, pkg)
	}
	// Only run tests in packages that have tests in them
	revdeps = slices.DeleteFunc(slices.Clone(revdeps), func(pkg *Package) bool {
		return len(pkg.TestGoFiles) == 0 && len(pkg.XTestGoFiles) == 0
	})
	paths := make([]string, len(revdeps))
	for i, pkg := range revdeps {
		paths[i] = pkg.ImportPath
	}
	if len(revdeps) == 0 {
		fmt.Println("No affected tests to run")
		return nil
	} else if len(revdeps) == 1 {
		fmt.Println("Running tests in 1 package...")
	} else {
		fmt.Printf("Running tests in %d packages...\n", len(revdeps))
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
