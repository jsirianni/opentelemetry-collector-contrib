# Plan 01 — Hoist `regexp.MustCompile(".*")` out of the per-value hot path

**Module:** `processor/redactionprocessor`
**Primary file:** `processor.go`
**Risk:** Low · **User-visible behavior change:** None · **Changelog:** Not required (`[chore]` / Skip Changelog)

---

## Problem

The "mask the whole value" path compiles a brand-new regular expression on every
masked key. `regexp.MustCompile` parses the pattern and builds an automaton; doing
it per value is one of the most expensive things on the hot path. The pattern is a
compile-time constant (`".*"`), so the compilation result is identical every time.

Call sites (line numbers from the current unmodified `processor.go`):

- `processor.go:364` — `processAttrs`, blocked-key mask branch:
  ```go
  maskedValue := s.maskValue(strVal, regexp.MustCompile(".*"))
  ```
- `processor.go:205` — `processLogBody`, map-key mask branch:
  ```go
  v.SetStr(s.maskValue(v.Str(), regexp.MustCompile(".*")))
  ```
- `processor.go:254` — `redactLogBodyRecursive`, nested-map-key mask branch:
  ```go
  v.SetStr(s.maskValue(v.Str(), regexp.MustCompile(".*")))
  ```

This path is reached whenever a key matches a `blocked_key_patterns` entry
(`shouldMaskKey` → true).

## Why it matters

Each `MustCompile(".*")` allocates and compiles a fresh `*regexp.Regexp`. For a
pipeline with many keys matching `blocked_key_patterns`, this is repeated per key
per record. Compiling once at package scope removes all of that per-value CPU and
allocation cost. The compiled object is stateless and safe for concurrent use, so a
single shared instance is correct.

## Change

Add one package-level variable and reuse it at all three call sites.

Add near the top of `processor.go` (e.g. just after the `attrValuesSeparator`
const at line 33):

```go
// maskAllRegex matches an entire string value and is used to mask values whose
// key matched a blocked_key_patterns entry. Compiled once: *regexp.Regexp is
// safe for concurrent use.
var maskAllRegex = regexp.MustCompile(".*")
```

Then replace the three `regexp.MustCompile(".*")` arguments with `maskAllRegex`:

```go
// processor.go:364
maskedValue := s.maskValue(strVal, maskAllRegex)

// processor.go:205
v.SetStr(s.maskValue(v.Str(), maskAllRegex))

// processor.go:254
v.SetStr(s.maskValue(v.Str(), maskAllRegex))
```

No other changes. `maskValue`'s signature is unchanged.

## Correctness / behavior notes (read before "optimizing" further)

- This change is **exactly behavior-preserving**: the same compiled pattern is fed
  to the same `maskValue` code path. Output bytes are identical.
- **Do NOT replace `maskValue(val, maskAllRegex)` with a direct
  `hashFunc(val)` / whole-string hash.** It looks equivalent but is not, because
  `.*` does not cross newlines in RE2: `ReplaceAllStringFunc` with `.*` masks each
  line of a multi-line value independently and preserves the `\n` separators
  (e.g. `"a\nb"` → `"****\n****"`), whereas hashing the whole string would collapse
  it to a single token. Preserving the regexp call keeps multi-line semantics
  intact. (This caveat is the main reason the change is "hoist only".)

## Tests

### Regression (must stay green)

Existing tests that exercise the `blocked_key_patterns` / mask-key path:

```bash
cd processor/redactionprocessor
go test -run 'TestRedactSummaryDebug|TestRedactSummaryDebugHashHMACSHA256|TestRedactSummaryDebugHashHMACSHA512|TestRedactSummaryDebugHashMD5' ./...
go test ./...
go test -race ./...
```

These configs use `BlockedKeyPatterns: []string{".*token.*", ".*api_key.*"}`
(see `processor_test.go:243`, `:392`, `:619`) and assert masked output, so they
cover all three call sites for the trace/attribute path.

### New test to add (locks multi-line semantics so a future refactor can't regress them)

Append to `processor_test.go` (package `redactionprocessor`, white-box — it can call
`newRedaction`/`processLogs` directly, matching the existing benchmarks). This
guards the caveat above and exercises the log-body mask-key path (lines 205/254):

```go
func TestMaskKeyPreservesMultilineSemantics(t *testing.T) {
	cfg := &Config{
		AllowAllKeys:       true,
		BlockedKeyPatterns: []string{".*secret.*"},
	}
	proc, err := newRedaction(context.Background(), cfg, zaptest.NewLogger(t))
	require.NoError(t, err)

	logs := plog.NewLogs()
	lr := logs.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	body := lr.Body()
	body.SetEmptyMap()
	// key matches blocked_key_patterns -> whole-value mask path (.* per line)
	body.Map().PutStr("my_secret", "line1\nline2")

	_, err = proc.processLogs(context.Background(), logs)
	require.NoError(t, err)

	got, ok := logs.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0).
		Body().Map().Get("my_secret")
	require.True(t, ok)
	// .* does not cross newlines: each line masked, separator preserved.
	assert.Equal(t, "****\n****", got.Str())
}
```

Run it:

```bash
go test -run TestMaskKeyPreservesMultilineSemantics ./...
```

> If, while implementing, you find this multi-line behavior is considered a bug by
> maintainers, that is a **separate** change with its own changelog — do not bundle
> it here. This plan only removes the recompilation cost.

## Acceptance criteria

- [ ] `var maskAllRegex` declared once; no `regexp.MustCompile(".*")` remains in `processor.go` (`grep -n 'MustCompile(".\*")' processor.go` returns nothing).
- [ ] `go build ./... && go vet ./...` clean.
- [ ] Full suite green: `go test ./...` and `go test -race ./...`.
- [ ] `TestMaskKeyPreservesMultilineSemantics` passes.
- [ ] `benchstat` shows no regression (this path isn't in `BenchmarkRedactLogsBlockedValues`, which sets no `blocked_key_patterns`; optionally add a quick benchmark with a `blocked_key_patterns` config to demonstrate the win).

## Coordination with other plans

- Touches the same `processAttrs` mask branch as **Plan 05** (line ~363–366) and the
  same body mask branches as **Plans 03/05**. If landing after those, the
  `regexp.MustCompile(".*")` argument may have moved by a few lines but the
  replacement is the same. Land independently; re-run the full suite.
