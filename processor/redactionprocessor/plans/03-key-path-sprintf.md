# Plan 03 — Replace `fmt.Sprintf` key-path building in the log-body recursion

**Module:** `processor/redactionprocessor`
**Primary file:** `processor.go`
**Risk:** Low · **User-visible behavior change:** None · **Changelog:** Not required (`[chore]` / Skip Changelog)

---

## Problem

The log-body walk builds a dotted key path for every nested map key and slice index
using `fmt.Sprintf`. `fmt.Sprintf` uses reflection and allocates a new string each
call, and these run for **every element of every nested body**, regardless of whether
anything is redacted.

Call sites:

- `processor.go:217` — `processLogBody`, slice element index:
  ```go
  s.redactLogBodyRecursive(ctx, fmt.Sprintf("[%d]", i), body.Slice().At(i), ...)
  ```
- `processor.go:243` — `redactLogBodyRecursive`, nested map child:
  ```go
  keyWithPath := fmt.Sprintf("%s.%s", key, k)
  ```
- `processor.go:262` — `redactLogBodyRecursive`, redacted-key path:
  ```go
  keyWithPath := fmt.Sprintf("%s.%s", key, k)
  ```
- `processor.go:267` — `redactLogBodyRecursive`, slice element:
  ```go
  keyWithPath := fmt.Sprintf("%s.[%d]", key, i)
  ```

## Why it matters

`fmt.Sprintf("%s.%s", a, b)` is materially slower and allocates more than `a + "." + b`
(no format-string parse, no reflection, no `[]any` boxing of the args). For logs with
nested maps and slices (the realistic body in `BenchmarkRedactLogsBlockedValues`),
this is per-element allocation churn on the hot path. Plain concatenation and
`strconv.Itoa` produce byte-identical strings.

## Change

Add `strconv` to the import block (`processor.go:7–31`). Keep `fmt` (still used by
`newRedaction` error wrapping and `makeRegexList`).

Replace the four call sites:

```go
// processor.go:217  (processLogBody, slice element)
s.redactLogBodyRecursive(ctx, "["+strconv.Itoa(i)+"]", body.Slice().At(i),
	&redactedKeys, &maskedKeys, &allowedKeys, &ignoredKeys)

// processor.go:243  (redactLogBodyRecursive, nested map child)
keyWithPath := key + "." + k

// processor.go:262  (redactLogBodyRecursive, redacted-key path)
keyWithPath := key + "." + k

// processor.go:267  (redactLogBodyRecursive, slice element)
keyWithPath := key + ".[" + strconv.Itoa(i) + "]"
```

## Correctness / behavior notes

- **Byte-identical output.** `fmt.Sprintf("%s.%s", key, k)` == `key + "." + k`.
  `fmt.Sprintf("[%d]", i)` == `"[" + strconv.Itoa(i) + "]"`.
  `fmt.Sprintf("%s.[%d]", key, i)` == `key + ".[" + strconv.Itoa(i) + "]"`.
  Slice indices are always non-negative `int`, so `%d` and `strconv.Itoa` agree.
- These path strings surface to users only via the `redaction.body.*.keys`
  diagnostic attributes (when `summary: debug`), so any string drift would be
  user-visible — hence the explicit byte-identity requirement and the test below.
- Leave the `fmt.Sprintf` usages elsewhere in the file (error wrapping) untouched.

## Tests

### Regression (must stay green)

```bash
go test -run 'TestLogBodyRedactionDifferentTypes|TestRedactSummaryDebug|TestRedactSummaryInfo' ./...
go test ./...
```

### New test to add — locks the exact path strings

Append to `processor_test.go`. It builds a nested map + slice body with
`summary: debug`, then asserts the dotted paths reported in
`redaction.body.masked.keys` exactly match the concatenation output:

```go
func TestLogBodyMaskedKeyPathsExact(t *testing.T) {
	cfg := &Config{
		AllowAllKeys:   true,
		RedactAllTypes: true,
		Summary:        "debug",
		BlockedValues:  []string{`4[0-9]{12}(?:[0-9]{3})?`}, // matches the card numbers below
	}
	proc, err := newRedaction(context.Background(), cfg, zaptest.NewLogger(t))
	require.NoError(t, err)

	logs := plog.NewLogs()
	lr := logs.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.Body().SetEmptyMap()
	body := lr.Body().Map()
	nested := body.PutEmptyMap("nested")
	nested.PutStr("card", "4111111111111111") // -> path "nested.card"
	slice := body.PutEmptySlice("cards")
	slice.AppendEmpty().SetStr("4111111111111111") // -> path "cards.[0]"

	_, err = proc.processLogs(context.Background(), logs)
	require.NoError(t, err)

	attrs := logs.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0).Attributes()
	maskedKeys, ok := attrs.Get("redaction.body.masked.keys")
	require.True(t, ok)
	// addMetaAttrs sorts the keys; assert both expected dotted paths are present
	// and exactly formatted.
	parts := strings.Split(maskedKeys.Str(), ",")
	assert.Contains(t, parts, "nested.card")
	assert.Contains(t, parts, "cards.[0]")
}
```

Run it:

```bash
go test -run TestLogBodyMaskedKeyPathsExact ./...
```

> Before editing, run this test against the unmodified code to confirm it captures
> the current path format; it must stay green through the change.

## Acceptance criteria

- [ ] No `fmt.Sprintf` remains in `processLogBody`/`redactLogBodyRecursive` (`grep -n 'fmt.Sprintf' processor.go` shows only the unrelated error-wrapping sites, if any).
- [ ] `strconv` imported; `fmt` still imported and used.
- [ ] `go build ./... && go vet ./...` clean; full suite + `-race` green.
- [ ] `TestLogBodyMaskedKeyPathsExact` passes against both old and new code (proving identical output).
- [ ] `benchstat` shows reduced allocs/op for `BenchmarkRedactLogsBlockedValues` (the body has a nested map + slice, so the change is exercised).

## Coordination with other plans

- Touches the body recursion that **Plan 05** also touches (bookkeeping appends).
  If both land, keep the `keyWithPath` construction (it is also the recursion prefix);
  Plan 05 only gates the `append` calls, not the path building. Land independently
  and re-run the suite.
