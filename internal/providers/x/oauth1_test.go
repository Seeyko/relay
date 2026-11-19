package x

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// Example from X's "Creating a signature" documentation.
func TestOAuth1Signature(t *testing.T) {
	o := &OAuth1{
		ConsumerKey:    "xvz1evFS4wEEPTGEFPHBog",
		ConsumerSecret: "kAcSOqF21Fu85e7zjz7ZN2U4ZRhfV3WpwPAoE3Z7kBw",
		Token:          "370773112-GmHxMAgYyLbNEtIKZeRNFsMKPR9EyMZeS9weJAEb",
		TokenSecret:    "LswwdoUaIvS8ltyTt5jkRh4J50vUPVVHtR2YPi5kE",
		now:            func() time.Time { return time.Unix(1318622958, 0) },
		nonce:          func() string { return "kYjzVBB8Y0ZFabxSWbWovY3uYSQ2pTgmZeNu2VS4cg" },
	}
	h := o.Header("POST", "https://api.twitter.com/1.1/statuses/update.json?include_entities=true",
		url.Values{"status": {"Hello Ladies + Gentlemen, a signed OAuth request!"}})
	want := `oauth_signature="hCtSmYh%2BiHYCEqBWrE7C7hYmtUk%3D"`
	if !strings.Contains(h, want) {
		t.Fatalf("signature mismatch\n got: %s\nwant: %s", h, want)
	}
}
