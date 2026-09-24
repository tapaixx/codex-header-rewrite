package main

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// oailbToken builds an unsigned __oailb value naming a backend and validity.
func oailbToken(host string, iat, exp time.Time) string {
	enc := func(v any) string { raw, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(raw) }
	return enc(map[string]any{"alg": "HS256", "typ": "JWT"}) + "." +
		enc(map[string]any{"host": host, "iat": iat.Unix(), "exp": exp.Unix()}) + "." + enc("sig")
}

func TestOailbRoute(t *testing.T) {
	iat, exp := time.Unix(1790000000, 0).UTC(), time.Unix(1790007200, 0).UTC()
	host, issued, expires, ok := oailbRoute("a=1; __oailb=" + oailbToken("gw-iad-7", iat, exp) + "; b=2")
	if !ok || host != "gw-iad-7" || !issued.Equal(iat) || !expires.Equal(exp) {
		t.Fatalf("host=%q iat=%v exp=%v ok=%v", host, issued, expires, ok)
	}
	for _, cookie := range []string{"", "a=1", "__oailb=not-a-jwt", "__oailb=a.b.c"} {
		if _, _, _, ok := oailbRoute(cookie); ok {
			t.Fatalf("%q names no backend", cookie)
		}
	}
}
