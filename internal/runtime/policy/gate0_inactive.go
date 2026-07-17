package policy

func Gate0Inactive(_ Invocation, pc PolicyContext) Decision {
	if !pc.AppActive {
		return deny(GateInactive, "app is not active (deployed but disabled)")
	}
	return allow(GateInactive)
}
