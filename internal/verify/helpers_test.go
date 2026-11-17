package verify

import (
	"crypto/ecdsa"
	"crypto/x509"
)

func marshalPKIX(pub *ecdsa.PublicKey) ([]byte, error) { return x509.MarshalPKIXPublicKey(pub) }
