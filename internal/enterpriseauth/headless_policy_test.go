package enterpriseauth

import (
	"errors"
	"testing"
)

func TestHeadlessConfirmationExactUppercaseFormats(t *testing.T) {
	for input, expected := range map[string]string{"ABCDEFGH": "ABCDEFGH", "ABCD-EFGH": "ABCDEFGH", "01234567": "01234567", "0123-4567": "01234567", "WXYZ-6789": "WXYZ6789"} {
		actual, err := NormalizeHeadlessConfirmationCode(input)
		if err != nil || actual != expected {
			t.Fatalf("valid format rejected: %q", input)
		}
	}
	for _, input := range []string{"", "abcd-efgh", "ABCD-EFG", "ABCD-EFGHJ", "AB-CDEFGH", " ABCDEFGH", "ABCDEFGH\n", "ABCD_EFGH", "ABCD OIL", "ABCD-U123", "ABCD-I123", "ABCD-L123", "ABCD-O123", "ＡＢＣＤＥＦＧＨ", "ABCD-\x00FGH"} {
		if value, err := NormalizeHeadlessConfirmationCode(input); !errors.Is(err, ErrDeviceRequestInvalid) || value != "" {
			t.Fatalf("invalid format accepted: %q", input)
		}
	}
	compact, err := HeadlessConfirmationHash("ABCDEFGH")
	if err != nil {
		t.Fatal(err)
	}
	grouped, err := HeadlessConfirmationHash("ABCD-EFGH")
	if err != nil || compact != grouped || compact == ([32]byte{}) {
		t.Fatal("display grouping changed confirmation identity")
	}
	if value, err := HeadlessConfirmationHash("abcd-efgh"); err == nil || value != ([32]byte{}) {
		t.Fatal("invalid code returned a verifier")
	}
}

func TestHeadlessConfirmationGeneratorUsesEightSymbols(t *testing.T) {
	for i := 0; i < 20; i++ {
		code, err := NewHeadlessConfirmationCode()
		if err != nil {
			t.Fatal(err)
		}
		if parsed, err := NormalizeHeadlessConfirmationCode(code); err != nil || len(code) != 8 || parsed != code {
			t.Fatal("generator returned an invalid compact code")
		}
	}
}
