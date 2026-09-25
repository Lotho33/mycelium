package pileus

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"fmt"
)

// randomUUID returns a random RFC 4122-shaped UUID string. Not imported from
// a library on purpose — a profile's PayloadUUID only needs to be a unique,
// stable-looking identifier, not a spec-perfect UUID, and this is the whole
// implementation in a few lines.
func randomUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// BuildMobileConfig wraps certPEM (as returned by CurrentCertPEM) in a
// minimal Apple Configuration Profile so Safari on iOS can install it via
// the standard "Profilo scaricato" flow (GET /cert.mobileconfig).
//
// This only gets the certificate INTO the device's keychain — Apple does
// not let a profile pre-authorize full trust for it. The user still has to
// go to Impostazioni → Generale → Info → Impostazioni attendibilità
// certificati and enable it by hand afterwards; there's no way around that
// step, by design (see /trust's own instructions, which spell this out).
func BuildMobileConfig(certPEM string) (string, error) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("mobileconfig: no PEM certificate block found")
	}
	certB64 := base64.StdEncoding.EncodeToString(block.Bytes)

	payloadUUID, err := randomUUID()
	if err != nil {
		return "", err
	}
	profileUUID, err := randomUUID()
	if err != nil {
		return "", err
	}

	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>PayloadContent</key>
	<array>
		<dict>
			<key>PayloadCertificateFileName</key>
			<string>mycelium.cer</string>
			<key>PayloadContent</key>
			<data>%s</data>
			<key>PayloadDescription</key>
			<string>Certificato self-signed del server Mycelium.</string>
			<key>PayloadDisplayName</key>
			<string>Mycelium</string>
			<key>PayloadIdentifier</key>
			<string>com.mycelium.cert.%s</string>
			<key>PayloadType</key>
			<string>com.apple.security.root</string>
			<key>PayloadUUID</key>
			<string>%s</string>
			<key>PayloadVersion</key>
			<integer>1</integer>
		</dict>
	</array>
	<key>PayloadDescription</key>
	<string>Installa il certificato del tuo server Mycelium, cos&#236; Safari lo riconosce come attendibile. Dopo l'installazione va comunque attivata la piena fiducia in Impostazioni &#8594; Generale &#8594; Info &#8594; Impostazioni attendibilit&#224; certificati.</string>
	<key>PayloadDisplayName</key>
	<string>Certificato Mycelium</string>
	<key>PayloadIdentifier</key>
	<string>com.mycelium.trust.%s</string>
	<key>PayloadRemovalDisallowed</key>
	<false/>
	<key>PayloadType</key>
	<string>Configuration</string>
	<key>PayloadUUID</key>
	<string>%s</string>
	<key>PayloadVersion</key>
	<integer>1</integer>
</dict>
</plist>
`, certB64, payloadUUID, payloadUUID, profileUUID, profileUUID), nil
}
