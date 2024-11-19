package bindata

import "embed"

//go:embed etcd/*
var f embed.FS

// Asset reads and returns the content of the named file.
func Asset(name string) ([]byte, error) {
	return f.ReadFile(name)
}

// MustAsset reads and returns the content of the named file or panics
// if something went wrong.
func MustAsset(name string) []byte {
	data, err := f.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return data
}
