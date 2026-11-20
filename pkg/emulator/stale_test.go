//go:build unix

package emulator

import (
	"reflect"
	"testing"
)

func TestMatchProcesses(t *testing.T) {
	ps := `  101 /sdk/emulator/emulator -avd emuctl-p1 -port 5600 -no-window
  102 /sdk/emulator/qemu/darwin-aarch64/qemu-system-aarch64 -avd emuctl-p1 -port 5600 -no-audio
  103 /sdk/emulator/emulator -avd emuctl-p1 -port 5602
  104 /sdk/emulator/emulator -avd emuctl-p10 -port 5600
  105 /sdk/emulator/emulator -avd Pixel_7 -port 5600
  106 vim -avd emuctl-p1
    1 /sbin/launchd -avd emuctl-p1 -port 5600
`
	got := matchProcesses(ps, "emuctl-p1", 5600)
	if want := []int{101, 102}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
