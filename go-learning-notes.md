# Go Learning Notes

These notes collect a few Go patterns that show up in Chamber's metadata code.
The goal is not to memorize clever syntax. The goal is to recognize the small
language features behind the patterns.

## Error wrapping and unwrapping

The built-in Go error interface is very small:

```go
type error interface {
	Error() string
}
```

That means a value can be used as an `error` if it has an `Error() string`
method. It does not mean an error value can only have that method. Concrete error
types can have extra methods too.

The `errors` package uses a convention: if an error value has an `Unwrap` method,
then it can point to one or more underlying errors.

Single wrapped error:

```go
interface {
	Unwrap() error
}
```

Multiple wrapped errors:

```go
interface {
	Unwrap() []error
}
```

These are anonymous interface types. They do not need names because Go lets you
describe the required method shape inline.

### `%w` versus `%v`

`fmt.Errorf` only wraps an error when the format string uses `%w`.

```go
err := fmt.Errorf("metadata failed: %w", metadata.ErrMetadataFailed)

errors.Is(err, metadata.ErrMetadataFailed) // true
```

Using `%v` only copies the error text into the message:

```go
err := fmt.Errorf("metadata failed: %v", metadata.ErrMetadataFailed)

errors.Is(err, metadata.ErrMetadataFailed) // false
```

In `internal/metadata/etcd/store.go`, this helper wraps the stable Chamber error:

```go
func metadataFailure(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{metadata.ErrMetadataFailed}, args...)...)
}
```

That means callers can ask:

```go
if errors.Is(err, metadata.ErrMetadataFailed) {
	// The metadata backend failed.
}
```

But `mapEtcdError` passes the etcd error with `%v`:

```go
func mapEtcdError(err error) error {
	if err == nil {
		return nil
	}
	return metadataFailure("%v", err)
}
```

So the etcd error's message appears in the final error string, but the actual
etcd error is not part of the unwrap chain. The caller can identify
`metadata.ErrMetadataFailed`, not the specific etcd error.

That is a deliberate boundary: outside this adapter, Chamber code should care
that metadata failed, not that etcd was the concrete tool.

### Multiple `%w` values

If `fmt.Errorf` has more than one `%w`, the returned concrete error implements
`Unwrap() []error`.

```go
err := fmt.Errorf("write failed: %w; close failed: %w", writeErr, closeErr)

errors.Is(err, writeErr) // true
errors.Is(err, closeErr) // true
```

Only `%w` wraps. This wraps only `writeErr`:

```go
err := fmt.Errorf("write failed: %w; close failed: %v", writeErr, closeErr)
```

One small gotcha: `errors.Unwrap(err)` handles only the single-error form,
`Unwrap() error`. For normal code, prefer `errors.Is` and `errors.As`; they know
how to walk both the single-error and multi-error forms.

### `errors.Is` versus `errors.As`

Use `errors.Is` when you are checking for a known sentinel value:

```go
if errors.Is(err, metadata.ErrNotFound) {
	// Missing record.
}
```

Use `errors.As` when you want to recover a specific error type:

```go
var pathErr *fs.PathError
if errors.As(err, &pathErr) {
	fmt.Println(pathErr.Path)
}
```

Plain equality only checks the top-level error:

```go
err == metadata.ErrNotFound
```

That misses wrapped errors. Prefer `errors.Is` for exported sentinel errors.

## Compile-time interface checks

This line appears at the bottom of the etcd store:

```go
var _ metadata.Store = (*Store)(nil)
```

It means: make the compiler prove that `*Store` implements `metadata.Store`.

Break it apart:

```go
(*Store)(nil)
```

This creates a nil pointer value whose type is `*Store`.

Then this assignment is checked by the compiler:

```go
var _ metadata.Store = (*Store)(nil)
```

If `*Store` does not have every method required by `metadata.Store`, the package
will not compile.

The `_` is the blank identifier. It means "I do not need to keep this value."
The line is there for type checking, not runtime behavior.

### Why `*Store` instead of `Store`?

The etcd store methods use pointer receivers:

```go
func (s *Store) PutImage(...)
func (s *Store) GetImage(...)
func (s *Store) Close()
```

So the pointer type, `*Store`, is the type that satisfies the interface.

A first-time Go trap: a value type and a pointer type have related, but not
identical, method sets. If an interface requires methods that are defined only
with pointer receivers, then the pointer type implements the interface.

This is also a good reason to put the check near the implementation. When the
interface changes, the adapter fails loudly at compile time.

## Starting goroutines together with a channel

The metadata store contract uses this helper:

```go
func runConcurrently(count int, fn func(worker int) error) []error {
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, count)

	wg.Add(count)
	for i := range count {
		go func(worker int) {
			defer wg.Done()
			<-start
			errs[worker] = fn(worker)
		}(i)
	}

	close(start)
	wg.Wait()
	return errs
}
```

The purpose is to make all workers wait at the same starting line. This makes the
test better at finding concurrency bugs.

### The pieces

`sync.WaitGroup` lets the main goroutine wait until all worker goroutines finish.

```go
var wg sync.WaitGroup
wg.Add(count)
...
wg.Wait()
```

Each worker calls `Done` when it exits:

```go
defer wg.Done()
```

Using `defer` matters because `Done` still runs if `fn(worker)` returns early.

The channel is a broadcast signal:

```go
start := make(chan struct{})
```

Each goroutine waits here:

```go
<-start
```

Receiving from an open channel blocks until a value arrives or the channel is
closed. This code never sends a value. Instead, it closes the channel:

```go
close(start)
```

Closing a channel wakes every goroutine waiting to receive from it. That gives
the test a burst of concurrent calls.

`chan struct{}` is common for pure signals because `struct{}` carries no data.
The channel is not saying "here is a value"; it is saying "go now."

### Why pass `i` as an argument?

The goroutine is written like this:

```go
go func(worker int) {
	...
}(i)
```

This gives each goroutine its own copy of the worker number.

Historically, Go learners often hit a bug where a goroutine accidentally captured
the loop variable itself and every goroutine saw the same final value. Modern Go
fixed the most common range-loop version of that problem, but passing the value
explicitly is still clear and works in all loop shapes.

### Why is writing to `errs[worker]` okay?

The slice is allocated before the goroutines start:

```go
errs := make([]error, count)
```

Each worker writes to a distinct index:

```go
errs[worker] = fn(worker)
```

The main goroutine does not read the slice until after `wg.Wait()`. That makes
the pattern safe: no append, no resizing, no two workers writing the same slot,
and no reads until all writes are done.

If workers appended to the same slice, that would be unsafe without a mutex or
channel, because append can modify shared slice metadata and the backing array.

### What the test is proving

This helper does not guarantee perfect simultaneous execution. The scheduler
still chooses exactly when each goroutine runs.

It does create contention: many goroutines are released at once and race to call
the same store operation. That is enough to test behavior like:

```go
assertOneSuccess(t, errs, metadata.ErrStateConflict)
```

For the metadata store, the desired outcome is often:

- exactly one goroutine succeeds;
- the others receive a stable conflict error;
- no update is silently lost.

## Other Go patterns worth noticing

### Interfaces are satisfied implicitly

Types do not declare that they implement an interface. They implement it by
having the required methods.

```go
type Store interface {
	GetOperation(ctx context.Context, id string) (Operation, error)
}
```

Any type with that method satisfies the interface. This keeps adapters small, but
it also means compile-time checks like this are helpful:

```go
var _ metadata.Store = (*Store)(nil)
```

### Small interfaces belong near callers

In Go, interfaces often describe what a caller needs, not what a concrete type
is. Chamber's daemon should depend on `metadata.Store`, while the etcd package
depends on etcd details.

That keeps the rest of the program from learning about etcd revisions, keys, and
transactions.

### `context.Context` is not storage

`context.Context` carries cancellation, deadlines, and request-scoped values. It
is not a place to hide durable state.

In the metadata tests, trace fields are stored explicitly in records. That is
important: if Chamber restarts, the durable record still says which operation and
trace it belonged to.

### Sentinel errors should be stable

Values like these are sentinel errors:

```go
var (
	ErrNotFound      = errors.New("metadata: not found")
	ErrAlreadyExists = errors.New("metadata: already exists")
)
```

They are useful when callers need a stable decision:

```go
if errors.Is(err, metadata.ErrNotFound) {
	// create a 404 response, retry, or choose another path
}
```

Do not create a fresh `errors.New("metadata: not found")` at the call site and
expect it to match. That would be a different value.

### Copy pointer fields when returning stored records

Some metadata records contain pointers:

```go
FinishedAt *time.Time
ExitCode   *int
```

If a store returns a record with a pointer into its internal storage, the caller
could mutate the store accidentally:

```go
updated.ExitCode = nil
```

or:

```go
*updated.ExitCode = 0
```

That is why the memory and etcd stores clone pointer fields before returning
records. This is not about performance first; it is about ownership.

### The zero value is often useful

Go code often tries to make the zero value meaningful. For example:

```go
var wg sync.WaitGroup
```

That is ready to use. No constructor required.

But maps and channels need `make` before use:

```go
images := make(map[string]metadata.Image)
start := make(chan struct{})
```

Writing to a nil map panics. Receiving from a nil channel blocks forever.

### A nil interface is not the same as an interface holding a nil pointer

This is one of Go's stranger first-time traps.

An interface value has two parts: a concrete type and a concrete value.

This is nil:

```go
var err error
err == nil // true
```

This is not nil:

```go
var pathErr *fs.PathError = nil
var err error = pathErr

err == nil // false
```

The interface has a concrete type, `*fs.PathError`, even though the concrete
pointer value is nil.

### `t.Helper()` makes failures point to the caller

Test helpers often start with:

```go
t.Helper()
```

When the helper fails, Go reports the line in the test that called the helper,
not only the line inside the helper. That makes test failures much easier to read.

### Table-driven tests are ordinary data plus a loop

Go tests often put cases in a slice or map:

```go
tests := map[metadata.StateTransition[metadata.OperationState]]bool{
	{From: metadata.OperationRunning, To: metadata.OperationSucceeded}: true,
	{From: metadata.OperationSucceeded, To: metadata.OperationRunning}: false,
}
```

Then the test loops over the cases. This makes it easy to add cases without
copying the whole test body.

Use `t.Run` when each case deserves its own name in test output.

### Run the race detector when testing shared state

For code with goroutines, locks, or shared memory, use:

```sh
go test -race ./...
```

The race detector cannot prove every concurrency property. It can catch a very
important class of bugs: two goroutines touching the same memory at the same time
when at least one is writing and there is no proper synchronization.

### Channels coordinate; mutexes protect

A useful first rule:

- use a channel to signal or hand off work;
- use a mutex to protect shared data.

The `runConcurrently` helper uses a channel to signal "start now."

The memory store uses a mutex to protect maps:

```go
s.mu.Lock()
defer s.mu.Unlock()
```

Both are basic Go tools. They solve different problems.

### Close channels from the sender side

The goroutine or function that sends values should normally be the one that
closes the channel. Closing tells receivers: no more values are coming.

Do not close a channel just because you are done receiving. Sending on a closed
channel panics, and closing a closed channel panics.

In `runConcurrently`, the main goroutine owns the `start` signal, and it is the
only one that closes it. That is why the pattern is simple and safe.

### `defer` is for cleanup at function exit

This pattern is everywhere in Go:

```go
file, err := os.Open(path)
if err != nil {
	return err
}
defer file.Close()
```

In concurrency helpers, the same idea appears as:

```go
defer wg.Done()
```

The cleanup is placed next to the successful setup. That makes it harder to
forget later.

### Prefer explicit ownership in concurrent code

When reading Go concurrency code, ask:

- Who owns this value?
- Can more than one goroutine write it?
- What makes the main goroutine wait?
- What happens if the context is canceled?
- What happens if one worker returns an error?

Those questions are more useful than trying to memorize every concurrency
primitive at once.

