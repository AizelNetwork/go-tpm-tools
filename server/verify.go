package server

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"fmt"

	"github.com/google/go-tpm-tools/internal"
	pb "github.com/google/go-tpm-tools/proto/attest"
	tpmpb "github.com/google/go-tpm-tools/proto/tpm"
	"github.com/google/go-tpm/tpm2"
)

// The hash algorithms we support, in their preferred order of use.
var supportedHashAlgs = []tpm2.Algorithm{tpm2.AlgSHA512, tpm2.AlgSHA384, tpm2.AlgSHA256}

// VerifyOpts allows for customizing the functionality of VerifyAttestation.
type VerifyOpts struct {
	// The nonce used when calling client.Attest
	Nonce []byte
	// Trusted public keys that can be used to directly verify the key used for
	// attestation. This option should be used if you already know the AK.
	TrustedAKs []crypto.PublicKey
}

// VerifyAttestation performs the following checks on an Attestation:
//    - the AK used to generate the attestation is trusted (based on VerifyOpts)
//    - the provided signature is generated by the trusted AK public key
//    - the signature signs the provided quote data
//    - the quote data starts with TPM_GENERATED_VALUE
//    - the quote data is a valid TPMS_QUOTE_INFO
//    - the quote data was taken over the provided PCRs
//    - the provided PCR values match the quote data internal digest
//    - the provided opts.Nonce matches that in the quote data
//    - the provided eventlog matches the provided PCR values
//
// After this, the eventlog is parsed and the corresponding MachineState is
// returned. This design prevents unverified MachineStates from being used.
func VerifyAttestation(attestation *pb.Attestation, opts VerifyOpts) (*pb.MachineState, error) {
	// Verify the AK
	akPubArea, err := tpm2.DecodePublic(attestation.GetAkPub())
	if err != nil {
		return nil, fmt.Errorf("failed to decode AK public area: %w", err)
	}
	akPubKey, err := akPubArea.Key()
	if err != nil {
		return nil, fmt.Errorf("failed to get AK public key: %w", err)
	}
	if err = checkAkTrusted(akPubKey, opts); err != nil {
		return nil, err
	}

	// Verify the signing hash algorithm
	signHashAlg, err := internal.GetSigningHashAlg(akPubArea)
	if err != nil {
		return nil, fmt.Errorf("bad AK public area: %w", err)
	}
	if err = checkHashAlgSupported(signHashAlg, opts); err != nil {
		return nil, fmt.Errorf("in AK public area: %w", err)
	}

	// Attempt to replay the log against our PCRs in order of hash preference
	var lastErr error
	for _, quote := range supportedQuotes(attestation.GetQuotes()) {
		// Verify the Quote
		if err = internal.VerifyQuote(quote, akPubKey, opts.Nonce); err != nil {
			lastErr = fmt.Errorf("failed to verify quote: %w", err)
			continue
		}

		// Parse the event log and replay the events against the provided PCRs
		pcrs := quote.GetPcrs()
		state, err := ParseAndReplayEventLog(attestation.GetEventLog(), pcrs)
		if err != nil {
			lastErr = fmt.Errorf("failed to validate the event log: %w", err)
			continue
		}

		// Verify the PCR hash algorithm
		pcrHashAlg := tpm2.Algorithm(pcrs.GetHash())
		if err = checkHashAlgSupported(pcrHashAlg, opts); err != nil {
			return nil, fmt.Errorf("when verifying PCRs: %w", err)
		}

		return state, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("attestation does not contain a supported quote")
}

func pubKeysEqual(k1 crypto.PublicKey, k2 crypto.PublicKey) bool {
	switch key := k1.(type) {
	case *rsa.PublicKey:
		return key.Equal(k2)
	case *ecdsa.PublicKey:
		return key.Equal(k2)
	default:
		return false
	}
}

// Checks if the provided AK public key can be trusted
func checkAkTrusted(ak crypto.PublicKey, opts VerifyOpts) error {
	if len(opts.TrustedAKs) == 0 {
		return fmt.Errorf("no mechanism for AK verification provided")
	}

	// Check against known AKs
	for _, trusted := range opts.TrustedAKs {
		if pubKeysEqual(ak, trusted) {
			return nil
		}
	}
	return fmt.Errorf("AK public key is not trusted")
}

func checkHashAlgSupported(hash tpm2.Algorithm, opts VerifyOpts) error {
	for _, alg := range supportedHashAlgs {
		if hash == alg {
			return nil
		}
	}
	return fmt.Errorf("unsupported hash algorithm: %v", hash)
}

// Retrieve the supported quotes in order of hash preference
func supportedQuotes(quotes []*tpmpb.Quote) []*tpmpb.Quote {
	out := make([]*tpmpb.Quote, 0, len(quotes))
	for _, alg := range supportedHashAlgs {
		for _, quote := range quotes {
			if tpm2.Algorithm(quote.GetPcrs().GetHash()) == alg {
				out = append(out, quote)
				break
			}
		}
	}
	return out
}
