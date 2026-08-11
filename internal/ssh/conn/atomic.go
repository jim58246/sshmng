package conn

import (
	"crypto/rand"
	"encoding/hex"
)

// AtomicRemotePath returns a temp path for remotePath used during atomic
// upload: <remotePath>.sshmng-tmp-<6 hex>. Same directory as the target (so
// PosixRename is same-filesystem = atomic). The random suffix avoids collisions
// across concurrent transfers to the same target.
func AtomicRemotePath(remotePath string) string {
	b := make([]byte, 3) // 3 bytes → 6 hex chars
	rand.Read(b)
	return remotePath + ".sshmng-tmp-" + hex.EncodeToString(b)
}
