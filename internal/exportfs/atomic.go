package exportfs

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

func AtomicFile(dir, name string, write func(*os.File) error) error {
	f, err := os.CreateTemp(dir, ".write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = write(f)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, name))
}
func WriteJSON(dir, name string, v any) error {
	return AtomicFile(dir, name, func(f *os.File) error { return json.NewEncoder(f).Encode(v) })
}
