// Package shared defines the public runtime SDK contract shared by runtime
// constructors and implementations.
//
// It owns runtime configuration, descriptors, statuses, supported signals, log
// streams, and the Runtime and Process interfaces. SDK callers must call
// Process.Wait for every successful Run call, and remain responsible for
// cleaning caller-owned bundle storage and logs.
package shared
