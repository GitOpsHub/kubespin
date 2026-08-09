package fleetinfra

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"time"
)

// handlerName is what provided.al2023 executes. The zip must contain exactly
// this file at its root.
const handlerName = "bootstrap"

// zipEpoch is a fixed timestamp for every archive entry.
//
// Determinism is load-bearing, not cosmetic: convergence is decided by comparing
// the archive's SHA-256 against the deployed function's CodeSha256, so an
// archive carrying the current time would hash differently on every run and
// report drift forever.
var zipEpoch = time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)

// PackageLambda reads a compiled handler binary and returns a deployable zip.
func PackageLambda(binaryPath string) ([]byte, error) {
	f, err := os.Open(binaryPath) //nolint:gosec // operator-supplied path, by design
	if err != nil {
		return nil, fmt.Errorf("opening lambda binary %s: %w", binaryPath, err)
	}
	defer f.Close() //nolint:errcheck // read-only handle

	return packageBinary(f)
}

func packageBinary(r io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)

	header := &zip.FileHeader{
		Name:     handlerName,
		Method:   zip.Deflate,
		Modified: zipEpoch,
	}
	header.SetMode(0o755) // the runtime must be able to execute it

	entry, err := w.CreateHeader(header)
	if err != nil {
		return nil, fmt.Errorf("creating zip entry: %w", err)
	}
	if _, err := io.Copy(entry, r); err != nil {
		return nil, fmt.Errorf("writing lambda binary into zip: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("finalising zip: %w", err)
	}

	return buf.Bytes(), nil
}

// codeSHA256 renders a zip's hash the way Lambda reports CodeSha256, so the two
// can be compared directly.
func codeSHA256(zipBytes []byte) string {
	sum := sha256.Sum256(zipBytes)
	return base64.StdEncoding.EncodeToString(sum[:])
}
