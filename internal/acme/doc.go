// Package acme obtains and renews TLS certificates through the ACME protocol
// (HTTP-01 and, optionally, DNS-01) using certmagic.
//
// Certificates, ACME account material and coordination locks are persisted
// in the metadata store through Storage, so a restarted server serves the
// same certificates and any node can answer an HTTP-01 challenge.
package acme
