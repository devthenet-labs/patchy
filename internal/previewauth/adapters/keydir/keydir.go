// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package keydir

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/bitwise-media-group/patchy/internal/previewauth"
)

// The file names, which are the keys Secret's data keys.
const (
	FileMaster             = "master"
	FileGeneration         = "generation"
	FilePreviousMaster     = "previousMaster"
	FilePreviousGeneration = "previousGeneration"
	FileSigningKey         = "signingKey"
	FilePreviousSigningKey = "previousSigningKey"
)

// maxFile bounds every file read.
const maxFile = 16 << 10

// Keys is the loaded key material.
type Keys struct {
	Ring *previewauth.KeyRing
	// SigningKey and PreviousSigningKey are PEM; PreviousSigningKey is nil
	// outside a rotation.
	SigningKey         []byte
	PreviousSigningKey []byte
}

// Load reads dir. Values are trimmed of surrounding whitespace (a Secret
// written by hand often ends in a newline).
func Load(dir string) (Keys, error) {
	master, err := read(dir, FileMaster, true)
	if err != nil {
		return Keys{}, err
	}
	gen, err := readGen(dir, FileGeneration, true)
	if err != nil {
		return Keys{}, err
	}
	signing, err := read(dir, FileSigningKey, true)
	if err != nil {
		return Keys{}, err
	}
	prevMaster, err := read(dir, FilePreviousMaster, false)
	if err != nil {
		return Keys{}, err
	}
	prevGen, err := readGen(dir, FilePreviousGeneration, false)
	if err != nil {
		return Keys{}, err
	}
	prevSigning, err := read(dir, FilePreviousSigningKey, false)
	if err != nil {
		return Keys{}, err
	}
	var previous *previewauth.Generation
	switch {
	case prevMaster != nil && prevGen >= 0:
		previous = &previewauth.Generation{Number: prevGen, Master: prevMaster}
	case prevMaster != nil || prevGen >= 0:
		return Keys{}, fmt.Errorf("keys: %s and %s must be set together", FilePreviousMaster, FilePreviousGeneration)
	}
	ring, err := previewauth.NewKeyRing(previewauth.Generation{Number: gen, Master: master}, previous)
	if err != nil {
		return Keys{}, fmt.Errorf("keys: %w", err)
	}
	return Keys{Ring: ring, SigningKey: signing, PreviousSigningKey: prevSigning}, nil
}

// read returns the trimmed file, nil when it is absent or empty and not
// required.
func read(dir, name string, required bool) ([]byte, error) {
	f, err := os.Open(filepath.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) && !required {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("keys: %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, maxFile+1)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("keys: %s: %w", name, err)
	}
	if n > maxFile {
		return nil, fmt.Errorf("keys: %s is larger than %d bytes", name, maxFile)
	}
	v := bytes.TrimSpace(buf[:n])
	if len(v) == 0 {
		if required {
			return nil, fmt.Errorf("keys: %s is empty", name)
		}
		return nil, nil
	}
	return v, nil
}

// readGen reads a generation number; -1 when it is absent and not required.
func readGen(dir, name string, required bool) (int, error) {
	raw, err := read(dir, name, required)
	if err != nil || raw == nil {
		return -1, err
	}
	n, err := strconv.Atoi(string(raw))
	if err != nil || n < 0 || n > previewauth.MaxGeneration || strconv.Itoa(n) != string(raw) {
		return -1, fmt.Errorf("keys: %s is not a generation number in [0, %d]", name, previewauth.MaxGeneration)
	}
	return n, nil
}
