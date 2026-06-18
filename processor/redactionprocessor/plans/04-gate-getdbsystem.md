# Plan 04 — Only call `db.GetDBSystem` when DB obfuscation is enabled

**Module:** `processor/redactionprocessor`
**Primary file:** `processor.go` (one site)
**Risk:** Low · **User-visible behavior change:** None · **Changelog:** Not required (`[chore]` / Skip Changelog)

---

## Problem

`processAttrs` resolves the DB system from the attribute set on **every** call,
guarded only by a nil check that is always false:

`processor.go:332–334`:

```go
if s.dbObfuscator != nil {
	s.dbObfuscator.DBSystem = db.GetDBSystem(attributes)
}
```

`s.dbObfuscator` is assigned unconditionally in `newRedaction`
(`processor.go:93`: `dbObfuscator := db.NewObfuscator(config.DBSanitizer, logger)`),
and `db.NewObfuscator` never returns nil (`internal/db/db.go:30`). So the guard is
always true and `db.GetDBSystem(attributes)` runs for every span / log-record /
metric-datapoint attribute set — including pipelines that don't configure the DB
sanitizer at all.

`db.GetDBSystem` is not free (`internal/db/spanname.go:44`): it performs up to two
`attributes.Get(...)` map lookups (`db.system.name`, then `db.system`) plus a
`strings.ToLower` when found.

## Why it matters

`processAttrs` is the per-record hot path for traces, logs, and metrics. When DB
obfuscation is disabled (the common case), the resolved `DBSystem` is never read:
both `processStringValueForAttribute` (`:453`) and `processStringValueForLogBody`
(`:477`) gate the DB obfuscation behind `s.dbObfuscator.HasObfuscators()`, and
`ObfuscateWithSystem` itself early-returns when there are no obfuscators
(`internal/db/db.go:210`). So the two map lookups per attribute set are pure waste
whenever DB sanitization is off.

## Change

Gate the assignment on `HasObfuscators()` (a cheap `len(o.obfuscators) > 0`,
`internal/db/db.go:206`):

```go
if s.dbObfuscator.HasObfuscators() {
	s.dbObfuscator.DBSystem = db.GetDBSystem(attributes)
}
```

`s.dbObfuscator` is never nil, so dropping the `!= nil` check in favor of the
method call is safe.

## Correctness / behavior notes

- **Behavior-preserving.** When `HasObfuscators()` is false, `DBSystem` is never
  consumed (all read paths are themselves gated on `HasObfuscators()`), so not
  setting it changes no output.
- When `HasObfuscators()` is true, behavior is identical to today — `DBSystem` is
  resolved exactly as before.
- `s.dbObfuscator.DBSystem` is a mutable field reused across calls. With the gate,
  it is only ever written when obfuscators exist, which is precisely when it is read.
  No stale-value concern, because the disabled path never reads it.

## Tests

### Regression (must stay green)

The DB-obfuscation tests configure obfuscators (`HasObfuscators()` → true), so they
exercise the unchanged branch:

```bash
go test -run 'TestDBObfuscation|TestLogAttributesObfuscationWithoutDBSystem|TestMetricAttributesDBObfuscation|TestDBObfuscationOnLogBody|TestDBObfuscationErrorInAttribute' ./...
go test ./...
go test -race ./...
```

Relevant existing tests: `TestDBObfuscationUsesDBSystemForAttributes` (`:1829`),
`TestDBObfuscationUsesDBSystemNameForAttributes` (`:1879`),
`TestDBObfuscationAttributesWithoutDBSystemDoesNothing` (`:1913`),
`TestLogAttributesObfuscationWithoutDBSystem` (`:1961`),
`TestMetricAttributesDBObfuscationWithSystem` (`:1991`),
`TestMetricAttributesDBObfuscationWithoutSystem` (`:2024`).

### New test to add — proves the disabled path is inert even when `db.system` is present

Append to `processor_test.go`. With no `DBSanitizer` configured, an attribute set
that contains `db.system` and a SQL-looking value must pass through untouched:

```go
func TestNoDBSanitizerLeavesDBSystemAttrsUntouched(t *testing.T) {
	// DBSanitizer not configured -> HasObfuscators() == false.
	cfg := &Config{AllowAllKeys: true}
	proc, err := newRedaction(context.Background(), cfg, zaptest.NewLogger(t))
	require.NoError(t, err)

	logs := plog.NewLogs()
	lr := logs.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	attrs := lr.Attributes()
	attrs.PutStr("db.system", "postgresql")
	attrs.PutStr("db.statement", "SELECT * FROM users WHERE id = 42")

	_, err = proc.processLogs(context.Background(), logs)
	require.NoError(t, err)

	got, ok := lr.Attributes().Get("db.statement")
	require.True(t, ok)
	assert.Equal(t, "SELECT * FROM users WHERE id = 42", got.Str(),
		"DB obfuscation must not run when no DB sanitizer is configured")
	sys, ok := lr.Attributes().Get("db.system")
	require.True(t, ok)
	assert.Equal(t, "postgresql", sys.Str())
}
```

Run it:

```bash
go test -run TestNoDBSanitizerLeavesDBSystemAttrsUntouched ./...
```

## Acceptance criteria

- [ ] The guard at `processor.go:332` reads `if s.dbObfuscator.HasObfuscators() {`.
- [ ] `go build ./... && go vet ./...` clean; full suite + `-race` green.
- [ ] All `TestDBObfuscation*` / metric / log DB tests still pass unchanged.
- [ ] `TestNoDBSanitizerLeavesDBSystemAttrsUntouched` passes.
- [ ] `benchstat` shows reduced allocs/lookups per record for `BenchmarkRedactLogsBlockedValues` (its config has no DB sanitizer, so the two `Get` lookups per attribute set disappear).

## Coordination with other plans

- Self-contained, single-line change in `processAttrs` near the top (`:332`),
  away from the mask/bookkeeping lines touched by Plans 01/05. Lowest-risk; land first.
