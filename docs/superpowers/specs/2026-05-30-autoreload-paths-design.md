# Autoreload Paths — Design Spec

**Date:** 2026-05-30
**Status:** Approved

## Problem

The `autoreload` directive currently only watches `working_dir` for `.py` file changes.
There is no way to monitor additional directories, or to watch a different set of paths
entirely. This is limiting for projects that import code from outside the working
directory (shared libraries, generated code, config files).

## Solution

Add a new `autoreload_paths` subdirective that accepts one or more directory paths.
When set, these paths **replace** `working_dir` for filesystem watching. The existing
`autoreload` directive remains the master on/off switch.

## Design

### Caddyfile

```caddyfile
python {
    module_wsgi "main:app"
    autoreload
    autoreload_paths /shared/lib /config/overrides
}
```

- `autoreload` is still required to enable watching (backward-compatible)
- `autoreload_paths` accepts zero or more paths as variadic arguments
- When `autoreload_paths` is **not** set → `working_dir` is watched (existing behavior)
- When `autoreload_paths` **is** set → those paths replace `working_dir` for watching
- Zero arguments to `autoreload_paths` is an error

### JSON

```json
{
    "module_wsgi": "main:app",
    "autoreload": "on",
    "autoreload_paths": ["/shared/lib", "/config/overrides"]
}
```

New field `autoreload_paths` (`[]string`) alongside existing `autoreload` (`string`).

### CLI

```bash
caddy python-server \
    --autoreload \
    --autoreload-path /shared/lib \
    --autoreload-path /config/overrides
```

- `--autoreload` stays as the existing boolean flag (unchanged)
- `--autoreload-path` is a new repeatable `StringSlice` flag

## Internal Changes

### Struct — `caddysnake.go`

```go
Autoreload      string   `json:"autoreload,omitempty"`
AutoreloadPaths []string `json:"autoreload_paths,omitempty"`
```

### Caddyfile Parsing — `UnmarshalCaddyfile` in `caddysnake.go`

```go
case "autoreload_paths":
    f.AutoreloadPaths = d.RemainingArgs()
    if len(f.AutoreloadPaths) == 0 {
        return d.Errf("expected at least one path for autoreload_paths")
    }
```

### CLI — `caddysnake.go` `pythonServer()` Flags

```go
cmd.Flags().StringSlice("autoreload-path", nil,
    "Additional paths to watch for .py changes (repeatable)")
```

And pass through to config:

```go
if autoreloadPaths := fs.StringSlice("autoreload-path"); len(autoreloadPaths) > 0 {
    pythonHandler.AutoreloadPaths = autoreloadPaths
}
```

### `AutoreloadableApp` — `autoreload.go`

- `workingDir string` → `watchDirs []string`
- `NewAutoreloadableApp` takes `watchDirs []string` instead of `workingDir string`
- Constructor calls `watchDirRecursive()` for **each** directory in `watchDirs`
- Logging changes from single `"working_dir"` to plural-aware output

### Provision Logic — `Provision()` in `caddysnake.go`

```go
if f.Autoreload == "on" {
    watchDirs := f.AutoreloadPaths
    if len(watchDirs) == 0 {
        // Fall back to working_dir (existing behavior)
        watchDir := f.WorkingDir
        if watchDir == "" {
            watchDir = "."
        }
        watchDirs = []string{watchDir}
    }
    // Resolve each to absolute path
    absDirs := make([]string, len(watchDirs))
    for i, d := range watchDirs {
        abs, err := filepath.Abs(d)
        if err != nil {
            return fmt.Errorf("autoreload: %w", err)
        }
        absDirs[i] = abs
    }
    // ... factory setup (unchanged) ...
    f.app, err = NewAutoreloadableApp(f.app, absDirs, factory, f.logger, nil)
}
```

### `DynamicApp` — `dynamic.go`

When `autoreload_paths` is set alongside a dynamic app (with Caddy placeholders):

- Dynamic per-resolved-dir watching still happens (no change)
- Explicit `autoreload_paths` paths are watched separately — they are shared library paths whose changes can affect *any* dynamic app
- When a `.py` change is detected in an `autoreload_paths` directory → evict **all** cached apps (full sweep). This is necessary because a shared library change can break any app regardless of its `working_dir`
- The eviction follows the existing cleanup pattern (10-second grace period for old app cleanup)

### Python CLI Wrapper — `caddysnake_cli.py`

```python
@click.option("--autoreload-path", multiple=True, help="Paths to watch for .py changes")

if autoreload_paths:
    for p in autoreload_paths:
        args.extend(["--autoreload-path", p])
```

## Edge Cases

| Case | Behavior |
| ---- | -------- |
| Path doesn't exist | `watchDirRecursive` logs a warning, fsnotify skips it |
| Path is a file (not dir) | `watchDirRecursive` skips it (only dirs are added) |
| Identical paths in list | `startWatchingDir` de-duplicates |
| Path overlap with working_dir | Both watches active; debounce deduplicates events |
| `autoreload_paths` with no args | Parse error: "expected at least one path" |

## Backward Compatibility

- `autoreload` without `autoreload_paths` → identical behavior to today
- All existing tests pass without modification
- No changes to existing Caddyfiles, JSON configs, or CLI scripts

## Testing

### Unit Tests

| Test | File | What It Covers |
| ---- | ---- | -------------- |
| Multiple watched dirs | `autoreload_test.go` | `NewAutoreloadableApp` with multiple dirs; write `.py` in each dir triggers reload |
| Empty paths fallback | `autoreload_test.go` | Empty `AutoreloadPaths` → fall back to `working_dir` |
| Caddyfile parse | `caddysnake_test.go` | `autoreload_paths /a /b /c` parses correctly |
| Caddyfile no-args error | `caddysnake_test.go` | `autoreload_paths` with no args returns error |
| CLI flag | `caddysnake_test.go` | `--autoreload-path /a --autoreload-path /b` populates field |

### Integration Tests

- Update `tests/simple_autoreload/` integration test to verify explicit paths work
