package metadata

import (
	"testing"
)

func TestContainerValidTransition(t *testing.T) {
	tests := map[StateTransition[ContainerState]]bool{
		{ContainerCreating, ContainerStarting}: true,
		{ContainerCreating, ContainerFailed}:   true,
		{ContainerStarting, ContainerRunning}:  true,
		{ContainerStarting, ContainerFailed}:   true,
		{ContainerStarting, ContainerExited}:   true,
		{ContainerRunning, ContainerExited}:    true,
		{ContainerRunning, ContainerFailed}:    true,

		{ContainerCreating, ContainerRunning}:       false,
		{ContainerRunning, ContainerStarting}:       false,
		{ContainerExited, ContainerRunning}:         false,
		{ContainerFailed, ContainerRunning}:         false,
		{ContainerRunning, ContainerRunning}:        false,
		{ContainerState("weird"), ContainerRunning}: false,
	}

	for transition, expected := range tests {
		result := isContainerTransitionValid(transition.From, transition.To)
		if result != expected {
			t.Fatalf("ValidContainerTransition(%q, %q) returned %v, expected %v", transition.From, transition.To, result, expected)
		}
	}
}

func TestOperationValidTransition(t *testing.T) {
	tests := map[StateTransition[OperationState]]bool{
		{OperationRunning, OperationSucceeded}: true,
		{OperationRunning, OperationFailed}:    true,
		{OperationRunning, OperationAborted}:   true,

		{OperationSucceeded, OperationRunning}:        false,
		{OperationSucceeded, OperationFailed}:         false,
		{OperationSucceeded, OperationAborted}:        false,
		{OperationSucceeded, OperationSucceeded}:      false,
		{OperationFailed, OperationRunning}:           false,
		{OperationFailed, OperationSucceeded}:         false,
		{OperationFailed, OperationAborted}:           false,
		{OperationFailed, OperationFailed}:            false,
		{OperationAborted, OperationRunning}:          false,
		{OperationAborted, OperationSucceeded}:        false,
		{OperationAborted, OperationFailed}:           false,
		{OperationAborted, OperationAborted}:          false,
		{OperationState("weird"), OperationRunning}:   false,
		{OperationRunning, OperationState("weird")}:   false,
		{OperationState("weird"), OperationSucceeded}: false,
	}

	for transition, expected := range tests {
		result := isOperationTransitionValid(transition.From, transition.To)
		if result != expected {
			t.Fatalf("ValidOperationTransition(%q, %q) returned %v, expected %v", transition.From, transition.To, result, expected)
		}
	}
}

func TestStoreContract(t *testing.T) {
	tests := map[string]func(t *testing.T) Store{
		// Add implementations as you build them.
	}
	for name, newStore := range tests {
		t.Run(name, func(t *testing.T) {
			store := newStore(t)
			t.Cleanup(func() { _ = store.Close() })
			// TODO: shared behavior assertions.
		})
	}
}
