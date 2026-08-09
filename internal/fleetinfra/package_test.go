package fleetinfra

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestPackageLambda_Deterministic is what makes redeploy a no-op. CodeSha256 is
// compared against this archive's hash, so identical input bytes must produce
// identical archive bytes — otherwise every converge run would report drift.
func TestPackageLambda_Deterministic(t *testing.T) {
	binary := []byte("\x7fELF fake handler binary")

	first, err := packageBinary(bytes.NewReader(binary))
	if err != nil {
		t.Fatalf("packaging: %v", err)
	}
	second, err := packageBinary(bytes.NewReader(binary))
	if err != nil {
		t.Fatalf("packaging: %v", err)
	}

	if !bytes.Equal(first, second) {
		t.Error("packaging the same binary twice produced different archives")
	}
	if codeSHA256(first) != codeSHA256(second) {
		t.Error("packaging the same binary twice produced different hashes")
	}
}

func TestPackageLambda_DifferentInputDiffers(t *testing.T) {
	a, err := packageBinary(bytes.NewReader([]byte("handler v1")))
	if err != nil {
		t.Fatalf("packaging: %v", err)
	}
	b, err := packageBinary(bytes.NewReader([]byte("handler v2")))
	if err != nil {
		t.Fatalf("packaging: %v", err)
	}

	if codeSHA256(a) == codeSHA256(b) {
		t.Error("different handlers hashed identically")
	}
}

// provided.al2023 executes a file named bootstrap and needs it executable.
func TestPackageLambda_ArchiveLayout(t *testing.T) {
	payload := []byte("handler bytes")

	archive, err := packageBinary(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("packaging: %v", err)
	}

	r, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("reading archive: %v", err)
	}
	if len(r.File) != 1 {
		t.Fatalf("archive has %d entries, want exactly 1", len(r.File))
	}

	entry := r.File[0]
	if entry.Name != handlerName {
		t.Errorf("entry name = %q, want %q", entry.Name, handlerName)
	}
	if mode := entry.Mode().Perm(); mode&0o100 == 0 {
		t.Errorf("entry mode = %v, want owner-executable", mode)
	}

	f, err := entry.Open()
	if err != nil {
		t.Fatalf("opening entry: %v", err)
	}
	defer f.Close() //nolint:errcheck // test cleanup

	var got bytes.Buffer
	if _, err := got.ReadFrom(f); err != nil {
		t.Fatalf("reading entry: %v", err)
	}
	if !bytes.Equal(got.Bytes(), payload) {
		t.Error("archived bytes do not match the input binary")
	}
}

func TestPackageLambda_MissingFile(t *testing.T) {
	_, err := PackageLambda(filepath.Join(t.TempDir(), "absent"))
	if !os.IsNotExist(errUnwrapAll(err)) {
		t.Fatalf("error = %v, want a not-exist error the CLI can detect", err)
	}
}

func errUnwrapAll(err error) error {
	for {
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return err
		}
		next := u.Unwrap()
		if next == nil {
			return err
		}
		err = next
	}
}

func TestPackageLambda_ReadsFromDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bootstrap")
	if err := os.WriteFile(path, []byte("compiled handler"), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatalf("writing fixture: %v", err)
	}

	archive, err := PackageLambda(path)
	if err != nil {
		t.Fatalf("PackageLambda: %v", err)
	}
	if len(archive) == 0 {
		t.Error("archive is empty")
	}
}
