package icloud

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
)

// SRP-6a client for Apple's idmsa sign-in, SHA-256 over the RFC 5054 2048-bit
// group (g=2). Conventions follow Tom Cocagne's `srp` library (used successfully
// by pyicloud against Apple): k/u/K hash zero-padded values to len(N), while the
// client proof M1 hashes the minimal big-endian encodings of A and B.
//
// The password key x is NOT the library default H(s | H(I|":"|p)); Apple derives
// it as PBKDF2-HMAC-SHA256 over a digest of the password (see derivePasswordKey).

// RFC 5054 Appendix A 2048-bit group prime (g = 2).
const srpN2048Hex = "AC6BDB41324A9A9BF166DE5E1389582FAF72B6651987EE07FC3192943DB56050" +
	"A37329CBB4A099ED8193E0757767A13DD52312AB4B03310DCD7F48A9DA04FD50" +
	"E8083969EDB767B0CF6095179A163AB3661A05FBD5FAAAE82918A9962F0B93B8" +
	"55F97993EC975EEAA80D740ADBF4FF747359D041D5C33EA71D281E446B14773B" +
	"CA97B43A23FB801676BD207A436C6481F1D2B9078717461A5B9D32E688F87748" +
	"544523B524B0D57D5EA77A2775D2ECFA032CFBDBF52FB3786160279004E57AE6" +
	"AF874E7303CE53299CCC041C7BC308D82A5698F3A8D0C38271AE35F8E9DBFBB6" +
	"94B5C803D89F7AE435DE236D525F54759B65E372FCD68EF20FA7111F9E4AFF73"

var (
	srpN    = mustHexBig(srpN2048Hex)
	srpG    = big.NewInt(2)
	srpNLen = (srpN.BitLen() + 7) / 8
)

func mustHexBig(s string) *big.Int {
	n, ok := new(big.Int).SetString(s, 16)
	if !ok {
		panic("icloud: invalid SRP N constant")
	}
	return n
}

func srpHash(parts ...[]byte) []byte {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

// pad left-pads b with zeros to len(N) bytes (no-op if already >= len(N)).
func pad(b []byte) []byte {
	if len(b) >= srpNLen {
		return b
	}
	out := make([]byte, srpNLen)
	copy(out[srpNLen-len(b):], b)
	return out
}

// derivePasswordKey computes Apple's SRP password key x-source:
// PBKDF2-HMAC-SHA256(pwHash, salt, iter, 32), where pwHash is SHA256(password)
// for protocol "s2k", or its lowercase hex encoding for "s2k_fo".
func derivePasswordKey(password string, salt []byte, iter int, protocol string) ([]byte, error) {
	if iter <= 0 {
		return nil, fmt.Errorf("icloud: invalid SRP iteration count %d", iter)
	}
	digest := sha256.Sum256([]byte(password))
	var pwHash []byte
	switch protocol {
	case "s2k_fo":
		pwHash = []byte(hex.EncodeToString(digest[:]))
	default: // "s2k"
		pwHash = digest[:]
	}
	return pbkdf2.Key(sha256.New, string(pwHash), salt, iter, 32)
}

// xMode selects how the SRP password key x is derived from the PBKDF2 output dk.
type xMode int

const (
	xDirect        xMode = iota // x = dk
	xHashSalt                   // x = H(salt | H(dk))            (no-username gen_x)
	xHashSaltColon              // x = H(salt | H(":" | dk))      (colon-prefixed variant)
)

// srpOptions selects an SRP byte convention. Apple's server accepts exactly one;
// since it can't be confirmed offline, Begin uses the config-selected variant
// (one attempt per bind, because Apple throttles repeated sign-in attempts).
type srpOptions struct {
	xMode xMode
	padM1 bool // rfc5054: pad A/B/g to len(N) inside the M1/M2 hashes
}

// srpVariants are the candidate conventions, selected by index via config
// (RECIPES_ICLOUD_SRP_VARIANT). Apple throttles repeated sign-in attempts, so we
// try only one per bind; index 1 is the default (pyicloud-style: rfc5054 padding
// + no_username_in_x, which makes x = H(salt | H(":" | dk))). Index 0 has been
// observed to return 401 against at least one account.
var srpVariants = []srpOptions{
	{xHashSalt, true},       // 0
	{xHashSaltColon, true},  // 1 (default)
	{xDirect, true},         // 2
	{xHashSaltColon, false}, // 3
	{xHashSalt, false},      // 4
	{xDirect, false},        // 5
}

// deriveX wraps the PBKDF2 output dk into the SRP x value per opts.
func deriveX(dk, salt []byte, opts srpOptions) []byte {
	switch opts.xMode {
	case xHashSalt:
		return srpHash(salt, srpHash(dk))
	case xHashSaltColon:
		return srpHash(salt, srpHash(append([]byte{':'}, dk...)))
	default:
		return dk
	}
}

type srpClient struct {
	a *big.Int
	A *big.Int
}

// newSRPClient picks a random ephemeral a and computes A = g^a mod N.
func newSRPClient() (*srpClient, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	a := new(big.Int).SetBytes(buf)
	A := new(big.Int).Exp(srpG, a, srpN)
	return &srpClient{a: a, A: A}, nil
}

// aWire returns A as it goes on the wire: zero-padded to len(N) (matching the
// iCloud web client, which sends a full 256-byte value).
func (c *srpClient) aWire() []byte { return pad(c.A.Bytes()) }

// proof computes the client proof M1 and the expected server proof M2 (H_AMK),
// given the PBKDF2 password key dk, the salt, the server public value B, and the
// SRP convention opts. k/u/K are always padded (rfc5054); opts controls the x
// derivation and whether A/B/g are padded inside M1/M2.
func (c *srpClient) proof(appleID string, dk, salt, B []byte, opts srpOptions) (m1, m2 []byte, err error) {
	bInt := new(big.Int).SetBytes(B)
	if new(big.Int).Mod(bInt, srpN).Sign() == 0 {
		return nil, nil, fmt.Errorf("icloud: SRP server value B is zero")
	}
	xInt := new(big.Int).SetBytes(deriveX(dk, salt, opts))

	// k = H(PAD(N) | PAD(g))
	k := new(big.Int).SetBytes(srpHash(pad(srpN.Bytes()), pad(srpG.Bytes())))
	// u = H(PAD(A) | PAD(B))
	u := new(big.Int).SetBytes(srpHash(pad(c.A.Bytes()), pad(bInt.Bytes())))
	if u.Sign() == 0 {
		return nil, nil, fmt.Errorf("icloud: SRP u is zero")
	}

	// S = (B - k*g^x) ^ (a + u*x) mod N
	gx := new(big.Int).Exp(srpG, xInt, srpN)
	base := new(big.Int).Sub(bInt, new(big.Int).Mul(k, gx))
	base.Mod(base, srpN)
	exp := new(big.Int).Add(c.a, new(big.Int).Mul(u, xInt))
	S := new(big.Int).Exp(base, exp, srpN)

	K := srpHash(pad(S.Bytes()))

	// M1 = H( H(N) XOR H(g) | H(I) | salt | A | B | K ).
	aEnc, bEnc := c.A.Bytes(), bInt.Bytes()
	gEnc := srpG.Bytes()
	nEnc := srpN.Bytes()
	if opts.padM1 {
		aEnc, bEnc, gEnc, nEnc = pad(aEnc), pad(bEnc), pad(gEnc), pad(nEnc)
	}
	hN := srpHash(nEnc)
	hG := srpHash(gEnc)
	hXor := make([]byte, len(hN))
	for i := range hN {
		hXor[i] = hN[i] ^ hG[i]
	}
	hI := srpHash([]byte(appleID))
	m1 = srpHash(hXor, hI, salt, aEnc, bEnc, K)
	// M2 (H_AMK) = H( A | M1 | K )
	m2 = srpHash(aEnc, m1, K)
	return m1, m2, nil
}
