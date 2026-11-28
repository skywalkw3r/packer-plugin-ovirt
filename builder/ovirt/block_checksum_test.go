package ovirt

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"
)

// The expected values are the sha1 vectors of ovirt-imageio's own test suite
// (test/blkhash_test.py, test_checksum): a file holding "data" extended with
// zeroes to the given size, hashed with 4 MiB blocks.
func TestBlockChecksum_ImageioVectors(t *testing.T) {
	const mib = 1024 * 1024
	cases := []struct {
		name     string
		size     int64
		checksum string
	}{
		{"aligned to the block size", 4 * mib, "3ed03b375b6658d99b63ced1867a95aeef080b79"},
		{"smaller than a block", 3 * mib, "6cba43b908381be45a55ab9b4361f8370b928354"},
		{"partial last block", 5 * mib, "d3936edd8e3a8ff10e8257a9f460d8da67838549"},
		{"empty", 0, "da39a3ee5e6b4b0d3255bfef95601890afd80709"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A full size reader, as when the file has the size of the image.
			data := make([]byte, tc.size)
			copy(data, "data")
			got, err := blockChecksum(bytes.NewReader(data), tc.size, sha1.New, blockChecksumBlockSize)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.checksum {
				t.Fatalf("full reader: expected %s, got %s", tc.checksum, got)
			}

			// A short reader, as when the image was rounded up on storage.
			short := []byte("data")
			if tc.size == 0 {
				short = nil
			}
			got, err = blockChecksum(bytes.NewReader(short), tc.size, sha1.New, blockChecksumBlockSize)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.checksum {
				t.Fatalf("zero padded: expected %s, got %s", tc.checksum, got)
			}
		})
	}
}

func TestBlockChecksum_IsDigestOfBlockDigests(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), 1000) // 16000 bytes
	const blockSize = 4096

	outer := sha256.New()
	for start := 0; start < len(data); start += blockSize {
		end := min(start+blockSize, len(data))
		digest := sha256.Sum256(data[start:end])
		outer.Write(digest[:])
	}
	want := hex.EncodeToString(outer.Sum(nil))

	got, err := blockChecksum(bytes.NewReader(data), int64(len(data)), sha256.New, blockSize)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("expected %s, got %s", want, got)
	}

	plain := sha256.Sum256(data)
	if got == hex.EncodeToString(plain[:]) {
		t.Fatal("the block checksum must differ from the plain sha256 of the data")
	}
}

func TestBlockChecksum_ReadError(t *testing.T) {
	r := io.MultiReader(bytes.NewReader(make([]byte, 10)), failingReader{})
	if _, err := blockChecksum(r, 100, sha256.New, 64); err == nil {
		t.Fatal("expected the read error to be returned")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrClosedPipe }
