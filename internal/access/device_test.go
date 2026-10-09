package access

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func deviceHome(t *testing.T) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h, ".config"))
}

// A member's device is kept by its hash alone, is who the member is now —
// never what the key itself lists — and is refused when no one resolves
// the member, when the member is gone, and once it is taken away.
func TestDeviceIsTheMembers(t *testing.T) {
	deviceHome(t)
	was := MemberOf
	t.Cleanup(func() { MemberOf = was })
	members := map[string]Identity{"m1": {Member: "m1", MemberName: "alice", Models: []string{"group/coding"}, Concurrency: 2}}
	MemberOf = func(k Key) (Identity, bool) { who, ok := members[k.Member]; return who, ok }

	id, secret, err := AddDevice("m1", "laptop", 42)
	if err != nil || !strings.HasPrefix(secret, Prefix) {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(Path())
	if strings.Contains(string(b), secret) || !strings.Contains(string(b), HashOf(secret)) {
		t.Fatal("a device's key is kept as given, or its hash isn't kept")
	}
	who, ok := Authenticate(secret)
	if !ok || who.KeyID != id || who.KeyName != "laptop" || who.Member != "m1" || who.MemberName != "alice" || who.Concurrency != 2 || len(who.Models) != 1 {
		t.Fatalf("the device: %+v %v", who, ok)
	}
	if !Named(secret) {
		t.Fatal("a device key isn't taken for a gateway key's form")
	}
	// the member's models change, and the next request sees it
	members["m1"] = Identity{Member: "m1", MemberName: "alice"}
	if who, _ := Authenticate(secret); len(who.Models) != 1 || who.Models[0] != NoModels || who.Allows("plan/m1") || who.Allows("group/coding") {
		t.Fatalf("a member with no models may use %v", who.Models)
	}
	// a key's own lists are never a device's: only the member's are
	if _, err := Update("models-key", Change{Key: id, Models: []string{"plan/*"}}); err != nil {
		t.Fatal(err)
	}
	if who, _ := Authenticate(secret); who.Allows("plan/m1") {
		t.Fatal("a device took models from the key's own list, past its member's")
	}
	// the list shows a device masked, never its hash's worth of secret
	keys, _ := List()
	if len(keys) != 1 || keys[0].Secret != "" || !strings.HasSuffix(keys[0].Masked, secret[len(secret)-4:]) || keys[0].Member != "m1" {
		t.Fatalf("the list: %+v", keys)
	}
	if got, _ := Update("copy-key", Change{Key: id}); got != "" {
		t.Fatal("a device's key was given out again")
	}
	// only its own member takes it away, or the administrator
	if err := RemoveDevice("m2", id); err == nil {
		t.Fatal("another member took the device away")
	}
	// the member gone: refused, though the key is still listed
	delete(members, "m1")
	if _, ok := Authenticate(secret); ok {
		t.Fatal("a device of a member that is gone was let in")
	}
	members["m1"] = Identity{Member: "m1"}
	MemberOf = nil
	if _, ok := Authenticate(secret); ok {
		t.Fatal("a device was let in with nothing to say whose it is")
	}
	MemberOf = func(k Key) (Identity, bool) { who, ok := members[k.Member]; return who, ok }
	if err := RemoveDevice("m1", id); err != nil {
		t.Fatal(err)
	}
	if _, ok := Authenticate(secret); ok {
		t.Fatal("a device taken away was let in")
	}
}

// Taking one device away, or all of one member's, leaves every other key.
func TestRemoveDevicesLeavesOthers(t *testing.T) {
	deviceHome(t)
	was := MemberOf
	t.Cleanup(func() { MemberOf = was })
	MemberOf = func(k Key) (Identity, bool) { return Identity{Member: k.Member}, true }
	own, err := Update("add-key", Change{Name: "the hub's own"})
	if err != nil {
		t.Fatal(err)
	}
	_, a1, _ := AddDevice("alice", "a1", 0)
	_, a2, _ := AddDevice("alice", "a2", 0)
	bid, b1, _ := AddDevice("bob", "b1", 0)
	if ds, _ := Devices("alice"); len(ds) != 2 {
		t.Fatalf("alice's devices: %+v", ds)
	}
	if ds, _ := Devices(""); len(ds) != 3 {
		t.Fatalf("every device: %d", len(ds))
	}
	if err := RemoveDevices("alice"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{a1, a2} {
		if _, ok := Authenticate(k); ok {
			t.Fatal("alice's device outlived her")
		}
	}
	for _, k := range []string{b1, own} {
		if _, ok := Authenticate(k); !ok {
			t.Fatal("another's key went with alice's")
		}
	}
	if err := RemoveDevice("", bid); err != nil {
		t.Fatal(err)
	}
	if _, ok := Authenticate(own); !ok {
		t.Fatal("the hub's own key went with bob's device")
	}
	if err := RemoveDevice("", own); err == nil {
		t.Fatal("a key that is no device was taken away as one")
	}
	if _, _, err := AddDevice("", "x", 0); err == nil {
		t.Fatal("a device of no member")
	}
}
