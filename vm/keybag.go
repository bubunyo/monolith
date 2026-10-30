package vm

/*
#include <dlfcn.h>

// Returns the MobileKeyBag lock state (0 unlocked, 1 locked, 2 locking,
// 3 no passcode), or -1 if it can't be determined. MobileKeyBag is a
// private framework, so it is loaded at runtime rather than linked.
static int keybag_lock_state(void) {
	static int (*get)(void *) = 0;
	if (!get) {
		void *h = dlopen("/System/Library/PrivateFrameworks/MobileKeyBag.framework/MobileKeyBag", RTLD_LAZY);
		if (!h) return -1;
		get = (int (*)(void *))dlsym(h, "MKBGetDeviceLockState");
		if (!get) return -1;
	}
	return get(0);
}
*/
import "C"

import "errors"

// errKeybagLocked explains why a restore can't work right now.
// Virtualization.framework encrypts saved VM state with a Secure Enclave
// key that can only decrypt while the user's keybag is unlocked. Saving
// works on a locked Mac; restoring fails with VZ error 12 "permission
// denied" until the user unlocks it.
var errKeybagLocked = errors.New("the Mac is locked: a suspended VM can only be restored while the screen is unlocked (unlock and retry; the snapshot is kept)")

// keybagLocked reports whether the user's keybag is locked, which happens
// shortly after the screen locks.
func keybagLocked() bool {
	switch C.keybag_lock_state() {
	case 1, 2:
		return true
	}
	return false
}
