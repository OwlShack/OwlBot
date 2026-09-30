package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	meshcore "github.com/meshcore-go/meshcore-go"
)

func loadOrCreateIdentity(path string) (meshcore.LocalIdentity, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return identityFromFile(data)
	}
	if !os.IsNotExist(err) {
		return meshcore.LocalIdentity{}, fmt.Errorf("reading key file: %w", err)
	}

	id, err := meshcore.GenerateLocalIdentity(nil)
	if err != nil {
		return meshcore.LocalIdentity{}, fmt.Errorf("generating identity: %w", err)
	}

	if err := writeIdentity(path, id); err != nil {
		return meshcore.LocalIdentity{}, err
	}

	return id, nil
}

func identityFromFile(data []byte) (meshcore.LocalIdentity, error) {
	seed, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return meshcore.LocalIdentity{}, fmt.Errorf("decoding key file: expected hex seed: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return meshcore.LocalIdentity{}, fmt.Errorf("key file: expected %d byte seed, got %d", ed25519.SeedSize, len(seed))
	}
	var s [ed25519.SeedSize]byte
	copy(s[:], seed)
	return meshcore.NewLocalIdentityFromSeed(s), nil
}

// writeIdentity persists the seed atomically (temp file + rename) so a crash
// mid-write can't corrupt the key and silently rotate the identity.
func writeIdentity(path string, id meshcore.LocalIdentity) error {
	seed := id.Seed()
	content := hex.EncodeToString(seed[:]) + "\n"
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return fmt.Errorf("writing key file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("renaming key file into place: %w", err)
	}
	return nil
}

func publicKeyHex(id meshcore.LocalIdentity) string {
	pk := id.PublicKey()
	return strings.ToUpper(hex.EncodeToString(pk[:]))
}
