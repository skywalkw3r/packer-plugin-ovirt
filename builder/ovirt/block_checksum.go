package ovirt

import (
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
)

// blockChecksumBlockSize is the default block size of ovirt-imageio's checksum.
const blockChecksumBlockSize = 4 * 1024 * 1024

// blockChecksum computes the block based checksum that ovirt-imageio returns
// for GET /images/{ticket-id}/checksum: every block is hashed on its own, and
// the block digests are hashed again with the same algorithm. The result is
// not the plain digest of the data, so it cannot be compared with a checksum
// published next to an ISO.
//
// size is the size of the image to hash. If r holds less data, the rest is
// hashed as zeroes, which is how a disk that was rounded up on storage reads.
func blockChecksum(r io.Reader, size int64, newHash func() hash.Hash, blockSize int) (string, error) {
	if blockSize <= 0 {
		return "", fmt.Errorf("invalid block size %d", blockSize)
	}

	outer := newHash()
	inner := newHash()
	block := make([]byte, blockSize)
	eof := false

	for remaining := size; remaining > 0; {
		n := int64(blockSize)
		if remaining < n {
			n = remaining
		}
		buf := block[:n]

		read := 0
		if !eof {
			var err error
			read, err = io.ReadFull(r, buf)
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				eof = true
			} else if err != nil {
				return "", err
			}
		}
		clear(buf[read:])

		inner.Reset()
		inner.Write(buf)
		outer.Write(inner.Sum(nil))
		remaining -= n
	}

	return hex.EncodeToString(outer.Sum(nil)), nil
}
