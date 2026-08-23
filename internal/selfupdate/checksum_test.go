package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestVerifySHA256(t *testing.T) {
	data := []byte("fake archive contents")
	sum := sha256.Sum256(data)
	hexSum := hex.EncodeToString(sum[:])
	checksums := hexSum + "  command-relay-agent_Linux_x86_64.tar.gz\n" +
		"deadbeef  other-file.tar.gz\n"

	if err := verifySHA256(data, checksums, "command-relay-agent_Linux_x86_64.tar.gz"); err != nil {
		t.Fatalf("verifySHA256: %v", err)
	}
}

func TestVerifySHA256_Mismatch(t *testing.T) {
	checksums := "deadbeef  command-relay-agent_Linux_x86_64.tar.gz\n"
	if err := verifySHA256([]byte("actual contents"), checksums, "command-relay-agent_Linux_x86_64.tar.gz"); err == nil {
		t.Error("verifySHA256: want error on checksum mismatch, got nil")
	}
}

func TestVerifySHA256_NoEntry(t *testing.T) {
	checksums := "deadbeef  other-file.tar.gz\n"
	if err := verifySHA256([]byte("data"), checksums, "command-relay-agent_Linux_x86_64.tar.gz"); err == nil {
		t.Error("verifySHA256: want error when no matching entry, got nil")
	}
}
