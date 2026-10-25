package reviewer

// NativeVMExecObservation records only entry into the pinned deny-only helper.
// It is written by guest PID1, never accepted from client output. Zero means
// no observed attempt. Exec failures before helper entry remain unobservable.
type NativeVMExecObservation struct {
	Version                                    int
	Binding                                    AuthBinding
	RequestHash, ImageSourceHead, HelperSHA256 string
	HelperEntry                                string
	UID                                        int
	Attempts, Denied                           int
	Failed, Complete                           bool
}

// Bound checks the private observation's identity and bounded counters. The
// caller must also establish its trusted guest/control-channel provenance.
func (o *NativeVMExecObservation) Bound(binding AuthBinding, requestHash string) bool {
	return o.valid(binding, requestHash) && !o.Failed && o.Complete
}

func (o *NativeVMExecObservation) valid(binding AuthBinding, requestHash string) bool {
	return o != nil && o.Version == 1 && binding.valid() && o.Binding == binding &&
		validAuthHash(requestHash) && o.RequestHash == requestHash &&
		objectID.MatchString(o.ImageSourceHead) && validAuthHash(o.HelperSHA256) &&
		o.HelperEntry == "sh-deny-v1" &&
		o.UID == nativeVMCodexUID && o.Attempts >= 0 && o.Attempts <= 64 &&
		o.Denied >= 0 && o.Denied <= o.Attempts
}

// Outcome describes observed helper executions, never a specific client hook
// or all process execution. A missing, stale or failed record is unknown.
func (o *NativeVMExecObservation) Outcome(binding AuthBinding, requestHash string) string {
	if !o.Bound(binding, requestHash) {
		return "unknown"
	}
	if o.Denied != o.Attempts {
		return "unsafe"
	}
	if o.Attempts == 0 {
		return "no-observed-attempt"
	}
	return "attempted-and-denied"
}
