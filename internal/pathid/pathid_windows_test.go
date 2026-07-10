//go:build windows

package pathid

import "testing"

func TestFoldIdentityNormalizesWindowsVolumeAndCase(t *testing.T) {
	got := foldIdentity(`c:\Users\Owner\Repo`)
	want := `C:\users\owner\repo`
	if got != want {
		t.Fatalf("foldIdentity = %q, want %q", got, want)
	}
}
