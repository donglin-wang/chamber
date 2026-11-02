package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
)

var revision = "development"

func main() {
	if err := runMain(); err != nil {
		slog.Error("CI stopped", "error", err)
		os.Exit(1)
	}
}

func runMain() error {
	configFlag := flag.String("config", "", "path to CI JSON config")
	check := flag.Bool("check", false, "check candidate startup and serve a temporary health endpoint")
	socketFD := flag.Int("listener-fd", 0, "internal: socket retained across an update")
	lockFD := flag.Int("lock-fd", 0, "internal: state lock retained across an update")
	flag.Parse()
	if runtime.GOOS != "linux" {
		return fmt.Errorf("unsupported_host: CI execution requires Linux")
	}
	if os.Geteuid() == 0 {
		return fmt.Errorf("CI must run as an unprivileged user")
	}
	if *configFlag == "" {
		return fmt.Errorf("-config is required")
	}
	configPath, err := filepath.Abs(*configFlag)
	if err != nil {
		return err
	}
	c, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	for _, name := range []string{"git", "go"} {
		if _, err := exec.LookPath(name); err != nil {
			return err
		}
	}
	shutdown, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if *check {
		return checkStartupHealth(shutdown, c)
	}
	for _, directory := range []string{c.Root, c.WorkRoot, filepath.Join(c.Root, "logs")} {
		if err := hostfs.CreatePrivateDirectory(directory); err != nil {
			return err
		}
	}
	// Retaining this open file across exec keeps the same flock continuously held.
	var lock *os.File
	if *lockFD != 0 {
		if *lockFD < 3 {
			return fmt.Errorf("invalid inherited lock descriptor")
		}
		syscall.CloseOnExec(*lockFD)
		lock = os.NewFile(uintptr(*lockFD), "CI state lock")
		inherited, err := lock.Stat()
		expected, pathErr := os.Stat(filepath.Join(c.Root, ".lock"))
		if err != nil || pathErr != nil || !os.SameFile(inherited, expected) {
			lock.Close()
			return fmt.Errorf("inherited lock does not match configured root")
		}
	} else {
		lock, err = os.OpenFile(filepath.Join(c.Root, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			return err
		}
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("CI state directory is already in use: %w", err)
	}
	passcode, err := updatePasscode(c.Root)
	if err != nil {
		return err
	}
	// Keep one socket file open while HTTP servers stop and restart. The kernel
	// queues new connections on it during exec; the port is never released.
	var socket *os.File
	if *socketFD != 0 {
		if *socketFD < 3 {
			return fmt.Errorf("invalid inherited listener descriptor")
		}
		syscall.CloseOnExec(*socketFD)
		socket = os.NewFile(uintptr(*socketFD), "CI listener")
	} else {
		address, err := net.ResolveTCPAddr("tcp", c.Listen)
		if err != nil {
			return err
		}
		listener, err := net.ListenTCP("tcp", address)
		if err != nil {
			return err
		}
		socket, err = listener.File()
		listener.Close()
		if err != nil {
			return err
		}
	}
	defer socket.Close()
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	for {
		store, err := openSQLiteStore(filepath.Join(c.Root, "jobs.sqlite"))
		if err != nil {
			return err
		}
		s, err := newServer(c, passcode, store)
		if err != nil {
			store.Close()
			return err
		}
		s.buildUpdate = func(ctx context.Context) (string, error) {
			candidate, err := buildUpdate(ctx, c)
			if err == nil {
				_, err = checkCandidate(ctx, candidate, configPath)
			}
			return candidate, err
		}
		listener, err := net.FileListener(socket)
		if err != nil {
			store.Close()
			return err
		}
		candidate, err := s.serve(shutdown, listener)
		if errors.Is(err, errShutdownTimeout) {
			return err
		} // Exit rather than close a DB still in use by stuck tasks.
		err = errors.Join(err, store.Close())
		if err != nil || shutdown.Err() != nil || candidate == "" {
			os.Remove(candidate)
			return err
		}
		err = replaceProcess(candidate, executable, configPath, socket, lock, syscall.Exec)
		os.Remove(candidate)
		slog.Error("update failed; resuming current server", "error", err)
	}
}
