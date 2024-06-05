package startupmonitor

import (
	"embed"
)

//go:embed assets/*
var f embed.FS

// asset reads and returns the content of the named file.
func asset(name string) ([]byte, error) {
	return f.ReadFile(name)
}

// mustAsset reads and returns the content of the named file or panics
// if something went wrong.
func mustAsset(name string) []byte {
	data, err := f.ReadFile(name)
	if err != nil {
		panic(err)
	}

	return data
}
