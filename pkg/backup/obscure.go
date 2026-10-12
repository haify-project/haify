package backup

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"

	"go.uber.org/zap"
)

// rclone stores SMB/WebDAV passwords "obscured" and refuses a plain-text one
// outright, so a config we render has to carry the obscured form. Obscuring is
// AES-256-CTR under a key that ships inside every rclone binary, with the IV
// prepended and the whole thing base64url-encoded; rclone's own documentation
// is explicit that this is obfuscation, not encryption, and that anyone holding
// the config can reverse it. The real protection is the file's 0600 mode.
//
// Doing it here rather than shelling out to `rclone obscure` on the node is
// deliberate: the plain-text password would otherwise have to reach a command
// line, which is exactly what this whole path exists to avoid.
//
// If a future rclone were to change this encoding, the symptom would be an
// authentication failure against SMB/WebDAV only (S3 secrets are stored plain
// in rclone config and never go through here). TargetSpec.SecretIsObscured is
// the escape hatch: an operator can run `rclone obscure` themselves and pass
// the result through untouched.
var rcloneObscureKey = []byte{
	0x9c, 0x93, 0x5b, 0x48, 0x73, 0x0a, 0x55, 0x4d,
	0x6b, 0xfd, 0x7c, 0x63, 0xc8, 0x86, 0xa9, 0x2b,
	0xd3, 0x90, 0x19, 0x8e, 0xb8, 0x12, 0x8a, 0xfb,
	0xf4, 0xde, 0x16, 0x2b, 0x8b, 0x95, 0xf6, 0x38,
}

// obscureRand is the IV source; a package var so tests can make it
// deterministic.
var obscureRand io.Reader = rand.Reader

// Obscure encodes a password into rclone's config representation.
func Obscure(plain string) (string, error) {
	log := zap.L()
	log.Debug("Obscure: obscuring a secret", zap.Bool("has_secret", plain != ""))
	block, err := aes.NewCipher(rcloneObscureKey)
	if err != nil {
		log.Debug("Obscure: cipher failed", zap.Error(err))
		return "", fmt.Errorf("backup: obscure cipher: %w", err)
	}
	buf := make([]byte, aes.BlockSize+len(plain))
	iv := buf[:aes.BlockSize]
	if _, err := io.ReadFull(obscureRand, iv); err != nil {
		log.Debug("Obscure: reading the iv failed", zap.Error(err))
		return "", fmt.Errorf("backup: obscure iv: %w", err)
	}
	cipher.NewCTR(block, iv).XORKeyStream(buf[aes.BlockSize:], []byte(plain))
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Reveal reverses Obscure. It exists so the round trip is testable and so an
// operator-supplied obscured secret can be sanity-checked before it is stored:
// a value that does not decode is a typo, and finding that out at `target add`
// time is far better than at restore time.
func Reveal(obscured string) (string, error) {
	log := zap.L()
	log.Debug("Reveal: decoding an obscured secret", zap.Bool("has_secret", obscured != ""))
	raw, err := base64.RawURLEncoding.DecodeString(obscured)
	if err != nil {
		log.Debug("Reveal: not base64url", zap.Error(err))
		return "", fmt.Errorf("backup: value is not an rclone-obscured secret: %w", err)
	}
	if len(raw) < aes.BlockSize {
		log.Debug("Reveal: value shorter than an iv", zap.Int("decoded_len", len(raw)))
		return "", fmt.Errorf("backup: value is not an rclone-obscured secret: too short")
	}
	block, err := aes.NewCipher(rcloneObscureKey)
	if err != nil {
		log.Debug("Reveal: cipher failed", zap.Error(err))
		return "", fmt.Errorf("backup: obscure cipher: %w", err)
	}
	out := make([]byte, len(raw)-aes.BlockSize)
	cipher.NewCTR(block, raw[:aes.BlockSize]).XORKeyStream(out, raw[aes.BlockSize:])
	return string(out), nil
}
