// Package shared defines the public runtime SDK contract shared by runtime
// constructors and implementations.
//
// It owns runtime configuration, descriptors, statuses, log streams, and the
// Runtime and Container interfaces. SDK callers must call
// Container.Wait for every successful Run call, and remain responsible for
// cleaning caller-owned bundle storage and logs.
package shared
