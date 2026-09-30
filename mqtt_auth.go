package main

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	meshcore "github.com/meshcore-go/meshcore-go"
)

const tokenLifetime = 10 * time.Minute

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

type jwtPayload struct {
	PublicKey string `json:"publicKey"`
	Aud       string `json:"aud"`
	Iat       int64  `json:"iat"`
	Exp       int64  `json:"exp"`
	Email     string `json:"email,omitempty"`
	Owner     string `json:"owner,omitempty"`
}

func generateToken(id meshcore.LocalIdentity, audience, email, owner string) (string, time.Time, error) {
	now := time.Now()
	exp := now.Add(tokenLifetime)

	header, err := json.Marshal(jwtHeader{Alg: "Ed25519", Typ: "JWT"})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("marshal header: %w", err)
	}

	payload, err := json.Marshal(jwtPayload{
		PublicKey: publicKeyHex(id),
		Aud:       audience,
		Iat:       now.Unix(),
		Exp:       exp.Unix(),
		Email:     email,
		Owner:     owner,
	})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("marshal payload: %w", err)
	}

	headerEnc := base64URLEncode(header)
	payloadEnc := base64URLEncode(payload)
	signingInput := headerEnc + "." + payloadEnc

	sig := id.Sign([]byte(signingInput))
	sigHex := strings.ToUpper(hex.EncodeToString(sig))

	token := signingInput + "." + sigHex
	return token, exp, nil
}

func tokenUsername(id meshcore.LocalIdentity) string {
	return "v1_" + publicKeyHex(id)
}

func base64URLEncode(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}
