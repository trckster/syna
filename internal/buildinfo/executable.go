package buildinfo

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"sync"
)

func ExecutableID() (string, error) {
	return executableID()
}

var executableID = sync.OnceValues(func() (string, error) {
	// /proc/self/exe identifies the running image even after its path is replaced by an upgrade.
	file, err := os.Open("/proc/self/exe")
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
})
