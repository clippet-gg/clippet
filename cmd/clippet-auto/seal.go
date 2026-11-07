package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"unicode/utf8"
)

// The ONE opener of a sealed Clippet Auto URL (§20 erratum 334; the format is
// clippet-pass forward/src/claim/seal.ts's, fixed by run 7's design review
// item 8 — the TypeScript seals and cannot open, this file opens and cannot
// seal):
//
//	blob = 0x01 ‖ ephemeral P-256 public key (65, uncompressed) ‖ IV (12)
//	            ‖ AES-256-GCM ciphertext and tag
//	key  = HKDF-SHA256(ECDH, salt = ephemeral ‖ recipient public,
//	                   info "clippet-onepress-v1")
//	AAD  = "v1|<guild>|<host>"
//
// base64url, at most 1024 characters; length- and shape-checked before any
// crypto. The private key is AUTO_SEAL_PRIVATE (a P-256 JWK, from
// clippet-pass autohost/scripts/keys.sh, set with `fly secrets set`). An opened
// URL goes to the one upload that asked and is never cached, stored or logged.

const (
	sealVersion = 0x01
	sealInfo    = "clippet-onepress-v1"
	sealMax     = 1024
	sealPub     = 65
	sealIV      = 12
	sealTag     = 16
)

type opener struct {
	priv   *ecdh.PrivateKey
	pubRaw []byte
}

// newOpener reads AUTO_SEAL_PRIVATE: {"kty":"EC","crv":"P-256","x","y","d"},
// base64url without padding, x/y/d 32 bytes each, and x/y the public half of d.
func newOpener(jwk string) (*opener, error) {
	bad := errors.New("AUTO_SEAL_PRIVATE is not a P-256 private JWK")
	var k struct {
		Kty, Crv, X, Y, D string
	}
	if jwk == "" || json.Unmarshal([]byte(jwk), &k) != nil || k.Kty != "EC" || k.Crv != "P-256" {
		return nil, bad
	}
	dec := func(s string) []byte {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil || len(b) != 32 {
			return nil
		}
		return b
	}
	x, y, d := dec(k.X), dec(k.Y), dec(k.D)
	if x == nil || y == nil || d == nil {
		return nil, bad
	}
	priv, err := ecdh.P256().NewPrivateKey(d)
	if err != nil {
		return nil, bad
	}
	pub := append([]byte{0x04}, append(x, y...)...)
	if string(priv.PublicKey().Bytes()) != string(pub) {
		return nil, bad
	}
	return &opener{priv: priv, pubRaw: pub}, nil
}

// open answers the URL a blob seals for this guild and host, or false.
func (o *opener) open(p, guild, host string) (string, bool) {
	if o == nil || len(p) == 0 || len(p) > sealMax {
		return "", false
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return "", false
		}
	}
	blob, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil || len(blob) < 1+sealPub+sealIV+sealTag+1 || blob[0] != sealVersion || blob[1] != 0x04 {
		return "", false
	}
	ephRaw := blob[1 : 1+sealPub]
	iv := blob[1+sealPub : 1+sealPub+sealIV]
	ct := blob[1+sealPub+sealIV:]
	eph, err := ecdh.P256().NewPublicKey(ephRaw)
	if err != nil {
		return "", false
	}
	shared, err := o.priv.ECDH(eph)
	if err != nil {
		return "", false
	}
	salt := append(append([]byte{}, ephRaw...), o.pubRaw...)
	key, err := hkdf.Key(sha256.New, shared, salt, sealInfo, 32)
	if err != nil {
		return "", false
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", false
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", false
	}
	pt, err := gcm.Open(nil, iv, ct, []byte("v1|"+guild+"|"+host))
	if err != nil || !utf8.Valid(pt) {
		return "", false
	}
	return string(pt), true
}
