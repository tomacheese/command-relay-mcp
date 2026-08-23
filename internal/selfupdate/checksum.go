package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// verifySHA256 checks data's SHA256 digest against the entry for
// assetName in checksumsText, GoReleaser's default
// "<sha256>  <filename>" per-line format.
func verifySHA256(data []byte, checksumsText, assetName string) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])

	for _, line := range strings.Split(checksumsText, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != assetName {
			continue
		}
		if fields[0] != got {
			return fmt.Errorf("selfupdate: checksum mismatch for %s: got %s, want %s", assetName, got, fields[0])
		}
		return nil
	}
	return fmt.Errorf("selfupdate: no checksum entry found for %s", assetName)
}
