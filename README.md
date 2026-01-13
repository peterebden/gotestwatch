# gotestwatch
A simple tool to watch a Go repo and automatically run affected tests within a module.

Usage is simple; run it within your Go module and when you edit files, it will re-run the affected tests.
It finds just the relevant ones affected by your change.

There are some known limitations which may or may not get addressed eventually:

 - It only checks test dependencies (packages imported by test files only) at one level, not recursively.
 - It probably only works on Linux right now because of a lightly modified version of fsnotify to get CloseWrite file events.
 - It may not fully account for some slightly more esoteric setups (cgo is likely an issue here).
