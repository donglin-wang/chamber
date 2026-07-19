package lima

import (
	"bytes"
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	chamberMachine "github.com/donglin-wang/chamber/pkg/machine"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
)

func TestNewCreatesAndStartsMissingMachine(t *testing.T) {
	root := filepath.Join(t.TempDir(), "machines")
	runner := &fakeRunner{
		results: []commandResult{
			{Stdout: []byte("[]")},
			{},
			{},
		},
	}

	vm, err := New(context.Background(), chamberMachine.Config{
		Root:  root,
		Name:  "chamber-ci",
		Spec:  chamberMachine.Spec{OS: "linux", Arch: "arm64"},
		Start: true,
	}, localfs.NewDirectoryManager(), withCommandRunner(runner))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if vm.Descriptor().Status != chamberMachine.StatusRunning {
		t.Fatalf("Descriptor().Status = %q, want running", vm.Descriptor().Status)
	}

	specPath := filepath.Join(root, defaultSpecDirectory, "chamber-ci.yaml")
	wantArgs := [][]string{
		{"list", "--format", "json", "chamber-ci"},
		{"create", "--name", "chamber-ci", specPath, "--tty=false"},
		{"start", "chamber-ci", "--timeout", defaultStartTimeout.String(), "--tty=false"},
	}
	assertCalls(t, runner.calls, wantArgs, filepath.Join(root, defaultLimaHome))
}

func TestNewTreatsUnmatchedInstanceAsMissing(t *testing.T) {
	root := filepath.Join(t.TempDir(), "machines")
	runner := &fakeRunner{
		results: []commandResult{
			{
				ExitCode: 1,
				Stderr:   []byte("No instance matching chamber-ci found.\nunmatched instances\n"),
			},
			{},
		},
	}

	_, err := New(context.Background(), chamberMachine.Config{
		Root: root,
		Name: "chamber-ci",
		Spec: chamberMachine.Spec{OS: "linux"},
	}, localfs.NewDirectoryManager(), withCommandRunner(runner))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	specPath := filepath.Join(root, defaultSpecDirectory, "chamber-ci.yaml")
	wantArgs := [][]string{
		{"list", "--format", "json", "chamber-ci"},
		{"create", "--name", "chamber-ci", specPath, "--tty=false"},
	}
	assertCalls(t, runner.calls, wantArgs, filepath.Join(root, defaultLimaHome))
}

func TestNewStartsStoppedMachineWithoutCreate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "machines")
	runner := &fakeRunner{
		results: []commandResult{
			{Stdout: []byte(`{"name":"chamber-ci","status":"Stopped","dir":"/tmp/lima/chamber-ci","os":"Linux","arch":"aarch64","cpus":4}`)},
			{},
		},
	}

	vm, err := New(context.Background(), chamberMachine.Config{
		Root:  root,
		Name:  "chamber-ci",
		Spec:  chamberMachine.Spec{OS: "linux", Arch: "arm64"},
		Start: true,
	}, localfs.NewDirectoryManager(), withCommandRunner(runner))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if vm.Descriptor().Directory != "/tmp/lima/chamber-ci" {
		t.Fatalf("Descriptor().Directory = %q, want Lima dir", vm.Descriptor().Directory)
	}
	wantArgs := [][]string{
		{"list", "--format", "json", "chamber-ci"},
		{"start", "chamber-ci", "--timeout", defaultStartTimeout.String(), "--tty=false"},
	}
	assertCalls(t, runner.calls, wantArgs, filepath.Join(root, defaultLimaHome))
}

func TestRunExecutesCommandInsideMachine(t *testing.T) {
	root := filepath.Join(t.TempDir(), "machines")
	runner := &fakeRunner{
		results: []commandResult{
			{Stdout: []byte(`{"name":"chamber-ci","status":"Running"}`)},
			{ExitCode: 7},
		},
	}
	vm, err := New(context.Background(), chamberMachine.Config{
		Root: root,
		Name: "chamber-ci",
		Spec: chamberMachine.Spec{OS: "linux"},
	}, localfs.NewDirectoryManager(), withCommandRunner(runner))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	var stdout bytes.Buffer
	result, err := vm.Run(context.Background(), chamberMachine.RunRequest{
		Args:    []string{"go", "test", "./pkg/..."},
		Workdir: "/workspace",
		Env:     []string{"GOCACHE=/gocache"},
		Stdout:  &stdout,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", result.ExitCode)
	}

	want := []string{"shell", "--workdir", "/workspace", "chamber-ci", "env", "GOCACHE=/gocache", "go", "test", "./pkg/..."}
	if !reflect.DeepEqual(runner.calls[1].Args, want) {
		t.Fatalf("Run args = %#v, want %#v", runner.calls[1].Args, want)
	}
	if runner.calls[1].Stdout != &stdout {
		t.Fatal("Run() did not pass stdout writer to command runner")
	}
}

func TestStopAndDeleteUseMachineName(t *testing.T) {
	root := filepath.Join(t.TempDir(), "machines")
	runner := &fakeRunner{
		results: []commandResult{
			{Stdout: []byte(`{"name":"chamber-ci","status":"Running"}`)},
			{},
			{},
		},
	}
	vm, err := New(context.Background(), chamberMachine.Config{
		Root: root,
		Name: "chamber-ci",
		Spec: chamberMachine.Spec{OS: "linux"},
	}, localfs.NewDirectoryManager(), withCommandRunner(runner))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := vm.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if vm.Descriptor().Status != chamberMachine.StatusStopped {
		t.Fatalf("Status after Stop = %q, want stopped", vm.Descriptor().Status)
	}
	if err := vm.Delete(context.Background()); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if vm.Descriptor().Status != chamberMachine.StatusMissing {
		t.Fatalf("Status after Delete = %q, want missing", vm.Descriptor().Status)
	}

	wantArgs := [][]string{
		{"list", "--format", "json", "chamber-ci"},
		{"stop", "chamber-ci"},
		{"delete", "--force", "chamber-ci", "--tty=false"},
	}
	assertCalls(t, runner.calls, wantArgs, filepath.Join(root, defaultLimaHome))
}

func TestParseListOutputAcceptsArrayOrObject(t *testing.T) {
	config := chamberMachine.Config{Name: "chamber-ci", Spec: chamberMachine.Spec{OS: "linux", Arch: "arm64"}}

	for _, data := range [][]byte{
		[]byte(`[{"name":"chamber-ci","status":"Running","dir":"/tmp/chamber","os":"Linux","arch":"aarch64","cpus":4,"memory":4294967296,"disk":107374182400}]`),
		[]byte(`{"name":"chamber-ci","status":"Running","dir":"/tmp/chamber","os":"Linux","arch":"aarch64","cpus":4,"memory":4294967296,"disk":107374182400}`),
	} {
		descriptor, err := parseListOutput(data, config)
		if err != nil {
			t.Fatalf("parseListOutput() error = %v", err)
		}
		if descriptor.Status != chamberMachine.StatusRunning || descriptor.Arch != "arm64" || descriptor.MemoryBytes != 4294967296 {
			t.Fatalf("descriptor = %#v, want parsed Lima fields", descriptor)
		}
	}
}

func TestParseListOutputTreatsEmptyAsMissing(t *testing.T) {
	descriptor, err := parseListOutput([]byte("[]"), chamberMachine.Config{Name: "chamber-ci"})
	if err != nil {
		t.Fatalf("parseListOutput() error = %v", err)
	}
	if descriptor.Status != chamberMachine.StatusMissing {
		t.Fatalf("Status = %q, want missing", descriptor.Status)
	}
}

type fakeRunner struct {
	results []commandResult
	calls   []commandInvocation
}

func (f *fakeRunner) Run(_ context.Context, invocation commandInvocation) (commandResult, error) {
	f.calls = append(f.calls, invocation)
	if len(f.results) == 0 {
		return commandResult{}, nil
	}
	result := f.results[0]
	f.results = f.results[1:]
	return result, nil
}

func assertCalls(t *testing.T, calls []commandInvocation, wantArgs [][]string, limaHome string) {
	t.Helper()

	if len(calls) != len(wantArgs) {
		t.Fatalf("calls = %d, want %d", len(calls), len(wantArgs))
	}
	for index, want := range wantArgs {
		if !reflect.DeepEqual(calls[index].Args, want) {
			t.Fatalf("call %d args = %#v, want %#v", index, calls[index].Args, want)
		}
		if len(calls[index].Env) == 0 || !strings.HasPrefix(calls[index].Env[0], "LIMA_HOME=") {
			t.Fatalf("call %d Env = %#v, want LIMA_HOME first", index, calls[index].Env)
		}
		if calls[index].Env[0] != "LIMA_HOME="+limaHome {
			t.Fatalf("call %d LIMA_HOME = %q, want %q", index, calls[index].Env[0], "LIMA_HOME="+limaHome)
		}
	}
}
