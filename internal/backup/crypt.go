package backup

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/scrypt"
)

// Encrypted backups: the plain archive (a ZIP) sealed with AES-256-GCM under
// a key derived from the passphrase with scrypt. The stream is cut into
// chunks, each sealed on its own, so neither side holds the whole file in
// memory:
//
//	magic "MYCBK" 0x01 | salt (16) | nonce prefix (8)
//	then per chunk: ciphertext length (uint32 BE) | ciphertext
//
// Chunk i's nonce is the prefix followed by i (uint32 BE); its additional
// data is one byte, 1 on the last chunk and 0 otherwise, so a truncated or
// reordered file fails to open instead of decrypting to a shorter archive.

var encMagic = []byte("MYCBK\x01")

const (
	encChunk   = 1 << 20
	saltLen    = 16
	noncePfx   = 8
	scryptN    = 1 << 15 // ~32 MiB, a fraction of a second even on a thin client
	scryptR    = 8
	scryptP    = 1
	gcmOverhed = 16
)

// ErrWrongPassphrase is returned when the archive doesn't open with the
// passphrase given (or is damaged: GCM can't tell the two apart).
var ErrWrongPassphrase = errors.New("passphrase errata o file danneggiato")

// ErrNeedPassphrase is returned for an encrypted archive opened without one.
var ErrNeedPassphrase = errors.New("il backup è cifrato: serve la passphrase")

func deriveAEAD(pass string, salt []byte) (cipher.AEAD, error) {
	key, err := scrypt.Key([]byte(pass), salt, scryptN, scryptR, scryptP, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func chunkNonce(prefix []byte, i uint32) []byte {
	n := make([]byte, 12)
	copy(n, prefix)
	binary.BigEndian.PutUint32(n[8:], i)
	return n
}

// encryptStream writes src to dst sealed with pass.
func encryptStream(dst io.Writer, src io.Reader, pass string) error {
	head := make([]byte, saltLen+noncePfx)
	if _, err := rand.Read(head); err != nil {
		return err
	}
	salt, prefix := head[:saltLen], head[saltLen:]
	aead, err := deriveAEAD(pass, salt)
	if err != nil {
		return err
	}
	if _, err := dst.Write(encMagic); err != nil {
		return err
	}
	if _, err := dst.Write(head); err != nil {
		return err
	}

	// Read one chunk ahead: a chunk is the last one only once the next read
	// hits EOF.
	br := bufio.NewReaderSize(src, encChunk)
	cur := make([]byte, encChunk)
	next := make([]byte, encChunk)
	n, err := io.ReadFull(br, cur)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return err
	}
	var lenBuf [4]byte
	for i := uint32(0); ; i++ {
		if i == ^uint32(0) {
			return errors.New("backup troppo grande")
		}
		m, rerr := io.ReadFull(br, next)
		if rerr != nil && rerr != io.ErrUnexpectedEOF && rerr != io.EOF {
			return rerr
		}
		last := m == 0
		ad := []byte{0}
		if last {
			ad[0] = 1
		}
		sealed := aead.Seal(nil, chunkNonce(prefix, i), cur[:n], ad)
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(sealed)))
		if _, err := dst.Write(lenBuf[:]); err != nil {
			return err
		}
		if _, err := dst.Write(sealed); err != nil {
			return err
		}
		if last {
			return nil
		}
		cur, next = next, cur
		n = m
	}
}

// decryptStream writes the plain archive sealed in src (whose magic has
// already been consumed) to dst.
func decryptStream(dst io.Writer, src io.Reader, pass string) error {
	head := make([]byte, saltLen+noncePfx)
	if _, err := io.ReadFull(src, head); err != nil {
		return ErrWrongPassphrase
	}
	aead, err := deriveAEAD(pass, head[:saltLen])
	if err != nil {
		return err
	}
	prefix := head[saltLen:]
	var lenBuf [4]byte
	buf := make([]byte, encChunk+gcmOverhed)
	for i := uint32(0); ; i++ {
		if _, err := io.ReadFull(src, lenBuf[:]); err != nil {
			return fmt.Errorf("%w (file troncato)", ErrWrongPassphrase)
		}
		l := binary.BigEndian.Uint32(lenBuf[:])
		if l < gcmOverhed || l > encChunk+gcmOverhed {
			return ErrWrongPassphrase
		}
		if _, err := io.ReadFull(src, buf[:l]); err != nil {
			return fmt.Errorf("%w (file troncato)", ErrWrongPassphrase)
		}
		nonce := chunkNonce(prefix, i)
		// Try "not last" first: every chunk but one is.
		plain, err := aead.Open(nil, nonce, buf[:l], []byte{0})
		last := false
		if err != nil {
			plain, err = aead.Open(nil, nonce, buf[:l], []byte{1})
			if err != nil {
				return ErrWrongPassphrase
			}
			last = true
		}
		if _, err := dst.Write(plain); err != nil {
			return err
		}
		if last {
			// Anything after the last chunk is not ours.
			if n, _ := src.Read(lenBuf[:1]); n > 0 {
				return ErrWrongPassphrase
			}
			return nil
		}
	}
}

// isEncrypted reports whether head (the file's first bytes) starts an
// encrypted backup.
func isEncrypted(head []byte) bool { return bytes.HasPrefix(head, encMagic) }
