# Plan 02 — Convert regex collections from maps to slices

**Module:** `processor/redactionprocessor`
**Primary file:** `processor.go`
**Risk:** Low–Medium · **User-visible behavior change:** Yes (makes `blocked_values` masking order deterministic) · **Changelog:** Required (`enhancement`)

---

## Problem

The four regex collections are stored as `map[string]*regexp.Regexp`, keyed by the
pattern string, but the key is **never looked up after construction** — every use is
a `for _, compiledRE := range ...` value-only iteration:

Struct fields (`processor.go:41–47`):

```go
ignoreKeyRegexList map[string]*regexp.Regexp // :41
blockRegexList     map[string]*regexp.Regexp // :43
allowRegexList     map[string]*regexp.Regexp // :45
blockKeyRegexList  map[string]*regexp.Regexp // :47
```

Iteration sites (all value-only):

- `processStringValueForAttribute` — `processor.go:442`
- `processStringValueForLogBody` — `processor.go:466`
- `shouldMaskKey` — `processor.go:490`
- `shouldAllowValue` — `processor.go:500`
- `shouldIgnoreKey` — `processor.go:513`

Built by `makeRegexList` (`processor.go:637`):

```go
regexList := make(map[string]*regexp.Regexp, len(valuesList))
for _, pattern := range valuesList {
    re, err := regexp.Compile(pattern)
    ...
    regexList[pattern] = re
}
```

Two costs:

1. **Performance** — map iteration has worse cache locality and per-element
   overhead than a contiguous slice. Every string value is matched against every
   pattern (`processStringValueForLogBody` loops all of `blockRegexList`), so this
   loop is hot; with the 14-pattern benchmark config each leaf string is scanned 14×.
2. **Determinism** — Go randomizes map iteration order per run. When a single value
   matches **multiple overlapping** `blocked_values` patterns, `maskValue` mutates the
   string between patterns, so the final masked output depends on iteration order and
   is **not stable run-to-run**. The existing test suite only uses single-pattern or
   non-overlapping multi-pattern cases, so this is currently unguarded.

## Why it matters

Switching to `[]*regexp.Regexp` (config order) makes iteration faster and the output
**deterministic and reproducible** (patterns always applied in configured order).
For the non-overlapping cases the current tests cover, output is byte-identical;
for overlapping cases it becomes stable instead of random.

## Change

### 1. Struct fields (`processor.go:41–47`)

```go
// Attribute key patterns ignored in a span
ignoreKeyRegexList []*regexp.Regexp
// Attribute values blocked in a span
blockRegexList []*regexp.Regexp
// Attribute values allowed in a span
allowRegexList []*regexp.Regexp
// Attribute keys blocked in a span
blockKeyRegexList []*regexp.Regexp
```

### 2. `makeRegexList` (`processor.go:637`)

```go
// makeRegexList precompiles all the regex patterns in the defined list,
// preserving configuration order.
func makeRegexList(_ context.Context, valuesList []string) ([]*regexp.Regexp, error) {
	regexList := make([]*regexp.Regexp, 0, len(valuesList))
	for _, pattern := range valuesList {
		re, err := regexp.Compile(pattern)
		if err != nil {
			// TODO: Placeholder for an error metric in the next PR
			return nil, fmt.Errorf("error compiling regex in list: %w", err)
		}
		regexList = append(regexList, re)
	}
	return regexList, nil
}
```

### 3. Iteration sites

No body changes are needed — every loop already discards the key
(`for _, compiledRE := range s.<field>`). After the type change, those same loops
compile unchanged against a slice. Verify each of lines 442, 466, 490, 500, 513
still reads `for _, compiledRE := range ...` (or `for _, compiledRE := range ...` with
the value bound) and compiles.

### 4. Constructor

`newRedaction` (`processor.go:61`) assigns the `makeRegexList` results into the
struct fields; the assignments are unchanged (the variable types just become slices).

## Correctness / behavior notes

- `regexp.Compile` already rejects duplicate patterns silently as separate slice
  entries; the old map deduplicated identical pattern strings. If a user lists the
  **exact same** pattern twice, the slice will now contain it twice and apply it
  twice. For `blocked_values`/`blocked_key_patterns`/`ignored_key_patterns` this is
  harmless (masking/checks are idempotent on a second identical pass for the default
  `****` replacement and for the boolean key checks). Note this in the PR
  description. (If exact-duplicate dedup must be preserved, dedupe the input slice
  while building, preserving first-seen order — but this is almost certainly
  unnecessary.)
- Determinism improvement is the intended, desirable behavior change. Call it out in
  the changelog.

## Pre-flight check

Confirm nothing references these fields as maps (indexing by pattern, `len()` on a
map, `delete`, etc.) before editing:

```bash
cd processor/redactionprocessor
grep -rn 'blockRegexList\|allowRegexList\|blockKeyRegexList\|ignoreKeyRegexList\|makeRegexList' . --include='*.go'
```

Expected: only the struct decl, `newRedaction`, the five iteration sites, and
`makeRegexList`. If a test indexes these maps by key, update it.

## Tests

### Regression (must stay green)

```bash
go test -run 'TestMultipleBlockValues|TestAllowAllKeysMaskValues|TestRedactAllTypesTrue|TestRedactAllTypesFalse|TestProcessAttrsAppliedTwice' ./...
go test ./...
go test -race ./...
```

`TestMultipleBlockValues` (`processor_test.go:960`) uses two non-overlapping
`blocked_values` patterns and asserts exact masked output — it must remain byte-identical.

### New test to add — determinism guard (the point of this change)

This test fails (flakily) against the current map-based code and passes after the
slice change, locking in deterministic ordering. Append to `processor_test.go`:

```go
func TestBlockedValuesDeterministicOrder(t *testing.T) {
	// Two OVERLAPPING patterns that both match the same value; with map
	// iteration the masked result depends on random order. With ordered
	// slices the result is stable across runs.
	cfg := &Config{
		AllowAllKeys:   true,
		RedactAllTypes: true,
		BlockedValues: []string{
			`\d{3}-\d{2}-\d{4}`, // SSN-like
			`\d{2}-\d{4}`,        // overlaps the tail of the SSN
		},
	}

	build := func() plog.Logs {
		logs := plog.NewLogs()
		lr := logs.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
		lr.Body().SetStr("id 123-45-6789 end")
		return logs
	}

	proc, err := newRedaction(context.Background(), cfg, zaptest.NewLogger(t))
	require.NoError(t, err)

	// First result is the reference.
	first := build()
	_, err = proc.processLogs(context.Background(), first)
	require.NoError(t, err)
	want := first.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0).Body().Str()

	// Re-running the SAME input many times must always produce the SAME output.
	for i := 0; i < 200; i++ {
		logs := build()
		_, err = proc.processLogs(context.Background(), logs)
		require.NoError(t, err)
		got := logs.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0).Body().Str()
		require.Equalf(t, want, got, "non-deterministic masking on iteration %d", i)
	}
}
```

Run it:

```bash
go test -run TestBlockedValuesDeterministicOrder -count=5 ./...
```

> Tip: to demonstrate the bug *before* the fix, run this new test against the
> unmodified code — it should fail intermittently (try `-count=20`). After the slice
> change it must pass every run.

## Acceptance criteria

- [ ] The four fields are `[]*regexp.Regexp`; `makeRegexList` returns `[]*regexp.Regexp`.
- [ ] `grep` pre-flight shows no map-keyed usage remaining.
- [ ] `go build ./... && go vet ./...` clean; full suite + `-race` green.
- [ ] `TestMultipleBlockValues` output unchanged (byte-identical).
- [ ] `TestBlockedValuesDeterministicOrder` passes with `-count=5`.
- [ ] `benchstat /tmp/redaction-before.txt /tmp/redaction-after.txt` shows neutral-to-improved ns/op and allocs for `BenchmarkRedactLogsBlockedValues`.
- [ ] `.chloggen` entry added (`change_type: enhancement`, `component: processor/redaction`, note: deterministic `blocked_values` application order + faster iteration).

## Further optimization (optional, NOT part of this plan)

For an even larger win on the match phase, the `blocked_values` patterns could be
compiled into a **single combined alternation** (`(p1)|(p2)|…`) so RE2 matches all
branches in one linear pass instead of N passes. This changes masking semantics more
substantially (one pass, no inter-pattern mutation) and needs its own test design and
changelog. Defer to a separate proposal; the slice swap above is the safe, contained
change.

## Coordination with other plans

- Independent of Plans 01/03/04/05 (different code regions). Recommended to land
  **last** so the others rebase cleanly. Re-run the full suite after rebasing.
