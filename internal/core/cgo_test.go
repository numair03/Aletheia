package core

import (
	"errors"
	"testing"
)

// The codes are written out here, not taken from the C headers, so a change
// to a header value (which would break the C contract) fails this test.
func TestStatusErrors(t *testing.T) {
	want := map[int32]error{
		-1: ErrArg, -2: ErrBuffer, -3: ErrNotFound, -100: ErrInternal, -101: ErrUnknown,
		-10: ErrLinkage, -11: ErrMerkle, -12: ErrTxSet, -13: ErrBalance, -14: ErrSeq, -15: ErrOverflow,
	}
	if codeErr(0) != nil {
		t.Fatal("AL_OK must map to nil")
	}
	seen := map[error]bool{}
	for code, e := range want {
		got := codeErr(code)
		if got != e {
			t.Errorf("status %d: got %v, want %v", code, got, e)
		}
		if seen[got] {
			t.Errorf("status %d: error %v is shared with another code", code, got)
		}
		seen[got] = true
		var ce *Error
		if !errors.As(got, &ce) || ce.Code != code {
			t.Errorf("status %d: Code not preserved", code)
		}
	}
	if len(statusErrors) != len(want) {
		t.Errorf("binding knows %d codes, test lists %d", len(statusErrors), len(want))
	}

	// An unknown code is still an error and keeps its value.
	got := codeErr(-7)
	var ce *Error
	if got == nil || !errors.As(got, &ce) || ce.Code != -7 {
		t.Errorf("unknown status: got %v", got)
	}
}

func TestMustPanicsOnFailure(t *testing.T) {
	defer func() {
		if r := recover(); r != ErrInternal {
			t.Fatalf("recover() = %v, want ErrInternal", r)
		}
	}()
	must(-100)
	t.Fatal("must did not panic")
}
