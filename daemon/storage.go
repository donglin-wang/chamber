package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	daemonconfig "github.com/donglin-wang/chamber/daemon/config"
)

func runStorage(args []string, getenv func(string) string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("expected storage command")
	}

	switch args[0] {
	case "remove":
		return runStorageRemove(args[1:], getenv, stdout)
	default:
		return fmt.Errorf("unknown storage command %q", args[0])
	}
}

func runStorageRemove(args []string, getenv func(string) string, stdout io.Writer) error {
	var yes bool

	fs := flag.NewFlagSet("chamberd storage remove", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&yes, "yes", false, "confirm removal of Chamber storage")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if !yes {
		return fmt.Errorf("refusing to remove Chamber storage without --yes")
	}

	root := daemonconfig.DefaultRoot(getenv)
	if err := validateStorageRoot(root); err != nil {
		return err
	}

	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stdout, "No Chamber storage found at %s\n", root)
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect Chamber storage: %w", err)
	}

	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("remove Chamber storage: %w", err)
	}

	fmt.Fprintf(stdout, "Removed Chamber storage at %s\n", root)
	return nil
}

func validateStorageRoot(root string) error {
	if root == "" {
		return fmt.Errorf("storage root is required")
	}
	if !filepath.IsAbs(root) {
		return fmt.Errorf("storage root %q must be absolute", root)
	}
	if filepath.Base(filepath.Clean(root)) != "chamber" {
		return fmt.Errorf("refusing to remove storage root %q because it does not end in chamber", root)
	}
	return nil
}
