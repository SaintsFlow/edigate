package contracts

import (
	"errors"
	"testing"
)

func TestValidateListInbox(t *testing.T) {
	cases := []struct {
		name string
		args string
		ok   bool
	}{
		{"empty object", `{}`, true},
		{"empty arguments", ``, true},
		{"status and limit", `{"status":"received","limit":10}`, true},
		{"extra field", `{"limit":1,"extra":true}`, false},
		{"limit zero", `{"limit":0}`, false},
		{"limit 101", `{"limit":101}`, false},
		{"limit 100", `{"limit":100}`, true},
		{"bad status", `{"status":"deleted"}`, false},
		{"not an object", `[]`, false},
		{"not json", `{`, false},
	}
	for _, c := range cases {
		err := Validate("edi.list_inbox", []byte(c.args))
		if c.ok && err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
		}
		if !c.ok && !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", c.name, err)
		}
	}
}

func TestValidateOutputListInbox(t *testing.T) {
	if err := ValidateOutput("edi.list_inbox", []byte(`{"documents":[]}`)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateOutput("edi.list_inbox", []byte(`{"documents":[],"x":1}`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("extra output field must fail, got %v", err)
	}
	if err := ValidateOutput("edi.list_inbox", []byte(`{}`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing documents must fail, got %v", err)
	}
}
