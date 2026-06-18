# Plan 05 — Skip diagnostic bookkeeping when `summary` is disabled

**Module:** `processor/redactionprocessor`
**Primary file:** `processor.go`
**Risk:** Low · **User-visible behavior change:** None · **Changelog:** Not required (`[chore]` / Skip Changelog)

---

## Problem

`processAttrs` and the log-body walk accumulate up to four `[]string` slices —
`redactedKeys`, `maskedKeys`, `allowedKeys`, `ignoredKeys` — via `append`. But three
of them (`maskedKeys`, `allowedKeys`, `ignoredKeys`) are **only** consumed by
`addMetaAttrs`, which emits nothing unless `summary` is `info` or `debug`
(`processor.go:419–438`):

```go
if s.config.Summary == debug && valuesAttr != "" { ... }      // :426
if s.config.Summary == info || s.config.Summary == debug { ... } // :433
```

`summary` defaults to empty and may also be `silent` (config.go documents
`debug`, `info`, `silent`). In all of those cases the three slices are grown on every
record and then discarded — wasted allocation on the per-record hot path for
traces, logs, and metrics.

`redactedKeys` is different: besides reporting, it drives the actual attribute
removal (`processAttrs:376` `for _, k := range redactedKeys { attributes.Remove(k) }`;
body equivalents at `:211` and `:260`). It must keep being accumulated regardless.

## Why it matters

For high-throughput pipelines that don't enable the diagnostic summary (the default),
this removes per-record slice growth/allocation that has no observable effect.

## Change

### 1. Add a precomputed flag to the struct (`processor.go:35`)

```go
type redaction struct {
	...
	// summaryEnabled is true when diagnostic summary attributes are emitted
	// (config.Summary is "info" or "debug"). When false, masked/allowed/ignored
	// key bookkeeping is skipped.
	summaryEnabled bool
	...
}
```

Set it in `newRedaction` (`processor.go:95`, in the returned struct literal):

```go
summaryEnabled: config.Summary == info || config.Summary == debug,
```

(`info` and `debug` are the existing package consts at `processor.go:584–585`.)

### 2. Gate the bookkeeping appends in `processAttrs` (`processor.go:344–373`)

Keep all control flow (`continue`/early-out) and all `value.SetStr(...)` masking and
the `redactedKeys` accumulation **unchanged**. Gate only these three appends:

```go
// ignored (was processor.go:346)
if s.shouldIgnoreKey(k) {
	if s.summaryEnabled {
		ignoredKeys = append(ignoredKeys, k)
	}
	continue
}

// redacted — UNCHANGED, always accumulate (drives Remove at :376)
if s.shouldRedactKey(k) {
	redactedKeys = append(redactedKeys, k)
	continue
}

...
// allowed (was processor.go:359)
if s.shouldAllowValue(strVal) {
	if s.summaryEnabled {
		allowedKeys = append(allowedKeys, k)
	}
	continue
}

// masked via blocked key (was processor.go:363)
if s.shouldMaskKey(k) {
	if s.summaryEnabled {
		maskedKeys = append(maskedKeys, k)
	}
	value.SetStr(s.maskValue(strVal, maskAllRegex)) // see Plan 01 for maskAllRegex
	continue
}

// masked via blocked value (was processor.go:369)
processedString := s.processStringValueForAttribute(strVal, k)
if processedString != strVal {
	if s.summaryEnabled {
		maskedKeys = append(maskedKeys, k)
	}
	value.SetStr(processedString)
}
```

The four `addMetaAttrs(...)` calls at the end (`:380–383`) can stay as-is — with the
gated slices nil, each returns early (`len == 0`). Optionally wrap them in
`if s.summaryEnabled { ... }` to skip the calls entirely; functionally identical.

### 3. (Extension) Gate the same appends in the log-body walk

`processLogBody` (`:188`) and `redactLogBodyRecursive` (`:238`) have the same
pattern. Gate the leaf bookkeeping appends behind `s.summaryEnabled`:

- `ignoredKeys` appends (`:196`, `:245`)
- `maskedKeys` appends (`:204`, `:227`, `:253`, `:278`)
- `allowedKeys` appends (`:222`, `:273`)
- the `redactedKeys` **path** appends inside the removal loops (`:213`, `:263`)

**Keep unchanged:** `redactedBodyKeys` / `redactedCurrentValueKeys` (they drive
`body.Map().Remove(k)` at `:212`/`:261`), all `SetStr` masking, the `keyWithPath`
construction (it is also the recursion prefix), and the recursion itself.

> If you'd rather keep the diff small and reviewable, land step 2 (`processAttrs`)
> first as its own commit — it covers the universally-hot path for all three signal
> types — then do step 3 as a follow-up. The `summaryEnabled` field is shared.

## Correctness / behavior notes

- **Behavior-preserving.** When `summaryEnabled` is false, the gated slices were
  already discarded by `addMetaAttrs`; skipping the appends changes no output. When
  true, every append runs exactly as before.
- `redactedKeys` (and the body `redacted*Keys`) are **never** gated — removal must
  still happen with `summary` disabled.
- Masking/allow/ignore **decisions and effects** (`SetStr`, `continue`, `Remove`) are
  untouched; only the diagnostic accounting is conditional.

## Tests

### Regression (must stay green) — covers both enabled and disabled modes

```bash
go test -run 'TestRedactSummaryDebug|TestRedactSummaryInfo|TestRedactSummarySilent|TestRedactSummaryDefault|TestRedactUnknownAttributes|TestAllowAllKeysMaskValues' ./...
go test ./...
go test -race ./...
```

- `TestRedactSummaryDebug` (`:236`) / `TestRedactSummaryInfo` (`:720`):
  `summaryEnabled` true → meta attrs still emitted, unchanged.
- `TestRedactSummarySilent` (`:824`) / `TestRedactSummaryDefault` (`:896`):
  `summaryEnabled` false → no meta attrs, and redaction/masking still applied.
  Verify these assert both the **absence** of `redaction.*` attributes and the
  **presence** of correct masking/removal; if the absence assertion is missing,
  strengthen the test (see below).

### New test to add — disabled summary still redacts/masks/allows, emits no meta attrs

Append to `processor_test.go`:

```go
func TestSummaryDisabledStillRedactsButEmitsNoMeta(t *testing.T) {
	cfg := &Config{
		AllowedKeys:   []string{"keep"},        // AllowAllKeys false -> non-allowed keys redacted
		BlockedValues: []string{`4[0-9]{12}(?:[0-9]{3})?`},
		Summary:       "", // disabled (default)
	}
	proc, err := newRedaction(context.Background(), cfg, zaptest.NewLogger(t))
	require.NoError(t, err)

	logs := plog.NewLogs()
	lr := logs.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	a := lr.Attributes()
	a.PutStr("keep", "placeholder 4111111111111111") // allowed key, value masked
	a.PutStr("drop", "anything")                       // not allowed -> redacted/removed

	_, err = proc.processLogs(context.Background(), logs)
	require.NoError(t, err)

	out := lr.Attributes()
	// functional redaction still happens
	kept, ok := out.Get("keep")
	require.True(t, ok)
	assert.Equal(t, "placeholder ****", kept.Str())
	_, ok = out.Get("drop")
	assert.False(t, ok, "non-allowed key must still be removed when summary disabled")

	// but NO diagnostic meta attributes are emitted
	for _, metaKey := range []string{
		"redaction.redacted.keys", "redaction.redacted.count",
		"redaction.masked.keys", "redaction.masked.count",
		"redaction.allowed.keys", "redaction.allowed.count",
		"redaction.ignored.count",
	} {
		_, ok := out.Get(metaKey)
		assert.Falsef(t, ok, "unexpected meta attribute %q when summary disabled", metaKey)
	}
}
```

Run it:

```bash
go test -run TestSummaryDisabledStillRedactsButEmitsNoMeta ./...
```

## Acceptance criteria

- [ ] `summaryEnabled` field added and set from `config.Summary` in `newRedaction`.
- [ ] In `processAttrs`, `redactedKeys` accumulation and all `SetStr`/`continue`/`Remove` flow are unchanged; only `maskedKeys`/`allowedKeys`/`ignoredKeys` appends are gated.
- [ ] (If step 3 done) body-walk leaf appends gated identically; `Remove` and recursion untouched.
- [ ] `go build ./... && go vet ./...` clean; full suite + `-race` green.
- [ ] All four `TestRedactSummary*` tests pass; `TestSummaryDisabledStillRedactsButEmitsNoMeta` passes.
- [ ] `benchstat` shows reduced allocs/op for `BenchmarkRedactLogsBlockedValues` (its config leaves `summary` unset → bookkeeping now skipped).

## Coordination with other plans

- Step 2 edits the same `processAttrs` mask branch as **Plan 01** (`maskAllRegex`).
  The snippet above already shows the post-Plan-01 form; if Plan 01 hasn't landed,
  use `regexp.MustCompile(".*")` there and let Plan 01 replace it later.
- Step 3 touches the same body recursion as **Plan 03**; they are orthogonal
  (Plan 03 changes *how* `keyWithPath` is built, Plan 05 gates *whether* it is
  appended for reporting). Land independently and re-run the suite.
