// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package tlsdial originally existed to set up a tls.Config for x509
// validation, using a memory-optimized path for iOS, but then we
// moved that to the tailscale/go tree instead, so now this package
// does very little. But for now we keep it as a unified point where
// we might want to add shared policy on outgoing TLS connections from
// the 3 places in the client that connect to Tailscale (logs,
// control, DERP).
package tlsdial

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"tailscale.com/envknob"
)

var counterFallbackOK int32 // atomic

// If SSLKEYLOGFILE is set, it's a file to which we write our TLS private keys
// in a way that WireShark can read.
//
// See https://developer.mozilla.org/en-US/docs/Mozilla/Projects/NSS/Key_Log_Format
var sslKeyLogFile = os.Getenv("SSLKEYLOGFILE")

var debug = envknob.Bool("TS_DEBUG_TLS_DIAL")

// Config returns a tls.Config for connecting to a server.
// If base is non-nil, it's cloned as the base config before
// being configured and returned.
func Config(host string, base *tls.Config) *tls.Config {
	var conf *tls.Config
	if base == nil {
		conf = new(tls.Config)
	} else {
		conf = base.Clone()
	}
	conf.ServerName = host

	if n := sslKeyLogFile; n != "" {
		f, err := os.OpenFile(n, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("WARNING: writing to SSLKEYLOGFILE %v", n)
		conf.KeyLogWriter = f
	}

	if conf.InsecureSkipVerify {
		panic("unexpected base.InsecureSkipVerify")
	}
	if conf.VerifyConnection != nil {
		panic("unexpected base.VerifyConnection")
	}

	// Set InsecureSkipVerify to prevent crypto/tls from doing its
	// own cert verification, as do the same work that it'd do
	// (with the baked-in fallback root) in the VerifyConnection hook.
	conf.InsecureSkipVerify = true
	conf.VerifyConnection = func(cs tls.ConnectionState) error {
		// First try doing x509 verification with the system's
		// root CA pool.
		opts := x509.VerifyOptions{
			DNSName:       cs.ServerName,
			Intermediates: x509.NewCertPool(),
		}
		for _, cert := range cs.PeerCertificates[1:] {
			opts.Intermediates.AddCert(cert)
		}
		_, errSys := cs.PeerCertificates[0].Verify(opts)
		if debug {
			log.Printf("tlsdial(sys %q): %v", host, errSys)
		}
		if errSys == nil {
			return nil
		}

		// If that failed, because the system's CA roots are old
		// or broken, fall back to trying LetsEncrypt at least.
		opts.Roots = bakedInRoots()
		_, err := cs.PeerCertificates[0].Verify(opts)
		if debug {
			log.Printf("tlsdial(bake %q): %v", host, err)
		}
		if err == nil {
			atomic.AddInt32(&counterFallbackOK, 1)
			return nil
		}
		return errSys
	}
	return conf
}

// SetConfigExpectedCert modifies c to expect and verify that the server returns
// a certificate for the provided certDNSName.
//
// This is for user-configurable client-side domain fronting support,
// where we send one SNI value but validate a different cert.
func SetConfigExpectedCert(c *tls.Config, certDNSName string) {
	if c.ServerName == certDNSName {
		return
	}
	if c.ServerName == "" {
		c.ServerName = certDNSName
		return
	}
	if c.VerifyPeerCertificate != nil {
		panic("refusing to override tls.Config.VerifyPeerCertificate")
	}
	// Set InsecureSkipVerify to prevent crypto/tls from doing its
	// own cert verification, but do the same work that it'd do
	// (but using certDNSName) in the VerifyPeerCertificate hook.
	c.InsecureSkipVerify = true
	c.VerifyConnection = nil
	c.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("no certs presented")
		}
		certs := make([]*x509.Certificate, len(rawCerts))
		for i, asn1Data := range rawCerts {
			cert, err := x509.ParseCertificate(asn1Data)
			if err != nil {
				return err
			}
			certs[i] = cert
		}
		opts := x509.VerifyOptions{
			CurrentTime:   time.Now(),
			DNSName:       certDNSName,
			Intermediates: x509.NewCertPool(),
		}
		for _, cert := range certs[1:] {
			opts.Intermediates.AddCert(cert)
		}
		_, errSys := certs[0].Verify(opts)
		if debug {
			log.Printf("tlsdial(sys %q/%q): %v", c.ServerName, certDNSName, errSys)
		}
		if errSys == nil {
			return nil
		}
		opts.Roots = bakedInRoots()
		_, err := certs[0].Verify(opts)
		if debug {
			log.Printf("tlsdial(bake %q/%q): %v", c.ServerName, certDNSName, err)
		}
		if err == nil {
			return nil
		}
		return errSys
	}
}

/*
letsEncryptX1 is the LetsEncrypt X1 root:

Certificate:
    Data:
        Version: 3 (0x2)
        Serial Number:
            82:10:cf:b0:d2:40:e3:59:44:63:e0:bb:63:82:8b:00
        Signature Algorithm: sha256WithRSAEncryption
        Issuer: C = US, O = Internet Security Research Group, CN = ISRG Root X1
        Validity
            Not Before: Jun  4 11:04:38 2015 GMT
            Not After : Jun  4 11:04:38 2035 GMT
        Subject: C = US, O = Internet Security Research Group, CN = ISRG Root X1
        Subject Public Key Info:
            Public Key Algorithm: rsaEncryption
                RSA Public-Key: (4096 bit)

We bake it into the binary as a fallback verification root,
in case the system we're running on doesn't have it.
(Tailscale runs on some ancient devices.)

To test that this code is working on Debian/Ubuntu:

$ sudo mv /usr/share/ca-certificates/mozilla/ISRG_Root_X1.crt{,.old}
$ sudo update-ca-certificates

Then restart tailscaled. To also test dnsfallback's use of it, nuke
your /etc/resolv.conf and it should still start & run fine.

*/
const letsEncryptX1 = `
-----BEGIN CERTIFICATE-----
MIIFazCCA1OgAwIBAgIRAIIQz7DSQONZRGPgu2OCiwAwDQYJKoZIhvcNAQELBQAw
TzELMAkGA1UEBhMCVVMxKTAnBgNVBAoTIEludGVybmV0IFNlY3VyaXR5IFJlc2Vh
cmNoIEdyb3VwMRUwEwYDVQQDEwxJU1JHIFJvb3QgWDEwHhcNMTUwNjA0MTEwNDM4
WhcNMzUwNjA0MTEwNDM4WjBPMQswCQYDVQQGEwJVUzEpMCcGA1UEChMgSW50ZXJu
ZXQgU2VjdXJpdHkgUmVzZWFyY2ggR3JvdXAxFTATBgNVBAMTDElTUkcgUm9vdCBY
MTCCAiIwDQYJKoZIhvcNAQEBBQADggIPADCCAgoCggIBAK3oJHP0FDfzm54rVygc
h77ct984kIxuPOZXoHj3dcKi/vVqbvYATyjb3miGbESTtrFj/RQSa78f0uoxmyF+
0TM8ukj13Xnfs7j/EvEhmkvBioZxaUpmZmyPfjxwv60pIgbz5MDmgK7iS4+3mX6U
A5/TR5d8mUgjU+g4rk8Kb4Mu0UlXjIB0ttov0DiNewNwIRt18jA8+o+u3dpjq+sW
T8KOEUt+zwvo/7V3LvSye0rgTBIlDHCNAymg4VMk7BPZ7hm/ELNKjD+Jo2FR3qyH
B5T0Y3HsLuJvW5iB4YlcNHlsdu87kGJ55tukmi8mxdAQ4Q7e2RCOFvu396j3x+UC
B5iPNgiV5+I3lg02dZ77DnKxHZu8A/lJBdiB3QW0KtZB6awBdpUKD9jf1b0SHzUv
KBds0pjBqAlkd25HN7rOrFleaJ1/ctaJxQZBKT5ZPt0m9STJEadao0xAH0ahmbWn
OlFuhjuefXKnEgV4We0+UXgVCwOPjdAvBbI+e0ocS3MFEvzG6uBQE3xDk3SzynTn
jh8BCNAw1FtxNrQHusEwMFxIt4I7mKZ9YIqioymCzLq9gwQbooMDQaHWBfEbwrbw
qHyGO0aoSCqI3Haadr8faqU9GY/rOPNk3sgrDQoo//fb4hVC1CLQJ13hef4Y53CI
rU7m2Ys6xt0nUW7/vGT1M0NPAgMBAAGjQjBAMA4GA1UdDwEB/wQEAwIBBjAPBgNV
HRMBAf8EBTADAQH/MB0GA1UdDgQWBBR5tFnme7bl5AFzgAiIyBpY9umbbjANBgkq
hkiG9w0BAQsFAAOCAgEAVR9YqbyyqFDQDLHYGmkgJykIrGF1XIpu+ILlaS/V9lZL
ubhzEFnTIZd+50xx+7LSYK05qAvqFyFWhfFQDlnrzuBZ6brJFe+GnY+EgPbk6ZGQ
3BebYhtF8GaV0nxvwuo77x/Py9auJ/GpsMiu/X1+mvoiBOv/2X/qkSsisRcOj/KK
NFtY2PwByVS5uCbMiogziUwthDyC3+6WVwW6LLv3xLfHTjuCvjHIInNzktHCgKQ5
ORAzI4JMPJ+GslWYHb4phowim57iaztXOoJwTdwJx4nLCgdNbOhdjsnvzqvHu7Ur
TkXWStAmzOVyyghqpZXjFaH3pO3JLF+l+/+sKAIuvtd7u+Nxe5AW0wdeRlN8NwdC
jNPElpzVmbUq4JUagEiuTDkHzsxHpFKVK7q4+63SM1N95R1NbdWhscdCb+ZAJzVc
oyi3B43njTOQ5yOf+1CceWxG1bQVs5ZufpsMljq4Ui0/1lvh+wjChP4kqKOJ2qxq
4RgqsahDYVvTH9w7jXbyLeiNdd8XM2w9U/t7y0Ff/9yi0GE44Za4rF2LN9d11TPA
mRGunUHBcnWEvgJBQl9nJEiU0Zsnvgc/ubhPgXRR4Xq37Z0j4r7g1SgEEzwxA57d
emyPxgcYxn/eR44/KJ4EBs+lVDR3veyJm+kXQ99b21/+jh5Xos1AnX5iItreGCc=
-----END CERTIFICATE-----
`

var bakedInRootsOnce struct {
	sync.Once
	p *x509.CertPool
}

func bakedInRoots() *x509.CertPool {
	bakedInRootsOnce.Do(func() {
		p := x509.NewCertPool()
		if !p.AppendCertsFromPEM([]byte(letsEncryptX1)) {
			panic("bogus PEM")
		}
		bakedInRootsOnce.p = p
	})
	return bakedInRootsOnce.p
}
