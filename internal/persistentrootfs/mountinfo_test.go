package persistentrootfs

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestFindMountPoint(t *testing.T) {
	const longPrefix = "1 0 0:1 / /other rw - overlay overlay rw,lowerdir="
	const record = "2 1 0:2 / /bundle/a\u00a0b\\040c rw - tmpfs tmpfs rw"
	input := longPrefix + strings.Repeat("x", 128<<10) + "\n" + record
	for _, test := range []struct {
		target   string
		children bool
		want     bool
	}{
		{"/bundle/a\u00a0b c", false, true},
		{"/bundle/a", false, false},
		{"/bundle", false, false},
		{"/bundle", true, true},
		{"/bund", true, false},
		{"/", true, true},
		{"/missing", true, false},
	} {
		found, err := findMountPoint(strings.NewReader(input), test.target, test.children)
		if err != nil || found != test.want {
			t.Errorf("target=%q children=%t: found=%t err=%v", test.target, test.children, found, err)
		}
	}
}

func TestFindMountPointDoesNotHideErrors(t *testing.T) {
	if found, err := findMountPoint(strings.NewReader("truncated\n"), "/bundle", true); found || err == nil {
		t.Fatalf("invalid record: found=%t err=%v", found, err)
	}
	failure := errors.New("read interrupted")
	input := io.MultiReader(strings.NewReader("1 0 0:1 / /bundle rw"), iotest.ErrReader(failure))
	if found, err := findMountPoint(input, "/bundle", true); found || !errors.Is(err, failure) {
		t.Fatalf("partial read: found=%t err=%v", found, err)
	}
}
