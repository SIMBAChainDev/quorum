package qbfttypes

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
)

// captureErrorLogs installs a handler on the root logger for the duration of
// the test and returns the records logged at LvlCrit/LvlError. The root
// logger defaults to a DiscardHandler, so without this nothing is visible.
func captureErrorLogs(t *testing.T) *[]*log.Record {
	t.Helper()
	var records []*log.Record
	prevHandler := log.Root().GetHandler()
	log.Root().SetHandler(log.FuncHandler(func(r *log.Record) error {
		if r.Lvl == log.LvlCrit || r.Lvl == log.LvlError {
			records = append(records, r)
		}
		return nil
	}))
	t.Cleanup(func() {
		log.Root().SetHandler(prevHandler)
	})
	return &records
}

func makeTestBlock(number int64) *types.Block {
	header := &types.Header{
		Difficulty: big.NewInt(0),
		Number:     big.NewInt(number),
		GasLimit:   0,
		GasUsed:    0,
		Time:       0,
	}
	block := &types.Block{}
	return block.WithSeal(header)
}

// TestPreprepareRLPRoundTripWithJustification guards against a regression
// where decoding a Preprepare with a non-empty JustificationRoundChanges
// logged a spurious ERROR ("QBFT: Error List() Signed Payload err=\"rlp: end
// of list\"") for every justified PRE-PREPARE. rlp.EOL is the slice
// decoder's normal end-of-list probe when decoding
// []*SignedRoundChangePayload, not a decode failure - decoding must succeed
// with no ERROR-level logs.
func TestPreprepareRLPRoundTripWithJustification(t *testing.T) {
	records := captureErrorLogs(t)

	block := makeTestBlock(1)

	// One round-change with no prepared block (nil branch of encodePayloadInternal),
	// one with a real prepared block (non-empty prepared list branch) - exercises
	// both branches the decoder's `size > 0` check has to mirror.
	rc1 := NewRoundChange(big.NewInt(1), big.NewInt(0), big.NewInt(0), nil)
	rc1.SetSignature([]byte{0x01, 0x02, 0x03})

	rc2 := NewRoundChange(big.NewInt(1), big.NewInt(0), big.NewInt(0), block)
	rc2.SetSignature([]byte{0x04, 0x05, 0x06})

	preprepare := NewPreprepare(big.NewInt(1), big.NewInt(0), block)
	preprepare.JustificationRoundChanges = []*SignedRoundChangePayload{
		&rc1.SignedRoundChangePayload,
		&rc2.SignedRoundChangePayload,
	}
	preprepare.SetSignature([]byte{0x07, 0x08, 0x09})

	encoded, err := rlp.EncodeToBytes(preprepare)
	if err != nil {
		t.Fatalf("EncodeToBytes: %v", err)
	}

	var decoded Preprepare
	if err := rlp.DecodeBytes(encoded, &decoded); err != nil {
		t.Fatalf("DecodeBytes: %v", err)
	}

	if len(decoded.JustificationRoundChanges) != 2 {
		t.Fatalf("len(JustificationRoundChanges) = %d, want 2", len(decoded.JustificationRoundChanges))
	}

	for i, want := range []*SignedRoundChangePayload{&rc1.SignedRoundChangePayload, &rc2.SignedRoundChangePayload} {
		got := decoded.JustificationRoundChanges[i]
		if got.Sequence.Cmp(want.Sequence) != 0 {
			t.Errorf("entry %d: Sequence = %v, want %v", i, got.Sequence, want.Sequence)
		}
		if got.Round.Cmp(want.Round) != 0 {
			t.Errorf("entry %d: Round = %v, want %v", i, got.Round, want.Round)
		}
		if got.PreparedDigest != want.PreparedDigest {
			t.Errorf("entry %d: PreparedDigest = %v, want %v", i, got.PreparedDigest, want.PreparedDigest)
		}
		if string(got.Signature()) != string(want.Signature()) {
			t.Errorf("entry %d: Signature = %x, want %x", i, got.Signature(), want.Signature())
		}
	}

	if len(*records) != 0 {
		t.Errorf("decoding logged %d ERROR-level record(s), want 0: %+v", len(*records), *records)
	}
}

// TestPreprepareRLPRoundTripEmptyJustification covers the zero-element path,
// where the very first elemdec call into SignedRoundChangePayload.DecodeRLP
// immediately hits EOL.
func TestPreprepareRLPRoundTripEmptyJustification(t *testing.T) {
	records := captureErrorLogs(t)

	block := makeTestBlock(1)
	preprepare := NewPreprepare(big.NewInt(1), big.NewInt(0), block)
	preprepare.SetSignature([]byte{0x01})

	encoded, err := rlp.EncodeToBytes(preprepare)
	if err != nil {
		t.Fatalf("EncodeToBytes: %v", err)
	}

	var decoded Preprepare
	if err := rlp.DecodeBytes(encoded, &decoded); err != nil {
		t.Fatalf("DecodeBytes: %v", err)
	}

	if len(decoded.JustificationRoundChanges) != 0 {
		t.Fatalf("len(JustificationRoundChanges) = %d, want 0", len(decoded.JustificationRoundChanges))
	}

	if len(*records) != 0 {
		t.Errorf("decoding logged %d ERROR-level record(s), want 0: %+v", len(*records), *records)
	}
}

// TestSignedRoundChangePayloadDecodeMalformed proves the EOL guard added to
// SignedRoundChangePayload.DecodeRLP silenced only the benign-EOL log line,
// not real error propagation: genuinely malformed input must still return
// an error.
func TestSignedRoundChangePayloadDecodeMalformed(t *testing.T) {
	garbage := []byte{0xff, 0xff, 0xff}

	var decoded SignedRoundChangePayload
	if err := rlp.DecodeBytes(garbage, &decoded); err == nil {
		t.Fatal("DecodeBytes(garbage) succeeded, want error")
	}
}
