package metadata

import "path/filepath"

type Config struct {
	Root string
}

func DefaultConfig(rootPath string) Config {
	return Config{
		Root: filepath.Join(rootPath, "metadata"),
	}
}
