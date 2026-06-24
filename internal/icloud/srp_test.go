package icloud

import (
	"bytes"
	"crypto/rand"
	"math/big"
	"testing"
)

func TestSRPGroupIs2048Bit(t *testing.T) {
	if srpN.BitLen() != 2048 {
		t.Fatalf("N is %d bits, want 2048", srpN.BitLen())
	}
	if srpNLen != 256 {
		t.Fatalf("len(N) = %d bytes, want 256", srpNLen)
	}
}

// TestSRPSelfConsistency simulates an SRP server (knowing v = g^x) and checks
// the client and server derive the same session key, so M1 verifies and the
// client's expected M2 matches the server's H_AMK. This locks the SRP math
// internally (it does not prove byte-equivalence with Apple's server).
func TestSRPSelfConsistency(t *testing.T) {
	appleID := "user@example.com"
	password := "correct horse battery staple"
	salt := []byte("0123456789abcdef")
	const iter = 20081

	x, err := derivePasswordKey(password, salt, iter, "s2k")
	if err != nil {
		t.Fatal(err)
	}
	xInt := new(big.Int).SetBytes(x)
	v := new(big.Int).Exp(srpG, xInt, srpN) // server verifier

	c, err := newSRPClient()
	if err != nil {
		t.Fatal(err)
	}

	// Server: B = (k*v + g^b) mod N
	k := new(big.Int).SetBytes(srpHash(pad(srpN.Bytes()), pad(srpG.Bytes())))
	bBuf := make([]byte, 32)
	if _, err := rand.Read(bBuf); err != nil {
		t.Fatal(err)
	}
	b := new(big.Int).SetBytes(bBuf)
	B := new(big.Int).Add(new(big.Int).Mul(k, v), new(big.Int).Exp(srpG, b, srpN))
	B.Mod(B, srpN)

	m1, m2, err := c.proof(appleID, x, salt, pad(B.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	// Server-side session key: S = (A * v^u)^b mod N
	u := new(big.Int).SetBytes(srpHash(pad(c.A.Bytes()), pad(B.Bytes())))
	serverS := new(big.Int).Exp(new(big.Int).Mul(c.A, new(big.Int).Exp(v, u, srpN)), b, srpN)
	serverK := srpHash(pad(serverS.Bytes()))

	hN := srpHash(srpN.Bytes())
	hG := srpHash(srpG.Bytes())
	hXor := make([]byte, len(hN))
	for i := range hN {
		hXor[i] = hN[i] ^ hG[i]
	}
	expM1 := srpHash(hXor, srpHash([]byte(appleID)), salt, c.A.Bytes(), B.Bytes(), serverK)
	if !bytes.Equal(m1, expM1) {
		t.Fatal("client M1 does not match server-recomputed M1 (session keys diverged)")
	}
	expM2 := srpHash(c.A.Bytes(), m1, serverK)
	if !bytes.Equal(m2, expM2) {
		t.Fatal("client expected-M2 does not match server H_AMK")
	}
}

func TestDerivePasswordKey(t *testing.T) {
	salt := []byte("saltsaltsaltsalt")
	a, err := derivePasswordKey("pw", salt, 1000, "s2k")
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 32 {
		t.Fatalf("key length %d, want 32", len(a))
	}
	// Deterministic.
	a2, _ := derivePasswordKey("pw", salt, 1000, "s2k")
	if !bytes.Equal(a, a2) {
		t.Fatal("derivation is not deterministic")
	}
	// Protocol changes the key (s2k vs s2k_fo hash the password differently).
	fo, _ := derivePasswordKey("pw", salt, 1000, "s2k_fo")
	if bytes.Equal(a, fo) {
		t.Fatal("s2k and s2k_fo produced the same key")
	}
	// Invalid iteration count is rejected.
	if _, err := derivePasswordKey("pw", salt, 0, "s2k"); err == nil {
		t.Fatal("expected error for zero iterations")
	}
}

func TestPadLength(t *testing.T) {
	if got := pad([]byte{1, 2, 3}); len(got) != srpNLen {
		t.Fatalf("pad length %d, want %d", len(got), srpNLen)
	}
	if got := pad(make([]byte, srpNLen)); len(got) != srpNLen {
		t.Fatalf("pad length %d, want %d", len(got), srpNLen)
	}
}
