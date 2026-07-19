package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewJSONLoggerEmitsJSON(t *testing.T) {
	var buffer bytes.Buffer
	logger := NewJSONLogger(&buffer, slog.LevelInfo)

	logger.Info("hello", "component", "test")

	entry := decodeLogEntry(t, buffer.Bytes())
	if entry["level"] != "INFO" {
		t.Fatalf("level = %v, want INFO", entry["level"])
	}
	if entry["msg"] != "hello" {
		t.Fatalf("msg = %v, want hello", entry["msg"])
	}
	if entry["component"] != "test" {
		t.Fatalf("component = %v, want test", entry["component"])
	}
}

func TestPackageLevelHelpersUseCurrentLogger(t *testing.T) {
	var buffer bytes.Buffer
	old := Logger()
	SetLogger(NewJSONLogger(&buffer, slog.LevelInfo))
	t.Cleanup(func() {
		SetLogger(old)
	})

	Info(context.TODO(), "helper event", "component", "sdk")

	entry := decodeLogEntry(t, buffer.Bytes())
	if entry["msg"] != "helper event" {
		t.Fatalf("msg = %v, want helper event", entry["msg"])
	}
	if entry["component"] != "sdk" {
		t.Fatalf("component = %v, want sdk", entry["component"])
	}
}

func TestNewLoggerSupportsTextFormat(t *testing.T) {
	var buffer bytes.Buffer
	logger, err := NewLogger(&buffer, Config{
		Level:  "info",
		Format: "text",
	})
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}

	logger.Info("hello", "component", "test")

	output := buffer.String()
	if !strings.Contains(output, `level=INFO`) ||
		!strings.Contains(output, `msg=hello`) ||
		!strings.Contains(output, `component=test`) {
		t.Fatalf("text output = %q, want level/msg/component", output)
	}
}

func TestConfigureAppliesExplicitConfigAndLeavesZeroConfigAlone(t *testing.T) {
	var buffer bytes.Buffer
	old := Logger()
	SetLogger(NewJSONLogger(&buffer, slog.LevelDebug))
	t.Cleanup(func() {
		SetLogger(old)
	})

	if err := Configure(Config{}, &buffer); err != nil {
		t.Fatalf("Configure(zero) error = %v", err)
	}
	Debug(context.TODO(), "debug before explicit config")
	if !strings.Contains(buffer.String(), "debug before explicit config") {
		t.Fatalf("zero config changed logger or level, output = %q", buffer.String())
	}

	buffer.Reset()
	if err := Configure(Config{Level: "info", Format: "json"}, &buffer); err != nil {
		t.Fatalf("Configure(explicit) error = %v", err)
	}
	Debug(context.TODO(), "debug after explicit config")
	if buffer.Len() != 0 {
		t.Fatalf("debug log produced output after info config: %s", buffer.String())
	}
}

func TestConfigureAcceptsCustomLogger(t *testing.T) {
	var buffer bytes.Buffer
	old := Logger()
	t.Cleanup(func() {
		SetLogger(old)
	})

	custom := NewJSONLogger(&buffer, slog.LevelInfo)
	if err := Configure(Config{Logger: custom}, nil); err != nil {
		t.Fatalf("Configure(custom logger) error = %v", err)
	}

	Info(context.TODO(), "custom logger event")

	entry := decodeLogEntry(t, buffer.Bytes())
	if entry["msg"] != "custom logger event" {
		t.Fatalf("msg = %v, want custom logger event", entry["msg"])
	}
}

func TestResolveRejectsUnsupportedLoggingConfig(t *testing.T) {
	if _, err := Resolve(DefaultConfig(), Override{Level: ptr("trace")}); err == nil {
		t.Fatal("Resolve() error = nil, want invalid level error")
	}
	if _, err := Resolve(DefaultConfig(), Override{Format: ptr("pretty")}); err == nil {
		t.Fatal("Resolve() error = nil, want invalid format error")
	}
}

func TestSDKProductionCodeUsesSharedLogging(t *testing.T) {
	pkgRoot := filepath.Clean("../..")
	disallowedEverywhere := map[string]bool{
		"log": true,
	}
	bridgeOnly := map[string]bool{
		"log/slog":                   true,
		"github.com/apex/log":        true,
		"github.com/sirupsen/logrus": true,
	}

	err := filepath.WalkDir(pkgRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path == filepath.Join(pkgRoot, "shared", "logging") ||
				path == filepath.Join(pkgRoot, "shared", "testutil") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		imports := parseImports(t, path)
		for _, importPath := range imports {
			if disallowedEverywhere[importPath] {
				t.Fatalf("SDK production file %s imports %q directly; use github.com/donglin-wang/chamber/pkg/shared/logging", path, importPath)
			}
			if bridgeOnly[importPath] && !isLoggingBridgeFile(path, imports) {
				t.Fatalf("SDK production file %s imports bridge logger %q outside an implementation-owned logging bridge", path, importPath)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk SDK packages: %v", err)
	}
}

func TestSDKProductionCodeDoesNotConfigureProcessGlobalLogging(t *testing.T) {
	pkgRoot := filepath.Clean("../..")

	err := filepath.WalkDir(pkgRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path == filepath.Join(pkgRoot, "shared", "logging") ||
				path == filepath.Join(pkgRoot, "shared", "testutil") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		fileset := token.NewFileSet()
		file, err := parser.ParseFile(fileset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		loggingNames := loggingImportNames(file)
		if len(loggingNames) == 0 {
			return nil
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || !isLoggingConfigureCall(call, loggingNames) {
				return true
			}
			t.Fatalf("SDK production file %s calls logging.Configure; constructors should use per-instance loggers", path)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk SDK packages: %v", err)
	}
}

func TestSDKProductionCodeDoesNotExposePreparationMethods(t *testing.T) {
	pkgRoot := filepath.Clean("../..")

	err := filepath.WalkDir(pkgRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path == filepath.Join(pkgRoot, "shared", "testutil") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		fileset := token.NewFileSet()
		file, err := parser.ParseFile(fileset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || !isPreparationName(function.Name.Name) {
				continue
			}
			t.Fatalf("SDK production file %s declares %s; initialization belongs in New", path, function.Name.Name)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk SDK packages: %v", err)
	}
}

func decodeLogEntry(t *testing.T, data []byte) map[string]any {
	t.Helper()

	var entry map[string]any
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("decode log entry %q: %v", string(data), err)
	}
	return entry
}

func isLoggingBridgeFile(path string, imports []string) bool {
	if filepath.Base(path) != "logging_bridge.go" {
		return false
	}
	for _, importPath := range imports {
		if importPath == "github.com/donglin-wang/chamber/pkg/shared/logging" {
			return true
		}
	}
	return false
}

func parseImports(t *testing.T, path string) []string {
	t.Helper()

	fileset := token.NewFileSet()
	file, err := parser.ParseFile(fileset, path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	imports := make([]string, 0, len(file.Imports))
	for _, importSpec := range file.Imports {
		imports = append(imports, strings.Trim(importSpec.Path.Value, `"`))
	}
	return imports
}

func loggingImportNames(file *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, importSpec := range file.Imports {
		importPath := strings.Trim(importSpec.Path.Value, `"`)
		if importPath != "github.com/donglin-wang/chamber/pkg/shared/logging" {
			continue
		}
		if importSpec.Name != nil {
			names[importSpec.Name.Name] = true
			continue
		}
		names["logging"] = true
	}
	return names
}

func isLoggingConfigureCall(call *ast.CallExpr, loggingNames map[string]bool) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Configure" {
		return false
	}
	identifier, ok := selector.X.(*ast.Ident)
	return ok && loggingNames[identifier.Name]
}

func isPreparationName(name string) bool {
	for _, prefix := range []string{"Ensure", "ensure", "Prepare", "prepare"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func insideConstructor(file *ast.File, pos token.Pos) bool {
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil || function.Name.Name != "New" {
			continue
		}
		if function.Body.Pos() <= pos && pos <= function.Body.End() {
			return true
		}
	}
	return false
}

func ptr(value string) *string {
	return &value
}
