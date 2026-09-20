package api

import (
	"strings"
	"testing"
)

func TestMissing(t *testing.T) {
	chat := &Model{Chat: true, Tools: true}
	if errs := chat.Missing(Needs{Chat: true, Tools: true}); len(errs) != 0 {
		t.Errorf("Missing = %v, want none", errs)
	}
	errs := chat.Missing(Needs{Vision: true})
	if len(errs) != 1 {
		t.Fatalf("Missing = %v, want one", errs)
	}
	if !strings.Contains(errs[0].Error(), "vision") {
		t.Errorf("Missing = %v, want vision named", errs)
	}

	eyes := &Model{Chat: true, Vision: true}
	if errs := eyes.Missing(Needs{Vision: true}); len(errs) != 0 {
		t.Errorf("Missing = %v, want none", errs)
	}
	if errs := eyes.Missing(Needs{Chat: true, Tools: true}); len(errs) != 1 {
		t.Errorf("Missing = %v, want one", errs)
	}
	if errs := chat.Missing(Needs{}); len(errs) != 0 {
		t.Errorf("Missing of nothing = %v", errs)
	}
}
