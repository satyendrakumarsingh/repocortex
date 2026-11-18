package cli

import (
	"strings"
	"testing"
)

func TestCheckSelectMessages(t *testing.T) {
	_, err := checkSelect("/*x*/ delete from files")
	if err == nil || !strings.Contains(err.Error(), "not DELETE") || strings.Contains(err.Error(), "/*X*/") {
		t.Errorf("a comment must not be named as the statement: %v", err)
	}
	if _, err := checkSelect("/* note */ select 1"); err != nil {
		t.Errorf("a SELECT after a comment is a SELECT: %v", err)
	}
	if _, err := checkSelect("-- note\nselect 1"); err != nil {
		t.Errorf("a SELECT after a line comment is a SELECT: %v", err)
	}
	_, err = checkSelect("select")
	if err == nil || strings.Contains(err.Error(), "near") || !strings.Contains(err.Error(), "nothing after") {
		t.Errorf("a bare SELECT needs a clear message: %v", err)
	}
}
